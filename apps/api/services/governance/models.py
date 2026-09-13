import uuid

from sqlalchemy import Boolean, Column, DateTime, Index, Integer, JSON, String, Text
from sqlalchemy.sql import func

from apps.api.database import Base


class GovernanceAuditEvent(Base):
    """Append-only, tenant-scoped record of authenticated API mutations."""
    __tablename__ = "governance_audit_events"
    __table_args__ = (
        Index("ix_governance_audit_ws_created", "workspace_id", "created_at"),
        Index("ix_governance_audit_ws_actor", "workspace_id", "actor_user_id"),
        Index("ix_governance_audit_request", "request_id"),
    )
    id = Column(String(36), primary_key=True, default=lambda: str(uuid.uuid4()))
    workspace_id = Column(String(64), nullable=False, index=True)
    actor_user_id = Column(Integer, nullable=True)
    actor_role = Column(String(32), nullable=False, default="")
    method = Column(String(10), nullable=False)
    route = Column(String(255), nullable=False)
    resource_path = Column(String(500), nullable=False)
    response_status = Column(Integer, nullable=False)
    outcome = Column(String(20), nullable=False)
    request_id = Column(String(64), nullable=False)
    metadata_json = Column(JSON, nullable=False, default=dict)
    created_at = Column(DateTime, nullable=False, server_default=func.now())

    def to_api(self):
        return {"id": self.id, "actor_user_id": self.actor_user_id, "actor_role": self.actor_role, "method": self.method, "route": self.route, "resource_path": self.resource_path, "response_status": self.response_status, "outcome": self.outcome, "request_id": self.request_id, "metadata": self.metadata_json or {}, "created_at": self.created_at}


class RetentionPolicy(Base):
    __tablename__ = "retention_policies"
    workspace_id = Column(String(64), primary_key=True)
    enabled = Column(Boolean, nullable=False, default=False, server_default="false")
    legal_hold = Column(Boolean, nullable=False, default=False, server_default="false")
    retention_days = Column(JSON, nullable=False, default=dict)
    next_run_at = Column(DateTime, nullable=True)
    updated_by = Column(Integer, nullable=True)
    created_at = Column(DateTime, nullable=False, server_default=func.now())
    updated_at = Column(DateTime, nullable=False, server_default=func.now(), onupdate=func.now())

    def to_api(self):
        return {"workspace_id": self.workspace_id, "enabled": bool(self.enabled), "legal_hold": bool(self.legal_hold), "retention_days": self.retention_days or {}, "next_run_at": self.next_run_at, "updated_by": self.updated_by, "created_at": self.created_at, "updated_at": self.updated_at}


class RetentionSchedule(Base):
    """Non-RLS identifiers-only mirror for scheduler cold starts."""
    __tablename__ = "retention_schedules"
    workspace_id = Column(String(64), primary_key=True)
    enabled = Column(Boolean, nullable=False, default=False, server_default="false")
    next_run_at = Column(DateTime, nullable=True)
    updated_at = Column(DateTime, nullable=False, server_default=func.now(), onupdate=func.now())


class RetentionRun(Base):
    __tablename__ = "retention_runs"
    __table_args__ = (Index("ix_retention_runs_ws_created", "workspace_id", "created_at"),)
    id = Column(String(36), primary_key=True, default=lambda: str(uuid.uuid4()))
    workspace_id = Column(String(64), nullable=False, index=True)
    status = Column(String(24), nullable=False, default="pending")
    requested_by = Column(String(64), nullable=True)
    policy_snapshot = Column(JSON, nullable=False, default=dict)
    deleted_counts = Column(JSON, nullable=False, default=dict)
    error = Column(Text, nullable=True)
    started_at = Column(DateTime, nullable=True)
    finished_at = Column(DateTime, nullable=True)
    created_at = Column(DateTime, nullable=False, server_default=func.now())

    def to_api(self):
        return {key: getattr(self, key) for key in ("id", "status", "requested_by", "policy_snapshot", "deleted_counts", "error", "started_at", "finished_at", "created_at")}
