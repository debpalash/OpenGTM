"""
SQLite database manager for the lead pipeline.

Provides CRUD operations, full-text search, deduplication, and stats queries.
All data is stored in a single SQLite file at config.DB_PATH.
"""

import sqlite3
import threading
from datetime import datetime, timezone
from pathlib import Path
from typing import List, Optional, Dict, Any


def _utcnow() -> datetime:
    """Return current UTC time (non-deprecated alternative to datetime.utcnow())."""
    return datetime.now(timezone.utc)

from apps.api.services.leadgen.models import Lead

# Thread-local singleton so each thread reuses one connection
_thread_local = threading.local()


def get_db(db_path: Optional[str] = None) -> "LeadDB":
    """Get a thread-local LeadDB singleton.
    
    Reuses the existing connection for the current thread instead of
    opening a new one on every call. Safe for SQLite in WAL mode.
    """
    if db_path is None:
        from apps.api.services.leadgen.config import DB_PATH
        db_path = str(DB_PATH)
    
    existing = getattr(_thread_local, "lead_db", None)
    if existing is not None and existing.db_path == db_path:
        return existing
    
    instance = LeadDB(db_path)
    _thread_local.lead_db = instance
    return instance


class LeadDB:
    """SQLite-backed lead database with FTS and upsert support."""

    def __init__(self, db_path: Optional[str] = None):
        if db_path is None:
            from apps.api.services.leadgen.config import DB_PATH
            db_path = str(DB_PATH)

        self.db_path = db_path
        Path(db_path).parent.mkdir(parents=True, exist_ok=True)
        self.conn = sqlite3.connect(db_path)
        self.conn.row_factory = sqlite3.Row
        self.conn.execute("PRAGMA journal_mode=WAL")
        # Wait (up to 30s) for a lock instead of erroring immediately, and skip
        # the per-write fsync — so a running collection job doesn't starve
        # concurrent API reads/writes of the leads DB.
        self.conn.execute("PRAGMA busy_timeout=30000")
        self.conn.execute("PRAGMA synchronous=NORMAL")
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
                source_url      TEXT DEFAULT '',
                collection_job_id TEXT DEFAULT '',
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

            -- Dedup is per tenant: (workspace_id, company, city). Two
            -- workspaces may each own (Acme, NYC). (workspace_id is added by
            -- _migrate() on the line below before this index is needed on a
            -- legacy file; on a fresh file the column is created by _migrate
            -- right after create_tables, so we (re)create the index in _migrate.)
            CREATE INDEX IF NOT EXISTS idx_leads_company_city
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
                intent      TEXT DEFAULT 'market_search',
                intent_details TEXT DEFAULT '{}',
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

            -- Workspaces for organizing collection campaigns
            CREATE TABLE IF NOT EXISTS workspaces (
                id          TEXT PRIMARY KEY,
                name        TEXT NOT NULL,
                description TEXT DEFAULT '',
                created_at  TEXT DEFAULT '',
                updated_at  TEXT DEFAULT ''
            );

            -- Pipeline stages per job
            CREATE TABLE IF NOT EXISTS job_stages (
                id           INTEGER PRIMARY KEY AUTOINCREMENT,
                job_id       TEXT NOT NULL,
                stage        TEXT NOT NULL,
                status       TEXT DEFAULT 'running',
                input_count  INTEGER DEFAULT 0,
                output_count INTEGER DEFAULT 0,
                rejected_count INTEGER DEFAULT 0,
                details      TEXT DEFAULT '{}',
                started_at   TEXT DEFAULT '',
                completed_at TEXT DEFAULT '',
                FOREIGN KEY (job_id) REFERENCES jobs(id)
            );
            CREATE INDEX IF NOT EXISTS idx_job_stages_job ON job_stages(job_id);

            CREATE TABLE IF NOT EXISTS llm_usage (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                provider TEXT NOT NULL,
                model TEXT DEFAULT '',
                calls INTEGER DEFAULT 0,
                prompt_tokens INTEGER DEFAULT 0,
                completion_tokens INTEGER DEFAULT 0,
                total_tokens INTEGER DEFAULT 0,
                rate_limit INTEGER DEFAULT 0,
                rate_remaining INTEGER DEFAULT 0,
                rate_reset TEXT DEFAULT '',
                date TEXT NOT NULL,
                updated_at TEXT DEFAULT ''
            );
            CREATE INDEX IF NOT EXISTS idx_llm_usage_date ON llm_usage(date);
            CREATE UNIQUE INDEX IF NOT EXISTS idx_llm_usage_provider_date ON llm_usage(provider, date);
        """)
        self.conn.commit()

        # ── Safe migrations (add columns if missing) ──
        self._migrate()

    def _migrate(self):
        """Add new columns to existing tables if they don't exist."""
        existing_lead_cols = {r[1] for r in self.conn.execute("PRAGMA table_info(leads)").fetchall()}
        existing_job_cols = {r[1] for r in self.conn.execute("PRAGMA table_info(jobs)").fetchall()}

        # New columns to add to leads table
        new_lead_columns = {
            "workspace_id": "TEXT DEFAULT ''",
            "address": "TEXT DEFAULT ''",
            "employee_count_exact": "INTEGER DEFAULT 0",
            "revenue_range": "TEXT DEFAULT ''",
            "founded_year": "TEXT DEFAULT ''",
            "industry_tags": "TEXT DEFAULT ''",
            "technologies": "TEXT DEFAULT ''",
            # Structured website technographics JSON [{name,category,source,confidence}]
            "technographics": "TEXT DEFAULT ''",
            "funding_stage": "TEXT DEFAULT ''",
            "company_size_basis": "TEXT DEFAULT ''",
            "secondary_emails": "TEXT DEFAULT ''",
            "secondary_phones": "TEXT DEFAULT ''",
            "decision_makers": "TEXT DEFAULT ''",
            "glassdoor_rating": "TEXT DEFAULT ''",
            "email_confidence": "TEXT DEFAULT ''",
            "email_provider": "TEXT DEFAULT ''",
            "phone_provider": "TEXT DEFAULT ''",
            "facebook_url": "TEXT DEFAULT ''",
            "hiring_signals": "TEXT DEFAULT ''",
            "enrichment_attempts": "INTEGER DEFAULT 0",
            "enrichment_waterfall": "TEXT DEFAULT ''",
            # Per-fact provenance JSON {field: {source,license,confidence,fetched_at}}
            "field_provenance": "TEXT DEFAULT ''",
            "source_url": "TEXT DEFAULT ''",
            "collection_job_id": "TEXT DEFAULT ''",
            # ── OSS Enrichment Fields ──
            "founding_year": "TEXT DEFAULT ''",
            "last_funding_amount": "TEXT DEFAULT ''",
            "investors": "TEXT DEFAULT ''",
            "recent_news": "TEXT DEFAULT ''",
            "google_rating": "TEXT DEFAULT ''",
            "revenue_estimate": "TEXT DEFAULT ''",
            "email_verify": "TEXT DEFAULT ''",
            "email_presence": "TEXT DEFAULT ''",
        }

        for col, col_type in new_lead_columns.items():
            if col not in existing_lead_cols:
                self.conn.execute(f"ALTER TABLE leads ADD COLUMN {col} {col_type}")

        self.conn.execute(
            "CREATE INDEX IF NOT EXISTS idx_leads_collection_job "
            "ON leads(collection_job_id)"
        )

        if "workspace_id" not in existing_job_cols:
            self.conn.execute("ALTER TABLE jobs ADD COLUMN workspace_id TEXT DEFAULT ''")
        if "intent" not in existing_job_cols:
            self.conn.execute(
                "ALTER TABLE jobs ADD COLUMN intent TEXT DEFAULT 'market_search'"
            )
        if "intent_details" not in existing_job_cols:
            self.conn.execute(
                "ALTER TABLE jobs ADD COLUMN intent_details TEXT DEFAULT '{}'"
            )

        # Composite dedup key: (workspace_id, company, city) — replaces the old
        # global UNIQUE(company, city) so two tenants can each own (Acme, NYC).
        # Done here (not in create_tables) because workspace_id was just added.
        has_composite = self.conn.execute(
            "SELECT 1 FROM sqlite_master WHERE type='index' "
            "AND name='uq_leads_ws_company_city'"
        ).fetchone()
        if not has_composite:
            # Drop the legacy non-tenant unique index if present.
            self.conn.execute("DROP INDEX IF EXISTS idx_leads_company_city")
            # Pre-collapse any (workspace_id, company, city) collisions, keeping
            # the most complete/most recent row, so the unique index can build.
            self.conn.execute("""
                DELETE FROM leads WHERE id NOT IN (
                    SELECT id FROM (
                        SELECT id, ROW_NUMBER() OVER (
                            PARTITION BY COALESCE(workspace_id,''), company, city
                            ORDER BY (email IS NOT NULL AND email != '') DESC,
                                     score DESC, id DESC
                        ) AS rn FROM leads
                    ) WHERE rn = 1
                )
            """)
            self.conn.execute(
                "CREATE UNIQUE INDEX IF NOT EXISTS uq_leads_ws_company_city "
                "ON leads(workspace_id, company, city)"
            )
            # Keep a non-unique (company, city) index for query performance.
            self.conn.execute(
                "CREATE INDEX IF NOT EXISTS idx_leads_company_city ON leads(company, city)"
            )

        # Migrate llm_usage table
        try:
            existing_usage_cols = {r[1] for r in self.conn.execute("PRAGMA table_info(llm_usage)").fetchall()}
            for col, col_type in [("rate_limit", "INTEGER DEFAULT 0"), ("rate_remaining", "INTEGER DEFAULT 0"), ("rate_reset", "TEXT DEFAULT ''")]:
                if existing_usage_cols and col not in existing_usage_cols:
                    self.conn.execute(f"ALTER TABLE llm_usage ADD COLUMN {col} {col_type}")
        except Exception:
            pass  # Table may not exist yet

        self.conn.commit()

    # ── CRUD ───────────────────────────────────────────────────────────

    def upsert_lead(self, lead: Lead) -> int:
        """Insert or update a lead. Deduplicates by (workspace_id, company, city)."""
        lead.updated_at = _utcnow().isoformat()

        existing = self.conn.execute(
            "SELECT id FROM leads "
            "WHERE COALESCE(workspace_id,'') = COALESCE(?,'') AND company = ? AND city = ?",
            (lead.workspace_id, lead.company, lead.city)
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
        collection_job_id: Optional[str] = None,
        score_min: Optional[int] = None,
        score_max: Optional[int] = None,
        score_tier: Optional[str] = None,
        search: Optional[str] = None,
        workspace_id: Optional[str] = None,
        has_email: Optional[bool] = None,
        has_phone: Optional[bool] = None,
        company_size: Optional[str] = None,
        limit: int = 500,
        offset: int = 0,
        order_by: str = "score DESC",
    ) -> List[Lead]:
        """Query leads with filters."""
        conditions = []
        params = []

        # Tri-state presence filters (True = present, False = missing, None = any).
        if has_email is not None:
            conditions.append("(l.email IS NOT NULL AND l.email != '')" if has_email
                              else "(l.email IS NULL OR l.email = '')")
        if has_phone is not None:
            conditions.append("(l.phone IS NOT NULL AND l.phone != '')" if has_phone
                              else "(l.phone IS NULL OR l.phone = '')")

        if status:
            conditions.append("l.status = ?")
            params.append(status)
        if city:
            conditions.append("l.city = ?")
            params.append(city)
        if source:
            conditions.append("l.source = ?")
            params.append(source)
        if collection_job_id:
            # Legacy rows encoded ownership in source; keep them visible while
            # all new rows preserve source as the discovery channel.
            conditions.append("(l.collection_job_id = ? OR l.source = ?)")
            params.extend([collection_job_id, f"job:{collection_job_id}"])
        if score_min is not None:
            conditions.append("l.score >= ?")
            params.append(score_min)
        if score_max is not None:
            conditions.append("l.score <= ?")
            params.append(score_max)
        if score_tier:
            conditions.append("l.score_tier = ?")
            params.append(score_tier)
        if company_size:
            conditions.append("l.company_size = ?")
            params.append(company_size)

        if search:
            # Use FTS for text search, wrapping in quotes to prevent FTS5 syntax errors with special chars
            clean_search = search.replace('"', '""')
            conditions.append("l.id IN (SELECT rowid FROM leads_fts WHERE leads_fts MATCH ?)")
            params.append(f'"{clean_search}"')

        if workspace_id:
            conditions.append("l.workspace_id = ?")
            params.append(workspace_id)

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

    def query_leads_page(
        self,
        filter_criteria: Optional[Dict[str, Any]] = None,
        page: int = 1,
        page_size: int = 100,
    ) -> tuple[List[Dict[str, Any]], int]:
        """Workbook-compatible filtered page over the SQLite lead store.

        This adapter keeps workbook code away from the backend-specific
        ``sqlite3.Connection`` and has a matching implementation on
        :class:`PgLeadStore`.
        """
        fc = filter_criteria or {}
        conditions: List[str] = []
        params: List[Any] = []

        for key in ("city", "state", "score_tier", "status", "source", "company_size"):
            if fc.get(key):
                conditions.append(f"l.{key} = ?")
                params.append(fc[key])
        if fc.get("job_ids"):
            job_ids = list(fc["job_ids"])
            sources = [f"job:{job_id}" for job_id in job_ids]
            conditions.append(
                f"(l.collection_job_id IN ({','.join('?' for _ in job_ids)}) "
                f"OR l.source IN ({','.join('?' for _ in sources)}))"
            )
            params.extend(job_ids + sources)
        if fc.get("specialization"):
            conditions.append("l.specialization LIKE ?")
            params.append(f"%{fc['specialization']}%")
        for key in ("email", "phone", "website"):
            flag = fc.get(f"has_{key}")
            if flag is True:
                conditions.append(f"l.{key} IS NOT NULL AND l.{key} != ''")
            elif flag is False:
                conditions.append(f"(l.{key} IS NULL OR l.{key} = '')")
        if fc.get("min_score") is not None:
            conditions.append("l.score >= ?")
            params.append(fc["min_score"])
        if fc.get("max_score") is not None:
            conditions.append("l.score <= ?")
            params.append(fc["max_score"])
        if fc.get("search"):
            clean_search = str(fc["search"]).replace('"', '""')
            conditions.append(
                "l.id IN (SELECT rowid FROM leads_fts WHERE leads_fts MATCH ?)"
            )
            params.append(f'"{clean_search}"')

        where = " AND ".join(conditions) if conditions else "1=1"
        total = int(
            self.conn.execute(
                f"SELECT COUNT(*) FROM leads l WHERE {where}", params
            ).fetchone()[0]
        )
        offset = (max(1, page) - 1) * max(1, page_size)
        rows = self.conn.execute(
            f"SELECT l.* FROM leads l WHERE {where} "
            "ORDER BY l.score DESC LIMIT ? OFFSET ?",
            params + [max(1, page_size), offset],
        ).fetchall()
        return [dict(row) for row in rows], total

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
        now = _utcnow().isoformat()
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
        allowed = set(Lead.__dataclass_fields__) - {
            "id", "workspace_id", "created_at", "updated_at"
        }
        clean = {key: value for key, value in fields.items() if key in allowed}
        if not clean:
            return
        clean["updated_at"] = _utcnow().isoformat()
        set_clause = ", ".join(f"{key} = ?" for key in clean)
        self.conn.execute(
            f"UPDATE leads SET {set_clause} WHERE id = ?",
            list(clean.values()) + [lead_id]
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

    def get_filter_options(self) -> Dict[str, Any]:
        """Return workbook filter facets through the lead-store contract."""
        def _distinct(column: str, limit: Optional[int] = None) -> List[str]:
            suffix = f" LIMIT {int(limit)}" if limit else ""
            rows = self.conn.execute(
                f"SELECT DISTINCT {column} FROM leads "
                f"WHERE {column} IS NOT NULL AND {column} != '' "
                f"ORDER BY {column}{suffix}"
            ).fetchall()
            return [row[0] for row in rows]

        return {
            "cities": _distinct("city"),
            "tiers": _distinct("score_tier"),
            "sources": _distinct("source"),
            "statuses": _distinct("status"),
            "specializations": _distinct("specialization", 50),
            "total_leads": self.count_leads(),
        }

    # ── Jobs ───────────────────────────────────────────────────────────

    def create_job(
        self,
        job_id: str,
        query: str,
        intent: str = "market_search",
        intent_details: str = "{}",
    ) -> str:
        """Create a new collection job."""
        now = _utcnow().isoformat()
        self.conn.execute(
            "INSERT OR IGNORE INTO jobs "
            "(id, query, intent, intent_details, created_at) VALUES (?, ?, ?, ?, ?)",
            (job_id, query, intent, intent_details, now)
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
        now = _utcnow().isoformat()
        self.conn.execute(
            "UPDATE jobs SET status = 'running', started_at = ?, attempts = attempts + 1 WHERE id = ?",
            (now, row["id"])
        )
        self.conn.commit()
        return dict(row)

    def complete_job(self, job_id: str, leads_found: int = 0):
        """Mark a job as completed."""
        now = _utcnow().isoformat()
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
                (error, _utcnow().isoformat(), job_id)
            )
        self.conn.commit()

    def cancel_job(self, job_id: str):
        """Cancel a running/pending job by marking it as cancelled."""
        self.conn.execute(
            "UPDATE jobs SET status = 'cancelled', error = 'Cancelled by user', completed_at = ? WHERE id = ? AND status IN ('running', 'pending')",
            (_utcnow().isoformat(), job_id)
        )
        self.conn.commit()

    def delete_job(self, job_id: str, keep_leads: bool = False):
        """Delete a job and optionally its associated leads.

        Args:
            job_id: The job ID to delete.
            keep_leads: If True, keeps the leads but removes the job record.
        """
        if not keep_leads:
            # Delete leads that were created by this job
            self.conn.execute(
                "DELETE FROM leads WHERE collection_job_id = ? OR source = ?",
                (job_id, f"job:{job_id}"),
            )
        # Delete stages
        self.conn.execute("DELETE FROM job_stages WHERE job_id = ?", (job_id,))
        # Delete the job itself
        self.conn.execute("DELETE FROM jobs WHERE id = ?", (job_id,))
        self.conn.commit()

    def retry_job(self, job_id: str):
        """Reset a failed/cancelled job to pending for re-processing."""
        self.conn.execute(
            "UPDATE jobs SET status = 'pending', error = '', completed_at = '', attempts = 0 WHERE id = ? AND status IN ('failed', 'cancelled')",
            (job_id,)
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

    # ── Job Stages ─────────────────────────────────────────────────────

    def create_stage(self, job_id: str, stage: str) -> int:
        """Create a pipeline stage record, return its ID."""
        now = _utcnow().isoformat()
        cur = self.conn.execute(
            "INSERT INTO job_stages (job_id, stage, status, started_at) VALUES (?, ?, 'running', ?)",
            (job_id, stage, now)
        )
        self.conn.commit()
        return cur.lastrowid or 0

    def complete_stage(self, stage_id: int, input_count: int = 0,
                       output_count: int = 0, rejected_count: int = 0,
                       details: str = '{}', status: str = 'done'):
        """Mark a stage as completed with its metrics."""
        now = _utcnow().isoformat()
        self.conn.execute(
            """UPDATE job_stages SET status = ?, input_count = ?, output_count = ?,
               rejected_count = ?, details = ?, completed_at = ? WHERE id = ?""",
            (status, input_count, output_count, rejected_count, details, now, stage_id)
        )
        self.conn.commit()

    def get_job_stages(self, job_id: str) -> List[Dict[str, Any]]:
        """Get all stages for a job, ordered by creation."""
        rows = self.conn.execute(
            "SELECT * FROM job_stages WHERE job_id = ? ORDER BY id", (job_id,)
        ).fetchall()
        return [dict(r) for r in rows]

    def get_job_detail(self, job_id: str) -> Optional[Dict[str, Any]]:
        """Get a single job with its stages."""
        row = self.conn.execute("SELECT * FROM jobs WHERE id = ?", (job_id,)).fetchone()
        if not row:
            return None
        job = dict(row)
        job["stages"] = self.get_job_stages(job_id)
        return job

    def get_job_leads(self, job_id: str, limit: int = 200) -> List[Dict[str, Any]]:
        """Get leads produced by a specific job."""
        rows = self.conn.execute(
            "SELECT * FROM leads WHERE collection_job_id = ? OR source = ? "
            "ORDER BY score DESC LIMIT ?",
            (job_id, f"job:{job_id}", limit)
        ).fetchall()
        return [dict(r) for r in rows]

    # ── LLM Usage Tracking ─────────────────────────────────────────────

    def record_llm_usage(self, provider: str, model: str, prompt_tokens: int, completion_tokens: int,
                         rate_limit: int = 0, rate_remaining: int = 0, rate_reset: str = ""):
        """Accumulate LLM usage for a provider on today's date."""
        today = _utcnow().strftime("%Y-%m-%d")
        now = _utcnow().isoformat()
        self.conn.execute("""
            INSERT INTO llm_usage (provider, model, calls, prompt_tokens, completion_tokens, total_tokens,
                                   rate_limit, rate_remaining, rate_reset, date, updated_at)
            VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(provider, date) DO UPDATE SET
                calls = calls + 1,
                prompt_tokens = prompt_tokens + excluded.prompt_tokens,
                completion_tokens = completion_tokens + excluded.completion_tokens,
                total_tokens = total_tokens + excluded.total_tokens,
                rate_limit = CASE WHEN excluded.rate_limit > 0 THEN excluded.rate_limit ELSE llm_usage.rate_limit END,
                rate_remaining = CASE WHEN excluded.rate_limit > 0 THEN excluded.rate_remaining ELSE llm_usage.rate_remaining END,
                rate_reset = CASE WHEN excluded.rate_reset != '' THEN excluded.rate_reset ELSE llm_usage.rate_reset END,
                model = excluded.model,
                updated_at = excluded.updated_at
        """, (provider, model, prompt_tokens, completion_tokens, prompt_tokens + completion_tokens,
              rate_limit, rate_remaining, rate_reset, today, now))
        self.conn.commit()

    def get_llm_usage(self, date: Optional[str] = None) -> List[Dict[str, Any]]:
        """Get LLM usage stats, optionally for a specific date (default: today)."""
        if not date:
            date = _utcnow().strftime("%Y-%m-%d")
        rows = self.conn.execute(
            "SELECT * FROM llm_usage WHERE date = ? ORDER BY calls DESC",
            (date,)
        ).fetchall()
        return [dict(r) for r in rows]

    def get_llm_usage_total(self) -> Dict[str, Any]:
        """Get total LLM usage across all time."""
        row = self.conn.execute(
            "SELECT SUM(calls) as total_calls, SUM(total_tokens) as total_tokens FROM llm_usage"
        ).fetchone()
        return dict(row) if row else {"total_calls": 0, "total_tokens": 0}

    # ── Workspaces ─────────────────────────────────────────────────────

    def create_workspace(self, name: str, description: str = "") -> str:
        """Create a new workspace and return its ID."""
        import uuid
        ws_id = str(uuid.uuid4())[:8]
        now = _utcnow().isoformat()
        self.conn.execute(
            "INSERT INTO workspaces (id, name, description, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
            (ws_id, name, description, now, now)
        )
        self.conn.commit()
        return ws_id

    def get_workspaces(self) -> List[Dict[str, Any]]:
        """List all workspaces with lead/job counts."""
        rows = self.conn.execute("""
            SELECT w.*,
                   (SELECT COUNT(*) FROM leads WHERE workspace_id = w.id) as lead_count,
                   (SELECT COUNT(*) FROM jobs WHERE workspace_id = w.id) as job_count,
                   (SELECT COUNT(*) FROM leads WHERE workspace_id = w.id AND status != 'dead') as active_lead_count
            FROM workspaces w
            ORDER BY w.updated_at DESC
        """).fetchall()
        return [dict(r) for r in rows]

    def get_workspace(self, ws_id: str) -> Optional[Dict[str, Any]]:
        """Get a single workspace by ID."""
        row = self.conn.execute("SELECT * FROM workspaces WHERE id = ?", (ws_id,)).fetchone()
        return dict(row) if row else None

    def delete_workspace(self, ws_id: str):
        """Delete a workspace and its leads/jobs."""
        self.conn.execute("DELETE FROM leads WHERE workspace_id = ?", (ws_id,))
        self.conn.execute("DELETE FROM jobs WHERE workspace_id = ?", (ws_id,))
        self.conn.execute("DELETE FROM workspaces WHERE id = ?", (ws_id,))
        self.conn.commit()

    # ── Cleanup ────────────────────────────────────────────────────────

    def close(self):
        """Close the database connection."""
        self.conn.close()

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.close()
