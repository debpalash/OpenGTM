import uuid

from sqlalchemy import Boolean, Column, DateTime, ForeignKey, Index, Integer, JSON, String, Text, UniqueConstraint
from sqlalchemy.sql import func

from apps.api.database import Base


class Audience(Base):
    """A reusable dynamic segment over the workspace lead store."""

    __tablename__ = "audiences"
    __table_args__ = (
        UniqueConstraint("workspace_id", "name", name="uq_audiences_workspace_name"),
        Index("ix_audiences_workspace_updated", "workspace_id", "updated_at"),
    )

    id = Column(String, primary_key=True, default=lambda: str(uuid.uuid4()))
    workspace_id = Column(String, nullable=False, index=True)
    name = Column(String(200), nullable=False)
    description = Column(Text, nullable=False, default="")
    filters = Column(JSON, nullable=False, default=dict)
    member_count = Column(Integer, nullable=False, default=0)
    created_at = Column(DateTime, nullable=False, server_default=func.now())
    updated_at = Column(DateTime, nullable=False, server_default=func.now(), onupdate=func.now())
    refreshed_at = Column(DateTime, nullable=True)
    refresh_enabled = Column(Boolean, nullable=False, default=True, server_default="true")
    refresh_interval_minutes = Column(Integer, nullable=False, default=60, server_default="60")
    next_refresh_at = Column(DateTime, nullable=True)

    def to_api(self) -> dict:
        return {
            "id": self.id,
            "name": self.name,
            "description": self.description or "",
            "filters": self.filters or {},
            "member_count": self.member_count or 0,
            "created_at": self.created_at,
            "updated_at": self.updated_at,
            "refreshed_at": self.refreshed_at,
            "refresh_enabled": bool(self.refresh_enabled),
            "refresh_interval_minutes": self.refresh_interval_minutes or 60,
            "next_refresh_at": self.next_refresh_at,
        }


class AudienceMember(Base):
    """Current materialized membership for fast activation and reliable diffs."""

    __tablename__ = "audience_members"
    __table_args__ = (
        UniqueConstraint("audience_id", "lead_id", name="uq_audience_members_audience_lead"),
        Index("ix_audience_members_workspace_audience", "workspace_id", "audience_id"),
    )

    id = Column(Integer, primary_key=True, autoincrement=True)
    workspace_id = Column(String, nullable=False, index=True)
    audience_id = Column(String, ForeignKey("audiences.id", ondelete="CASCADE"), nullable=False, index=True)
    lead_id = Column(Integer, nullable=False, index=True)
    snapshot = Column(JSON, nullable=False, default=dict)
    joined_at = Column(DateTime, nullable=False, server_default=func.now())
    last_seen_at = Column(DateTime, nullable=False, server_default=func.now(), onupdate=func.now())

    def to_api(self) -> dict:
        return {
            "lead_id": self.lead_id,
            "snapshot": self.snapshot or {},
            "joined_at": self.joined_at,
            "last_seen_at": self.last_seen_at,
        }


class AudienceMembershipEvent(Base):
    """Append-only audience entry/exit history used by automations and audit."""

    __tablename__ = "audience_membership_events"
    __table_args__ = (
        Index("ix_audience_events_workspace_audience_created", "workspace_id", "audience_id", "created_at"),
    )

    id = Column(Integer, primary_key=True, autoincrement=True)
    workspace_id = Column(String, nullable=False, index=True)
    audience_id = Column(String, ForeignKey("audiences.id", ondelete="CASCADE"), nullable=False, index=True)
    lead_id = Column(Integer, nullable=False, index=True)
    event_type = Column(String(16), nullable=False)
    snapshot = Column(JSON, nullable=False, default=dict)
    created_at = Column(DateTime, nullable=False, server_default=func.now())

    def to_api(self) -> dict:
        return {
            "id": self.id,
            "audience_id": self.audience_id,
            "lead_id": self.lead_id,
            "event_type": self.event_type,
            "snapshot": self.snapshot or {},
            "created_at": self.created_at,
        }


class AudienceSchedule(Base):
    """Non-RLS scheduling mirror containing identifiers and timing only."""

    __tablename__ = "audience_schedules"
    __table_args__ = (Index("ix_audience_schedules_due", "enabled", "next_refresh_at"),)

    audience_id = Column(String, primary_key=True)
    workspace_id = Column(String, nullable=False)
    next_refresh_at = Column(DateTime, nullable=True)
    enabled = Column(Boolean, nullable=False, default=True, server_default="true")
    updated_at = Column(DateTime, nullable=False, server_default=func.now(), onupdate=func.now())
