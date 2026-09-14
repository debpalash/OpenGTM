"""Durable audience-scoped research playbook execution."""

from datetime import datetime, timezone

from apps.api.database import SessionLocal


def reconcile_playbook_job_failure(
    job_id: int,
    payload: dict,
    error: str,
    will_retry: bool,
) -> None:
    """Reconcile playbook state when its isolated worker is killed or fails."""
    workspace_id = str(payload.get("workspace_id") or "").strip()
    run_id = str(payload.get("run_id") or "").strip()
    if not workspace_id or not run_id:
        raise ValueError("playbook failure payload requires workspace_id and run_id")

    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.playbooks.models import PlaybookResult, PlaybookRun

    message = str(error or "playbook attempt failed")[:1000]
    with workspace_scope(workspace_id):
        with SessionLocal() as db:
            run = db.query(PlaybookRun).filter(
                PlaybookRun.id == run_id,
                PlaybookRun.workspace_id == workspace_id,
            ).first()
            if run is None or run.status in {"completed", "completed_with_errors", "cancelled"}:
                return
            results = db.query(PlaybookResult).filter(
                PlaybookResult.workspace_id == workspace_id,
                PlaybookResult.run_id == run_id,
            ).all()
            if will_retry:
                run.status = "pending"
                run.error = f"Queue retry scheduled: {message}"
                run.finished_at = None
                for result in results:
                    if result.status == "running":
                        result.status = "pending"
                        result.error = run.error
            else:
                run.status = "failed"
                run.error = f"Final failure: {message}"
                run.finished_at = datetime.now(timezone.utc)
                for result in results:
                    if result.status in {"pending", "running"}:
                        result.status = "failed"
                        result.error = run.error
                run.attempted = len(results)
                run.succeeded = sum(result.status == "success" for result in results)
                run.failed = sum(result.status == "failed" for result in results)
            db.commit()


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
            if run is None or run.status in {"completed", "cancelled"}:
                return
            playbook = db.query(ResearchPlaybook).filter(ResearchPlaybook.id == run.playbook_id, ResearchPlaybook.workspace_id == workspace_id).first()
            if playbook is None:
                raise ValueError("playbook not found")
            run.status, run.started_at, run.error = "running", run.started_at or datetime.now(timezone.utc), None
            db.commit()
            members = db.query(AudienceMember).filter(AudienceMember.workspace_id == workspace_id, AudienceMember.audience_id == run.audience_id).order_by(AudienceMember.lead_id).limit(run.max_members).all()
            for member in members:
                db.refresh(run)
                if run.status == "cancelled":
                    return
                result = db.query(PlaybookResult).filter(PlaybookResult.workspace_id == workspace_id, PlaybookResult.run_id == run.id, PlaybookResult.lead_id == member.lead_id).first()
                if result and result.status == "success":
                    continue
                result = result or PlaybookResult(workspace_id=workspace_id, run_id=run.id, lead_id=member.lead_id)
                if result.id is None:
                    db.add(result)
                result.status, result.attempts, result.error = "running", (result.attempts or 0) + 1, None
                db.commit()
                try:
                    context = dict(member.snapshot or {})
                    steps = run.steps_snapshot or [{"key": "result", "name": "Research", "prompt_template": run.prompt_snapshot, "output_format": playbook.output_format}]
                    step_outputs = []
                    output = None
                    for step in steps:
                        db.refresh(run)
                        if run.status == "cancelled":
                            result.status, result.error = "cancelled", "Cancelled by user"
                            db.commit()
                            return
                        output = await execute_research_column(step["prompt_template"], context, [], max_steps=playbook.max_steps, output_format=step.get("output_format", "text"), workspace_id=workspace_id, cell_budget_usd=playbook.cell_budget_usd / len(steps))
                        step_outputs.append({"key": step["key"], "name": step["name"], "success": bool(output.get("success")), "value": output.get("value"), "metadata": output.get("metadata") or {}, "error": output.get("error")})
                        if not output.get("success"):
                            break
                        context[step["key"]] = output.get("value")
                    assert output is not None
                    result.status = "success" if output.get("success") else "failed"
                    result.value = str(output.get("value") or "")
                    result.result_metadata = {"steps": step_outputs, "completed_steps": sum(item["success"] for item in step_outputs), "total_steps": len(steps)}
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
