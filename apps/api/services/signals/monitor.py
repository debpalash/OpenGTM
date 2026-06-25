"""
Signal Monitor — Detect buying signals from leads.

Runs periodically to check for hiring activity, website changes,
funding news, and technology adoption signals.
Stores signals in SQLite for feed display.
"""

import json
import time
import logging
import sqlite3
import uuid
from typing import List, Dict, Optional, Any
from dataclasses import dataclass, field, asdict
from pathlib import Path

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


# ── Signal Detection ─────────────────────────────────────────

async def detect_hiring_signals(leads) -> List[Signal]:
    """Detect hiring activity using the JobSpy provider."""
    signals = []
    try:
        from apps.api.services.leadgen.enrichment.providers.jobspy_signals import JobSpySignalProvider
        provider = JobSpySignalProvider()

        for lead in leads[:20]:  # Cap per run
            if not lead.company:
                continue
            try:
                result = await provider.enrich(lead)
                if result.success and result.data:
                    jobs_data = result.data
                    if isinstance(jobs_data, dict) and jobs_data.get("total_jobs", 0) > 0:
                        total = jobs_data.get("total_jobs", 0)
                        signals.append(Signal(
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
    """Run a full signal scan across all hot/warm leads.

    This is the main entry point called by the scheduler.
    """
    from apps.api.services.leadgen.db import LeadDB

    db = LeadDB()
    hot_leads = db.get_leads(score_tier="hot", limit=50)
    warm_leads = db.get_leads(score_tier="warm", limit=30)
    all_leads = hot_leads + warm_leads
    db.close()

    if not all_leads:
        return {"scanned": 0, "signals_found": 0}

    all_signals = []

    # Detect hiring signals
    hiring = await detect_hiring_signals(all_leads)
    all_signals.extend(hiring)

    # Store all detected signals
    for s in all_signals:
        add_signal(s)

    # ── signal → score: boost leads with fresh buying signals ──
    boosted = _apply_signal_boosts(all_signals)

    logger.info(
        f"Signal scan complete: {len(all_leads)} leads scanned, "
        f"{len(all_signals)} signals found, {boosted} leads boosted"
    )

    return {
        "scanned": len(all_leads),
        "signals_found": len(all_signals),
        "leads_boosted": boosted,
        "by_type": {
            "hiring": len([s for s in all_signals if s.signal_type == "hiring"]),
        },
    }


def _apply_signal_boosts(signals) -> int:
    """Bump lead scores for leads with new signals (capped at 100), recompute tier."""
    if not signals:
        return 0
    from apps.api.services.leadgen.db import LeadDB

    # Aggregate weight per lead (a lead may fire multiple signals)
    by_lead: Dict[int, int] = {}
    for s in signals:
        if s.lead_id:
            by_lead[s.lead_id] = by_lead.get(s.lead_id, 0) + int(s.weight or 0)

    if not by_lead:
        return 0

    db = LeadDB()
    boosted = 0
    try:
        for lead_id, weight in by_lead.items():
            row = db.conn.execute(
                "SELECT score FROM leads WHERE id = ?", (lead_id,)
            ).fetchone()
            if not row:
                continue
            cur = row[0] or 0
            new_score = min(100, cur + weight)
            if new_score == cur:
                continue
            tier = ("hot" if new_score >= 75 else "warm" if new_score >= 50
                    else "cold" if new_score >= 25 else "unqualified")
            db.update_lead_fields(lead_id, {"score": new_score, "score_tier": tier})
            boosted += 1
    except Exception as e:
        logger.warning(f"signal score boost failed: {e}")
    finally:
        db.close()
    return boosted
