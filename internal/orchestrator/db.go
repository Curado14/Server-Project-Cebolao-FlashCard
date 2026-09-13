package orchestrator

import (
	"database/sql"
	"fmt"
	_ "modernc.org/sqlite"
	"time"
)

type DB struct{ sql *sql.DB }

func openDB(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec(`PRAGMA foreign_keys=ON; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, err
	}
	if err = migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{sql: db}, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS events(id TEXT PRIMARY KEY, name TEXT NOT NULL, status TEXT NOT NULL CHECK(status IN ('DRAFT','ACTIVE','CLOSED')), created_at TEXT NOT NULL) STRICT;
CREATE TABLE IF NOT EXISTS devices(id TEXT PRIMARY KEY, name TEXT NOT NULL, token_hash TEXT NOT NULL, enabled INTEGER NOT NULL CHECK(enabled IN(0,1)), last_seen_at TEXT) STRICT;
CREATE TABLE IF NOT EXISTS photos(id TEXT PRIMARY KEY, event_id TEXT NOT NULL REFERENCES events(id), device_id TEXT NOT NULL REFERENCES devices(id), idempotency_key TEXT NOT NULL, original_filename TEXT NOT NULL, original_path TEXT NOT NULL, media_type TEXT NOT NULL, size_bytes INTEGER NOT NULL, sha256 TEXT NOT NULL, received_at TEXT NOT NULL, status TEXT NOT NULL CHECK(status IN('RECEIVING','STORED','PROCESSING','READY','EXPORTING','EXPORTED','FAILED','RETRY_WAIT','QUARANTINED')), width INTEGER NOT NULL, height INTEGER NOT NULL, last_error_code TEXT, UNIQUE(device_id,idempotency_key)) STRICT;
CREATE INDEX IF NOT EXISTS idx_photos_event_received ON photos(event_id,received_at);
CREATE TABLE IF NOT EXISTS ingestion_requests(id TEXT PRIMARY KEY, photo_id TEXT NOT NULL REFERENCES photos(id), device_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, request_hash TEXT NOT NULL, status TEXT NOT NULL, created_at TEXT NOT NULL, UNIQUE(device_id,idempotency_key)) STRICT;
CREATE TABLE IF NOT EXISTS photo_files(id TEXT PRIMARY KEY, photo_id TEXT NOT NULL REFERENCES photos(id), kind TEXT NOT NULL, path TEXT NOT NULL, size_bytes INTEGER NOT NULL, sha256 TEXT NOT NULL, created_at TEXT NOT NULL, UNIQUE(photo_id,kind)) STRICT;
CREATE TABLE IF NOT EXISTS jobs(id TEXT PRIMARY KEY, photo_id TEXT NOT NULL REFERENCES photos(id), kind TEXT NOT NULL, operation_key TEXT NOT NULL UNIQUE, status TEXT NOT NULL CHECK(status IN('PENDING','RUNNING','RETRY_WAIT','SUCCEEDED','DEAD')), attempt_count INTEGER NOT NULL, max_attempts INTEGER NOT NULL, available_at TEXT NOT NULL, lease_owner TEXT, lease_until TEXT, last_error_code TEXT, last_error_message TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL) STRICT;
CREATE INDEX IF NOT EXISTS idx_jobs_queue ON jobs(status,available_at);
CREATE TABLE IF NOT EXISTS job_attempts(id INTEGER PRIMARY KEY, job_id TEXT NOT NULL REFERENCES jobs(id), attempt_no INTEGER NOT NULL, event_type TEXT NOT NULL CHECK(event_type IN('START','FINISH')), started_at TEXT NOT NULL, finished_at TEXT, result TEXT NOT NULL, error_code TEXT, error_message TEXT) STRICT;
CREATE TABLE IF NOT EXISTS presets(id TEXT PRIMARY KEY, name TEXT NOT NULL, version INTEGER NOT NULL, config_json TEXT NOT NULL, config_hash TEXT NOT NULL, active INTEGER NOT NULL CHECK(active IN(0,1)), created_at TEXT NOT NULL, UNIQUE(name,version)) STRICT;
CREATE TABLE IF NOT EXISTS artifacts(id TEXT PRIMARY KEY, photo_id TEXT NOT NULL REFERENCES photos(id), file_id TEXT NOT NULL REFERENCES photo_files(id), preset_id TEXT NOT NULL REFERENCES presets(id), processor TEXT NOT NULL, processor_version TEXT NOT NULL, status TEXT NOT NULL CHECK(status IN('CREATING','READY','INVALID','SUPERSEDED')), sha256 TEXT NOT NULL, created_at TEXT NOT NULL, UNIQUE(photo_id,preset_id,processor_version)) STRICT;
CREATE TABLE IF NOT EXISTS export_destinations(id TEXT PRIMARY KEY, name TEXT NOT NULL, type TEXT NOT NULL CHECK(type IN('LOCAL_FOLDER','MANIFEST')), config_json TEXT NOT NULL, enabled INTEGER NOT NULL CHECK(enabled IN(0,1)), created_at TEXT NOT NULL, updated_at TEXT NOT NULL) STRICT;
CREATE TABLE IF NOT EXISTS exports(id TEXT PRIMARY KEY, artifact_id TEXT NOT NULL REFERENCES artifacts(id), destination_id TEXT NOT NULL REFERENCES export_destinations(id), status TEXT NOT NULL CHECK(status IN('PENDING','EXPORTING','EXPORTED','VERIFIED','FAILED')), destination_path TEXT, receipt_id TEXT, sha256 TEXT, created_at TEXT NOT NULL, exported_at TEXT, verified_at TEXT, UNIQUE(artifact_id,destination_id)) STRICT;
CREATE TABLE IF NOT EXISTS export_attempts(id INTEGER PRIMARY KEY, export_id TEXT NOT NULL REFERENCES exports(id), attempt_no INTEGER NOT NULL, started_at TEXT NOT NULL, finished_at TEXT, result TEXT NOT NULL, bytes_written INTEGER, error_code TEXT, error_message TEXT, UNIQUE(export_id,attempt_no)) STRICT;
CREATE TABLE IF NOT EXISTS quarantine_entries(id TEXT PRIMARY KEY, photo_id TEXT REFERENCES photos(id), file_id TEXT REFERENCES photo_files(id), reason_code TEXT NOT NULL, reason_message TEXT, created_at TEXT NOT NULL, resolved_at TEXT, resolution TEXT) STRICT;
CREATE TABLE IF NOT EXISTS audit_events(id INTEGER PRIMARY KEY, entity_type TEXT NOT NULL, entity_id TEXT NOT NULL, action TEXT NOT NULL, actor_type TEXT NOT NULL, actor_id TEXT, request_id TEXT, metadata_json TEXT, created_at TEXT NOT NULL) STRICT;`)
	if err != nil {
		return err
	}
	if _, err = db.Exec(`INSERT OR IGNORE INTO schema_migrations(version,name,checksum,applied_at) VALUES(1,'initial','photoedge-v1',?)`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return migrateAuditV2(db)
}

func migrateAuditV2(db *sql.DB) error {
	columns, err := tableColumns(db, "audit_events")
	if err != nil {
		return err
	}
	for _, column := range []struct {
		name string
		ddl  string
	}{
		{"job_id", `ALTER TABLE audit_events ADD COLUMN job_id TEXT`},
		{"event_id", `ALTER TABLE audit_events ADD COLUMN event_id TEXT`},
		{"camera_id", `ALTER TABLE audit_events ADD COLUMN camera_id TEXT`},
		{"status", `ALTER TABLE audit_events ADD COLUMN status TEXT`},
		{"error_code", `ALTER TABLE audit_events ADD COLUMN error_code TEXT`},
	} {
		if !columns[column.name] {
			if _, err := db.Exec(column.ddl); err != nil {
				return err
			}
			columns[column.name] = true
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_audit_request ON audit_events(request_id, created_at);
CREATE INDEX IF NOT EXISTS idx_audit_job ON audit_events(job_id, created_at);`); err != nil {
		return err
	}
	_, err = db.Exec(`INSERT OR IGNORE INTO schema_migrations(version,name,checksum,applied_at) VALUES(2,'audit_correlation','photoedge-v2-audit-correlation',?)`, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

func requiredSchemaReady(db *sql.DB) error {
	var n int
	if err := db.QueryRow("SELECT count(*) FROM schema_migrations WHERE version=2").Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("schema migration v2 is pending")
	}
	columns, err := tableColumns(db, "audit_events")
	if err != nil {
		return err
	}
	for _, name := range []string{"job_id", "event_id", "camera_id", "status", "error_code"} {
		if !columns[name] {
			return fmt.Errorf("audit_events.%s is missing", name)
		}
	}
	return nil
}

func (d *DB) Close() error { return d.sql.Close() }
func (d *DB) pragma(name string) (string, error) {
	var v string
	err := d.sql.QueryRow("PRAGMA " + name).Scan(&v)
	return v, err
}
func wrap(err error) error { return fmt.Errorf("storage: %w", err) }
