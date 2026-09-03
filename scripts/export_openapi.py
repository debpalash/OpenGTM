#!/usr/bin/env python3
"""Export the FastAPI OpenAPI document for the docs site.

    uv run python scripts/export_openapi.py          # write apps/docs/openapi/openapi.json
    uv run python scripts/export_openapi.py --check  # exit 1 if the committed file is stale

The app is imported with a throwaway SQLite database so no Postgres or
provider keys are needed. Output is deterministic (sorted keys) so the
--check mode is a plain byte comparison.
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import tempfile
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
OUT_PATH = REPO_ROOT / "apps" / "docs" / "openapi" / "openapi.json"
SERVERS = [
    {"url": "http://localhost:8000", "description": "Local API (uvicorn)"},
    {"url": "http://localhost:3000", "description": "Docker Compose (nginx front)"},
]
DESCRIPTION = (
    "OpenGTM REST API. Every endpoint here is available on self-hosted "
    "installs with no plan gating. Authenticate with `Authorization: Bearer "
    "<access token>` from `POST /api/auth/login`, or with a scoped MCP / "
    "ingest token where the endpoint says so."
)


def build_spec() -> dict:
    tmp = tempfile.mkdtemp(prefix="opengtm-openapi-")
    os.environ.setdefault("APP_ENV", "test")
    os.environ.setdefault("DATABASE_URL", f"sqlite:///{tmp}/openapi.db")
    os.environ.setdefault("YUPCHA_DB_INIT", "skip")
    sys.path.insert(0, str(REPO_ROOT))
    from apps.api.main import app  # noqa: WPS433 — import after env is set

    spec = app.openapi()
    spec.setdefault("info", {})
    spec["info"]["description"] = DESCRIPTION
    spec["info"]["license"] = {
        "name": "AGPL-3.0-only",
        "url": "https://github.com/debpalash/opengtm/blob/main/LICENSE",
    }
    spec["info"]["contact"] = {
        "name": "OpenGTM",
        "url": "https://opengtm.palash.dev",
    }
    spec["servers"] = SERVERS
    spec["externalDocs"] = {
        "description": "OpenGTM documentation",
        "url": "https://opengtm.palash.dev",
    }
    return spec


def render(spec: dict) -> str:
    return json.dumps(spec, indent=2, sort_keys=True, ensure_ascii=False) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="fail if the committed spec is stale")
    parser.add_argument("--out", type=Path, default=OUT_PATH)
    args = parser.parse_args()

    text = render(build_spec())
    if args.check:
        current = args.out.read_text() if args.out.exists() else ""
        if current != text:
            print(f"{args.out.relative_to(REPO_ROOT)} is stale; run scripts/export_openapi.py", file=sys.stderr)
            return 1
        print("openapi.json is up to date")
        return 0

    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(text)
    paths = len(json.loads(text)["paths"])
    print(f"wrote {args.out.relative_to(REPO_ROOT)} ({paths} paths)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
