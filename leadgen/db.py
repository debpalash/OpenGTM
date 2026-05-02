"""
SQLite database manager for the lead pipeline.

Provides CRUD operations, full-text search, deduplication, and stats queries.
All data is stored in a single SQLite file at config.DB_PATH.
"""

import sqlite3
from datetime import datetime
from pathlib import Path
from typing import List, Optional, Dict, Any

from leadgen.models import Lead


class LeadDB:
    """SQLite-backed lead database with FTS and upsert support."""

    def __init__(self, db_path: Optional[str] = None):
        if db_path is None:
            from config import DB_PATH
            db_path = str(DB_PATH)

        self.db_path = db_path
        Path(db_path).parent.mkdir(parents=True, exist_ok=True)
        self.conn = sqlite3.connect(db_path)
        self.conn.row_factory = sqlite3.Row
        self.conn.execute("PRAGMA journal_mode=WAL")
        self.conn.execute("PRAGMA foreign_keys=ON")
        self.create_tables()

    # ── Schema ─────────────────────────────────────────────────────────

    def create_tables(self):
        """Create leads table and FTS index if they don't exist."""
        self.conn.executescript("""
            CREATE TABLE IF NOT EXISTS leads (
                id              INTEGER PRIMARY KEY AUTOINCREMENT,
                company         TEXT NOT NULL,
                website         TEXT DEFAULT '',
                email           TEXT DEFAULT '',
                phone           TEXT DEFAULT '',
                contact_person  TEXT DEFAULT '',
                contact_title   TEXT DEFAULT '',
                city            TEXT DEFAULT '',
                state           TEXT DEFAULT '',
                specialization  TEXT DEFAULT '',
                company_size    TEXT DEFAULT '',
                description     TEXT DEFAULT '',
                linkedin_url    TEXT DEFAULT '',
                twitter_url     TEXT DEFAULT '',
                source          TEXT DEFAULT '',
                score           INTEGER DEFAULT 0,
                score_tier      TEXT DEFAULT 'unqualified',
                status          TEXT DEFAULT 'new',
                yupcha_value_prop TEXT DEFAULT '',
                company_need    TEXT DEFAULT '',
                notes           TEXT DEFAULT '',
                created_at      TEXT DEFAULT '',
                updated_at      TEXT DEFAULT '',
                last_enriched_at TEXT DEFAULT ''
            );

            CREATE UNIQUE INDEX IF NOT EXISTS idx_leads_company_city
                ON leads(company, city);

            CREATE INDEX IF NOT EXISTS idx_leads_score ON leads(score DESC);
            CREATE INDEX IF NOT EXISTS idx_leads_status ON leads(status);
            CREATE INDEX IF NOT EXISTS idx_leads_city ON leads(city);
            CREATE INDEX IF NOT EXISTS idx_leads_source ON leads(source);

            -- Full-text search virtual table
            CREATE VIRTUAL TABLE IF NOT EXISTS leads_fts USING fts5(
                company, city, specialization, notes, description,
                content=leads,
                content_rowid=id
            );

            -- Triggers to keep FTS in sync
            CREATE TRIGGER IF NOT EXISTS leads_ai AFTER INSERT ON leads BEGIN
                INSERT INTO leads_fts(rowid, company, city, specialization, notes, description)
                VALUES (new.id, new.company, new.city, new.specialization, new.notes, new.description);
            END;

            CREATE TRIGGER IF NOT EXISTS leads_ad AFTER DELETE ON leads BEGIN
                INSERT INTO leads_fts(leads_fts, rowid, company, city, specialization, notes, description)
                VALUES ('delete', old.id, old.company, old.city, old.specialization, old.notes, old.description);
            END;

            CREATE TRIGGER IF NOT EXISTS leads_au AFTER UPDATE ON leads BEGIN
                INSERT INTO leads_fts(leads_fts, rowid, company, city, specialization, notes, description)
                VALUES ('delete', old.id, old.company, old.city, old.specialization, old.notes, old.description);
                INSERT INTO leads_fts(rowid, company, city, specialization, notes, description)
                VALUES (new.id, new.company, new.city, new.specialization, new.notes, new.description);
            END;

            -- Jobs queue for async collection queries
            CREATE TABLE IF NOT EXISTS jobs (
                id          TEXT PRIMARY KEY,
                query       TEXT NOT NULL,
                status      TEXT DEFAULT 'pending',
                tier        INTEGER DEFAULT 1,
                attempts    INTEGER DEFAULT 0,
                max_attempts INTEGER DEFAULT 3,
                leads_found INTEGER DEFAULT 0,
                proxy_used  TEXT DEFAULT '',
                error       TEXT DEFAULT '',
                created_at  TEXT DEFAULT '',
                started_at  TEXT DEFAULT '',
                completed_at TEXT DEFAULT ''
            );

            CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);

            -- Activity log for pipeline tracking
            CREATE TABLE IF NOT EXISTS activity_log (
                id          INTEGER PRIMARY KEY AUTOINCREMENT,
                lead_id     INTEGER,
                action      TEXT NOT NULL,
                details     TEXT DEFAULT '',
                created_at  TEXT DEFAULT '',
                FOREIGN KEY (lead_id) REFERENCES leads(id)
            );
        """)
        self.conn.commit()

    # ── CRUD ───────────────────────────────────────────────────────────

    def upsert_lead(self, lead: Lead) -> int:
        """Insert or update a lead. Deduplicates by (company, city)."""
        lead.updated_at = datetime.utcnow().isoformat()

        existing = self.conn.execute(
            "SELECT id FROM leads WHERE company = ? AND city = ?",
            (lead.company, lead.city)
        ).fetchone()

        if existing:
            lead.id = existing["id"]
            fields = {k: v for k, v in lead.to_dict().items()
                      if k != "id" and k != "created_at"}
            set_clause = ", ".join(f"{k} = ?" for k in fields)
            values = list(fields.values()) + [lead.id]
            self.conn.execute(
                f"UPDATE leads SET {set_clause} WHERE id = ?", values
            )
        else:
            d = lead.to_dict()
            d.pop("id", None)
            cols = ", ".join(d.keys())
            placeholders = ", ".join("?" for _ in d)
            cursor = self.conn.execute(
                f"INSERT INTO leads ({cols}) VALUES ({placeholders})",
                list(d.values())
            )
            lead.id = cursor.lastrowid

        self.conn.commit()
        return lead.id

    def bulk_upsert(self, leads: List[Lead]) -> int:
        """Upsert multiple leads. Returns count of upserted records."""
        count = 0
        for lead in leads:
            self.upsert_lead(lead)
            count += 1
        return count

    def get_lead(self, lead_id: int) -> Optional[Lead]:
        """Get a single lead by ID."""
        row = self.conn.execute(
            "SELECT * FROM leads WHERE id = ?", (lead_id,)
        ).fetchone()
        if row:
            return Lead.from_dict(dict(row))
        return None

    def get_leads(
        self,
        status: Optional[str] = None,
        city: Optional[str] = None,
        source: Optional[str] = None,
        score_min: Optional[int] = None,
        score_max: Optional[int] = None,
        score_tier: Optional[str] = None,
        search: Optional[str] = None,
        limit: int = 500,
        offset: int = 0,
        order_by: str = "score DESC",
    ) -> List[Lead]:
        """Query leads with filters."""
        conditions = []
        params = []

        if status:
            conditions.append("l.status = ?")
            params.append(status)
        if city:
            conditions.append("l.city = ?")
            params.append(city)
        if source:
            conditions.append("l.source = ?")
            params.append(source)
        if score_min is not None:
            conditions.append("l.score >= ?")
            params.append(score_min)
        if score_max is not None:
            conditions.append("l.score <= ?")
            params.append(score_max)
        if score_tier:
            conditions.append("l.score_tier = ?")
            params.append(score_tier)

        if search:
            # Use FTS for text search
            conditions.append("l.id IN (SELECT rowid FROM leads_fts WHERE leads_fts MATCH ?)")
            params.append(search)

        where = " AND ".join(conditions) if conditions else "1=1"

        # Validate order_by to prevent SQL injection
        allowed_orders = {
            "score DESC", "score ASC", "company ASC", "company DESC",
            "created_at DESC", "created_at ASC", "updated_at DESC",
            "city ASC", "city DESC", "status ASC",
        }
        if order_by not in allowed_orders:
            order_by = "score DESC"

        rows = self.conn.execute(
            f"SELECT * FROM leads l WHERE {where} ORDER BY {order_by} LIMIT ? OFFSET ?",
            params + [limit, offset]
        ).fetchall()

        return [Lead.from_dict(dict(r)) for r in rows]

    def count_leads(self, **filters) -> int:
        """Count leads matching filters."""
        conditions = []
        params = []
        for key, val in filters.items():
            if val is not None:
                conditions.append(f"{key} = ?")
                params.append(val)
        where = " AND ".join(conditions) if conditions else "1=1"
        row = self.conn.execute(
            f"SELECT COUNT(*) as cnt FROM leads WHERE {where}", params
        ).fetchone()
        return row["cnt"]

    def update_status(self, lead_id: int, status: str, note: str = "") -> None:
        """Update lead status and log the activity."""
        now = datetime.utcnow().isoformat()
        self.conn.execute(
            "UPDATE leads SET status = ?, updated_at = ? WHERE id = ?",
            (status, now, lead_id)
        )
        self.conn.execute(
            "INSERT INTO activity_log (lead_id, action, details, created_at) VALUES (?, ?, ?, ?)",
            (lead_id, f"status_change:{status}", note, now)
        )
        self.conn.commit()

    def update_lead_fields(self, lead_id: int, fields: Dict[str, Any]) -> None:
        """Update specific fields on a lead."""
        fields["updated_at"] = datetime.utcnow().isoformat()
        set_clause = ", ".join(f"{k} = ?" for k in fields)
        self.conn.execute(
            f"UPDATE leads SET {set_clause} WHERE id = ?",
            list(fields.values()) + [lead_id]
        )
        self.conn.commit()

    def delete_lead(self, lead_id: int) -> None:
        """Delete a lead by ID."""
        self.conn.execute("DELETE FROM leads WHERE id = ?", (lead_id,))
        self.conn.commit()

    # ── Stats ──────────────────────────────────────────────────────────

    def get_stats(self) -> Dict[str, Any]:
        """Return dashboard summary statistics."""
        total = self.count_leads()

        status_counts = {}
        for row in self.conn.execute(
            "SELECT status, COUNT(*) as cnt FROM leads GROUP BY status"
        ).fetchall():
            status_counts[row["status"]] = row["cnt"]

        tier_counts = {}
        for row in self.conn.execute(
            "SELECT score_tier, COUNT(*) as cnt FROM leads GROUP BY score_tier"
        ).fetchall():
            tier_counts[row["score_tier"]] = row["cnt"]

        city_counts = {}
        for row in self.conn.execute(
            "SELECT city, COUNT(*) as cnt FROM leads GROUP BY city ORDER BY cnt DESC LIMIT 15"
        ).fetchall():
            city_counts[row["city"]] = row["cnt"]

        source_counts = {}
        for row in self.conn.execute(
            "SELECT source, COUNT(*) as cnt FROM leads GROUP BY source ORDER BY cnt DESC"
        ).fetchall():
            source_counts[row["source"]] = row["cnt"]

        enrichment = self.conn.execute("""
            SELECT
                COUNT(*) as total,
                SUM(CASE WHEN email != '' AND email != 'N/A' THEN 1 ELSE 0 END) as with_email,
                SUM(CASE WHEN phone != '' AND phone != 'N/A' THEN 1 ELSE 0 END) as with_phone,
                SUM(CASE WHEN website != '' AND website != 'N/A' THEN 1 ELSE 0 END) as with_website,
                SUM(CASE WHEN linkedin_url != '' AND linkedin_url != 'N/A' THEN 1 ELSE 0 END) as with_linkedin,
                SUM(CASE WHEN contact_person != '' AND contact_person != 'N/A' THEN 1 ELSE 0 END) as with_contact,
                ROUND(AVG(score), 1) as avg_score
            FROM leads
        """).fetchone()

        return {
            "total": total,
            "by_status": status_counts,
            "by_tier": tier_counts,
            "by_city": dict(city_counts),
            "by_source": source_counts,
            "enrichment": {
                "total": enrichment["total"],
                "with_email": enrichment["with_email"],
                "with_phone": enrichment["with_phone"],
                "with_website": enrichment["with_website"],
                "with_linkedin": enrichment["with_linkedin"],
                "with_contact": enrichment["with_contact"],
                "avg_score": enrichment["avg_score"] or 0,
            },
        }

    def get_cities(self) -> List[str]:
        """Get all distinct cities."""
        rows = self.conn.execute(
            "SELECT DISTINCT city FROM leads WHERE city != '' ORDER BY city"
        ).fetchall()
        return [r["city"] for r in rows]

    def get_sources(self) -> List[str]:
        """Get all distinct sources."""
        rows = self.conn.execute(
            "SELECT DISTINCT source FROM leads WHERE source != '' ORDER BY source"
        ).fetchall()
        return [r["source"] for r in rows]

    # ── Jobs ───────────────────────────────────────────────────────────

    def create_job(self, job_id: str, query: str) -> str:
        """Create a new collection job."""
        now = datetime.utcnow().isoformat()
        self.conn.execute(
            "INSERT OR IGNORE INTO jobs (id, query, created_at) VALUES (?, ?, ?)",
            (job_id, query, now)
        )
        self.conn.commit()
        return job_id

    def claim_job(self) -> Optional[Dict[str, Any]]:
        """Claim the next pending job for processing."""
        row = self.conn.execute(
            "SELECT * FROM jobs WHERE status = 'pending' ORDER BY created_at LIMIT 1"
        ).fetchone()
        if not row:
            return None
        now = datetime.utcnow().isoformat()
        self.conn.execute(
            "UPDATE jobs SET status = 'running', started_at = ?, attempts = attempts + 1 WHERE id = ?",
            (now, row["id"])
        )
        self.conn.commit()
        return dict(row)

    def complete_job(self, job_id: str, leads_found: int = 0):
        """Mark a job as completed."""
        now = datetime.utcnow().isoformat()
        self.conn.execute(
            "UPDATE jobs SET status = 'done', completed_at = ?, leads_found = ? WHERE id = ?",
            (now, leads_found, job_id)
        )
        self.conn.commit()

    def fail_job(self, job_id: str, error: str):
        """Mark a job as failed. Re-queues if under max_attempts."""
        row = self.conn.execute("SELECT attempts, max_attempts FROM jobs WHERE id = ?", (job_id,)).fetchone()
        if row and row["attempts"] < row["max_attempts"]:
            self.conn.execute(
                "UPDATE jobs SET status = 'pending', error = ? WHERE id = ?",
                (error, job_id)
            )
        else:
            self.conn.execute(
                "UPDATE jobs SET status = 'failed', error = ?, completed_at = ? WHERE id = ?",
                (error, datetime.utcnow().isoformat(), job_id)
            )
        self.conn.commit()

    def get_jobs(self, status: Optional[str] = None, limit: int = 50) -> List[Dict[str, Any]]:
        """List jobs, optionally filtered by status."""
        if status:
            rows = self.conn.execute(
                "SELECT * FROM jobs WHERE status = ? ORDER BY created_at DESC LIMIT ?",
                (status, limit)
            ).fetchall()
        else:
            rows = self.conn.execute(
                "SELECT * FROM jobs ORDER BY created_at DESC LIMIT ?", (limit,)
            ).fetchall()
        return [dict(r) for r in rows]

    # ── Cleanup ────────────────────────────────────────────────────────

    def close(self):
        """Close the database connection."""
        self.conn.close()

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.close()
