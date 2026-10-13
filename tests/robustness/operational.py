from __future__ import annotations

import argparse
import json
import os
import socket
import sqlite3
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from .core import (
    ALT_JPEG,
    Config,
    Harness,
    PreconditionError,
    ProductFailure,
    Report,
    VALID_JPEG,
    assert_no_secret_text,
    fixture_sentinels,
    is_relative_to,
    now,
    parse_verify,
    repo_root,
)


LOCAL_MAX_PHOTOS = 12
LOCAL_MAX_CONCURRENCY = 3
LOCAL_MAX_DURATION_SECONDS = 90
LOCAL_MAX_TIMEOUT_SECONDS = 120
DEFAULT_TOKEN = "photoedge-operational-secret-token"
DEFAULT_EVENT_ID = "operational-event"
DEFAULT_DEVICE_ID = "operational-camera"
SYSTEM_NOT_AUTHORIZED = "SYSTEM_NOT_AUTHORIZED"


@dataclass
class OperationalPaths:
    work_root: Path
    reports_dir: Path
    db_path: Path
    stdout_path: Path
    binary_path: Path


class ManagedPhotoEdge:
    def __init__(self, paths: OperationalPaths, cfg: Config, report: Report, timeout_seconds: float) -> None:
        self.paths = paths
        self.cfg = cfg
        self.report = report
        self.timeout_seconds = timeout_seconds
        self.process: subprocess.Popen[str] | None = None
        self.stdout_handle: Any | None = None

    def build(self) -> None:
        suffix = ".exe" if os.name == "nt" else ""
        self.paths.binary_path = self.paths.binary_path.with_suffix(suffix)
        cmd = ["go", "build", "-o", str(self.paths.binary_path), "./cmd/photoedge"]
        started = time.perf_counter()
        completed = subprocess.run(
            cmd,
            cwd=repo_root(),
            capture_output=True,
            text=True,
            timeout=max(120.0, self.timeout_seconds),
        )
        elapsed = round(time.perf_counter() - started, 3)
        output = (completed.stdout or "") + (completed.stderr or "")
        assert_no_secret_text(output, self.report.secret_terms)
        self.report.operational["build"] = {
            "command": " ".join(cmd),
            "exit_code": completed.returncode,
            "duration_seconds": elapsed,
        }
        if completed.returncode != 0:
            raise PreconditionError("go build failed for cmd/photoedge")

    def start(self, min_free_bytes: int | None = None) -> None:
        self.paths.work_root.mkdir(parents=True, exist_ok=True)
        self.paths.stdout_path.parent.mkdir(parents=True, exist_ok=True)
        env = os.environ.copy()
        env.update(
            {
                "PHOTOEDGE_ROOT": str(self.paths.work_root),
                "PHOTOEDGE_DB_PATH": str(self.paths.db_path),
                "PHOTOEDGE_BIND": bind_from_base_url(self.cfg.base_url),
                "PHOTOEDGE_TOKEN": self.cfg.token,
                "PHOTOEDGE_DEVICE_ID": self.cfg.device_id,
                "PHOTOEDGE_EVENT_ID": self.cfg.event_id,
                "PHOTOEDGE_INSTANCE_ID": "operational-harness",
            }
        )
        if min_free_bytes is not None:
            env["PHOTOEDGE_MIN_FREE_BYTES"] = str(min_free_bytes)
        self.stdout_handle = self.paths.stdout_path.open("w", encoding="utf-8")
        self.process = subprocess.Popen(
            [str(self.paths.binary_path)],
            cwd=repo_root(),
            env=env,
            stdout=self.stdout_handle,
            stderr=subprocess.STDOUT,
            text=True,
        )
        self.report.operational.setdefault("processes", []).append(
            {"pid": self.process.pid, "action": "started", "stdout": str(self.paths.stdout_path)}
        )

    def wait_ready(self) -> bool:
        deadline = time.monotonic() + self.timeout_seconds
        ready_url = self.cfg.base_url + "/health/ready"
        while time.monotonic() < deadline:
            if self.process and self.process.poll() is not None:
                return False
            try:
                with urllib.request.urlopen(ready_url, timeout=1.0) as response:
                    if response.status == 200:
                        return True
            except (OSError, urllib.error.HTTPError):
                time.sleep(0.2)
        return False

    def stop(self, method: str = "controlled-terminate", timeout: float = 5.0) -> dict[str, Any]:
        info: dict[str, Any] = {"method": method, "pid": self.process.pid if self.process else None}
        if self.process is None:
            info["alive_after_cleanup"] = False
            return info
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                info["kill_after_timeout"] = True
                self.process.kill()
                self.process.wait(timeout=timeout)
        info["exit_code"] = self.process.returncode
        info["alive_after_cleanup"] = self.process.poll() is None
        if self.stdout_handle is not None:
            self.stdout_handle.close()
            self.stdout_handle = None
        self.report.operational.setdefault("processes", []).append({"action": "stopped", **info})
        return info

    def __enter__(self) -> "ManagedPhotoEdge":
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        info = self.stop()
        if info.get("alive_after_cleanup"):
            self.report.fail("PhotoEdge child process remained alive after cleanup")


def run_operational_scenario(args: argparse.Namespace) -> tuple[Report, int]:
    try:
        paths = operational_paths(args)
        cfg = operational_config(args, paths)
        verify = parse_verify(args.verify)
        report = Report(args.scenario, cfg, args, verify)
        enrich_operational_report(report, args, paths)
    except PreconditionError as exc:
        report = precondition_failure_report(args, exc)
        write_report_if_enabled(report, args)
        return report, report.exit_code

    try:
        validate_local_limits(args)
        if args.request_system:
            block_system_request(report, args.request_system)
            report.finish("blocked", 2)
            write_report_if_enabled(report, args)
            return report, report.exit_code

        if args.scenario == "operational-smoke":
            operational_smoke(args, paths, cfg, report)
        elif args.scenario == "operational-load":
            operational_load(args, paths, cfg, report)
        elif args.scenario == "operational-restart-recovery":
            operational_restart_recovery(args, paths, cfg, report)
        elif args.scenario == "operational-storage-limit":
            operational_storage_limit(args, paths, cfg, report)
        elif args.scenario == "operational-timeout-cleanup":
            operational_timeout_cleanup(args, paths, cfg, report)
        else:
            raise PreconditionError(f"unknown operational scenario: {args.scenario}")

        if report.inconsistencies:
            report.finish("failed", 3)
        else:
            report.finish("passed", 0)
    except PreconditionError as exc:
        report.unexpected_errors.append({"type": type(exc).__name__, "message": str(exc)})
        report.finish("precondition-failed", exc.exit_code)
    except ProductFailure as exc:
        report.unexpected_errors.append({"type": type(exc).__name__, "message": str(exc)})
        report.finish("failed", exc.exit_code)
    except Exception as exc:  # pragma: no cover - defensive evidence path.
        report.unexpected_errors.append({"type": type(exc).__name__, "message": str(exc)})
        report.finish("harness-error", 4)

    write_report_if_enabled(report, args)
    return report, report.exit_code


def operational_paths(args: argparse.Namespace) -> OperationalPaths:
    work_root = Path(args.work_root) if args.work_root else Path(tempfile.mkdtemp(prefix="photoedge-operational-"))
    reports_dir = Path(args.reports_dir) if args.reports_dir else work_root / "reports"
    paths = OperationalPaths(
        work_root=work_root,
        reports_dir=reports_dir,
        db_path=work_root / "db" / "orchestrator.db",
        stdout_path=work_root / "stdout.ndjson",
        binary_path=work_root / "photoedge",
    )
    for label, path in {
        "work root": paths.work_root,
        "database path": paths.db_path,
        "stdout path": paths.stdout_path,
        "reports dir": paths.reports_dir,
        "binary path": paths.binary_path,
    }.items():
        ensure_safe_operational_path(label, path)
    return paths


def precondition_failure_report(args: argparse.Namespace, exc: PreconditionError) -> Report:
    paths = fallback_report_paths()
    cfg = Config(
        base_url="http://127.0.0.1:1",
        token=DEFAULT_TOKEN,
        event_id=DEFAULT_EVENT_ID,
        device_id=DEFAULT_DEVICE_ID,
        storage_root=paths.work_root,
        db_path=paths.db_path,
        log_path=paths.stdout_path,
        http_timeout_seconds=1.0,
        processing_timeout_seconds=1.0,
        reports_dir=paths.reports_dir,
    )
    cfg.validate_targets()
    report = Report(args.scenario, cfg, args, set())
    report.secret_terms.update(raw_path_terms(args))
    enrich_operational_report(report, args, paths)
    report.coverage.update({"PO-001", "EVD-004"})
    report.operational["precondition_before_spawn"] = True
    report.operational["spawned_process"] = False
    report.operational["rejected_cli_paths"] = rejected_cli_path_labels(args)
    report.operational["requested_reports_dir_was_used"] = False
    report.unexpected_errors.append({"type": type(exc).__name__, "message": str(exc)})
    report.finish("precondition-failed", exc.exit_code)
    return report


def fallback_report_paths() -> OperationalPaths:
    work_root = Path(tempfile.mkdtemp(prefix="photoedge-op-precondition-"))
    return OperationalPaths(
        work_root=work_root,
        reports_dir=work_root / "reports",
        db_path=work_root / "db" / "orchestrator.db",
        stdout_path=work_root / "stdout.ndjson",
        binary_path=work_root / "photoedge",
    )


def raw_path_terms(args: argparse.Namespace) -> set[str]:
    terms = set()
    for value in (getattr(args, "work_root", None), getattr(args, "reports_dir", None)):
        if value:
            terms.add(str(value))
            try:
                terms.add(str(Path(value).resolve()))
            except OSError:
                pass
    return terms


def rejected_cli_path_labels(args: argparse.Namespace) -> list[str]:
    labels = []
    if getattr(args, "work_root", None):
        labels.append("work_root")
    if getattr(args, "reports_dir", None):
        labels.append("reports_dir")
    return labels


def operational_config(args: argparse.Namespace, paths: OperationalPaths) -> Config:
    port = reserve_loopback_port()
    cfg = Config(
        base_url=f"http://127.0.0.1:{port}",
        token=DEFAULT_TOKEN,
        event_id=DEFAULT_EVENT_ID,
        device_id=DEFAULT_DEVICE_ID,
        storage_root=paths.work_root,
        db_path=paths.db_path,
        log_path=paths.stdout_path,
        http_timeout_seconds=min(float(args.timeout_seconds or 30.0), 30.0),
        processing_timeout_seconds=float(args.timeout_seconds or 60.0),
        default_photos=args.photos or 1,
        default_concurrency=args.concurrency or 1,
        reports_dir=paths.reports_dir,
    )
    cfg.validate_targets()
    return cfg


def enrich_operational_report(report: Report, args: argparse.Namespace, paths: OperationalPaths) -> None:
    report.coverage.update({"REQ-001", "REQ-002", "REQ-008", "INV-001", "INV-005", "INV-006"})
    report.secret_terms.update({"Authorization", "Bearer", "Idempotency-Key"})
    report.secret_terms.update(fixture_sentinels(VALID_JPEG))
    report.secret_terms.update(fixture_sentinels(ALT_JPEG))
    report.operational.update(
        {
            "tier": "local-safe",
            "change_id": report.as_dict()["change_id"],
            "work_root": str(paths.work_root),
            "reports_dir": str(paths.reports_dir),
            "stdout_path": str(paths.stdout_path),
            "db_path": str(paths.db_path),
            "limits": {
                "photos": args.photos,
                "concurrency": args.concurrency,
                "duration_seconds": args.duration_seconds,
                "timeout_seconds": args.timeout_seconds,
                "local_max_photos": LOCAL_MAX_PHOTOS,
                "local_max_concurrency": LOCAL_MAX_CONCURRENCY,
                "local_max_duration_seconds": LOCAL_MAX_DURATION_SECONDS,
                "local_max_timeout_seconds": LOCAL_MAX_TIMEOUT_SECONDS,
            },
            "system_not_executed": [
                "disk-fill",
                "quota",
                "mount",
                "power-loss",
                "mobile",
                "lan",
                "material-load",
            ],
            "shutdown_graceful_claim": False,
        }
    )


def validate_local_limits(args: argparse.Namespace) -> None:
    photos = args.photos if args.photos is not None else (12 if args.scenario == "operational-load" else 1)
    concurrency = args.concurrency if args.concurrency is not None else (3 if args.scenario == "operational-load" else 1)
    duration = args.duration_seconds if args.duration_seconds is not None else 0
    timeout = args.timeout_seconds if args.timeout_seconds is not None else 60
    if photos > LOCAL_MAX_PHOTOS:
        raise PreconditionError(f"local-safe photos limit exceeded: {photos} > {LOCAL_MAX_PHOTOS}; SYSTEM required")
    if concurrency > LOCAL_MAX_CONCURRENCY:
        raise PreconditionError(
            f"local-safe concurrency limit exceeded: {concurrency} > {LOCAL_MAX_CONCURRENCY}; SYSTEM required"
        )
    if duration and duration > LOCAL_MAX_DURATION_SECONDS:
        raise PreconditionError(
            f"local-safe duration limit exceeded: {duration} > {LOCAL_MAX_DURATION_SECONDS}; SYSTEM required"
        )
    if timeout > LOCAL_MAX_TIMEOUT_SECONDS:
        raise PreconditionError(
            f"local-safe timeout limit exceeded: {timeout} > {LOCAL_MAX_TIMEOUT_SECONDS}; SYSTEM required"
        )


def block_system_request(report: Report, request: str) -> None:
    report.coverage.update({"PO-008", "EVD-011"})
    report.operational["system_gate"] = {
        "requested": request,
        "result": SYSTEM_NOT_AUTHORIZED,
        "bytes_of_filler_written": 0,
        "spawned_process": False,
    }
    report.unexpected_errors.append(
        {
            "type": SYSTEM_NOT_AUTHORIZED,
            "message": f"{request} is a SYSTEM procedure and is not authorized in local-safe execution",
        }
    )


def operational_smoke(args: argparse.Namespace, paths: OperationalPaths, cfg: Config, report: Report) -> None:
    report.coverage.update({"PO-001", "PO-002", "PO-003", "PO-008", "PO-011", "EVD-004"})
    with ManagedPhotoEdge(paths, cfg, report, args.timeout_seconds) as proc:
        proc.build()
        proc.start()
        if not proc.wait_ready():
            raise PreconditionError("PhotoEdge did not become ready for operational-smoke")
        harness = Harness(cfg, report)
        harness.smoke()
        harness.single_upload()
    records = read_stdout_records(paths.stdout_path, report, require_json=True)
    require_log_events(report, records, {"service.started", "service.ready", "http.request_completed"})


def operational_load(args: argparse.Namespace, paths: OperationalPaths, cfg: Config, report: Report) -> None:
    report.coverage.update({"PO-006", "PO-010", "EVD-007"})
    photos = args.photos if args.photos is not None else LOCAL_MAX_PHOTOS
    concurrency = args.concurrency if args.concurrency is not None else LOCAL_MAX_CONCURRENCY
    report.operational["load"] = {
        "photos": photos,
        "concurrency": concurrency,
        "duration_seconds": args.duration_seconds or LOCAL_MAX_DURATION_SECONDS,
        "benchmark_claim": False,
    }
    with ManagedPhotoEdge(paths, cfg, report, args.timeout_seconds) as proc:
        proc.build()
        proc.start()
        if not proc.wait_ready():
            raise PreconditionError("PhotoEdge did not become ready for operational-load")
        harness = Harness(cfg, report)
        harness.concurrent_upload(photos, concurrency)
        harness.idempotency()
    read_stdout_records(paths.stdout_path, report, require_json=True)


def operational_restart_recovery(args: argparse.Namespace, paths: OperationalPaths, cfg: Config, report: Report) -> None:
    report.coverage.update({"PO-004", "PO-009", "EVD-005"})
    if not args.force_expired_lease:
        raise PreconditionError("operational-restart-recovery requires --force-expired-lease")
    first = ManagedPhotoEdge(paths, cfg, report, args.timeout_seconds)
    try:
        first.build()
        first.start()
        if not first.wait_ready():
            raise PreconditionError("PhotoEdge did not become ready before recovery preparation")
        accepted = Harness(cfg, report).single_upload()
    finally:
        stop_info = first.stop("prepare-restart")
        if stop_info.get("alive_after_cleanup"):
            report.fail("PhotoEdge child process remained alive before restart")

    prepared = force_expired_lease(cfg.db_path, accepted["photo_id"])
    report.operational["prepared_recovery"] = prepared

    second = ManagedPhotoEdge(paths, cfg, report, args.timeout_seconds)
    try:
        second.start()
        if not second.wait_ready():
            raise PreconditionError("PhotoEdge did not become ready after restart")
        verify_recovery(cfg.db_path, prepared["job_id"], report)
    finally:
        stop_info = second.stop("post-restart-cleanup")
        if stop_info.get("alive_after_cleanup"):
            report.fail("PhotoEdge child process remained alive after restart")
    records = read_stdout_records(paths.stdout_path, report, require_json=True)
    require_log_events(report, records, {"job.recovered_after_restart"})
    report.limitations.append("restart=recovered-with-controlled-process-stop; physical power-loss was not executed")
    report.limitations.append("shutdown.completed is diagnostic and is not required by this oracle")


def operational_storage_limit(args: argparse.Namespace, paths: OperationalPaths, cfg: Config, report: Report) -> None:
    report.coverage.update({"PO-005", "PO-008", "EVD-006"})
    min_free = parse_min_free_bytes(args.min_free_bytes)
    report.operational["storage_limit"] = {
        "min_free_bytes": min_free,
        "bytes_of_filler_written": 0,
        "quota_or_mount_used": False,
    }
    proc = ManagedPhotoEdge(paths, cfg, report, args.timeout_seconds)
    try:
        proc.build()
        proc.start(min_free_bytes=min_free)
        ready = proc.wait_ready()
        if ready:
            harness = Harness(cfg, report)
            ready_result = harness.request("GET", "/health/ready")
            if ready_result.status == 200:
                report.fail("storage limit scenario unexpectedly became ready")
            before = harness.count_logical_entities("storage-limit")
            result = harness.upload(VALID_JPEG, "storage-limit", "storage-limit.jpg")
            if result.status == 202:
                report.fail("storage limit scenario accepted an upload")
            after = harness.count_logical_entities("storage-limit")
            if before != after:
                report.fail(f"storage limit created durable entities: before={before}, after={after}")
        else:
            report.operational["storage_limit"]["classification"] = "startup-not-ready"
    finally:
        stop_info = proc.stop("storage-limit-cleanup")
        if stop_info.get("alive_after_cleanup"):
            report.fail("PhotoEdge child process remained alive after storage limit cleanup")
    read_stdout_records(paths.stdout_path, report, require_json=False)
    report.limitations.append("low-disk-real=SYSTEM_NOT_AUTHORIZED; no filler, quota, or mount was executed")


def operational_timeout_cleanup(args: argparse.Namespace, paths: OperationalPaths, cfg: Config, report: Report) -> None:
    report.coverage.update({"PO-009", "EVD-009"})
    proc = ManagedPhotoEdge(paths, cfg, report, args.timeout_seconds)
    try:
        proc.build()
        proc.start()
        if not proc.wait_ready():
            raise PreconditionError("PhotoEdge did not become ready for timeout cleanup")
    finally:
        stop_info = proc.stop("timeout-cleanup", timeout=1.0)
        report.operational["timeout_cleanup"] = stop_info
        if stop_info.get("alive_after_cleanup"):
            report.fail("PhotoEdge child process remained alive after timeout cleanup")
    read_stdout_records(paths.stdout_path, report, require_json=True)


def force_expired_lease(db_path: Path, photo_id: str) -> dict[str, str]:
    expired = "2000-01-01T00:00:00Z"
    updated = now()
    con = sqlite3.connect(db_path)
    con.row_factory = sqlite3.Row
    try:
        row = con.execute("SELECT id FROM jobs WHERE photo_id=? ORDER BY created_at LIMIT 1", (photo_id,)).fetchone()
        if row is None:
            raise ProductFailure(f"cannot prepare restart recovery: no job for {photo_id}")
        job_id = str(row["id"])
        con.execute(
            "UPDATE jobs SET status='RUNNING', lease_owner='operational-harness', lease_until=?, attempt_count=1, updated_at=? WHERE id=?",
            (expired, updated, job_id),
        )
        con.commit()
        return {"photo_id": photo_id, "job_id": job_id, "lease_until": expired}
    finally:
        con.close()


def verify_recovery(db_path: Path, job_id: str, report: Report) -> None:
    con = sqlite3.connect(db_path)
    con.row_factory = sqlite3.Row
    try:
        row = con.execute("SELECT status, lease_until FROM jobs WHERE id=?", (job_id,)).fetchone()
        if row is None:
            report.fail(f"recovery job disappeared: {job_id}")
            return
        if row["status"] == "RUNNING" and str(row["lease_until"] or "") < now():
            report.fail(f"job remained RUNNING with expired lease after restart: {job_id}")
        recovered = con.execute(
            "SELECT count(*) FROM audit_events WHERE entity_id=? AND action='RECOVERED_AFTER_RESTART'",
            (job_id,),
        ).fetchone()[0]
        if int(recovered) < 1:
            report.fail(f"missing RECOVERED_AFTER_RESTART audit for {job_id}")
        report.operational["recovery_result"] = {
            "job_id": job_id,
            "status": row["status"],
            "lease_until": row["lease_until"],
            "recovered_audit_rows": int(recovered),
        }
    finally:
        con.close()


def read_stdout_records(stdout_path: Path, report: Report, require_json: bool) -> list[dict[str, Any]]:
    if not stdout_path.exists():
        report.fail(f"stdout capture does not exist: {stdout_path}")
        return []
    raw = stdout_path.read_text(encoding="utf-8", errors="replace")
    assert_no_secret_text(raw, report.secret_terms)
    records: list[dict[str, Any]] = []
    non_json = 0
    for lineno, line in enumerate(raw.splitlines(), start=1):
        if not line.strip():
            continue
        try:
            item = json.loads(line)
        except json.JSONDecodeError:
            non_json += 1
            if require_json:
                report.fail(f"stdout line {lineno} is not valid JSON")
            continue
        if isinstance(item, dict):
            records.append(item)
    report.operational["stdout"] = {
        "path": str(stdout_path),
        "records": len(records),
        "non_json_lines": non_json,
        "required_ndjson": require_json,
    }
    return records


def require_log_events(report: Report, records: list[dict[str, Any]], required: set[str]) -> None:
    events = {str(item.get("event")) for item in records if item.get("event")}
    missing = sorted(required - events)
    if missing:
        report.fail("stdout capture missing event(s): " + ", ".join(missing))


def reserve_loopback_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def bind_from_base_url(base_url: str) -> str:
    return base_url.removeprefix("http://")


def parse_min_free_bytes(value: str | None) -> int:
    if value is None:
        return (1 << 64) - 1
    try:
        parsed = int(value)
    except ValueError as exc:
        raise PreconditionError("min-free-bytes must be an integer") from exc
    if parsed <= 0:
        raise PreconditionError("min-free-bytes must be positive")
    return parsed


def ensure_safe_operational_path(label: str, path: Path) -> None:
    resolved = path.resolve()
    data_root = (repo_root() / "data").resolve()
    if same_or_child(resolved, data_root):
        raise PreconditionError(f"refusing repository data/ as operational {label}")


def same_or_child(path: Path, parent: Path) -> bool:
    return path == parent or is_relative_to(path, parent)


def write_report_if_enabled(report: Report, args: argparse.Namespace) -> None:
    if not args.no_report:
        report.write()
