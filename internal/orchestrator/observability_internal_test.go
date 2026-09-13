package orchestrator

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestContractJSONLog(t *testing.T) {
	var output bytes.Buffer
	logger, err := NewJSONLogger(&output, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	handler := AccessLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusAccepted, map[string]string{"ok": "true"})
	}), logger)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/photos", strings.NewReader("private-body"))
	req.Header.Set("Authorization", "Bearer private-token")
	req.Header.Set("Idempotency-Key", "private-key")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &entry); err != nil {
		t.Fatalf("log is not ndjson: %v\n%s", err, output.String())
	}
	for _, field := range []string{"time", "level", "msg", "event", "service", "version", "instance_id", "component", "request_id"} {
		if entry[field] == "" || entry[field] == nil {
			t.Fatalf("missing log field %s in %#v", field, entry)
		}
	}
	if _, ok := entry["response_bytes"]; ok {
		t.Fatalf("access log emitted a field outside the allowlist: %#v", entry)
	}
	for _, secret := range []string{"private-body", "private-token", "private-key"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("log exposed secret %q", secret)
		}
	}
}

func TestContractTelemetryPrivacy(t *testing.T) {
	s := newTestService(t)
	token := s.cfg.DeviceToken
	key := "telemetry-privacy-key"
	payload := jpegWithEXIFMarker(t, "PRIVATE-TELEMETRY-EXIF")
	if got := request(t, s, key, payload, "local-event").Code; got != http.StatusAccepted {
		t.Fatalf("status=%d", got)
	}

	var auditDump string
	rows, err := s.db.sql.Query(`SELECT COALESCE(request_id,''), COALESCE(metadata_json,'') FROM audit_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var requestID, metadata string
		if err := rows.Scan(&requestID, &metadata); err != nil {
			t.Fatal(err)
		}
		auditDump += requestID + "\n" + metadata + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	metrics := contractMetricsBody(t, s)
	for _, secret := range []string{token, key, "PRIVATE-TELEMETRY-EXIF"} {
		if strings.Contains(auditDump, secret) || strings.Contains(metrics, secret) {
			t.Fatalf("telemetry exposed secret %q", secret)
		}
	}
	if !regexp.MustCompile(`"idempotency_key_hash":"[a-f0-9]{16}"`).MatchString(auditDump) {
		t.Fatalf("audit did not expose the bounded idempotency hash: %s", auditDump)
	}
}

func TestContractAuditTransitionsAppendOnly(t *testing.T) {
	s := newTestService(t)
	resp := request(t, s, "audit-append", jpegBytes(t), "local-event")
	if resp.Code != http.StatusAccepted {
		t.Fatal(resp.Body.String())
	}
	if err := s.ProcessPending(); err != nil {
		t.Fatal(err)
	}
	var accepted, completed int
	if err := s.db.sql.QueryRow(`SELECT count(*) FROM audit_events WHERE action='ACCEPTED' AND job_id IS NOT NULL AND request_id IS NOT NULL`).Scan(&accepted); err != nil {
		t.Fatal(err)
	}
	if err := s.db.sql.QueryRow(`SELECT count(*) FROM audit_events WHERE entity_type='JOB' AND action='COMPLETED' AND job_id IS NOT NULL`).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if accepted != 1 || completed != 1 {
		t.Fatalf("accepted=%d completed=%d", accepted, completed)
	}
}

func TestContractReadinessFaultMatrix(t *testing.T) {
	s := newTestService(t)
	s.cfg.MinFreeBytes = math.MaxUint64
	ready := httptest.NewRecorder()
	s.Handler().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready status=%d", ready.Code)
	}
	if got := request(t, s, "low-space", jpegBytes(t), "local-event").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("intake status=%d", got)
	}
}

func TestContractMinFreeBytesConfig(t *testing.T) {
	base := DefaultConfig()
	if base.MinFreeBytes != 1<<30 {
		t.Fatalf("default min free bytes=%d", base.MinFreeBytes)
	}
	t.Setenv("PHOTOEDGE_MIN_FREE_BYTES", "4096")
	cfg, err := ConfigFromEnv(base)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinFreeBytes != 4096 {
		t.Fatalf("override min free bytes=%d", cfg.MinFreeBytes)
	}
	t.Setenv("PHOTOEDGE_MIN_FREE_BYTES", "0")
	if _, err := ConfigFromEnv(base); err == nil {
		t.Fatal("zero min free bytes accepted")
	}
}

func TestContractAuditMetricsDifferential(t *testing.T) {
	s := newTestService(t)
	if got := request(t, s, "differential", jpegBytes(t), "local-event").Code; got != http.StatusAccepted {
		t.Fatalf("status=%d", got)
	}
	if err := s.ProcessPending(); err != nil {
		t.Fatal(err)
	}
	samples := parseUnlabelledMetrics(t, contractMetricsBody(t, s))
	var photos, jobs int
	if err := s.db.sql.QueryRow(`SELECT count(*) FROM photos`).Scan(&photos); err != nil {
		t.Fatal(err)
	}
	if err := s.db.sql.QueryRow(`SELECT count(*) FROM jobs WHERE status='SUCCEEDED'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if samples["photoedge_photos_accepted_total"] != float64(photos) || samples["photoedge_jobs_completed_total"] != float64(jobs) {
		t.Fatalf("metrics accepted=%v completed=%v db photos=%d jobs=%d", samples["photoedge_photos_accepted_total"], samples["photoedge_jobs_completed_total"], photos, jobs)
	}
}

func TestContractWorkerObservability(t *testing.T) {
	s := newTestService(t)
	if got := request(t, s, "worker-observability", jpegBytes(t), "local-event").Code; got != http.StatusAccepted {
		t.Fatalf("status=%d", got)
	}
	if err := s.ProcessPending(); err != nil {
		t.Fatal(err)
	}
	var started, completed int
	if err := s.db.sql.QueryRow(`SELECT count(*) FROM audit_events WHERE entity_type='JOB' AND action IN ('STARTED','COMPLETED')`).Scan(&started); err != nil {
		t.Fatal(err)
	}
	if err := s.db.sql.QueryRow(`SELECT count(*) FROM jobs WHERE status='SUCCEEDED'`).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if started != 2 || completed != 1 {
		t.Fatalf("job audit rows=%d completed jobs=%d", started, completed)
	}
}

func TestContractCriticalAuditFailuresBlockTransitions(t *testing.T) {
	t.Run("job start", func(t *testing.T) {
		s := newTestService(t)
		if got := request(t, s, "audit-job-start", jpegBytes(t), "local-event").Code; got != http.StatusAccepted {
			t.Fatalf("status=%d", got)
		}
		if _, err := s.db.sql.Exec(`CREATE TRIGGER reject_job_started_audit
BEFORE INSERT ON audit_events WHEN NEW.entity_type='JOB' AND NEW.action='STARTED'
BEGIN SELECT RAISE(ABORT, 'forced job audit failure'); END`); err != nil {
			t.Fatal(err)
		}
		if err := s.ProcessPending(); err == nil {
			t.Fatal("job processing continued after required STARTED audit failed")
		}
		var status string
		var attempts int
		if err := s.db.sql.QueryRow(`SELECT status FROM jobs`).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if err := s.db.sql.QueryRow(`SELECT count(*) FROM job_attempts`).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if status != "PENDING" || attempts != 0 {
			t.Fatalf("job transition was not rolled back: status=%s attempts=%d", status, attempts)
		}
	})

	t.Run("recovery", func(t *testing.T) {
		s := newTestService(t)
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := s.db.sql.Exec(`INSERT INTO photos(id,event_id,device_id,idempotency_key,original_filename,original_path,media_type,size_bytes,sha256,received_at,status,width,height) VALUES('audit-recover-photo','local-event','camera_01','recover','x','missing','image/jpeg',1,'bad',?,'STORED',1,1)`, now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.sql.Exec(`INSERT INTO jobs(id,photo_id,kind,operation_key,status,attempt_count,max_attempts,available_at,lease_owner,lease_until,created_at,updated_at) VALUES('audit-recover-job','audit-recover-photo','PROCESS','audit-recover-photo:process','RUNNING',1,3,?,'worker','2026-01-01T00:00:00Z',?,?)`, now, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.sql.Exec(`CREATE TRIGGER reject_recovery_audit
BEFORE INSERT ON audit_events WHEN NEW.entity_type='JOB' AND NEW.action='RECOVERED_AFTER_RESTART'
BEGIN SELECT RAISE(ABORT, 'forced recovery audit failure'); END`); err != nil {
			t.Fatal(err)
		}
		if err := s.RecoverJobs(); err == nil {
			t.Fatal("recovery continued after required audit failed")
		}
		var status string
		if err := s.db.sql.QueryRow(`SELECT status FROM jobs WHERE id='audit-recover-job'`).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "RUNNING" {
			t.Fatalf("recovery transition was not rolled back: %s", status)
		}
	})

	t.Run("export start", func(t *testing.T) {
		s := newTestService(t)
		resp := request(t, s, "audit-export", jpegBytes(t), "local-event")
		if resp.Code != http.StatusAccepted {
			t.Fatal(resp.Body.String())
		}
		var body PhotoResponse
		if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if err := s.ProcessPending(); err != nil {
			t.Fatal(err)
		}
		var artifactID string
		if err := s.db.sql.QueryRow(`SELECT id FROM artifacts WHERE photo_id=?`, body.PhotoID).Scan(&artifactID); err != nil {
			t.Fatal(err)
		}
		cfg, _ := json.Marshal(localDestination{Path: filepath.Join(s.cfg.Root, "exports")})
		if _, err := s.db.sql.Exec(`INSERT INTO export_destinations(id,name,type,config_json,enabled,created_at,updated_at) VALUES('audit-export-dest','local','LOCAL_FOLDER',?,1,?,?)`, string(cfg), time.Now(), time.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.sql.Exec(`CREATE TRIGGER reject_export_started_audit
BEFORE INSERT ON audit_events WHEN NEW.entity_type='EXPORT' AND NEW.action='STARTED'
BEGIN SELECT RAISE(ABORT, 'forced export audit failure'); END`); err != nil {
			t.Fatal(err)
		}
		if err := s.ExportArtifact(artifactID, "audit-export-dest"); err == nil {
			t.Fatal("export continued after required STARTED audit failed")
		}
		var attempts int
		if err := s.db.sql.QueryRow(`SELECT count(*) FROM export_attempts`).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if attempts != 0 {
			t.Fatalf("export attempt was not rolled back: %d", attempts)
		}
	})
}

func TestContractRecoveryObservability(t *testing.T) {
	s := newTestService(t)
	now := "2026-01-01T00:00:00Z"
	_, err := s.db.sql.Exec(`INSERT INTO photos(id,event_id,device_id,idempotency_key,original_filename,original_path,media_type,size_bytes,sha256,received_at,status,width,height) VALUES('recover-photo','local-event','camera_01','recover-key','x','missing','image/jpeg',1,'bad',?,'STORED',1,1)`, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.sql.Exec(`INSERT INTO jobs(id,photo_id,kind,operation_key,status,attempt_count,max_attempts,available_at,lease_owner,lease_until,created_at,updated_at) VALUES('recover-job','recover-photo','PROCESS','recover-photo:process','RUNNING',1,3,?,'worker','2026-01-01T00:00:00Z',?,?)`, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverJobs(); err != nil {
		t.Fatal(err)
	}
	var recovered, quarantined int
	if err := s.db.sql.QueryRow(`SELECT count(*) FROM audit_events WHERE action='RECOVERED_AFTER_RESTART' AND job_id='recover-job'`).Scan(&recovered); err != nil {
		t.Fatal(err)
	}
	if err := s.db.sql.QueryRow(`SELECT count(*) FROM audit_events WHERE action='QUARANTINED' AND error_code='ORIGINAL_MISSING'`).Scan(&quarantined); err != nil {
		t.Fatal(err)
	}
	if recovered != 1 || quarantined != 1 {
		t.Fatalf("recovered=%d quarantined=%d", recovered, quarantined)
	}
}

func TestContractExportObservability(t *testing.T) {
	s := newTestService(t)
	resp := request(t, s, "export-observability", jpegBytes(t), "local-event")
	if resp.Code != http.StatusAccepted {
		t.Fatal(resp.Body.String())
	}
	var body PhotoResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if err := s.ProcessPending(); err != nil {
		t.Fatal(err)
	}
	var artifactID string
	if err := s.db.sql.QueryRow(`SELECT id FROM artifacts WHERE photo_id=?`, body.PhotoID).Scan(&artifactID); err != nil {
		t.Fatal(err)
	}
	destination := s.cfg.Root + "/exports"
	cfg, _ := json.Marshal(localDestination{Path: destination})
	if _, err := s.db.sql.Exec(`INSERT INTO export_destinations(id,name,type,config_json,enabled,created_at,updated_at) VALUES('obs-dest','local','LOCAL_FOLDER',?,1,?,?)`, string(cfg), "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := s.ExportArtifact(artifactID, "obs-dest"); err != nil {
		t.Fatal(err)
	}
	var verified int
	if err := s.db.sql.QueryRow(`SELECT count(*) FROM audit_events WHERE entity_type='EXPORT' AND action='VERIFIED'`).Scan(&verified); err != nil {
		t.Fatal(err)
	}
	if verified != 1 {
		t.Fatalf("verified export audits=%d", verified)
	}
}

func TestContractLocalOnly(t *testing.T) {
	s := newTestService(t)
	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	r.RemoteAddr = "203.0.113.10:4321"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("remote metrics status=%d", w.Code)
	}
}

func TestContractLoggerFailure(t *testing.T) {
	if _, err := NewJSONLogger(nil, DefaultConfig()); err == nil {
		t.Fatal("nil logger output accepted")
	}
	s := newTestService(t)
	logger, err := newJSONLogger(failingWriter{}, s.cfg, func() { s.metricsRegistry.loggingErrors.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	s.logger = logger
	w := request(t, s, "logger-post-startup", jpegBytes(t), "local-event")
	if w.Code != http.StatusAccepted {
		t.Fatalf("diagnostic log failure changed business result: status=%d body=%s", w.Code, w.Body.String())
	}
	samples := parseUnlabelledMetrics(t, contractMetricsBody(t, s))
	if samples["photoedge_logging_errors_total"] == 0 {
		t.Fatal("log writer failure did not increment photoedge_logging_errors_total")
	}
}

func TestContractVersionInstance(t *testing.T) {
	t.Setenv("PHOTOEDGE_INSTANCE_ID", "instance with spaces! and symbols")
	cfg, err := ConfigFromEnv(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InstanceID != "instancewithspacesandsymbols" {
		t.Fatalf("sanitized instance_id=%q", cfg.InstanceID)
	}
}

func TestContractServiceLifecycleLog(t *testing.T) {
	cfg := DefaultConfig()
	var output bytes.Buffer
	logger, err := NewJSONLogger(&output, cfg)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("service ready", "event", "service.ready", "component", "service")
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["event"] != "service.ready" || entry["service"] != serviceName || entry["instance_id"] != cfg.InstanceID {
		t.Fatalf("unexpected lifecycle log: %#v", entry)
	}
}

func TestContractCanonicalFailureEvents(t *testing.T) {
	s := newTestService(t)
	var output bytes.Buffer
	logger, err := newJSONLogger(&output, s.cfg, func() { s.metricsRegistry.loggingErrors.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	s.logger = logger

	missingKey := httptest.NewRequest(http.MethodPost, "/api/v1/photos", nil)
	missingKey.Header.Set("Authorization", "Bearer "+s.cfg.DeviceToken)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, missingKey)

	if err := os.Remove(filepath.Join(s.cfg.Root, "incoming", ".tmp")); err != nil {
		t.Fatal(err)
	}
	if got := request(t, s, "storage-failure", jpegBytes(t), "local-event").Code; got == http.StatusAccepted {
		t.Fatal("storage failure produced 202")
	}

	s.logSlowQuery(101*time.Millisecond, "database")
	_ = s.db.sql.Close()
	_ = request(t, s, "database-failure", jpegBytes(t), "local-event")

	for _, event := range []string{"photo.validation_failed", "storage.write_failed", "database.slow_query", "database.unavailable"} {
		if !strings.Contains(output.String(), `"event":"`+event+`"`) {
			t.Fatalf("missing canonical event %s in logs:\n%s", event, output.String())
		}
	}
	for _, forbidden := range []string{"response_bytes", "storage-failure", "database-failure"} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("forbidden value %q appeared in logs:\n%s", forbidden, output.String())
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write(_ []byte) (int, error) {
	return 0, errors.New("forced log writer failure")
}
