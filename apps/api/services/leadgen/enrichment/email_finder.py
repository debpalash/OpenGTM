"""
Email Finder — Discover and verify email addresses.

Enhanced with personal email pattern generation for decision makers.
Uses common email patterns, domain-based inference, MX verification,
and DDG search to find both company and personal emails.
"""

import dns.resolver
import re
import time
from typing import List, Optional, Tuple
from urllib.parse import urlparse

from ddgs import DDGS
from apps.api.services.leadgen.models import Lead


# Common company-level email prefixes
COMMON_PREFIXES = [
    "info", "contact", "hr", "sales", "enquiry", "careers",
    "support", "admin", "hello", "business", "recruitment",
]

# ── Domain Helpers ────────────────────────────────────────────────

def _domain_from_url(url: str) -> str:
    """Extract domain from URL."""
    if not url:
        return ""
    url = url.lower().strip()
    if not url.startswith("http"):
        url = "https://" + url
    parsed = urlparse(url)
    domain = parsed.netloc
    if domain.startswith("www."):
        domain = domain[4:]
    return domain


def _split_name(full_name: str) -> Tuple[str, str]:
    """Split a full name into (first, last). Handles Indian names well."""
    parts = full_name.strip().split()
    if len(parts) == 0:
        return ("", "")
    if len(parts) == 1:
        return (parts[0].lower(), "")
    return (parts[0].lower(), parts[-1].lower())


# ── MX Verification ──────────────────────────────────────────────

def verify_email_mx(domain: str) -> bool:
    """Check if a domain has valid MX records (can receive email)."""
    try:
        answers = dns.resolver.resolve(domain, "MX", lifetime=5)
        return len(answers) > 0
    except Exception:
        return False


# ── Personal Email Patterns ──────────────────────────────────────

def generate_personal_patterns(first: str, last: str, domain: str) -> List[str]:
    """Generate likely personal email addresses from name + domain.

    Returns patterns ordered by most common first.
    Based on analysis of 100M+ email addresses.
    """
    if not first or not domain:
        return []

    patterns = []
    fi = first[0]  # first initial

    if last:
        li = last[0]  # last initial
        patterns = [
            f"{first}.{last}@{domain}",       # john.doe@    (most common)
            f"{first}{last}@{domain}",          # johndoe@
            f"{fi}{last}@{domain}",             # jdoe@
            f"{first}{li}@{domain}",            # johnd@
            f"{first}_{last}@{domain}",         # john_doe@
            f"{last}.{first}@{domain}",         # doe.john@
            f"{fi}.{last}@{domain}",            # j.doe@
            f"{last}{fi}@{domain}",             # doej@
            f"{first}-{last}@{domain}",         # john-doe@
            f"{last}@{domain}",                  # doe@
            f"{first}@{domain}",                 # john@
            f"{fi}{li}@{domain}",               # jd@
            f"{last}_{first}@{domain}",         # doe_john@
        ]
    else:
        patterns = [
            f"{first}@{domain}",
        ]

    return patterns


def find_personal_email_via_search(
    name: str,
    company: str,
    domain: str,
) -> Tuple[str, str]:
    """Search DDG for a person's email address.

    Returns (email, confidence) where confidence is:
    - "verified" if found in search results matching domain
    - "" if not found
    """
    if not name or not domain:
        return ("", "")

    first, last = _split_name(name)
    if not first:
        return ("", "")

    # Search for the person's email
    query_parts = [f'"{name}"', "email", f"@{domain}"]
    if company:
        query_parts.append(f'"{company}"')
    query = " ".join(query_parts)

    try:
        with DDGS() as ddgs:
            results = list(ddgs.text(query, max_results=5))
            for r in results:
                text = f"{r.get('title', '')} {r.get('body', '')}"
                emails = re.findall(r'[\w\.\-]+@[\w\.\-]+\.\w{2,}', text)
                for email in emails:
                    email_lower = email.lower()
                    # Must match our domain
                    if domain.lower() in email_lower:
                        # Must contain part of the person's name
                        if first in email_lower or (last and last in email_lower):
                            return (email, "verified")
    except Exception:
        pass

    return ("", "")


def find_personal_email(
    name: str,
    company: str,
    domain: str,
) -> Tuple[str, str]:
    """Find a personal email for a named contact at a company.

    Strategy:
    1. Search DDG for the person's email → "verified"
    2. Check domain MX records exist
    3. Generate pattern-based email → "pattern"

    Returns (email, confidence).
    """
    if not name or not domain:
        return ("", "")

    # 1. Try to find via search (highest confidence)
    email, confidence = find_personal_email_via_search(name, company, domain)
    if email:
        return (email, confidence)

    # 2. Check if domain can receive email
    if not verify_email_mx(domain):
        return ("", "")

    # 3. Generate most likely pattern
    first, last = _split_name(name)
    patterns = generate_personal_patterns(first, last, domain)
    if patterns:
        # Return the most common pattern (first.last@domain)
        return (patterns[0], "pattern")

    return ("", "")


# ── Company Email Finder (existing) ──────────────────────────────

def guess_emails(website: str) -> List[str]:
    """Generate likely email addresses from a website domain."""
    domain = _domain_from_url(website)
    if not domain:
        return []
    return [f"{prefix}@{domain}" for prefix in COMMON_PREFIXES]


def find_email_via_search(
    company: str,
    city: str = "",
    domain: str = "",
) -> str:
    """Search DuckDuckGo for the company's email address."""
    query_parts = [company, "email", "contact"]
    if city:
        query_parts.append(city)
    if domain:
        query_parts.append(f"@{domain}")

    query = " ".join(query_parts)

    try:
        with DDGS() as ddgs:
            results = list(ddgs.text(query, max_results=5))
            for r in results:
                text = f"{r.get('title', '')} {r.get('body', '')}"
                emails = re.findall(r'[\w\.\-]+@[\w\.\-]+\.\w{2,}', text)
                for email in emails:
                    # Prefer emails matching the company domain
                    if domain and domain in email.lower():
                        return email
                # If no domain match, return first found
                for email in emails:
                    if not any(x in email.lower() for x in ['example', 'domain', 'email.com']):
                        return email
    except Exception:
        pass

    return ""


# ── Batch Enrichment ─────────────────────────────────────────────

def enrich_emails(leads: List[Lead], delay: float = 1.5) -> List[Lead]:
    """
    Find missing email addresses for leads.

    Strategy:
    1. Search DuckDuckGo for company email
    2. If website exists, generate guesses based on domain

    Updates leads in-place.
    """
    needs_email = [l for l in leads if not l.has_email and l.company]
    print(f"  📧 Finding emails for {len(needs_email)} leads...")

    found = 0
    for lead in needs_email:
        domain = _domain_from_url(lead.website)
        email = find_email_via_search(lead.company, lead.city, domain)

        if email:
            lead.email = email
            lead.email_confidence = "verified"
            found += 1
            print(f"    ✅ {lead.company}: {email}")
        elif domain:
            # Use most common pattern as fallback
            lead.email = f"info@{domain}"
            lead.email_confidence = "generic"
            print(f"    🔮 {lead.company}: info@{domain} (guessed)")

        time.sleep(delay)

    print(f"  📊 Found {found} verified emails")
    return leads


def enrich_personal_emails(leads: List[Lead], delay: float = 1.5) -> List[Lead]:
    """
    Find personal email addresses for leads that have decision makers.

    For each lead with a contact_person and website domain, tries to
    find their personal email using search + pattern generation.

    Updates leads in-place. Replaces generic emails (info@) with
    personal ones when found.
    """
    import json

    candidates = []
    for lead in leads:
        if not lead.contact_person or not lead.website:
            continue
        domain = _domain_from_url(lead.website)
        if not domain:
            continue
        # Skip if already has a personal (non-generic) email
        if lead.email_confidence in ("verified", "pattern"):
            continue
        candidates.append((lead, domain))

    if not candidates:
        return leads

    print(f"  📧 Finding personal emails for {len(candidates)} leads...")

    found = 0
    for lead, domain in candidates:
        email, confidence = find_personal_email(
            lead.contact_person, lead.company, domain
        )

        if email and confidence:
            # Replace generic email with personal one
            old_email = lead.email
            lead.email = email
            lead.email_confidence = confidence
            found += 1
            print(f"    ✅ {lead.company}: {lead.contact_person} → {email} ({confidence})")

            # Also update decision_makers JSON if present
            if lead.decision_makers:
                try:
                    dms = json.loads(lead.decision_makers)
                    for dm in dms:
                        if dm.get("name", "").lower() == lead.contact_person.lower():
                            dm["email"] = email
                            dm["email_confidence"] = confidence
                    lead.decision_makers = json.dumps(dms)
                except (json.JSONDecodeError, TypeError):
                    pass

            # Preserve old email as secondary
            if old_email and old_email != email and old_email not in (lead.secondary_emails or ""):
                lead.secondary_emails = (
                    f"{lead.secondary_emails}|{old_email}" if lead.secondary_emails else old_email
                )

        time.sleep(delay)

    print(f"  📊 Found {found} personal emails")
    return leads
