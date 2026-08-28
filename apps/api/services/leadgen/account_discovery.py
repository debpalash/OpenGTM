"""Structured account-discovery briefs and evidence-gated row matching."""

from __future__ import annotations

from datetime import datetime, timezone
import hashlib
import json
import re
from typing import Any, Mapping
from urllib.parse import urlsplit

from apps.api.services.leadgen.dedup import normalize_domain


_COUNT_RE = re.compile(
    r"\b(?:find|source|list|build(?:\s+(?:me\s+)?)?(?:a\s+list\s+of)?|show)\s+"
    r"(?P<count>\d{1,4})\b",
    re.IGNORECASE,
)
_COMPANY_TYPE_RE = re.compile(
    r"(?:\b\d{1,4}\s+|\b(?:find|source|list|show)\s+(?:\d{1,4}\s+)?)"
    r"(?P<company_type>.+?)\s+(?:companies|businesses|accounts)\b",
    re.IGNORECASE,
)
_GEOGRAPHY_RE = re.compile(
    r"\bin\s+(?P<geography>[a-z][a-z .'-]{1,60}?)"
    r"(?=\s+(?:that|which|who|using|with|and\s+(?:are|that))\b|[,.!?]|$)",
    re.IGNORECASE,
)
_TECHNOLOGY_RE = re.compile(
    r"\b(?:use|uses|using|powered\s+by)\s+"
    r"(?P<technology>[a-z0-9][a-z0-9 .+_-]{0,60}?)"
    r"(?=\s+(?:and|that|which|who|while)\b|[,.!?]|$)",
    re.IGNORECASE,
)
_HIRING_RE = re.compile(
    r"\b(?:are\s+)?hiring(?:\s+for)?\s+"
    r"(?P<role>[a-z0-9][a-z0-9 &/+_-]{0,80}?)"
    r"(?:\s+roles?|\s+positions?|\s+jobs?)?(?=[,.!?]|$)",
    re.IGNORECASE,
)
_MARKET_REQUEST_RE = re.compile(
    r"\b(?:find|source|list|build|show)\b.*\b\d{1,4}\b.*"
    r"\b(?:companies|businesses|accounts)\b",
    re.IGNORECASE,
)
_TOKEN_RE = re.compile(r"[a-z0-9]+")
_STOPWORDS = frozenset({"a", "an", "and", "business", "businesses", "company", "companies", "the"})


def _clean(value: Any) -> str:
    return re.sub(r"\s+", " ", str(value or "").strip())


def _one(value: str) -> list[str]:
    value = _clean(value).strip(" ,.")
    return [value] if value else []


def _canonical_payload(brief: Mapping[str, Any]) -> dict[str, Any]:
    return {
        "requested_count": int(brief.get("requested_count") or 0),
        "company_types": list(brief.get("company_types") or []),
        "geographies": list(brief.get("geographies") or []),
        "technologies": list(brief.get("technologies") or []),
        "hiring_roles": list(brief.get("hiring_roles") or []),
        "exclusions": list(brief.get("exclusions") or []),
    }


def account_discovery_brief_id(brief: Mapping[str, Any]) -> str:
    payload = json.dumps(_canonical_payload(brief), sort_keys=True, separators=(",", ":"))
    return f"accounts_{hashlib.sha256(payload.encode()).hexdigest()[:24]}"


def build_account_discovery_brief(query: str) -> dict[str, Any]:
    """Parse a bounded market request without relying on an LLM."""
    query = _clean(query)[:500]
    count_match = _COUNT_RE.search(query)
    type_match = _COMPANY_TYPE_RE.search(query)
    geo_match = _GEOGRAPHY_RE.search(query)
    tech_match = _TECHNOLOGY_RE.search(query)
    hiring_match = _HIRING_RE.search(query)
    requested_count = int(count_match.group("count")) if count_match else 0
    requested_count = max(0, min(requested_count, 500))
    company_types = _one(type_match.group("company_type")) if type_match else []
    geographies = _one(geo_match.group("geography")) if geo_match else []
    technologies = _one(tech_match.group("technology")) if tech_match else []
    hiring_roles = _one(hiring_match.group("role")) if hiring_match else []
    missing = []
    if not requested_count:
        missing.append("requested_count")
    if not company_types:
        missing.append("company_type")

    brief = {
        "schema_version": "1.0",
        "original_query": query,
        "requested_count": requested_count,
        "company_types": company_types,
        "geographies": geographies,
        "technologies": technologies,
        "hiring_roles": hiring_roles,
        "exclusions": [],
        "evidence_requirements": [
            "canonical_company_domain",
            "criterion_evidence",
            "evidence_url",
            "retrieved_at",
            "field_confidence",
        ],
        "missing_fields": missing,
        "complete": not missing,
    }
    brief["brief_id"] = account_discovery_brief_id(brief)
    return brief


def extract_account_discovery_request(query: str) -> dict[str, Any] | None:
    """Return a complete explicit list request that can be routed deterministically."""
    if not _MARKET_REQUEST_RE.search(_clean(query)):
        return None
    brief = build_account_discovery_brief(query)
    return brief if brief["complete"] else None


def _tokens(value: Any) -> set[str]:
    tokens = set()
    for token in _TOKEN_RE.findall(_clean(value).lower()):
        if token in _STOPWORDS:
            continue
        if len(token) > 4 and token.endswith("s") and not token.endswith("ss") and token != "saas":
            token = token[:-1]
        tokens.add(token)
    return tokens


def _flatten(value: Any) -> str:
    if isinstance(value, (dict, list, tuple)):
        return json.dumps(value, default=str, sort_keys=True)
    return _clean(value)


def _http_url(value: Any) -> str:
    raw = _clean(value)
    parsed = urlsplit(raw)
    return raw if parsed.scheme in {"http", "https"} and parsed.netloc else ""


def evaluate_account_fit(
    lead: Mapping[str, Any],
    brief: Mapping[str, Any],
) -> dict[str, Any]:
    """Require positive evidence for every requested G1 criterion."""
    company = _clean(lead.get("company"))
    website = _clean(lead.get("website"))
    canonical_domain = normalize_domain(website)
    source_url = _http_url(lead.get("source_url"))
    website_url = _http_url(
        website if website.startswith(("http://", "https://")) else f"https://{website}"
    ) if website else ""
    retrieved_at = _clean(lead.get("updated_at") or lead.get("created_at"))

    reasons: list[str] = []
    criteria: dict[str, dict[str, Any]] = {}
    rejections: list[str] = []

    type_text = " ".join(_flatten(lead.get(field)) for field in (
        "specialization", "industry_tags", "description",
    )).lower()
    for company_type in brief.get("company_types") or []:
        required = _tokens(company_type)
        matched = bool(required) and required.issubset(_tokens(type_text))
        criteria[f"company_type:{company_type}"] = {
            "matched": matched,
            "source_fields": ["specialization", "industry_tags", "description"],
            "evidence_url": source_url or website_url,
        }
        if matched:
            reasons.append(f"Company type evidence matches {company_type}")
        else:
            rejections.append("company_type_missing")

    geo_text = " ".join(_flatten(lead.get(field)) for field in (
        "city", "state", "address",
    )).lower()
    for geography in brief.get("geographies") or []:
        required = _tokens(geography)
        matched = bool(required) and required.issubset(_tokens(geo_text))
        criteria[f"geography:{geography}"] = {
            "matched": matched,
            "source_fields": ["city", "state", "address"],
            "evidence_url": source_url or website_url,
        }
        if matched:
            reasons.append(f"Geography evidence matches {geography}")
        else:
            rejections.append("geography_missing")

    technology_text = " ".join(_flatten(lead.get(field)) for field in (
        "technologies", "technographics",
    )).lower()
    for technology in brief.get("technologies") or []:
        required = _tokens(technology)
        matched = bool(required) and required.issubset(_tokens(technology_text))
        criteria[f"technology:{technology}"] = {
            "matched": matched,
            "source_fields": ["technologies", "technographics"],
            "evidence_url": source_url,
        }
        if matched and source_url:
            reasons.append(f"Technology evidence matches {technology}")
        else:
            rejections.append("technology_evidence_missing")

    hiring_text = _flatten(lead.get("hiring_signals")).lower()
    for role in brief.get("hiring_roles") or []:
        required = _tokens(role)
        matched = bool(required) and required.issubset(_tokens(hiring_text))
        criteria[f"hiring:{role}"] = {
            "matched": matched,
            "source_fields": ["hiring_signals"],
            "evidence_url": source_url,
        }
        if matched and source_url:
            reasons.append(f"Hiring evidence matches {role}")
        else:
            rejections.append("hiring_evidence_missing")

    if not company:
        rejections.append("company_name_missing")
    if not canonical_domain:
        rejections.append("canonical_domain_missing")
    if not source_url:
        rejections.append("evidence_url_missing")
    if not retrieved_at:
        rejections.append("retrieval_time_missing")

    raw_score = lead.get("score")
    confidence = (
        round(max(0.0, min(float(raw_score) / 100.0, 1.0)), 4)
        if isinstance(raw_score, (int, float)) and not isinstance(raw_score, bool)
        else 0.0
    )
    accepted = not rejections
    evidence_urls = list(dict.fromkeys(url for url in (source_url, website_url) if url))
    return {
        "accepted": accepted,
        "company": company,
        "canonical_domain": canonical_domain,
        "fit_reasons": reasons,
        "criteria_evidence": criteria,
        "evidence_urls": evidence_urls,
        "retrieved_at": retrieved_at,
        "field_confidence": confidence,
        "rejection_reasons": list(dict.fromkeys(rejections)),
        "evaluated_at": datetime.now(timezone.utc).isoformat(),
    }
