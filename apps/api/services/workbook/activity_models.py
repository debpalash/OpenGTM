"""WorkbookActivity (Pillar 3) — the living-workbook event feed."""

from sqlalchemy import Column, String, Integer, Text, DateTime
from sqlalchemy.sql import func

from apps.api.database import Base


class WorkbookActivity(Base):
    __tablename__ = "workbook_activity"

    id = Column(Integer, primary_key=True, autoincrement=True)
    # Denormalized tenant (RLS migration e5f6a7b8c9d0). Always set from the parent
    # workbook's workspace_id so the fail-closed RLS policy + WITH CHECK bind.
    workspace_id = Column(String, nullable=False, index=True)
    workbook_id = Column(String, index=True, nullable=False)
    kind = Column(String(50), default="info")  # source_run | refresh | signal | rows_added | reenrich
    message = Column(Text, default="")
    created_at = Column(DateTime, server_default=func.now(), index=True)

    def to_api(self) -> dict:
        return {
            "id": self.id, "kind": self.kind, "message": self.message,
            "created_at": self.created_at.isoformat() if self.created_at else None,
        }
