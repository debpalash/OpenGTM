"""Billing — credit-ledger enforcement (WI-9).

A thin ENFORCEMENT layer on top of the economics that `vendor_catalog` already
computes. Before a run, the catalog says "N rows × providers = $X"; billing
debits the platform-billed (non-BYOK) portion of that from a per-workspace
credit balance, blocking the run with HTTP 402 when there aren't enough credits.
BYOK providers (user's own API key) are never charged.

Everything is gated behind the ``BILLING_ENABLED`` feature flag (default off) so
existing self-host deployments are completely unaffected.
"""
