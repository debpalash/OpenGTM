#!/usr/bin/env python3
"""Attest one controlled-live integration certification."""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from apps.api.services.integrations.certification import (
    CERTIFICATION_KEY_ENV,
    attest_certificate,
)


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Attest an integration's controlled-live certification."
    )
    parser.add_argument("--input", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    key = os.getenv(CERTIFICATION_KEY_ENV, "")
    if not key:
        print(f"{CERTIFICATION_KEY_ENV} must be set", file=sys.stderr)
        return 2
    try:
        value = json.loads(Path(args.input).read_text(encoding="utf-8"))
        if not isinstance(value, dict):
            raise ValueError("certificate must be a JSON object")
        output = Path(args.output)
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(
            json.dumps(attest_certificate(value, key), indent=2) + "\n",
            encoding="utf-8",
        )
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"Unable to attest integration certification: {exc}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
