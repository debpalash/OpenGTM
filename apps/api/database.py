from sqlalchemy import create_engine, text, inspect
from sqlalchemy.orm import declarative_base, sessionmaker
from apps.api.core.config import settings
import logging

# Database Setup
engine = create_engine(settings.DATABASE_URL, connect_args={"check_same_thread": False})
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

        print("✓ Database migration completed successfully")

    except Exception as e:
        print(f"✗ Migration failed: {e}")

