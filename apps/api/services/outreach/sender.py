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


def get_smtp_config() -> SMTPConfig:
    """Load SMTP config from settings DB."""
    try:
        from apps.api.routers.settings import _db_get
        return SMTPConfig(
            host=_db_get("SMTP_HOST", ""),
            port=int(_db_get("SMTP_PORT", "587")),
            email=_db_get("SMTP_EMAIL", ""),
            password=_db_get("SMTP_PASSWORD", ""),
            from_name=_db_get("SMTP_FROM_NAME", "Yupcha"),
            use_tls=_db_get("SMTP_USE_TLS", "1") == "1",
            max_per_hour=int(_db_get("SMTP_MAX_PER_HOUR", "50")),
        )
    except Exception:
        return SMTPConfig()


def is_smtp_configured() -> bool:
    """Check if SMTP is properly configured."""
    cfg = get_smtp_config()
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
) -> SendResult:
    """Send a single email via SMTP.

    Args:
        to_email: Recipient email address
        subject: Email subject line
        body_html: HTML body content
        body_text: Plain text fallback (auto-generated if not provided)
        config: SMTP configuration (loaded from DB if not provided)

    Returns:
        SendResult with success status and message ID or error
    """
    cfg = config or get_smtp_config()

    if not cfg.host or not cfg.email or not cfg.password:
        return SendResult(success=False, error="SMTP not configured")

    if not _rate_limiter.can_send():
        return SendResult(
            success=False,
            error=f"Rate limit reached ({cfg.max_per_hour}/hr). {_rate_limiter.remaining} remaining."
        )

    # Build MIME message
    msg = MIMEMultipart("alternative")
    msg["From"] = f"{cfg.from_name} <{cfg.email}>"
    msg["To"] = to_email
    msg["Subject"] = subject

    # Plain text fallback
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

        _rate_limiter.record_send()
        message_id = msg.get("Message-ID", "")
        logger.info(f"Email sent to {to_email}: {subject[:50]}")

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
