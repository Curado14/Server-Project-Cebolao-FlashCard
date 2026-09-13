# Disk degradation manual procedure

This SYSTEM scenario is intentionally manual and must not fill a real disk.

Use only a disposable filesystem boundary or a service-level minimum-free-space configuration that makes readiness fail safely without writing large filler files. Expected evidence is `/health/ready` returning `503`, uploads returning `503` before durable acceptance, no new `photos`/`jobs` for rejected attempts, and sanitized `storage.low_space` diagnostics when stdout capture is enabled.
