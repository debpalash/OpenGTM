"""
data_collector Import — Imports leads from the Brazil data_collector submodule.

Reads the CNPJ and GitHub CSV outputs and maps them into the Yupcha Lead model.

Key design decisions:
- STREAMS the CSV row-by-row (never loads the full 666K into memory)
- Checkpoints progress to a JSON file after every batch
- On restart, skips already-processed rows by reading the checkpoint
- Catches per-batch errors so one bad row doesn't kill the whole import
"""

import csv
import json
from pathlib import Path
from typing import Optional

from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.scoring import score_leads

# Path to the data_collector submodule output
_COLLECTOR_ROOT = Path(__file__).resolve().parents[5] / "data_collector" / "data"
CNPJ_CSV = _COLLECTOR_ROOT / "processed" / "cnpj_leads.csv"
GITHUB_CSV = _COLLECTOR_ROOT / "processed" / "github_leads.csv"

# Legacy CLI checkpoint — API/worker jobs use tenant-specific paths below.
CHECKPOINT_FILE = (
    Path(__file__).resolve().parents[5]
    / "data"
    / "imports"
    / "br_import_checkpoint.json"
)

# Import tuning
BATCH_SIZE = 500          # Rows per DB transaction (smaller = less RAM)
LOG_EVERY = 10_000        # Print progress every N rows (reduce log spam)

# Brazilian state abbreviation → full name (for better display in UI)
_BR_STATES = {
    "AC": "Acre", "AL": "Alagoas", "AP": "Amapá", "AM": "Amazonas",
    "BA": "Bahia", "CE": "Ceará", "DF": "Distrito Federal", "ES": "Espírito Santo",
    "GO": "Goiás", "MA": "Maranhão", "MT": "Mato Grosso", "MS": "Mato Grosso do Sul",
    "MG": "Minas Gerais", "PA": "Pará", "PB": "Paraíba", "PR": "Paraná",
    "PE": "Pernambuco", "PI": "Piauí", "RJ": "Rio de Janeiro",
    "RN": "Rio Grande do Norte", "RS": "Rio Grande do Sul", "RO": "Rondônia",
    "RR": "Roraima", "SC": "Santa Catarina", "SP": "São Paulo", "SE": "Sergipe",
    "TO": "Tocantins",
}


# ── Checkpoint helpers ──────────────────────────────────────────────────


def checkpoint_path_for(slug: str) -> Path:
    """Return a tenant-specific checkpoint path on the shared data volume."""
    if not slug or slug in (".", "..") or "/" in slug or "\\" in slug:
        raise ValueError("valid workspace slug is required")
    path = Path(__file__).resolve().parents[5] / "data" / "workspaces" / slug / "imports"
    path.mkdir(parents=True, exist_ok=True)
    return path / "br_import_checkpoint.json"


def _load_checkpoint(checkpoint_file: Optional[Path] = None) -> dict:
    """Load checkpoint from disk. Returns dict with row offsets per module."""
    checkpoint_file = checkpoint_file or CHECKPOINT_FILE
    if checkpoint_file.exists():
        try:
            return json.loads(checkpoint_file.read_text())
        except (json.JSONDecodeError, OSError):
            pass
    return {"cnpj_row": 0, "github_done": False}


def _save_checkpoint(data: dict, checkpoint_file: Optional[Path] = None) -> None:
    """Atomically save checkpoint (write to tmp then rename)."""
    checkpoint_file = checkpoint_file or CHECKPOINT_FILE
    checkpoint_file.parent.mkdir(parents=True, exist_ok=True)
    tmp = checkpoint_file.with_suffix(".tmp")
    tmp.write_text(json.dumps(data))
    tmp.replace(checkpoint_file)


# ── Row mappers ─────────────────────────────────────────────────────────


def _clean(val: str) -> str:
    """Strip and normalize a cell value."""
    if not val:
        return ""
    val = val.strip()
    if val.lower() in ("n/a", "nan", "none", ""):
        return ""
    return val


def _map_cnpj_row(row: dict) -> Optional[Lead]:
    """Map a data_collector CNPJ CSV row to a Yupcha Lead."""
    company = _clean(row.get("company_name", ""))
    if not company:
        return None

    cnpj = _clean(row.get("cnpj", ""))
    state_abbr = _clean(row.get("state", ""))
    state_full = _BR_STATES.get(state_abbr, state_abbr)

    return Lead(
        company=company,
        website=_clean(row.get("website", "")),
        email=_clean(row.get("email", "")),
        phone=_clean(row.get("phone", "")),
        city=_clean(row.get("city", "")),
        state=state_full,
        specialization=_clean(row.get("industry", "")),
        industry_tags=f"CNPJ:{cnpj}" if cnpj else "",
        notes=f"CNPJ: {cnpj}" if cnpj else "",
        source="br_cnpj",
    )


def _map_github_row(row: dict) -> Optional[Lead]:
    """Map a data_collector GitHub CSV row to a Yupcha Lead."""
    company = _clean(row.get("company_name", ""))
    if not company:
        return None

    return Lead(
        company=company,
        website=_clean(row.get("website", "")),
        email=_clean(row.get("email", "")),
        phone=_clean(row.get("phone", "")),
        city=_clean(row.get("city", "")),
        state=_clean(row.get("state", "")),
        specialization=_clean(row.get("industry", "")),
        description=_clean(row.get("repo_description", "")),
        source="br_github",
    )


# ── Streaming importers ────────────────────────────────────────────────


def _stream_import_cnpj(
    db,
    limit: Optional[int] = None,
    checkpoint: Optional[dict] = None,
    checkpoint_file: Optional[Path] = None,
    workspace_id: str = "",
) -> int:
    """
    Stream-import CNPJ CSV into LeadDB in small batches.
    Skips rows already processed (per checkpoint).
    Returns number of rows processed in this run.
    """
    if not CNPJ_CSV.exists():
        print(f"  ⚠ CNPJ CSV not found: {CNPJ_CSV}")
        return 0

    if checkpoint is None:
        checkpoint = _load_checkpoint(checkpoint_file)

    start_row = checkpoint.get("cnpj_row", 0)
    if start_row > 0:
        print(f"  ↻ Resuming CNPJ import from row {start_row:,}")

    processed = 0
    batch = []
    row_num = 0

    with open(CNPJ_CSV, "r", encoding="utf-8") as f:
        reader = csv.DictReader(f)
        for row in reader:
            row_num += 1

            # Skip already-processed rows
            if row_num <= start_row:
                continue

            lead = _map_cnpj_row(row)
            if lead:
                lead.workspace_id = workspace_id
                batch.append(lead)

            # Flush batch to DB
            if len(batch) >= BATCH_SIZE or (
                limit is not None and processed + len(batch) >= limit
            ):
                # Do not advance the checkpoint past a failed batch. The
                # durable queue will retry from the last committed checkpoint;
                # upsert semantics make replay safe if a provider failed after
                # only part of this batch committed.
                score_leads(batch)
                db.bulk_upsert(batch)

                processed += len(batch)
                batch = []

                # Save checkpoint after every batch
                checkpoint["cnpj_row"] = row_num
                _save_checkpoint(checkpoint, checkpoint_file)

                # Progress log (only every LOG_EVERY rows)
                total_done = start_row + processed
                if total_done % LOG_EVERY < BATCH_SIZE:
                    print(f"  🇧🇷 CNPJ: {total_done:,} rows processed")

            # Respect limit
            if limit and processed >= limit:
                break

    # Flush remaining batch
    if batch:
        score_leads(batch)
        db.bulk_upsert(batch)
        processed += len(batch)

        checkpoint["cnpj_row"] = row_num
        _save_checkpoint(checkpoint, checkpoint_file)

    total = start_row + processed
    print(f"  ✅ CNPJ import: {processed:,} new rows this run ({total:,} total)")
    return processed


def _stream_import_github(
    db,
    checkpoint: Optional[dict] = None,
    checkpoint_file: Optional[Path] = None,
    workspace_id: str = "",
) -> int:
    """Import GitHub leads (small dataset — fits in memory)."""
    if not GITHUB_CSV.exists():
        print(f"  ⚠ GitHub CSV not found: {GITHUB_CSV}")
        return 0

    if checkpoint is None:
        checkpoint = _load_checkpoint(checkpoint_file)

    if checkpoint.get("github_done"):
        print("  ↻ GitHub import already completed (skipping)")
        return 0

    leads = []
    with open(GITHUB_CSV, "r", encoding="utf-8") as f:
        reader = csv.DictReader(f)
        for row in reader:
            lead = _map_github_row(row)
            if lead:
                lead.workspace_id = workspace_id
                leads.append(lead)

    if leads:
        score_leads(leads)
        db.bulk_upsert(leads)

    checkpoint["github_done"] = True
    _save_checkpoint(checkpoint, checkpoint_file)

    print(f"  ✅ GitHub import: {len(leads)} leads")
    return len(leads)


# ── Public API ──────────────────────────────────────────────────────────


def import_all_collector_leads(
    limit: Optional[int] = None,
    *,
    workspace_id: str = "",
    slug: str = "",
) -> int:
    """
    Import all data_collector leads into the Yupcha LeadDB.
    Streams CSV rows, checkpoints progress, and resumes from where it left off.
    Returns the total number of leads upserted in this run.
    """
    if workspace_id and slug:
        from apps.api.services.leadgen.store import get_lead_store

        db = get_lead_store(workspace_id, slug)
        checkpoint_file = checkpoint_path_for(slug)
    else:
        # Backwards-compatible CLI path. Authenticated API/worker callers must
        # always supply a tenant and never enter this branch.
        from apps.api.services.leadgen.db import LeadDB

        db = LeadDB()
        checkpoint_file = CHECKPOINT_FILE
    checkpoint = _load_checkpoint(checkpoint_file)
    total = 0

    try:
        print("\n🇧🇷 Importing CNPJ leads from data_collector...")
        total += _stream_import_cnpj(
            db,
            limit=limit,
            checkpoint=checkpoint,
            checkpoint_file=checkpoint_file,
            workspace_id=workspace_id,
        )

        print("\n🇧🇷 Importing GitHub leads from data_collector...")
        total += _stream_import_github(
            db,
            checkpoint=checkpoint,
            checkpoint_file=checkpoint_file,
            workspace_id=workspace_id,
        )
    except Exception as e:
        print(f"  ✗ Import interrupted: {e}")
        print(f"    Checkpoint saved — will resume on next run")
    finally:
        db.close()

    print(f"\n🇧🇷 Import run complete: {total:,} leads processed this run")
    return total


def reset_checkpoint(checkpoint_file: Optional[Path] = None) -> None:
    """Reset the checkpoint file to start imports from scratch."""
    checkpoint_file = checkpoint_file or CHECKPOINT_FILE
    if checkpoint_file.exists():
        checkpoint_file.unlink()
    print("  ✓ Checkpoint reset — next import starts from row 0")


async def handle_data_collector_import(queue_job_id: int, payload: dict) -> None:
    """Durable, tenant-scoped queue handler for the Brazil CSV import."""
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.leadgen.progress import progress
    from apps.api.services.leadgen.store import get_lead_store

    module = payload.get("module") or "all"
    workspace_id = payload.get("workspace_id")
    slug = payload.get("slug")
    import_job_id = payload.get("job_id")
    if module not in ("cnpj", "github", "all"):
        raise ValueError("module must be cnpj, github, or all")
    if not workspace_id or not slug or not import_job_id:
        raise ValueError("data collector payload requires workspace_id, slug, and job_id")

    checkpoint_file = checkpoint_path_for(slug)
    if payload.get("reset"):
        reset_checkpoint(checkpoint_file)
    checkpoint = _load_checkpoint(checkpoint_file)
    limit = payload.get("limit")
    progress.bind_job(import_job_id, workspace_id)
    progress.emit(
        "data_import_started",
        {"job_id": import_job_id, "module": module, "workspace_id": workspace_id},
    )

    total = 0
    with workspace_scope(workspace_id):
        db = get_lead_store(workspace_id, slug)
        try:
            if module in ("cnpj", "all"):
                total += _stream_import_cnpj(
                    db,
                    limit=limit,
                    checkpoint=checkpoint,
                    checkpoint_file=checkpoint_file,
                    workspace_id=workspace_id,
                )
            if module in ("github", "all"):
                total += _stream_import_github(
                    db,
                    checkpoint=checkpoint,
                    checkpoint_file=checkpoint_file,
                    workspace_id=workspace_id,
                )
        finally:
            db.close()

    progress.emit(
        "data_import_completed",
        {
            "job_id": import_job_id,
            "module": module,
            "processed": total,
            "workspace_id": workspace_id,
        },
    )
