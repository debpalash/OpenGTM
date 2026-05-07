"""
Email Sequence Engine — Multi-step outreach sequences with state tracking.

Each sequence has steps (emails) with configurable delays.
Per-lead state machine: pending → scheduled → sent → opened → replied → bounced.
"""

import json
import time
import logging
import sqlite3
import uuid
from typing import List, Optional, Dict, Any
from dataclasses import dataclass, field, asdict
from enum import Enum

logger = logging.getLogger("outreach.sequence")


class StepStatus(str, Enum):
    PENDING = "pending"
    SCHEDULED = "scheduled"
    SENT = "sent"
    OPENED = "opened"
    REPLIED = "replied"
    BOUNCED = "bounced"
    SKIPPED = "skipped"
    FAILED = "failed"


@dataclass
class SequenceStep:
    """A single step in a sequence (one email template)."""
    step_number: int
    subject: str
    body_html: str
    delay_hours: int = 0  # Hours after previous step (0 = immediately)

    def to_dict(self) -> dict:
        return asdict(self)


@dataclass
class Sequence:
    """An outreach sequence with multiple steps."""
    id: str = field(default_factory=lambda: str(uuid.uuid4()))
    name: str = ""
    description: str = ""
    steps: List[SequenceStep] = field(default_factory=list)
    status: str = "draft"  # draft, active, paused, completed
    created_at: float = field(default_factory=time.time)
    updated_at: float = field(default_factory=time.time)
    # Stats
    total_leads: int = 0
    total_sent: int = 0
    total_opened: int = 0
    total_replied: int = 0
    total_bounced: int = 0
    daily_limit: int = 50
    send_window_start: int = 9   # 9 AM
    send_window_end: int = 18    # 6 PM


@dataclass
class LeadSequenceState:
    """Tracks a lead's progress through a sequence."""
    lead_id: int
    sequence_id: str
    current_step: int = 0
    status: str = "pending"
    next_send_at: float = 0
    sent_count: int = 0
    last_sent_at: float = 0
    error: str = ""


def _get_db():
    """Get a connection to the outreach DB."""
    from apps.api.core.config import settings
    import os
    db_path = os.path.join(settings.DATA_DIR, "outreach.db")
    conn = sqlite3.connect(db_path)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")

    # Create tables
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS sequences (
            id TEXT PRIMARY KEY,
            name TEXT NOT NULL,
            description TEXT DEFAULT '',
            steps TEXT DEFAULT '[]',
            status TEXT DEFAULT 'draft',
            daily_limit INTEGER DEFAULT 50,
            send_window_start INTEGER DEFAULT 9,
            send_window_end INTEGER DEFAULT 18,
            created_at REAL,
            updated_at REAL
        );

        CREATE TABLE IF NOT EXISTS sequence_leads (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            sequence_id TEXT NOT NULL,
            lead_id INTEGER NOT NULL,
            current_step INTEGER DEFAULT 0,
            status TEXT DEFAULT 'pending',
            next_send_at REAL DEFAULT 0,
            sent_count INTEGER DEFAULT 0,
            last_sent_at REAL DEFAULT 0,
            error TEXT DEFAULT '',
            FOREIGN KEY (sequence_id) REFERENCES sequences(id),
            UNIQUE(sequence_id, lead_id)
        );

        CREATE TABLE IF NOT EXISTS send_log (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            sequence_id TEXT NOT NULL,
            lead_id INTEGER NOT NULL,
            step_number INTEGER NOT NULL,
            to_email TEXT NOT NULL,
            subject TEXT NOT NULL,
            status TEXT DEFAULT 'sent',
            message_id TEXT DEFAULT '',
            sent_at REAL,
            opened_at REAL,
            replied_at REAL,
            error TEXT DEFAULT ''
        );

        CREATE INDEX IF NOT EXISTS idx_seq_leads_status ON sequence_leads(sequence_id, status);
        CREATE INDEX IF NOT EXISTS idx_send_log_seq ON send_log(sequence_id);
    """)
    conn.commit()
    return conn


# ── Sequence CRUD ─────────────────────────────────────────────

def create_sequence(name: str, description: str = "", steps: List[dict] = None) -> Sequence:
    """Create a new outreach sequence."""
    seq = Sequence(
        name=name,
        description=description,
        steps=[SequenceStep(**s) for s in (steps or [])],
    )
    conn = _get_db()
    conn.execute(
        """INSERT INTO sequences (id, name, description, steps, status, daily_limit,
           send_window_start, send_window_end, created_at, updated_at)
           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
        (seq.id, seq.name, seq.description, json.dumps([s.to_dict() for s in seq.steps]),
         seq.status, seq.daily_limit, seq.send_window_start, seq.send_window_end,
         seq.created_at, seq.updated_at),
    )
    conn.commit()
    conn.close()
    logger.info(f"Created sequence: {seq.name} ({len(seq.steps)} steps)")
    return seq


def get_sequence(seq_id: str) -> Optional[Sequence]:
    """Get a sequence by ID."""
    conn = _get_db()
    row = conn.execute("SELECT * FROM sequences WHERE id = ?", (seq_id,)).fetchone()
    conn.close()
    if not row:
        return None
    return _row_to_sequence(row)


def list_sequences() -> List[Sequence]:
    """List all sequences."""
    conn = _get_db()
    rows = conn.execute("SELECT * FROM sequences ORDER BY created_at DESC").fetchall()
    conn.close()
    return [_row_to_sequence(r) for r in rows]


def update_sequence(seq_id: str, updates: Dict[str, Any]) -> Optional[Sequence]:
    """Update a sequence."""
    conn = _get_db()
    allowed = {"name", "description", "steps", "status", "daily_limit",
               "send_window_start", "send_window_end"}
    fields = {k: v for k, v in updates.items() if k in allowed}
    if "steps" in fields and isinstance(fields["steps"], list):
        fields["steps"] = json.dumps(fields["steps"])
    fields["updated_at"] = time.time()

    if not fields:
        return get_sequence(seq_id)

    set_clause = ", ".join(f"{k} = ?" for k in fields)
    values = list(fields.values()) + [seq_id]
    conn.execute(f"UPDATE sequences SET {set_clause} WHERE id = ?", values)
    conn.commit()
    conn.close()
    return get_sequence(seq_id)


def delete_sequence(seq_id: str):
    """Delete a sequence and all its lead states."""
    conn = _get_db()
    conn.execute("DELETE FROM sequence_leads WHERE sequence_id = ?", (seq_id,))
    conn.execute("DELETE FROM send_log WHERE sequence_id = ?", (seq_id,))
    conn.execute("DELETE FROM sequences WHERE id = ?", (seq_id,))
    conn.commit()
    conn.close()


# ── Lead Enrollment ───────────────────────────────────────────

def enroll_leads(seq_id: str, lead_ids: List[int]) -> int:
    """Enroll leads into a sequence. Returns count of newly enrolled."""
    conn = _get_db()
    enrolled = 0
    for lid in lead_ids:
        try:
            conn.execute(
                """INSERT OR IGNORE INTO sequence_leads
                   (sequence_id, lead_id, current_step, status, next_send_at)
                   VALUES (?, ?, 0, 'pending', ?)""",
                (seq_id, lid, time.time()),
            )
            enrolled += 1
        except Exception:
            continue
    conn.commit()
    conn.close()
    logger.info(f"Enrolled {enrolled} leads into sequence {seq_id}")
    return enrolled


def get_sequence_stats(seq_id: str) -> Dict[str, int]:
    """Get aggregate stats for a sequence."""
    conn = _get_db()
    stats = {}
    for status in ["pending", "scheduled", "sent", "opened", "replied", "bounced", "failed", "skipped"]:
        row = conn.execute(
            "SELECT COUNT(*) as c FROM sequence_leads WHERE sequence_id = ? AND status = ?",
            (seq_id, status),
        ).fetchone()
        stats[status] = row["c"] if row else 0

    total_row = conn.execute(
        "SELECT COUNT(*) as c FROM sequence_leads WHERE sequence_id = ?", (seq_id,),
    ).fetchone()
    stats["total"] = total_row["c"] if total_row else 0

    log_row = conn.execute(
        "SELECT COUNT(*) as c FROM send_log WHERE sequence_id = ?", (seq_id,),
    ).fetchone()
    stats["emails_sent"] = log_row["c"] if log_row else 0
    conn.close()
    return stats


def get_pending_sends(seq_id: str, limit: int = 50) -> List[dict]:
    """Get leads ready to receive their next email."""
    conn = _get_db()
    now = time.time()
    rows = conn.execute(
        """SELECT sl.*, s.steps FROM sequence_leads sl
           JOIN sequences s ON s.id = sl.sequence_id
           WHERE sl.sequence_id = ? AND sl.status IN ('pending', 'scheduled')
           AND sl.next_send_at <= ?
           ORDER BY sl.next_send_at ASC
           LIMIT ?""",
        (seq_id, now, limit),
    ).fetchall()
    conn.close()
    return [dict(r) for r in rows]


def record_send(seq_id: str, lead_id: int, step_number: int,
                to_email: str, subject: str, message_id: str = "",
                success: bool = True, error: str = ""):
    """Record an email send in the log and update lead state."""
    conn = _get_db()
    now = time.time()

    # Insert send log
    conn.execute(
        """INSERT INTO send_log (sequence_id, lead_id, step_number, to_email, subject,
           status, message_id, sent_at, error)
           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)""",
        (seq_id, lead_id, step_number, to_email, subject,
         "sent" if success else "failed", message_id, now, error),
    )

    if success:
        # Get sequence to find next step delay
        seq_row = conn.execute("SELECT steps FROM sequences WHERE id = ?", (seq_id,)).fetchone()
        steps = json.loads(seq_row["steps"]) if seq_row else []
        next_step = step_number + 1

        if next_step < len(steps):
            next_delay = steps[next_step].get("delay_hours", 24) * 3600
            conn.execute(
                """UPDATE sequence_leads SET current_step = ?, status = 'scheduled',
                   sent_count = sent_count + 1, last_sent_at = ?,
                   next_send_at = ?
                   WHERE sequence_id = ? AND lead_id = ?""",
                (next_step, now, now + next_delay, seq_id, lead_id),
            )
        else:
            # Sequence complete for this lead
            conn.execute(
                """UPDATE sequence_leads SET current_step = ?, status = 'sent',
                   sent_count = sent_count + 1, last_sent_at = ?
                   WHERE sequence_id = ? AND lead_id = ?""",
                (step_number, now, seq_id, lead_id),
            )
    else:
        conn.execute(
            """UPDATE sequence_leads SET status = 'failed', error = ?
               WHERE sequence_id = ? AND lead_id = ?""",
            (error, seq_id, lead_id),
        )

    conn.commit()
    conn.close()


# ── Execute Sequence Step ─────────────────────────────────────

async def execute_pending_sends(seq_id: str) -> Dict[str, int]:
    """Process all pending sends for a sequence. Returns send results."""
    from apps.api.services.outreach.sender import send_email, render_template, build_lead_variables, get_smtp_config
    from apps.api.services.leadgen.db import LeadDB

    seq = get_sequence(seq_id)
    if not seq or seq.status != "active":
        return {"error": 1, "sent": 0}

    pending = get_pending_sends(seq_id, limit=seq.daily_limit)
    if not pending:
        return {"sent": 0, "pending": 0}

    db = LeadDB()
    config = get_smtp_config()
    results = {"sent": 0, "failed": 0, "skipped": 0}

    for item in pending:
        lead = db.get_lead(item["lead_id"])
        if not lead or not lead.email:
            results["skipped"] += 1
            continue

        steps = json.loads(item["steps"]) if isinstance(item["steps"], str) else item["steps"]
        step_idx = item["current_step"]
        if step_idx >= len(steps):
            results["skipped"] += 1
            continue

        step = steps[step_idx]
        variables = build_lead_variables(lead)

        subject = render_template(step["subject"], variables)
        body = render_template(step["body_html"], variables)

        result = await send_email(
            to_email=lead.email,
            subject=subject,
            body_html=body,
            config=config,
        )

        record_send(
            seq_id=seq_id,
            lead_id=lead.id,
            step_number=step_idx,
            to_email=lead.email,
            subject=subject,
            message_id=result.message_id,
            success=result.success,
            error=result.error,
        )

        if result.success:
            results["sent"] += 1
        else:
            results["failed"] += 1

        # Small delay between sends to avoid being flagged
        import asyncio
        await asyncio.sleep(2)

    db.close()
    return results


# ── Helpers ───────────────────────────────────────────────────

def _row_to_sequence(row) -> Sequence:
    """Convert a DB row to a Sequence object."""
    steps_raw = json.loads(row["steps"]) if row["steps"] else []
    steps = [SequenceStep(**s) for s in steps_raw]

    conn = _get_db()
    stats = {}
    for status in ["pending", "sent", "opened", "replied", "bounced"]:
        r = conn.execute(
            "SELECT COUNT(*) as c FROM sequence_leads WHERE sequence_id = ? AND status = ?",
            (row["id"], status),
        ).fetchone()
        stats[status] = r["c"] if r else 0
    total = conn.execute(
        "SELECT COUNT(*) as c FROM sequence_leads WHERE sequence_id = ?", (row["id"],),
    ).fetchone()
    conn.close()

    return Sequence(
        id=row["id"],
        name=row["name"],
        description=row["description"] or "",
        steps=steps,
        status=row["status"],
        daily_limit=row["daily_limit"],
        send_window_start=row["send_window_start"],
        send_window_end=row["send_window_end"],
        created_at=row["created_at"],
        updated_at=row["updated_at"],
        total_leads=total["c"] if total else 0,
        total_sent=stats.get("sent", 0),
        total_opened=stats.get("opened", 0),
        total_replied=stats.get("replied", 0),
        total_bounced=stats.get("bounced", 0),
    )
