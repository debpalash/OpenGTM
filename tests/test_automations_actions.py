"""Trigger Engine — action executor tests (SSRF guard, CRM, webhook). AC-10.

External HTTP is never actually sent in the blocked cases; the SSRF guard rejects
before connect. The success path patches the pinned-send helper.
"""

import asyncio

import pytest

from apps.api.core.config import settings
from apps.api.core.url_guard import BlockedUrlError
from apps.api.services.automations import actions as actmod


def _run(coro):
    return asyncio.run(coro)


# ── AC-10: SSRF / DNS-pin guard ─────────────────────────────────────────────

def test_validate_and_pin_blocks_loopback():
    with pytest.raises(BlockedUrlError):
        actmod._validate_and_pin("http://127.0.0.1/hook")


def test_validate_and_pin_blocks_metadata_ip():
    with pytest.raises(BlockedUrlError):
        actmod._validate_and_pin("http://169.254.169.254/latest/meta-data/")


def test_validate_and_pin_blocks_private_literal():
    with pytest.raises(BlockedUrlError):
        actmod._validate_and_pin("http://10.0.0.5/x")


def test_validate_and_pin_blocks_octal_smuggle():
    with pytest.raises(BlockedUrlError):
        actmod._validate_and_pin("http://0177.0.0.1/x")  # 127.0.0.1 in octal


def test_validate_and_pin_blocks_credentials_and_scheme():
    with pytest.raises(BlockedUrlError):
        actmod._validate_and_pin("ftp://example.com/x")
    with pytest.raises(BlockedUrlError):
        actmod._validate_and_pin("http://user:pass@example.com/x")


def test_validate_and_pin_blocks_rebind_when_any_record_private(monkeypatch):
    """A host that resolves to BOTH a public and a private IP is blocked (the
    rebinding guard validates EVERY resolved record)."""
    import apps.api.services.automations.actions as a

    def fake_getaddrinfo(host, port):
        return [
            (2, 1, 6, "", ("93.184.216.34", port)),   # public
            (2, 1, 6, "", ("169.254.169.254", port)),  # private/metadata
        ]

    monkeypatch.setattr(a.socket, "getaddrinfo", fake_getaddrinfo)
    with pytest.raises(BlockedUrlError):
        a._validate_and_pin("http://rebind.example.com/x")


def test_validate_and_pin_allows_public(monkeypatch):
    import apps.api.services.automations.actions as a

    def fake_getaddrinfo(host, port):
        return [(2, 1, 6, "", ("93.184.216.34", port))]

    monkeypatch.setattr(a.socket, "getaddrinfo", fake_getaddrinfo)
    pinned, host, port, is_https = a._validate_and_pin("https://example.com/x")
    assert pinned == "93.184.216.34" and host == "example.com" and is_https is True


def test_webhook_domain_allowlist(monkeypatch):
    import apps.api.services.automations.actions as a

    def fake_getaddrinfo(host, port):
        return [(2, 1, 6, "", ("93.184.216.34", port))]

    monkeypatch.setattr(a.socket, "getaddrinfo", fake_getaddrinfo)
    monkeypatch.setattr(settings, "AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST",
                        ["hooks.allowed.com"], raising=False)
    # not on allowlist → blocked
    with pytest.raises(BlockedUrlError):
        a._validate_and_pin("https://example.com/x")
    # on allowlist (subdomain ok) → allowed
    a._validate_and_pin("https://api.hooks.allowed.com/x")


def test_act_webhook_blocked_url_returns_failed():
    """A blocked URL → ActionResult failed with skip_reason blocked_url (no send)."""
    res = _run(actmod._act_webhook(
        "ws", {"url": "http://169.254.169.254/x"}, {"company": "Acme"}, [],
    ))
    assert res.status == "failed" and res.skip_reason == "blocked_url"


def test_act_webhook_success(monkeypatch):
    async def fake_send(url, method, headers, kwargs):
        return {"success": True, "value": "POST 200", "error": None}

    monkeypatch.setattr(actmod, "_validate_and_pin", lambda url: ("1.2.3.4", "h", 443, True))
    monkeypatch.setattr(actmod, "_send_webhook_pinned", fake_send)
    res = _run(actmod._act_webhook("ws", {"url": "https://ok.test/x"}, {"a": 1}, []))
    assert res.status == "success" and "200" in res.summary


# ── CRM executor (mocked output) ────────────────────────────────────────────

def test_act_push_crm_not_connected(monkeypatch):
    from apps.api.services.workbook import output

    async def fake_exec(col_config, lead_data, cols, workbook_id, lead_id, workspace_id=None):
        return {"success": False, "value": "", "error": "HubSpot not connected"}

    monkeypatch.setattr(output, "execute_output_column", fake_exec)
    res = _run(actmod._act_push_crm("ws", {"type": "hubspot"}, {"company": "Acme"}, [], 1))
    assert res.status == "failed" and res.skip_reason == "not_connected"


def test_act_push_crm_success(monkeypatch):
    from apps.api.services.workbook import output

    async def fake_exec(col_config, lead_data, cols, workbook_id, lead_id, workspace_id=None):
        return {"success": True, "value": "HubSpot: created 123", "error": None}

    monkeypatch.setattr(output, "execute_output_column", fake_exec)
    res = _run(actmod._act_push_crm("ws", {"type": "hubspot"}, {"company": "Acme"}, [], 1))
    assert res.status == "success" and "HubSpot" in res.summary


# ── re_enrich no-target skip ─────────────────────────────────────────────────

def test_act_re_enrich_no_target_skips():
    res = _run(actmod._act_re_enrich("ws", {"column_ids": ["nope"]}, "wb1", "5",
                                     [{"id": "other"}]))
    assert res.status == "skipped" and res.skip_reason == "no_target"
