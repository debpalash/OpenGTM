import asyncio

from apps.api.services.leadgen.db import LeadDB
from apps.api.services.leadgen.job_runner import JobRunner


def test_persisted_bare_domain_is_blocked_before_source_stages(tmp_path):
    db = LeadDB(str(tmp_path / "leads.db"))
    db.create_job("blocked", "stripe.com", intent="market_search")
    runner = JobRunner(db=db)

    asyncio.run(runner._process_job({
        "id": "blocked",
        "query": "stripe.com",
        "intent": "market_search",
        "workspace_id": "workspace-1",
    }))

    job = db.get_job_detail("blocked")
    db.close()
    assert job["status"] == "failed"
    assert job["error"].startswith("collection_intent_blocked:")
    assert job["stages"] == []
