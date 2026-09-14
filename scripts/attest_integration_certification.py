#!/usr/bin/env python3
"""Attest one controlled-live integration certification."""

from __future__ import annotations

import argparse
import hashlib
import hmac
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


def _resolve_json_pointer(document: object, reference: object) -> None:
    value = str(reference or "")
    _, separator, pointer = value.partition("#")
    if not separator or not pointer.startswith("/"):
        raise ValueError(f"evidence reference is not a JSON pointer: {value}")
    current = document
    for raw_token in pointer[1:].split("/"):
        token = raw_token.replace("~1", "/").replace("~0", "~")
        if isinstance(current, dict) and token in current:
            current = current[token]
        elif isinstance(current, list) and token.isdigit() and int(token) < len(current):
            current = current[int(token)]
        else:
            raise ValueError(f"evidence reference does not resolve: {value}")


def _verify_check_references(certificate: dict, evidence_path: Path) -> None:
    try:
        document = json.loads(evidence_path.read_text(encoding="utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError("evidence artifact must be valid UTF-8 JSON") from exc
    for name, check in certificate.get("checks", {}).items():
        if isinstance(check, dict) and check.get("passed") is True:
            try:
                _resolve_json_pointer(document, check.get("evidence"))
            except ValueError as exc:
                raise ValueError(f"check {name}: {exc}") from exc


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Attest an integration's controlled-live certification."
    )
    parser.add_argument("--input", required=True)
    parser.add_argument(
        "--evidence", required=True,
        help="Local controlled-live evidence artifact whose SHA-256 is signed",
    )
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
        evidence_path = Path(args.evidence)
        digest = hashlib.sha256()
        with evidence_path.open("rb") as evidence:
            for chunk in iter(lambda: evidence.read(1024 * 1024), b""):
                digest.update(chunk)
        if not hmac.compare_digest(
            digest.hexdigest(), str(value.get("evidence_sha256") or ""),
        ):
            raise ValueError("evidence artifact SHA-256 does not match certificate")
        _verify_check_references(value, evidence_path)
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
