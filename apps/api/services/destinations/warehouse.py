"""Portable checksum-verified JSONL warehouse batch activation."""

import hashlib
import json
from dataclasses import dataclass


@dataclass
class WarehouseBatchResult:
    success: bool
    summary: str
    error: str | None = None
    external_id: str | None = None


def build_jsonl(run_id: str, workspace_id: str, destination, rows: list[dict]) -> tuple[bytes, dict]:
    records = [{"_opengtm_lead_id": lead_id, **payload} for lead_id, payload in rows]
    record_lines = [json.dumps(row, sort_keys=True, separators=(",", ":"), default=str) for row in records]
    records_blob = ("\n".join(record_lines) + ("\n" if record_lines else "")).encode()
    manifest = {"type": "opengtm.audience.export", "version": 1, "run_id": run_id, "workspace_id": workspace_id, "audience_id": destination.audience_id, "destination_id": destination.id, "dataset": (destination.config or {}).get("dataset") or "opengtm_audience", "mode": (destination.config or {}).get("mode") or "snapshot", "row_count": len(records), "records_sha256": hashlib.sha256(records_blob).hexdigest()}
    body = (json.dumps({"_manifest": manifest}, sort_keys=True, separators=(",", ":")) + "\n").encode() + records_blob
    return body, manifest


async def sync_warehouse_batch(workspace_id: str, destination, run_id: str, rows: list[dict]) -> WarehouseBatchResult:
    from apps.api.services.automations.actions import pinned_get
    from apps.api.services.workspace.secrets import get_secret
    config = destination.config or {}
    secret = get_secret(workspace_id, str(config.get("header_secret_ref") or ""), "")
    if not secret:
        return WarehouseBatchResult(False, "", "warehouse authentication secret is missing")
    body, manifest = build_jsonl(run_id, workspace_id, destination, rows)
    headers = {"Content-Type": "application/x-ndjson", "Idempotency-Key": f"warehouse:{destination.id}:{run_id}", str(config.get("header_name") or "Authorization"): secret}
    response = await pinned_get(str(config["url"]), headers, method="POST", timeout=30.0, kwargs={"content": body})
    if not 200 <= response.status_code < 300:
        return WarehouseBatchResult(False, "", f"warehouse HTTP {response.status_code}: {response.text[:300]}")
    external_id = response.headers.get("x-job-id") or response.headers.get("location")
    if not external_id:
        try:
            parsed = response.json(); external_id = str(parsed.get("job_id") or parsed.get("id") or "") or None
        except Exception:
            pass
    return WarehouseBatchResult(True, f"exported {manifest['row_count']} rows; sha256={manifest['records_sha256'][:12]}", external_id=external_id)
