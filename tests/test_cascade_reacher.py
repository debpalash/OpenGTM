"""
Cascade integration with Reacher + the per-email cache:
  - Reacher (front) definitive wins; SMTP not called.
  - Reacher UNKNOWN / error → falls through to SMTP.
  - Cache hit avoids a second verifier call.
  - Flag OFF → chain is byte-identical to today (no reacher, no cache).
  - Deliverability mapping for the Reacher verdicts.
"""
import asyncio

import apps.api.services.leadgen.enrichment.email_verify_cascade as cascade
import apps.api.services.leadgen.enrichment.email_verify_cache as evc
import apps.api.services.leadgen.enrichment.email_deliverability as deliver
from apps.api.services.leadgen.enrichment.email_verify_cascade import (
    VerifyResult, Verifier, VALID, INVALID, CATCH_ALL, UNKNOWN, _CONFIDENCE,
)


class _Stub(Verifier):
    def __init__(self, name, status, *, detail=""):
        self.name = name
        self._status = status
        self._detail = detail
        self.calls = 0

    def is_available(self):
        return True

    async def verify(self, email):
        self.calls += 1
        return VerifyResult(email, self._status, self.name, _CONFIDENCE[self._status], self._detail)


def _wire(monkeypatch, tmp_path, chain):
    """Force verify_email's default path to use `chain` + a throwaway cache."""
    monkeypatch.setattr(cascade, "_bootstrap_verifiers", lambda ws=None: chain)
    cache = evc.EmailVerifyCache(db_path=str(tmp_path / "vc.db"))
    monkeypatch.setattr(evc, "get_verify_cache", lambda: cache)
    return cache


def test_reacher_definitive_wins(monkeypatch, tmp_path):
    reacher = _Stub("reacher", VALID)
    smtp = _Stub("smtp", INVALID)
    _wire(monkeypatch, tmp_path, [reacher, smtp])
    r = asyncio.run(cascade.verify_email("a@b.com"))
    assert r.status == VALID and r.source == "reacher"
    assert smtp.calls == 0  # definitive Reacher stopped the cascade


def test_reacher_unknown_falls_through_to_smtp(monkeypatch, tmp_path):
    reacher = _Stub("reacher", UNKNOWN)
    smtp = _Stub("smtp", VALID)
    _wire(monkeypatch, tmp_path, [reacher, smtp])
    r = asyncio.run(cascade.verify_email("a@b.com"))
    assert r.status == VALID and r.source == "smtp"
    assert reacher.calls == 1 and smtp.calls == 1


def test_reacher_raise_is_swallowed_and_cascades(monkeypatch, tmp_path):
    class _Boom(Verifier):
        name = "reacher"
        calls = 0
        def is_available(self): return True
        async def verify(self, email):
            type(self).calls += 1
            raise RuntimeError("dead")
    smtp = _Stub("smtp", VALID)
    _wire(monkeypatch, tmp_path, [_Boom(), smtp])
    r = asyncio.run(cascade.verify_email("a@b.com"))
    assert r.status == VALID and r.source == "smtp"


def test_cache_hit_avoids_second_call(monkeypatch, tmp_path):
    reacher = _Stub("reacher", VALID)
    _wire(monkeypatch, tmp_path, [reacher])
    r1 = asyncio.run(cascade.verify_email("dup@b.com"))
    r2 = asyncio.run(cascade.verify_email("dup@b.com"))
    assert r1.status == VALID and r2.status == VALID
    assert reacher.calls == 1  # second call served from cache


def test_transient_unknown_not_cached(monkeypatch, tmp_path):
    # An infra-error UNKNOWN (detail="error") must NOT be cached, so it self-heals.
    reacher = _Stub("reacher", UNKNOWN, detail="error")
    _wire(monkeypatch, tmp_path, [reacher])
    asyncio.run(cascade.verify_email("flap@b.com"))
    asyncio.run(cascade.verify_email("flap@b.com"))
    assert reacher.calls == 2  # not cached → re-called


def test_clean_unknown_is_cached(monkeypatch, tmp_path):
    # A provider "unknown" (e.g. gmail, detail="unknown") IS cached briefly.
    reacher = _Stub("reacher", UNKNOWN, detail="unknown")
    _wire(monkeypatch, tmp_path, [reacher])
    asyncio.run(cascade.verify_email("gmail@b.com"))
    asyncio.run(cascade.verify_email("gmail@b.com"))
    assert reacher.calls == 1


def test_flag_off_chain_is_byte_identical(monkeypatch):
    """With Reacher unavailable, the chain == available_verifiers(): no reacher,
    and the default path engages no cache (byte-for-byte today's behavior)."""
    monkeypatch.delenv("REACHER_ENABLED", raising=False)
    chain = cascade._bootstrap_verifiers(None)
    names = [v.name for v in chain]
    assert "reacher" not in names
    assert names == [v.name for v in cascade.available_verifiers()]
    assert cascade._reacher_active(chain) is False


# ── deliverability mapping for Reacher verdicts ──────────────────────────────

def _stub_verdict(monkeypatch, status, detail=""):
    async def fake(email, *, verifiers=None, workspace_id=None):
        return VerifyResult(email, status, "reacher", _CONFIDENCE[status], detail)
    monkeypatch.setattr(cascade, "verify_email", fake)


def test_deliver_catch_all_is_risky(monkeypatch):
    _stub_verdict(monkeypatch, CATCH_ALL)
    res = asyncio.run(deliver.verify_deliverability("person@corp.com"))
    assert res.confidence == deliver.RISKY and res.catch_all


def test_deliver_valid_nonrole_is_verified(monkeypatch):
    _stub_verdict(monkeypatch, VALID)
    res = asyncio.run(deliver.verify_deliverability("jane@corp.com"))
    assert res.confidence == deliver.VERIFIED


def test_deliver_role_valid_demoted_to_risky(monkeypatch):
    _stub_verdict(monkeypatch, VALID)
    res = asyncio.run(deliver.verify_deliverability("info@corp.com"))
    assert res.is_role and res.confidence == deliver.RISKY


def test_deliver_invalid_is_dropped(monkeypatch):
    _stub_verdict(monkeypatch, INVALID)
    res = asyncio.run(deliver.verify_deliverability("ghost@corp.com"))
    assert res.confidence == "" and res.keep is False


def test_deliver_all_unknown_is_unknown_never_false_verified(monkeypatch):
    _stub_verdict(monkeypatch, UNKNOWN)
    res = asyncio.run(deliver.verify_deliverability("maybe@corp.com"))
    assert res.confidence == deliver.UNKNOWN
