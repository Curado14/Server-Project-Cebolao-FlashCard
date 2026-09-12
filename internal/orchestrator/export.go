package orchestrator

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type localDestination struct {
	Path string `json:"path"`
}

// ExportArtifact is deliberately local-only. The receipt is committed only after hash verification.
func (s *Service) ExportArtifact(artifactID, destinationID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var source, cfgJSON, typ string
	if err := s.db.sql.QueryRow(`SELECT f.path,d.config_json,d.type FROM artifacts a JOIN photo_files f ON f.id=a.file_id JOIN export_destinations d ON d.enabled=1 WHERE a.id=? AND d.id=?`, artifactID, destinationID).Scan(&source, &cfgJSON, &typ); err != nil {
		return err
	}
	if typ != "LOCAL_FOLDER" && typ != "MANIFEST" {
		return errors.New("external destination rejected")
	}
	var cfg localDestination
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil || cfg.Path == "" {
		return errors.New("invalid local destination")
	}
	if !within(s.cfg.Root, cfg.Path) {
		return errors.New("destination outside storage root")
	}
	if err := os.MkdirAll(cfg.Path, 0700); err != nil {
		return err
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	var exportID, status, oldPath, receiptID, oldHash string
	now := time.Now().UTC().Format(time.RFC3339Nano)
	err = s.db.sql.QueryRow(`SELECT id,status,COALESCE(destination_path,''),COALESCE(receipt_id,''),COALESCE(sha256,'') FROM exports WHERE artifact_id=? AND destination_id=?`, artifactID, destinationID).Scan(&exportID, &status, &oldPath, &receiptID, &oldHash)
	if err == sql.ErrNoRows {
		exportID = id("export")
		status = "PENDING"
		if _, err = s.db.sql.Exec(`INSERT INTO exports(id,artifact_id,destination_id,status,created_at) VALUES(?,?,?,?,?)`, exportID, artifactID, destinationID, status, now); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if status == "VERIFIED" && oldPath != "" && oldHash == hash {
		if existing, e := os.ReadFile(oldPath); e == nil {
			checked := sha256.Sum256(existing)
			if hex.EncodeToString(checked[:]) == hash {
				return nil
			}
		}
	}
	var attempts int
	if err = s.db.sql.QueryRow(`SELECT count(*) FROM export_attempts WHERE export_id=?`, exportID).Scan(&attempts); err != nil {
		return err
	}
	attemptNo := attempts + 1
	now = time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = s.db.sql.Exec(`INSERT INTO export_attempts(export_id,attempt_no,started_at,result) VALUES(?,?,?,'RUNNING')`, exportID, attemptNo, now); err != nil {
		return err
	}
	dst := filepath.Join(cfg.Path, artifactID+".jpg")
	tmp := dst + ".part"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		s.finishExportAttempt(exportID, attemptNo, "FAILURE", err)
		return err
	}
	if f, e := os.OpenFile(tmp, os.O_WRONLY, 0600); e == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	if err := os.Rename(tmp, dst); err != nil {
		s.finishExportAttempt(exportID, attemptNo, "RETRY", err)
		return err
	}
	check, e := os.ReadFile(dst)
	if e != nil {
		s.finishExportAttempt(exportID, attemptNo, "RETRY", e)
		return e
	}

	verify := sha256.Sum256(check)
	if hex.EncodeToString(verify[:]) != hash {
		s.finishExportAttempt(exportID, attemptNo, "FAILURE", errors.New("export hash mismatch"))
		return errors.New("export hash mismatch")
	}
	now = time.Now().UTC().Format(time.RFC3339Nano)
	tx, e := s.db.sql.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if receiptID == "" {
		receiptID = id("receipt")
	}
	_, e = tx.Exec(`UPDATE exports SET status='VERIFIED',destination_path=?,receipt_id=?,sha256=?,exported_at=?,verified_at=? WHERE id=?`, dst, receiptID, hash, now, now, exportID)
	if e != nil {
		return e
	}
	_, e = tx.Exec(`UPDATE photos SET status='EXPORTED' WHERE id=(SELECT photo_id FROM artifacts WHERE id=?)`, artifactID)
	if e != nil {
		return e
	}
	if _, e = tx.Exec(`UPDATE export_attempts SET finished_at=?,result='SUCCESS',bytes_written=? WHERE export_id=? AND attempt_no=?`, now, len(data), exportID, attemptNo); e != nil {
		return e
	}
	return tx.Commit()
}

func (s *Service) finishExportAttempt(exportID string, attemptNo int, result string, cause error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	msg := ""
	if cause != nil {
		msg = sanitizeError(cause)
	}
	_, _ = s.db.sql.Exec(`UPDATE export_attempts SET finished_at=?,result=?,error_code=CASE WHEN ?='' THEN NULL ELSE 'export_failed' END,error_message=? WHERE export_id=? AND attempt_no=?`, now, result, msg, msg, exportID, attemptNo)
	_, _ = s.db.sql.Exec(`UPDATE exports SET status='PENDING' WHERE id=?`, exportID)
}

func (s *Service) RecoverJobs() error { return s.recover() }
