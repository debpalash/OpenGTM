import uuid

from sqlalchemy import Boolean, Column, DateTime, Float, ForeignKey, Index, Integer, JSON, String, Text, UniqueConstraint
from sqlalchemy.sql import func

from apps.api.database import Base


class ResearchPlaybook(Base):
    __tablename__ = "research_playbooks"
    __table_args__ = (UniqueConstraint("workspace_id", "name", name="uq_research_playbook_name"),)
    id = Column(String, primary_key=True, default=lambda: str(uuid.uuid4()))
    workspace_id = Column(String, nullable=False, index=True)
    name = Column(String(200), nullable=False)
    description = Column(Text, nullable=False, default="")
    prompt_template = Column(Text, nullable=False)
    steps = Column(JSON, nullable=False, default=list)
    output_format = Column(String(20), nullable=False, default="text")
    max_steps = Column(Integer, nullable=False, default=4)
    cell_budget_usd = Column(Float, nullable=False, default=0.10)
    version = Column(Integer, nullable=False, default=1)
    enabled = Column(Boolean, nullable=False, default=True, server_default="true")
    schedule_audience_id = Column(String, ForeignKey("audiences.id", ondelete="SET NULL"), nullable=True)
    schedule_interval_minutes = Column(Integer, nullable=True)
    next_run_at = Column(DateTime, nullable=True)
    created_at = Column(DateTime, nullable=False, server_default=func.now())
    updated_at = Column(DateTime, nullable=False, server_default=func.now(), onupdate=func.now())

    def to_api(self):
        return {key: getattr(self, key) for key in ("id", "name", "description", "prompt_template", "steps", "output_format", "max_steps", "cell_budget_usd", "version", "enabled", "schedule_audience_id", "schedule_interval_minutes", "next_run_at", "created_at", "updated_at")}


class PlaybookSchedule(Base):
    """Non-RLS identifiers-only mirror used for restart reconciliation."""
    __tablename__ = "playbook_schedules"
    playbook_id = Column(String, primary_key=True)
    workspace_id = Column(String, nullable=False, index=True)
    enabled = Column(Boolean, nullable=False, default=False, server_default="false")
    next_run_at = Column(DateTime, nullable=True)
    updated_at = Column(DateTime, nullable=False, server_default=func.now(), onupdate=func.now())


class PlaybookRun(Base):
    __tablename__ = "playbook_runs"
    __table_args__ = (Index("ix_playbook_runs_ws_playbook_created", "workspace_id", "playbook_id", "created_at"),)
    id = Column(String, primary_key=True, default=lambda: str(uuid.uuid4()))
    workspace_id = Column(String, nullable=False, index=True)
    playbook_id = Column(String, ForeignKey("research_playbooks.id", ondelete="CASCADE"), nullable=False, index=True)
    audience_id = Column(String, ForeignKey("audiences.id", ondelete="CASCADE"), nullable=False, index=True)
    status = Column(String(24), nullable=False, default="pending")
    prompt_version = Column(Integer, nullable=False)
    prompt_snapshot = Column(Text, nullable=False)
    steps_snapshot = Column(JSON, nullable=False, default=list)
    max_members = Column(Integer, nullable=False, default=100)
    attempted = Column(Integer, nullable=False, default=0)
    succeeded = Column(Integer, nullable=False, default=0)
    failed = Column(Integer, nullable=False, default=0)
    error = Column(Text, nullable=True)
    requested_by = Column(String, nullable=True)
    started_at = Column(DateTime, nullable=True)
    finished_at = Column(DateTime, nullable=True)
    created_at = Column(DateTime, nullable=False, server_default=func.now())

    def to_api(self):
        return {key: getattr(self, key) for key in ("id", "playbook_id", "audience_id", "status", "prompt_version", "max_members", "attempted", "succeeded", "failed", "error", "started_at", "finished_at", "created_at")}


class PlaybookResult(Base):
    __tablename__ = "playbook_results"
    __table_args__ = (UniqueConstraint("workspace_id", "run_id", "lead_id", name="uq_playbook_result_run_lead"),)
    id = Column(Integer, primary_key=True, autoincrement=True)
    workspace_id = Column(String, nullable=False, index=True)
    run_id = Column(String, ForeignKey("playbook_runs.id", ondelete="CASCADE"), nullable=False, index=True)
    lead_id = Column(Integer, nullable=False, index=True)
    status = Column(String(20), nullable=False, default="pending")
    value = Column(Text, nullable=False, default="")
    result_metadata = Column(JSON, nullable=False, default=dict)
    error = Column(Text, nullable=True)
    attempts = Column(Integer, nullable=False, default=0)
    created_at = Column(DateTime, nullable=False, server_default=func.now())
    updated_at = Column(DateTime, nullable=False, server_default=func.now(), onupdate=func.now())

    def to_api(self):
        return {"id": self.id, "run_id": self.run_id, "lead_id": self.lead_id, "status": self.status, "value": self.value, "metadata": self.result_metadata or {}, "error": self.error, "attempts": self.attempts, "created_at": self.created_at, "updated_at": self.updated_at}
