"""Intent classification for lead-collection requests.

The broad collection pipeline is appropriate for market searches such as
``IT staffing companies in Pune``.  It is *not* an all-purpose web research
engine: a bare domain can mean company research, people discovery, technology
usage, or monitoring.  Sending that ambiguity through every directory, job
board, and review source creates page titles masquerading as leads.

This module is deliberately deterministic and runs before a job row or durable
queue entry is created.  An LLM may later enrich an already-explicit intent,
but availability of an LLM must never decide whether a request is safe to run.
"""

from __future__ import annotations

from dataclasses import asdict, dataclass
import re
from typing import Literal, Optional
from urllib.parse import urlparse


CollectionIntent = Literal[
    "market_search",
    "company_research",
    "people_at_company",
    "technology_users",
    "signal_monitor",
]

SPECIALIZED_INTENTS = {
    "company_research",
    "people_at_company",
    "technology_users",
    "signal_monitor",
}

_DOMAIN_RE = re.compile(
    r"^(?:https?://)?(?:www\.)?"
    r"(?P<domain>[a-z0-9](?:[a-z0-9-]{0,62})(?:\.[a-z0-9](?:[a-z0-9-]{0,62}))+?)"
    r"(?:/)?$",
    re.IGNORECASE,
)

_DOMAIN_IN_TEXT_RE = re.compile(
    r"(?<![@\w.-])(?:https?://)?(?:www\.)?"
    r"(?P<domain>[a-z0-9](?:[a-z0-9-]{0,62})(?:\.[a-z0-9](?:[a-z0-9-]{0,62}))+)"
    r"(?:/[^\s]*)?",
    re.IGNORECASE,
)


@dataclass(frozen=True)
class IntentOption:
    key: str
    intent: CollectionIntent
    label: str
    description: str
    route: str
    draft: str

    def to_dict(self) -> dict:
        return asdict(self)


@dataclass(frozen=True)
class CollectionDecision:
    query: str
    intent: CollectionIntent
    domain: str = ""
    entity: str = ""
    clarification_kind: str = ""
    clarification_required: bool = False
    reason: str = ""
    options: tuple[IntentOption, ...] = ()

    @property
    def can_collect(self) -> bool:
        return self.intent == "market_search" and not self.clarification_required

    def to_dict(self) -> dict:
        return {
            "query": self.query,
            "intent": self.intent,
            "domain": self.domain,
            "entity": self.entity,
            "clarification_kind": self.clarification_kind,
            "clarification_required": self.clarification_required,
            "reason": self.reason,
            "options": [option.to_dict() for option in self.options],
        }


def normalize_domain(value: str) -> str:
    """Return a normalized host or ``""`` when *value* is not a domain URL."""
    raw = (value or "").strip()
    match = _DOMAIN_RE.fullmatch(raw)
    if not match:
        return ""
    domain = match.group("domain").lower().rstrip(".")
    # urlparse handles IDN/port oddities more safely once a scheme is present.
    parsed = urlparse(f"https://{domain}")
    return (parsed.hostname or "").lower()


def domain_in_query(query: str) -> str:
    """Extract the first domain mentioned in arbitrary query text."""
    bare = normalize_domain(query)
    if bare:
        return bare
    match = _DOMAIN_IN_TEXT_RE.search((query or "").strip())
    return match.group("domain").lower().rstrip(".") if match else ""


def _brand(domain: str) -> str:
    label = domain.split(".")[0].replace("-", " ").strip()
    return label.title() or domain


def clarification_options(domain: str) -> tuple[IntentOption, ...]:
    brand = _brand(domain)
    return (
        IntentOption(
            key="company_research",
            intent="company_research",
            label=f"Research {brand}",
            description="Build one evidence-backed company profile; do not create page-title leads.",
            route="/chat",
            draft=f"Research the company at {domain}. Build one evidence-backed company profile with products, market, size, leadership, customers, recent signals, and cited sources.",
        ),
        IntentOption(
            key="people_at_company",
            intent="people_at_company",
            label=f"Find people at {brand}",
            description="Find named decision-makers who are demonstrably associated with this company.",
            route="/chat",
            draft=f"Find decision-makers at the company {domain}. Return named people with title, LinkedIn evidence, and only company-attributable contact data.",
        ),
        IntentOption(
            key="technology_users",
            intent="technology_users",
            label=f"Find {brand} users",
            description="Find actual companies using the product; keep list vendors and articles as evidence only.",
            route="/chat",
            draft=f"Find companies that use the technology or product represented by {domain}. Return actual customer companies, not directories or list vendors, and include evidence for each relationship.",
        ),
        IntentOption(
            key="signal_monitor",
            intent="signal_monitor",
            label=f"Monitor {brand}",
            description="Create a watch for company, hiring, product, pricing, and leadership changes.",
            route="/watches",
            draft=domain,
        ),
    )


def _specialized_route_option(
    intent: CollectionIntent,
    query: str,
) -> tuple[IntentOption, ...]:
    """Build the single safe route for an explicit non-domain request."""
    if intent == "signal_monitor":
        return (IntentOption(
            key="signal_monitor",
            intent=intent,
            label="Create a signal watch",
            description="Monitor the target instead of running a one-time lead search.",
            route="/watches",
            draft=query,
        ),)
    labels = {
        "company_research": "Open company research",
        "people_at_company": "Find people in Chat",
        "technology_users": "Research technology users",
    }
    descriptions = {
        "company_research": "Build one evidence-backed account profile.",
        "people_at_company": "Find named, attributable decision-makers.",
        "technology_users": "Find companies with evidence of actual product usage.",
    }
    if intent not in labels:
        return ()
    return (IntentOption(
        key=intent,
        intent=intent,
        label=labels[intent],
        description=descriptions[intent],
        route="/chat",
        draft=query,
    ),)


_TEAM_FUNCTIONS = (
    r"partnerships?", r"alliances?", r"business development", r"biz dev",
    r"channel(?: partnerships?)?", r"partner ecosystem", r"leadership",
    r"executive", r"sales", r"marketing", r"revenue", r"growth",
    r"engineering", r"product", r"customer success",
)
_TEAM_FUNCTION_RE = "(?:" + "|".join(_TEAM_FUNCTIONS) + ")"
_COMPANY_TEAM_RE = re.compile(
    rf"^(?:find|show|map|research|identify)?\s*"
    rf"(?P<entity>[a-z0-9][a-z0-9&.'-]*(?:\s+[a-z0-9][a-z0-9&.'-]*){{0,4}}?)\s+"
    rf"(?P<function>{_TEAM_FUNCTION_RE})\s+"
    r"(?:teams?|org(?:anization|anisation)?|leaders?|leadership|people|contacts?|executives?)"
    r"(?:\s+(?:in|across)\s+.+)?[?.!]*$",
    re.IGNORECASE,
)
_GENERIC_MARKET_ENTITIES = {
    "b2b", "companies", "company", "fintech", "saas", "software",
    "startups", "technology", "tech", "it", "enterprise", "businesses",
}


@dataclass(frozen=True)
class CompanyTeamRequest:
    entity: str
    function: str


def extract_company_team_request(query: str) -> Optional[CompanyTeamRequest]:
    """Parse terse named-company org requests such as ``Stripe partnership teams``.

    These noun phrases are unsafe market-search inputs: the first token is a
    target account, while the rest describes an org function. The recognizer
    stays intentionally narrow so ordinary market searches still collect.
    """
    clean = re.sub(r"\s+", " ", (query or "").strip())
    match = _COMPANY_TEAM_RE.fullmatch(clean)
    if not match:
        return None
    entity = match.group("entity").strip(" .")
    if entity.lower() in _GENERIC_MARKET_ENTITIES:
        return None
    return CompanyTeamRequest(
        entity=entity,
        function=match.group("function").strip().lower(),
    )


_EXPLICIT_PEOPLE_RE = re.compile(
    r"^(?:find|show|identify)\s+(?:named\s+)?people\s+currently\s+working\s+on\s+"
    r"(?P<entity>.+?)(?:'s|’s)\s+(?P<function>.+?)\s+team(?:\.|\s|$)",
    re.IGNORECASE,
)


def extract_explicit_people_request(query: str) -> Optional[CompanyTeamRequest]:
    """Recognize the explicit people-workflow draft emitted by this router."""
    clean = re.sub(r"\s+", " ", (query or "").strip())
    match = _EXPLICIT_PEOPLE_RE.match(clean)
    if not match:
        return None
    entity = match.group("entity").strip(" .")
    function = match.group("function").strip(" .").lower()
    if not entity or not function:
        return None
    return CompanyTeamRequest(entity=entity, function=function)


def company_team_options(request: CompanyTeamRequest) -> tuple[IntentOption, ...]:
    """Offer the three distinct jobs hidden in an ambiguous org-team phrase."""
    entity = request.entity
    function = request.function
    label_function = function.title()
    return (
        IntentOption(
            key="team_people",
            intent="people_at_company",
            label=f"People on {label_function}",
            description=(
                f"Find named people at {entity} and require public evidence of both "
                f"current employment and a {function} remit."
            ),
            route="/chat",
            draft=(
                f"Find named people currently working on {entity}'s {function} team. "
                f"Require evidence for current employment and their {function} remit. "
                "Return name, title, location, LinkedIn profile, evidence URL, retrieval "
                "date, and confidence. Reject similarly named companies and do not guess emails."
            ),
        ),
        IntentOption(
            key="partner_ecosystem",
            intent="company_research",
            label=f"{entity} partner ecosystem",
            description=(
                f"Find organizations with documented {entity} partner relationships, "
                "not people or lookalike companies."
            ),
            route="/chat",
            draft=(
                f"Research {entity}'s partner ecosystem. Return organizations with a "
                f"documented partnership with {entity}, the relationship type, evidence URL, "
                "evidence date, and confidence. Reject directories and unsupported claims."
            ),
        ),
        IntentOption(
            key="team_overview",
            intent="company_research",
            label=f"Overview of {label_function}",
            description=(
                f"Explain how {entity}'s {function} organization and programs appear to work."
            ),
            route="/chat",
            draft=(
                f"Build an evidence-backed overview of {entity}'s {function} organization "
                "and programs. Separate verified facts from inference and cite every material claim."
            ),
        ),
    )


def infer_collection_intent(query: str) -> CollectionIntent:
    """Classify an explicit natural-language request without an LLM."""
    text = re.sub(r"\s+", " ", (query or "").strip().lower())

    if any(term in text for term in (
        "monitor ", "watch ", "track changes", "alert me", "signals for",
    )):
        return "signal_monitor"

    if extract_explicit_people_request(query) or any(term in text for term in (
        "people at", "person at", "decision makers at", "decision-makers at",
        "employees at", "leadership at", "executives at", "contacts at",
    )):
        return "people_at_company"

    uses_relation = bool(re.search(
        r"\b(?:companies|businesses|websites|customers|merchants|teams)\b.*"
        r"\b(?:use|using|uses|adopted|powered by)\b",
        text,
    )) or bool(re.search(
        r"\b(?:use|using|uses)\b.*\b(?:companies|businesses|websites|customers|merchants)\b",
        text,
    ))
    if uses_relation:
        return "technology_users"

    if any(term in text for term in (
        "research ", "company profile", "analyze company", "analyse company",
        "tell me about", "dossier", "account brief",
    )):
        return "company_research"

    return "market_search"


def decide_collection(
    query: str,
    requested_intent: Optional[str] = None,
) -> CollectionDecision:
    """Return the safe routing decision for a collection request.

    A bare domain is always ambiguous unless the caller supplied an explicit
    intent.  Specialized intents never enter the broad market collector; their
    route is returned to the caller instead.
    """
    clean = re.sub(r"\s+", " ", (query or "").strip())
    domain = domain_in_query(clean)
    bare_domain = bool(normalize_domain(clean))
    team_request = extract_company_team_request(clean)

    if team_request:
        return CollectionDecision(
            query=clean,
            intent="people_at_company",
            domain=domain,
            entity=team_request.entity,
            clarification_kind="company_team",
            clarification_required=True,
            reason=(
                f"“{clean}” can mean people on the team, partner companies, or "
                "an overview of the organization."
            ),
            options=company_team_options(team_request),
        )

    inferred_intent = infer_collection_intent(clean)
    intent: CollectionIntent
    if requested_intent in {
        "market_search", "company_research", "people_at_company",
        "technology_users", "signal_monitor",
    }:
        intent = requested_intent  # type: ignore[assignment]
    else:
        intent = inferred_intent

    # A caller cannot relabel an obviously specialized request as a market
    # search to bypass routing. Explicit specialized intent may narrow an
    # ambiguous request, but broad collection is never a force flag.
    if intent == "market_search" and inferred_intent in SPECIALIZED_INTENTS:
        intent = inferred_intent

    if bare_domain and requested_intent is None:
        return CollectionDecision(
            query=clean,
            intent="company_research",
            domain=domain,
            entity=_brand(domain),
            clarification_kind="bare_domain",
            clarification_required=True,
            reason="A domain is a company target, not a market-search specification.",
            options=clarification_options(domain),
        )

    if intent in SPECIALIZED_INTENTS:
        options = (
            tuple(option for option in clarification_options(domain)
                  if option.intent == intent)
            if domain else _specialized_route_option(intent, clean)
        )
        return CollectionDecision(
            query=clean,
            intent=intent,
            domain=domain,
            clarification_required=True,
            reason=(
                "This request needs a specialized GTM workflow and cannot safely "
                "run through the broad lead collector."
            ),
            options=options,
        )

    if bare_domain:
        return CollectionDecision(
            query=clean,
            intent="market_search",
            domain=domain,
            clarification_required=True,
            reason="A bare domain cannot be forced into market-search mode.",
            options=clarification_options(domain),
        )

    return CollectionDecision(query=clean, intent="market_search", domain=domain)
