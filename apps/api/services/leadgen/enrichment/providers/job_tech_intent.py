"""
Job-Posting Tech-Adoption Intent parser.

Free technographics / buying-intent signal extracted from job-posting TEXT that
the leadgen pipeline ALREADY fetches (JobSpy search snippets, ATS board roles).
NO new network calls — this is pure text analysis on sunk scraping cost.

Why it's high value:
  A job description naming a tool ("experience with Salesforce", "we use
  Snowflake", "migrating off HubSpot to Marketo") is a near-certain technographic
  signal: the company runs — or is actively adopting — that tool. Unlike
  website fingerprinting (passive, what's on the marketing site), hiring text
  reveals the *internal* stack and *future* intent ("hiring for <tech>" =>
  adopting / expanding that capability).

What it emits (all derived from text, degrade gracefully when text is absent):
  - technologies          : sorted list of detected tools (the stack)
  - tech_adoption_signal  : list of {tech, category, intent} where intent is
                            "adopting" (migration/greenfield language nearby),
                            "expanding" (hiring multiple roles around the tech),
                            or "using" (mentioned, baseline).
  - hiring_velocity       : {open_roles, recent_roles, recency_days, level}
                            derived from posting count + recency when available.

Maintaining the dictionary:
  TECH_DICTIONARY is grouped by category. Each entry is
      "Canonical Name": [list of case-insensitive regex aliases]
  Aliases use word boundaries internally so short tokens ("go", "r", "c#")
  don't false-match inside other words. To add a tool, add one line under the
  right category. Keep aliases specific — prefer "salesforce" over "sf".
"""

from __future__ import annotations

import re
from typing import Dict, List, Optional

# ── Curated tech dictionary ──────────────────────────────────────────────
# {category: {"Canonical Name": [alias, ...]}}.  Aliases are matched
# case-insensitively with word boundaries (see _compile). Order within a
# category is irrelevant; canonical names are deduped across categories.
TECH_DICTIONARY: Dict[str, Dict[str, List[str]]] = {
    "CRM": {
        "Salesforce": ["salesforce", "sfdc", "sales cloud", "apex"],
        "HubSpot": ["hubspot"],
        "Microsoft Dynamics": ["dynamics 365", "dynamics crm", "ms dynamics"],
        "Zoho CRM": ["zoho crm", "zoho"],
        "Pipedrive": ["pipedrive"],
        "Close": ["close.com", "close crm"],
    },
    "MarTech": {
        "Marketo": ["marketo"],
        "Pardot": ["pardot"],
        "Mailchimp": ["mailchimp"],
        "Klaviyo": ["klaviyo"],
        "Braze": ["braze"],
        "Iterable": ["iterable"],
        "Marketing Cloud": ["marketing cloud", "sfmc"],
        "Google Analytics": ["google analytics", "ga4", "gtag"],
        "Segment": ["segment.io", "twilio segment"],
    },
    "Data & Warehouse": {
        "Snowflake": ["snowflake"],
        "Databricks": ["databricks"],
        "BigQuery": ["bigquery", "big query"],
        "Redshift": ["redshift"],
        "dbt": ["dbt", "data build tool"],
        "Airflow": ["airflow"],
        "Fivetran": ["fivetran"],
        "Looker": ["looker"],
        "Tableau": ["tableau"],
        "Power BI": ["power bi", "powerbi"],
        "Kafka": ["kafka"],
        "Spark": ["apache spark", "pyspark", "spark"],
    },
    "Databases": {
        "PostgreSQL": ["postgresql", "postgres", "psql"],
        "MySQL": ["mysql"],
        "MongoDB": ["mongodb", "mongo"],
        "Redis": ["redis"],
        "Elasticsearch": ["elasticsearch", "elastic search"],
        "Cassandra": ["cassandra"],
        "DynamoDB": ["dynamodb"],
    },
    "Cloud & Infra": {
        "AWS": ["amazon web services", "aws"],
        "Azure": ["microsoft azure", "azure"],
        "GCP": ["google cloud", "gcp"],
        "Kubernetes": ["kubernetes", "k8s"],
        "Docker": ["docker"],
        "Terraform": ["terraform"],
        "Ansible": ["ansible"],
    },
    "Languages": {
        "Python": ["python"],
        "JavaScript": ["javascript"],
        "TypeScript": ["typescript"],
        "Java": ["java"],
        "Go": ["golang", r"\bgo\b"],
        "Rust": ["rust"],
        "Ruby": ["ruby"],
        "C#": [r"c#", r"c♯", r"\bdotnet\b", r"\.net\b"],
        "PHP": ["php"],
        "Scala": ["scala"],
    },
    "Web Frameworks": {
        "React": ["react", "react.js", "reactjs"],
        "Vue.js": ["vue.js", "vuejs", "vue"],
        "Angular": ["angular"],
        "Next.js": ["next.js", "nextjs"],
        "Node.js": ["node.js", "nodejs", "node"],
        "Django": ["django"],
        "Rails": ["ruby on rails", "rails"],
        "Spring": ["spring boot", "spring framework"],
    },
    "Competitor Tools": {
        # Direct competitors / adjacent data-enrichment & GTM tooling. A
        # company hiring around these is a prime switch / displacement target.
        "Clay": ["clay.com", r"clay\b"],
        "Apollo.io": ["apollo.io", "apollo"],
        "ZoomInfo": ["zoominfo", "zoom info"],
        "Outreach": ["outreach.io", "outreach"],
        "Salesloft": ["salesloft", "sales loft"],
        "Lemlist": ["lemlist"],
        "Instantly": ["instantly.ai", "instantly"],
        "Clearbit": ["clearbit"],
        "Lusha": ["lusha"],
        "Gong": ["gong.io", "gong"],
    },
}

# Phrases near a tech mention that imply NEW adoption / migration (vs. just
# "we use X"). Used to upgrade intent to "adopting".
_ADOPTING_CUES = [
    "migrat", "implement", "roll out", "rolling out", "adopt", "introduc",
    "transition to", "moving to", "move to", "switch to", "switching to",
    "greenfield", "from scratch", "build out", "stand up", "standing up",
    "evaluate", "evaluating", "proof of concept", "poc ", "first ",
    "set up", "setting up",
]

# How far (chars) around a match to scan for adoption cues.
_CUE_WINDOW = 80


def _compile(aliases: List[str]) -> List[re.Pattern]:
    """Compile aliases to case-insensitive, boundary-aware patterns.

    A literal alias gets wrapped in \\b ... \\b so "go" won't match "google".
    Aliases that already contain regex metachars (e.g. r"go\\b", r"c#") are
    used as-is (still case-insensitive).
    """
    pats: List[re.Pattern] = []
    for a in aliases:
        if re.search(r"[\\^$.|?*+()\[\]{}#]", a):
            # Caller supplied an explicit regex / special token — trust it.
            pats.append(re.compile(a, re.IGNORECASE))
        else:
            pats.append(re.compile(r"\b" + re.escape(a) + r"\b", re.IGNORECASE))
    return pats


# Pre-compiled index: [(canonical, category, [patterns])]
_COMPILED: List[tuple] = []
for _cat, _entries in TECH_DICTIONARY.items():
    for _canon, _aliases in _entries.items():
        _COMPILED.append((_canon, _cat, _compile(_aliases)))


def _category_of(name: str) -> str:
    for cat, entries in TECH_DICTIONARY.items():
        if name in entries:
            return cat
    return "Other"


def detect_technologies(text: str) -> List[Dict[str, str]]:
    """Detect named technologies in free job-posting text.

    Returns a list of {"name", "category"} dicts, deduped by canonical name,
    sorted by name. Empty list for empty/whitespace text (graceful degrade).
    """
    if not text or not text.strip():
        return []

    found: Dict[str, str] = {}  # canonical -> category
    for canon, cat, pats in _COMPILED:
        if canon in found:
            continue
        for rx in pats:
            if rx.search(text):
                found[canon] = cat
                break

    return [{"name": n, "category": c} for n, c in sorted(found.items())]


def _intent_for(name: str, text_lower: str, occurrences: int) -> str:
    """Classify adoption intent for a detected tech.

    "adopting"  — migration / greenfield / evaluation language near a mention.
    "expanding" — mentioned in 2+ places (multiple roles / repeated emphasis).
    "using"     — mentioned once with no adoption cue (baseline technographic).
    """
    aliases_pats = next(
        (pats for canon, _cat, pats in _COMPILED if canon == name), []
    )
    for rx in aliases_pats:
        for m in rx.finditer(text_lower):
            start = max(0, m.start() - _CUE_WINDOW)
            end = min(len(text_lower), m.end() + _CUE_WINDOW)
            window = text_lower[start:end]
            if any(cue in window for cue in _ADOPTING_CUES):
                return "adopting"
    if occurrences >= 2:
        return "expanding"
    return "using"


def detect_tech_adoption_signal(text: str) -> List[Dict[str, str]]:
    """Detect technologies + per-tech adoption intent from job-posting text.

    Returns list of {"tech", "category", "intent"} (intent in
    adopting/expanding/using). Empty list when no text or nothing detected.
    """
    techs = detect_technologies(text)
    if not techs:
        return []

    text_lower = text.lower()
    signals: List[Dict[str, str]] = []
    for t in techs:
        name = t["name"]
        # Count total occurrences across all this tool's aliases.
        occ = 0
        for canon, _cat, pats in _COMPILED:
            if canon == name:
                for rx in pats:
                    occ += len(rx.findall(text_lower))
                break
        signals.append({
            "tech": name,
            "category": t["category"],
            "intent": _intent_for(name, text_lower, occ),
        })
    return signals


def compute_hiring_velocity(
    open_roles: int,
    recent_roles: int = 0,
    recency_days: Optional[int] = None,
) -> Dict:
    """Derive a hiring-velocity summary from counts/recency.

    `open_roles`   — number of open postings observed.
    `recent_roles` — postings within `recency_days` (0 if unknown).
    `recency_days` — age window the recent_roles count refers to (None if N/A).

    Level buckets (by open_roles): 0=none, 1=low, 2-4=moderate, 5-9=high,
    10+=hypergrowth. Degrades gracefully — returns level "none" for 0 roles.
    """
    if open_roles >= 10:
        level = "hypergrowth"
    elif open_roles >= 5:
        level = "high"
    elif open_roles >= 2:
        level = "moderate"
    elif open_roles >= 1:
        level = "low"
    else:
        level = "none"

    return {
        "open_roles": open_roles,
        "recent_roles": recent_roles,
        "recency_days": recency_days,
        "level": level,
    }


def analyze_job_text(
    text: str,
    open_roles: Optional[int] = None,
    recent_roles: int = 0,
    recency_days: Optional[int] = None,
) -> Dict:
    """One-shot: turn job-posting text (+ optional counts) into intent signals.

    Returns a dict safe to merge into the existing `hiring_signals` JSON:
        {
          "technologies": [...names...],
          "tech_adoption_signal": [{tech, category, intent}, ...],
          "hiring_velocity": {open_roles, recent_roles, recency_days, level}
        }
    Empty/missing text yields empty technologies & tech_adoption_signal but a
    still-valid hiring_velocity (so callers never crash on partial data).
    """
    adoption = detect_tech_adoption_signal(text or "")
    velocity = None
    if open_roles is not None:
        velocity = compute_hiring_velocity(open_roles, recent_roles, recency_days)

    out: Dict = {
        "technologies": [a["tech"] for a in adoption],
        "tech_adoption_signal": adoption,
    }
    if velocity is not None:
        out["hiring_velocity"] = velocity
    return out
