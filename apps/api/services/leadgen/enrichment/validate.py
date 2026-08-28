"""
Enrichment output validation — accept-gate for waterfall results.

Ported from masteranime/enrichment-kit (utils/validate.ts) + VAV-Technologies/
clay-clone junk filters. The waterfall must NOT accept (or charge for, or stop
on) garbage values: role/placeholder emails, provider sentinel strings like
"no-results-found", malformed LinkedIn URLs, or an email whose domain doesn't
match the company domain. A value that fails validation is treated as "no data"
so the waterfall keeps cascading.

See docs/research/clay-alternatives-ingestion-catalog.md (Phase 1, item 3).
"""

import re
from typing import Optional

# Role / generic mailboxes — usable for a company catch-all but NOT a person's
# deliverable address; reject when we're finding a *person's* email.
ROLE_EMAIL_LOCALPARTS = {
    "info", "support", "sales", "contact", "hello", "admin", "office",
    "team", "help", "service", "services", "enquiry", "enquiries",
    "inquiry", "inquiries", "marketing", "billing", "accounts", "hr",
    "careers", "jobs", "press", "media", "noreply", "no-reply", "donotreply",
    "webmaster", "postmaster", "abuse", "privacy", "legal", "general",
}

# Provider sentinels / placeholders that some APIs return in a value field
# instead of an empty response.
SENTINEL_VALUES = {
    "", "n/a", "na", "none", "null", "nil", "-", "—", "unknown",
    "not found", "not_found", "notfound", "no-results-found", "no results found",
    "no_results_found", "no result", "no data", "no_data", "pending",
    "processing", "error", "failed", "undefined", "[object object]",
}

_EMAIL_RE = re.compile(r"^[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}$")
_PLACEHOLDER_EMAIL_RE = re.compile(
    r"(^|@)(example|test|domain|email|yourcompany|company|sample|placeholder)\.",
    re.IGNORECASE,
)
_LINKEDIN_RE = re.compile(
    r"^https?://([a-z]{2,3}\.)?linkedin\.com/(in|company|pub)/[^/\s?]+",
    re.IGNORECASE,
)


def _is_sentinel(value: str) -> bool:
    return value.strip().lower() in SENTINEL_VALUES


def _domain_of(email: str) -> Optional[str]:
    if "@" not in email:
        return None
    return email.rsplit("@", 1)[1].strip().lower().lstrip("@")


def _root_domain(domain: str) -> str:
    """Strip scheme/path/www and return the registrable-ish domain (last 2 labels)."""
    d = domain.strip().lower()
    d = re.sub(r"^https?://", "", d)
    d = d.split("/")[0].split("?")[0]
    d = d[4:] if d.startswith("www.") else d
    parts = [p for p in d.split(".") if p]
    return ".".join(parts[-2:]) if len(parts) >= 2 else d


def is_valid_value(value, *, allow_sentinels: bool = False) -> bool:
    """Generic: is this a real value (not empty / not a provider sentinel)?"""
    if value is None:
        return False
    if not isinstance(value, str):
        return bool(value)
    v = value.strip()
    if not v:
        return False
    if not allow_sentinels and _is_sentinel(v):
        return False
    return True


def is_valid_email(
    email,
    *,
    company_domain: Optional[str] = None,
    allow_role: bool = False,
) -> bool:
    """Accept-gate for an email value.

    Rejects: empty/sentinel, malformed, placeholder domains (example.com),
    role mailboxes (info@, sales@ …) unless allow_role, and — when
    company_domain is given — emails whose domain doesn't match it.
    """
    if not is_valid_value(email):
        return False
    e = email.strip().lower()
    if not _EMAIL_RE.match(e):
        return False
    if _PLACEHOLDER_EMAIL_RE.search(e):
        return False
    local = e.split("@", 1)[0]
    if not allow_role and local in ROLE_EMAIL_LOCALPARTS:
        return False
    if company_domain:
        want = _root_domain(company_domain)
        got = _domain_of(e)
        # free-mail addresses are allowed (a person may use gmail); only reject
        # when both are corporate domains and they disagree.
        if want and got and got not in FREE_EMAIL_DOMAINS and _root_domain(got) != want:
            return False
    return True


def is_valid_linkedin(url) -> bool:
    """Accept-gate for a LinkedIn profile/company URL."""
    if not is_valid_value(url):
        return False
    return bool(_LINKEDIN_RE.match(url.strip()))


# Common free / personal mail providers — a person legitimately using one of
# these should NOT trip the email-domain-vs-company-domain mismatch check.
FREE_EMAIL_DOMAINS = {
    "gmail.com", "googlemail.com", "yahoo.com", "ymail.com", "hotmail.com",
    "outlook.com", "live.com", "msn.com", "aol.com", "icloud.com", "me.com",
    "mac.com", "proton.me", "protonmail.com", "gmx.com", "zoho.com",
    "yandex.com", "mail.com", "rediffmail.com",
}


# Field → validator. Used by the waterfall accept-gate. company_domain is
# threaded in for email so we can reject cross-domain matches.
def validate_field(field_name: str, value, *, company_domain: Optional[str] = None) -> bool:
    """Return True if `value` is an acceptable result for `field_name`."""
    if field_name == "email":
        return is_valid_email(value, company_domain=company_domain)
    if field_name in ("linkedin_url", "linkedin"):
        return is_valid_linkedin(value)
    return is_valid_value(value)
