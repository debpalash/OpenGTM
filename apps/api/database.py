from sqlalchemy import create_engine, text, inspect
from sqlalchemy.orm import declarative_base, sessionmaker
from apps.api.core.config import settings
import logging

# Database Setup
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
SessionLocal = sessionmaker(autocommit=False, autoflush=False, bind=engine)
Base = declarative_base()


def get_db():
    db = SessionLocal()
    try:
        yield db
    finally:
        db.close()


def check_and_migrate_db():
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

        print("✓ Database migration completed successfully")

    except Exception as e:
        print(f"✗ Migration failed: {e}")

