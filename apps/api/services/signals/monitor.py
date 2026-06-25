"""
Signal Monitor — Detect buying signals from leads.

Runs periodically to check for hiring activity, website changes,
funding news, and technology adoption signals.

CUTOVER NOTE (self-host on_signal): ``run_signal_scan`` now writes detected
signals through the shared, workspace-scoped ORM signal store
(:mod:`apps.api.services.signals.store`) on BOTH backends, so ``on_signal``
automations fire on SQLite/self-host too (not just Postgres). The legacy
per-file ``data/signals.db`` store is NO LONGER written or read by the feed —
its rows are intentionally NOT backfilled into the ORM ``signals`` table
(feed signals are ephemeral). The legacy file CRUD below is retained only as a
back-compat shim for any out-of-band reader; it is off the main path.
"""

import hashlib
import json
import time
import logging
import sqlite3
import uuid
from datetime import datetime, timezone
from typing import List, Dict, Optional, Any
from dataclasses import dataclass, field, asdict
from pathlib import Path

from apps.api.core.config import settings

logger = logging.getLogger("signals.monitor")


# ── Signal Types ──────────────────────────────────────────────

SIGNAL_TYPES = {
    "hiring": {"label": "Hiring", "icon": "users", "weight": 8},
    "funding": {"label": "Funding", "icon": "trending-up", "weight": 10},
    "tech_change": {"label": "Tech Change", "icon": "code", "weight": 6},
    "website_change": {"label": "Website Update", "icon": "globe", "weight": 4},
    "news": {"label": "News Mention", "icon": "newspaper", "weight": 5},
    "growth": {"label": "Growth Signal", "icon": "bar-chart-3", "weight": 7},
    "social_activity": {"label": "Social Activity", "icon": "message-square", "weight": 3},
}


@dataclass
class Signal:
    id: str = field(default_factory=lambda: str(uuid.uuid4()))
    workspace_id: str = ""
    lead_id: int = 0
    company: str = ""
    signal_type: str = ""
    title: str = ""
    description: str = ""
    source: str = ""
    source_url: str = ""
    weight: int = 5
    created_at: float = field(default_factory=time.time)
    read: bool = False


def _get_db():
    """Get a connection to the signals DB."""
    project_root = Path(__file__).resolve().parents[4]
    db_path = project_root / "data" / "signals.db"
    db_path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(db_path))
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")

    conn.executescript("""
        CREATE TABLE IF NOT EXISTS signals (
            id TEXT PRIMARY KEY,
            workspace_id TEXT DEFAULT '',
            lead_id INTEGER DEFAULT 0,
            company TEXT DEFAULT '',
            signal_type TEXT DEFAULT '',
            title TEXT DEFAULT '',
            description TEXT DEFAULT '',
            source TEXT DEFAULT '',
            source_url TEXT DEFAULT '',
            weight INTEGER DEFAULT 5,
            created_at REAL,
            read INTEGER DEFAULT 0
        );

        CREATE INDEX IF NOT EXISTS idx_signals_type ON signals(signal_type);
        CREATE INDEX IF NOT EXISTS idx_signals_lead ON signals(lead_id);
        CREATE INDEX IF NOT EXISTS idx_signals_created ON signals(created_at DESC);
        CREATE INDEX IF NOT EXISTS idx_signals_ws ON signals(workspace_id);
    """)
    # Safe migration: add workspace_id to a pre-existing signals.db file.
    cols = {r[1] for r in conn.execute("PRAGMA table_info(signals)").fetchall()}
    if "workspace_id" not in cols:
        conn.execute("ALTER TABLE signals ADD COLUMN workspace_id TEXT DEFAULT ''")
    conn.commit()
    return conn


# ── Signal CRUD ───────────────────────────────────────────────

def add_signal(signal: Signal) -> str:
    """Store a new signal (legacy SQLite file path).

    ``signal.workspace_id`` is persisted so the file path is also tenant-scoped;
    the Postgres path uses :meth:`PgLeadStore.add_signal` instead.
    """
    conn = _get_db()
    conn.execute(
        """INSERT OR IGNORE INTO signals (id, workspace_id, lead_id, company, signal_type, title,
           description, source, source_url, weight, created_at, read)
           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
        (signal.id, signal.workspace_id, signal.lead_id, signal.company, signal.signal_type,
         signal.title, signal.description, signal.source, signal.source_url,
         signal.weight, signal.created_at, 0),
    )
    conn.commit()
    conn.close()
    return signal.id


def get_signals(
    signal_type: Optional[str] = None,
    lead_id: Optional[int] = None,
    limit: int = 50,
    offset: int = 0,
    workspace_id: Optional[str] = None,
) -> List[dict]:
    """Get recent signals, optionally filtered. Scoped to ``workspace_id`` when
    given (always pass it from a request so the file path is tenant-isolated)."""
    conn = _get_db()
    query = "SELECT * FROM signals WHERE 1=1"
    params: list = []

    if workspace_id is not None:
        query += " AND workspace_id = ?"
        params.append(workspace_id)
    if signal_type:
        query += " AND signal_type = ?"
        params.append(signal_type)
    if lead_id:
        query += " AND lead_id = ?"
        params.append(lead_id)

    query += " ORDER BY created_at DESC LIMIT ? OFFSET ?"
    params.extend([limit, offset])

    rows = conn.execute(query, params).fetchall()
    conn.close()
    return [dict(r) for r in rows]


def get_signal_counts(workspace_id: Optional[str] = None) -> Dict[str, int]:
    """Get signal counts by type, scoped to ``workspace_id`` when given."""
    conn = _get_db()
    ws = "" if workspace_id is None else " WHERE workspace_id = ?"
    wp: list = [] if workspace_id is None else [workspace_id]
    rows = conn.execute(
        f"SELECT signal_type, COUNT(*) as c FROM signals{ws} GROUP BY signal_type", wp
    ).fetchall()
    total = conn.execute(f"SELECT COUNT(*) as c FROM signals{ws}", wp).fetchone()
    unread_clause = (ws + " AND read = 0") if ws else " WHERE read = 0"
    unread = conn.execute(
        f"SELECT COUNT(*) as c FROM signals{unread_clause}", wp
    ).fetchone()
    conn.close()
    result = {r["signal_type"]: r["c"] for r in rows}
    result["total"] = total["c"] if total else 0
    result["unread"] = unread["c"] if unread else 0
    return result


def mark_read(signal_ids: List[str], workspace_id: Optional[str] = None):
    """Mark signals as read, scoped to ``workspace_id`` when given."""
    conn = _get_db()
    for sid in signal_ids:
        if workspace_id is None:
            conn.execute("UPDATE signals SET read = 1 WHERE id = ?", (sid,))
        else:
            conn.execute(
                "UPDATE signals SET read = 1 WHERE id = ? AND workspace_id = ?",
                (sid, workspace_id),
            )
    conn.commit()
    conn.close()


# ── Deterministic signal id (dedup → exactly-once on_signal) ──────────────────
#
# Root cause of duplicate fires on the legacy path was ``uuid4`` per scan → a new
# PK every run → emit on every scan. We mirror the intent-poller's deterministic
# id (services/poller/keys.py): the natural key is a COARSE per-state value, not
# the raw job count, so an unchanged company does not re-fire on every scan.
# ``add_signal`` is idempotent on this id (insert-only-when-absent) and emits
# on_signal only on the inserted path → a re-scan of the same underlying event
# produces no new row and no second fire.

SCAN_KEY_SCHEMA_VERSION = 1


def _hiring_band(total_jobs: int) -> str:
    """Coarse open-positions band (NOT raw counts) so small count jitter between
    scans does not churn the dedup key / re-fire on_signal."""
    if total_jobs >= 51:
        return "surge"
    if total_jobs >= 21:
        return "high"
    if total_jobs >= 6:
        return "moderate"
    return "low"


def _period_bucket(ts: Optional[float] = None) -> str:
    """ISO year-week bucket. Re-scans within the same week dedup to one signal;
    a genuinely fresh week is allowed to re-fire once (a new buying signal)."""
    dt = datetime.fromtimestamp(ts if ts is not None else time.time(), tz=timezone.utc)
    iso = dt.isocalendar()
    return f"{iso[0]}-W{iso[1]:02d}"


def scan_signal_id(workspace_id: str, lead_id: int, signal_type: str, natural_key: str) -> str:
    """Deterministic sha256 id for a scanner-detected signal (mirrors §8.2).

        sha256(f"scan|v{V}|{ws}|{lead_id}|{signal_type}|{natural_key}")
    """
    raw = (
        f"scan|v{SCAN_KEY_SCHEMA_VERSION}|{workspace_id}|{lead_id}"
        f"|{signal_type}|{natural_key}"
    )
    return hashlib.sha256(raw.encode("utf-8")).hexdigest()


# ── Signal Detection ─────────────────────────────────────────

def _extract_hiring_data(result) -> Optional[dict]:
    """Pull the hiring-signals dict off a provider result, tolerating both shapes:

      * the real :class:`EnrichmentResult` — ``fields["hiring_signals"]`` (a JSON
        string produced by ``JobSpySignalProvider.enrich``), and
      * a plain ``.data`` dict (test doubles / legacy callers).

    Returns the dict or ``None`` when no hiring data is present.
    """
    data = getattr(result, "data", None)
    if isinstance(data, dict):
        return data
    fields = getattr(result, "fields", None)
    if isinstance(fields, dict) and fields.get("hiring_signals"):
        raw = fields["hiring_signals"]
        if isinstance(raw, dict):
            return raw
        try:
            parsed = json.loads(raw)
            return parsed if isinstance(parsed, dict) else None
        except (ValueError, TypeError):
            return None
    return None


async def detect_hiring_signals(leads, workspace_id: str = "", *, max_leads: Optional[int] = None) -> List[Signal]:
    """Detect hiring activity using the JobSpy provider.

    Stamps each signal with ``workspace_id`` and a DETERMINISTIC id keyed on a
    coarse hiring band + week bucket (so re-scans of unchanged state dedup).
    """
    signals: List[Signal] = []
    cap = max_leads if max_leads is not None else int(
        getattr(settings, "SIGNAL_SCAN_MAX_LEADS_PER_WORKSPACE", 20)
    )
    try:
        from apps.api.services.leadgen.enrichment.providers.jobspy_signals import JobSpySignalProvider
        provider = JobSpySignalProvider()

        for lead in leads[:cap]:  # Cap per run (per workspace)
            if not lead.company:
                continue
            try:
                result = await provider.enrich(lead)
                jobs_data = _extract_hiring_data(result) if getattr(result, "success", False) else None
                if jobs_data:
                    if jobs_data.get("total_jobs", 0) > 0:
                        total = jobs_data.get("total_jobs", 0)
                        band = _hiring_band(total)
                        natural_key = f"{band}|{_period_bucket()}"
                        signals.append(Signal(
                            id=scan_signal_id(workspace_id, lead.id, "hiring", natural_key),
                            workspace_id=workspace_id,
                            lead_id=lead.id,
                            company=lead.company,
                            signal_type="hiring",
                            title=f"{lead.company} has {total} open positions",
                            description=f"Active hiring detected. Top roles: {', '.join(jobs_data.get('top_titles', [])[:3])}",
                            source="JobSpy",
                            weight=min(10, total),
                        ))
            except Exception as e:
                logger.debug(f"Hiring check failed for {lead.company}: {e}")

    except ImportError:
        logger.warning("JobSpy provider not available for signal detection")

    return signals


async def run_signal_scan() -> Dict[str, int]:
    """Run a full signal scan across all hot/warm leads, PER WORKSPACE.

    This is the main entry point called by the scheduler. Self-host has a single
    'main' workspace → the loop runs once. Multi-tenant SQLite / Postgres → one
    scoped pass per workspace. Each workspace's signals are written through the
    shared ORM store (``get_signal_store``) so ``on_signal`` automations fire on
    BOTH backends. One workspace failing does not abort the rest (AC-7).
    """
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.leadgen.store import get_lead_store
    from apps.api.services.signals.store import get_signal_store
    from apps.api.services.workspace import manager as ws_manager

    hot_cap = int(getattr(settings, "SIGNAL_SCAN_MAX_HOT_LEADS", 50))
    warm_cap = int(getattr(settings, "SIGNAL_SCAN_MAX_WARM_LEADS", 30))
    per_ws_cap = int(getattr(settings, "SIGNAL_SCAN_MAX_LEADS_PER_WORKSPACE", 20))
    global_cap = int(getattr(settings, "SIGNAL_SCAN_GLOBAL_MAX_LEADS", 500))

    try:
        workspaces = ws_manager.list_workspaces()
    except Exception as e:
        logger.warning(f"signal scan: could not list workspaces: {e}")
        return {"scanned": 0, "signals_found": 0, "workspaces": 0}

    total_scanned = 0
    total_signals = 0
    total_boosted = 0
    ws_done = 0
    ws_failed = 0
    global_budget = global_cap if global_cap > 0 else None

    for ws in workspaces:
        if global_budget is not None and global_budget <= 0:
            logger.info("signal scan: global lead cap reached, stopping workspace loop")
            break
        try:
            with workspace_scope(ws.id):
                store = get_lead_store(ws.id, ws.slug)
                try:
                    hot_leads = store.get_leads(score_tier="hot", limit=hot_cap)
                    warm_leads = store.get_leads(score_tier="warm", limit=warm_cap)
                    all_leads = hot_leads + warm_leads
                finally:
                    store.close()

                if not all_leads:
                    ws_done += 1
                    continue

                # Politeness: clamp per-workspace enrich count to the per-ws cap
                # AND the remaining global budget.
                cap = per_ws_cap
                if global_budget is not None:
                    cap = min(cap, global_budget)
                if cap <= 0:
                    break

                signals = await detect_hiring_signals(all_leads, ws.id, max_leads=cap)

                sig_store = get_signal_store(ws.id)
                for s in signals:
                    sig_store.add_signal(s)

                boosted = _apply_signal_boosts(signals, ws.id, ws.slug)

                processed = min(len(all_leads), cap)
                total_scanned += len(all_leads)
                total_signals += len(signals)
                total_boosted += boosted
                if global_budget is not None:
                    global_budget -= processed
                ws_done += 1
        except Exception as e:
            ws_failed += 1
            logger.warning(f"signal scan failed for workspace {ws.id}: {e}")
            continue

    logger.info(
        f"Signal scan complete: {ws_done} workspaces scanned ({ws_failed} failed), "
        f"{total_scanned} leads, {total_signals} signals, {total_boosted} leads boosted"
    )

    return {
        "scanned": total_scanned,
        "signals_found": total_signals,
        "leads_boosted": total_boosted,
        "workspaces": ws_done,
        "workspaces_failed": ws_failed,
        "by_type": {"hiring": total_signals},
    }


def _apply_signal_boosts(signals, workspace_id: str, slug: str) -> int:
    """Bump lead scores for leads with new signals (capped at 100), recompute
    tier — within the given workspace's scoped lead store."""
    if not signals:
        return 0
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.leadgen.store import get_lead_store

    # Aggregate weight per lead (a lead may fire multiple signals)
    by_lead: Dict[int, int] = {}
    for s in signals:
        if s.lead_id:
            by_lead[s.lead_id] = by_lead.get(s.lead_id, 0) + int(s.weight or 0)

    if not by_lead:
        return 0

    boosted = 0
    try:
        with workspace_scope(workspace_id):
            store = get_lead_store(workspace_id, slug)
            try:
                for lead_id, weight in by_lead.items():
                    lead = store.get_lead(lead_id)
                    if not lead:
                        continue
                    cur = lead.score or 0
                    new_score = min(100, cur + weight)
                    if new_score == cur:
                        continue
                    tier = ("hot" if new_score >= 75 else "warm" if new_score >= 50
                            else "cold" if new_score >= 25 else "unqualified")
                    store.update_lead_fields(lead_id, {"score": new_score, "score_tier": tier})
                    boosted += 1
            finally:
                store.close()
    except Exception as e:
        logger.warning(f"signal score boost failed: {e}")
    return boosted
