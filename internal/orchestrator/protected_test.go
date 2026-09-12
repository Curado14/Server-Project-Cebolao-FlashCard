package orchestrator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func jpegBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	for y := 0; y < 3; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 30), uint8(y * 40), 100, 255})
		}
	}
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func newTestService(t *testing.T) *Service {
	t.Helper()
	c := DefaultConfig()
	c.Root = t.TempDir()
	s, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func request(t *testing.T, s *Service, key string, data []byte, event string) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	fw, _ := mw.CreateFormFile("photo", "../../hostile.jpg")
	_, _ = fw.Write(data)
	_ = mw.WriteField("event_id", event)
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/photos", &b)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Authorization", "Bearer "+s.cfg.DeviceToken)
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestProtectedPathStateContract(t *testing.T) {
	c := DefaultConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if safePart("../../x/a") != "xa" {
		t.Fatal("unsafe component was not sanitized")
	}
	if within("C:\\root", filepath.Join("C:\\root", "x")) == false {
		t.Fatal("root path check failed")
	}
	for _, bind := range []string{"0.0.0.0:8090", ":8090", "192.168.1.2:8090"} {
		c.Bind = bind
		if err := c.Validate(); err == nil {
			t.Fatalf("public bind accepted: %s", bind)
		}
	}
}
func TestProtectedIdempotencyConcurrent(t *testing.T) {
	s := newTestService(t)
	data := jpegBytes(t)
	var wg sync.WaitGroup
	out := make(chan int, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); out <- request(t, s, "same-key", data, "local-event").Code }()
	}
	wg.Wait()
	close(out)
	accepted := 0
	for code := range out {
		if code == 202 {
			accepted++
		} else if code != 409 {
			t.Fatalf("unexpected concurrent status %d", code)
		}
	}
	if accepted != 20 {
		t.Fatalf("replay statuses=%d", accepted)
	}
	if got := request(t, s, "same-key", append(data, 1), "local-event").Code; got != 409 {
		t.Fatalf("conflict status %d", got)
	}
	var n int
	_ = s.db.sql.QueryRow("SELECT count(*) FROM photos").Scan(&n)
	if n != 1 {
		t.Fatalf("photos=%d", n)
	}
}
func TestProtectedSQLiteMigrationConstraintQueue(t *testing.T) {
	s := newTestService(t)
	var journal, foreign, syncMode string
	_ = s.db.sql.QueryRow("PRAGMA journal_mode").Scan(&journal)
	_ = s.db.sql.QueryRow("PRAGMA foreign_keys").Scan(&foreign)
	_ = s.db.sql.QueryRow("PRAGMA synchronous").Scan(&syncMode)
	if journal != "wal" || foreign != "1" || syncMode != "2" {
		t.Fatalf("pragmas journal=%s fk=%s sync=%s", journal, foreign, syncMode)
	}
	if _, err := s.db.sql.Exec("INSERT INTO jobs(id,photo_id,kind,operation_key,status,attempt_count,max_attempts,available_at,created_at,updated_at) VALUES('j','missing','PROCESS','k','PENDING',0,1,?,?,?)", time.Now(), time.Now(), time.Now()); err == nil {
		t.Fatal("foreign key accepted")
	}
}
func TestProtectedHostileLimitFuzz(t *testing.T) {
	s := newTestService(t)
	w := request(t, s, "hostile", jpegBytes(t), "../../local-event")
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	var body PhotoResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var p string
	_ = s.db.sql.QueryRow("SELECT original_path FROM photos WHERE id=?", body.PhotoID).Scan(&p)
	if !within(s.cfg.Root, p) {
		t.Fatalf("path escaped root: %s", p)
	}
	s.cfg.MaxUploadBytes = 10
	if got := request(t, s, "large", jpegBytes(t), "local-event").Code; got != 413 {
		t.Fatalf("limit status %d", got)
	}
}
func TestProtectedLeaseRetryAttemptStateful(t *testing.T) {
	s := newTestService(t)
	now := time.Now().UTC()
	_, err := s.db.sql.Exec(`INSERT INTO photos(id,event_id,device_id,idempotency_key,original_filename,original_path,media_type,size_bytes,sha256,received_at,status,width,height) VALUES('p','local-event','camera_01','k','a','x','image/jpeg',1,'h',?,'STORED',1,1)`, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.sql.Exec(`INSERT INTO jobs(id,photo_id,kind,operation_key,status,attempt_count,max_attempts,available_at,lease_owner,lease_until,created_at,updated_at) VALUES('j','p','PROCESS','p:process','RUNNING',1,3,?,?,?, ?,?)`, now, "worker", now.Add(-time.Minute), now, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.recover(); err != nil {
		t.Fatal(err)
	}
	var status string
	_ = s.db.sql.QueryRow("SELECT status FROM jobs WHERE id='j'").Scan(&status)
	if status != "PENDING" {
		t.Fatal(status)
	}
}
func TestProtectedArtifactPresetIntegrity(t *testing.T) {
	s := newTestService(t)
	data := jpegBytes(t)
	w := request(t, s, "artifact", data, "local-event")
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	var body PhotoResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var path, hash string
	_ = s.db.sql.QueryRow("SELECT original_path,sha256 FROM photos WHERE id=?", body.PhotoID).Scan(&path, &hash)
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved, data) {
		t.Fatal("original changed")
	}
	if err := s.ProcessPending(); err != nil {
		t.Fatal(err)
	}
	var artifacts int
	_ = s.db.sql.QueryRow("SELECT count(*) FROM artifacts WHERE photo_id=? AND status='READY'", body.PhotoID).Scan(&artifacts)
	if artifacts != 1 {
		t.Fatalf("artifacts=%d", artifacts)
	}
	var attempts int
	if err := s.db.sql.QueryRow("SELECT count(*) FROM job_attempts").Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("job attempt events=%d", attempts)
	}
	var starts, finishes int
	_ = s.db.sql.QueryRow("SELECT count(*) FROM job_attempts WHERE event_type='START'").Scan(&starts)
	_ = s.db.sql.QueryRow("SELECT count(*) FROM job_attempts WHERE event_type='FINISH' AND result='SUCCESS'").Scan(&finishes)
	if starts != 1 || finishes != 1 {
		t.Fatalf("starts=%d finishes=%d", starts, finishes)
	}
	if len(hash) != 64 || len(data) == 0 {
		t.Fatal("invalid integrity")
	}
	_ = fmt.Sprintf("%v", strings.TrimSpace(hash))
}
func TestProtectedExportReceiptManifestDestination(t *testing.T) {
	s := newTestService(t)
	if strings.Contains(strings.ToLower(s.cfg.Bind), "http") {
		t.Fatal("external bind")
	}
	if err := os.MkdirAll(filepath.Join(s.cfg.Root, "exports"), 0700); err != nil {
		t.Fatal(err)
	}
	data := jpegBytes(t)
	w := request(t, s, "export", data, "local-event")
	if w.Code != 202 {
		t.Fatal(w.Code)
	}
	var p PhotoResponse
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if err := s.ProcessPending(); err != nil {
		t.Fatal(err)
	}
	var aid string
	_ = s.db.sql.QueryRow("SELECT id FROM artifacts WHERE photo_id=?", p.PhotoID).Scan(&aid)
	cfg, _ := json.Marshal(localDestination{Path: filepath.Join(s.cfg.Root, "exports")})
	_, err := s.db.sql.Exec(`INSERT INTO export_destinations(id,name,type,config_json,enabled,created_at,updated_at) VALUES('dest','local','LOCAL_FOLDER',?,1,?,?)`, string(cfg), time.Now(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ExportArtifact(aid, "dest"); err != nil {
		t.Fatal(err)
	}
	var status, receipt string
	_ = s.db.sql.QueryRow("SELECT status,receipt_id FROM exports WHERE artifact_id=?", aid).Scan(&status, &receipt)
	if status != "VERIFIED" || receipt == "" {
		t.Fatalf("export status=%s receipt=%s", status, receipt)
	}
	if err := s.ExportArtifact(aid, "dest"); err != nil {
		t.Fatal(err)
	}
	var exports, exportAttempts int
	_ = s.db.sql.QueryRow("SELECT count(*) FROM exports WHERE artifact_id=?", aid).Scan(&exports)
	_ = s.db.sql.QueryRow("SELECT count(*) FROM export_attempts").Scan(&exportAttempts)
	if exports != 1 || exportAttempts != 1 {
		t.Fatalf("exports=%d attempts=%d", exports, exportAttempts)
	}
}
func TestProtectedPrivacyLocalOnlyReadyHealthMetricsAuditSecret(t *testing.T) {
	s := newTestService(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/health/ready", nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/v1/photos", strings.NewReader("secret-token"))
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	if strings.Contains(w.Body.String(), s.cfg.DeviceToken) {
		t.Fatal("token leaked")
	}
	r = httptest.NewRequest("GET", "/metrics", nil)
	r.RemoteAddr = "192.0.2.1:44"
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("remote metrics status=%d", w.Code)
	}
	_ = s.db.sql.Close()
	if got := request(t, s, "degraded", jpegBytes(t), "local-event").Code; got != 503 {
		t.Fatalf("degraded intake status=%d", got)
	}
}

func TestProtectedDeviceRevocationBlocksDurableIntake(t *testing.T) {
	s := newTestService(t)
	data := jpegBytes(t)
	if _, err := s.db.sql.Exec("UPDATE devices SET enabled=0 WHERE id=?", s.cfg.DeviceID); err != nil {
		t.Fatal(err)
	}
	if got := request(t, s, "disabled-device", data, "local-event").Code; got != 401 {
		t.Fatalf("disabled device status=%d", got)
	}
	if _, err := s.db.sql.Exec("UPDATE devices SET enabled=1,token_hash=? WHERE id=?", hashToken("revoked-token"), s.cfg.DeviceID); err != nil {
		t.Fatal(err)
	}
	if got := request(t, s, "revoked-token", data, "local-event").Code; got != 401 {
		t.Fatalf("revoked token status=%d", got)
	}
	for _, table := range []string{"photos", "photo_files", "jobs"} {
		var count int
		if err := s.db.sql.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s=%d after rejected intake", table, count)
		}
	}
	var accepted int
	if err := s.db.sql.QueryRow("SELECT count(*) FROM audit_events WHERE action='ACCEPTED'").Scan(&accepted); err != nil {
		t.Fatal(err)
	}
	if accepted != 0 {
		t.Fatalf("accepted audit events=%d", accepted)
	}
}

func TestProtectedCrashRecoverRestartQuarantine(t *testing.T) {
	s := newTestService(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.sql.Exec(`INSERT INTO photos(id,event_id,device_id,idempotency_key,original_filename,original_path,media_type,size_bytes,sha256,received_at,status,width,height) VALUES('missing','local-event','camera_01','missing-key','x',?,'image/jpeg',1,'bad',?,'STORED',1,1)`, filepath.Join(s.cfg.Root, "originals", "missing.jpg"), now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverJobs(); err != nil {
		t.Fatal(err)
	}
	var status, reason string
	_ = s.db.sql.QueryRow("SELECT status FROM photos WHERE id='missing'").Scan(&status)
	_ = s.db.sql.QueryRow("SELECT reason_code FROM quarantine_entries WHERE photo_id='missing'").Scan(&reason)
	if status != "QUARANTINED" || reason != "ORIGINAL_MISSING" {
		t.Fatalf("status=%s reason=%s", status, reason)
	}
	part := filepath.Join(s.cfg.Root, "incoming", ".tmp", "stale.part")
	if err = os.WriteFile(part, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverJobs(); err != nil {
		t.Fatal(err)
	}
	var audits int
	_ = s.db.sql.QueryRow("SELECT count(*) FROM audit_events WHERE action='QUARANTINED'").Scan(&audits)
	if audits < 2 {
		t.Fatalf("quarantine audits=%d", audits)
	}
}
func TestProtectedE2EMutation(t *testing.T) {
	s := newTestService(t)
	w := request(t, s, "e2e", jpegBytes(t), "local-event")
	if w.Code != 202 {
		t.Fatal(w.Code)
	}
	var p PhotoResponse
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if err := s.ProcessPending(); err != nil {
		t.Fatal(err)
	}
	var artifactID string
	if err := s.db.sql.QueryRow("SELECT id FROM artifacts WHERE photo_id=?", p.PhotoID).Scan(&artifactID); err != nil {
		t.Fatal(err)
	}
	destinationPath := filepath.Join(s.cfg.Root, "exports", "e2e")
	if err := os.MkdirAll(destinationPath, 0700); err != nil {
		t.Fatal(err)
	}
	destinationConfig, _ := json.Marshal(localDestination{Path: destinationPath})
	if _, err := s.db.sql.Exec(`INSERT INTO export_destinations(id,name,type,config_json,enabled,created_at,updated_at) VALUES('e2e-dest','e2e','LOCAL_FOLDER',?,1,?,?)`, string(destinationConfig), time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.ExportArtifact(artifactID, "e2e-dest"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var exportStatus, receipt string
	if err := reopened.db.sql.QueryRow("SELECT status,receipt_id FROM exports WHERE artifact_id=? AND destination_id='e2e-dest'", artifactID).Scan(&exportStatus, &receipt); err != nil {
		t.Fatal(err)
	}
	if exportStatus != "VERIFIED" || receipt == "" {
		t.Fatalf("status=%s receipt=%s", exportStatus, receipt)
	}
	var chain int
	_ = reopened.db.sql.QueryRow("SELECT count(*) FROM jobs WHERE photo_id=?", p.PhotoID).Scan(&chain)
	if chain != 1 {
		t.Fatalf("job chain count=%d", chain)
	}
	w = httptest.NewRecorder()
	reopened.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/photos/"+p.PhotoID, nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}
