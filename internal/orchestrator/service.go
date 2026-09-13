package orchestrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"log/slog"
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
	cfg             Config
	db              *DB
	logger          *slog.Logger
	metricsRegistry *metricsRegistry
	mu              sync.Mutex
	ready           bool
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
	metrics := newMetricsRegistry()
	logger, err := newJSONLogger(os.Stdout, cfg, func() { metrics.loggingErrors.Add(1) })
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Service{cfg: cfg, db: db, logger: logger, metricsRegistry: metrics}
	s.log(nil, slog.LevelInfo, "service.started", "service started", "service")
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
	s.log(nil, slog.LevelInfo, "service.ready", "service ready", "service")
	return s, nil
}
func (s *Service) Close() error {
	s.log(nil, slog.LevelInfo, "shutdown.started", "shutdown started", "service")
	err := s.db.Close()
	s.log(nil, slog.LevelInfo, "shutdown.completed", "shutdown completed", "service")
	return err
}
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
	return s.observeHTTP(AccessLog(mux, s.logger))
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	if p, ok := v.(problem); ok {
		w.Header().Set("X-PhotoEdge-Problem-Code", p.Code)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Service) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "alive", "version": effectiveVersion(), "instance_id": s.cfg.InstanceID})
}
func (s *Service) readyCheck(w http.ResponseWriter, _ *http.Request) {
	if !s.ready {
		writeJSON(w, 503, problem{"about:blank", "not ready", 503, "not_ready"})
		return
	}
	if err := s.checkReadiness(); err != nil {
		s.log(nil, slog.LevelWarn, "service.not_ready", "service not ready", "readiness", "error_code", readinessCode(err))
		writeJSON(w, 503, problem{"about:blank", "not ready", 503, "storage_unavailable"})
		return
	}
	s.log(nil, slog.LevelInfo, "service.ready", "service ready", "readiness")
	writeJSON(w, 200, map[string]any{"status": "ready", "database": "ok", "storage": "ok", "version": effectiveVersion(), "instance_id": s.cfg.InstanceID})
}
func (s *Service) metrics(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || (host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback())) {
		writeJSON(w, 403, problem{"about:blank", "forbidden", 403, "forbidden"})
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	s.writeMetrics(w)
}

func (s *Service) observeHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		s.metricsRegistry.observeHTTPDuration(float64(time.Since(started).Milliseconds()))
	})
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
		s.reject(r, "method_not_allowed")
		writeJSON(w, 405, problem{"about:blank", "method not allowed", 405, "method_not_allowed"})
		return
	}
	if err := s.checkReadiness(); !s.ready || err != nil {
		s.reject(r, "not_ready")
		writeJSON(w, 503, problem{"about:blank", "not ready", 503, "not_ready"})
		return
	}
	if !s.auth(r) {
		s.reject(r, "unauthorized")
		s.log(r, slog.LevelWarn, "auth.failed", "authentication failed", "ingestion_api", "error_code", "unauthorized")
		writeJSON(w, 401, problem{"about:blank", "unauthorized", 401, "unauthorized"})
		return
	}
	if r.Header.Get("Idempotency-Key") == "" {
		s.validationFailed(r, "missing_idempotency_key")
		writeJSON(w, 400, problem{"about:blank", "missing idempotency key", 400, "missing_idempotency_key"})
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data;") {
		s.validationFailed(r, "unsupported_media_type")
		writeJSON(w, 415, problem{"about:blank", "multipart required", 415, "unsupported_media_type"})
		return
	}
	if err := r.ParseMultipartForm(s.cfg.MaxUploadBytes + 1024); err != nil {
		s.validationFailed(r, "payload_too_large")
		writeJSON(w, 413, problem{"about:blank", "payload too large", 413, "payload_too_large"})
		return
	}
	f, hdr, err := r.FormFile("photo")
	if err != nil {
		s.validationFailed(r, "missing_photo")
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
		code := "photo_rejected"
		if errors.Is(err, errConflict) {
			status = 409
			code = "idempotency_conflict"
		}
		if errors.Is(err, errLimit) {
			status = 413
			code = "payload_too_large"
		}
		if errors.Is(err, errDB) {
			status = 503
			code = "storage_unavailable"
			s.databaseUnavailable(r, "ingestion_api")
		}
		if status == 422 || status == 413 {
			s.validationFailed(r, code)
		} else {
			s.reject(r, code)
		}
		writeJSON(w, status, problem{"about:blank", "photo rejected", status, "photo_rejected"})
		return
	}
	s.metricsRegistry.photosAccepted.Add(1)
	writeJSON(w, 202, out)
}

var errConflict = errors.New("idempotency conflict")
var errLimit = errors.New("upload limit")
var errDB = errors.New("database unavailable")

type auditEvent struct {
	EntityType string
	EntityID   string
	Action     string
	ActorType  string
	ActorID    string
	RequestID  string
	JobID      string
	EventID    string
	CameraID   string
	Status     string
	ErrorCode  string
	Metadata   map[string]any
	CreatedAt  string
}

type auditExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func (s *Service) audit(tx auditExecer, ev auditEvent) error {
	if ev.CreatedAt == "" {
		ev.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	metadata := "{}"
	if len(ev.Metadata) > 0 {
		data, err := json.Marshal(ev.Metadata)
		if err != nil {
			return err
		}
		metadata = string(data)
	}
	if tx == nil {
		tx = s.db.sql
	}
	_, err := tx.Exec(`INSERT INTO audit_events(entity_type,entity_id,action,actor_type,actor_id,request_id,metadata_json,created_at,job_id,event_id,camera_id,status,error_code) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ev.EntityType, ev.EntityID, ev.Action, ev.ActorType, nullString(ev.ActorID), nullString(ev.RequestID), metadata, ev.CreatedAt, nullString(ev.JobID), nullString(ev.EventID), nullString(ev.CameraID), nullString(ev.Status), nullString(ev.ErrorCode))
	if err != nil {
		s.metricsRegistry.sqliteErrors.Add(1)
	}
	return err
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *Service) reject(r *http.Request, code string) {
	s.metricsRegistry.photosRejected.Add(1)
	s.log(r, slog.LevelWarn, "photo.rejected", "photo rejected", "ingestion_api", "error_code", code)
}

func (s *Service) validationFailed(r *http.Request, code string) {
	s.metricsRegistry.photosRejected.Add(1)
	s.log(r, slog.LevelWarn, "photo.validation_failed", "photo validation failed", "ingestion_api", "error_code", code)
	s.log(r, slog.LevelWarn, "photo.rejected", "photo rejected", "ingestion_api", "error_code", code)
}

func (s *Service) storageWriteFailed(r *http.Request, code string) {
	s.log(r, slog.LevelError, "storage.write_failed", "storage write failed", "storage", "error_code", code)
}

func (s *Service) databaseUnavailable(r *http.Request, component string) {
	s.log(r, slog.LevelError, "database.unavailable", "database unavailable", component, "error_code", "database_unavailable")
}

func (s *Service) logSlowQuery(duration time.Duration, component string) {
	if duration > 100*time.Millisecond {
		s.log(nil, slog.LevelWarn, "database.slow_query", "database slow query", component, "duration_ms", duration.Milliseconds())
	}
}

func (s *Service) log(r *http.Request, level slog.Level, event, msg, component string, attrs ...any) {
	if s.logger == nil {
		return
	}
	base := []any{"event", event, "component", component}
	if r != nil {
		if requestID := requestIDFromContext(r.Context()); requestID != "" {
			base = append(base, "request_id", requestID)
		}
	}
	base = append(base, attrs...)
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	s.logger.Log(ctx, level, msg, base...)
}

func readinessCode(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "migration") || strings.Contains(msg, "audit_events"):
		return "migration_pending"
	case strings.Contains(msg, "free space"):
		return "storage_low_space"
	case strings.Contains(msg, "integrity"):
		return "database_integrity_failed"
	default:
		return "storage_unavailable"
	}
}

func (s *Service) ingest(r *http.Request, event, filename string, src io.Reader) (PhotoResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	started := time.Now()
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
		if err := s.audit(nil, auditEvent{
			EntityType: "PHOTO",
			EntityID:   pid,
			Action:     "REPLAYED",
			ActorType:  "DEVICE",
			ActorID:    s.cfg.DeviceID,
			RequestID:  requestIDFromContext(r.Context()),
			EventID:    event,
			CameraID:   s.cfg.DeviceID,
			Status:     status,
			Metadata:   map[string]any{"idempotency_key_hash": idempotencyHash(key)},
			CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		}); err != nil {
			return PhotoResponse{}, errDB
		}
		s.log(r, slog.LevelInfo, "idempotency.replayed", "idempotency replayed", "ingestion_api",
			"photo_id", pid,
			"event_id", event,
			"camera_id", s.cfg.DeviceID,
			"idempotency_key_hash", idempotencyHash(key),
		)
		return PhotoResponse{pid, "queued", received, "/api/v1/photos/" + pid}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		s.databaseUnavailable(r, "ingestion_api")
		return PhotoResponse{}, errDB
	}
	pid = id("photo")
	part, n, _, err := writeBlob(s.cfg.Root, pid, bytes.NewReader(raw), s.cfg.MaxUploadBytes)
	if err != nil {
		if errors.Is(err, errLimit) {
			return PhotoResponse{}, err
		}
		s.storageWriteFailed(r, "original_write_failed")
		return PhotoResponse{}, err
	}
	f, err := os.Open(part)
	if err != nil {
		s.storageWriteFailed(r, "temporary_open_failed")
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
		s.storageWriteFailed(r, "original_rename_failed")
		return PhotoResponse{}, err
	}
	committed, commitAttempted := false, false
	// This path belongs only to this attempt; deferred rollback runs before cleanup.
	defer func() {
		if committed {
			return
		}
		if commitAttempted {
			var references int
			if err := s.db.sql.QueryRow(`SELECT (SELECT count(*) FROM photos WHERE original_path=?) + (SELECT count(*) FROM photo_files WHERE path=?)`, path, path).Scan(&references); err != nil {
				s.storageWriteFailed(r, "original_cleanup_unconfirmed")
				return
			}
			if references != 0 {
				return
			}
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.storageWriteFailed(r, "original_cleanup_failed")
		}
	}()
	s.metricsRegistry.observePersistDuration(float64(time.Since(started).Milliseconds()))
	s.log(r, slog.LevelInfo, "photo.persisted", "photo persisted", "ingestion_api",
		"photo_id", pid,
		"event_id", event,
		"camera_id", s.cfg.DeviceID,
		"bytes", n,
		"mime_type", "image/jpeg",
		"sha256_prefix", hash[:16],
		"duration_ms", time.Since(started).Milliseconds(),
	)
	received = time.Now().UTC().Format(time.RFC3339Nano)
	job := id("job")
	file := id("file")
	tx, err := s.db.sql.Begin()
	if err != nil {
		s.databaseUnavailable(r, "ingestion_api")
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
		err = s.audit(tx, auditEvent{
			EntityType: "PHOTO",
			EntityID:   pid,
			Action:     "ACCEPTED",
			ActorType:  "DEVICE",
			ActorID:    s.cfg.DeviceID,
			RequestID:  requestIDFromContext(r.Context()),
			JobID:      job,
			EventID:    event,
			CameraID:   s.cfg.DeviceID,
			Status:     "STORED",
			Metadata: map[string]any{
				"idempotency_key_hash": idempotencyHash(key),
				"media_type":           "image/jpeg",
				"sha256_prefix":        hash[:16],
			},
			CreatedAt: received,
		})
	}
	if err != nil {
		s.databaseUnavailable(r, "ingestion_api")
		return PhotoResponse{}, errDB
	}
	commitAttempted = true
	if err = tx.Commit(); err != nil {
		s.databaseUnavailable(r, "ingestion_api")
		return PhotoResponse{}, errDB
	}
	committed = true
	s.log(r, slog.LevelInfo, "job.queued", "job queued", "ingestion_api",
		"photo_id", pid,
		"job_id", job,
		"event_id", event,
		"camera_id", s.cfg.DeviceID,
	)
	s.log(r, slog.LevelInfo, "photo.accepted", "photo accepted", "ingestion_api",
		"photo_id", pid,
		"job_id", job,
		"event_id", event,
		"camera_id", s.cfg.DeviceID,
		"status_code", 202,
		"bytes", n,
		"mime_type", "image/jpeg",
		"duration_ms", time.Since(started).Milliseconds(),
	)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rows, err := s.db.sql.Query(`SELECT id,photo_id FROM jobs WHERE status='RUNNING' AND lease_until < ?`, now)
	if err != nil {
		return err
	}
	type recoveredJob struct{ id, photo string }
	var recovered []recoveredJob
	for rows.Next() {
		var job recoveredJob
		if err := rows.Scan(&job.id, &job.photo); err != nil {
			rows.Close()
			return err
		}
		recovered = append(recovered, job)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, job := range recovered {
		tx, err := s.db.sql.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE jobs SET status='PENDING',lease_owner=NULL,lease_until=NULL,updated_at=? WHERE id=?`, now, job.id); err != nil {
			tx.Rollback()
			return err
		}
		if err := s.audit(tx, auditEvent{EntityType: "JOB", EntityID: job.id, Action: "RECOVERED_AFTER_RESTART", ActorType: "SYSTEM", JobID: job.id, Status: "PENDING", CreatedAt: now}); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		s.log(nil, slog.LevelWarn, "job.recovered_after_restart", "job recovered after restart", "recovery", "photo_id", job.photo, "job_id", job.id)
	}
	rows, err = s.db.sql.Query(`SELECT id,id,original_path,sha256,status FROM photos WHERE status IN ('RECEIVING','STORED','PROCESSING','READY','EXPORTING','EXPORTED')`)
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
		s.metricsRegistry.sqliteErrors.Add(1)
		s.databaseUnavailable(nil, "readiness")
		return err
	}
	if err := requiredSchemaReady(s.db.sql); err != nil {
		s.databaseUnavailable(nil, "readiness")
		return err
	}
	var integrity string
	queryStarted := time.Now()
	if err := s.db.sql.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		if err != nil {
			s.metricsRegistry.sqliteErrors.Add(1)
			s.databaseUnavailable(nil, "readiness")
		}
		return errors.New("database integrity check failed")
	}
	s.logSlowQuery(time.Since(queryStarted), "readiness")
	free, err := freeSpace(s.cfg.Root)
	if err != nil {
		return fmt.Errorf("free space measurement failed: %w", err)
	}
	s.metricsRegistry.setStorageFree(free)
	if free <= s.cfg.MinFreeBytes {
		s.log(nil, slog.LevelWarn, "storage.low_space", "storage free space below configured minimum", "storage",
			"bytes", free,
			"error_code", "storage_low_space",
		)
		return fmt.Errorf("free space below minimum")
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
		err = s.audit(tx, auditEvent{EntityType: "PHOTO", EntityID: photo, Action: "QUARANTINED", ActorType: "SYSTEM", Status: "QUARANTINED", ErrorCode: code, Metadata: map[string]any{"reason_code": code}, CreatedAt: now})
	}
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.metricsRegistry.jobsQuarantined.Add(1)
	s.log(nil, slog.LevelError, "job.quarantined", "photo quarantined", "recovery", "photo_id", photo, "error_code", code)
	return nil
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
			return s.quarantineFile(path, dest, "ORPHAN_ORIGINAL")
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
		return s.quarantineFile(path, dest, "STALE_PART")
	})
}

func (s *Service) quarantineFile(source, destination, code string) error {
	tx, err := s.db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.audit(tx, auditEvent{EntityType: "FILE", EntityID: id("file"), Action: "QUARANTINED", ActorType: "SYSTEM", Status: "QUARANTINED", ErrorCode: code, Metadata: map[string]any{"reason_code": code}}); err != nil {
		return err
	}
	if err = os.Rename(source, destination); err != nil {
		s.storageWriteFailed(nil, "quarantine_move_failed")
		return err
	}
	// SQLite and rename cannot share a transaction; restore discovery on commit failure.
	if err = tx.Commit(); err != nil {
		if restoreErr := os.Rename(destination, source); restoreErr != nil {
			s.storageWriteFailed(nil, "quarantine_restore_failed")
			return errors.Join(err, restoreErr)
		}
		return err
	}
	s.metricsRegistry.jobsQuarantined.Add(1)
	s.log(nil, slog.LevelError, "job.quarantined", "file quarantined", "recovery", "error_code", code)
	return nil
}
