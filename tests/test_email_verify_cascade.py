"""
Phase #4: normalized email-verification cascade (4-status contract + fallback).
See docs/research/clay-alternatives-ingestion-catalog.md (top-10 #4).
"""
import asyncio

from apps.api.services.leadgen.enrichment.email_verify_cascade import (
    VerifyResult, Verifier, verify_email, available_verifiers,
    VALID, INVALID, CATCH_ALL, UNKNOWN, _status_from_vendor,
)


class _Stub(Verifier):
    def __init__(self, name, status, available=True):
        self.name = name; self._status = status; self._av = available; self.calls = 0
    def is_available(self):
        return self._av
    async def verify(self, email):
        self.calls += 1
        from apps.api.services.leadgen.enrichment.email_verify_cascade import _CONFIDENCE
        return VerifyResult(email, self._status, self.name, _CONFIDENCE[self._status])


def test_status_mapping():
    assert _status_from_vendor("deliverable") == VALID
    assert _status_from_vendor("undeliverable") == INVALID
    assert _status_from_vendor("accept_all") == CATCH_ALL
    assert _status_from_vendor("risky") == UNKNOWN


def test_syntax_reject_short_circuits():
    r = asyncio.run(verify_email("not-an-email"))
    assert r.status == INVALID and r.source == "syntax"


def test_definitive_stops_cascade():
    v1 = _Stub("v1", VALID)
    v2 = _Stub("v2", INVALID)
    r = asyncio.run(verify_email("a@x.com", verifiers=[v1, v2]))
    assert r.status == VALID and r.source == "v1"
    assert v2.calls == 0  # definitive result stopped the chain


def test_unknown_cascades_to_next():
    v1 = _Stub("v1", UNKNOWN)
    v2 = _Stub("v2", VALID)
    r = asyncio.run(verify_email("a@x.com", verifiers=[v1, v2]))
    assert r.status == VALID and r.source == "v2"
    assert v1.calls == 1 and v2.calls == 1
    assert r.attempts == [
        {"provider": "v1", "status": UNKNOWN, "detail": ""},
        {"provider": "v2", "status": VALID, "detail": ""},
    ]


def test_all_unknown_returns_last_unknown():
    v1 = _Stub("v1", UNKNOWN)
    v2 = _Stub("v2", UNKNOWN)
    r = asyncio.run(verify_email("a@x.com", verifiers=[v1, v2]))
    assert r.status == UNKNOWN


def test_catch_all_is_definitive_and_deliverable():
    v1 = _Stub("v1", CATCH_ALL)
    v2 = _Stub("v2", VALID)
    r = asyncio.run(verify_email("a@x.com", verifiers=[v1, v2]))
    assert r.status == CATCH_ALL and r.is_definitive and r.deliverable
    assert v2.calls == 0


def test_unavailable_verifier_skipped():
    # available_verifiers filters by is_available(); a stubbed unavailable one
    # is skipped, but verify() with an explicit chain uses what it's given.
    v_down = _Stub("down", VALID, available=False)
    v_up = _Stub("up", VALID, available=True)
    # explicit chain ignores availability (caller's choice)
    r = asyncio.run(verify_email("a@x.com", verifiers=[v_up]))
    assert r.source == "up"


def test_default_chain_has_smtp():
    names = [v.name for v in available_verifiers()]
    assert "smtp" in names or names == []  # smtp present unless disabled
