import argparse
import json
import tempfile
import unittest
from pathlib import Path

from tests.robustness.core import Config, PreconditionError, Report


class ConfigAndReportTests(unittest.TestCase):
    def test_rejects_unsafe_target_and_redacts_sentinels(self):
        repo_data = Path.cwd() / "data"
        with tempfile.TemporaryDirectory() as tmp:
            cfg_file = Path(tmp) / "bad.env"
            cfg_file.write_text(
                "\n".join(
                    [
                        "PHOTOEDGE_ROBUST_BASE_URL=http://127.0.0.1:8090",
                        "PHOTOEDGE_ROBUST_API_TOKEN=secret-token",
                        "PHOTOEDGE_ROBUST_EVENT_ID=local-event",
                        "PHOTOEDGE_ROBUST_DEVICE_ID=camera_01",
                        f"PHOTOEDGE_ROBUST_STORAGE_ROOT={repo_data}",
                        f"PHOTOEDGE_ROBUST_DB_PATH={repo_data / 'db' / 'orchestrator.db'}",
                    ]
                ),
                encoding="utf-8",
            )
            with self.assertRaises(PreconditionError):
                Config.load(cfg_file)

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "root"
            reports = Path(tmp) / "reports"
            db = root / "db" / "orchestrator.db"
            cfg = Config(
                base_url="http://127.0.0.1:8090",
                token="secret-token",
                event_id="local-event",
                device_id="camera_01",
                storage_root=root,
                db_path=db,
                log_path=None,
                reports_dir=reports,
            )
            args = argparse.Namespace(scenario="smoke", photos=1, concurrency=1)
            report = Report("smoke", cfg, args, {"metrics"})
            report.add_key_secret("raw-idempotency-key")
            report.unexpected_errors.append({"authorization": "Bearer secret-token", "key": "raw-idempotency-key"})
            report.finish("failed", 3)
            data = report.as_dict()
            rendered = json.dumps(data)
            self.assertNotIn("secret-token", rendered)
            self.assertNotIn("raw-idempotency-key", rendered)
            self.assertIn("[REDACTED]", rendered)


if __name__ == "__main__":
    unittest.main()
