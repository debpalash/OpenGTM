from sqlalchemy import create_engine, text, inspect
from sqlalchemy.orm import declarative_base, sessionmaker
from apps.api.core.config import settings
import logging

# Database Setup — backend-aware. SQLite needs per-connection pragmas and a
# single-writer lock workaround; Postgres handles concurrent writers natively
# (MVCC), so the workbook enrichment run no longer hits "database is locked".
IS_SQLITE = settings.DATABASE_URL.startswith("sqlite")

if IS_SQLITE:
    engine = create_engine(
        settings.DATABASE_URL,
        connect_args={"check_same_thread": False, "timeout": 30},
        pool_pre_ping=True,
    )

    # Enable WAL mode for concurrent reads/writes
    from sqlalchemy import event

    @event.listens_for(engine, "connect")
    def _set_sqlite_pragma(dbapi_connection, connection_record):
        cursor = dbapi_connection.cursor()
        cursor.execute("PRAGMA journal_mode=WAL")
        cursor.execute("PRAGMA busy_timeout=30000")
        cursor.execute("PRAGMA synchronous=NORMAL")
        cursor.close()
else:
    # Postgres (or any non-sqlite). pool_pre_ping recycles dropped connections;
    # a modest pool comfortably covers the concurrent enrichment workers.
    engine = create_engine(
        settings.DATABASE_URL,
        pool_pre_ping=True,
        pool_size=10,
        max_overflow=20,
    )

SessionLocal = sessionmaker(autocommit=False, autoflush=False, bind=engine)
Base = declarative_base()


def get_db():
    db = SessionLocal()
    try:
        yield db
    finally:
        db.close()


def check_and_migrate_db():
    # These are incremental ALTER-based migrations for the legacy SQLite file.
    # On Postgres the schema is built fresh by Base.metadata.create_all() with
    # all columns/constraints already present, so there is nothing to migrate
    # (and the sqlite_master probe below would error). Skip entirely.
    if not IS_SQLITE:
        print("✓ Postgres backend — schema managed by create_all(), no migration needed")
        return
    try:
        inspector = inspect(engine)

        with engine.connect() as conn:
            # Users Table
            user_columns = [c["name"] for c in inspector.get_columns("users")]

            if "role" not in user_columns:
                print("Migrating DB: Adding 'role' column to users")
                conn.execute(
                    text("ALTER TABLE users ADD COLUMN role VARCHAR DEFAULT 'user'")
                )
                conn.commit()

            if "profile_image" not in user_columns:
                print("Migrating DB: Adding 'profile_image' column to users")
                conn.execute(text("ALTER TABLE users ADD COLUMN profile_image VARCHAR"))
                conn.commit()

            if "last_login" not in user_columns:
                print("Migrating DB: Adding 'last_login' column to users")
                conn.execute(text("ALTER TABLE users ADD COLUMN last_login DATETIME"))
                conn.commit()

            if "created_at" not in user_columns:
                print("Migrating DB: Adding 'created_at' column to users")
                conn.execute(text("ALTER TABLE users ADD COLUMN created_at DATETIME"))
                conn.commit()

            # EmailData Table
            email_columns = [c["name"] for c in inspector.get_columns("email_data")]

            if "created_at" not in email_columns:
                print("Migrating DB: Adding 'created_at' column to email_data")
                conn.execute(
                    text("ALTER TABLE email_data ADD COLUMN created_at DATETIME")
                )
                conn.commit()

            if "tags" not in email_columns:
                print("Migrating email_data table: adding tags...")
                conn.execute(text("ALTER TABLE email_data ADD COLUMN tags VARCHAR"))
                conn.commit()

            if "notes" not in email_columns:
                print("Migrating email_data table: adding notes...")
                conn.execute(text("ALTER TABLE email_data ADD COLUMN notes TEXT"))
                conn.commit()

            # Links Table
            link_columns = [c["name"] for c in inspector.get_columns("links")]

            if "source" not in link_columns:
                print("Migrating DB: Adding 'source' column to links")
                conn.execute(text("ALTER TABLE links ADD COLUMN source VARCHAR"))
                conn.commit()

            if "updated_at" not in link_columns:
                print("Migrating DB: Adding 'updated_at' column to links")
                conn.execute(text("ALTER TABLE links ADD COLUMN updated_at VARCHAR"))
                conn.commit()

            # Jobs Table
            job_columns = [c["name"] for c in inspector.get_columns("jobs")]

            if "retry_count" not in job_columns:
                print("Migrating jobs table: adding retry_count...")
                conn.execute(
                    text("ALTER TABLE jobs ADD COLUMN retry_count INTEGER DEFAULT 0")
                )
                conn.commit()

            if "max_retries" not in job_columns:
                print("Migrating jobs table: adding max_retries...")
                conn.execute(
                    text("ALTER TABLE jobs ADD COLUMN max_retries INTEGER DEFAULT 3")
                )
                conn.commit()

            if "last_heartbeat" not in job_columns:
                print("Migrating jobs table: adding last_heartbeat...")
                conn.execute(
                    text("ALTER TABLE jobs ADD COLUMN last_heartbeat TIMESTAMP")
                )
                conn.commit()

            if "next_run_at" not in job_columns:
                print("Migrating jobs table: adding next_run_at...")
                conn.execute(text("ALTER TABLE jobs ADD COLUMN next_run_at TIMESTAMP"))
                conn.commit()

            if "worker_id" not in job_columns:
                print("Migrating jobs table: adding worker_id...")
                conn.execute(text("ALTER TABLE jobs ADD COLUMN worker_id VARCHAR"))
                conn.commit()

            # PersonIntel Table — check existence and add any new columns
            if "person_intel" in inspector.get_table_names():
                pi_columns = [c["name"] for c in inspector.get_columns("person_intel")]
                if "updated_at" not in pi_columns:
                    print("Migrating person_intel table: adding updated_at...")
                    conn.execute(text("ALTER TABLE person_intel ADD COLUMN updated_at DATETIME"))
                    conn.commit()

            # Workbooks Table — Clay v2 migration (source_type, source_config, sync_to_leads)
            if "workbooks" in inspector.get_table_names():
                wb_columns = [c["name"] for c in inspector.get_columns("workbooks")]
                if "source_type" not in wb_columns:
                    print("Migrating workbooks: adding source_type...")
                    conn.execute(text("ALTER TABLE workbooks ADD COLUMN source_type VARCHAR(50) DEFAULT 'leads_filter'"))
                    conn.commit()
                if "source_config" not in wb_columns:
                    print("Migrating workbooks: adding source_config...")
                    conn.execute(text("ALTER TABLE workbooks ADD COLUMN source_config JSON DEFAULT '{}'"))
                    conn.commit()
                if "sync_to_leads" not in wb_columns:
                    print("Migrating workbooks: adding sync_to_leads...")
                    conn.execute(text("ALTER TABLE workbooks ADD COLUMN sync_to_leads BOOLEAN DEFAULT 1"))
                    conn.commit()
                if "workspace_id" not in wb_columns:
                    print("Migrating workbooks: adding workspace_id (tenancy)...")
                    conn.execute(text("ALTER TABLE workbooks ADD COLUMN workspace_id VARCHAR"))
                    conn.commit()
                # Pillar 2: budget ceiling
                if "budget_max_usd" not in wb_columns:
                    print("Migrating workbooks: adding budget_max_usd...")
                    conn.execute(text("ALTER TABLE workbooks ADD COLUMN budget_max_usd FLOAT DEFAULT 0"))
                    conn.commit()
                if "budget_spent_usd" not in wb_columns:
                    print("Migrating workbooks: adding budget_spent_usd...")
                    conn.execute(text("ALTER TABLE workbooks ADD COLUMN budget_spent_usd FLOAT DEFAULT 0"))
                    conn.commit()
                # Pillar 3: living refresh policy
                if "refresh_policy" not in wb_columns:
                    print("Migrating workbooks: adding refresh_policy...")
                    conn.execute(text("ALTER TABLE workbooks ADD COLUMN refresh_policy JSON DEFAULT '{}'"))
                    conn.commit()

            # CompanyEntity — tenant isolation column (added after first release)
            if "company_entities" in inspector.get_table_names():
                ce_columns = [c["name"] for c in inspector.get_columns("company_entities")]
                if "workspace_id" not in ce_columns:
                    print("Migrating company_entities: adding workspace_id...")
                    conn.execute(text("ALTER TABLE company_entities ADD COLUMN workspace_id VARCHAR DEFAULT ''"))
                    conn.commit()

            # WorkbookRow — Pillar 1: canonical entity binding
            if "workbook_rows" in inspector.get_table_names():
                wr_columns = [c["name"] for c in inspector.get_columns("workbook_rows")]
                if "canonical_entity_id" not in wr_columns:
                    print("Migrating workbook_rows: adding canonical_entity_id...")
                    conn.execute(text("ALTER TABLE workbook_rows ADD COLUMN canonical_entity_id VARCHAR"))
                    conn.commit()
                if "corroboration_count" not in wr_columns:
                    print("Migrating workbook_rows: adding corroboration_count...")
                    conn.execute(text("ALTER TABLE workbook_rows ADD COLUMN corroboration_count INTEGER DEFAULT 1"))
                    conn.commit()

            # WorkbookEnrichment — one row per (workbook, lead, column). Dedupe
            # any pre-existing duplicates (stale "running" rows from concurrent
            # runs/retries) then add the unique index. Guarded so it runs once.
            if "workbook_enrichments" in inspector.get_table_names():
                we_columns = [c["name"] for c in inspector.get_columns("workbook_enrichments")]
                if "cell_metadata" not in we_columns:
                    print("Migrating workbook_enrichments: adding cell_metadata (verify status)...")
                    conn.execute(text("ALTER TABLE workbook_enrichments ADD COLUMN cell_metadata JSON"))
                    conn.commit()
                has_uq = conn.execute(text(
                    "SELECT 1 FROM sqlite_master WHERE type='index' AND name='uq_enrichment_cell'"
                )).fetchone()
                if not has_uq:
                    print("Migrating workbook_enrichments: dedupe + unique cell index...")
                    # Keep the best row per cell: prefer a real value, then a
                    # terminal status, then the most recent id.
                    conn.execute(text("""
                        DELETE FROM workbook_enrichments
                        WHERE id NOT IN (
                          SELECT id FROM (
                            SELECT id, ROW_NUMBER() OVER (
                              PARTITION BY workbook_id, lead_id, column_id
                              ORDER BY (value IS NOT NULL AND value != '') DESC,
                                       CASE status WHEN 'complete' THEN 4 WHEN 'error' THEN 3
                                                   WHEN 'skipped' THEN 2 ELSE 1 END DESC,
                                       id DESC
                            ) AS rn
                            FROM workbook_enrichments
                          ) WHERE rn = 1
                        )
                    """))
                    conn.commit()
                    conn.execute(text(
                        "CREATE UNIQUE INDEX IF NOT EXISTS uq_enrichment_cell "
                        "ON workbook_enrichments (workbook_id, lead_id, column_id)"
                    ))
                    conn.commit()

            # provider_stats: correctness-prior columns (accuracy eval harness).
            if inspector.has_table("provider_stats"):
                ps_columns = [c["name"] for c in inspector.get_columns("provider_stats")]
                for col, ddl in (
                    ("accuracy_score", "ALTER TABLE provider_stats ADD COLUMN accuracy_score FLOAT"),
                    ("accuracy_samples", "ALTER TABLE provider_stats ADD COLUMN accuracy_samples INTEGER DEFAULT 0"),
                    ("accuracy_updated_at", "ALTER TABLE provider_stats ADD COLUMN accuracy_updated_at DATETIME"),
                ):
                    if col not in ps_columns:
                        print(f"Migrating DB: Adding '{col}' column to provider_stats")
                        conn.execute(text(ddl))
                        conn.commit()

        print("✓ Database migration completed successfully")

    except Exception as e:
        print(f"✗ Migration failed: {e}")

