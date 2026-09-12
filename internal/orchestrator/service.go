package orchestrator

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Service struct {
	cfg                Config
	db                 *DB
	mu                 sync.Mutex
	ready              bool
	accepted, rejected atomic.Int64
}

var sequence uint64

type PhotoResponse struct {
	PhotoID    string `json:"photo_id"`
	Status     string `json:"status"`
	ReceivedAt string `json:"received_at"`
	StatusURL  string `json:"status_url"`
}
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   string `json:"code"`
}

func Open(cfg Config) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := ensureRoots(cfg.Root); err != nil {
		return nil, err
	}
	db, err := openDB(filepath.Join(cfg.Root, "db", "orchestrator.db"))
	if err != nil {
		return nil, err
	}
	s := &Service{cfg: cfg, db: db}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = db.sql.Exec(`INSERT OR IGNORE INTO events(id,name,status,created_at) VALUES(?,?,?,?)`, cfg.EventID, "Local event", "ACTIVE", now)
	if err == nil {
		_, err = db.sql.Exec(`INSERT OR IGNORE INTO devices(id,name,token_hash,enabled) VALUES(?,?,?,1)`, cfg.DeviceID, "Local camera", hashToken(cfg.DeviceToken))
	}
	if err == nil {
		_, err = db.sql.Exec(`INSERT OR IGNORE INTO presets(id,name,version,config_json,config_hash,active,created_at) VALUES('sports-web-v1','sports-web',1,'{"output_format":"jpeg","quality":88}', 'builtin-sports-web-v1',1,?)`, now)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = s.recover(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.checkReadiness(); err != nil {
		db.Close()
		return nil, err
	}
	s.ready = true
	return s, nil
}
func (s *Service) Close() error { return s.db.Close() }
func hashToken(t string) string { h := sha256.Sum256([]byte(t)); return hex.EncodeToString(h[:]) }
func id(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), atomic.AddUint64(&sequence, 1))
}
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", s.live)
	mux.HandleFunc("/health/ready", s.readyCheck)
	mux.HandleFunc("/metrics", s.metrics)
	mux.HandleFunc("/api/v1/photos", s.photos)
	mux.HandleFunc("/api/v1/photos/", s.photo)
	mux.HandleFunc("/api/v1/events/", s.event)
	return mux
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Service) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "alive"})
}
func (s *Service) readyCheck(w http.ResponseWriter, _ *http.Request) {
	if !s.ready {
		writeJSON(w, 503, problem{"about:blank", "not ready", 503, "not_ready"})
		return
	}
	if err := s.checkReadiness(); err != nil {
		writeJSON(w, 503, problem{"about:blank", "not ready", 503, "storage_unavailable"})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ready", "database": "ok", "storage": "ok"})
}
func (s *Service) metrics(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || (host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback())) {
		writeJSON(w, 403, problem{"about:blank", "forbidden", 403, "forbidden"})
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "photoedge_accepted_total %d\nphotoedge_rejected_total %d\n", s.accepted.Load(), s.rejected.Load())
}
func (s *Service) auth(r *http.Request) bool {
	a := r.Header.Get("Authorization")
	if !strings.HasPrefix(a, "Bearer ") {
		return false
	}
	var storedHash string
	err := s.db.sql.QueryRow(`SELECT token_hash FROM devices WHERE id=? AND enabled=1`, s.cfg.DeviceID).Scan(&storedHash)
	if err != nil {
		return false
	}
	providedHash := hashToken(strings.TrimPrefix(a, "Bearer "))
	return subtle.ConstantTimeCompare([]byte(providedHash), []byte(storedHash)) == 1
}
func (s *Service) photos(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, problem{"about:blank", "method not allowed", 405, "method_not_allowed"})
		return
	}
	if !s.ready || s.checkReadiness() != nil {
		s.rejected.Add(1)
		writeJSON(w, 503, problem{"about:blank", "not ready", 503, "not_ready"})
		return
	}
	if !s.auth(r) {
		s.rejected.Add(1)
		writeJSON(w, 401, problem{"about:blank", "unauthorized", 401, "unauthorized"})
		return
	}
	if r.Header.Get("Idempotency-Key") == "" {
		writeJSON(w, 400, problem{"about:blank", "missing idempotency key", 400, "missing_idempotency_key"})
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data;") {
		writeJSON(w, 415, problem{"about:blank", "multipart required", 415, "unsupported_media_type"})
		return
	}
	if err := r.ParseMultipartForm(s.cfg.MaxUploadBytes + 1024); err != nil {
		s.rejected.Add(1)
		writeJSON(w, 413, problem{"about:blank", "payload too large", 413, "payload_too_large"})
		return
	}
	f, hdr, err := r.FormFile("photo")
	if err != nil {
		writeJSON(w, 400, problem{"about:blank", "photo is required", 400, "missing_photo"})
		return
	}
	defer f.Close()
	event := s.cfg.EventID
	if v := r.FormValue("event_id"); v != "" {
		event = safePart(v)
	}
	filename := safePart(hdr.Filename)
	out, err := s.ingest(r, event, filename, f)

	if err != nil {
		status := 422
		if errors.Is(err, errConflict) {
			status = 409
		}
		if errors.Is(err, errLimit) {
			status = 413
		}
		if errors.Is(err, errDB) {
			status = 503
		}
		s.rejected.Add(1)
		writeJSON(w, status, problem{"about:blank", "photo rejected", status, "photo_rejected"})
		return
	}
	s.accepted.Add(1)
	writeJSON(w, 202, out)
}

var errConflict = errors.New("idempotency conflict")
var errLimit = errors.New("upload limit")
var errDB = errors.New("database unavailable")

func (s *Service) ingest(r *http.Request, event, filename string, src io.Reader) (PhotoResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := r.Header.Get("Idempotency-Key")
	raw, err := io.ReadAll(io.LimitReader(src, s.cfg.MaxUploadBytes+1))
	if err != nil {
		return PhotoResponse{}, err
	}
	if int64(len(raw)) > s.cfg.MaxUploadBytes {
		return PhotoResponse{}, errLimit
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	var pid, oldHash, status, received string
	err = s.db.sql.QueryRow(`SELECT id,sha256,status,received_at FROM photos WHERE device_id=? AND idempotency_key=?`, s.cfg.DeviceID, key).Scan(&pid, &oldHash, &status, &received)
	if err == nil {
		if oldHash != hash {
			return PhotoResponse{}, errConflict
		}
		return PhotoResponse{pid, "queued", received, "/api/v1/photos/" + pid}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PhotoResponse{}, errDB
	}
	pid = id("photo")
	part, n, _, err := writeBlob(s.cfg.Root, pid, strings.NewReader(string(raw)), s.cfg.MaxUploadBytes)
	if err != nil {
		return PhotoResponse{}, err
	}
	f, err := os.Open(part)
	if err != nil {
		return PhotoResponse{}, err
	}
	cfg, err := jpeg.DecodeConfig(f)
	f.Close()
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > s.cfg.MaxPixels {
		os.Remove(part)
		return PhotoResponse{}, fmt.Errorf("invalid jpeg")
	}
	path, err := durableOriginal(s.cfg.Root, event, pid, part)
	if err != nil {
		return PhotoResponse{}, err
	}
	received = time.Now().UTC().Format(time.RFC3339Nano)
	job := id("job")
	file := id("file")
	tx, err := s.db.sql.Begin()
	if err != nil {
		return PhotoResponse{}, errDB
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO photos(id,event_id,device_id,idempotency_key,original_filename,original_path,media_type,size_bytes,sha256,received_at,status,width,height) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, pid, event, s.cfg.DeviceID, key, filename, path, "image/jpeg", n, hash, received, "STORED", cfg.Width, cfg.Height)
	if err == nil {
		_, err = tx.Exec(`INSERT INTO ingestion_requests(id,photo_id,device_id,idempotency_key,request_hash,status,created_at) VALUES(?,?,?,?,?,?,?)`, id("request"), pid, s.cfg.DeviceID, key, hash, "ACCEPTED", received)
	}
	if err == nil {
		_, err = tx.Exec(`INSERT INTO photo_files(id,photo_id,kind,path,size_bytes,sha256,created_at) VALUES(?,?,?,?,?,?,?)`, file, pid, "ORIGINAL", path, n, hash, received)
	}
	if err == nil {
		_, err = tx.Exec(`INSERT INTO jobs(id,photo_id,kind,operation_key,status,attempt_count,max_attempts,available_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, job, pid, "PROCESS", pid+":process", "PENDING", 0, s.cfg.MaxAttempts, received, received, received)
	}
	if err == nil {
		_, err = tx.Exec(`INSERT INTO audit_events(entity_type,entity_id,action,actor_type,actor_id,request_id,metadata_json,created_at) VALUES('PHOTO',?,'ACCEPTED','DEVICE',?,?,?,?)`, pid, s.cfg.DeviceID, key, `{"media_type":"image/jpeg"}`, received)
	}
	if err != nil {
		return PhotoResponse{}, errDB
	}
	if err = tx.Commit(); err != nil {
		return PhotoResponse{}, errDB
	}
	return PhotoResponse{pid, "queued", received, "/api/v1/photos/" + pid}, nil
}
func (s *Service) photo(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeJSON(w, 405, problem{"about:blank", "method not allowed", 405, "method_not_allowed"})
		return
	}
	pid := strings.TrimPrefix(r.URL.Path, "/api/v1/photos/")
	var status, path, hash string
	var size int64
	err := s.db.sql.QueryRow(`SELECT status,original_path,sha256,size_bytes FROM photos WHERE id=?`, pid).Scan(&status, &path, &hash, &size)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, 404, problem{"about:blank", "not found", 404, "not_found"})
		return
	}
	if err != nil {
		writeJSON(w, 503, problem{"about:blank", "storage unavailable", 503, "storage_unavailable"})
		return
	}
	writeJSON(w, 200, map[string]any{"photo_id": pid, "status": status, "original_path": path, "sha256": hash, "size_bytes": size})
}
func (s *Service) event(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/events/"), "/summary")
	var photos, jobs int
	_ = s.db.sql.QueryRow(`SELECT count(*) FROM photos WHERE event_id=?`, id).Scan(&photos)
	_ = s.db.sql.QueryRow(`SELECT count(*) FROM jobs WHERE status IN ('PENDING','RUNNING','RETRY_WAIT')`).Scan(&jobs)
	writeJSON(w, 200, map[string]any{"event_id": id, "photos": photos, "pending_jobs": jobs})
}
func (s *Service) recover() error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.sql.Exec(`UPDATE jobs SET status='PENDING',lease_owner=NULL,lease_until=NULL,updated_at=? WHERE status='RUNNING' AND lease_until < ?`, now, now); err != nil {
		return err
	}
	rows, err := s.db.sql.Query(`SELECT id,id,original_path,sha256,status FROM photos WHERE status IN ('RECEIVING','STORED','PROCESSING','READY','EXPORTING','EXPORTED')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type item struct{ id, photo, path, hash, status string }
	var items []item
	for rows.Next() {
		var v item
		if err := rows.Scan(&v.id, &v.photo, &v.path, &v.hash, &v.status); err != nil {
			return err
		}
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, v := range items {
		data, e := os.ReadFile(v.path)
		if e != nil {
			if v.status == "STORED" || v.status == "RECEIVING" {
				if err := s.quarantine(v.photo, "", "ORIGINAL_MISSING", "original file is missing"); err != nil {
					return err
				}
			}
			continue
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != v.hash {
			if err := s.quarantine(v.photo, "", "ORIGINAL_HASH_MISMATCH", "original hash does not match"); err != nil {
				return err
			}
		}
	}
	if err := s.reconcileOrphans(); err != nil {
		return err
	}
	return s.reconcileTempParts()
}

func (s *Service) checkReadiness() error {
	if _, err := os.Stat(s.cfg.Root); err != nil {
		return err
	}
	if err := s.db.sql.Ping(); err != nil {
		return err
	}
	var integrity string
	if err := s.db.sql.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		return errors.New("database integrity check failed")
	}
	probe := filepath.Join(s.cfg.Root, ".readiness-probe-"+id("check"))
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Remove(probe)
}
func (s *Service) quarantine(photo, file, code, message string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	q := id("quarantine")
	tx, err := s.db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE photos SET status='QUARANTINED',last_error_code=? WHERE id=?`, code, photo); err == nil {
		_, err = tx.Exec(`INSERT INTO quarantine_entries(id,photo_id,file_id,reason_code,reason_message,created_at) VALUES(?,?,?,?,?,?)`, q, photo, nil, code, message, now)
	}
	if err == nil {
		_, err = tx.Exec(`INSERT INTO audit_events(entity_type,entity_id,action,actor_type,metadata_json,created_at) VALUES('PHOTO',?,'QUARANTINED','SYSTEM',?,?)`, photo, `{"reason_code":"`+code+`"}`, now)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Service) reconcileOrphans() error {
	root := filepath.Join(s.cfg.Root, "originals")
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		var n int
		if e := s.db.sql.QueryRow("SELECT count(*) FROM photo_files WHERE path=?", path).Scan(&n); e != nil {
			return e
		}
		if n == 0 {
			dest := filepath.Join(s.cfg.Root, "quarantine", safePart(filepath.Base(path))+"-"+id("orphan"))
			if e := os.Rename(path, dest); e != nil {
				return e
			}
			now := time.Now().UTC().Format(time.RFC3339Nano)
			_, e := s.db.sql.Exec(`INSERT INTO audit_events(entity_type,entity_id,action,actor_type,metadata_json,created_at) VALUES('FILE',?,'QUARANTINED','SYSTEM',?,?)`, path, `{"reason_code":"ORPHAN_ORIGINAL"}`, now)
			return e
		}
		return nil
	})
}
func (s *Service) reconcileTempParts() error {
	root := filepath.Join(s.cfg.Root, "incoming", ".tmp")
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".part") {
			return nil
		}
		dest := filepath.Join(s.cfg.Root, "quarantine", safePart(info.Name())+"-"+id("part"))
		if err := os.Rename(path, dest); err != nil {
			return err
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		_, err = s.db.sql.Exec(`INSERT INTO audit_events(entity_type,entity_id,action,actor_type,metadata_json,created_at) VALUES('FILE',?,'QUARANTINED','SYSTEM',?,?)`, path, `{"reason_code":"STALE_PART"}`, now)
		return err
	})
}
