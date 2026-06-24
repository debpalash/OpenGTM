"""Billing API router (WI-9) — credit balance, Stripe top-ups, webhook.

All endpoints except the webhook are workspace-scoped via ``current_workspace``.
The webhook is unauthenticated (Stripe calls it) but verifies the event
signature through the abstracted Stripe client.
"""

import logging

from fastapi import APIRouter, Depends, HTTPException, Request
from pydantic import BaseModel, Field
from sqlalchemy.orm import Session

from apps.api.core.config import settings
from apps.api.core.tenancy import WorkspaceCtx, current_workspace
from apps.api.database import get_db
from apps.api.services.billing import service as billing
from apps.api.services.billing.stripe_client import get_stripe_client

logger = logging.getLogger("billing.api")
router = APIRouter(prefix="/api/billing", tags=["billing"])


# ── Balance + ledger ─────────────────────────────────────────────────────────

@router.get("/balance")
def get_balance(
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Current credit balance + recent ledger entries for the active workspace."""
    return {
        "billing_enabled": billing.billing_enabled(),
        "workspace_id": ctx.workspace_id,
        "balance_usd": billing.get_balance(db, ctx.workspace_id),
        "entries": billing.recent_entries(db, ctx.workspace_id, limit=50),
    }


# ── Stripe checkout (top-up) ─────────────────────────────────────────────────

class CheckoutRequest(BaseModel):
    amount_usd: float = Field(..., gt=0, description="USD of credit to purchase")
    success_url: str = "https://app.yupcha.com/billing?status=success"
    cancel_url: str = "https://app.yupcha.com/billing?status=cancel"


@router.post("/checkout")
def create_checkout(
    body: CheckoutRequest,
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Create a Stripe Checkout session to buy credits. Returns the hosted URL."""
    if not billing.billing_enabled():
        raise HTTPException(status_code=400, detail="Billing is disabled on this deployment.")
    try:
        client = get_stripe_client()
    except RuntimeError as e:
        raise HTTPException(status_code=503, detail=str(e))

    session = client.create_checkout_session(
        amount_usd=body.amount_usd,
        workspace_id=ctx.workspace_id,
        success_url=body.success_url,
        cancel_url=body.cancel_url,
    )
    return {"session_id": session.get("id"), "url": session.get("url")}


# ── Stripe webhook (credits the ledger on payment) ───────────────────────────

@router.post("/webhook")
async def stripe_webhook(request: Request, db: Session = Depends(get_db)):
    """Stripe webhook. On a completed checkout, credit the workspace's ledger.

    Idempotent: keyed on the Stripe session id so a redelivered event won't
    double-credit.
    """
    if not billing.billing_enabled():
        # Don't 500 if Stripe retries against a disabled deployment.
        return {"status": "ignored", "reason": "billing_disabled"}

    payload = await request.body()
    sig = request.headers.get("stripe-signature")
    try:
        client = get_stripe_client()
        event = client.construct_event(payload, sig)
    except RuntimeError as e:
        raise HTTPException(status_code=503, detail=str(e))
    except Exception as e:  # signature/parse failure
        logger.warning("Stripe webhook verification failed: %s", e)
        raise HTTPException(status_code=400, detail="Invalid webhook signature")

    event_type = event.get("type")
    if event_type != "checkout.session.completed":
        return {"status": "ignored", "type": event_type}

    obj = (event.get("data") or {}).get("object") or {}
    # Only credit on a paid session.
    if obj.get("payment_status") not in (None, "paid"):
        return {"status": "ignored", "reason": "unpaid"}

    metadata = obj.get("metadata") or {}
    workspace_id = metadata.get("workspace_id")
    session_id = obj.get("id")
    try:
        credit_usd = float(metadata.get("credit_usd"))
    except (TypeError, ValueError):
        credit_usd = 0.0

    if not workspace_id or credit_usd <= 0 or not session_id:
        logger.warning("Stripe webhook missing workspace_id/credit_usd/session id: %s", obj)
        raise HTTPException(status_code=400, detail="Malformed checkout session")

    result = billing.credit(
        db,
        workspace_id=workspace_id,
        amount_usd=credit_usd,
        reason="stripe_topup",
        idempotency_key=f"stripe:{session_id}",
    )
    return {
        "status": "ok",
        "credited_usd": result.charged_usd,
        "balance_usd": result.balance_usd,
        "idempotent_replay": result.idempotent_replay,
    }
