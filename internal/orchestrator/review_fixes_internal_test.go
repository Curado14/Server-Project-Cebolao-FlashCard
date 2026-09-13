package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReviewAcceptanceFilesystemCompensation(t *testing.T) {
	s := newTestService(t)
	payload := jpegBytes(t)
	reviewUpload(t, s)
	var acceptedPath string
	if err := s.db.sql.QueryRow(`SELECT original_path FROM photos`).Scan(&acceptedPath); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(acceptedPath)
	if err != nil {
		t.Fatal(err)
	}
	rejectAudit(t, s, "PHOTO", "ACCEPTED")
	var wg sync.WaitGroup
	statuses := make(chan int, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses <- request(t, s, "rejected-concurrent-key", payload, "local-event").Code
		}()
	}
	wg.Wait()
	close(statuses)
	for code := range statuses {
		if code != http.StatusServiceUnavailable {
			t.Fatalf("status=%d", code)
		}
	}
	for _, table := range []string{"photos", "photo_files", "ingestion_requests", "jobs"} {
		if reviewCount(t, s, "SELECT count(*) FROM "+table) != 1 {
			t.Fatalf("partial rows in %s", table)
		}
	}
	checkFiles := func(want int) {
		t.Helper()
		count := 0
		for _, dir := range []string{"originals", "incoming/.tmp"} {
			if err := filepath.Walk(filepath.Join(s.cfg.Root, dir), func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if !info.IsDir() {
					count++
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		if count != want {
			t.Fatalf("filesystem files=%d want=%d", count, want)
		}
	}
	checkFiles(1)
	if got, err := os.ReadFile(acceptedPath); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("accepted original changed: %v", err)
	}
	if got := request(t, s, "review-private-key", original, "local-event").Code; got != http.StatusAccepted {
		t.Fatalf("replay status=%d", got)
	}
	checkFiles(1)
	if _, err := s.db.sql.Exec(`DROP TRIGGER review_reject_audit`); err != nil {
		t.Fatal(err)
	}
	if got := request(t, s, "rejected-concurrent-key", payload, "local-event").Code; got != http.StatusAccepted {
		t.Fatalf("retry status=%d", got)
	}
	checkFiles(2)
}

func TestReviewLeaseEventAfterCommit(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "rollback"}[rollback], func(t *testing.T) {
			s := newTestService(t)
			reviewUpload(t, s)
			var output bytes.Buffer
			s.logger = slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
			if rollback {
				rejectAudit(t, s, "JOB", "STARTED")
			}
			err := s.ProcessPending()
			if (err != nil) != rollback {
				t.Fatalf("processing error=%v", err)
			}
			count := 0
			for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
				if len(line) == 0 {
					continue
				}
				var event map[string]any
				if err := json.Unmarshal(line, &event); err != nil {
					t.Fatal(err)
				}
				if event["event"] != "job.lease_acquired" {
					continue
				}
				count++
				if event["level"] != "DEBUG" || event["component"] != "worker" || event["attempt"] != float64(1) {
					t.Fatalf("invalid lease event: %v", event)
				}
				var matches int
				if err := s.db.sql.QueryRow(`SELECT count(*) FROM jobs WHERE id=? AND photo_id=? AND attempt_count=1`, event["job_id"], event["photo_id"]).Scan(&matches); err != nil || matches != 1 {
					t.Fatalf("lease correlation: %d %v", matches, err)
				}
			}
			want := 1
			if rollback {
				want = 0
			}
			if count != want {
				t.Fatalf("lease events=%d want=%d", count, want)
			}
			if reviewCount(t, s, `SELECT count(*) FROM job_attempts WHERE event_type='START'`) != want {
				t.Fatal("lease event and committed attempt disagree")
			}
		})
	}
}

func rejectAudit(t *testing.T, s *Service, entity, action string) {
	t.Helper()
	// Values come only from the closed test matrix, never from an HTTP input.
	_, err := s.db.sql.Exec(`CREATE TRIGGER review_reject_audit BEFORE INSERT ON audit_events WHEN NEW.entity_type='` + entity + `' AND NEW.action='` + action + `' BEGIN SELECT RAISE(ABORT, 'PRIVATE-AUDIT-FAILURE'); END`)
	if err != nil {
		t.Fatal(err)
	}
}

func reviewCount(t *testing.T, s *Service, query string) int {
	t.Helper()
	var count int
	if err := s.db.sql.QueryRow(query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func reviewUpload(t *testing.T, s *Service) {
	t.Helper()
	if got := request(t, s, "review-private-key", jpegBytes(t), "local-event"); got.Code != http.StatusAccepted {
		t.Fatal(got.Body.String())
	}
}

func TestReviewWorkerFinalAuditRollback(t *testing.T) {
	for _, action := range []string{"COMPLETED", "RETRY_SCHEDULED", "QUARANTINED"} {
		t.Run(action, func(t *testing.T) {
			s := newTestService(t)
			reviewUpload(t, s)
			if action != "COMPLETED" {
				var path string
				if err := s.db.sql.QueryRow(`SELECT original_path FROM photos`).Scan(&path); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if action == "QUARANTINED" {
				if _, err := s.db.sql.Exec(`UPDATE jobs SET max_attempts=1`); err != nil {
					t.Fatal(err)
				}
			}
			rejectAudit(t, s, "JOB", action)
			if err := s.ProcessPending(); err == nil {
				t.Fatal("required final audit failure was swallowed")
			}
			if reviewCount(t, s, `SELECT count(*) FROM jobs WHERE status='RUNNING' AND attempt_count=1`) != 1 || reviewCount(t, s, `SELECT count(*) FROM job_attempts WHERE event_type='FINISH'`) != 0 || reviewCount(t, s, `SELECT count(*) FROM artifacts`) != 0 {
				t.Fatal("partial job finalization committed")
			}
			if s.metricsRegistry.jobsCompleted.Load() != 0 || s.metricsRegistry.jobsFailed.Load() != 0 || s.metricsRegistry.jobsQuarantined.Load() != 0 || s.metricsRegistry.jobsActive.Load() != 0 {
				t.Fatal("failed transition changed metrics")
			}
			if reviewCount(t, s, `SELECT count(*) FROM audit_events WHERE action='ACCEPTED'`) != 1 {
				t.Fatal("prior audit lost")
			}
		})
	}
}

func TestReviewFileRecoveryAuditRollback(t *testing.T) {
	for _, subdir := range []string{"originals", "incoming/.tmp"} {
		t.Run(subdir, func(t *testing.T) {
			s := newTestService(t)
			path := filepath.Join(s.cfg.Root, subdir, "PRIVATE-FILENAME.part")
			payload := []byte("PRIVATE-FILE-CONTENT")
			if err := os.WriteFile(path, payload, 0600); err != nil {
				t.Fatal(err)
			}
			rejectAudit(t, s, "FILE", "QUARANTINED")
			if err := s.RecoverJobs(); err == nil {
				t.Fatal("required file audit failure swallowed")
			}
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("file no longer discoverable: %v", err)
			}
			if s.metricsRegistry.jobsQuarantined.Load() != 0 {
				t.Fatal("uncommitted quarantine counted")
			}
			if _, err := s.db.sql.Exec(`DROP TRIGGER review_reject_audit`); err != nil {
				t.Fatal(err)
			}
			if err := s.RecoverJobs(); err != nil {
				t.Fatal(err)
			}
			if err := s.RecoverJobs(); err != nil {
				t.Fatal(err)
			}
			if reviewCount(t, s, `SELECT count(*) FROM audit_events WHERE entity_type='FILE'`) != 1 || s.metricsRegistry.jobsQuarantined.Load() != 1 {
				t.Fatal("recovery did not commit exactly once")
			}
			var metadata string
			if err := s.db.sql.QueryRow(`SELECT metadata_json FROM audit_events WHERE entity_type='FILE'`).Scan(&metadata); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(metadata, "PRIVATE") {
				t.Fatal("file identity leaked to audit")
			}
		})
	}
}

func TestReviewPhotoQuarantineAuditRollback(t *testing.T) {
	s := newTestService(t)
	reviewUpload(t, s)
	rejectAudit(t, s, "PHOTO", "QUARANTINED")
	var photo string
	if err := s.db.sql.QueryRow(`SELECT id FROM photos`).Scan(&photo); err != nil {
		t.Fatal(err)
	}
	if err := s.quarantine(photo, "", "ORIGINAL_MISSING", "original file is missing"); err == nil {
		t.Fatal("audit failure swallowed")
	}
	if reviewCount(t, s, `SELECT count(*) FROM photos WHERE status='STORED'`) != 1 || reviewCount(t, s, `SELECT count(*) FROM quarantine_entries`) != 0 {
		t.Fatal("partial quarantine committed")
	}
}

func TestReviewExportFinalAuditRollback(t *testing.T) {
	for _, action := range []string{"STARTED", "VERIFIED", "FAILURE", "RETRY"} {
		t.Run(action, func(t *testing.T) {
			s := newTestService(t)
			reviewUpload(t, s)
			if err := s.ProcessPending(); err != nil {
				t.Fatal(err)
			}
			var artifact string
			if err := s.db.sql.QueryRow(`SELECT id FROM artifacts`).Scan(&artifact); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(s.cfg.Root, "exports")
			cfg, err := json.Marshal(localDestination{Path: dir})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.sql.Exec(`INSERT INTO export_destinations(id,name,type,config_json,enabled,created_at,updated_at) VALUES('review','review','LOCAL_FOLDER',?,1,?,?)`, string(cfg), time.Now(), time.Now()); err != nil {
				t.Fatal(err)
			}
			if action == "FAILURE" || action == "RETRY" {
				path := filepath.Join(dir, artifact+".jpg")
				if action == "FAILURE" {
					path += ".part"
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			rejectAudit(t, s, "EXPORT", action)
			if err := s.ExportArtifact(artifact, "review"); err == nil || !strings.Contains(err.Error(), "PRIVATE-AUDIT-FAILURE") {
				t.Fatalf("audit error not propagated: %v", err)
			}
			if action == "STARTED" {
				if reviewCount(t, s, `SELECT count(*) FROM exports`) != 0 || reviewCount(t, s, `SELECT count(*) FROM export_attempts`) != 0 {
					t.Fatal("export created without start audit")
				}
				return
			}
			if reviewCount(t, s, `SELECT count(*) FROM exports WHERE status='PENDING' AND receipt_id IS NULL`) != 1 || reviewCount(t, s, `SELECT count(*) FROM export_attempts WHERE result='RUNNING' AND finished_at IS NULL`) != 1 || reviewCount(t, s, `SELECT count(*) FROM photos WHERE status='READY'`) != 1 {
				t.Fatal("partial export finalization committed")
			}
		})
	}
}

type reviewWriter struct {
	bytes.Buffer
	fail     bool
	failures int
}

func (w *reviewWriter) Write(p []byte) (int, error) {
	if w.fail {
		w.failures++
		return 0, errors.New("PRIVATE-WRITER-ERROR")
	}
	return w.Buffer.Write(p)
}

func TestReviewWriterFailureAcrossBusinessLifecycle(t *testing.T) {
	s := newTestService(t)
	w := &reviewWriter{}
	logger, err := newJSONLogger(w, s.cfg, func() { s.metricsRegistry.loggingErrors.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	s.logger = logger.With("component", "test")
	s.logger.Info("healthy writer")
	if s.metricsRegistry.loggingErrors.Load() != 0 {
		t.Fatal("successful write counted as failure")
	}
	w.fail = true
	reviewUpload(t, s)
	if err := s.ProcessPending(); err != nil {
		t.Fatal(err)
	}
	s.logger.WithGroup("test").Info("grouped failure")
	if int64(w.failures) != s.metricsRegistry.loggingErrors.Load() || w.failures == 0 {
		t.Fatal("writer errors were lost or counted twice")
	}
	if reviewCount(t, s, `SELECT count(*) FROM jobs WHERE status='SUCCEEDED'`) != 1 || reviewCount(t, s, `SELECT count(*) FROM audit_events WHERE action='COMPLETED'`) != 1 {
		t.Fatal("diagnostic failure affected durable business state")
	}
}

func TestReviewSlowWorkerQuery(t *testing.T) {
	s := newTestService(t)
	var output bytes.Buffer
	s.logger, _ = NewJSONLogger(&output, s.cfg)
	conn, err := s.db.sql.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	done := make(chan error, 1)
	go func() { done <- s.ProcessPending() }()
	deadline := time.Now().Add(2 * time.Second)
	for s.db.sql.Stats().WaitCount == 0 {
		if time.Now().After(deadline) {
			t.Fatal("worker did not reach database")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(120 * time.Millisecond)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"event":"database.slow_query"`) {
		t.Fatal("real query wait was not instrumented")
	}
}
