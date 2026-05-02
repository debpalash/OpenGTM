from sqlalchemy import Column, Integer, String, Boolean, DateTime, Text, JSON
from datetime import datetime
from apps.api.database import Base


class User(Base):
    __tablename__ = "users"
    id = Column(Integer, primary_key=True, index=True)
    username = Column(String, unique=True, index=True)
    hashed_password = Column(String)
    is_active = Column(Boolean, default=True)
    is_admin = Column(Boolean, default=False)
    # New Fields
    role = Column(String, default="user")  # 'admin', 'editor', 'user', 'viewer'
    profile_image = Column(String, nullable=True)
    last_login = Column(DateTime, nullable=True)
    created_at = Column(DateTime, default=datetime.utcnow)


class Link(Base):
    __tablename__ = "links"
    id = Column(Integer, primary_key=True, index=True)
    url = Column(String, unique=True, index=True)
    status = Column(String, default="Pending")  # Pending, Processing, Completed, Failed
    created_at = Column(String, default=datetime.utcnow().isoformat)
    source = Column(String, nullable=True)  # Added source column for tracking
    updated_at = Column(String, nullable=True)  # Added updated_at


class Job(Base):
    __tablename__ = "jobs"
    id = Column(Integer, primary_key=True, index=True)
    type = Column(String, index=True)  # 'download_link', 'health_check', etc
    payload = Column(JSON)
    status = Column(String, default="pending", index=True)
    priority = Column(Integer, default=1)
    created_at = Column(DateTime, default=datetime.utcnow)
    started_at = Column(DateTime, nullable=True)
    completed_at = Column(DateTime, nullable=True)
    error = Column(String, nullable=True)
    # Enhanced Fields
    retry_count = Column(Integer, default=0)
    max_retries = Column(Integer, default=3)
    last_heartbeat = Column(DateTime, nullable=True)
    worker_id = Column(String, nullable=True)
    next_run_at = Column(DateTime, default=datetime.utcnow)


class ScrapeHistory(Base):
    __tablename__ = "scrape_history"

    id = Column(Integer, primary_key=True, index=True)
    url = Column(String, index=True)
    title = Column(String, nullable=True)
    method = Column(String)  # fast, robust
    word_count = Column(Integer, default=0)
    email_count = Column(Integer, default=0)
    created_at = Column(
        String, default=lambda: datetime.now().strftime("%Y-%m-%d %H:%M:%S")
    )


class EmailData(Base):
    __tablename__ = "email_data"
    id = Column(Integer, primary_key=True, index=True)
    name = Column(String)
    email = Column(String)
    source_link_id = Column(Integer)
    created_at = Column(String, default=datetime.utcnow().isoformat)
    is_used = Column(Boolean, default=False)
    # CRM Fields
    tags = Column(String, nullable=True)  # Comma-separated tags
    notes = Column(Text, nullable=True)


class PersonIntel(Base):
    __tablename__ = "person_intel"
    id = Column(Integer, primary_key=True, index=True)
    linkedin_url = Column(String, index=True)
    username = Column(String, index=True)
    name = Column(String, nullable=True)
    headline = Column(String, nullable=True)
    location = Column(String, nullable=True)
    summary = Column(Text, nullable=True)
    emails = Column(JSON, default=[])
    social_links = Column(JSON, default={})
    articles = Column(JSON, default=[])
    mentions = Column(JSON, default=[])
    companies = Column(JSON, default=[])
    education = Column(JSON, default=[])
    skills = Column(JSON, default=[])
    raw_sources = Column(JSON, default=[])
    status = Column(String, default="pending")
    created_at = Column(DateTime, default=datetime.utcnow)
    updated_at = Column(DateTime, nullable=True)
