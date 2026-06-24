"""WI-9 route-level tests — Stripe checkout + webhook + balance, fully offline.

A fake Stripe client is injected via ``set_stripe_client`` so no real Stripe API
call is ever made. The webhook is exercised with a hand-built fake event.
"""
import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.config import settings
from apps.api.core.tenancy import WorkspaceCtx, current_workspace
from apps.api.database import Base, get_db
from apps.api.services.billing.models import WorkspaceCredit, CreditLedgerEntry
from apps.api.services.billing import service as billing
from apps.api.services.billing import stripe_client
from apps.api.routers.billing import router as billing_router


WS = "ws_route"


class FakeStripe:
    """In-memory stand-in for the Stripe SDK wrapper."""

    def __init__(self):
        self.sessions = []

    def create_checkout_session(self, *, amount_usd, workspace_id, success_url, cancel_url):
        sid = f"sess_{len(self.sessions) + 1}"
        self.sessions.append((sid, amount_usd, workspace_id))
        return {"id": sid, "url": f"https://stripe.test/{sid}"}

    def construct_event(self, payload, sig_header):
        # Tests pass the already-parsed event JSON in via a closure; here we
        # simply trust the body (signature verification is the SDK's job, which
        # is exactly what we're stubbing). Return the parsed dict.
        import json
        return json.loads(payload)


@pytest.fixture()
def client(monkeypatch):
    monkeypatch.setattr(settings, "BILLING_ENABLED", True, raising=False)

    # Single shared in-memory connection so the schema created here is visible to
    # every session opened by the request-scoped get_db override.
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(
        engine, tables=[WorkspaceCredit.__table__, CreditLedgerEntry.__table__]
    )
    Session = sessionmaker(bind=engine)

    fake = FakeStripe()
    stripe_client.set_stripe_client(fake)

    app = FastAPI()
    app.include_router(billing_router)

    def _override_db():
        s = Session()
        try:
            yield s
        finally:
            s.close()

    def _override_ws():
        return WorkspaceCtx(user=None, workspace_id=WS, slug="route")

    app.dependency_overrides[get_db] = _override_db
    app.dependency_overrides[current_workspace] = _override_ws

    try:
        yield TestClient(app), Session, fake
    finally:
        stripe_client.set_stripe_client(None)


def test_balance_endpoint_empty(client):
    tc, _, _ = client
    r = tc.get("/api/billing/balance")
    assert r.status_code == 200
    body = r.json()
    assert body["billing_enabled"] is True
    assert body["balance_usd"] == 0.0
    assert body["entries"] == []


def test_checkout_returns_session_url(client):
    tc, _, fake = client
    r = tc.post("/api/billing/checkout", json={"amount_usd": 25.0})
    assert r.status_code == 200
    body = r.json()
    assert body["session_id"] == "sess_1"
    assert body["url"].endswith("sess_1")
    assert fake.sessions[0][1] == 25.0


def test_webhook_credits_ledger(client):
    tc, Session, _ = client
    event = {
        "type": "checkout.session.completed",
        "data": {
            "object": {
                "id": "sess_paid_1",
                "payment_status": "paid",
                "metadata": {"workspace_id": WS, "credit_usd": "30.0"},
            }
        },
    }
    r = tc.post("/api/billing/webhook", json=event,
                headers={"stripe-signature": "ignored-by-fake"})
    assert r.status_code == 200
    body = r.json()
    assert body["status"] == "ok"
    assert body["credited_usd"] == 30.0
    assert body["balance_usd"] == 30.0

    # Balance now reflects the top-up.
    assert tc.get("/api/billing/balance").json()["balance_usd"] == 30.0


def test_webhook_idempotent(client):
    tc, _, _ = client
    event = {
        "type": "checkout.session.completed",
        "data": {"object": {
            "id": "sess_dup",
            "payment_status": "paid",
            "metadata": {"workspace_id": WS, "credit_usd": "15.0"},
        }},
    }
    h = {"stripe-signature": "x"}
    tc.post("/api/billing/webhook", json=event, headers=h)
    second = tc.post("/api/billing/webhook", json=event, headers=h).json()
    assert second["idempotent_replay"] is True
    assert second["credited_usd"] == 0.0
    assert tc.get("/api/billing/balance").json()["balance_usd"] == 15.0


def test_webhook_ignores_non_checkout_event(client):
    tc, _, _ = client
    event = {"type": "payment_intent.created", "data": {"object": {}}}
    r = tc.post("/api/billing/webhook", json=event, headers={"stripe-signature": "x"})
    assert r.status_code == 200
    assert r.json()["status"] == "ignored"
