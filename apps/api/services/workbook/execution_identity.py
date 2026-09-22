"""Stable queue identity propagated through concurrent workbook cell tasks."""
from contextlib import contextmanager
from contextvars import ContextVar
from dataclasses import dataclass
import hashlib
import json


@dataclass(frozen=True)
class WorkbookExecutionIdentity:
    workspace_id: str
    workbook_id: str
    job_id: int

    @property
    def run_id(self):
        return f"job:{self.job_id}"

    def attempt_key(self, *, row_identity: str, column_id: str, provider: str) -> str:
        if not all(isinstance(value, str) and value for value in (row_identity, column_id, provider)):
            raise ValueError("Explicit row, column and provider identities required")
        # Retry pass, worker lease and mutable inputs are intentionally excluded.
        # Changed inputs must conflict with the reservation contract, not mint
        # another billable attempt under the same approved run.
        encoded = json.dumps([self.workspace_id, self.workbook_id, self.run_id,
                              row_identity, column_id, provider], separators=(",", ":"))
        return hashlib.sha256(encoded.encode()).hexdigest()


current_execution: ContextVar[WorkbookExecutionIdentity | None] = ContextVar("workbook_execution", default=None)


@contextmanager
def execution_scope(workspace_id: str, workbook_id: str, job_id: int | None):
    if not workspace_id or not workbook_id:
        raise ValueError("Workspace and workbook are required")
    if job_id is not None and (type(job_id) is not int or job_id <= 0):
        raise ValueError("Invalid durable job ID")
    identity = WorkbookExecutionIdentity(workspace_id, workbook_id, job_id) if job_id is not None else None
    token = current_execution.set(identity)
    try:
        yield identity
    finally:
        current_execution.reset(token)
