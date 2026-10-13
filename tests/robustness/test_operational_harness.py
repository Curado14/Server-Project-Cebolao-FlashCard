import argparse
import tempfile
import unittest
from pathlib import Path

from tests.robustness.core import PreconditionError
from tests.robustness.operational import (
    LOCAL_MAX_CONCURRENCY,
    LOCAL_MAX_PHOTOS,
    SYSTEM_NOT_AUTHORIZED,
    block_system_request,
    operational_config,
    operational_paths,
    validate_local_limits,
)
from tests.robustness.core import Report


def args(**overrides):
    values = {
        "scenario": "operational-smoke",
        "work_root": None,
        "reports_dir": None,
        "photos": 1,
        "concurrency": 1,
        "duration_seconds": None,
        "timeout_seconds": 30.0,
        "min_free_bytes": None,
        "request_system": None,
        "verify": "sqlite,filesystem,audit,logs,metrics",
        "no_report": True,
        "config": "tests/robustness/config.local.env",
        "force_expired_lease": False,
    }
    values.update(overrides)
    return argparse.Namespace(**values)


class OperationalHarnessTests(unittest.TestCase):
    def test_operational_paths_reject_repo_data_before_spawn(self):
        repo_data = Path.cwd() / "data" / "operational"
        with self.assertRaises(PreconditionError):
            operational_paths(args(work_root=str(repo_data), reports_dir=str(repo_data / "reports")))

    def test_local_limits_require_system_for_material_load(self):
        with self.assertRaises(PreconditionError):
            validate_local_limits(args(scenario="operational-load", photos=LOCAL_MAX_PHOTOS + 1))
        with self.assertRaises(PreconditionError):
            validate_local_limits(args(scenario="operational-load", concurrency=LOCAL_MAX_CONCURRENCY + 1))

    def test_system_gate_records_block_without_spawn(self):
        with tempfile.TemporaryDirectory() as tmp:
            parsed = args(work_root=tmp, reports_dir=str(Path(tmp) / "reports"))
            paths = operational_paths(parsed)
            cfg = operational_config(parsed, paths)
            report = Report("operational-smoke", cfg, parsed, {"metrics"})
            block_system_request(report, "disk-fill")
            self.assertEqual(report.operational["system_gate"]["result"], SYSTEM_NOT_AUTHORIZED)
            self.assertFalse(report.operational["system_gate"]["spawned_process"])
            self.assertEqual(report.operational["system_gate"]["bytes_of_filler_written"], 0)


if __name__ == "__main__":
    unittest.main()
