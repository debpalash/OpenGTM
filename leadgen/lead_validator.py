"""
Lead Validator — Quality gate that rejects non-business entries.

Prevents garbage like article titles, search result placeholders, and
publisher contact info from entering the lead database.

Usage:
    from leadgen.lead_validator import validate_lead
    is_valid, reason = validate_lead(lead)
"""

import re
from urllib.parse import urlparse
from leadgen.models import Lead


# Article/listicle title patterns
_ARTICLE_PATTERNS = [
    r"(?i)\btop\s+\d+",               # "Top 10 ..."
    r"(?i)\bbest\s+\d+",              # "Best 20 ..."
    r"(?i)\b\d+\s+best\b",            # "20 Best ..."
    r"(?i)\bguide\b",                 # "... Guide"
    r"(?i)\bupdated\s+20\d{2}",       # "Updated 2024"
    r"(?i)\b20(2[3-9]|3\d)\b",        # Year mentions 2023-2039
    r"(?i)\bhow\s+to\b",             # "How to ..."
    r"(?i)\bwhat\s+(is|are)\b",       # "What is ..."
    r"(?i)\bdiscover\s+the\b",        # "Discover the ..."
    r"(?i)\bcomplete\s+(list|guide)\b",
    r"(?i)\b(list|ranking|review)\s+of\b",
]

# Placeholder / generic names
_PLACEHOLDER_NAMES = {
    "results", "home", "about", "contact", "contact us",
    "search results", "page not found", "404",
    "untitled", "no title", "loading", "error",
    "recruitment firm", "staffing agency", "hr agency",
    "it company", "consulting firm",
}

# Known publisher/aggregator/directory brand names (NOT real companies)
_PUBLISHER_NAMES = {
    "clutch", "goodfirms", "g2", "capterra", "softwaresuggest",
    "ambitionbox", "glassdoor", "indeed", "naukri", "shine",
    "linkedin", "twitter", "facebook", "instagram", "youtube",
    "wikipedia", "quora", "reddit", "medium", "trustpilot",
    "geeksforgeeks", "justdial", "sulekha", "indiamart",
    "built in", "builtinhyderabad", "techbehemoths",
    "consultingcase101", "mordorintelligence",
}

# Generic service title patterns (not company names)
_GENERIC_TITLE_PATTERNS = [
    r"(?i)^(IT|HR|Software|Staffing|Recruitment|Consulting|Consultancy)\s+(Services?|Agency|Firms?)\s+(in|of)\s+",
    r"(?i)^(Best|Top)\s+.*(in|of|for)\s+\w+$",
    r"(?i)\bcompanies?\s+in\s+\w+",
    r"(?i)\b(staffing|recruitment|consulting)\s+&?\s*(recruitment|staffing)?\s+agency\s+in\b",
    r"(?i)^(ERP|CRM|SAP|IT)\s+\w+\s+(In|Of)\s+",
]

# Publisher / aggregator domains (emails from these are not real leads)
_PUBLISHER_DOMAINS = {
    "softwaresuggest.com", "goodfirms.co", "clutch.co",
    "ambitionbox.com", "glassdoor.com", "glassdoor.co.in",
    "indeed.com", "naukri.com", "linkedin.com", "twitter.com",
    "facebook.com", "instagram.com", "youtube.com",
    "wikipedia.org", "quora.com", "reddit.com", "medium.com",
    "mordorintelligence.com", "rankexdigital.com",
    "g2.com", "capterra.com", "trustpilot.com",
}

# Non-India phone prefixes (for India-focused queries)
_NON_INDIA_PREFIXES = ["+1", "+44", "+61", "+49", "+33", "+971"]


def validate_lead(lead: Lead) -> tuple[bool, str]:
    """
    Validate a lead for data quality.

    Returns:
        (is_valid, reason) — reason is empty string if valid
    """
    name = (lead.company or "").strip()

    # ── Name checks ────────────────────────────────────────────────

    if not name:
        return False, "empty_name"

    if len(name) < 2:
        return False, "name_too_short"

    if len(name) > 60:
        return False, "name_too_long_likely_headline"

    # Check for placeholder names
    if name.lower() in _PLACEHOLDER_NAMES:
        return False, "placeholder_name"

    # Check for publisher/directory brand names
    if name.lower().replace(" ", "") in {n.replace(" ", "") for n in _PUBLISHER_NAMES}:
        return False, "publisher_name"

    # Check for article/listicle title patterns
    for pattern in _ARTICLE_PATTERNS:
        if re.search(pattern, name):
            return False, "article_title_pattern"

    # Check for generic service title patterns
    for pattern in _GENERIC_TITLE_PATTERNS:
        if re.search(pattern, name):
            return False, "generic_service_title"

    # Name must contain at least some alpha characters
    alpha_count = sum(1 for c in name if c.isalpha())
    if alpha_count < 3:
        return False, "insufficient_alpha_chars"

    # Name shouldn't be all-caps generic phrase
    words = name.split()
    if len(words) >= 5 and all(w[0].isupper() for w in words if w.isalpha()):
        # Likely a sentence/headline, not a company name
        # But allow if it looks like a real company (PascalCase each word)
        if len(name) > 50:
            return False, "likely_headline"

    # ── Email checks ───────────────────────────────────────────────

    if lead.email:
        email_domain = lead.email.split("@")[-1].lower() if "@" in lead.email else ""

        # Reject publisher/aggregator emails
        if email_domain in _PUBLISHER_DOMAINS:
            lead.email = ""  # Clear the bad email, don't reject the lead

        # Reject if email looks like an image path
        if any(ext in lead.email.lower() for ext in [".png", ".jpg", ".svg", ".gif"]):
            lead.email = ""

        # Cross-check: if lead has a website, email domain should relate
        if lead.email and lead.website:
            website_domain = _extract_domain(lead.website)
            if email_domain and website_domain:
                # Allow if email domain is in website domain or vice versa
                if (email_domain not in website_domain and
                    website_domain not in email_domain and
                    email_domain not in _KNOWN_EMAIL_PROVIDERS):
                    # Suspicious: email from different domain than website
                    # Don't reject, but flag
                    pass

    # ── Phone checks ───────────────────────────────────────────────

    if lead.phone:
        phone = lead.phone.strip()

        # Reject obviously fake numbers
        if phone in ("1234567890", "0000000000", "9999999999"):
            lead.phone = ""

        # Reject US/EU numbers for India queries
        for prefix in _NON_INDIA_PREFIXES:
            if phone.startswith(prefix):
                lead.phone = ""
                break

        # Reject numbers that are clearly Unix timestamps or IDs
        clean_digits = re.sub(r"[^\d]", "", phone)
        if len(clean_digits) > 12:
            lead.phone = ""

    # ── Website checks ─────────────────────────────────────────────

    if lead.website:
        domain = _extract_domain(lead.website)
        if domain in _PUBLISHER_DOMAINS:
            # Website is a publisher/aggregator page, not the company's site
            lead.website = ""

    return True, ""


def _extract_domain(url: str) -> str:
    """Extract base domain from URL."""
    try:
        parsed = urlparse(url)
        domain = parsed.netloc.lower()
        if domain.startswith("www."):
            domain = domain[4:]
        return domain
    except Exception:
        return ""


# Common email providers (Gmail, Yahoo, etc. are OK for small businesses)
_KNOWN_EMAIL_PROVIDERS = {
    "gmail.com", "yahoo.com", "hotmail.com", "outlook.com",
    "rediffmail.com", "ymail.com", "live.com", "aol.com",
}


def validate_and_clean_leads(leads: list[Lead]) -> tuple[list[Lead], list[tuple[Lead, str]]]:
    """
    Validate a batch of leads, returning (valid_leads, rejected_leads_with_reasons).
    """
    valid = []
    rejected = []

    for lead in leads:
        is_valid, reason = validate_lead(lead)
        if is_valid:
            valid.append(lead)
        else:
            rejected.append((lead, reason))

    return valid, rejected
