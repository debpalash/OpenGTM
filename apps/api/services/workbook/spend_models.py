"""Durable estimated provider exposure, separate from invoiced spend/credits."""
from sqlalchemy import Column, String, BigInteger, Float, JSON, Index, UniqueConstraint, CheckConstraint
from apps.api.database import Base


class WorkbookSpendAttempt(Base):
    __tablename__ = "workbook_spend_attempts"
    __table_args__ = (
        UniqueConstraint("workspace_id", "attempt_key", name="uq_workbook_spend_attempt_key"),
        CheckConstraint("reserved_microusd >= 0", name="ck_workbook_spend_nonnegative"),
        CheckConstraint("settled_microusd IS NULL OR settled_microusd >= 0", name="ck_workbook_spend_settlement_nonnegative"),
        CheckConstraint("status IN ('reserved', 'dispatched', 'settled', 'released', 'uncertain')", name="ck_workbook_spend_status"),
        Index("ix_workbook_spend_exposure", "workspace_id", "workbook_id", "status"),
    )
    id = Column(String(36), primary_key=True)
    workspace_id = Column(String(255), nullable=False)
    workbook_id = Column(String(36), nullable=False)
    run_id = Column(String(255), nullable=False)
    row_identity = Column(String(255), nullable=False)
    column_id = Column(String(255), nullable=False)
    provider = Column(String(255), nullable=False)
    attempt_key = Column(String(255), nullable=False)
    contract_hash = Column(String(64), nullable=False)
    reserved_microusd = Column(BigInteger, nullable=False)
    settled_microusd = Column(BigInteger, nullable=True)
    cost_basis = Column(JSON, nullable=False)
    status = Column(String(20), nullable=False)
    created_at = Column(Float, nullable=False)
    updated_at = Column(Float, nullable=False)
    result = Column(JSON, nullable=True)
