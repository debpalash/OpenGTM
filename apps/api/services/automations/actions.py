"""Action executors for the trigger engine (§3.5 / §5 / §6 / §7).

v1 LOCKED SCOPE action types: ``re_enrich``, ``push_crm``, ``webhook``.
``sequencer`` / ``send_email`` are rejected at rule-create and not executed here.

Each executor returns an :class:`ActionResult`. Paid-action billing, idempotency
markers and cap reservations are orchestrated by ``engine.py``; this module
performs the side effect and reports its outcome (it does NOT debit).

Webhook sends are SSRF-guarded with a DNS-PINNED connect (§6.2): the host is
resolved once, every resolved A/AAAA record is validated as public, and the
socket connects to the validated IP with the ``Host`` header / SNI preserved —
closing the TOCTOU/rebinding hole the static guard leaves open. Redirects are
disabled (``follow_redirects=False``).
"""

from __future__ import annotations

import ipaddress
import json
import logging
import socket
from dataclasses import dataclass, field
from typing import Any, Optional
from urllib.parse import urlparse

import httpx

from apps.api.core.config import settings
from apps.api.core.url_guard import BlockedUrlError, _ip_is_blocked, _parse_ip_any_encoding, _BLOCKED_HOSTNAMES

logger = logging.getLogger("automations.actions")

# Action types whose external effect is idempotent (safe to re-attempt after a
# crash between debit and send). CRM upsert keys on the contact; re_enrich is
# guarded by the workbook-budget pre-send marker.
IDEMPOTENT_ACTION_TYPES = {"push_crm", "re_enrich"}
# Non-idempotent: at-most-once via the in_flight pre-send marker. send_email is
# at-most-once via the committed in_flight marker on outreach_sends (§6.2.1).
NON_IDEMPOTENT_ACTION_TYPES = {"webhook", "send_email"}


@dataclass
class ActionResult:
    status: str                       # success / failed / skipped
    summary: str = ""
    error: Optional[str] = None
    skip_reason: Optional[str] = None
    charged_usd: float = 0.0


# ── cost projection (§7) ────────────────────────────────────────────────────

def _providers_by_column(cfg_column_ids: list, columns_config: list) -> dict:
    """Build providers_by_column EXACTLY like routers/workbooks.py:758-766 —
    a column's waterfall, else [provider], else DEFAULT_WATERFALLS[target]."""
    from apps.api.services.workbook.enrichment import DEFAULT_WATERFALLS

    by_col: dict = {}
    by_id = {c.get("id"): c for c in (columns_config or [])}
    for cid in (cfg_column_ids or []):
        c = by_id.get(cid)
        if not c:
            # Column not present in this workbook → no projected cost (the action
            # will be skipped no_target at execution).
            continue
        target = c.get("target_field") or c.get("lead_field") or c.get("id")
        chain = c.get("waterfall") or ([c["provider"]] if c.get("provider") else [])
        if not chain:
            chain = DEFAULT_WATERFALLS.get(target, [])
        by_col[c.get("id") or target] = chain
    return by_col


def project_action_cost(action: dict, columns_config: list) -> float:
    """Worst-case platform-billed USD for ONE row's action. re_enrich uses the
    real waterfall projection; push_crm/webhook are 0 (no platform-billed spend)."""
    atype = action.get("type")
    if atype == "send_email":
        # The trigger-engine action only ENQUEUES a `send` job; metering is
        # debit-on-success inside handle_send (LOCKED SCOPE decision 3). So the
        # engine-level cost is 0 (enqueue is free) — charging here would be
        # debit-on-enqueue under a different run_id → double charge.
        return 0.0
    if atype != "re_enrich":
        return 0.0
    from apps.api.services.billing import service as billing

    cfg = action.get("config") or {}
    by_col = _providers_by_column(cfg.get("column_ids") or [], columns_config)
    return billing.projected_platform_cost(1, by_col)


# ── DNS-pinned webhook send (§6.2) ──────────────────────────────────────────

def _validate_and_pin(url: str) -> tuple[str, str, int, bool]:
    """Validate the URL host, resolve DNS, validate EVERY resolved IP, return a
    pinned (validated public IP, hostname, port, is_https). Raises BlockedUrlError."""
    parsed = urlparse(url)
    scheme = (parsed.scheme or "").lower()
    if scheme not in ("http", "https"):
        raise BlockedUrlError(f"scheme '{scheme}' not allowed")
    if parsed.username or parsed.password:
        raise BlockedUrlError("credentials in url not allowed")
    host = (parsed.hostname or "").lower()
    if not host:
        raise BlockedUrlError("no host")
    if host in _BLOCKED_HOSTNAMES:
        raise BlockedUrlError(f"host '{host}' is blocked")

    # optional egress allowlist (cloud)
    allowlist = list(getattr(settings, "AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST", []) or [])
    if allowlist:
        if not any(host == d.lower() or host.endswith("." + d.lower()) for d in allowlist):
            raise BlockedUrlError(f"host '{host}' not in webhook domain allowlist")

    is_https = scheme == "https"
    port = parsed.port or (443 if is_https else 80)

    # literal-IP host (any encoding) → validate directly, pin to it.
    lit = _parse_ip_any_encoding(host)
    if lit is not None:
        if _ip_is_blocked(lit):
            raise BlockedUrlError(f"ip '{lit}' is private/blocked")
        return str(lit), host, port, is_https

    try:
        infos = socket.getaddrinfo(host, port)
    except socket.gaierror as e:
        raise BlockedUrlError(f"cannot resolve host: {e}")
    pinned = None
    for info in infos:
        addr = info[4][0]
        try:
            ip = ipaddress.ip_address(addr)
        except ValueError:
            continue
        if _ip_is_blocked(ip):
            # ANY resolved record being private blocks the whole send (rebinding).
            raise BlockedUrlError(f"host resolves to private ip {addr}")
        if pinned is None:
            pinned = addr
    if pinned is None:
        raise BlockedUrlError("no public address resolved for host")
    return pinned, host, port, is_https


async def pinned_get(
    url: str, headers: Optional[dict] = None, *, method: str = "GET",
    timeout: float = 15.0, kwargs: Optional[dict] = None,
) -> httpx.Response:
    """One audited SSRF-pinned sender shared by webhooks AND the RSS fetcher.

    DNS is resolved + every record validated inside ``_validate_and_pin`` (ANY
    private A/AAAA blocks the whole send — DNS rebinding), then the request URL
    is rewritten to the validated, pinned IP so httpx connects to THAT IP (no
    second, attacker-controllable resolution), preserving the ``Host`` header
    for vhost routing and the original hostname for TLS SNI/verification.
    ``follow_redirects=False`` so a 30x to a private host is never chased.

    Raises :class:`BlockedUrlError` when the URL is unsafe. Returns the raw
    ``httpx.Response`` so callers can inspect status (e.g. 304) + headers
    (ETag / Last-Modified) for conditional GET. The webhook path wraps this.
    """
    pinned_ip, host, port, is_https = _validate_and_pin(url)

    parsed = urlparse(url)
    netloc_ip = f"[{pinned_ip}]" if ":" in pinned_ip else pinned_ip
    pinned_url = parsed._replace(netloc=f"{netloc_ip}:{port}").geturl()

    send_headers = dict(headers or {})
    send_headers["Host"] = host if port in (80, 443) else f"{host}:{port}"

    async with httpx.AsyncClient(timeout=timeout, follow_redirects=False, verify=True) as client:
        request = client.build_request(method, pinned_url, headers=send_headers, **(kwargs or {}))
        # Override SNI / TLS server hostname to the validated hostname (httpx
        # honours the sni_hostname extension for the TLS handshake).
        request.extensions = {**request.extensions, "sni_hostname": host}
        return await client.send(request)


async def _send_webhook_pinned(
    url: str, method: str, headers: dict, kwargs: dict
) -> dict:
    """Send a webhook via the shared :func:`pinned_get` audited sender.

    Connects ONLY to the validated, pinned IP with the ``Host``/SNI preserved and
    ``follow_redirects=False``. This closes the TOCTOU/DNS-rebinding window the
    static guard leaves open. Behaviour is unchanged from the prior inline impl.
    """
    resp = await pinned_get(url, headers, method=method, kwargs=kwargs)
    ok = 200 <= resp.status_code < 300
    return {
        "success": ok,
        "value": f"{method} {resp.status_code}",
        "error": None if ok else f"HTTP {resp.status_code}: {resp.text[:160]}",
    }


def _resolve_webhook_headers(cfg: dict, ws_id: str, lead_data: dict, columns_config: list) -> dict:
    from apps.api.services.workbook.output import _resolve

    headers = {
        k: _resolve(str(v), lead_data, columns_config)
        for k, v in (cfg.get("headers") or {}).items()
    }
    # header_secret_ref → resolve via get_secret and inject as Authorization.
    ref = cfg.get("header_secret_ref")
    if ref:
        from apps.api.services.workspace.secrets import get_secret

        secret = get_secret(ws_id, ref, "")
        if secret:
            header_name = cfg.get("header_name") or "Authorization"
            headers.setdefault(header_name, secret)
    return headers


async def _act_webhook(ws_id: str, cfg: dict, lead_data: dict, columns_config: list) -> ActionResult:
    from apps.api.services.workbook.output import _resolve, _resolve_deep

    raw_url = (cfg.get("url") or "").strip()
    if not raw_url:
        return ActionResult("failed", error="webhook url not configured")
    url = _resolve(raw_url, lead_data, columns_config)

    # DNS-pinned validation BEFORE any connect (post-resolution + rebinding).
    try:
        _validate_and_pin(url)
    except BlockedUrlError as e:
        return ActionResult("failed", error=f"blocked url: {e}", skip_reason="blocked_url")

    method = (cfg.get("method") or "POST").upper()
    headers = _resolve_webhook_headers(cfg, ws_id, lead_data, columns_config)

    kwargs: dict[str, Any] = {}
    if method in ("POST", "PUT", "PATCH"):
        raw_body = cfg.get("body")
        if raw_body is None:
            kwargs["json"] = lead_data
        elif isinstance(raw_body, (dict, list)):
            kwargs["json"] = _resolve_deep(raw_body, lead_data, columns_config)
        else:
            resolved = _resolve(str(raw_body), lead_data, columns_config)
            try:
                kwargs["json"] = json.loads(resolved)
            except (json.JSONDecodeError, TypeError):
                kwargs["content"] = resolved
                headers.setdefault("Content-Type", "text/plain")

    try:
        res = await _send_webhook_pinned(url, method, headers, kwargs)
    except BlockedUrlError as e:
        return ActionResult("failed", error=f"blocked url: {e}", skip_reason="blocked_url")
    except Exception as e:
        return ActionResult("failed", error=str(e)[:200])
    if res["success"]:
        return ActionResult("success", summary=res["value"])
    return ActionResult("failed", summary=res.get("value", ""), error=res.get("error"))


# ── push_crm (reuses output.execute_output_column) ──────────────────────────

async def _act_push_crm(ws_id: str, cfg: dict, lead_data: dict, columns_config: list, lead_id: int) -> ActionResult:
    from apps.api.services.workbook.output import execute_output_column

    col_config = {"destination": "crm", "destination_config": cfg}
    res = await execute_output_column(
        col_config, lead_data, columns_config,
        workbook_id="", lead_id=lead_id, workspace_id=ws_id,
    )
    if res.get("success"):
        return ActionResult("success", summary=res.get("value", ""))
    err = res.get("error") or "crm push failed"
    skip = "not_connected" if "not connected" in err.lower() else None
    return ActionResult("failed", error=err, skip_reason=skip)


# ── re_enrich (reuses run_workbook_enrichment) ──────────────────────────────

async def _act_re_enrich(ws_id: str, cfg: dict, workbook_id: str, row_id: str, columns_config: list) -> ActionResult:
    """Re-enrich the configured column UUIDs for ONE workbook row.

    The engine writes a committed ``in_flight`` marker BEFORE calling this, so a
    retry after a crash will find the marker and NOT re-invoke (preventing a
    double ``budget_spent_usd`` increment, §7). Here we just run + report."""
    column_ids = cfg.get("column_ids") or []
    if not column_ids:
        return ActionResult("skipped", skip_reason="no_target", summary="no column_ids")

    by_id = {c.get("id") for c in (columns_config or [])}
    present = [cid for cid in column_ids if cid in by_id]
    if not present:
        return ActionResult("skipped", skip_reason="no_target",
                            summary="none of the configured columns exist in this workbook")

    from apps.api.services.workbook.enrichment import run_workbook_enrichment

    try:
        # Single-row, bounded provider timeout (§4 nested-timeout safety).
        # Pass the tenant explicitly (OD-4) so the run scopes correctly even
        # though this already executes inside the engine's workspace_scope.
        result = await run_workbook_enrichment(
            workbook_id=workbook_id,
            column_ids=present,
            row_ids=[int(row_id)] if str(row_id).isdigit() else [row_id],
            provider_timeout=10.0,
            workspace_id=ws_id,
        )
    except Exception as e:
        return ActionResult("failed", error=str(e)[:200])

    if result.get("error"):
        return ActionResult("failed", error=str(result.get("error")))
    completed = result.get("completed", 0)
    errors = result.get("errors", 0)
    summary = f"re_enrich: {completed} cells completed, {errors} errors"
    # Surface a workbook-budget hit if the inner run reports it (§7).
    if result.get("budget_exceeded"):
        summary += " [workbook budget cap hit]"
    return ActionResult("success" if errors == 0 else "failed",
                        summary=summary,
                        error=None if errors == 0 else f"{errors} cell errors")


# ── dispatcher ──────────────────────────────────────────────────────────────

async def execute_action(
    ws_id: str,
    action: dict,
    workbook_id: str,
    row_id: str,
    lead_id: Optional[int],
    lead_data: dict,
    columns_config: list,
    idem: Optional[str] = None,
) -> ActionResult:
    """Execute one action against one row. Pure side effect — NO billing here.

    ``idem`` is the engine's per-action idempotency key; standalone trigger sends
    (no sequence step) reuse it so the engine action-result and the enqueued
    ``send`` job collapse to one logical send (§8.1).
    """
    atype = action.get("type")
    cfg = action.get("config") or {}
    if atype == "webhook":
        return await _act_webhook(ws_id, cfg, lead_data, columns_config)
    if atype == "push_crm":
        return await _act_push_crm(ws_id, cfg, lead_data, columns_config, lead_id or 0)
    if atype == "re_enrich":
        return await _act_re_enrich(ws_id, cfg, workbook_id, row_id, columns_config)
    if atype == "sequencer":
        return _act_sequencer(ws_id, cfg, lead_id, lead_data)
    if atype == "send_email":
        return _act_send_email(ws_id, cfg, lead_id, lead_data, idem)
    return ActionResult("failed", error=f"action type '{atype}' not supported",
                        skip_reason="not_connected")


# ── outreach executors (§1.4) ────────────────────────────────────────────────

def _act_sequencer(ws_id: str, cfg: dict, lead_id: Optional[int], lead_data: dict) -> ActionResult:
    """Enroll the row's lead into a sequence in THIS workspace (idempotent)."""
    from datetime import datetime, timezone

    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.outreach.normalize import normalize_email
    from apps.api.services.outreach.store import get_outreach_store

    seq_id = (cfg.get("sequence_id") or "").strip()
    if not seq_id:
        return ActionResult("failed", error="sequence_id not configured", skip_reason="not_connected")
    email = normalize_email((lead_data or {}).get("email", ""))
    if not email:
        return ActionResult("skipped", skip_reason="no_email", summary="lead has no email")
    try:
        with workspace_scope(ws_id):
            store = get_outreach_store(ws_id)
            if not store.sequence_exists(seq_id):
                return ActionResult("failed", error="sequence not found in workspace",
                                    skip_reason="not_connected")
            if store.is_suppressed(email):
                return ActionResult("skipped", skip_reason="suppressed", summary="suppressed")
            eid = store.enroll(
                seq_id, lead_id or 0, email,
                consent_source=f"trigger:{cfg.get('_trigger_id', '')}",
                consent_at=datetime.now(timezone.utc),
            )
        ok = eid is not None
        return ActionResult("success", summary="enrolled" if ok else "already enrolled")
    except Exception as e:
        return ActionResult("failed", error=str(e)[:200])


def _act_send_email(ws_id: str, cfg: dict, lead_id: Optional[int], lead_data: dict,
                    idem: Optional[str]) -> ActionResult:
    """Enqueue a ``send`` job (SMTP I/O happens on the durable queue, §1.4).

    When the action targets a sequence step, mint the canonical per-step key
    (``seq:``) so the ticker and this trigger collapse to one send (§8.2).
    Standalone sends reuse the engine's ``trig:`` key.
    """
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.outreach.normalize import normalize_email
    from apps.api.services.outreach.store import get_outreach_store
    from apps.api.services.outreach.sending import sequence_step_idem, _enqueue_if_absent

    email = normalize_email((lead_data or {}).get("email", ""))
    if not email:
        return ActionResult("skipped", skip_reason="no_email", summary="lead has no email")
    seq_id = (cfg.get("sequence_id") or "").strip() or None
    step_number = int(cfg.get("step_number", 0) or 0)
    try:
        with workspace_scope(ws_id):
            store = get_outreach_store(ws_id)
            enrollment_id = None
            send_idem = idem or f"trig:{ws_id}:adhoc:{lead_id}:{step_number}"
            if seq_id:
                if not store.sequence_exists(seq_id):
                    return ActionResult("failed", error="sequence not found in workspace",
                                        skip_reason="not_connected")
                # Resolve/ensure an enrollment so we can key on (enrollment, step).
                eid = store.enroll(seq_id, lead_id or 0, email,
                                   consent_source="trigger_send")
                # eid is None when already enrolled → look it up.
                if eid is None:
                    with store._session() as s:
                        from apps.api.services.outreach.orm_models import OutreachEnrollment
                        row = (s.query(OutreachEnrollment)
                               .filter(OutreachEnrollment.workspace_id == ws_id,
                                       OutreachEnrollment.sequence_id == seq_id,
                                       OutreachEnrollment.lead_id == (lead_id or 0))
                               .first())
                        eid = row.id if row else None
                enrollment_id = eid
                if enrollment_id is not None:
                    send_idem = sequence_step_idem(ws_id, seq_id, enrollment_id, step_number)
            payload = {
                "workspace_id": ws_id,
                "sequence_id": seq_id,
                "enrollment_id": enrollment_id,
                "lead_id": lead_id,
                "step_number": step_number,
                "to_email": email,
                "subject": cfg.get("subject", ""),
                "body_html": cfg.get("body_html", ""),
                "variables": lead_data,
            }
            with store._session() as db:
                enqueued = _enqueue_if_absent(db, ws_id, send_idem, payload)
        return ActionResult("success", summary="send enqueued" if enqueued else "already queued/sent")
    except Exception as e:
        return ActionResult("failed", error=str(e)[:200])
