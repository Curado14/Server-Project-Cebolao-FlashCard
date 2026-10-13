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
    unexpected_metric_labels,
    validate_prometheus_contract,
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

            fixture_secret = next(iter(report.secret_terms - {"secret-token", "Bearer secret-token"}))
            log.write_text(f'{{"event":"photo.accepted","payload_prefix":"{fixture_secret}"}}\n', encoding="utf-8")
            with self.assertRaises(ProductFailure):
                harness.read_logs()

    def test_metrics_respect_loopback_and_entity_label_policy(self):
        text = "\n".join(
            [
                "# HELP photoedge_photos_accepted_total Photos accepted.",
                "# TYPE photoedge_photos_accepted_total counter",
                "photoedge_photos_accepted_total 1",
                "# HELP photoedge_photos_rejected_total Photos rejected.",
                "# TYPE photoedge_photos_rejected_total counter",
                "photoedge_photos_rejected_total 2",
                "# HELP photoedge_http_request_duration_ms HTTP duration.",
                "# TYPE photoedge_http_request_duration_ms histogram",
                'photoedge_http_request_duration_ms_bucket{le="5"} 1',
                'photoedge_http_request_duration_ms_bucket{le="+Inf"} 1',
                "photoedge_http_request_duration_ms_sum 3",
                "photoedge_http_request_duration_ms_count 1",
                "# HELP photoedge_photo_persist_duration_ms Persist duration.",
                "# TYPE photoedge_photo_persist_duration_ms histogram",
                'photoedge_photo_persist_duration_ms_bucket{le="5"} 1',
                'photoedge_photo_persist_duration_ms_bucket{le="+Inf"} 1',
                "photoedge_photo_persist_duration_ms_sum 3",
                "photoedge_photo_persist_duration_ms_count 1",
                "# HELP photoedge_queue_depth Queue depth.",
                "# TYPE photoedge_queue_depth gauge",
                "photoedge_queue_depth 0",
                "# HELP photoedge_jobs_active Jobs active.",
                "# TYPE photoedge_jobs_active gauge",
                "photoedge_jobs_active 0",
                "# HELP photoedge_jobs_completed_total Jobs completed.",
                "# TYPE photoedge_jobs_completed_total counter",
                "photoedge_jobs_completed_total 0",
                "# HELP photoedge_jobs_failed_total Jobs failed.",
                "# TYPE photoedge_jobs_failed_total counter",
                "photoedge_jobs_failed_total 0",
                "# HELP photoedge_jobs_quarantined_total Jobs quarantined.",
                "# TYPE photoedge_jobs_quarantined_total counter",
                "photoedge_jobs_quarantined_total 0",
                "# HELP photoedge_storage_free_bytes Storage free.",
                "# TYPE photoedge_storage_free_bytes gauge",
                "photoedge_storage_free_bytes 1024",
                "# HELP photoedge_sqlite_errors_total SQLite errors.",
                "# TYPE photoedge_sqlite_errors_total counter",
                "photoedge_sqlite_errors_total 0",
                "# HELP photoedge_logging_errors_total Logging errors.",
                "# TYPE photoedge_logging_errors_total counter",
                "photoedge_logging_errors_total 0",
                "# HELP photoedge_accepted_total Alias.",
                "# TYPE photoedge_accepted_total counter",
                "photoedge_accepted_total 1",
                "# HELP photoedge_rejected_total Alias.",
                "# TYPE photoedge_rejected_total counter",
                "photoedge_rejected_total 2",
            ]
        )
        parsed = parse_prometheus(text)
        self.assertIn("photoedge_photos_accepted_total", parsed)
        metadata = {
            line.split()[2]: line.split()[3]
            for line in text.splitlines()
            if line.startswith("# TYPE ")
        }
        self.assertEqual(validate_prometheus_contract(parsed, metadata, text), [])
        self.assertEqual(forbidden_metric_labels('photoedge_jobs_active{photo_id="photo-1"} 1'), {"photo_id"})
        self.assertEqual(unexpected_metric_labels('photoedge_jobs_active{status="ready"} 1'), {"status"})
        broken_alias = text.replace("photoedge_accepted_total 1", "photoedge_accepted_total 99")
        self.assertTrue(any("alias" in item for item in validate_prometheus_contract(parse_prometheus(broken_alias), metadata, broken_alias)))

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

            class OrphaningHarness(Harness):
                def upload(self, photo, key, filename, token=""):
                    original = self.cfg.storage_root / "originals" / f"{filename}.orphan"
                    original.parent.mkdir(parents=True, exist_ok=True)
                    original.write_bytes(b"orphan-original")
                    status = 401 if token in {None, "wrong-token"} else 400
                    if filename in {"not-jpeg.jpg", "corrupt-jpeg.jpg"}:
                        status = 422
                    return HTTPResult("POST", "/api/v1/photos", status, 1.0, {"code": "rejected"}, {}, False)

            report = make_report(cfg)
            OrphaningHarness(cfg, report).invalid_inputs()
            self.assertTrue(any("created original files under root" in item for item in report.inconsistencies))
            self.assertTrue(any("increased orphan originals" in item for item in report.inconsistencies))

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
            accepted_ids, replay_ids = harness.validate_replay_audit("photo-1", "req-1", "req-2", 0)
            self.assertEqual(accepted_ids, {"req-1"})
            self.assertEqual(replay_ids, {"req-2"})
            harness.reconcile_acceptance("photo-1")
            self.assertEqual(report.inconsistencies, [])

            con = sqlite3.connect(cfg.db_path)
            con.execute("DELETE FROM audit_events WHERE action='REPLAYED'")
            con.execute("INSERT INTO audit_events(entity_id,action,request_id,job_id,event_id,camera_id,status) VALUES(?,?,?,?,?,?,?)", ("photo-1", "REPLAYED", "req-1", None, "local-event", "camera_01", "STORED"))
            con.commit()
            con.close()
            failing_report = make_report(cfg)
            Harness(cfg, failing_report).validate_replay_audit("photo-1", "req-1", "req-2", 0)
            self.assertTrue(any("not uniquely correlated" in item for item in failing_report.inconsistencies))
            self.assertTrue(any("not distinct" in item for item in failing_report.inconsistencies))


if __name__ == "__main__":
    unittest.main()
