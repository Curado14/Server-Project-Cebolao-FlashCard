import unittest

from tests.robustness import core, runner


class OperationalContractCLITests(unittest.TestCase):
    def test_reports_use_approved_change_id(self):
        self.assertEqual(core.CHANGE_ID, "robustez-operacional-servidor-photo-edge")

    def test_operational_scenarios_are_public_cli_contract(self):
        parser = runner.build_parser()
        scenarios = [
            "operational-smoke",
            "operational-restart-recovery",
            "operational-storage-limit",
            "operational-load",
            "operational-timeout-cleanup",
        ]
        for scenario in scenarios:
            with self.subTest(scenario=scenario):
                parser.parse_args(
                    [
                        "--scenario",
                        scenario,
                        "--work-root",
                        "C:\\tmp\\photoedge-contract-root",
                        "--reports-dir",
                        "C:\\tmp\\photoedge-contract-root\\reports",
                        "--no-report",
                    ]
                )


if __name__ == "__main__":
    unittest.main()
