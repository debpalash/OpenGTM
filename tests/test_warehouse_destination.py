import asyncio
import hashlib
import json

from apps.api.services.destinations.warehouse import (
    WarehouseBatchResult,
    WarehouseExportBuffer,
    build_jsonl,
    sync_warehouse_export,
)
from tests.test_audience_destinations import destination_app  # noqa: F401


def test_jsonl_manifest_is_deterministic_and_verifiable():
    destination = type("Destination", (), {"id": "dest", "audience_id": "aud", "config": {"dataset": "sales.accounts", "mode": "upsert"}})()
    body, manifest = build_jsonl("run", "ws", destination, [(2, {"company": "Beta"}), (1, {"company": "Acme"})])
    lines = body.decode().splitlines()
    assert json.loads(lines[0])["_manifest"] == manifest
    records_blob = ("\n".join(lines[1:]) + "\n").encode()
    assert hashlib.sha256(records_blob).hexdigest() == manifest["records_sha256"]
    assert manifest["row_count"] == 2 and json.loads(lines[1])["_opengtm_lead_id"] == 2


def test_jsonl_export_spills_to_disk_without_changing_manifest():
    destination = type("Destination", (), {
        "id": "dest", "audience_id": "aud", "config": {},
    })()
    export = WarehouseExportBuffer(
        "run", "ws", destination, max_memory_bytes=1,
    )
    try:
        export.append(1, {"company": "Acme"})
        assert export.rolled_to_disk is True
        body = export.body_bytes()
        lines = body.splitlines(keepends=True)
        manifest = json.loads(lines[0])["_manifest"]
        assert manifest["row_count"] == 1
        assert hashlib.sha256(b"".join(lines[1:])).hexdigest() == manifest["records_sha256"]
    finally:
        export.close()


def test_warehouse_transport_streams_checksum_valid_jsonl(monkeypatch):
    destination = type("Destination", (), {
        "id": "dest", "audience_id": "aud",
        "config": {
            "url": "https://warehouse.example.test/ingest",
            "header_secret_ref": "WAREHOUSE_TOKEN",
        },
    })()
    export = WarehouseExportBuffer("run", "ws", destination, max_memory_bytes=1)
    export.append(7, {"company": "Acme"})
    captured = {}

    class Response:
        status_code = 202
        text = ""
        headers = {"x-job-id": "load-7"}

        @staticmethod
        def json():
            return {}

    async def fake_pinned_get(url, headers, *, method, timeout, kwargs):
        chunks = []
        async for chunk in kwargs["content"]:
            chunks.append(chunk)
        captured.update(url=url, headers=headers, method=method, body=b"".join(chunks))
        return Response()

    monkeypatch.setattr("apps.api.services.workspace.secrets.get_secret", lambda *args: "Bearer test")
    monkeypatch.setattr("apps.api.services.automations.actions.pinned_get", fake_pinned_get)
    try:
        result = asyncio.run(sync_warehouse_export("ws", destination, export))
    finally:
        export.close()

    lines = captured["body"].splitlines(keepends=True)
    manifest = json.loads(lines[0])["_manifest"]
    assert hashlib.sha256(b"".join(lines[1:])).hexdigest() == manifest["records_sha256"]
    assert captured["method"] == "POST"
    assert captured["headers"]["Idempotency-Key"] == "warehouse:dest:run"
    assert result.success is True and result.external_id == "load-7"


def test_warehouse_destination_runs_as_one_idempotent_batch(destination_app, monkeypatch):
    tc, Session, _ = destination_app
    monkeypatch.setattr("apps.api.services.workspace.secrets.get_secret", lambda *args: "Bearer test")
    created = tc.post("/api/audience-destinations", json={"audience_id": "aud-1", "name": "Warehouse", "destination_type": "warehouse_http", "config": {"url": "https://warehouse.example.test/ingest", "header_secret_ref": "WAREHOUSE_TOKEN", "dataset": "gtm.contacts", "mode": "snapshot"}, "field_map": {"company": "account_name", "email": "email"}})
    assert created.status_code == 201, created.text
    run_id = tc.post(f"/api/audience-destinations/{created.json()['id']}/sync").json()["id"]
    captured = []

    async def fake_sync(workspace_id, destination, export):
        captured.append(export.body_bytes())
        return WarehouseBatchResult(True, "exported 1 row", external_id="load-1")

    from apps.api.services.destinations import engine
    monkeypatch.setattr(engine, "SessionLocal", Session)
    monkeypatch.setattr("apps.api.services.destinations.warehouse.sync_warehouse_export", fake_sync)
    asyncio.run(engine.handle_destination_sync(1, {"workspace_id": "ws-dest-1", "run_id": run_id}))
    records = [json.loads(line) for line in captured[0].decode().splitlines()]
    assert records[1] == {
        "_opengtm_lead_id": 42,
        "account_name": "Acme",
        "email": "buyer@acme.test",
    }
    delivery = tc.get(f"/api/audience-destinations/runs/{run_id}/deliveries").json()[0]
    assert delivery["status"] == "success" and delivery["external_id"] == "load-1"
