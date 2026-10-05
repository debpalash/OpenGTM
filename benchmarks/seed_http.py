"""Create one user and workspace in the throwaway database and print a token.

Runs inside the benchmark sandbox (see run.py), so workspaces.db and friends
land in the sandbox's data/ directory, never the repository's.

The authenticated endpoints in the HTTP benchmark need a real identity: both
FastAPI and the Go server's /api/v2 routes authorize through FastAPI's
workspace-context dependency. Prints JSON {"token", "workspace_id"}.
"""

from __future__ import annotations

import json
import os
import sys
from datetime import timedelta

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if ROOT not in sys.path:
    sys.path.insert(0, ROOT)

from apps.api.auth import create_access_token, get_password_hash  # noqa: E402
from apps.api.database import SessionLocal  # noqa: E402
from apps.api.models import User  # noqa: E402
from apps.api.services.workspace import manager  # noqa: E402


def main() -> None:
    with SessionLocal() as db:
        user = db.query(User).filter(User.username == "bench").first()
        if user is None:
            user = User(
                username="bench",
                hashed_password=get_password_hash("bench-only"),
                is_active=True,
                is_admin=True,
                role="admin",
            )
            db.add(user)
            db.commit()
            db.refresh(user)
        user_id = user.id
    ws = manager.create_workspace("Bench", owner_id=user_id)
    token = create_access_token({"sub": "bench", "amr": ["pwd"]}, expires_delta=timedelta(hours=6))
    json.dump({"token": token, "workspace_id": ws.id}, sys.stdout)


if __name__ == "__main__":
    main()
