import uuid

from sqlalchemy import Column, DateTime, Index, Integer, JSON, String
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
