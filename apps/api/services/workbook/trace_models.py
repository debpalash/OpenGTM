"""CellTrace (Pillar 4) — the reasoning/provenance trace of an agent column run."""

from sqlalchemy import Column, String, Integer, JSON, DateTime, UniqueConstraint
from sqlalchemy.sql import func

from apps.api.database import Base


class CellTrace(Base):
    __tablename__ = "cell_traces"
    __table_args__ = (UniqueConstraint("workbook_id", "lead_id", "column_id", name="uq_cell_trace"),)

    id = Column(Integer, primary_key=True, autoincrement=True)
    # Denormalized tenant (RLS migration e5f6a7b8c9d0). Always set from the parent
    # workbook's workspace_id so the fail-closed RLS policy + WITH CHECK bind.
    workspace_id = Column(String, nullable=False, index=True)
    workbook_id = Column(String, index=True, nullable=False)
    lead_id = Column(Integer, index=True, nullable=False)
    column_id = Column(String, nullable=False)
    goal = Column(String(512), default="")
    steps = Column(JSON, default=list)     # [{step, provider, success, value, cost, reason}]
    outcome = Column(String(32), default="")  # found | exhausted | budget | error
    total_cost_usd = Column(JSON, default=dict)  # {"spent": float}
    created_at = Column(DateTime, server_default=func.now())

    def to_api(self) -> dict:
        return {
            "workbook_id": self.workbook_id, "lead_id": self.lead_id,
            "column_id": self.column_id, "goal": self.goal,
            "steps": self.steps or [], "outcome": self.outcome,
            "total_cost_usd": self.total_cost_usd or {},
        }
