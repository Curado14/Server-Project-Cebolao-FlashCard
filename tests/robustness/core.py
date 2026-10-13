from __future__ import annotations

import argparse
import base64
import concurrent.futures
import dataclasses
import hashlib
import json
import os
import platform
import re
import socket
import sqlite3
import statistics
import sys
import time
import traceback
import urllib.error
import urllib.parse
import urllib.request
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable


CHANGE_ID = "robustez-operacional-servidor-photo-edge"
DEFAULT_VERIFY = ("sqlite", "filesystem", "audit", "logs", "metrics")
METRIC_TYPES = {
    "photoedge_photos_accepted_total": "counter",
    "photoedge_photos_rejected_total": "counter",
    "photoedge_http_request_duration_ms": "histogram",
    "photoedge_photo_persist_duration_ms": "histogram",
    "photoedge_queue_depth": "gauge",
    "photoedge_jobs_active": "gauge",
    "photoedge_jobs_completed_total": "counter",
    "photoedge_jobs_failed_total": "counter",
    "photoedge_jobs_quarantined_total": "counter",
    "photoedge_storage_free_bytes": "gauge",
    "photoedge_sqlite_errors_total": "counter",
    "photoedge_logging_errors_total": "counter",
    "photoedge_accepted_total": "counter",
    "photoedge_rejected_total": "counter",
}
METRIC_ALIASES = {
    "photoedge_accepted_total": "photoedge_photos_accepted_total",
    "photoedge_rejected_total": "photoedge_photos_rejected_total",
}
ENTITY_LABEL_FORBIDDEN = {
    "photo_id",
    "job_id",
    "request_id",
    "idempotency_key",
    "path",
    "filename",
    "token",
    "remote_addr",
    "user_agent",
}
METRIC_ALLOWED_LABELS = {"le"}
PROMETHEUS_SAMPLE_RE = re.compile(
    r"^[a-zA-Z_:][a-zA-Z0-9_:]*(?:\{[^{}\r\n]*\})?\s+[-+]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][-+]?\d+)?$|"
    r"^[a-zA-Z_:][a-zA-Z0-9_:]*(?:\{[^{}\r\n]*\})?\s+(?:NaN|\+Inf|-Inf)$"
)

# 1x1 JPEG, deterministic and intentionally tiny.
VALID_JPEG = base64.b64decode(
    "/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAP//////////////////////////////////////////////////////////////////////////////////////"
    "////////////////////////////////////////////2wBDAf//////////////////////////////////////////////////////////////////////////////////////"
    "////////////////////////////////////////////wAARCAABAAEDASIAAhEBAxEB/8QAFQABAQAAAAAAAAAAAAAAAAAAAAX/xAAUEAEAAAAAAAAAAAAAAAAAAAAA/"
    "9oADAMBAAIQAxAAAAH/xAAUEAEAAAAAAAAAAAAAAAAAAAAA/9oACAEBAAEFAqf/xAAUEQEAAAAAAAAAAAAAAAAAAAAA/9oACAEDAQE/ASP/xAAUEQEAAAAAAAAAAAAAAAAAAAAA/"
    "9oACAECAQE/ASP/xAAUEAEAAAAAAAAAAAAAAAAAAAAA/9oACAEBAAY/Al//xAAUEAEAAAAAAAAAAAAAAAAAAAAA/9oACAEBAAE/IV//2gAMAwEAAgADAAAAEP/E"
    "ABQRAQAAAAAAAAAAAAAAAAAAABD/2gAIAQMBAT8QH//EABQRAQAAAAAAAAAAAAAAAAAAABD/2gAIAQIBAT8QH//EABQQAQAAAAAAAAAAAAAAAAAAABD/2gAIAQEA"
    "AT8QH//Z"
)
CORRUPT_JPEG = b"\xff\xd8\xff\xe0corrupt-photoedge-fixture"
ALT_JPEG = VALID_JPEG + b"\nalt"


class HarnessError(Exception):
    exit_code = 1


class PreconditionError(HarnessError):
    exit_code = 2


class ProductFailure(HarnessError):
    exit_code = 3


@dataclasses.dataclass
class Config:
    base_url: str
    token: str
    event_id: str
    device_id: str
    storage_root: Path
    db_path: Path
    log_path: Path | None
    http_timeout_seconds: float = 30.0
    processing_timeout_seconds: float = 30.0
    poll_interval_ms: int = 250
    default_photos: int = 10
    default_concurrency: int = 2
    reports_dir: Path = Path("tests/robustness/reports")
    config_path: Path | None = None

    @classmethod
    def load(cls, path: str | Path | None) -> "Config":
        values = dict(os.environ)
        config_path = Path(path) if path else None
        if config_path:
            if not config_path.exists():
                raise PreconditionError(f"config file not found: {config_path}")
            values.update(read_env_file(config_path))
        root = get_any(values, "PHOTOEDGE_ROBUST_STORAGE_ROOT", "PHOTO_EDGE_STORAGE_ROOT", "PHOTOEDGE_ROOT")
        db_path = get_any(values, "PHOTOEDGE_ROBUST_DB_PATH", "PHOTO_EDGE_DB_PATH")
        base_url = get_any(values, "PHOTOEDGE_ROBUST_BASE_URL", "PHOTO_EDGE_BASE_URL")
        token = get_any(values, "PHOTOEDGE_ROBUST_API_TOKEN", "PHOTO_EDGE_API_TOKEN", "PHOTOEDGE_TOKEN")
        event_id = get_any(values, "PHOTOEDGE_ROBUST_EVENT_ID", "PHOTO_EDGE_EVENT_ID", "PHOTOEDGE_EVENT_ID")
        device_id = get_any(values, "PHOTOEDGE_ROBUST_DEVICE_ID", "PHOTO_EDGE_CAMERA_ID", "PHOTOEDGE_DEVICE_ID")
        missing = [
            name
            for name, value in {
                "PHOTOEDGE_ROBUST_BASE_URL": base_url,
                "PHOTOEDGE_ROBUST_API_TOKEN": token,
                "PHOTOEDGE_ROBUST_EVENT_ID": event_id,
                "PHOTOEDGE_ROBUST_DEVICE_ID": device_id,
                "PHOTOEDGE_ROBUST_STORAGE_ROOT": root,
                "PHOTOEDGE_ROBUST_DB_PATH": db_path,
            }.items()
            if not value
        ]
        if missing:
            raise PreconditionError("missing harness configuration: " + ", ".join(missing))
        log_path = get_any(values, "PHOTOEDGE_ROBUST_LOG_PATH", "PHOTO_EDGE_LOG_PATH")
        cfg = cls(
            base_url=base_url.rstrip("/"),
            token=token,
            event_id=event_id,
            device_id=device_id,
            storage_root=Path(root),
            db_path=Path(db_path),
            log_path=Path(log_path) if log_path else None,
            http_timeout_seconds=float(get_any(values, "PHOTOEDGE_ROBUST_HTTP_TIMEOUT_SECONDS", "PHOTO_EDGE_TIMEOUT_SECONDS") or 30),
            processing_timeout_seconds=float(get_any(values, "PHOTOEDGE_ROBUST_PROCESSING_TIMEOUT_SECONDS") or 30),
            poll_interval_ms=int(get_any(values, "PHOTOEDGE_ROBUST_POLL_INTERVAL_MS", "PHOTO_EDGE_POLL_INTERVAL_MS") or 250),
            default_photos=int(get_any(values, "PHOTOEDGE_ROBUST_DEFAULT_PHOTOS") or 10),
            default_concurrency=int(get_any(values, "PHOTOEDGE_ROBUST_DEFAULT_CONCURRENCY") or 2),
            reports_dir=Path(get_any(values, "PHOTOEDGE_ROBUST_REPORTS_DIR") or "tests/robustness/reports"),
            config_path=config_path,
        )
        cfg.validate_targets()
        return cfg

    def validate_targets(self) -> None:
        parsed = urllib.parse.urlparse(self.base_url)
        if parsed.scheme not in {"http", "https"} or not parsed.hostname:
            raise PreconditionError("base_url must be an absolute HTTP URL")
        root = self.storage_root.resolve()
        repo = repo_root()
        unsafe_data = (repo / "data").resolve()
        if root == unsafe_data or is_relative_to(root, unsafe_data):
            raise PreconditionError("refusing repository data/ as harness storage root")
        db = self.db_path.resolve()
        if db == unsafe_data or is_relative_to(db, unsafe_data):
            raise PreconditionError("refusing repository data/ as harness database path")
        if not is_relative_to(db, root):
            raise PreconditionError("database path must be under the configured storage root")

    def sanitized(self) -> dict[str, Any]:
        return {
            "base_url": self.base_url,
            "event_id": self.event_id,
            "device_id": self.device_id,
            "storage_root": str(self.storage_root),
            "db_path": str(self.db_path),
            "log_path": str(self.log_path) if self.log_path else None,
            "http_timeout_seconds": self.http_timeout_seconds,
            "processing_timeout_seconds": self.processing_timeout_seconds,
            "poll_interval_ms": self.poll_interval_ms,
            "default_photos": self.default_photos,
            "default_concurrency": self.default_concurrency,
        }


@dataclasses.dataclass
class HTTPResult:
    method: str
    path: str
    status: int
    elapsed_ms: float
    body: dict[str, Any] | str | None
    headers: dict[str, str]
    ok: bool


class Report:
    def __init__(self, scenario: str, cfg: Config, args: argparse.Namespace, verify: set[str]) -> None:
        self.scenario = scenario
        self.cfg = cfg
        self.args = args
        self.verify = verify
        self.started = now()
        self.finished: str | None = None
        self.result = "running"
        self.exit_code = 1
        self.http_counts: dict[str, int] = {}
        self.http_latencies_ms: list[float] = []
        self.accepted: list[dict[str, Any]] = []
        self.expected_rejections: list[dict[str, Any]] = []
        self.unexpected_errors: list[dict[str, Any]] = []
        self.inconsistencies: list[str] = []
        self.limitations: list[str] = []
        self.metrics: dict[str, Any] = {}
        self.operational: dict[str, Any] = {}
        self.coverage: set[str] = set()
        self.secret_terms: set[str] = {cfg.token, "Bearer " + cfg.token}
        for fixture in (VALID_JPEG, ALT_JPEG, CORRUPT_JPEG, b"not a jpeg"):
            self.secret_terms.update(fixture_sentinels(fixture))

    def record_http(self, result: HTTPResult) -> None:
        self.http_counts[str(result.status)] = self.http_counts.get(str(result.status), 0) + 1
        self.http_latencies_ms.append(result.elapsed_ms)

    def add_key_secret(self, key: str) -> None:
        self.secret_terms.add(key)

    def fail(self, message: str) -> None:
        self.inconsistencies.append(message)

    def finish(self, result: str, exit_code: int) -> None:
        self.finished = now()
        self.result = result
        self.exit_code = exit_code

    def as_dict(self) -> dict[str, Any]:
        duration = None
        if self.finished:
            duration = iso_duration_seconds(self.started, self.finished)
        data = {
            "change_id": CHANGE_ID,
            "scenario": self.scenario,
            "started_at": self.started,
            "finished_at": self.finished,
            "duration_seconds": duration,
            "result": self.result,
            "exit_code": self.exit_code,
            "command": " ".join(sys.argv),
            "environment": {"python": sys.version.split()[0], "platform": platform.platform()},
            "config": self.cfg.sanitized(),
            "parameters": {
                "photos": getattr(self.args, "photos", None),
                "concurrency": getattr(self.args, "concurrency", None),
                "verify": sorted(self.verify),
            },
            "http_counts": self.http_counts,
            "http_latency_ms": latency_summary(self.http_latencies_ms),
            "accepted": self.accepted,
            "expected_rejections": self.expected_rejections,
            "unexpected_errors": self.unexpected_errors,
            "inconsistencies": self.inconsistencies,
            "limitations": self.limitations,
            "metrics": self.metrics,
            "operational": self.operational,
            "coverage": sorted(self.coverage),
        }
        return redact(data, self.secret_terms)

    def write(self) -> tuple[Path, Path]:
        self.cfg.reports_dir.mkdir(parents=True, exist_ok=True)
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        stem = f"{CHANGE_ID}-{self.scenario}-{stamp}"
        json_path = self.cfg.reports_dir / f"{stem}.json"
        md_path = self.cfg.reports_dir / f"{stem}.md"
        data = self.as_dict()
        text = json.dumps(data, indent=2, sort_keys=True)
        assert_no_secret_text(text, self.secret_terms)
        json_path.write_text(text + "\n", encoding="utf-8")
        md = self.to_markdown(data)
        assert_no_secret_text(md, self.secret_terms)
        md_path.write_text(md, encoding="utf-8")
        return json_path, md_path

    def to_markdown(self, data: dict[str, Any]) -> str:
        lines = [
            f"# Robustness report: {data['scenario']}",
            "",
            f"- change_id: `{CHANGE_ID}`",
            f"- result: `{data['result']}`",
            f"- duration_seconds: `{data['duration_seconds']}`",
            f"- coverage: `{', '.join(data['coverage'])}`",
            f"- http_counts: `{json.dumps(data['http_counts'], sort_keys=True)}`",
            f"- inconsistencies: `{len(data['inconsistencies'])}`",
            f"- limitations: `{len(data['limitations'])}`",
            "",
            "## Inconsistencies",
        ]
        lines.extend(f"- {item}" for item in data["inconsistencies"] or ["none"])
        lines.append("")
        lines.append("## Limitations")
        lines.extend(f"- {item}" for item in data["limitations"] or ["none"])
        lines.append("")
        lines.append("## Sanitized config")
        lines.append("```json")
        lines.append(json.dumps(data["config"], indent=2, sort_keys=True))
        lines.append("```")
        return "\n".join(lines) + "\n"


class Harness:
    def __init__(self, cfg: Config, report: Report) -> None:
        self.cfg = cfg
        self.report = report

    def smoke(self) -> None:
        self.report.coverage.update({"REQ-001", "REQ-005", "REQ-006", "PO-001", "PO-005", "EVD-001", "EVD-005"})
        for path in ("/health/live", "/health/ready"):
            result = self.request("GET", path)
            if result.status != 200:
                self.report.fail(f"{path} returned {result.status}")
        self.open_readonly_db().close()
        if "metrics" in self.report.verify:
            self.verify_metrics()

    def single_upload(self, photo_bytes: bytes = VALID_JPEG, suffix: str | None = None) -> dict[str, Any]:
        self.report.coverage.update({"REQ-002", "REQ-003", "REQ-004", "PO-002", "PO-003", "PO-004", "EVD-002", "EVD-003", "EVD-004"})
        key = logical_key("single", suffix)
        result = self.upload(photo_bytes, key, filename="sample_001.jpg")
        accepted = self.accept_202(result)
        self.reconcile_acceptance(accepted["photo_id"])
        self.verify_logs_for_acceptance(accepted["photo_id"], result.headers.get("x-request-id"))
        self.report.accepted.append({"photo_id": accepted["photo_id"], "status": accepted.get("status"), "status_url": accepted.get("status_url")})
        return accepted

    def sequential_upload(self, count: int) -> None:
        self.report.coverage.update({"REQ-007", "PO-006", "EVD-006"})
        for i in range(count):
            self.single_upload(photo_bytes=VALID_JPEG + f"\nseq-{i}".encode("ascii"), suffix=f"seq-{i}")

    def concurrent_upload(self, count: int, concurrency: int) -> None:
        self.report.coverage.update({"REQ-007", "PO-007", "EVD-007"})
        if concurrency < 1:
            raise PreconditionError("concurrency must be positive")
        items = [(i, VALID_JPEG + f"\nconcurrent-{i}".encode("ascii"), logical_key("concurrent", str(i))) for i in range(count)]
        with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
            futures = [pool.submit(self.upload, data, key, f"sample_{i:03d}.jpg") for i, data, key in items]
            for future in concurrent.futures.as_completed(futures):
                result = future.result()
                accepted = self.accept_202(result)
                self.reconcile_acceptance(accepted["photo_id"])
                self.report.accepted.append({"photo_id": accepted["photo_id"], "status": accepted.get("status")})

    def invalid_inputs(self) -> None:
        self.report.coverage.update({"REQ-008", "PO-008", "EVD-008"})
        cases = [
            ("missing-auth", None, logical_key("invalid", "missing-auth"), {"expect": {401}, "photo": VALID_JPEG}),
            ("bad-auth", "wrong-token", logical_key("invalid", "bad-auth"), {"expect": {401}, "photo": VALID_JPEG}),
            ("missing-photo", self.cfg.token, logical_key("invalid", "missing-photo"), {"expect": {400}, "photo": None}),
            ("not-jpeg", self.cfg.token, logical_key("invalid", "not-jpeg"), {"expect": {422, 415}, "photo": b"not a jpeg"}),
            ("corrupt-jpeg", self.cfg.token, logical_key("invalid", "corrupt-jpeg"), {"expect": {422}, "photo": CORRUPT_JPEG}),
        ]
        for name, token, key, spec in cases:
            before = self.count_logical_entities(key)
            originals_before = self.snapshot_original_files()
            orphan_count_before = self.count_orphan_originals()
            result = self.upload(spec["photo"], key, filename=f"{name}.jpg", token=token)
            if result.status not in spec["expect"]:
                self.report.fail(f"{name} expected {sorted(spec['expect'])}, got {result.status}")
                self.report.unexpected_errors.append({"case": name, "status": result.status, "body": result.body})
            else:
                self.report.expected_rejections.append({"case": name, "status": result.status})
            after = self.count_logical_entities(key)
            if after != before:
                self.report.fail(f"{name} created durable entities: before={before}, after={after}")
            originals_after = self.snapshot_original_files()
            new_originals = sorted(originals_after - originals_before)
            if new_originals:
                self.report.fail(f"{name} created original files under root: {new_originals}")
            orphan_count_after = self.count_orphan_originals()
            if orphan_count_after > orphan_count_before:
                self.report.fail(f"{name} increased orphan originals: before={orphan_count_before}, after={orphan_count_after}")
            assert_problem_sanitized(result.body, self.report.secret_terms, name, self.report)

    def idempotency(self) -> None:
        self.report.coverage.update({"REQ-009", "PO-009", "EVD-009"})
        key = logical_key("idempotency", "replay")
        first_result = self.upload(VALID_JPEG, key, "sample_a.jpg")
        first_request_id = first_result.headers.get("x-request-id")
        first = self.accept_202(first_result)
        before = self.count_logical_entities(key)
        replay_audit_before = self.count_audit(first["photo_id"], "REPLAYED")
        second_result = self.upload(VALID_JPEG, key, "sample_a.jpg")
        second_request_id = second_result.headers.get("x-request-id")
        second = self.accept_202(second_result)
        after_replay = self.count_logical_entities(key)
        if first["photo_id"] != second["photo_id"]:
            self.report.fail("idempotency replay returned a different photo_id")
        if before != after_replay:
            self.report.fail(f"idempotency replay changed logical cardinality: {before} -> {after_replay}")
        accepted_request_ids, replay_request_ids = self.validate_replay_audit(
            first["photo_id"],
            first_request_id,
            second_request_id,
            replay_audit_before,
        )
        conflict = self.upload(ALT_JPEG, key, "sample_b.jpg")
        if conflict.status != 409:
            self.report.fail(f"idempotency conflict expected 409, got {conflict.status}")
        after_conflict = self.count_logical_entities(key)
        if after_conflict != after_replay:
            self.report.fail(f"idempotency conflict changed logical cardinality: {after_replay} -> {after_conflict}")
        self.reconcile_acceptance(first["photo_id"])
        self.report.accepted.append({
            "photo_id": first["photo_id"],
            "status": first.get("status"),
            "replayed": True,
            "accepted_request_ids": sorted(accepted_request_ids),
            "replayed_request_ids": sorted(replay_request_ids),
        })
        self.report.expected_rejections.append({"case": "idempotency-conflict", "status": conflict.status})

    def validate_replay_audit(
        self,
        photo_id: str,
        first_request_id: str | None,
        second_request_id: str | None,
        replay_count_before: int,
    ) -> tuple[set[str], set[str]]:
        replay_audit = self.audit_rows(photo_id, "REPLAYED")
        accepted_audit = self.audit_rows(photo_id, "ACCEPTED")
        accepted_request_ids = {row.get("request_id") for row in accepted_audit if row.get("request_id")}
        replay_request_ids = {row.get("request_id") for row in replay_audit if row.get("request_id")}
        if len(replay_audit) <= replay_count_before:
            self.report.fail("idempotency replay did not append a REPLAYED audit row")
        if len(replay_audit) != len(replay_request_ids):
            self.report.fail("idempotency REPLAYED audit row without request_id")
        if not replay_request_ids:
            self.report.fail("idempotency replay did not create REPLAYED audit row with request_id")
        if not second_request_id:
            self.report.fail("idempotency replay response did not expose X-Request-Id")
        else:
            matching_replay_rows = [row for row in replay_audit if row.get("request_id") == second_request_id]
            if len(matching_replay_rows) != 1:
                self.report.fail("idempotency REPLAYED audit row is not uniquely correlated to the replay X-Request-Id")
        if first_request_id and second_request_id and first_request_id == second_request_id:
            self.report.fail("idempotency first and replay attempts used the same request_id")
        if accepted_request_ids & replay_request_ids:
            self.report.fail("idempotency REPLAYED audit request_id was not distinct from ACCEPTED request_id")
        return accepted_request_ids, replay_request_ids

    def request(self, method: str, path: str, body: bytes | None = None, headers: dict[str, str] | None = None) -> HTTPResult:
        url = self.cfg.base_url + path
        started = time.perf_counter()
        req = urllib.request.Request(url, data=body, method=method, headers=headers or {})
        try:
            with urllib.request.urlopen(req, timeout=self.cfg.http_timeout_seconds) as response:
                raw = response.read()
                status = response.status
                response_headers = {k.lower(): v for k, v in response.headers.items()}
        except urllib.error.HTTPError as exc:
            raw = exc.read()
            status = exc.code
            response_headers = {k.lower(): v for k, v in exc.headers.items()}
        except OSError as exc:
            raise PreconditionError(f"HTTP request failed; is the local service running? {exc}") from exc
        elapsed = (time.perf_counter() - started) * 1000
        body_obj: dict[str, Any] | str | None
        if raw:
            try:
                body_obj = json.loads(raw.decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError):
                body_obj = raw.decode("utf-8", errors="replace")
        else:
            body_obj = None
        result = HTTPResult(method, path, status, elapsed, body_obj, response_headers, 200 <= status < 300)
        self.report.record_http(result)
        return result

    def upload(self, photo: bytes | None, key: str, filename: str, token: str | None = "") -> HTTPResult:
        self.report.add_key_secret(key)
        if photo is not None:
            self.report.secret_terms.update(fixture_sentinels(photo))
        boundary = "photoedge-" + uuid.uuid4().hex
        fields: list[tuple[str, str, bytes | None, str | None]] = [("event_id", "", self.cfg.event_id.encode("utf-8"), None)]
        if photo is not None:
            fields.append(("photo", filename, photo, "image/jpeg"))
        body = multipart_body(boundary, fields)
        headers = {
            "Content-Type": f"multipart/form-data; boundary={boundary}",
            "Idempotency-Key": key,
        }
        if token == "":
            token = self.cfg.token
        if token is not None:
            headers["Authorization"] = "Bearer " + token
        return self.request("POST", "/api/v1/photos", body, headers)

    def accept_202(self, result: HTTPResult) -> dict[str, Any]:
        if result.status != 202:
            raise ProductFailure(f"expected 202 accepted upload, got {result.status}")
        if not isinstance(result.body, dict):
            raise ProductFailure("202 response body is not JSON object")
        expected = {"photo_id", "status", "received_at", "status_url"}
        missing = expected - set(result.body)
        if missing:
            raise ProductFailure("202 response missing fields: " + ", ".join(sorted(missing)))
        if "job_id" in result.body:
            raise ProductFailure("202 response unexpectedly contains job_id")
        return result.body

    def reconcile_acceptance(self, photo_id: str) -> None:
        if not {"sqlite", "filesystem", "audit"} & self.report.verify:
            return
        con = self.open_readonly_db()
        try:
            photo = query_one(con, "SELECT id,event_id,device_id,original_path,status,sha256,size_bytes FROM photos WHERE id=?", (photo_id,))
            if not photo:
                self.report.fail(f"{photo_id} missing photos row")
                return
            file_row = query_one(con, "SELECT path,kind,sha256,size_bytes FROM photo_files WHERE photo_id=? AND kind='ORIGINAL'", (photo_id,))
            if not file_row:
                self.report.fail(f"{photo_id} missing ORIGINAL photo_files row")
            job = query_one(con, "SELECT id,status FROM jobs WHERE photo_id=?", (photo_id,))
            if not job:
                self.report.fail(f"{photo_id} missing job row")
            accepted = query_one(con, "SELECT id,request_id,job_id,event_id,camera_id,status FROM audit_events WHERE entity_id=? AND action='ACCEPTED'", (photo_id,))
            if not accepted:
                self.report.fail(f"{photo_id} missing ACCEPTED audit row")
            if file_row and "filesystem" in self.report.verify:
                self.verify_original_path(file_row["path"])
        finally:
            con.close()

    def verify_original_path(self, stored_path: str) -> None:
        root = self.cfg.storage_root.resolve()
        path = Path(stored_path)
        if not path.is_absolute():
            path = (Path.cwd() / path).resolve()
        else:
            path = path.resolve()
        if not is_relative_to(path, root):
            self.report.fail(f"original path escapes configured root: {path}")
            return
        if not path.exists():
            self.report.fail(f"original file is missing: {path}")

    def verify_logs_for_acceptance(self, photo_id: str, request_id: str | None) -> None:
        if "logs" not in self.report.verify:
            return
        records = self.read_logs()
        if records is None:
            return
        if not any(item.get("photo_id") == photo_id or (request_id and item.get("request_id") == request_id) for item in records):
            self.report.fail(f"logs capture lacks correlation for {photo_id}")

    def read_logs(self) -> list[dict[str, Any]] | None:
        if not self.cfg.log_path:
            self.report.limitations.append("logs=not-run: PHOTOEDGE_ROBUST_LOG_PATH was not configured")
            return None
        if not self.cfg.log_path.exists():
            self.report.limitations.append(f"logs=not-run: capture path does not exist: {self.cfg.log_path}")
            return None
        raw = self.cfg.log_path.read_text(encoding="utf-8", errors="replace")
        assert_no_secret_text(raw, self.report.secret_terms)
        records: list[dict[str, Any]] = []
        for lineno, line in enumerate(raw.splitlines(), start=1):
            if not line.strip():
                continue
            try:
                item = json.loads(line)
            except json.JSONDecodeError:
                self.report.fail(f"log line {lineno} is not valid JSON")
                continue
            if isinstance(item, dict):
                records.append(item)
        return records

    def verify_metrics(self) -> None:
        parsed = urllib.parse.urlparse(self.cfg.base_url)
        host = parsed.hostname or ""
        if not is_loopback_host(host):
            self.report.limitations.append("metrics=not-run: base_url host is not loopback")
            return
        result = self.request("GET", "/metrics")
        if result.status != 200:
            self.report.fail(f"/metrics returned {result.status}")
            return
        content_type = result.headers.get("content-type", "")
        if "text/plain" not in content_type or "version=0.0.4" not in content_type:
            self.report.fail(f"/metrics Content-Type is not Prometheus 0.0.4: {content_type}")
        if not isinstance(result.body, str):
            self.report.fail("/metrics response was parsed as JSON, expected text")
            return
        parsed_metrics = parse_prometheus(result.body)
        metadata = parse_prometheus_metadata(result.body)
        metric_errors = validate_prometheus_contract(parsed_metrics, metadata, result.body)
        for error in metric_errors:
            self.report.fail(error)
        forbidden = forbidden_metric_labels(result.body)
        if forbidden:
            self.report.fail("metrics contain forbidden entity labels: " + ", ".join(sorted(forbidden)))
        unexpected = unexpected_metric_labels(result.body)
        if unexpected:
            self.report.fail("metrics contain unexpected labels: " + ", ".join(sorted(unexpected)))
        self.report.metrics = {
            "series": sorted(parsed_metrics),
            "types": metadata,
            "aliases": {alias: parsed_metrics.get(alias, []) for alias in sorted(METRIC_ALIASES)},
            "forbidden_entity_labels": sorted(forbidden),
            "unexpected_labels": sorted(unexpected),
        }

    def open_readonly_db(self) -> sqlite3.Connection:
        if not self.cfg.db_path.exists():
            raise PreconditionError(f"database not found: {self.cfg.db_path}")
        uri = self.cfg.db_path.resolve().as_posix()
        con = sqlite3.connect(f"file:{uri}?mode=ro", uri=True)
        con.row_factory = sqlite3.Row
        con.execute("PRAGMA query_only=ON")
        con.execute("SELECT count(*) FROM sqlite_master").fetchone()
        return con

    def count_logical_entities(self, key: str) -> dict[str, int]:
        if not self.cfg.db_path.exists():
            return {"photos": 0, "requests": 0, "files": 0, "jobs": 0}
        con = self.open_readonly_db()
        try:
            photos = scalar(con, "SELECT count(*) FROM photos WHERE device_id=? AND idempotency_key=?", (self.cfg.device_id, key))
            requests = scalar(con, "SELECT count(*) FROM ingestion_requests WHERE device_id=? AND idempotency_key=?", (self.cfg.device_id, key))
            files = scalar(con, "SELECT count(*) FROM photo_files WHERE photo_id IN (SELECT id FROM photos WHERE device_id=? AND idempotency_key=?)", (self.cfg.device_id, key))
            jobs = scalar(con, "SELECT count(*) FROM jobs WHERE photo_id IN (SELECT id FROM photos WHERE device_id=? AND idempotency_key=?)", (self.cfg.device_id, key))
            return {"photos": photos, "requests": requests, "files": files, "jobs": jobs}
        finally:
            con.close()

    def count_audit(self, photo_id: str, action: str) -> int:
        return len(self.audit_rows(photo_id, action))

    def audit_rows(self, photo_id: str, action: str) -> list[dict[str, Any]]:
        con = self.open_readonly_db()
        try:
            rows = con.execute(
                "SELECT id,request_id,job_id,event_id,camera_id,status FROM audit_events WHERE entity_id=? AND action=? ORDER BY id",
                (photo_id, action),
            ).fetchall()
            return [dict(row) for row in rows]
        finally:
            con.close()

    def snapshot_original_files(self) -> set[str]:
        root = (self.cfg.storage_root / "originals").resolve()
        if not root.exists():
            return set()
        return {str(path.resolve().relative_to(root)) for path in root.rglob("*") if path.is_file()}

    def count_orphan_originals(self) -> int:
        root = (self.cfg.storage_root / "originals").resolve()
        if not root.exists() or not self.cfg.db_path.exists():
            return 0
        con = self.open_readonly_db()
        try:
            count = 0
            for path in root.rglob("*"):
                if not path.is_file():
                    continue
                if scalar(con, "SELECT count(*) FROM photo_files WHERE path=?", (str(path.resolve()),)) == 0:
                    count += 1
            return count
        finally:
            con.close()


def run_scenario(args: argparse.Namespace) -> tuple[Report, int]:
    if args.scenario.startswith("operational-"):
        from .operational import run_operational_scenario

        return run_operational_scenario(args)

    cfg = Config.load(args.config)
    verify = parse_verify(args.verify)
    report = Report(args.scenario, cfg, args, verify)
    harness = Harness(cfg, report)
    try:
        if args.scenario == "smoke":
            harness.smoke()
        elif args.scenario == "single-upload":
            harness.single_upload()
        elif args.scenario == "sequential-upload":
            harness.sequential_upload(args.photos or cfg.default_photos)
        elif args.scenario == "concurrent-upload":
            harness.concurrent_upload(args.photos or cfg.default_photos, args.concurrency or cfg.default_concurrency)
        elif args.scenario == "invalid-inputs":
            harness.invalid_inputs()
        elif args.scenario == "idempotency":
            harness.idempotency()
        elif args.scenario == "all":
            harness.smoke()
            harness.single_upload()
            harness.sequential_upload(args.photos or cfg.default_photos)
            harness.concurrent_upload(args.photos or cfg.default_photos, args.concurrency or cfg.default_concurrency)
            harness.invalid_inputs()
            harness.idempotency()
            report.coverage.add("EVD-011")
        else:
            raise PreconditionError(f"unknown scenario: {args.scenario}")
        if report.inconsistencies:
            report.finish("failed", 3)
        else:
            report.finish("passed", 0)
    except HarnessError as exc:
        report.unexpected_errors.append({"type": type(exc).__name__, "message": str(exc)})
        report.finish("precondition-failed" if isinstance(exc, PreconditionError) else "failed", exc.exit_code)
    except Exception as exc:  # pragma: no cover - defensive report path.
        report.unexpected_errors.append({"type": type(exc).__name__, "message": str(exc), "traceback": traceback.format_exc(limit=5)})
        report.finish("harness-error", 4)
    if not args.no_report:
        report.write()
    return report, report.exit_code


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Photo Edge local robustness harness")
    parser.add_argument(
        "--scenario",
        required=True,
        choices=[
            "smoke",
            "single-upload",
            "sequential-upload",
            "concurrent-upload",
            "invalid-inputs",
            "idempotency",
            "all",
            "operational-smoke",
            "operational-restart-recovery",
            "operational-storage-limit",
            "operational-load",
            "operational-timeout-cleanup",
        ],
    )
    parser.add_argument("--config", default="tests/robustness/config.local.env")
    parser.add_argument("--verify", default=",".join(DEFAULT_VERIFY), help="comma-separated: sqlite,filesystem,audit,logs,metrics")
    parser.add_argument("--photos", type=int)
    parser.add_argument("--concurrency", type=int)
    parser.add_argument("--work-root")
    parser.add_argument("--reports-dir")
    parser.add_argument("--timeout-seconds", type=float, default=60.0)
    parser.add_argument("--duration-seconds", type=float)
    parser.add_argument("--min-free-bytes")
    parser.add_argument("--force-expired-lease", action="store_true")
    parser.add_argument("--request-system", choices=["disk-fill", "quota", "mount", "power-loss", "mobile", "lan", "material-load"])
    parser.add_argument("--no-report", action="store_true")
    return parser


def read_env_file(path: Path) -> dict[str, str]:
    values: dict[str, str] = {}
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        values[key.strip()] = value.strip().strip('"').strip("'")
    return values


def get_any(values: dict[str, str], *names: str) -> str:
    for name in names:
        value = values.get(name)
        if value:
            return value
    return ""


def parse_verify(value: str | None) -> set[str]:
    if not value:
        return set()
    selected = {item.strip().lower() for item in value.split(",") if item.strip()}
    allowed = set(DEFAULT_VERIFY)
    unknown = selected - allowed
    if unknown:
        raise PreconditionError("unknown verification target(s): " + ", ".join(sorted(unknown)))
    return selected


def multipart_body(boundary: str, fields: Iterable[tuple[str, str, bytes | None, str | None]]) -> bytes:
    chunks: list[bytes] = []
    for name, filename, content, content_type in fields:
        chunks.append(f"--{boundary}\r\n".encode("ascii"))
        if filename:
            chunks.append(f'Content-Disposition: form-data; name="{name}"; filename="{filename}"\r\n'.encode("utf-8"))
            chunks.append(f"Content-Type: {content_type or 'application/octet-stream'}\r\n\r\n".encode("ascii"))
        else:
            chunks.append(f'Content-Disposition: form-data; name="{name}"\r\n\r\n'.encode("ascii"))
        chunks.append(content or b"")
        chunks.append(b"\r\n")
    chunks.append(f"--{boundary}--\r\n".encode("ascii"))
    return b"".join(chunks)


def logical_key(prefix: str, suffix: str | None = None) -> str:
    tail = suffix or uuid.uuid4().hex
    return f"robust-{prefix}-{tail}-{uuid.uuid4().hex[:12]}"


def query_one(con: sqlite3.Connection, sql: str, params: tuple[Any, ...]) -> dict[str, Any] | None:
    row = con.execute(sql, params).fetchone()
    return dict(row) if row else None


def scalar(con: sqlite3.Connection, sql: str, params: tuple[Any, ...]) -> int:
    row = con.execute(sql, params).fetchone()
    return int(row[0] if row else 0)


def parse_prometheus(text: str) -> dict[str, list[str]]:
    series: dict[str, list[str]] = {}
    for line in text.splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        name = stripped.split(None, 1)[0].split("{", 1)[0]
        series.setdefault(name, []).append(stripped)
    return series


def parse_prometheus_metadata(text: str) -> dict[str, str]:
    metadata: dict[str, str] = {}
    for line in text.splitlines():
        stripped = line.strip()
        if not stripped.startswith("# TYPE "):
            continue
        parts = stripped.split()
        if len(parts) == 4:
            metadata[parts[2]] = parts[3]
    return metadata


def metric_value(samples: list[str]) -> float | None:
    for sample in samples:
        parts = sample.rsplit(None, 1)
        if len(parts) != 2:
            continue
        try:
            return float(parts[1])
        except ValueError:
            continue
    return None


def validate_prometheus_contract(series: dict[str, list[str]], metadata: dict[str, str], text: str | None = None) -> list[str]:
    errors: list[str] = []
    if text is not None:
        errors.extend(validate_prometheus_text_format(text))
    for name, expected_type in sorted(METRIC_TYPES.items()):
        if metadata.get(name) != expected_type:
            errors.append(f"metrics type mismatch for {name}: expected {expected_type}, got {metadata.get(name)}")
        if expected_type == "histogram":
            required = {f"{name}_bucket", f"{name}_sum", f"{name}_count"}
            missing_histogram = sorted(required - set(series))
            if missing_histogram:
                errors.append(f"metrics missing histogram samples for {name}: {', '.join(missing_histogram)}")
        elif name not in series:
            errors.append(f"metrics missing series: {name}")
    for alias, canonical in sorted(METRIC_ALIASES.items()):
        alias_value = metric_value(series.get(alias, []))
        canonical_value = metric_value(series.get(canonical, []))
        if alias_value is None or canonical_value is None or alias_value != canonical_value:
            errors.append(f"metrics alias {alias} does not match {canonical}")
    return errors


def validate_prometheus_text_format(text: str) -> list[str]:
    errors: list[str] = []
    for lineno, line in enumerate(text.splitlines(), start=1):
        stripped = line.strip()
        if not stripped or stripped.startswith("# HELP ") or stripped.startswith("# TYPE "):
            continue
        if stripped.startswith("#"):
            errors.append(f"metrics line {lineno} is unsupported comment format")
        elif not PROMETHEUS_SAMPLE_RE.match(stripped):
            errors.append(f"metrics line {lineno} is not valid Prometheus 0.0.4 sample")
    return errors


def forbidden_metric_labels(text: str) -> set[str]:
    found: set[str] = set()
    for line in text.splitlines():
        if "{" not in line or "}" not in line or line.strip().startswith("#"):
            continue
        labels = line.split("{", 1)[1].split("}", 1)[0]
        for pair in labels.split(","):
            name = pair.split("=", 1)[0].strip()
            if name in ENTITY_LABEL_FORBIDDEN:
                found.add(name)
    return found


def unexpected_metric_labels(text: str) -> set[str]:
    found: set[str] = set()
    for line in text.splitlines():
        if "{" not in line or "}" not in line or line.strip().startswith("#"):
            continue
        labels = line.split("{", 1)[1].split("}", 1)[0]
        for pair in labels.split(","):
            name = pair.split("=", 1)[0].strip()
            if name and name not in METRIC_ALLOWED_LABELS:
                found.add(name)
    return found


def assert_problem_sanitized(body: Any, secrets: set[str], case: str, report: Report) -> None:
    text = json.dumps(body, sort_keys=True) if not isinstance(body, str) else body
    try:
        assert_no_secret_text(text, secrets)
    except ProductFailure as exc:
        report.fail(f"{case} leaked secret material: {exc}")
    if isinstance(body, dict) and "code" not in body:
        report.fail(f"{case} problem response has no code")


def assert_no_secret_text(text: str, secrets: Iterable[str]) -> None:
    for secret in secrets:
        if secret and secret in text:
            raise ProductFailure("secret material appeared in output")


def fixture_sentinels(payload: bytes) -> set[str]:
    sentinels: set[str] = set()
    if not payload:
        return sentinels
    prefix = payload[:24]
    sentinels.add(base64.b64encode(prefix).decode("ascii"))
    sentinels.add(prefix.hex())
    ascii_prefix = prefix.decode("ascii", errors="ignore").strip()
    if len(ascii_prefix) >= 6:
        sentinels.add(ascii_prefix)
    if len(payload) <= 128:
        sentinels.add(base64.b64encode(payload).decode("ascii"))
        sentinels.add(payload.hex())
    return {item for item in sentinels if item}


def redact(value: Any, secrets: Iterable[str]) -> Any:
    if isinstance(value, dict):
        return {k: redact(v, secrets) for k, v in value.items()}
    if isinstance(value, list):
        return [redact(v, secrets) for v in value]
    if isinstance(value, str):
        redacted = value
        for secret in secrets:
            if secret:
                redacted = redacted.replace(secret, "[REDACTED]")
        return redacted
    return value


def latency_summary(values: list[float]) -> dict[str, float | None]:
    if not values:
        return {"p50": None, "p95": None, "max": None}
    ordered = sorted(values)
    p50 = statistics.median(ordered)
    p95 = ordered[max(0, min(len(ordered) - 1, int(round(len(ordered) * 0.95)) - 1))]
    return {"p50": round(p50, 3), "p95": round(p95, 3), "max": round(max(ordered), 3)}


def is_loopback_host(host: str) -> bool:
    if host == "localhost":
        return True
    try:
        infos = socket.getaddrinfo(host, None)
    except socket.gaierror:
        return False
    return all(ip_is_loopback(item[4][0]) for item in infos)


def ip_is_loopback(value: str) -> bool:
    try:
        return urllib.parse.ipaddress.ip_address(value).is_loopback  # type: ignore[attr-defined]
    except AttributeError:
        import ipaddress

        return ipaddress.ip_address(value).is_loopback


def repo_root() -> Path:
    return Path(__file__).resolve().parents[2]


def is_relative_to(path: Path, parent: Path) -> bool:
    try:
        path.relative_to(parent)
        return True
    except ValueError:
        try:
            common = os.path.commonpath([os.path.normcase(str(path)), os.path.normcase(str(parent))])
        except ValueError:
            return False
        return common == os.path.normcase(str(parent))


def now() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def iso_duration_seconds(start: str, end: str) -> float:
    s = datetime.fromisoformat(start.replace("Z", "+00:00"))
    e = datetime.fromisoformat(end.replace("Z", "+00:00"))
    return round((e - s).total_seconds(), 3)
