"""Durable audience-scoped research playbook execution."""

from datetime import datetime, timezone

from apps.api.database import SessionLocal


async def handle_playbook_run(job_id: int, payload: dict) -> None:
    workspace_id, run_id = payload.get("workspace_id"), payload.get("run_id")
    if not workspace_id or not run_id:
        raise ValueError("playbook run requires workspace_id and run_id")
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.audiences.models import AudienceMember
    from apps.api.services.playbooks.models import PlaybookResult, PlaybookRun, ResearchPlaybook
    from apps.api.services.workbook.research_column import execute_research_column
    with workspace_scope(workspace_id):
        with SessionLocal() as db:
            run = db.query(PlaybookRun).filter(PlaybookRun.id == run_id, PlaybookRun.workspace_id == workspace_id).first()
            if run is None or run.status == "completed":
                return
            playbook = db.query(ResearchPlaybook).filter(ResearchPlaybook.id == run.playbook_id, ResearchPlaybook.workspace_id == workspace_id).first()
            if playbook is None:
                raise ValueError("playbook not found")
            run.status, run.started_at, run.error = "running", run.started_at or datetime.now(timezone.utc), None
            db.commit()
            members = db.query(AudienceMember).filter(AudienceMember.workspace_id == workspace_id, AudienceMember.audience_id == run.audience_id).order_by(AudienceMember.lead_id).limit(run.max_members).all()
            for member in members:
                result = db.query(PlaybookResult).filter(PlaybookResult.workspace_id == workspace_id, PlaybookResult.run_id == run.id, PlaybookResult.lead_id == member.lead_id).first()
                if result and result.status == "success":
                    continue
                result = result or PlaybookResult(workspace_id=workspace_id, run_id=run.id, lead_id=member.lead_id)
                if result.id is None:
                    db.add(result)
                result.status, result.attempts, result.error = "running", (result.attempts or 0) + 1, None
                db.commit()
                try:
                    output = await execute_research_column(run.prompt_snapshot, dict(member.snapshot or {}), [], max_steps=playbook.max_steps, output_format=playbook.output_format, workspace_id=workspace_id, cell_budget_usd=playbook.cell_budget_usd)
                    result.status = "success" if output.get("success") else "failed"
                    result.value = str(output.get("value") or "")
                    result.result_metadata = output.get("metadata") or {}
                    result.error = (output.get("error") or None)
                except Exception as exc:
                    result.status, result.error = "failed", str(exc)[:1000]
                db.commit()
            results = db.query(PlaybookResult).filter(PlaybookResult.workspace_id == workspace_id, PlaybookResult.run_id == run.id).all()
            run.attempted = len(results)
            run.succeeded = sum(x.status == "success" for x in results)
            run.failed = sum(x.status == "failed" for x in results)
            run.status = "completed" if not run.failed else "completed_with_errors"
            run.finished_at = datetime.now(timezone.utc)
            db.commit()
