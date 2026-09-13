import uuid

from sqlalchemy import Boolean, Column, DateTime, ForeignKey, Index, Integer, JSON, String, Text, UniqueConstraint
from sqlalchemy.sql import func

from apps.api.database import Base


def _uuid() -> str:
    return str(uuid.uuid4())


class AudienceDestination(Base):
    __tablename__ = "audience_destinations"
    __table_args__ = (
        UniqueConstraint("workspace_id", "audience_id", "name", name="uq_audience_destinations_name"),
        Index("ix_audience_destinations_workspace_audience", "workspace_id", "audience_id"),
    )

    id = Column(String, primary_key=True, default=_uuid)
    workspace_id = Column(String, nullable=False, index=True)
    audience_id = Column(String, ForeignKey("audiences.id", ondelete="CASCADE"), nullable=False, index=True)
    name = Column(String(200), nullable=False)
    destination_type = Column(String(32), nullable=False)
    enabled = Column(Boolean, nullable=False, default=True, server_default="true")
    config = Column(JSON, nullable=False, default=dict)
    field_map = Column(JSON, nullable=False, default=dict)
    health_status = Column(String(20), nullable=False, default="unverified", server_default="unverified")
    last_error = Column(Text, nullable=True)
    last_success_at = Column(DateTime, nullable=True)
    created_at = Column(DateTime, nullable=False, server_default=func.now())
    updated_at = Column(DateTime, nullable=False, server_default=func.now(), onupdate=func.now())

    def to_api(self) -> dict:
        safe_config = {k: v for k, v in (self.config or {}).items() if k not in {"token", "api_key", "secret"}}
        return {
            "id": self.id, "audience_id": self.audience_id, "name": self.name,
            "destination_type": self.destination_type, "enabled": bool(self.enabled),
            "config": safe_config, "field_map": self.field_map or {},
            "health_status": self.health_status, "last_error": self.last_error,
            "last_success_at": self.last_success_at, "created_at": self.created_at,
            "updated_at": self.updated_at,
        }


class DestinationRun(Base):
    __tablename__ = "destination_runs"
    __table_args__ = (Index("ix_destination_runs_workspace_destination_created", "workspace_id", "destination_id", "created_at"),)

    id = Column(String, primary_key=True, default=_uuid)
    workspace_id = Column(String, nullable=False, index=True)
    destination_id = Column(String, ForeignKey("audience_destinations.id", ondelete="CASCADE"), nullable=False, index=True)
    status = Column(String(20), nullable=False, default="pending", server_default="pending")
    requested_by = Column(String, nullable=True)
    attempted = Column(Integer, nullable=False, default=0, server_default="0")
    succeeded = Column(Integer, nullable=False, default=0, server_default="0")
    failed = Column(Integer, nullable=False, default=0, server_default="0")
    skipped = Column(Integer, nullable=False, default=0, server_default="0")
    error = Column(Text, nullable=True)
    started_at = Column(DateTime, nullable=True)
    finished_at = Column(DateTime, nullable=True)
    created_at = Column(DateTime, nullable=False, server_default=func.now())

    def to_api(self) -> dict:
        return {key: getattr(self, key) for key in (
            "id", "destination_id", "status", "attempted", "succeeded", "failed",
            "skipped", "error", "started_at", "finished_at", "created_at",
        )}


class DestinationDelivery(Base):
    __tablename__ = "destination_deliveries"
    __table_args__ = (
        UniqueConstraint("workspace_id", "idempotency_key", name="uq_destination_delivery_idem"),
        Index("ix_destination_deliveries_workspace_run", "workspace_id", "run_id"),
    )

    id = Column(Integer, primary_key=True, autoincrement=True)
    workspace_id = Column(String, nullable=False, index=True)
    run_id = Column(String, ForeignKey("destination_runs.id", ondelete="CASCADE"), nullable=False, index=True)
    destination_id = Column(String, ForeignKey("audience_destinations.id", ondelete="CASCADE"), nullable=False, index=True)
    lead_id = Column(Integer, nullable=False, index=True)
    operation = Column(String(16), nullable=False, default="upsert")
    idempotency_key = Column(String(255), nullable=False)
    payload_fingerprint = Column(String(64), nullable=False)
    status = Column(String(20), nullable=False, default="pending")
    attempts = Column(Integer, nullable=False, default=0)
    external_id = Column(String, nullable=True)
    summary = Column(Text, nullable=True)
    error = Column(Text, nullable=True)
    delivered_at = Column(DateTime, nullable=True)
    created_at = Column(DateTime, nullable=False, server_default=func.now())
    updated_at = Column(DateTime, nullable=False, server_default=func.now(), onupdate=func.now())

    def to_api(self) -> dict:
        return {key: getattr(self, key) for key in (
            "id", "run_id", "destination_id", "lead_id", "operation", "status",
            "attempts", "external_id", "summary", "error", "delivered_at", "created_at",
        )}
