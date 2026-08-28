"""Evidence-gated people research for a single named company.

This is intentionally separate from broad lead collection. It searches public
LinkedIn profile results for a named company/function and only returns a
candidate when the result text contains an exact company-employment pattern
and a matching functional remit. Search-query terms alone never count as
evidence, and contact details are never inferred.
"""

from __future__ import annotations

import asyncio
from datetime import datetime, timezone
import hashlib
import re
from typing import Any, Iterable
from urllib.parse import urlparse

from apps.api.services.leadgen.enrichment.providers.crosslinked import (
    CrossLinkedProvider,
)
from apps.api.services.leadgen.enrichment.providers.wikidata_provider import (
    WikidataProvider,
)
from apps.api.services.leadgen.enrichment.web_search import DDGS
from apps.api.services.leadgen.dedup import normalize_domain


_FUNCTION_QUERIES: dict[str, list[str]] = {
    "partnership": [
        "partnerships", "strategic partnerships", "alliances",
        "business development", "partner ecosystem",
    ],
    "alliance": ["alliances", "strategic alliances", "partnerships"],
    "business development": ["business development", "partnerships"],
    "biz dev": ["business development", "partnerships"],
    "channel": ["channel partnerships", "channel sales", "alliances"],
    "sales": ["head of sales", "sales director", "vice president sales"],
    "marketing": ["head of marketing", "marketing director", "CMO"],
    "revenue": ["chief revenue officer", "revenue operations", "VP revenue"],
    "growth": ["head of growth", "growth director", "VP growth"],
    "product": ["head of product", "product director", "VP product"],
    "engineering": ["head of engineering", "engineering director", "CTO"],
    "customer success": ["head of customer success", "customer success director"],
    "leadership": ["CEO", "founder", "vice president", "director"],
    "executive": ["CEO", "founder", "vice president", "director"],
}

_FUNCTION_EVIDENCE: dict[str, tuple[str, ...]] = {
    "partnership": (
        "partnership", "partner development", "partner ecosystem",
        "alliance", "business development", "channel partner",
    ),
    "alliance": ("alliance", "partnership"),
    "business development": ("business development", "partnership"),
    "biz dev": ("business development", "partnership"),
    "channel": ("channel", "partnership", "alliance"),
    "leadership": (
        "chief executive", "ceo", "founder", "president", "vice president",
        "director", "head of",
    ),
    "executive": (
        "chief ", "ceo", "cto", "cfo", "coo", "cmo", "president",
        "vice president", "director",
    ),
}

_LEGAL_SUFFIX_RE = re.compile(
    r"\s+(?:incorporated|inc|corp(?:oration)?|llc|ltd|limited|plc|pvt)\.?$",
    re.IGNORECASE,
)
_DOMAIN_RE = re.compile(
    r"^(?:https?://)?(?:www\.)?([a-z0-9-]+)(?:\.[a-z0-9-]+)+(?:/.*)?$",
    re.IGNORECASE,
)


def _company_aliases(company: str) -> list[str]:
    clean = re.sub(r"\s+", " ", (company or "").strip())
    domain = _DOMAIN_RE.fullmatch(clean)
    if domain:
        clean = domain.group(1).replace("-", " ")
    aliases = [clean]
    without_suffix = _LEGAL_SUFFIX_RE.sub("", clean).strip()
    if without_suffix and without_suffix.lower() != clean.lower():
        aliases.append(without_suffix)
    return [alias for alias in aliases if len(alias) >= 2]


def _identity_text(value: str) -> str:
    return re.sub(r"[^a-z0-9]+", " ", str(value or "").lower()).strip()


def person_entity_id(company: str, person: dict[str, Any]) -> str:
    """Return a stable ID for a person scoped to the canonical target company."""
    existing = str(person.get("person_id") or "").strip()
    if re.fullmatch(r"person_[a-zA-Z0-9_-]{1,56}", existing):
        return existing

    aliases = _company_aliases(company)
    company_key = _identity_text(aliases[-1] if aliases else company)
    profile_url = str(
        person.get("linkedin_url")
        or person.get("linkedin")
        or person.get("evidence_url")
        or ""
    ).strip()
    parsed = urlparse(profile_url)
    if parsed.hostname and parsed.path:
        profile_key = f"{parsed.hostname.lower()}{parsed.path.rstrip('/').lower()}"
    else:
        profile_key = ""
    name_key = _identity_text(person.get("name") or person.get("full_name") or "")
    identity_key = profile_key or name_key
    digest = hashlib.sha256(f"{company_key}|{identity_key}".encode()).hexdigest()[:24]
    return f"person_{digest}"


def people_result_set_id(
    company: str,
    function: str,
    people: Iterable[dict[str, Any]],
) -> str:
    """Identify the exact ordered-independent people set returned to Chat."""
    aliases = _company_aliases(company)
    company_key = _identity_text(aliases[-1] if aliases else company)
    function_key = _identity_text(function)
    person_ids = sorted(person_entity_id(company, person) for person in people)
    seed = "|".join((company_key, function_key, *person_ids))
    return f"people_{hashlib.sha256(seed.encode()).hexdigest()[:24]}"


async def resolve_company_identity(company: str) -> dict[str, Any]:
    """Resolve one target company without guessing through an ambiguous name."""
    company = re.sub(r"\s+", " ", (company or "").strip())[:120]
    domain_match = _DOMAIN_RE.fullmatch(company)
    if domain_match:
        parsed = urlparse(company if "://" in company else f"https://{company}")
        canonical_domain = (parsed.hostname or "").lower()
        if canonical_domain.startswith("www."):
            canonical_domain = canonical_domain[4:]
        return {
            "status": "resolved",
            "company": company,
            "canonical_domain": canonical_domain,
            "website": f"https://{canonical_domain}",
            "confidence": 1.0,
            "observed_at": datetime.now(timezone.utc).date().isoformat(),
            "evidence_url": f"https://{canonical_domain}",
            "source": "user_supplied_domain",
        }

    try:
        result = await asyncio.wait_for(
            WikidataProvider().resolve_identity(company),
            timeout=15.0,
        )
    except Exception as exc:
        return {
            "status": "resolver_unavailable",
            "company": company,
            "canonical_domain": "",
            "error_class": type(exc).__name__,
        }
    if not isinstance(result, dict):
        return {
            "status": "invalid_response",
            "company": company,
            "canonical_domain": "",
        }
    canonical_domain = normalize_domain(
        str(result.get("canonical_domain") or "").strip()
    )
    if result.get("status") != "resolved" or not canonical_domain:
        return {
            **result,
            "company": company,
            "canonical_domain": "",
        }
    return {**result, "company": company, "canonical_domain": canonical_domain}


def _exact_company_relationship(text: str, company: str) -> bool:
    """Require a company name in an employment-like context.

    The separator lookahead is important: a query for ``Stripe`` must not
    accept ``Stripe Theory`` or ``Stripe Communications``.
    """
    haystack = re.sub(r"\s+", " ", text or "")
    separator = r"(?=\s*(?:[|·,;:.()\[\]–—-]|$))"
    for alias in _company_aliases(company):
        escaped = re.escape(alias)
        patterns = (
            rf"\b(?:at|@|for|with)\s+{escaped}{separator}",
            rf"\bexperience\s*:\s*{escaped}{separator}",
            rf"(?:^|[|·;:–—-])\s*{escaped}{separator}",
        )
        if any(re.search(pattern, haystack, re.IGNORECASE) for pattern in patterns):
            return True
    return False


def _function_terms(function: str, titles: Iterable[str]) -> tuple[str, ...]:
    normalized = re.sub(r"\s+", " ", (function or "").strip().lower())
    for stem, terms in _FUNCTION_EVIDENCE.items():
        if stem in normalized:
            return terms

    terms: list[str] = []
    for title in titles:
        clean = re.sub(r"\s+", " ", str(title).strip().lower())
        clean = re.sub(
            r"\b(?:head of|vice president|vp|director of|director|manager|lead)\b",
            "",
            clean,
        ).strip(" -")
        if len(clean) >= 3:
            terms.append(clean)
    if normalized:
        terms.append(normalized)
    return tuple(dict.fromkeys(terms))


def _function_evidence_strength(
    evidence_title: str,
    evidence_snippet: str,
    function_terms: tuple[str, ...],
    company: str,
) -> tuple[str, float] | None:
    """Accept functional remit, not a coincidental mention in a profile post.

    Search snippets frequently contain a person's recent posts. A Stripe
    employee posting about a partnership is not necessarily on the partnership
    team, so arbitrary snippet term matches are rejected. Accepted evidence is
    limited to the profile headline, the summary immediately preceding an
    explicit ``Experience: <company>`` marker, or a first-person responsibility
    statement.
    """
    title = evidence_title.lower()
    snippet = evidence_snippet.lower()
    if any(term in title for term in function_terms):
        return "profile_headline", 0.78

    experience_index = snippet.find("experience:")
    if experience_index >= 0:
        summary = snippet[:experience_index]
        experience = snippet[experience_index:]
        if (
            any(term in summary for term in function_terms)
            and _exact_company_relationship(experience, company)
        ):
            return "profile_summary_plus_experience", 0.72

    responsibility = re.search(
        r"\bi\s+(?:currently\s+)?(?:lead|head|manage|run|build|own|oversee|"
        r"am\s+(?:directly\s+)?responsible\s+for)\b.{0,220}",
        snippet,
        re.IGNORECASE,
    )
    if responsibility and any(
        term in responsibility.group(0).lower() for term in function_terms
    ):
        return "first_person_responsibility", 0.74

    return None


def _query_titles(function: str, titles: Iterable[str]) -> list[str]:
    supplied = [str(title).strip() for title in titles if str(title).strip()]
    if supplied:
        return supplied[:5]
    normalized = re.sub(r"\s+", " ", (function or "").strip().lower())
    for stem, queries in _FUNCTION_QUERIES.items():
        if stem in normalized:
            return queries[:5]
    return [function.strip()] if function.strip() else ["leadership"]


async def research_people_at_company(
    company: str,
    function: str,
    *,
    titles: Iterable[str] = (),
    location: str = "",
    limit: int = 8,
) -> dict[str, Any]:
    """Return evidence-gated people candidates without persisting anything."""
    company = re.sub(r"\s+", " ", (company or "").strip())[:120]
    function = re.sub(r"\s+", " ", (function or "").strip())[:100]
    if not company:
        return {"ok": False, "error": "company_required", "people": [], "count": 0}
    if not function:
        return {"ok": False, "error": "function_required", "people": [], "count": 0}

    limit = max(1, min(int(limit or 8), 15))
    query_titles = _query_titles(function, titles)
    identity_task = asyncio.create_task(resolve_company_identity(company))
    provider = CrossLinkedProvider(max_people=max(limit * 3, 15), delay=0.2)
    try:
        discovered, searches_used = await asyncio.wait_for(
            provider.find_people_by_titles(
                company_name=company,
                titles=query_titles,
                geo=(location or "").strip()[:80],
                max_people=max(limit * 3, 15),
                max_searches=5,
                results_per_search=12,
            ),
            timeout=45.0,
        )
    except asyncio.TimeoutError:
        company_resolution = await identity_task
        return {
            "ok": False,
            "error": "people_research_timeout",
            "company": company,
            "function": function,
            "company_resolution": company_resolution,
            "people": [],
            "count": 0,
            "message": "Targeted people research exceeded its 45-second safety limit.",
        }
    except Exception:
        if not identity_task.done():
            identity_task.cancel()
        await asyncio.gather(identity_task, return_exceptions=True)
        raise

    company_resolution = await identity_task
    canonical_domain = (
        str(company_resolution.get("canonical_domain") or "").strip().lower()
        if company_resolution.get("status") == "resolved"
        else ""
    )

    function_terms = _function_terms(function, query_titles)
    retrieved_at = datetime.now(timezone.utc).date().isoformat()
    accepted: list[dict[str, Any]] = []
    rejected_company = rejected_function = 0

    for person in discovered:
        evidence_title = (person.get("evidence_title") or "").strip()
        evidence_snippet = (person.get("evidence_snippet") or "").strip()
        evidence = f"{evidence_title}. {evidence_snippet}".strip()
        if not _exact_company_relationship(evidence, company):
            rejected_company += 1
            continue
        function_evidence = _function_evidence_strength(
            evidence_title,
            evidence_snippet,
            function_terms,
            company,
        )
        if not function_evidence:
            rejected_function += 1
            continue

        title = (person.get("title") or "").strip()
        if title.upper() == "N/A":
            title = ""
        if any(title.lower() == alias.lower() for alias in _company_aliases(company)):
            title = ""
        verification_status, confidence = function_evidence
        candidate = {
            "name": (person.get("name") or "").strip(),
            "title": title,
            "company": company,
            "canonical_company_domain": canonical_domain,
            "company_resolution": company_resolution,
            "function": function,
            "location": (location or "").strip(),
            "linkedin_url": person.get("linkedin") or "",
            "evidence_url": person.get("evidence_url") or person.get("linkedin") or "",
            "evidence_title": evidence_title[:240],
            "evidence_snippet": evidence_snippet[:280],
            "retrieved_at": retrieved_at,
            "confidence": confidence,
            "verification_status": verification_status,
            "email": None,
        }
        candidate["person_id"] = person_entity_id(company, candidate)
        accepted.append(candidate)
        if len(accepted) >= limit:
            break

    return {
        "ok": True,
        "company": company,
        "function": function,
        "company_resolution": company_resolution,
        "result_set_id": people_result_set_id(company, function, accepted),
        "people": accepted,
        "count": len(accepted),
        "searches_used": searches_used,
        "candidates_rejected": {
            "company_relationship_missing": rejected_company,
            "function_evidence_missing": rejected_function,
        },
        "methodology": (
            "Public LinkedIn-profile search results; exact target-company employment "
            "and function terms are both required. No email addresses are guessed."
        ),
        "warning": (
            "Public profile snippets can be stale. Treat matches as research candidates "
            "and re-check the profile before outreach."
        ),
    }


async def _public_search(query: str, max_results: int = 8) -> list[dict]:
    def _run():
        with DDGS(timeout=10) as search:
            return list(search.text(query, max_results=max_results))

    try:
        return await asyncio.wait_for(asyncio.to_thread(_run), timeout=16.0)
    except (asyncio.TimeoutError, Exception):
        return []


def _exact_name_match(text: str, name: str) -> bool:
    compact_text = re.sub(r"\s+", " ", (text or "").lower())
    compact_name = re.sub(r"\s+", " ", (name or "").strip().lower())
    return bool(compact_name and re.search(rf"\b{re.escape(compact_name)}\b", compact_text))


def _verification_company_match(text: str, company: str, name: str) -> bool:
    if _exact_company_relationship(text, company):
        return True
    compact = re.sub(r"\s+", " ", text or "")
    for alias in _company_aliases(company):
        company_then_person = re.search(
            rf"\b{re.escape(alias)}\b.{{0,100}}\b(?:appointed|named|hired|promoted|"
            rf"welcomed|selected)\b.{{0,100}}\b{re.escape(name)}\b",
            compact,
            re.IGNORECASE,
        )
        person_then_company = re.search(
            rf"\b{re.escape(name)}\b.{{0,100}}\b(?:joined|joins|leads?|heads?|runs?|"
            rf"works?\s+(?:at|for))\b.{{0,80}}\b{re.escape(alias)}\b",
            compact,
            re.IGNORECASE,
        )
        if company_then_person or person_then_company:
            return True
    return False


def _verification_function_match(
    hit_title: str,
    snippet: str,
    company: str,
    name: str,
    function_terms: tuple[str, ...],
) -> bool:
    """Require the function inside a headline or employment clause.

    A bio can mention partnerships or alliances as historical subject matter
    while the person's actual role is legal, engineering, etc. Arbitrary page
    mentions therefore do not verify functional ownership.
    """
    if any(term in hit_title.lower() for term in function_terms):
        return True
    compact = re.sub(r"\s+", " ", snippet or "")
    for alias in _company_aliases(company):
        clauses = re.findall(
            rf"\b{re.escape(name)}\b.{{0,180}}?\b(?:at|for)\s+{re.escape(alias)}\b",
            compact,
            re.IGNORECASE,
        )
        clauses += re.findall(
            rf"\b{re.escape(alias)}\b.{{0,180}}?\b{re.escape(name)}\b",
            compact,
            re.IGNORECASE,
        )
        if any(term in clause.lower() for clause in clauses for term in function_terms):
            return True
    return _function_evidence_strength(
        hit_title, snippet, function_terms, company
    ) is not None


async def verify_people_at_company(
    company: str,
    function: str,
    people: Iterable[dict],
    *,
    max_people: int = 15,
) -> dict[str, Any]:
    """Re-check people against fresh public search evidence.

    This is role verification, not contact enrichment. Independent sources
    corroborate a candidate; a fresh LinkedIn/profile hit can only reconfirm
    profile evidence. Absence of corroboration remains explicit.
    """
    company = re.sub(r"\s+", " ", (company or "").strip())[:120]
    function = re.sub(r"\s+", " ", (function or "").strip())[:100]
    candidates = [dict(person) for person in people if isinstance(person, dict)][:max_people]
    for person in candidates:
        person["person_id"] = person_entity_id(company, person)
    checked_at = datetime.now(timezone.utc).date().isoformat()
    function_terms = _function_terms(function, ())
    semaphore = asyncio.Semaphore(3)

    async def _verify_one(person: dict) -> dict:
        name = re.sub(r"\s+", " ", str(person.get("name") or "").strip())[:120]
        if not name:
            return {
                **person,
                "verification_status": "not_corroborated",
                "verification_confidence": 0.0,
                "verification_sources": [],
                "checked_at": checked_at,
            }
        title = re.sub(r"\s+", " ", str(person.get("title") or "").strip())[:140]
        query = f'"{name}" "{company}" "{function}"'
        async with semaphore:
            hits = await _public_search(query, max_results=8)

        sources: list[dict[str, str]] = []
        original_url = str(person.get("linkedin_url") or person.get("evidence_url") or "").rstrip("/").lower()
        for hit in hits:
            url = str(hit.get("href") or hit.get("url") or "").strip()
            hit_title = str(hit.get("title") or "").strip()
            snippet = str(hit.get("body") or hit.get("content") or "").strip()
            evidence = f"{hit_title}. {snippet}"
            if not _exact_name_match(evidence, name):
                continue
            if not _verification_company_match(evidence, company, name):
                continue
            if not _verification_function_match(
                hit_title, snippet, company, name, function_terms
            ):
                continue
            host = (urlparse(url).hostname or "").lower()
            normalized_url = url.rstrip("/").lower()
            source_type = "linkedin_profile" if "linkedin.com" in host else "independent_web"
            if normalized_url == original_url:
                source_type = "linkedin_profile"
            sources.append({
                "url": url,
                "title": hit_title[:240],
                "snippet": snippet[:320],
                "source_type": source_type,
            })
            if len(sources) >= 3:
                break

        independent = [s for s in sources if s["source_type"] == "independent_web"]
        profile = [s for s in sources if s["source_type"] == "linkedin_profile"]
        if independent:
            status = "independent_role_evidence"
            confidence = 0.85
        elif profile:
            status = "profile_reconfirmed"
            confidence = max(0.75, float(person.get("confidence") or 0))
        else:
            status = "not_corroborated"
            confidence = min(0.5, float(person.get("confidence") or 0.5))
        return {
            **person,
            "title": title,
            "verification_status": status,
            "verification_confidence": confidence,
            "verification_sources": sources,
            "checked_at": checked_at,
        }

    try:
        verified = await asyncio.wait_for(
            asyncio.gather(*[_verify_one(person) for person in candidates]),
            timeout=40.0,
        )
    except asyncio.TimeoutError:
        verified = [{
            **person,
            "verification_status": "verification_timeout",
            "verification_confidence": 0.0,
            "verification_sources": [],
            "checked_at": checked_at,
        } for person in candidates]

    summary = {
        status: sum(1 for person in verified if person.get("verification_status") == status)
        for status in (
            "independent_role_evidence", "profile_reconfirmed",
            "not_corroborated", "verification_timeout",
        )
    }
    company_resolution = next(
        (
            dict(person.get("company_resolution"))
            for person in candidates
            if isinstance(person.get("company_resolution"), dict)
            and person["company_resolution"].get("status") == "resolved"
        ),
        {
            "status": "unresolved",
            "company": company,
            "canonical_domain": "",
        },
    )
    return {
        "ok": True,
        "company": company,
        "function": function,
        "company_resolution": company_resolution,
        "result_set_id": people_result_set_id(company, function, verified),
        "people": verified,
        "count": len(verified),
        "summary": summary,
        "checked_at": checked_at,
        "methodology": (
            "Fresh exact-name/company/function web searches. Independent pages can "
            "support a role; LinkedIn-only matches remain profile-level evidence."
        ),
        "warning": "Not corroborated does not prove a person is incorrect; it means fresh supporting evidence was not found.",
    }
