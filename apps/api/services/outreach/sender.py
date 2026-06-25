"""
SMTP Email Sender — Async email sending with rate limiting.

Supports any SMTP provider (Gmail, SendGrid, Mailgun, custom).
Template rendering via Jinja2 with lead variables.
"""

import asyncio
import logging
import time
from email.mime.text import MIMEText
from email.mime.multipart import MIMEMultipart
from typing import Optional, Dict, Any
from dataclasses import dataclass, field

logger = logging.getLogger("outreach.sender")


@dataclass
class SMTPConfig:
    host: str = ""
    port: int = 587
    email: str = ""
    password: str = ""
    from_name: str = "Yupcha"
    use_tls: bool = True
    max_per_hour: int = 50


@dataclass
class SendResult:
    success: bool
    message_id: str = ""
    error: str = ""
    timestamp: float = field(default_factory=time.time)


def get_smtp_config(workspace_id: Optional[str] = None) -> SMTPConfig:
    """Load SMTP config.

    When ``workspace_id`` is given, each field resolves to the per-workspace
    encrypted secret first (spec WI-6), falling back to the global settings DB /
    environment so single-tenant installs are unaffected.
    """
    try:
        from apps.api.services.workspace.secrets import get_secret

        def _g(key: str, default: str = "") -> str:
            return get_secret(workspace_id, key, default)

        return SMTPConfig(
            host=_g("SMTP_HOST", ""),
            port=int(_g("SMTP_PORT", "587") or "587"),
            email=_g("SMTP_EMAIL", ""),
            password=_g("SMTP_PASSWORD", ""),
            from_name=_g("SMTP_FROM_NAME", "Yupcha"),
            use_tls=_g("SMTP_USE_TLS", "1") == "1",
            max_per_hour=int(_g("SMTP_MAX_PER_HOUR", "50") or "50"),
        )
    except Exception:
        return SMTPConfig()


def is_smtp_configured(workspace_id: Optional[str] = None) -> bool:
    """Check if SMTP is properly configured (per-workspace, else global)."""
    cfg = get_smtp_config(workspace_id)
    return bool(cfg.host and cfg.email and cfg.password)


# ── Rate Limiter ──────────────────────────────────────────────

class RateLimiter:
    """Simple sliding-window rate limiter for email sends."""

    def __init__(self, max_per_hour: int = 50):
        self.max_per_hour = max_per_hour
        self._timestamps: list[float] = []

    def can_send(self) -> bool:
        now = time.time()
        cutoff = now - 3600
        self._timestamps = [t for t in self._timestamps if t > cutoff]
        return len(self._timestamps) < self.max_per_hour

    def record_send(self):
        self._timestamps.append(time.time())

    @property
    def remaining(self) -> int:
        now = time.time()
        cutoff = now - 3600
        active = [t for t in self._timestamps if t > cutoff]
        return max(0, self.max_per_hour - len(active))


_rate_limiter = RateLimiter()


# ── Header-injection hardening (spec §6.5.1) ──────────────────────────────────

import re as _re

# A pragmatic single-address email regex (post-normalization). Rejects control
# chars / multiple addresses. Not a full RFC 5322 parser — defense in depth on
# top of canonical normalization.
_EMAIL_RE = _re.compile(r"^[^\s@,;<>\r\n]+@[^\s@,;<>\r\n]+\.[^\s@,;<>\r\n]+$")


def sanitize_header_value(value: str, max_len: int = 998) -> str:
    """Strip CR/LF (and other control chars) and bound length for a MIME header.

    Header injection (CAN-SPAM/SMTP) flows through ``from_name``/``subject``;
    we collapse any CRLF or embedded newlines and truncate (RFC 5322 line cap).
    """
    if value is None:
        return ""
    # Drop all C0 control chars including CR/LF/TAB-as-fold attempts.
    cleaned = "".join(ch for ch in str(value) if ch == " " or (ord(ch) >= 32 and ord(ch) != 127))
    cleaned = cleaned.replace("\r", " ").replace("\n", " ").strip()
    return cleaned[:max_len]


def validate_recipient(to_email: str) -> bool:
    """True when ``to_email`` is a single, control-char-free address."""
    if not to_email or "\r" in to_email or "\n" in to_email:
        return False
    return bool(_EMAIL_RE.match(to_email.strip()))


def render_template(template: str, variables: Dict[str, Any]) -> str:
    """Render a template string with lead variables.

    Supports {{variable}} syntax for simplicity.
    Falls back to empty string for missing variables.
    """
    result = template
    for key, value in variables.items():
        result = result.replace("{{" + key + "}}", str(value or ""))
    # Clean up any remaining unreplaced variables
    import re
    result = re.sub(r"\{\{[^}]+\}\}", "", result)
    return result.strip()


def build_lead_variables(lead) -> Dict[str, Any]:
    """Extract template variables from a lead object."""
    return {
        "company": getattr(lead, "company", ""),
        "email": getattr(lead, "email", ""),
        "name": getattr(lead, "contact_person", ""),
        "contact_person": getattr(lead, "contact_person", ""),
        "city": getattr(lead, "city", ""),
        "phone": getattr(lead, "phone", ""),
        "website": getattr(lead, "website", ""),
        "specialization": getattr(lead, "specialization", ""),
        "company_size": getattr(lead, "company_size", ""),
        "score": getattr(lead, "score", 0),
        "title": getattr(lead, "contact_title", ""),
    }


async def send_email(
    to_email: str,
    subject: str,
    body_html: str,
    body_text: Optional[str] = None,
    config: Optional[SMTPConfig] = None,
    headers: Optional[Dict[str, str]] = None,
) -> SendResult:
    """Send a single email via SMTP.

    Args:
        to_email: Recipient email address
        subject: Email subject line
        body_html: HTML body content
        body_text: Plain text fallback (auto-generated if not provided). When the
            caller (``handle_send``) supplies it, it carries the conspicuous
            unsubscribe link + footer in BOTH parts (spec §6.5) — we DO NOT
            re-derive plaintext from HTML in that case.
        config: SMTP configuration. When provided (the handler is the rate
            authority, spec §6.3) the process-global ``_rate_limiter`` is
            BYPASSED so one workspace never blocks another.
        headers: Extra MIME headers (e.g. List-Unsubscribe / -Post). Values are
            CRLF-sanitized (spec §6.5.1).

    Returns:
        SendResult with success status and message ID or error
    """
    cfg = config or get_smtp_config()

    if not cfg.host or not cfg.email or not cfg.password:
        return SendResult(success=False, error="SMTP not configured")

    # Header-injection hardening (§6.5.1): validate the recipient and CRLF-strip
    # everything that lands in a header.
    if not validate_recipient(to_email):
        return SendResult(success=False, error="invalid recipient address")
    safe_from_name = sanitize_header_value(cfg.from_name, max_len=200) or "Yupcha"
    safe_subject = sanitize_header_value(subject)

    # The process-global limiter is ONLY consulted when no explicit per-workspace
    # config was passed (legacy single-tenant path). The handler owns durable,
    # per-workspace rate limiting and passes a config → bypass here.
    if config is None and not _rate_limiter.can_send():
        return SendResult(
            success=False,
            error=f"Rate limit reached ({cfg.max_per_hour}/hr). {_rate_limiter.remaining} remaining."
        )

    # Build MIME message
    msg = MIMEMultipart("alternative")
    msg["From"] = f"{safe_from_name} <{cfg.email}>"
    msg["To"] = to_email
    msg["Subject"] = safe_subject
    for hk, hv in (headers or {}).items():
        msg[hk] = sanitize_header_value(hv, max_len=2000)

    # Plain text fallback (only auto-derived when the caller didn't supply one).
    if not body_text:
        import re
        body_text = re.sub(r"<[^>]+>", "", body_html)
        body_text = re.sub(r"\s+", " ", body_text).strip()

    msg.attach(MIMEText(body_text, "plain"))
    msg.attach(MIMEText(body_html, "html"))

    try:
        import aiosmtplib

        smtp = aiosmtplib.SMTP(
            hostname=cfg.host,
            port=cfg.port,
            use_tls=cfg.use_tls,
        )

        await smtp.connect()
        if cfg.use_tls and cfg.port == 587:
            await smtp.starttls()
        await smtp.login(cfg.email, cfg.password)
        response = await smtp.send_message(msg)
        await smtp.quit()

        if config is None:
            _rate_limiter.record_send()
        message_id = msg.get("Message-ID", "")
        logger.info(f"Email sent to {to_email}: {safe_subject[:50]}")

        return SendResult(success=True, message_id=str(message_id))

    except ImportError:
        # Fallback to sync smtplib if aiosmtplib not installed
        import smtplib

        def _send_sync():
            with smtplib.SMTP(cfg.host, cfg.port) as server:
                if cfg.use_tls:
                    server.starttls()
                server.login(cfg.email, cfg.password)
                server.send_message(msg)
            return True

        try:
            await asyncio.to_thread(_send_sync)
            if config is None:
                _rate_limiter.record_send()
            logger.info(f"Email sent (sync fallback) to {to_email}")
            return SendResult(success=True, message_id="sync")
        except Exception as e:
            logger.error(f"SMTP sync send failed: {e}")
            return SendResult(success=False, error=str(e))

    except Exception as e:
        logger.error(f"SMTP send failed to {to_email}: {e}")
        return SendResult(success=False, error=str(e))


async def send_test_email(to_email: str) -> SendResult:
    """Send a test email to verify SMTP configuration."""
    return await send_email(
        to_email=to_email,
        subject="Yupcha — SMTP Test ✓",
        body_html="""
        <div style="font-family: -apple-system, sans-serif; max-width: 480px; margin: 0 auto; padding: 24px;">
            <h2 style="margin: 0 0 12px;">✓ SMTP Connected</h2>
            <p style="color: #666; font-size: 14px; line-height: 1.5;">
                Your SMTP configuration is working correctly.
                Yupcha can now send outreach emails on your behalf.
            </p>
            <hr style="border: none; border-top: 1px solid #eee; margin: 16px 0;">
            <p style="color: #999; font-size: 12px;">
                Sent from Yupcha · GTM Engine
            </p>
        </div>
        """,
    )
