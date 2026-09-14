#!/usr/bin/env python3
"""Attest one controlled PostgreSQL scale-gate report."""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from apps.api.services.scale_certification import (
    SCALE_ATTESTATION_KEY_ENV,
    attest_scale_report,
)


def main() -> int:
    parser = argparse.ArgumentParser(description="Attest a controlled scale report")
    parser.add_argument("--input", required=True, help="Unsigned scale report JSON")
    parser.add_argument("--output", required=True, help="Attested scale report JSON")
    args = parser.parse_args()
    key = os.getenv(SCALE_ATTESTATION_KEY_ENV, "")
    if not key:
        print(f"{SCALE_ATTESTATION_KEY_ENV} must be set", file=sys.stderr)
        return 2
    try:
        report = json.loads(Path(args.input).read_text(encoding="utf-8"))
        if not isinstance(report, dict):
            raise ValueError("scale report must be a JSON object")
        output = Path(args.output)
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(
            json.dumps(attest_scale_report(report, key), indent=2) + "\n",
            encoding="utf-8",
        )
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"Unable to attest scale report: {exc}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
