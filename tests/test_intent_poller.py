"""Scheduled Intent-Signal Poller — unit tests (SQLite, no Postgres required).

Maps to the spec acceptance criteria (AC-#). PG-gated RLS / on_signal /
concurrency / budget-atomicity tests live in tests/test_intent_poller_rls.py.

All external HTTP (SEC, JobSpy/DDG, RSS) is mocked; the webhook sender is never
invoked by the poller (delivery is on_signal→webhook only).
"""

import asyncio
from datetime import datetime, timezone

import httpx
import pytest

from apps.api.services.poller import keys, sources
from apps.api.services.poller import rss as rss_mod
from apps.api.services.poller.models import KEY_SCHEMA_VERSION


# ── tiny fake watch ───────────────────────────────────────────────────────────

class FakeWatch:
    def __init__(self, **kw):
        self.id = kw.get("id", "w1")
        self.workspace_id = kw.get("workspace_id", "wsA")
        self.kind = kw.get("kind", "company")
        self.target = kw.get("target", "Acme Robotics")
        self.resolved_cik = kw.get("resolved_cik")
        self.lead_id = kw.get("lead_id", 1)
        self.signal_types = kw.get("signal_types")
        self.cursor = kw.get("cursor", {"bootstrapped": True})


# ════════════════════════════ keys / dedup (AC-1) ════════════════════════════

def test_signal_event_id_deterministic_and_versioned():
    a = keys.signal_event_id("wsA", "funding", "0001", "company_funded", "0001:acc1")
    b = keys.signal_event_id("wsA", "funding", "0001", "company_funded", "0001:acc1")
    assert a == b and len(a) == 64
    # lead_id is NOT part of the key — different lead, same id.
    assert "lead" not in a
    # a different natural id changes the hash
    assert a != keys.signal_event_id("wsA", "funding", "0001", "company_funded", "0001:acc2")


def test_cik_supremacy_and_normalization():
    # once a CIK is known, the stable id uses the CIK not the (mutable) name
    assert keys.stable_company_id("0001234567", "Acme Robotics Inc") == "0001234567"
    # sec_cik: target normalizes to padded CIK
    assert keys.normalize_target("sec_cik:1234567") == "0001234567"
    # name normalization strips legal suffix
    assert keys.normalize_target("Acme Robotics, Inc.") == keys.normalize_target("acme robotics")
    # URL normalization
    assert keys.normalize_target("https://www.acme.com/blog/") == "acme.com/blog"


def test_role_tier_and_exec_titles():
    assert keys.role_tier("Chief Executive Officer") == "c_level"
    assert keys.role_tier("Director") == "director"
    assert keys.role_tier("VP Engineering") == "vp"
    assert keys.is_executive_title("CFO") is True
    assert keys.is_executive_title("Software Engineer") is False


# ════════════════════════ funding / exec (AC-5/16/19) ════════════════════════

class _FakeFiling:
    def __init__(self, accession, fields=None, persons=None, date="2026-01-01", cik="0001234567"):
        self.cik = cik
        self.accession = accession
        self.form = "D"
        self.filing_date = date
        self.fields = fields or {}
        self.related_persons = persons or []


class _FakeSec:
    def __init__(self, filings):
        self._filings = filings

    async def list_form_d_since(self, target, since):
        # newest-first; honor the cursor (strictly newer)
        out = [f for f in self._filings if not since or f.accession > since.replace("-", "")]
        return sorted(out, key=lambda f: f.accession, reverse=True)


def test_funding_new_accession_emits_then_noop_AC5():
    w = FakeWatch(cursor={"bootstrapped": True})
    sec = _FakeSec([_FakeFiling("acc100", fields={"funding_amount": 5_000_000})])
    events, patch = sources.fetch_funding_and_exec(
        w, want_funding=True, want_exec=False, backfill=False, provider=sec,
    )
    assert len(events) == 1
    assert events[0].signal_type == "company_funded"
    assert events[0].natural_event_id == "0001234567:acc100"
    assert patch["sec_last_accession"] == "acc100"
    # re-poll with cursor advanced → no new event
    w.cursor = {"bootstrapped": True, **patch}
    events2, _ = sources.fetch_funding_and_exec(
        w, want_funding=True, want_exec=False, backfill=False, provider=sec,
    )
    assert events2 == []


def test_missed_intermediate_funding_accessions_AC19():
    # TWO new accessions since cursor → BOTH emit, cursor advances to newest.
    w = FakeWatch(cursor={"bootstrapped": True, "sec_last_accession": "acc100"})
    sec = _FakeSec([
        _FakeFiling("acc102", fields={"funding_amount": 9}),
        _FakeFiling("acc101", fields={"funding_amount": 8}),
    ])
    events, patch = sources.fetch_funding_and_exec(
        w, want_funding=True, want_exec=False, backfill=False, provider=sec,
    )
    nat = {e.natural_event_id for e in events}
    assert nat == {"0001234567:acc101", "0001234567:acc102"}
    assert patch["sec_last_accession"] == "acc102"  # newest only


def test_executive_hired_from_form_d_and_noop_AC16():
    persons = [
        {"name": "Jane Doe", "title": "Chief Executive Officer"},
        {"name": "Bob Roe", "title": "Software Engineer"},  # not exec → no signal
    ]
    w = FakeWatch(cursor={"bootstrapped": True})
    sec = _FakeSec([_FakeFiling("acc200", persons=persons)])
    events, patch = sources.fetch_funding_and_exec(
        w, want_funding=False, want_exec=True, backfill=False, provider=sec,
    )
    exec_events = [e for e in events if e.signal_type == "executive_hired"]
    assert len(exec_events) == 1
    assert exec_events[0].natural_event_id == "0001234567:jane doe:c_level"
    # the engineer produced NO executive_hired (no NER / non-exec title)
    assert all("Bob Roe" not in e.title for e in exec_events)
    # repeat filing → known exec recorded → no re-emit
    w.cursor = {"bootstrapped": True, **patch}
    events2, _ = sources.fetch_funding_and_exec(
        w, want_funding=False, want_exec=True, backfill=False, provider=sec,
    )
    assert [e for e in events2 if e.signal_type == "executive_hired"] == []


def test_bootstrap_suppresses_pre_existing_funding_AC24_semantics():
    # first poll (bootstrapped=False, no backfill) records cursor, suppresses emit.
    w = FakeWatch(cursor={"bootstrapped": False})
    sec = _FakeSec([_FakeFiling("acc300", fields={"funding_amount": 1})])
    events, patch = sources.fetch_funding_and_exec(
        w, want_funding=True, want_exec=False, backfill=False, provider=sec,
    )
    assert events == []
    assert patch["sec_last_accession"] == "acc300"  # state recorded


# ════════════════════════ hiring band / tech (AC-6) ══════════════════════════

def _signals(band, tech=None):
    return {"growth_signal": band, "tech_adoption_signal": tech or []}


def test_hiring_upward_band_crossing_emits_once_per_week_AC6():
    w = FakeWatch(cursor={"bootstrapped": True, "hiring_band": "growing"})
    ev, patch = sources.fetch_hiring_and_tech(
        w, want_hiring=True, want_tech=False, backfill=False,
        signals_override=_signals("rapid_growth"),
    )
    surges = [e for e in ev if e.signal_type == "hiring_surge"]
    assert len(surges) == 1
    week = patch["hiring_band_week"]
    # same-week re-cross is suppressed
    w.cursor = {"bootstrapped": True, "hiring_band": "growing", "hiring_band_week": week}
    ev2, _ = sources.fetch_hiring_and_tech(
        w, want_hiring=True, want_tech=False, backfill=False,
        signals_override=_signals("rapid_growth"),
    )
    assert [e for e in ev2 if e.signal_type == "hiring_surge"] == []


def test_hiring_same_band_does_not_emit_AC6():
    w = FakeWatch(cursor={"bootstrapped": True, "hiring_band": "rapid_growth"})
    # raw count would change but band is the same → NO emit (asserts band semantics)
    ev, _ = sources.fetch_hiring_and_tech(
        w, want_hiring=True, want_tech=False, backfill=False,
        signals_override=_signals("rapid_growth"),
    )
    assert [e for e in ev if e.signal_type == "hiring_surge"] == []


def test_new_tech_and_intent_upgrade_AC6():
    w = FakeWatch(cursor={"bootstrapped": True})
    tech = [{"tech": "Snowflake", "category": "Data", "intent": "mentions"}]
    ev, patch = sources.fetch_hiring_and_tech(
        w, want_hiring=False, want_tech=True, backfill=False,
        signals_override=_signals("none", tech),
    )
    assert [e for e in ev if e.signal_type == "new_tech_adopted"]
    # same tier → no re-emit
    w.cursor = {"bootstrapped": True, **patch}
    ev2, patch2 = sources.fetch_hiring_and_tech(
        w, want_hiring=False, want_tech=True, backfill=False,
        signals_override=_signals("none", tech),
    )
    assert [e for e in ev2 if e.signal_type == "new_tech_adopted"] == []
    # intent upgrade → re-emit ONCE
    tech_up = [{"tech": "Snowflake", "category": "Data", "intent": "adopting"}]
    w.cursor = {"bootstrapped": True, **patch2}
    ev3, _ = sources.fetch_hiring_and_tech(
        w, want_hiring=False, want_tech=True, backfill=False,
        signals_override=_signals("none", tech_up),
    )
    up = [e for e in ev3 if e.signal_type == "new_tech_adopted"]
    assert len(up) == 1 and "adopting" in up[0].natural_event_id


# ════════════════════════ RSS / news (AC-7, AC-21) ═══════════════════════════

_RSS_XML = """<?xml version="1.0"?><rss version="2.0"><channel>
<item><guid>g1</guid><title>One</title><link>http://x/1</link><pubDate>Mon, 01 Jun 2026 00:00:00 GMT</pubDate></item>
<item><guid>g2</guid><title>Two</title><link>http://x/2</link><pubDate>Tue, 02 Jun 2026 00:00:00 GMT</pubDate></item>
<item><guid>g3</guid><title>Three</title><link>http://x/3</link><pubDate>Wed, 03 Jun 2026 00:00:00 GMT</pubDate></item>
</channel></rss>"""


def _feed_result(entries_xml, status=200, etag=None, lm=None, not_modified=False):
    entries = rss_mod.parse_feed(entries_xml, 100) if status == 200 else []
    return rss_mod.FeedResult(
        status=status, not_modified=not_modified, entries=entries, etag=etag, last_modified=lm,
    )


def test_rss_bootstrap_then_incremental_AC7():
    w = FakeWatch(kind="feed", target="https://acme.com/rss", cursor={"bootstrapped": False})

    def fetch(url, etag=None, last_modified=None):
        return _feed_result(_RSS_XML)

    ev, patch = sources.fetch_feed(w, backfill=False, fetcher=fetch)
    assert ev == []  # bootstrap suppresses pre-existing entries
    assert set(patch["feed_seen_guids"]) == {"g1", "g2", "g3"}

    # +1 new entry → exactly one news signal
    w.cursor = {"bootstrapped": True, **patch}
    xml2 = _RSS_XML.replace(
        "</channel>",
        '<item><guid>g4</guid><title>Four</title><link>http://x/4</link>'
        '<pubDate>Thu, 04 Jun 2026 00:00:00 GMT</pubDate></item></channel>',
    )

    def fetch2(url, etag=None, last_modified=None):
        return _feed_result(xml2)

    ev2, _ = sources.fetch_feed(w, backfill=False, fetcher=fetch2)
    news = [e for e in ev2 if e.signal_type == "news"]
    assert len(news) == 1 and news[0].natural_event_id == "g4"


def test_rss_conditional_get_304_short_circuit_AC21():
    w = FakeWatch(kind="feed", target="https://acme.com/rss",
                  cursor={"bootstrapped": True, "feed_etag": "abc"})

    def fetch(url, etag=None, last_modified=None):
        assert etag == "abc"  # cursor etag passed through
        return _feed_result("", status=304, not_modified=True)

    ev, patch = sources.fetch_feed(w, backfill=False, fetcher=fetch)
    assert ev == []  # 304 → no work
    assert "feed_seen_guids" not in patch  # no re-scan


def test_rss_blocked_url_returns_error_no_emit_AC21():
    from apps.api.core.url_guard import BlockedUrlError

    w = FakeWatch(kind="feed", target="http://169.254.169.254/feed",
                  cursor={"bootstrapped": True})

    def fetch(url, etag=None, last_modified=None):
        return rss_mod.FeedResult(error="blocked_url: private")

    ev, patch = sources.fetch_feed(w, backfill=False, fetcher=fetch)
    assert ev is None and patch is None  # fetch failure → no cursor advance


def test_real_rss_fetcher_pins_and_blocks_private(monkeypatch):
    # AC-21: the REAL fetch_feed must go through pinned_get → BlockedUrlError on
    # a private-resolving host (no emit, error surfaced).
    from apps.api.core.url_guard import BlockedUrlError

    res = rss_mod.fetch_feed("http://127.0.0.1/feed")
    assert res.error and "blocked" in res.error.lower()


# ════════════════════════ SEC provider contract (AC-25) ══════════════════════

# Realistic submissions JSON: parallel arrays NEWEST-FIRST (accession monotonic
# by SEC issuance, so newest-first == highest accession first).
_SUBMISSIONS = {
    "filings": {"recent": {
        "form": ["D/A", "D", "10-K"],
        "accessionNumber": ["0000000000-26-000003", "0000000000-26-000002", "0000000000-26-000001"],
        "filingDate": ["2026-03-01", "2026-02-01", "2026-01-01"],
    }}
}
_PRIMARY_DOC = """<?xml version="1.0"?><edgarSubmission xmlns="x">
<offeringData><totalAmountSold>1000000</totalAmountSold>
<dateOfFirstSale><value>2026-02-01</value></dateOfFirstSale></offeringData>
<relatedPersonsList><relatedPersonInfo>
<relatedPersonName><firstName>Jane</firstName><lastName>Doe</lastName></relatedPersonName>
<relatedPersonRelationshipList><relationship>Executive Officer</relationship></relatedPersonRelationshipList>
</relatedPersonInfo></relatedPersonsList></edgarSubmission>"""


def test_real_provider_list_form_d_since_returns_accession_AC25(monkeypatch):
    from apps.api.services.leadgen.enrichment.providers import sec_edgar as se

    def handler(request: httpx.Request) -> httpx.Response:
        url = str(request.url)
        if "submissions" in url:
            return httpx.Response(200, json=_SUBMISSIONS)
        if "primary_doc.xml" in url:
            return httpx.Response(200, text=_PRIMARY_DOC)
        return httpx.Response(404)

    transport = httpx.MockTransport(handler)
    real_async_client = httpx.AsyncClient

    def patched_client(*a, **kw):
        kw.pop("transport", None)
        return real_async_client(*a, transport=transport, **{k: v for k, v in kw.items()})

    monkeypatch.setattr(se.httpx, "AsyncClient", patched_client)

    provider = se.SecEdgarProvider()
    filings = asyncio.run(provider.list_form_d_since("sec_cik:1234567", None))
    # D and D/A returned (the 10-K is filtered out), with accession + persons
    accs = [f.accession for f in filings]
    assert "000000000026000003" in accs  # D/A
    assert "000000000026000002" in accs  # D
    assert "000000000026000001" not in accs  # 10-K is NOT a Form D
    # related persons parsed from the real parser
    assert any(p["name"] == "Jane Doe" for f in filings for p in f.related_persons)
    # cursor honored: nothing newer than the max
    none_newer = asyncio.run(provider.list_form_d_since("sec_cik:1234567", "000000000026000003"))
    assert none_newer == []


# ════════════════════════ handler registration (AC-10) ═══════════════════════

def test_handler_registered_in_main_and_worker_AC10():
    import apps.api.worker as worker_mod
    from apps.api.services.queue_service import queue_service, JOB_TIMEOUTS

    queue_service.handlers.clear()
    worker_mod._register_handlers()
    assert "watch_poll" in queue_service.handlers
    assert JOB_TIMEOUTS["watch_poll"] == 600


# ════════════════════════ flag OFF (AC-13) ═══════════════════════════════════

def test_handler_early_exits_when_disabled_AC13(monkeypatch):
    from apps.api.services.poller import engine as eng
    monkeypatch.setattr(eng.settings, "INTENT_POLLER_ENABLED", False, raising=False)
    # returns immediately (no DB touch) — must not raise
    asyncio.run(eng.handle_watch_poll(1, {"workspace_id": "ws", "watch_id": "w"}))


def test_bootstrap_noop_when_disabled_AC13(monkeypatch):
    from apps.api.services.poller import engine as eng
    monkeypatch.setattr(eng.settings, "INTENT_POLLER_ENABLED", False, raising=False)
    assert eng.bootstrap_watch_schedules() == 0
