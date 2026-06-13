"""Shared pytest fixtures/config.

Default DATABASE_URL to SQLite so importing API modules (which build a
SQLAlchemy engine at import time) doesn't require psycopg / a live Postgres.
Set only if the caller hasn't already chosen a database.
"""
import os

os.environ.setdefault("DATABASE_URL", "sqlite:///./data/_pytest.db")
