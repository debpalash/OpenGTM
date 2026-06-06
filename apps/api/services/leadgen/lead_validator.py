"""
Lead Validator — Quality gate that rejects non-business entries.

Prevents garbage like article titles, search result placeholders, and
publisher contact info from entering the lead database.

Usage:
    from apps.api.services.leadgen.lead_validator import validate_lead
    is_valid, reason = validate_lead(lead)
"""

import re
from urllib.parse import urlparse
from apps.api.services.leadgen.models import Lead


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

# Placeholder / generic names (case-insensitive, checked after .lower().strip())
_PLACEHOLDER_NAMES = {
    "results", "home", "about", "contact", "contact us",
    "search results", "page not found", "404",
    "untitled", "no title", "loading", "error",
    "recruitment firm", "staffing agency", "hr agency",
    "it company", "consulting firm",
    # Generic industry terms that are NOT company names
    "human resources", "staffing", "recruitment", "consulting",
    "outsourcing", "manpower", "placement", "headhunting",
    "it services", "hr services", "hr solutions", "it solutions",
    "software development", "web development", "digital marketing",
    "business consulting", "management consulting",
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
    "placementindia", "placement india", "timesjobs", "monster",
    "foundit", "apna", "upwork", "freelancer", "fiverr",
    "mouthshut", "shiksha", "careers360", "collegedunia",
    # Directory / aggregator / tool sites
    "the manifest", "manifest", "aeroleads", "lusha", "apollo",
    "zoominfo", "hunter", "snov", "rocketreach", "clearbit",
    "freelistingindia", "free listing india", "fundoodata",
    "grotal", "tradeindia", "exportersindia", "dial4trade",
    "urbanpro", "bark", "thumbtack", "yelp", "yellowpages",
    "manta", "crunchbase", "owler", "tracxn", "wellfound",
    "angellist", "f6s", "startupindia", "yourstory",
}

# Generic service title patterns (not company names)
_GENERIC_TITLE_PATTERNS = [
    r"(?i)^(IT|HR|Software|Staffing|Recruitment|Consulting|Consultancy)\s+(Services?|Agency|Firms?)\s+(in|of)\s+",
    r"(?i)^(Best|Top)\s+.*(in|of|for)\s+\w+$",
    r"(?i)\bcompanies?\s+in\s+\w+",
    r"(?i)\b(staffing|recruitment|consulting)\s+\&?\s*(recruitment|staffing)?\s+agency\s+in\b",
    r"(?i)^(ERP|CRM|SAP|IT)\s+\w+\s+(In|Of)\s+",
    # Directory listing/category page titles
    r"(?i)^find\s+",                         # "Find HR Consultants..."
    r"(?i)^looking\s+for\s+",                # "Looking for construction staffing..."
    r"(?i)^search\s+for\s+",                 # "Search for..."
    r"(?i)^get\s+(quotes?|details?)\s+",     # "Get Quotes for..."
    r"(?i)^hire\s+",                         # "Hire Top..."
    r"(?i)^browse\s+",                       # "Browse..."
    r"(?i)^explore\s+",                      # "Explore..."
    r"(?i)\bfree\s+classifieds?\b",          # "Free Classifieds Ads..."
    r"(?i)\bads?\s+(in|buy|sell)\b",         # "Ads In Buy..."
    r"(?i)^(all|list of)\s+",               # "All ... Companies"
    r"(?i)^page\s+\d+",                      # "Page 3"
    r"(?i)^multi\s+(recruit|search|find)\b", # "Multi Recruit"
    r"(?i)\b(verified|trusted)\s+(list|suppliers?|vendors?)\b",
    r"(?i)\b\d+\s+\w+\s*(companies|agencies|firms|suppliers|vendors|based)\b",  # "52 Bangalore Based Staffing Agency Companies"
    r"(?i)^(placement|recruiting|staffing)\s+(services?|agencies?|companies?)\s+(in|near|at)\s+",
    r"(?i)\b(directory|listing|catalogue|catalog)\b",
    # Service description titles (not a specific company)
    r"(?i)^(HR|IT)\s+(consulting|staffing|outsourcing)\s+(service|firm|company|agency)\s+\w+$",
    r"(?i)^(HR|IT)\s+(staffing)\s+(\&|and)\s+(recruiting|recruitment)\s+",
    r"(?i)^top\s+.*(firms?|agencies?|companies?)\s+(in|of|for)\s+",
    r"(?i)\.com/",                            # URL fragments as company names
    r"(?i)^(warehouse|construction|manufacturing)\s+(staffing|recruitment)\s+(agency|company|service)\s+(in|of)\s+",
    r"(?i)\bcompanies/agencies\b",            # "IT Staffing Companies/Agencies"
    # "X Outsourcing - SiteName" pattern
    r"(?i)^(HR|IT|BPO|KPO|RPO)\s+(outsourcing|offshoring)\s*[-\u2013\u2014]",
    # Generic service name without a proper company identifier
    r"(?i)^(HR|IT|BPO|KPO)\s+(consulting|staffing|outsourcing|recruitment|solutions?)\s+(service|india|company|agency)s?$",
    # "X Agency in Y" without a company name
    r"(?i)^\w+\s+(staffing|recruitment|consulting|outsourcing)\s+(agency|service|firm|company)\s+(in|near|for)\s+",
    # Queries/questions disguised as names
    r"(?i)^(need|want|searching|seeking|require)\s+",
    r"(?i)^(compare|check|verify|view|see|read|know)\s+",
    r"(?i)\b(reviews?|ratings?|testimonials?)\s+(of|for|on)\b",
    # Navigation/pagination artifacts
    r"(?i)^(next|prev|previous|back|home|menu|footer|header|sidebar)$",
    r"(?i)^(show|view|see)\s+(more|all|less)",
    # bare domain as company name
    r"(?i)^[a-z]+\.(com|co\.in|org|net|in)(/|$)",

    # ── BROAD patterns for service+location combos ──
    # "Staffing in Bangalore", "HR Consultancy in Mumbai"
    r"(?i)^(staffing|recruitment|consulting|consultancy|outsourcing|placement|manpower|headhunting)\s+(in|at|near|for)\s+",
    # "HR Consultancy in Bangalore" / "IT Services in Delhi"
    r"(?i)^(HR|IT)\s+.+\s+(in|at|near)\s+[A-Z]",
    # Any "X in <City>" where X is a service keyword
    r"(?i)\b(staffing|recruitment|consulting|consultancy|outsourcing|placement|manpower)\s+(services?\s+)?(in|at|near)\s+[A-Z]",
    # "X & Y in City" pattern — catches "HR Consulting & Career Choice in Bangalore"
    r"(?i)\b\w+\s*[&]\s*.+\s+(in|at|near)\s+[A-Z]\w+$",
    # Trailing pipe, dash, or colon (truncated page titles)
    r"[|:;]\s*$",
    # "Manufacturers Suppliers" / "Wholesalers Dealers" — directory category keywords
    r"(?i)\b(manufacturers?|suppliers?|wholesalers?|dealers?|distributors?|exporters?|importers?)\b",
    # "X Services in Y Manufacturers Suppliers" pattern
    r"(?i)\bservices?\s+in\s+\w+\s+\w+\s+(manufacturers?|suppliers?)\b",
    # Generic "X Companies" at end
    r"(?i)\b(staffing|recruitment|consulting)\s+(agency\s+)?companies$",
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
    # Search engines / email providers — never a sourced company's own site
    "bing.com", "google.com", "google.co.in", "microsoft.com",
    "yahoo.com", "duckduckgo.com", "baidu.com", "yandex.com",
}

# Job-title words — a name ending in one of these (≤3 words) is a title, not a company
_JOB_TITLE_TERMS = {
    "executive", "manager", "director", "officer", "president", "founder",
    "cofounder", "ceo", "cto", "coo", "cfo", "cmo", "cio", "vp", "svp", "evp",
    "head", "lead", "recruiter", "consultant", "analyst", "engineer",
    "developer", "designer", "intern", "associate", "specialist",
    "coordinator", "administrator", "representative", "agent", "principal",
    "supervisor", "strategist", "architect",
}

# Search engine / email-provider brands that leak in as company names (e.g. from
# a pattern email like name@bing.com). Pure brand-as-company → reject.
_SEARCH_EMAIL_BRANDS = {
    "bing", "google", "microsoft", "yahoo", "outlook", "gmail", "hotmail",
    "duckduckgo", "baidu", "yandex", "rediffmail",
}


def _name_rejection(name: str) -> str:
    """Return a rejection reason for junk company names, else ''.

    Catches gaps the pattern lists miss: pure job titles ('Sales Executive',
    'Managing Director') and search/email brands ('Bing') derived from pattern
    emails. Shared by validate_lead and validate_lead_light.
    """
    low = name.lower().strip()
    words = [w for w in re.split(r"[^a-z0-9]+", low) if w]
    if not words:
        return ""
    # Pure job title: short name whose last word is a title term.
    if len(words) <= 3 and words[-1] in _JOB_TITLE_TERMS:
        return "job_title_not_company"
    # Search engine / email-provider brand as the whole company name.
    if low.replace(" ", "") in _SEARCH_EMAIL_BRANDS:
        return "search_or_email_brand"
    return ""

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

    # Job titles / search-engine brands masquerading as company names
    nr = _name_rejection(name)
    if nr:
        return False, nr

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

        # Reject if email looks like an image/asset path
        if any(ext in lead.email.lower() for ext in [".png", ".jpg", ".svg", ".gif", ".webp", ".css", ".js", ".woff"]):
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
        # Normalize to root domain (strip paths like /about-us/locations/bangalore/)
        lead.website = _normalize_url(lead.website)
        domain = _extract_domain(lead.website)
        if domain in _PUBLISHER_DOMAINS:
            # Website is a publisher/aggregator page, not the company's site
            lead.website = ""

    # ── Structural quality: a real lead must have some contact info ──
    has_website = bool(lead.website and lead.website.strip() and lead.website != "N/A")
    has_email = bool(lead.email and lead.email.strip() and lead.email != "N/A")
    has_phone = bool(lead.phone and lead.phone.strip() and lead.phone != "N/A")
    has_linkedin = bool(lead.linkedin_url and lead.linkedin_url.strip() and lead.linkedin_url != "N/A")

    if not has_website and not has_email and not has_phone and not has_linkedin:
        return False, "no_contact_data"

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


def _normalize_url(url: str) -> str:
    """Normalize website URL to root domain (strip paths/query/fragments).
    
    'https://dexian.com/about-us/locations/bangalore/' → 'https://dexian.com'
    """
    if not url:
        return url
    url = url.strip()
    if not url.startswith("http"):
        url = "https://" + url
    try:
        parsed = urlparse(url)
        return f"{parsed.scheme}://{parsed.netloc}"
    except Exception:
        return url


# Common email providers (Gmail, Yahoo, etc. are OK for small businesses)
_KNOWN_EMAIL_PROVIDERS = {
    "gmail.com", "yahoo.com", "hotmail.com", "outlook.com",
    "rediffmail.com", "ymail.com", "live.com", "aol.com",
}


def validate_lead_light(lead: Lead) -> tuple[bool, str]:
    """
    Light validation — only reject obvious garbage (names, titles).
    Does NOT reject leads for missing contact data.
    Use this BEFORE enrichment; use validate_lead() AFTER enrichment.
    """
    name = (lead.company or "").strip()

    if not name:
        return False, "empty_name"
    if len(name) < 2:
        return False, "name_too_short"
    if len(name) > 60:
        return False, "name_too_long_likely_headline"
    if name.lower() in _PLACEHOLDER_NAMES:
        return False, "placeholder_name"
    if name.lower().replace(" ", "") in {n.replace(" ", "") for n in _PUBLISHER_NAMES}:
        return False, "publisher_name"
    for pattern in _ARTICLE_PATTERNS:
        if re.search(pattern, name):
            return False, "article_title_pattern"
    for pattern in _GENERIC_TITLE_PATTERNS:
        if re.search(pattern, name):
            return False, "generic_service_title"
    nr = _name_rejection(name)
    if nr:
        return False, nr
    alpha_count = sum(1 for c in name if c.isalpha())
    if alpha_count < 3:
        return False, "insufficient_alpha_chars"
    words = name.split()
    if len(words) >= 5 and all(w[0].isupper() for w in words if w.isalpha()):
        if len(name) > 50:
            return False, "likely_headline"

    # Clean bad emails/phones but don't reject for missing them
    if lead.email:
        email_domain = lead.email.split("@")[-1].lower() if "@" in lead.email else ""
        if email_domain in _PUBLISHER_DOMAINS:
            lead.email = ""
        if any(ext in lead.email.lower() for ext in [".png", ".jpg", ".svg", ".gif", ".webp", ".css", ".js", ".woff"]):
            lead.email = ""
    if lead.phone:
        phone = lead.phone.strip()
        if phone in ("1234567890", "0000000000", "9999999999"):
            lead.phone = ""
        clean_digits = re.sub(r"[^\d]", "", phone)
        if len(clean_digits) > 12:
            lead.phone = ""
    if lead.website:
        lead.website = _normalize_url(lead.website)
        domain = _extract_domain(lead.website)
        if domain in _PUBLISHER_DOMAINS:
            lead.website = ""

    return True, ""


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


def validate_and_clean_leads_light(leads: list[Lead]) -> tuple[list[Lead], list[tuple[Lead, str]]]:
    """
    Light validation — only reject garbage names/titles.
    Keeps leads without contact data (they'll be enriched later).
    """
    valid = []
    rejected = []

    for lead in leads:
        is_valid, reason = validate_lead_light(lead)
        if is_valid:
            valid.append(lead)
        else:
            rejected.append((lead, reason))

    return valid, rejected
