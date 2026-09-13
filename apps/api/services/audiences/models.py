import uuid

from sqlalchemy import Column, DateTime, Index, Integer, JSON, String, Text, UniqueConstraint
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

    def to_api(self) -> dict:
        return {
            "id": self.id,
            "name": self.name,
            "description": self.description or "",
            "filters": self.filters or {},
            "member_count": self.member_count or 0,
            "created_at": self.created_at,
            "updated_at": self.updated_at,
        }
