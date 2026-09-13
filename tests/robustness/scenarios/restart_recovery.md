# Restart recovery manual procedure

This SYSTEM scenario is intentionally manual and must not run by default.

Preconditions:

- Explicit approval for a controlled restart.
- Disposable `PHOTOEDGE_ROOT` and SQLite database, never the repository `data/` directory.
- Stdout NDJSON capture enabled by the operator.
- A small in-flight batch started by the harness.

Evidence to collect:

- Accepted photo identities before interruption.
- Job rows before and after restart.
- `RECOVERED_AFTER_RESTART` audit rows when leases expire and are recovered.
- Preservation of original files under the configured root.
- Sanitized stdout lines containing `job.recovered_after_restart`.

Stop if the configured root or database is not disposable.
