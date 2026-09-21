#!/usr/bin/env python3
"""Run the authenticated OpenGTM HTTP workflow smoke test."""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from apps.api.services.evaluation.live_smoke import main

if __name__ == "__main__":
    raise SystemExit(main())
