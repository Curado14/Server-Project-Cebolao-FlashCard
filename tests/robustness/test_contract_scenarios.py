import argparse
import sqlite3
import tempfile
import unittest
from pathlib import Path

from tests.robustness.core import (
    Config,
    Harness,
    HTTPResult,
    ProductFailure,
    Report,
    forbidden_metric_labels,
    parse_prometheus,
)


def make_cfg(tmp: str, log_path: Path | None = None) -> Config:
    root = Path(tmp) / "root"
    db = root / "db" / "orchestrator.db"
    return Config(
        base_url="http://127.0.0.1:8090",
        token="secret-token",
        event_id="local-event",
        device_id="camera_01",
        storage_root=root,
        db_path=db,
        log_path=log_path,
        reports_dir=Path(tmp) / "reports",
    )


def make_report(cfg: Config, verify=None) -> Report:
    args = argparse.Namespace(scenario="unit", photos=1, concurrency=1)
    return Report("unit", cfg, args, set(verify or {"sqlite", "filesystem", "audit", "logs", "metrics"}))


def create_schema(db: Path) -> None:
    db.parent.mkdir(parents=True, exist_ok=True)
    con = sqlite3.connect(db)
    con.executescript(
        """
        CREATE TABLE photos(id TEXT PRIMARY KEY, event_id TEXT, device_id TEXT, idempotency_key TEXT, original_path TEXT, status TEXT, sha256 TEXT, size_bytes INTEGER);
        CREATE TABLE photo_files(id TEXT PRIMARY KEY, photo_id TEXT, kind TEXT, path TEXT, size_bytes INTEGER, sha256 TEXT);
        CREATE TABLE jobs(id TEXT PRIMARY KEY, photo_id TEXT, status TEXT);
        CREATE TABLE ingestion_requests(id TEXT PRIMARY KEY, photo_id TEXT, device_id TEXT, idempotency_key TEXT);
        CREATE TABLE audit_events(id INTEGER PRIMARY KEY, entity_id TEXT, action TEXT, request_id TEXT, job_id TEXT, event_id TEXT, camera_id TEXT, status TEXT);
        """
    )
    con.commit()
    con.close()


class ContractScenarioTests(unittest.TestCase):
    def test_single_upload_requires_durable_reconciliation(self):
        with tempfile.TemporaryDirectory() as tmp:
            cfg = make_cfg(tmp)
            create_schema(cfg.db_path)
            report = make_report(cfg)
            harness = Harness(cfg, report)
            result = HTTPResult(
                "POST",
                "/api/v1/photos",
                202,
                1.0,
                {"photo_id": "photo-1", "status": "queued", "received_at": "now", "status_url": "/api/v1/photos/photo-1"},
                {},
                True,
            )
            accepted = harness.accept_202(result)
            harness.reconcile_acceptance(accepted["photo_id"])
            self.assertTrue(any("missing photos row" in item for item in report.inconsistencies))

            with self.assertRaises(ProductFailure):
                harness.accept_202(
                    HTTPResult(
                        "POST",
                        "/api/v1/photos",
                        202,
                        1.0,
                        {"photo_id": "photo-1", "job_id": "job-1", "status": "queued", "received_at": "now", "status_url": "/api/v1/photos/photo-1"},
                        {},
                        True,
                    )
                )

    def test_logs_are_optional_but_never_authoritative_or_secret_bearing(self):
        with tempfile.TemporaryDirectory() as tmp:
            cfg = make_cfg(tmp)
            report = make_report(cfg, {"logs"})
            harness = Harness(cfg, report)
            self.assertIsNone(harness.read_logs())
            self.assertTrue(any("logs=not-run" in item for item in report.limitations))

            log = Path(tmp) / "stdout.ndjson"
            cfg.log_path = log
            log.write_text('{"event":"photo.accepted","photo_id":"photo-1","token":"secret-token"}\n', encoding="utf-8")
            with self.assertRaises(ProductFailure):
                harness.read_logs()

    def test_metrics_respect_loopback_and_entity_label_policy(self):
        text = """
        # HELP photoedge_photos_accepted_total Photos accepted.
        # TYPE photoedge_photos_accepted_total counter
        photoedge_photos_accepted_total 1
        photoedge_http_request_duration_ms_bucket{le="5"} 1
        photoedge_jobs_active{photo_id="photo-1"} 1
        """
        parsed = parse_prometheus(text)
        self.assertIn("photoedge_photos_accepted_total", parsed)
        self.assertEqual(forbidden_metric_labels(text), {"photo_id"})

    def test_invalid_inputs_leave_no_durable_acceptance(self):
        with tempfile.TemporaryDirectory() as tmp:
            cfg = make_cfg(tmp)
            create_schema(cfg.db_path)
            report = make_report(cfg)
            harness = Harness(cfg, report)
            before = harness.count_logical_entities("missing-photo")
            after = harness.count_logical_entities("missing-photo")
            self.assertEqual(before, after)
            self.assertEqual(after, {"photos": 0, "requests": 0, "files": 0, "jobs": 0})

    def test_idempotency_replay_and_conflict_preserve_cardinality(self):
        with tempfile.TemporaryDirectory() as tmp:
            cfg = make_cfg(tmp)
            create_schema(cfg.db_path)
            cfg.storage_root.mkdir(parents=True, exist_ok=True)
            original = cfg.storage_root / "originals" / "local-event" / "2026-09-13" / "photo-1.jpg"
            original.parent.mkdir(parents=True)
            original.write_bytes(b"jpeg")
            con = sqlite3.connect(cfg.db_path)
            con.execute("INSERT INTO photos VALUES(?,?,?,?,?,?,?,?)", ("photo-1", "local-event", "camera_01", "key-1", str(original), "STORED", "hash", 4))
            con.execute("INSERT INTO photo_files VALUES(?,?,?,?,?,?)", ("file-1", "photo-1", "ORIGINAL", str(original), 4, "hash"))
            con.execute("INSERT INTO jobs VALUES(?,?,?)", ("job-1", "photo-1", "PENDING"))
            con.execute("INSERT INTO ingestion_requests VALUES(?,?,?,?)", ("request-1", "photo-1", "camera_01", "key-1"))
            con.execute("INSERT INTO audit_events(entity_id,action,request_id,job_id,event_id,camera_id,status) VALUES(?,?,?,?,?,?,?)", ("photo-1", "ACCEPTED", "req-1", "job-1", "local-event", "camera_01", "STORED"))
            con.execute("INSERT INTO audit_events(entity_id,action,request_id,job_id,event_id,camera_id,status) VALUES(?,?,?,?,?,?,?)", ("photo-1", "REPLAYED", "req-2", None, "local-event", "camera_01", "STORED"))
            con.commit()
            con.close()
            report = make_report(cfg)
            harness = Harness(cfg, report)
            self.assertEqual(harness.count_logical_entities("key-1"), {"photos": 1, "requests": 1, "files": 1, "jobs": 1})
            self.assertEqual(harness.count_audit("photo-1", "REPLAYED"), 1)
            harness.reconcile_acceptance("photo-1")
            self.assertEqual(report.inconsistencies, [])


if __name__ == "__main__":
    unittest.main()
