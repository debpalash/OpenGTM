#!/usr/bin/env python3
"""Attest one controlled-live gauntlet history record."""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from apps.api.services.evaluation.gtm_gauntlet import (
    ATTESTATION_KEY_ENV,
    attest_validation_run,
)


def main() -> int:
    parser = argparse.ArgumentParser(
        description="HMAC-attest one controlled-live GTM gauntlet run record."
    )
    parser.add_argument("--input", required=True, help="Unsigned run-record JSON")
    parser.add_argument("--output", required=True, help="Attested run-record JSON")
    args = parser.parse_args()

    key = os.getenv(ATTESTATION_KEY_ENV, "")
    if not key:
        print(f"{ATTESTATION_KEY_ENV} must be set", file=sys.stderr)
        return 2
    try:
        with Path(args.input).open("r", encoding="utf-8") as handle:
            run = json.load(handle)
        if not isinstance(run, dict):
            raise ValueError("run record must be a JSON object")
        attested = attest_validation_run(run, key)
        output = Path(args.output)
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(
            json.dumps(attested, indent=2, ensure_ascii=False) + "\n",
            encoding="utf-8",
        )
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"Unable to attest validation record: {exc}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
