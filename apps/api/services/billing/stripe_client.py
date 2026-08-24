"""Stripe client abstraction (WI-9).

The router never imports ``stripe`` directly — it goes through a small protocol
so tests can inject a fake and we NEVER hit the real Stripe API offline.

Two operations are needed:
  * ``create_checkout_session(...)`` — start a hosted top-up checkout.
  * ``construct_event(payload, sig_header)`` — verify + parse a webhook event.

``get_stripe_client()`` returns the real implementation (lazy-importing the
``stripe`` package and reading keys from config) unless a client has been
injected via :func:`set_stripe_client` (used by tests).
"""

from __future__ import annotations

from typing import Any, Optional, Protocol

from apps.api.core.config import settings


class StripeClient(Protocol):
    def create_checkout_session(
        self, *, amount_usd: float, workspace_id: str, success_url: str, cancel_url: str
    ) -> dict:
        """Return at least {"id": <session_id>, "url": <checkout_url>}."""

    def construct_event(self, payload: bytes, sig_header: Optional[str]) -> dict:
        """Verify the webhook signature and return the parsed event dict."""


class RealStripeClient:
    """Thin wrapper over the official ``stripe`` SDK."""

    def __init__(self, api_key: str, webhook_secret: str):
        import stripe  # lazy: only needed when billing actually talks to Stripe

        self._stripe = stripe
        self._stripe.api_key = api_key
        self._webhook_secret = webhook_secret

    def create_checkout_session(
        self, *, amount_usd: float, workspace_id: str, success_url: str, cancel_url: str
    ) -> dict:
        session = self._stripe.checkout.Session.create(
            mode="payment",
            line_items=[
                {
                    "price_data": {
                        "currency": "usd",
                        "unit_amount": int(round(amount_usd * 100)),
                        "product_data": {"name": "OpenGTM enrichment credits"},
                    },
                    "quantity": 1,
                }
            ],
            metadata={"workspace_id": workspace_id, "credit_usd": str(amount_usd)},
            success_url=success_url,
            cancel_url=cancel_url,
        )
        return {"id": session.id, "url": session.url}

    def construct_event(self, payload: bytes, sig_header: Optional[str]) -> dict:
        event = self._stripe.Webhook.construct_event(
            payload, sig_header, self._webhook_secret
        )
        # Newer SDK returns a StripeObject; normalize to a plain dict.
        return dict(event)


# ── Injection seam ───────────────────────────────────────────────────────────

_INJECTED: Optional[StripeClient] = None


def set_stripe_client(client: Optional[StripeClient]) -> None:
    """Inject a (fake) client. Pass ``None`` to reset to the real one."""
    global _INJECTED
    _INJECTED = client


def get_stripe_client() -> StripeClient:
    if _INJECTED is not None:
        return _INJECTED
    api_key = getattr(settings, "STRIPE_SECRET_KEY", "") or ""
    webhook_secret = getattr(settings, "STRIPE_WEBHOOK_SECRET", "") or ""
    if not api_key:
        raise RuntimeError(
            "Stripe is not configured: set STRIPE_SECRET_KEY (and "
            "STRIPE_WEBHOOK_SECRET) in the environment."
        )
    return RealStripeClient(api_key, webhook_secret)
