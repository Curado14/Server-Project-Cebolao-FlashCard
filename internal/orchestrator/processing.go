package orchestrator

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"image/jpeg"
	"os"
	"path/filepath"
	"time"
)

// StartWorkers polls SQLite so a restart never depends on an in-memory wake-up.
func (s *Service) StartWorkers(ctx context.Context, workers int) {
	if workers < 1 {
		workers = 1
	}
	for i := 0; i < workers; i++ {
		go func() {
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					_ = s.ProcessPending()
				}
			}
		}()
	}
}

// ProcessPending executes one durable job. Repeated execution is guarded by operation_key and artifact uniqueness.
func (s *Service) ProcessPending() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var job, photo, original string
	err := s.db.sql.QueryRow(`SELECT j.id,j.photo_id,p.original_path FROM jobs j JOIN photos p ON p.id=j.photo_id WHERE j.status IN ('PENDING','RETRY_WAIT') AND j.available_at<=? ORDER BY j.created_at LIMIT 1`, time.Now().UTC().Format(time.RFC3339Nano)).Scan(&job, &photo, &original)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err = s.db.sql.Exec(`UPDATE jobs SET status='RUNNING',lease_owner='local-worker',lease_until=?,attempt_count=attempt_count+1,updated_at=? WHERE id=? AND status IN ('PENDING','RETRY_WAIT')`, now.Add(5*time.Minute), now, job); err != nil {
		return err
	}
	var attemptNo int
	if err = s.db.sql.QueryRow(`SELECT attempt_count FROM jobs WHERE id=?`, job).Scan(&attemptNo); err != nil {
		return err
	}
	if _, err = s.db.sql.Exec(`INSERT INTO job_attempts(job_id,attempt_no,event_type,started_at,result) VALUES(?,?,?,?,'RUNNING')`, job, attemptNo, "START", now.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	f, err := os.Open(original)
	if err != nil {
		return s.failJob(job, err)
	}
	img, err := jpeg.Decode(f)
	f.Close()
	if err != nil {
		return s.failJob(job, err)
	}
	_ = img
	artifact := id("artifact")
	file := id("file")
	dir := filepath.Join(s.cfg.Root, "processed", safePart(s.cfg.EventID), "sports-web-v1")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return s.failJob(job, err)
	}
	path := filepath.Join(dir, artifact+".jpg")
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return s.failJob(job, err)
	}
	err = jpeg.Encode(out, img, &jpeg.Options{Quality: 88})
	if err == nil {
		err = out.Sync()
	}
	out.Close()
	if err != nil {
		return s.failJob(job, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s.failJob(job, err)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	tx, err := s.db.sql.Begin()
	if err != nil {
		return err
	}

	defer tx.Rollback()
	now = time.Now().UTC()
	_, err = tx.Exec(`INSERT INTO photo_files(id,photo_id,kind,path,size_bytes,sha256,created_at) VALUES(?,?,?,?,?,?,?)`, file, photo, "DERIVED", path, len(data), hash, now)
	if err == nil {
		_, err = tx.Exec(`INSERT INTO artifacts(id,photo_id,file_id,preset_id,processor,processor_version,status,sha256,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, artifact, photo, file, "sports-web-v1", "jpeg", "1", "READY", hash, now)
	}
	if err == nil {
		_, err = tx.Exec(`UPDATE jobs SET status='SUCCEEDED',lease_owner=NULL,lease_until=NULL,updated_at=? WHERE id=?`, now, job)
	}
	if err == nil {
		_, err = tx.Exec(`UPDATE photos SET status='READY' WHERE id=?`, photo)
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO job_attempts(job_id,attempt_no,event_type,started_at,finished_at,result) VALUES(?,?,?,?,?,'SUCCESS')`, job, attemptNo, "FINISH", now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Service) failJob(job string, cause error) error {
	now := time.Now().UTC()
	var attemptNo int
	if err := s.db.sql.QueryRow(`SELECT attempt_count FROM jobs WHERE id=?`, job).Scan(&attemptNo); err != nil {
		return err
	}
	_, err := s.db.sql.Exec(`UPDATE jobs SET status=CASE WHEN attempt_count>=max_attempts THEN 'DEAD' ELSE 'RETRY_WAIT' END,last_error_code='processing_failed',last_error_message=?,lease_owner=NULL,lease_until=NULL,available_at=?,updated_at=? WHERE id=?`, sanitizeError(cause), now.Add(time.Second).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), job)
	if err == nil {
		result := "RETRY"
		if attemptNo >= 1 {
			var max int
			_ = s.db.sql.QueryRow(`SELECT max_attempts FROM jobs WHERE id=?`, job).Scan(&max)
			if attemptNo >= max {
				result = "FAILURE"
			}
		}
		_, err = s.db.sql.Exec(`INSERT INTO job_attempts(job_id,attempt_no,event_type,started_at,finished_at,result,error_code,error_message) VALUES(?,?,?,?,?,?,?,?)`, job, attemptNo, "FINISH", now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), result, "processing_failed", sanitizeError(cause))
	}
	return err
}
func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > 160 {
		msg = msg[:160]
	}
	return fmt.Sprintf("%s", msg)
}
