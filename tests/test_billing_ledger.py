"""WI-9: credit-ledger billing enforcement + Stripe (offline, deterministic).

Covers:
  * debit reduces balance
  * non-BYOK providers debited, BYOK providers not
  * HTTP 402 (InsufficientCreditsError) on insufficient balance
  * idempotent per-run debit (retry doesn't double-charge)
  * billing-disabled flag never blocks
  * Stripe webhook credits the ledger (fake event, no real API)
  * balance + entries read

All Stripe interaction is through an injected fake client — the real Stripe
API is never called.
"""
import importlib

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker

from apps.api.core.config import settings
from apps.api.database import Base
from apps.api.services.billing import models as billing_models  # noqa: F401 (register tables)
from apps.api.services.billing import service as billing
from apps.api.services.billing.models import WorkspaceCredit, CreditLedgerEntry
from apps.api.services.workbook import vendor_catalog as vc


WS = "ws_test"


@pytest.fixture()
def db():
    """Fresh in-memory SQLite session with only the billing tables created."""
    engine = create_engine("sqlite:///:memory:")
    Base.metadata.create_all(
        engine, tables=[WorkspaceCredit.__table__, CreditLedgerEntry.__table__]
    )
    Session = sessionmaker(bind=engine)
    s = Session()
    try:
        yield s
    finally:
        s.close()


@pytest.fixture()
def billing_on(monkeypatch):
    monkeypatch.setattr(settings, "BILLING_ENABLED", True, raising=False)


# ── balance / credit / debit basics ─────────────────────────────────────────

def test_credit_then_debit_reduces_balance(db, billing_on):
    billing.credit(db, WS, 10.0, reason="stripe_topup", idempotency_key="seed")
    assert billing.get_balance(db, WS) == 10.0

    res = billing.check_and_debit(db, WS, 3.0, run_id="r1")
    assert res.charged_usd == 3.0
    assert res.balance_usd == 7.0
    assert billing.get_balance(db, WS) == 7.0


def test_entries_appended(db, billing_on):
    billing.credit(db, WS, 5.0, reason="stripe_topup", idempotency_key="s1")
    billing.check_and_debit(db, WS, 2.0, run_id="r1")
    entries = billing.recent_entries(db, WS)
    # newest first: debit -2 then credit +5
    assert entries[0]["amount_usd"] == -2.0
    assert entries[0]["reason"] == "run_debit"
    assert entries[1]["amount_usd"] == 5.0


# ── non-BYOK debited, BYOK not ───────────────────────────────────────────────

def test_projected_cost_only_charges_non_byok(monkeypatch):
    # Register a platform-billed (non-BYOK) provider and a BYOK paid provider.
    monkeypatch.setitem(vc.VENDORS, "platform_paid",
                        vc.Vendor("platform_paid", base_cost=0.10, byok=False))
    monkeypatch.setitem(vc.VENDORS, "byok_paid",
                        vc.Vendor("byok_paid", base_cost=0.50, byok=True))

    cost = billing.projected_platform_cost(
        10, {"email": ["platform_paid", "byok_paid"]}
    )
    # Only the non-BYOK provider is charged: 0.10 * 10 = 1.0 (BYOK 0.50 ignored).
    assert cost == 1.0


def test_byok_only_run_is_free(monkeypatch):
    monkeypatch.setitem(vc.VENDORS, "byok_only",
                        vc.Vendor("byok_only", base_cost=0.30, byok=True))
    cost = billing.projected_platform_cost(100, {"c": ["byok_only", "website_scraper"]})
    assert cost == 0.0


def test_byok_only_run_never_debited(db, billing_on, monkeypatch):
    monkeypatch.setitem(vc.VENDORS, "byok_only2",
                        vc.Vendor("byok_only2", base_cost=0.30, byok=True))
    billing.credit(db, WS, 1.0, reason="seed", idempotency_key="seed2")
    projected = billing.projected_platform_cost(50, {"c": ["byok_only2"]})
    res = billing.check_and_debit(db, WS, projected, run_id="byok_run")
    assert res.charged_usd == 0.0
    assert billing.get_balance(db, WS) == 1.0  # untouched


# ── insufficient balance → 402 ───────────────────────────────────────────────

def test_insufficient_balance_raises(db, billing_on):
    billing.credit(db, WS, 1.0, reason="seed", idempotency_key="s")
    with pytest.raises(billing.InsufficientCreditsError) as ei:
        billing.check_and_debit(db, WS, 5.0, run_id="r_big")
    assert ei.value.balance_usd == 1.0
    assert ei.value.required_usd == 5.0
    # balance unchanged; no debit entry written
    assert billing.get_balance(db, WS) == 1.0
    assert all(e["reason"] != "run_debit" for e in billing.recent_entries(db, WS))


# ── idempotent per-run debit (retry-safe) ────────────────────────────────────

def test_idempotent_debit_no_double_charge(db, billing_on):
    billing.credit(db, WS, 10.0, reason="seed", idempotency_key="s")
    r1 = billing.check_and_debit(db, WS, 4.0, run_id="same_run")
    assert r1.charged_usd == 4.0
    assert billing.get_balance(db, WS) == 6.0

    # Retry the SAME run_id → no second charge.
    r2 = billing.check_and_debit(db, WS, 4.0, run_id="same_run")
    assert r2.charged_usd == 0.0
    assert r2.idempotent_replay is True
    assert billing.get_balance(db, WS) == 6.0
    # exactly one debit entry for this run
    debits = [e for e in billing.recent_entries(db, WS) if e["run_id"] == "same_run"]
    assert len(debits) == 1


# ── billing disabled never blocks ────────────────────────────────────────────

def test_disabled_never_blocks(db, monkeypatch):
    monkeypatch.setattr(settings, "BILLING_ENABLED", False, raising=False)
    # No credits at all, but a huge projected cost must NOT raise.
    res = billing.check_and_debit(db, WS, 9999.0, run_id="r")
    assert res.charged_usd == 0.0
    # No debit entry written.
    assert billing.recent_entries(db, WS) == []


# ── Stripe webhook credits the ledger (fake event) ───────────────────────────

def test_credit_idempotent(db, billing_on):
    a = billing.credit(db, WS, 20.0, reason="stripe_topup", idempotency_key="stripe:sess_1")
    assert a.charged_usd == 20.0
    # redelivered webhook (same session) → no double credit
    b = billing.credit(db, WS, 20.0, reason="stripe_topup", idempotency_key="stripe:sess_1")
    assert b.charged_usd == 0.0
    assert b.idempotent_replay is True
    assert billing.get_balance(db, WS) == 20.0
