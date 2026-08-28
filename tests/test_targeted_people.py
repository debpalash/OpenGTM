"""Evidence and lookalike rejection tests for targeted people research."""

import asyncio
import json

from apps.api.services.leadgen import targeted_people as tp


def _person(name, title, company, slug):
    return {
        "name": name,
        "title": title,
        "linkedin": f"https://www.linkedin.com/in/{slug}",
        "evidence_url": f"https://www.linkedin.com/in/{slug}",
        "evidence_title": f"{name} - {title} at {company} | LinkedIn",
        "evidence_snippet": f"{name}. {title} at {company}.",
    }


def test_exact_company_and_function_evidence_are_both_required(monkeypatch):
    async def fake_find(self, **kwargs):
        return ([
            _person("Jane Valid", "Head of Partnerships", "Stripe", "jane-valid"),
            _person("Theo Lookalike", "Head of Partnerships", "Stripe Theory", "theo"),
            _person("Sam Wrong Team", "Head of Engineering", "Stripe", "sam"),
        ], 3)

    monkeypatch.setattr(tp.CrossLinkedProvider, "find_people_by_titles", fake_find)
    result = asyncio.run(tp.research_people_at_company("Stripe", "partnerships"))

    assert result["count"] == 1
    assert result["people"][0]["name"] == "Jane Valid"
    assert result["people"][0]["evidence_url"].endswith("/jane-valid")
    assert result["people"][0]["email"] is None
    assert result["candidates_rejected"] == {
        "company_relationship_missing": 1,
        "function_evidence_missing": 1,
    }


def test_domain_target_uses_brand_but_still_rejects_lookalikes(monkeypatch):
    async def fake_find(self, **kwargs):
        return ([
            _person("Jane Valid", "Partnerships Director", "Stripe", "jane"),
            _person("Tara Wrong", "Partnerships Director", "Stripe Communications", "tara"),
        ], 1)

    monkeypatch.setattr(tp.CrossLinkedProvider, "find_people_by_titles", fake_find)
    result = asyncio.run(tp.research_people_at_company("stripe.com", "partnerships"))
    assert [person["name"] for person in result["people"]] == ["Jane Valid"]


def test_partnership_post_is_not_treated_as_partnership_role(monkeypatch):
    async def fake_find(self, **kwargs):
        return ([{
            "name": "Pat Executive",
            "title": "Stripe",
            "linkedin": "https://www.linkedin.com/in/pat",
            "evidence_url": "https://www.linkedin.com/in/pat",
            "evidence_title": "Pat Executive - Stripe | LinkedIn",
            "evidence_snippet": (
                "One week ago - We announced a strategic partnership with Acme. "
                "This will expand the Stripe platform."
            ),
        }], 1)

    monkeypatch.setattr(tp.CrossLinkedProvider, "find_people_by_titles", fake_find)
    result = asyncio.run(tp.research_people_at_company("Stripe", "partnerships"))
    assert result["people"] == []
    assert result["candidates_rejected"]["function_evidence_missing"] == 1


def test_profile_summary_plus_exact_experience_counts_as_role_evidence(monkeypatch):
    async def fake_find(self, **kwargs):
        return ([{
            "name": "Eva Ecosystem",
            "title": "Stripe",
            "linkedin": "https://www.linkedin.com/in/eva",
            "evidence_url": "https://www.linkedin.com/in/eva",
            "evidence_title": "Eva Ecosystem - Stripe | LinkedIn",
            "evidence_snippet": (
                "Partner ecosystems builder through alliances and partnerships. "
                "· Experience: Stripe · Location: London"
            ),
        }], 1)

    monkeypatch.setattr(tp.CrossLinkedProvider, "find_people_by_titles", fake_find)
    result = asyncio.run(tp.research_people_at_company("Stripe", "partnerships"))
    assert [person["name"] for person in result["people"]] == ["Eva Ecosystem"]
    assert result["people"][0]["verification_status"] == "profile_summary_plus_experience"


def test_verification_distinguishes_independent_corroboration(monkeypatch):
    async def fake_search(query, max_results=8):
        if "Jane Valid" in query:
            return [{
                "title": "Jane Valid leads strategic partnerships at PayPal",
                "body": "Jane Valid works at PayPal as Vice President of Strategic Partnerships.",
                "href": "https://example.com/paypal-jane-valid",
            }]
        return []

    monkeypatch.setattr(tp, "_public_search", fake_search)
    result = asyncio.run(tp.verify_people_at_company(
        "PayPal",
        "partnerships",
        [
            {"name": "Jane Valid", "title": "VP Partnerships", "confidence": 0.78},
            {"name": "No Evidence", "title": "Partnerships", "confidence": 0.78},
        ],
    ))

    assert result["people"][0]["verification_status"] == "independent_role_evidence"
    assert result["people"][0]["verification_sources"][0]["source_type"] == "independent_web"
    assert result["people"][1]["verification_status"] == "not_corroborated"
    assert result["summary"]["independent_role_evidence"] == 1
    assert result["summary"]["not_corroborated"] == 1


def test_verification_rejects_coincidental_company_or_function_hits(monkeypatch):
    async def fake_search(query, max_results=8):
        return [{
            "title": "Pat Person announces a partnership",
            "body": "Pat Person spoke about a PayPal partnership, but works at Acme.",
            "href": "https://example.com/pat",
        }]

    monkeypatch.setattr(tp, "_public_search", fake_search)
    result = asyncio.run(tp.verify_people_at_company(
        "PayPal", "partnerships", [{"name": "Pat Person", "title": "Engineer"}],
    ))
    assert result["people"][0]["verification_status"] == "not_corroborated"


def test_verification_rejects_function_mentioned_outside_actual_role(monkeypatch):
    async def fake_search(query, max_results=8):
        return [{
            "title": "Alumni Profiles - Example Law Firm",
            "body": (
                "Laura Lawyer – Global Antitrust Counsel at PayPal – has decades of "
                "experience in antitrust law covering mergers and strategic alliances."
            ),
            "href": "https://example.com/alumni",
        }]

    monkeypatch.setattr(tp, "_public_search", fake_search)
    result = asyncio.run(tp.verify_people_at_company(
        "PayPal", "partnerships", [{"name": "Laura Lawyer", "title": "Global Antitrust Counsel"}],
    ))
    assert result["people"][0]["verification_status"] == "not_corroborated"


def test_formatter_is_honest_about_evidence_and_email():
    from apps.api.routers.copilotkit import _format_people_research

    text = _format_people_research({
        "company": "Stripe",
        "function": "partnerships",
        "people": [{
            "name": "Jane Valid",
            "title": "Head of Partnerships",
            "linkedin_url": "https://www.linkedin.com/in/jane",
            "retrieved_at": "2026-08-28",
            "confidence": 0.72,
        }],
    })
    assert "evidence-matched" in text
    assert "can still be stale" in text
    assert "No email addresses were inferred" in text


def test_linkedin_company_discovery_preserves_discovery_url(monkeypatch):
    from apps.api.services.leadgen import job_runner as jr

    async def fake_search(query, max_results=10):
        return [{
            "href": "https://www.linkedin.com/company/acme-corp/",
            "title": "Acme Corp | LinkedIn",
            "body": "Acme Corp has 100 employees.",
        }]

    async def no_sleep(_seconds):
        return None

    monkeypatch.setattr(jr, "_ddg_search", fake_search)
    monkeypatch.setattr(jr.asyncio, "sleep", no_sleep)
    monkeypatch.setattr(jr.progress, "emit", lambda *args, **kwargs: None)
    runner = object.__new__(jr.JobRunner)

    leads = asyncio.run(runner._search_linkedin("job-1", "IT staffing Pune"))
    assert len(leads) == 1
    assert leads[0].source_url == "https://www.linkedin.com/company/acme-corp"
    assert leads[0].source_url != leads[0].website


def test_chat_intercepts_company_team_request_before_llm(monkeypatch):
    from apps.api.routers import copilotkit as ck

    class Request:
        async def json(self):
            return {
                "messages": [{"role": "user", "content": "Stripe partnership teams"}],
            }

    stored = []
    monkeypatch.setattr(ck, "_resolve_chat_workspace", lambda request: ("W1", None, "main"))
    monkeypatch.setattr(ck, "get_lead_store", lambda workspace_id, slug: object())
    monkeypatch.setattr(ck.chat_history, "create_conversation", lambda *a, **k: {"id": "conv-1"})
    monkeypatch.setattr(ck.chat_history, "add_message", lambda *args, **kwargs: stored.append(args))
    monkeypatch.setattr(
        ck,
        "_get_provider_chain",
        lambda: (_ for _ in ()).throw(AssertionError("LLM provider was consulted")),
    )

    async def run():
        response = await ck.copilot_chat(Request())
        chunks = []
        async for chunk in response.body_iterator:
            chunks.append(chunk.decode() if isinstance(chunk, bytes) else chunk)
        return "".join(chunks)

    stream = asyncio.run(run())
    assert '"conversation_id": "conv-1"' in stream
    assert '"intent_clarification"' in stream
    assert '"clarification_kind": "company_team"' in stream
    assert '"key": "team_people"' in stream
    assert "[DONE]" in stream
    assert any(message[1] == "assistant" for message in stored)


def test_explicit_people_workflow_runs_without_llm(monkeypatch):
    from apps.api.routers import copilotkit as ck

    class Request:
        async def json(self):
            return {
                "messages": [{
                    "role": "user",
                    "content": (
                        "Find named people currently working on Stripe's partnership team. "
                        "Require evidence for current employment and their partnership remit."
                    ),
                }],
            }

    async def fake_research(**kwargs):
        return {
            "ok": True,
            "company": kwargs["company"],
            "function": kwargs["function"],
            "people": [],
            "count": 0,
            "candidates_rejected": {},
        }

    stored = []
    monkeypatch.setattr(tp, "research_people_at_company", fake_research)
    monkeypatch.setattr(ck, "_resolve_chat_workspace", lambda request: ("W1", None, "main"))
    monkeypatch.setattr(ck, "get_lead_store", lambda workspace_id, slug: object())
    monkeypatch.setattr(ck.chat_history, "create_conversation", lambda *a, **k: {"id": "conv-2"})
    monkeypatch.setattr(ck.chat_history, "add_message", lambda *args, **kwargs: stored.append(args))
    monkeypatch.setattr(
        ck,
        "_get_provider_chain",
        lambda: (_ for _ in ()).throw(AssertionError("LLM provider was consulted")),
    )

    async def run():
        response = await ck.copilot_chat(Request())
        chunks = []
        async for chunk in response.body_iterator:
            chunks.append(chunk.decode() if isinstance(chunk, bytes) else chunk)
        return "".join(chunks)

    stream = asyncio.run(run())
    assert '"name": "find_people_at_company"' in stream
    assert "No broad collection was run" in stream
    assert "[DONE]" in stream
    assert any(message[1] == "tool" for message in stored)


def test_trusted_tool_context_preserves_people_for_followups(monkeypatch):
    from apps.api.routers import copilotkit as ck

    monkeypatch.setattr(ck.chat_history, "get_messages", lambda *args: [{
        "role": "tool",
        "tool_data": json.dumps({
            "name": "find_people_at_company",
            "result": {"company": "PayPal", "people": [{"name": "Jane Valid"}]},
        }),
    }])
    context = ck._conversation_tool_context("conv", "W1", 1)
    assert "Jane Valid" in context
    assert "Resolve pronouns" in context


def test_verify_them_uses_prior_people_without_llm(monkeypatch):
    from apps.api.routers import copilotkit as ck

    class Request:
        async def json(self):
            return {
                "conversation_id": "conv-verify",
                "messages": [{"role": "user", "content": "verify them"}],
            }

    prior = {
        "name": "find_people_at_company",
        "result": {
            "company": "PayPal", "function": "partnerships",
            "people": [{"name": "Jane Valid", "title": "Partnerships"}],
        },
    }

    async def fake_verify(**kwargs):
        return {
            "ok": True, "company": "PayPal", "function": "partnerships",
            "people": [{
                **kwargs["people"][0],
                "verification_status": "not_corroborated",
                "verification_confidence": 0.5,
                "verification_sources": [],
            }],
            "count": 1,
            "summary": {"independent_role_evidence": 0, "profile_reconfirmed": 0, "not_corroborated": 1},
        }

    stored = []
    monkeypatch.setattr(tp, "verify_people_at_company", fake_verify)
    monkeypatch.setattr(ck, "_resolve_chat_workspace", lambda request: ("W1", None, "main"))
    monkeypatch.setattr(ck, "get_lead_store", lambda *args: object())
    monkeypatch.setattr(ck.chat_history, "get_conversation", lambda *args: {"id": "conv-verify"})
    monkeypatch.setattr(ck.chat_history, "add_message", lambda *args, **kwargs: stored.append(args))
    monkeypatch.setattr(ck, "_latest_conversation_tool_result", lambda *args: prior)
    monkeypatch.setattr(
        ck, "_get_provider_chain",
        lambda: (_ for _ in ()).throw(AssertionError("LLM provider was consulted")),
    )

    async def run():
        response = await ck.copilot_chat(Request())
        chunks = []
        async for chunk in response.body_iterator:
            chunks.append(chunk.decode() if isinstance(chunk, bytes) else chunk)
        return "".join(chunks)

    stream = asyncio.run(run())
    assert '"name": "verify_people_at_company"' in stream
    assert "not corroborated" in stream.lower()
    assert any(message[1] == "tool" for message in stored)


def test_make_workbook_with_them_emits_confirmation_without_llm(monkeypatch):
    from apps.api.routers import copilotkit as ck

    class Request:
        async def json(self):
            return {
                "conversation_id": "conv-workbook",
                "messages": [{"role": "user", "content": "make a workbook with them"}],
            }

    prior = {
        "name": "find_people_at_company",
        "result": {
            "company": "PayPal", "function": "partnerships",
            "people": [{"name": "Jane Valid"}, {"name": "Alex Valid"}],
        },
    }
    monkeypatch.setattr(ck, "_resolve_chat_workspace", lambda request: ("W1", None, "main"))
    monkeypatch.setattr(ck, "get_lead_store", lambda *args: object())
    monkeypatch.setattr(ck.chat_history, "get_conversation", lambda *args: {"id": "conv-workbook"})
    monkeypatch.setattr(ck.chat_history, "add_message", lambda *args, **kwargs: None)
    monkeypatch.setattr(ck.chat_history, "create_tool_approval", lambda *args: "approval-1")
    monkeypatch.setattr(ck, "_latest_conversation_tool_result", lambda *args: prior)
    monkeypatch.setattr(
        ck, "_get_provider_chain",
        lambda: (_ for _ in ()).throw(AssertionError("LLM provider was consulted")),
    )

    async def run():
        response = await ck.copilot_chat(Request())
        chunks = []
        async for chunk in response.body_iterator:
            chunks.append(chunk.decode() if isinstance(chunk, bytes) else chunk)
        return "".join(chunks)

    stream = asyncio.run(run())
    assert '"name": "create_people_workbook"' in stream
    assert '"confirmation_id": "approval-1"' in stream
    assert "exact 2 people" in stream
