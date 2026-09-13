from __future__ import annotations

import sys
from pathlib import Path

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    from core import build_parser, run_scenario
else:  # pragma: no cover
    from .core import build_parser, run_scenario


def main() -> int:
    args = build_parser().parse_args()
    report, code = run_scenario(args)
    print(f"{report.scenario}: {report.result} (exit={code})")
    if report.inconsistencies:
        print(f"inconsistencies={len(report.inconsistencies)}")
    if report.limitations:
        print(f"limitations={len(report.limitations)}")
    return code


if __name__ == "__main__":
    raise SystemExit(main())
