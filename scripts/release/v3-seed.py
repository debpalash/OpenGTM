"""Seed a v3.0.0 installation for scripts/release/rehearse-upgrade.sh.

Runs INSIDE a container built from the v3.0.0 tag (cwd = the tag's tree, deps installed with its own uv.lock),
against the PostgreSQL named by DATABASE_URL, with the v3 data directory mounted at /data. It starts the real v3
API (uvicorn), creates the data through that API, and writes /data/seed.json describing what it created so the
rehearsal can look for exactly those rows after the upgrade.

Through the v3 HTTP API: login, a second workspace, a saved provider key, a workspace secret, a lead, a workbook
function. In-process with v3's own code, because the HTTP route needs a live LLM: a chat conversation (the v3
chat_history module the copilot route calls). Directly in PostgreSQL: the first admin (v3 has no signup route; its
seed_demo script does the same) and one queue row.

Never run outside a throwaway container: it writes a data directory and an admin account.
"""
import json
import os
import subprocess
import sys
import time
import urllib.parse
import urllib.request

PASSWORD = os.environ["V3_ADMIN_PASSWORD"]
PROVIDER_KEY = os.environ["V3_PROVIDER_KEY"]
WS_SECRET = os.environ["V3_WS_SECRET"]
BASE = "http://127.0.0.1:8000"


def call(method, path, body=None, token=None, headers=None, form=None):
    data = None
    h = dict(headers or {})
    if form is not None:
        data = urllib.parse.urlencode(form).encode()
        h["Content-Type"] = "application/x-www-form-urlencoded"
    elif body is not None:
        data = json.dumps(body).encode()
        h["Content-Type"] = "application/json"
    if token:
        h["Authorization"] = "Bearer " + token
    req = urllib.request.Request(BASE + path, data=data, method=method, headers=h)
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return json.loads(r.read() or b"null")
    except urllib.error.HTTPError as e:
        raise SystemExit(f"v3 API {method} {path} -> {e.code}: {e.read()[:300]!r}")


def main():
    from sqlalchemy import text

    from apps.api.auth import get_password_hash
    from apps.api.database import SessionLocal
    from apps.api.models import User

    db = SessionLocal()
    admin = User(username="admin", hashed_password=get_password_hash(PASSWORD), is_active=True, is_admin=True, role="admin")
    db.add(admin)
    db.commit()
    db.refresh(admin)
    admin_id = admin.id
    db.close()

    server = subprocess.Popen(
        [sys.executable, "-m", "uvicorn", "apps.api.main:app", "--host", "127.0.0.1", "--port", "8000"],
        stdout=open("/tmp/v3-uvicorn.log", "w"), stderr=subprocess.STDOUT,
    )
    try:
        for _ in range(120):
            try:
                urllib.request.urlopen(BASE + "/docs", timeout=2)
                break
            except Exception:
                if server.poll() is not None:
                    raise SystemExit("v3 API exited:\n" + open("/tmp/v3-uvicorn.log").read()[-3000:])
                time.sleep(1)
        else:
            raise SystemExit("v3 API did not start:\n" + open("/tmp/v3-uvicorn.log").read()[-3000:])

        tok = call("POST", "/auth/token", form={"username": "admin", "password": PASSWORD})["access_token"]
        workspaces = call("GET", "/api/workspaces", token=tok)["workspaces"]
        main_ws = next(w for w in workspaces if w["slug"] == "main")["id"]
        acme = call("POST", "/api/workspaces", {"name": "Acme Rehearsal", "description": "second workspace", "icon": "A"}, tok)
        call("POST", "/api/workspaces/switch", {"workspace_id": main_ws}, tok)

        call("PUT", "/api/settings/providers/anthropic", {"api_key": PROVIDER_KEY}, tok)
        call("PUT", f"/api/settings/workspace-integrations/hubspot?workspace_id={acme['id']}", {"values": {"HUBSPOT_TOKEN": WS_SECRET}}, tok)
        lead = call("POST", "/api/lead", {"company": "Rehearsal Co", "website": "https://rehearsal.example", "email": "ceo@rehearsal.example"},
                    tok, headers={"X-Workspace-Id": acme["id"]})
        lead_main = call("POST", "/api/lead", {"company": "Main Workspace Co", "website": "https://main.example"},
                         tok, headers={"X-Workspace-Id": main_ws})
        func = call("POST", "/api/functions", {"name": "Rehearsal function", "description": "d", "columns_chain": [{"id": "c1", "name": "Email"}]}, tok)

        from apps.api.services import chat_history
        conv = chat_history.create_conversation(acme["id"], str(admin_id), title="Rehearsal chat")
        conv_id = conv["id"] if isinstance(conv, dict) else conv
        chat_history.add_message(conv_id, "user", "hello from v3")
        chat_history.add_message(conv_id, "assistant", "hello back")

        db = SessionLocal()
        db.execute(text("SELECT set_config('app.workspace_id', :w, false)"), {"w": acme["id"]})
        db.execute(text("INSERT INTO jobs (type, payload, status, priority, workspace_id, created_at, next_run_at) "
                        "VALUES ('run_workbook', CAST(:p AS json), 'pending', 0, :w, now(), now())"),
                   {"p": json.dumps({"workspace_id": acme["id"], "workbook_id": "none"}), "w": acme["id"]})
        db.execute(text("INSERT INTO jobs (type, payload, status, priority, workspace_id, created_at, completed_at) "
                        "VALUES ('collect', CAST(:p AS json), 'completed', 0, :w, now(), now())"),
                   {"p": json.dumps({"workspace_id": main_ws}), "w": main_ws})
        db.commit()
        db.close()

        out = {
            "admin_id": admin_id, "main_ws": main_ws, "acme_ws": acme["id"], "acme_slug": acme["slug"],
            "lead_id": lead["id"], "lead_main_id": lead_main["id"], "function_id": func.get("id"),
            "conversation_id": conv_id,
        }
        json.dump(out, open("/data/seed.json", "w"))
        print("v3 seeded:", json.dumps(out))
    finally:
        server.terminate()
        try:
            server.wait(timeout=20)
        except Exception:
            server.kill()


main()
