"""Print the OpenAPI contract: ``python -m browser_service.openapi_export > openapi.json``."""

from __future__ import annotations

import json
import sys

from .app import create_app, openapi_schema
from .config import Settings


def render() -> str:
    app = create_app(Settings(token="x" * 32, launch_browser=False), pool=object())
    return json.dumps(openapi_schema(app), indent=2, sort_keys=True) + "\n"


if __name__ == "__main__":
    sys.stdout.write(render())
