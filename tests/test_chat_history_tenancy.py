"""PR-C tenancy tests — chat_history + Mem0/memory partitioning, /conversations auth.

These run offline on the standalone SQLite stores (chat_history.db / the builtin
memory backend) — no Postgres, no network. They pin OD-6:

  * Conversations are partitioned by workspace_id + user_id: workspace/user A
    cannot list, read, continue, or delete workspace/user B's chats.
  * Memory recall is partitioned the same way (no cross-tenant memory bleed).
  * Self-host (single `main` workspace, user_id None) still works end-to-end.
  * The /conversations endpoints require auth in cloud (401 without) and bind to
    the keyless `main` workspace on self-host.
"""
import os
import sys

import pytest

os.environ.setdefault("DATABASE_URL", "sqlite:///./data/_pytest.db")

PROJECT_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if PROJECT_ROOT not in sys.path:
    sys.path.insert(0, PROJECT_ROOT)

from apps.api.services import chat_history as ch  # noqa: E402
from apps.api.services import memory as mem  # noqa: E402


# ── Fixtures: isolate the SQLite stores to a temp dir ────────────────────────

@pytest.fixture
def chat_db(tmp_path, monkeypatch):
    """Point chat_history at a throwaway DB and init its schema."""
    db_path = str(tmp_path / "chat_history.db")
    monkeypatch.setattr(ch, "DATA_DIR", str(tmp_path))
    monkeypatch.setattr(ch, "DB_PATH", db_path)
    conn = ch._get_db()
    ch._init_tables(conn)
    conn.close()
    return db_path


@pytest.fixture
def mem_store(tmp_path, monkeypatch):
    """Bind the memory singleton to a throwaway builtin SQLite backend."""
    inst = mem._BuiltinMemory(str(tmp_path / "mem.db"))
    monkeypatch.setattr(mem, "_memory_instance", inst)
    return inst


# ── chat_history: conversation isolation by (workspace, user) ────────────────

def test_conversations_isolated_by_workspace(chat_db):
    a = ch.create_conversation("W1", 1, title="A1")
    b = ch.create_conversation("W2", 2, title="B1")

    a_list = ch.list_conversations("W1", 1)
    b_list = ch.list_conversations("W2", 2)
    assert [c["id"] for c in a_list] == [a["id"]]
    assert [c["id"] for c in b_list] == [b["id"]]

    # Cross-tenant read of a known id → None (no existence leak).
    assert ch.get_conversation(a["id"], "W2", 2) is None
    assert ch.get_conversation(a["id"], "W1", 1) is not None


def test_conversations_isolated_by_user_within_workspace(chat_db):
    """Same workspace, different users → still partitioned (OD-6 keeps user_id)."""
    a = ch.create_conversation("W1", 1, title="user1")
    b = ch.create_conversation("W1", 2, title="user2")
    assert [c["id"] for c in ch.list_conversations("W1", 1)] == [a["id"]]
    assert [c["id"] for c in ch.list_conversations("W1", 2)] == [b["id"]]
    assert ch.get_conversation(b["id"], "W1", 1) is None


def test_messages_not_readable_cross_tenant(chat_db):
    a = ch.create_conversation("W1", 1)
    ch.add_message(a["id"], "user", "secret W1 message")
    # Owner sees the message.
    assert any("secret" in m["content"] for m in ch.get_messages(a["id"], "W1", 1))
    # Another tenant guessing the conv id sees nothing.
    assert ch.get_messages(a["id"], "W2", 2) == []


def test_delete_is_scoped(chat_db):
    a = ch.create_conversation("W1", 1)
    # Wrong tenant cannot delete.
    ch.delete_conversation(a["id"], "W2", 2)
    assert ch.get_conversation(a["id"], "W1", 1) is not None
    # Owner can.
    ch.delete_conversation(a["id"], "W1", 1)
    assert ch.get_conversation(a["id"], "W1", 1) is None


def test_self_host_user_none_roundtrips(chat_db):
    """Self-host: user_id None keys on a stable sentinel and works end-to-end."""
    c = ch.create_conversation("main", None, title="self-host chat")
    ch.add_message(c["id"], "user", "hi")
    assert [x["id"] for x in ch.list_conversations("main", None)] == [c["id"]]
    assert ch.get_conversation(c["id"], "main", None) is not None
    assert len(ch.get_messages(c["id"], "main", None)) == 1
    # None and "" normalize to the same partition.
    assert ch.get_conversation(c["id"], "main", "") is not None
    # A real user in the same workspace does NOT see the self-host chat.
    assert ch.list_conversations("main", 7) == []


def test_workspace_id_required(chat_db):
    with pytest.raises(ValueError):
        ch.create_conversation("", 1)
    with pytest.raises(ValueError):
        ch.list_conversations("", 1)


# ── memory: partitioned recall (no cross-tenant bleed) ───────────────────────

def test_memory_partitioned_by_workspace(mem_store):
    mem.add_memory("User asked about IT staffing firms in Pune", "W1", user_id=1)
    # Same query under a different workspace → no recall.
    assert mem.search_memory("staffing firms pune", "W2", user_id=2) == []
    # Under the owning tenant → recalled.
    hits = mem.search_memory("staffing firms pune", "W1", user_id=1)
    assert any("Pune" in h["memory"] for h in hits)


def test_memory_partitioned_by_user_within_workspace(mem_store):
    mem.add_memory("alice likes concise answers", "W1", user_id=1)
    assert mem.search_memory("concise answers", "W1", user_id=2) == []
    assert mem.search_memory("concise answers", "W1", user_id=1)


def test_memory_self_host_user_none(mem_store):
    mem.add_memory("self host note about pune leads", "main", user_id=None)
    assert mem.search_memory("pune leads", "main", user_id=None)
    assert mem.get_all_memories("main", None)
    # A real user can't see the self-host memory.
    assert mem.search_memory("pune leads", "main", user_id=5) == []


def test_memory_namespace_shape():
    assert mem._namespace("W1", 1) == "W1:1"
    assert mem._namespace("W1", None) == f"W1:{mem.SELF_HOST_USER}"
    with pytest.raises(ValueError):
        mem._namespace("", 1)


# ── /conversations endpoints: auth (cloud fail-closed, self-host keyless) ────

def _client():
    from fastapi import FastAPI
    from fastapi.testclient import TestClient
    from apps.api.routers import copilotkit as ck

    app = FastAPI()
    app.include_router(ck.router)
    return TestClient(app), ck


def test_conversations_endpoint_requires_auth_cloud(monkeypatch):
    client, ck = _client()
    monkeypatch.setattr(ck.settings, "CHAT_REQUIRE_AUTH", True)
    # No Authorization header → 401, never leaks any tenant's conversations.
    for verb, path in [("get", "/api/copilotkit/conversations"),
                       ("get", "/api/copilotkit/conversations/abc"),
                       ("delete", "/api/copilotkit/conversations/abc"),
                       ("get", "/api/copilotkit/memories")]:
        resp = getattr(client, verb)(path)
        assert resp.status_code == 401, f"{verb} {path} -> {resp.status_code}"


def test_conversations_endpoint_self_host_keyless(chat_db, monkeypatch):
    client, ck = _client()
    monkeypatch.setattr(ck.settings, "CHAT_REQUIRE_AUTH", False)
    monkeypatch.setattr(ck.ws_manager, "_get_active_workspace_id", lambda: "main")
    monkeypatch.setattr(ck.ws_manager, "workspace_slug", lambda wid: "main")

    # Seed a self-host conversation, then list it through the keyless endpoint.
    ch.create_conversation("main", None, title="self-host")
    resp = client.get("/api/copilotkit/conversations")
    assert resp.status_code == 200
    convs = resp.json()["conversations"]
    assert len(convs) == 1 and convs[0]["title"] == "self-host"
