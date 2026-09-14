"""Portable checksum-verified JSONL warehouse batch activation."""

import hashlib
import json
import tempfile
from dataclasses import dataclass
from typing import Iterable


@dataclass
class WarehouseBatchResult:
    success: bool
    summary: str
    error: str | None = None
    external_id: str | None = None


class WarehouseExportBuffer:
    """Incremental JSONL export that spills records to disk at a fixed bound."""

    def __init__(
        self, run_id: str, workspace_id: str, destination,
        *, max_memory_bytes: int = 1_000_000,
    ):
        self.run_id = run_id
        self.workspace_id = workspace_id
        self.destination = destination
        self.row_count = 0
        self._digest = hashlib.sha256()
        self._records = tempfile.SpooledTemporaryFile(max_size=max_memory_bytes, mode="w+b")

    def append(self, lead_id: int, payload: dict) -> None:
        record = {"_opengtm_lead_id": lead_id, **payload}
        line = (
            json.dumps(record, sort_keys=True, separators=(",", ":"), default=str) + "\n"
        ).encode()
        self._records.write(line)
        self._digest.update(line)
        self.row_count += 1

    @property
    def manifest(self) -> dict:
        config = self.destination.config or {}
        return {
            "type": "opengtm.audience.export",
            "version": 1,
            "run_id": self.run_id,
            "workspace_id": self.workspace_id,
            "audience_id": self.destination.audience_id,
            "destination_id": self.destination.id,
            "dataset": config.get("dataset") or "opengtm_audience",
            "mode": config.get("mode") or "snapshot",
            "row_count": self.row_count,
            "records_sha256": self._digest.hexdigest(),
        }

    @property
    def rolled_to_disk(self) -> bool:
        return bool(getattr(self._records, "_rolled", False))

    def _manifest_line(self) -> bytes:
        return (
            json.dumps({"_manifest": self.manifest}, sort_keys=True, separators=(",", ":"))
            + "\n"
        ).encode()

    async def iter_content(self, chunk_size: int = 64 * 1024):
        yield self._manifest_line()
        self._records.seek(0)
        while True:
            chunk = self._records.read(chunk_size)
            if not chunk:
                break
            yield chunk

    def body_bytes(self) -> bytes:
        self._records.seek(0)
        return self._manifest_line() + self._records.read()

    def close(self) -> None:
        self._records.close()


def build_jsonl(
    run_id: str, workspace_id: str, destination, rows: Iterable[tuple[int, dict]],
) -> tuple[bytes, dict]:
    export = WarehouseExportBuffer(run_id, workspace_id, destination)
    try:
        for lead_id, payload in rows:
            export.append(lead_id, payload)
        return export.body_bytes(), export.manifest
    finally:
        export.close()


async def sync_warehouse_batch(workspace_id: str, destination, run_id: str, rows: list[dict]) -> WarehouseBatchResult:
    export = WarehouseExportBuffer(run_id, workspace_id, destination)
    try:
        for lead_id, payload in rows:
            export.append(lead_id, payload)
        return await sync_warehouse_export(workspace_id, destination, export)
    finally:
        export.close()


async def sync_warehouse_export(
    workspace_id: str, destination, export: WarehouseExportBuffer,
) -> WarehouseBatchResult:
    from apps.api.services.automations.actions import pinned_get
    from apps.api.services.workspace.secrets import get_secret
    config = destination.config or {}
    secret = get_secret(workspace_id, str(config.get("header_secret_ref") or ""), "")
    if not secret:
        return WarehouseBatchResult(False, "", "warehouse authentication secret is missing")
    manifest = export.manifest
    headers = {"Content-Type": "application/x-ndjson", "Idempotency-Key": f"warehouse:{destination.id}:{export.run_id}", str(config.get("header_name") or "Authorization"): secret}
    response = await pinned_get(str(config["url"]), headers, method="POST", timeout=30.0, kwargs={"content": export.iter_content()})
    if not 200 <= response.status_code < 300:
        return WarehouseBatchResult(False, "", f"warehouse HTTP {response.status_code}: {response.text[:300]}")
    external_id = response.headers.get("x-job-id") or response.headers.get("location")
    if not external_id:
        try:
            parsed = response.json(); external_id = str(parsed.get("job_id") or parsed.get("id") or "") or None
        except Exception:
            pass
    return WarehouseBatchResult(True, f"exported {manifest['row_count']} rows; sha256={manifest['records_sha256'][:12]}", external_id=external_id)
