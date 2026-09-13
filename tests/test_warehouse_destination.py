import asyncio
import hashlib
import json

from apps.api.services.destinations.warehouse import WarehouseBatchResult, build_jsonl
from tests.test_audience_destinations import destination_app  # noqa: F401


def test_jsonl_manifest_is_deterministic_and_verifiable():
    destination = type("Destination", (), {"id": "dest", "audience_id": "aud", "config": {"dataset": "sales.accounts", "mode": "upsert"}})()
    body, manifest = build_jsonl("run", "ws", destination, [(2, {"company": "Beta"}), (1, {"company": "Acme"})])
    lines = body.decode().splitlines()
    assert json.loads(lines[0])["_manifest"] == manifest
    records_blob = ("\n".join(lines[1:]) + "\n").encode()
    assert hashlib.sha256(records_blob).hexdigest() == manifest["records_sha256"]
    assert manifest["row_count"] == 2 and json.loads(lines[1])["_opengtm_lead_id"] == 2


def test_warehouse_destination_runs_as_one_idempotent_batch(destination_app, monkeypatch):
    tc, Session, _ = destination_app
    monkeypatch.setattr("apps.api.services.workspace.secrets.get_secret", lambda *args: "Bearer test")
    created = tc.post("/api/audience-destinations", json={"audience_id": "aud-1", "name": "Warehouse", "destination_type": "warehouse_http", "config": {"url": "https://warehouse.example.test/ingest", "header_secret_ref": "WAREHOUSE_TOKEN", "dataset": "gtm.contacts", "mode": "snapshot"}, "field_map": {"company": "account_name", "email": "email"}})
    assert created.status_code == 201, created.text
    run_id = tc.post(f"/api/audience-destinations/{created.json()['id']}/sync").json()["id"]
    captured = []

    async def fake_sync(workspace_id, destination, run, rows):
        captured.extend(rows)
        return WarehouseBatchResult(True, "exported 1 row", external_id="load-1")

    from apps.api.services.destinations import engine
    monkeypatch.setattr(engine, "SessionLocal", Session)
    monkeypatch.setattr("apps.api.services.destinations.warehouse.sync_warehouse_batch", fake_sync)
    asyncio.run(engine.handle_destination_sync(1, {"workspace_id": "ws-dest-1", "run_id": run_id}))
    assert captured == [(42, {"account_name": "Acme", "email": "buyer@acme.test"})]
    delivery = tc.get(f"/api/audience-destinations/runs/{run_id}/deliveries").json()[0]
    assert delivery["status"] == "success" and delivery["external_id"] == "load-1"
