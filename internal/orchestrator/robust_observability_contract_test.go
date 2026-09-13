package orchestrator

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestContractMigrationV2IsAdditiveAndIdempotent(t *testing.T) {
	db, err := sql.Open("sqlite", t.TempDir()+"/migration.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);
CREATE TABLE audit_events(id INTEGER PRIMARY KEY, entity_type TEXT NOT NULL, entity_id TEXT NOT NULL, action TEXT NOT NULL, actor_type TEXT NOT NULL, actor_id TEXT, request_id TEXT, metadata_json TEXT, created_at TEXT NOT NULL) STRICT;
INSERT INTO schema_migrations(version,name,checksum,applied_at) VALUES(1,'initial','photoedge-v1','2026-01-01T00:00:00Z');
INSERT INTO audit_events(entity_type,entity_id,action,actor_type,request_id,metadata_json,created_at) VALUES('PHOTO','legacy-photo','ACCEPTED','DEVICE','legacy-request','{}','2026-01-01T00:00:00Z');`)
	if err != nil {
		t.Fatal(err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("first migration run: %v", err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("second migration run must be idempotent: %v", err)
	}

	wantColumns := map[string]bool{"job_id": false, "event_id": false, "camera_id": false, "status": false, "error_code": false}
	rows, err := db.Query("PRAGMA table_info(audit_events)")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if _, ok := wantColumns[name]; ok {
			wantColumns[name] = true
		}
	}
	rows.Close()
	for name, found := range wantColumns {
		if !found {
			t.Errorf("migration v2 did not add audit_events.%s", name)
		}
	}

	var legacyRows, versionRows, indexes int
	if err := db.QueryRow("SELECT count(*) FROM audit_events WHERE entity_id='legacy-photo' AND request_id='legacy-request'").Scan(&legacyRows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM schema_migrations WHERE version=2").Scan(&versionRows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='index' AND name IN ('idx_audit_request','idx_audit_job')").Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if legacyRows != 1 || versionRows != 1 || indexes != 2 {
		t.Fatalf("legacy=%d migration_v2=%d required_indexes=%d", legacyRows, versionRows, indexes)
	}
}

func TestContractAcceptanceAuditIsAtomic(t *testing.T) {
	s := newTestService(t)
	_, err := s.db.sql.Exec(`CREATE TRIGGER contract_reject_accepted_audit
BEFORE INSERT ON audit_events WHEN NEW.action='ACCEPTED'
BEGIN SELECT RAISE(ABORT, 'forced audit failure'); END`)
	if err != nil {
		t.Fatal(err)
	}

	w := request(t, s, "atomic-audit-key", jpegBytes(t), "local-event")
	if w.Code == http.StatusAccepted {
		t.Fatal("audit failure produced 202")
	}
	for _, table := range []string{"photos", "ingestion_requests", "photo_files", "jobs"} {
		var count int
		if err := s.db.sql.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("%s has %d rows after rejected acceptance transaction", table, count)
		}
	}
}

func TestContractRequestCorrelationReplayAndTelemetryPrivacy(t *testing.T) {
	s := newTestService(t)
	secretKey := "raw-idempotency-contract-secret"
	payload := jpegWithEXIFMarker(t, "PRIVATE-EXIF-GPS-PAYLOAD")

	first := request(t, s, secretKey, payload, "local-event")
	replay := request(t, s, secretKey, payload, "local-event")
	if first.Code != http.StatusAccepted || replay.Code != http.StatusAccepted {
		t.Fatalf("first=%d replay=%d", first.Code, replay.Code)
	}
	var firstBody, replayBody PhotoResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &replayBody); err != nil {
		t.Fatal(err)
	}
	if firstBody.PhotoID == "" || firstBody.PhotoID != replayBody.PhotoID {
		t.Fatalf("first photo=%q replay photo=%q", firstBody.PhotoID, replayBody.PhotoID)
	}

	for table, want := range map[string]int{"photos": 1, "ingestion_requests": 1, "photo_files": 1, "jobs": 1} {
		var count int
		if err := s.db.sql.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Errorf("%s=%d, want %d after replay", table, count, want)
		}
	}

	var auditRows, distinctRequests int
	if err := s.db.sql.QueryRow("SELECT count(*), count(DISTINCT request_id) FROM audit_events WHERE entity_id=?", firstBody.PhotoID).Scan(&auditRows, &distinctRequests); err != nil {
		t.Fatal(err)
	}
	if auditRows < 2 || distinctRequests < 2 {
		t.Fatalf("replay must be observable with a distinct request_id: rows=%d distinct=%d", auditRows, distinctRequests)
	}
	var unsafe int
	if err := s.db.sql.QueryRow(`SELECT count(*) FROM audit_events
WHERE request_id=? OR instr(COALESCE(metadata_json,''),?)>0 OR instr(COALESCE(metadata_json,''),'PRIVATE-EXIF-GPS-PAYLOAD')>0`, secretKey, secretKey).Scan(&unsafe); err != nil {
		t.Fatal(err)
	}
	if unsafe != 0 {
		t.Fatalf("audit contains raw idempotency or EXIF/payload marker in %d rows", unsafe)
	}
}

func TestContractMetricsAliasesAndCountersAreConcurrencySafe(t *testing.T) {
	s := newTestService(t)
	data := jpegBytes(t)
	const uploads = 12
	statuses := make(chan int, uploads)
	var wg sync.WaitGroup
	for i := 0; i < uploads; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses <- request(t, s, fmt.Sprintf("metric-key-%d", i), data, "local-event").Code
		}(i)
	}
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusAccepted {
			t.Fatalf("concurrent upload status=%d", status)
		}
	}

	for i := 0; i < uploads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.ProcessPending(); err != nil {
				t.Errorf("ProcessPending: %v", err)
			}
		}()
	}
	wg.Wait()

	body := contractMetricsBody(t, s)
	assertPrometheusMetadata(t, body)
	samples := parseUnlabelledMetrics(t, body)
	if samples["photoedge_photos_accepted_total"] != uploads || samples["photoedge_accepted_total"] != uploads {
		t.Fatalf("canonical=%v alias=%v want=%d", samples["photoedge_photos_accepted_total"], samples["photoedge_accepted_total"], uploads)
	}
	if samples["photoedge_photos_accepted_total"] != samples["photoedge_accepted_total"] {
		t.Fatal("accepted alias has an independently incremented value")
	}
	if samples["photoedge_jobs_completed_total"] != uploads || samples["photoedge_queue_depth"] != 0 || samples["photoedge_jobs_active"] != 0 {
		t.Fatalf("completed=%v queue=%v active=%v", samples["photoedge_jobs_completed_total"], samples["photoedge_queue_depth"], samples["photoedge_jobs_active"])
	}
}

func assertPrometheusMetadata(t *testing.T, body string) {
	t.Helper()
	metrics := map[string]string{
		"photoedge_photos_accepted_total":     "counter",
		"photoedge_photos_rejected_total":     "counter",
		"photoedge_http_request_duration_ms":  "histogram",
		"photoedge_photo_persist_duration_ms": "histogram",
		"photoedge_queue_depth":               "gauge",
		"photoedge_jobs_active":               "gauge",
		"photoedge_jobs_completed_total":      "counter",
		"photoedge_jobs_failed_total":         "counter",
		"photoedge_jobs_quarantined_total":    "counter",
		"photoedge_storage_free_bytes":        "gauge",
		"photoedge_sqlite_errors_total":       "counter",
		"photoedge_logging_errors_total":      "counter",
		"photoedge_accepted_total":            "counter",
		"photoedge_rejected_total":            "counter",
	}
	for name, typ := range metrics {
		if strings.Count(body, "# HELP "+name+" ") != 1 {
			t.Errorf("metric %s must have exactly one HELP line", name)
		}
		if strings.Count(body, "# TYPE "+name+" "+typ) != 1 {
			t.Errorf("metric %s must have TYPE %s", name, typ)
		}
	}
}

func TestContractIPhoneAndroidUploadRemains202(t *testing.T) {
	s := newTestService(t)
	jpeg := jpegWithEXIFMarker(t, "DEVICE-PRIVATE-EXIF")
	clients := []struct{ name, userAgent, filename string }{
		{"iphone", "Shortcuts/6 CFNetwork iOS", "IMG_0001.HEIC-converted.jpg"},
		{"android", "okhttp/4 Android", "PXL_20260913_120000.jpg"},
	}
	for _, client := range clients {
		t.Run(client.name, func(t *testing.T) {
			w := contractUpload(t, s, client.name+"-key", jpeg, client.filename, client.userAgent)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			var response PhotoResponse
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.PhotoID == "" || response.Status != "queued" || response.ReceivedAt == "" || response.StatusURL != "/api/v1/photos/"+response.PhotoID {
				t.Fatalf("unexpected 202 response: %#v", response)
			}
		})
	}
}

func TestContractPendingMigrationMakesReadinessAndIntakeUnavailable(t *testing.T) {
	s := newTestService(t)
	if _, err := s.db.sql.Exec("DELETE FROM schema_migrations WHERE version=2"); err != nil {
		t.Fatal(err)
	}

	ready := httptest.NewRecorder()
	s.Handler().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness with pending migration=%d", ready.Code)
	}
	if got := request(t, s, "migration-pending", jpegBytes(t), "local-event").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("intake with pending migration=%d", got)
	}
}

func contractUpload(t *testing.T, s *Service, key string, data []byte, filename, userAgent string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("photo", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("event_id", s.cfg.EventID); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/photos", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Authorization", "Bearer "+s.cfg.DeviceToken)
	r.Header.Set("Idempotency-Key", key)
	r.Header.Set("User-Agent", userAgent)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func jpegWithEXIFMarker(t *testing.T, marker string) []byte {
	t.Helper()
	base := jpegBytes(t)
	payload := append([]byte("Exif\x00\x00"), []byte(marker)...)
	segment := make([]byte, 4+len(payload))
	segment[0], segment[1] = 0xff, 0xe1
	binary.BigEndian.PutUint16(segment[2:4], uint16(len(payload)+2))
	copy(segment[4:], payload)
	return append(append(append([]byte{}, base[:2]...), segment...), base[2:]...)
}

func contractMetricsBody(t *testing.T, s *Service) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	r.RemoteAddr = "127.0.0.1:43210"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("metrics status=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "text/plain; version=0.0.4" {
		t.Fatalf("metrics content type=%q", got)
	}
	return w.Body.String()
}

func parseUnlabelledMetrics(t *testing.T, body string) map[string]float64 {
	t.Helper()
	values := make(map[string]float64)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("invalid metric sample %q", line)
		}
		name := fields[0]
		if open := strings.IndexByte(name, '{'); open >= 0 {
			labels := name[open:]
			if !strings.HasPrefix(labels, `{le="`) || !strings.HasSuffix(labels, `"}`) {
				t.Fatalf("entity/high-cardinality labels are forbidden: %q", line)
			}
			name = name[:open]
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			t.Fatalf("invalid metric value %q: %v", line, err)
		}
		values[name] = value
	}
	return values
}
