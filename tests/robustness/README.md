# Photo Edge robustness harness

This suite drives the local Photo Edge Orchestrator through the documented HTTP API and reconciles accepted uploads against SQLite, the filesystem, audit rows, optional stdout NDJSON capture, and loopback Prometheus metrics.

The classic scenarios run against a service that is already started by the operator. The `operational-*` scenarios build a disposable `cmd/photoedge` binary under `--work-root`, start it on loopback, capture stdout NDJSON, and clean up only the process they started. Run every scenario only against a disposable local root and database. Do not point it at the repository `data/` directory or at production/user data.

## Configuration

Copy `config.example.env` to `config.local.env` and fill the local values. `config.local.env`, generated reports, and temporary harness data are ignored by Git.

Service process variables use `PHOTOEDGE_*`, for example:

```powershell
$env:PHOTOEDGE_ROOT="C:\tmp\photoedge-root"
$env:PHOTOEDGE_BIND="127.0.0.1:8090"
$env:PHOTOEDGE_TOKEN="local-test-token"
$env:PHOTOEDGE_DEVICE_ID="camera_01"
$env:PHOTOEDGE_EVENT_ID="local-event"
go run .\cmd\photoedge
```

Harness variables use `PHOTOEDGE_ROBUST_*`, for example:

```env
PHOTOEDGE_ROBUST_BASE_URL=http://127.0.0.1:8090
PHOTOEDGE_ROBUST_API_TOKEN=local-test-token
PHOTOEDGE_ROBUST_EVENT_ID=local-event
PHOTOEDGE_ROBUST_DEVICE_ID=camera_01
PHOTOEDGE_ROBUST_STORAGE_ROOT=C:\tmp\photoedge-root
PHOTOEDGE_ROBUST_DB_PATH=C:\tmp\photoedge-root\db\orchestrator.db
PHOTOEDGE_ROBUST_LOG_PATH=C:\tmp\photoedge-stdout.ndjson
```

The log path is optional. When it is absent, log verification is reported as `not-run` with an explicit precondition instead of being treated as product evidence.

## Commands

```powershell
python tests\robustness\runner.py --scenario smoke --verify metrics --config tests\robustness\config.local.env
python tests\robustness\runner.py --scenario single-upload --config tests\robustness\config.local.env
python tests\robustness\runner.py --scenario single-upload --verify sqlite,filesystem,audit,logs --config tests\robustness\config.local.env
python tests\robustness\runner.py --scenario sequential-upload --photos 10 --concurrency 1 --config tests\robustness\config.local.env
python tests\robustness\runner.py --scenario concurrent-upload --photos 10 --concurrency 2 --config tests\robustness\config.local.env
python tests\robustness\runner.py --scenario invalid-inputs --config tests\robustness\config.local.env
python tests\robustness\runner.py --scenario idempotency --config tests\robustness\config.local.env
python tests\robustness\runner.py --scenario all --photos 10 --concurrency 2 --config tests\robustness\config.local.env
```

Reports are written as JSON and Markdown under `tests/robustness/reports/`. They include sanitized configuration, command, Python/platform details, counts, latencies, inconsistencies, limitations, and covered evidence IDs. Raw bearer tokens, Authorization headers, raw idempotency keys, and fixture bytes are never written.

To reproduce the report sanitization check with ripgrep, run `rg -uuu -n "photoedge-operational-secret-token|Bearer photoedge-operational-secret-token|Authorization|Idempotency-Key|/9j/4AAQSkZJRgABAQ|ffd8ffe0|not a jpeg" tests/robustness/reports`. On Windows hosts without `rg`, the equivalent is `Get-ChildItem -Path tests\\robustness\\reports -Recurse -File | Select-String -Pattern "photoedge-operational-secret-token|Bearer photoedge-operational-secret-token|Authorization|Idempotency-Key|/9j/4AAQSkZJRgABAQ|ffd8ffe0|not a jpeg"`; it must return no matches.

## Operational local scenarios

Operational commands require explicit disposable paths. A safe Windows pattern is:

```powershell
$root=Join-Path $env:TEMP ('photoedge-op-smoke-' + [guid]::NewGuid().ToString('N'))
python -m tests.robustness.runner --scenario operational-smoke --work-root $root --reports-dir (Join-Path $root 'reports') --photos 1 --concurrency 1 --timeout-seconds 45
```

Available local-safe scenarios:

- `operational-smoke`: builds and starts `cmd/photoedge`, waits for readiness, verifies health/metrics, performs one upload, reconciles SQLite/filesystem/audit, and validates stdout NDJSON.
- `operational-restart-recovery`: prepares an expired lease only in the disposable SQLite database, restarts the managed process, and verifies `RECOVERED_AFTER_RESTART` without claiming physical power-loss coverage.
- `operational-storage-limit`: sets a safe `PHOTOEDGE_MIN_FREE_BYTES` threshold and classifies startup/readiness failure without filler files, quota, or mount operations.
- `operational-load`: performs bounded consistency load with at most 12 photos, concurrency 3, duration 90 seconds, and timeout 120 seconds; it is not a benchmark.
- `operational-timeout-cleanup`: verifies that a process started by the harness is terminated or reported as a failure.

Requests for `--request-system disk-fill`, quota, mount, power-loss, mobile/LAN validation, or material load are blocked as `SYSTEM_NOT_AUTHORIZED` in the local tier before spawning a process.

## Scenarios

- `smoke`: checks liveness, readiness, SQLite read-only access, and metrics when requested and loopback-accessible.
- `single-upload`: sends one JPEG fixture using Bearer auth, `Idempotency-Key`, multipart `photo`, and optional `event_id`; a `202` must reconcile with `photos`, `photo_files`, `jobs`, and `audit_events`.
- `sequential-upload`: sends a small sequential batch and reconciles every accepted upload independently.
- `concurrent-upload`: sends a small thread-pool batch and classifies every result without relying on global metrics as truth.
- `invalid-inputs`: sends expected rejections and verifies they do not create durable acceptance for their logical keys.
- `idempotency`: sends A/K, replays A/K, then sends B/K and verifies same-photo replay plus `409` conflict without new logical entities.
- `all`: runs the automatic scenarios above. Manual SYSTEM procedures are not included.

## Manual SYSTEM procedures

Quota/mount/disk fill, real low-disk certification, real phones, LAN validation, material load, and physical power-loss are outside the automatic local budget. They require a disposable root, explicit human approval, and a separate SYSTEM procedure before execution.
