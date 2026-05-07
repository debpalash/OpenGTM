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
import os
import threading
import time
from pathlib import Path
from typing import Optional

from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.scoring import score_leads

# Path to the data_collector submodule output
_COLLECTOR_ROOT = Path(__file__).resolve().parents[5] / "data_collector" / "data"
CNPJ_CSV = _COLLECTOR_ROOT / "processed" / "cnpj_leads.csv"
GITHUB_CSV = _COLLECTOR_ROOT / "processed" / "github_leads.csv"

# Checkpoint file — persists across crashes
_CHECKPOINT_DIR = Path(__file__).resolve().parent / "data"
_CHECKPOINT_DIR.mkdir(parents=True, exist_ok=True)
CHECKPOINT_FILE = _CHECKPOINT_DIR / "br_import_checkpoint.json"

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


def _load_checkpoint() -> dict:
    """Load checkpoint from disk. Returns dict with row offsets per module."""
    if CHECKPOINT_FILE.exists():
        try:
            return json.loads(CHECKPOINT_FILE.read_text())
        except (json.JSONDecodeError, OSError):
            pass
    return {"cnpj_row": 0, "github_done": False}


def _save_checkpoint(data: dict) -> None:
    """Atomically save checkpoint (write to tmp then rename)."""
    tmp = CHECKPOINT_FILE.with_suffix(".tmp")
    tmp.write_text(json.dumps(data))
    tmp.rename(CHECKPOINT_FILE)


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


def _stream_import_cnpj(db, limit: Optional[int] = None, checkpoint: Optional[dict] = None) -> int:
    """
    Stream-import CNPJ CSV into LeadDB in small batches.
    Skips rows already processed (per checkpoint).
    Returns number of rows processed in this run.
    """
    if not CNPJ_CSV.exists():
        print(f"  ⚠ CNPJ CSV not found: {CNPJ_CSV}")
        return 0

    if checkpoint is None:
        checkpoint = _load_checkpoint()

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
                batch.append(lead)

            # Flush batch to DB
            if len(batch) >= BATCH_SIZE:
                try:
                    score_leads(batch)
                    db.bulk_upsert(batch)
                except Exception as e:
                    print(f"  ⚠ Batch error at row {row_num}: {e}")

                processed += len(batch)
                batch = []

                # Save checkpoint after every batch
                checkpoint["cnpj_row"] = row_num
                _save_checkpoint(checkpoint)

                # Progress log (only every LOG_EVERY rows)
                total_done = start_row + processed
                if total_done % LOG_EVERY < BATCH_SIZE:
                    print(f"  🇧🇷 CNPJ: {total_done:,} rows processed")

            # Respect limit
            if limit and processed >= limit:
                break

    # Flush remaining batch
    if batch:
        try:
            score_leads(batch)
            db.bulk_upsert(batch)
            processed += len(batch)
        except Exception as e:
            print(f"  ⚠ Final batch error: {e}")

        checkpoint["cnpj_row"] = row_num
        _save_checkpoint(checkpoint)

    total = start_row + processed
    print(f"  ✅ CNPJ import: {processed:,} new rows this run ({total:,} total)")
    return processed


def _stream_import_github(db, checkpoint: Optional[dict] = None) -> int:
    """Import GitHub leads (small dataset — fits in memory)."""
    if not GITHUB_CSV.exists():
        print(f"  ⚠ GitHub CSV not found: {GITHUB_CSV}")
        return 0

    if checkpoint is None:
        checkpoint = _load_checkpoint()

    if checkpoint.get("github_done"):
        print("  ↻ GitHub import already completed (skipping)")
        return 0

    leads = []
    with open(GITHUB_CSV, "r", encoding="utf-8") as f:
        reader = csv.DictReader(f)
        for row in reader:
            lead = _map_github_row(row)
            if lead:
                leads.append(lead)

    if leads:
        score_leads(leads)
        db.bulk_upsert(leads)

    checkpoint["github_done"] = True
    _save_checkpoint(checkpoint)

    print(f"  ✅ GitHub import: {len(leads)} leads")
    return len(leads)


# ── Public API ──────────────────────────────────────────────────────────


def import_all_collector_leads(limit: Optional[int] = None) -> int:
    """
    Import all data_collector leads into the Yupcha LeadDB.
    Streams CSV rows, checkpoints progress, and resumes from where it left off.
    Returns the total number of leads upserted in this run.
    """
    from apps.api.services.leadgen.db import LeadDB

    checkpoint = _load_checkpoint()
    db = LeadDB()
    total = 0

    try:
        print("\n🇧🇷 Importing CNPJ leads from data_collector...")
        total += _stream_import_cnpj(db, limit=limit, checkpoint=checkpoint)

        print("\n🇧🇷 Importing GitHub leads from data_collector...")
        total += _stream_import_github(db, checkpoint=checkpoint)
    except Exception as e:
        print(f"  ✗ Import interrupted: {e}")
        print(f"    Checkpoint saved — will resume on next run")
    finally:
        db.close()

    print(f"\n🇧🇷 Import run complete: {total:,} leads processed this run")
    return total


def reset_checkpoint() -> None:
    """Reset the checkpoint file to start imports from scratch."""
    if CHECKPOINT_FILE.exists():
        CHECKPOINT_FILE.unlink()
    print("  ✓ Checkpoint reset — next import starts from row 0")


def run_import_background(module: str = "all", limit: Optional[int] = None) -> str:
    """Run the import in a background thread. Returns a job descriptor."""
    import uuid
    job_id = f"br-import-{str(uuid.uuid4())[:6]}"

    def _run():
        try:
            from apps.api.services.leadgen.db import LeadDB

            if module == "cnpj":
                checkpoint = _load_checkpoint()
                db = LeadDB()
                try:
                    _stream_import_cnpj(db, limit=limit, checkpoint=checkpoint)
                finally:
                    db.close()

            elif module == "github":
                checkpoint = _load_checkpoint()
                db = LeadDB()
                try:
                    _stream_import_github(db, checkpoint=checkpoint)
                finally:
                    db.close()

            else:
                import_all_collector_leads(limit=limit)

            print(f"  ✓ Background import job {job_id} completed")
        except Exception as e:
            print(f"  ✗ Background import job {job_id} failed: {e}")
            print(f"    Checkpoint saved — run again to resume")

    thread = threading.Thread(target=_run, daemon=True)
    thread.start()
    return job_id
