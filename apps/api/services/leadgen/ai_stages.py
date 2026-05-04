"""
AI Pipeline Stages — LLM-powered extraction, validation, and scoring.

These functions replace the regex/heuristic approaches with intelligent
LLM calls for higher-quality lead data.
"""

import re
from typing import Optional
from apps.api.services.leadgen.llm import LLMClient
from apps.api.services.leadgen.models import Lead


# ── Page Text Cleaning ───────────────────────────────────────────────

def clean_page_text(html: str, max_chars: int = 3000) -> str:
    """Strip HTML tags and navigation noise, return clean text for LLM."""
    try:
        from bs4 import BeautifulSoup
        soup = BeautifulSoup(html, "html.parser")

        # Remove script, style, nav, footer, header elements
        for tag in soup.find_all(["script", "style", "nav", "footer", "header", "noscript", "iframe"]):
            tag.decompose()

        text = soup.get_text(separator=" ", strip=True)
    except Exception:
        # Fallback: strip tags with regex
        text = re.sub(r'<[^>]+>', ' ', html)

    # Collapse whitespace
    text = re.sub(r'\s+', ' ', text).strip()

    # Truncate to max_chars
    return text[:max_chars]


# ── AI Query Expansion ───────────────────────────────────────────────

async def ai_expand_query(client: LLMClient, query: str) -> list[str]:
    """Use LLM to generate optimized search queries.

    Instead of hardcoded suffix variations, the LLM generates
    search-optimized queries that target actual company websites.
    """
    data = await client.extract_json(
        f"""Generate exactly 4 DuckDuckGo search queries to find real companies for: "{query}"

Rules:
- Each query should find actual company WEBSITES, not directories or listicles
- Include the company type and location from the original query
- Vary the keywords: use "company", "pvt ltd", "agency", "services" etc.
- One query should target the company's about/contact page
- Do NOT include queries for "top 10" or "best" lists

Return JSON: {{"queries": ["query1", "query2", "query3", "query4"]}}""",
        system="You are a B2B lead generation expert. Generate search queries that find company websites directly.",
        max_tokens=200,
    )

    queries = data.get("queries", [])
    if queries and len(queries) >= 2:
        return queries[:4]

    # Fallback to original query if LLM fails
    return [query]


# ── AI Page Extraction ───────────────────────────────────────────────

async def ai_extract_company(client: LLMClient, page_text: str, url: str, search_query: str = "") -> Optional[dict]:
    """Extract structured company data from a webpage using LLM.

    Returns a dict with company_name, description, email, phone, city,
    specialization, employee_count, contact_person, revenue, founded_year,
    industry_tags, technologies, address — or None if not a company page.
    """
    clean = clean_page_text(page_text)
    if len(clean) < 50:
        return None

    data = await client.extract_json(
        f"""Extract business information from this company webpage.

URL: {url}
Page content:
{clean}

Return JSON with these fields (use null for fields you're not confident about):
{{
  "company_name": "Official registered company name (not a tagline or slogan)",
  "description": "What the company does in 1-2 sentences",
  "email": "Primary business email address (not personal gmail/yahoo)",
  "phone": "Primary phone number with country code",
  "city": "City where the company is headquartered",
  "state": "State or region",
  "address": "Full registered office address if mentioned",
  "specialization": "Primary service type (e.g. IT Staffing, HR Consulting, RPO)",
  "industry_tags": "Comma-separated industry tags (e.g. IT Staffing, Payroll, Contract Staffing)",
  "employee_count": "Approximate employee count or range like '50-200'",
  "employee_count_exact": null,
  "revenue_range": "Annual revenue if mentioned (e.g. ₹10-50 Cr, $5M-10M)",
  "founded_year": "Year the company was founded/established",
  "technologies": "Key technologies, platforms, or tools mentioned (e.g. SAP, Workday, Oracle HCM)",
  "funding_stage": "Funding stage if mentioned (Bootstrapped, Seed, Series A, etc.)",
  "contact_person": "Name and title of a key contact if mentioned (e.g. 'Rahul Sharma, CEO')",
  "secondary_emails": "Any additional email addresses found, pipe-separated",
  "secondary_phones": "Any additional phone numbers found, pipe-separated",
  "glassdoor_rating": "Company rating if mentioned on the page",
  "is_real_company": true
}}

If this is NOT a real company page (it's a directory, article, blog, or listing site), return:
{{"is_real_company": false}}""",
        system="You are a data extraction expert. Extract only factual information visible on the page. Never guess or hallucinate. For fields not found on the page, use null.",
        max_tokens=600,
    )

    if not data or not data.get("is_real_company", False):
        return None

    return data


# ── AI Lead Validation ───────────────────────────────────────────────

async def ai_validate_leads(client: LLMClient, leads: list[Lead]) -> list[tuple[Lead, bool, str]]:
    """Validate a batch of leads using LLM.

    Returns list of (lead, is_valid, reason) tuples.
    Processes up to 10 leads per LLM call for efficiency.
    """
    results = []

    for i in range(0, len(leads), 10):
        batch = leads[i:i+10]
        lead_descriptions = []
        for j, lead in enumerate(batch):
            lead_descriptions.append(
                f'{j+1}. "{lead.company}" — website: {lead.website or "none"}, '
                f'email: {lead.email or "none"}, city: {lead.city or "none"}'
            )

        leads_text = "\n".join(lead_descriptions)

        data = await client.extract_json(
            f"""Evaluate each lead below. Is it a REAL business that could be a potential client?

{leads_text}

For each lead, return whether it's valid. Reject if:
- Company name is a generic service description (e.g. "HR Services in Mumbai")
- It's a directory/aggregator site (e.g. Clutch, Glassdoor, Indeed)
- Company name is an article title or listicle heading
- It's clearly not a real business

Return JSON: {{"results": [
  {{"id": 1, "valid": true, "reason": ""}},
  {{"id": 2, "valid": false, "reason": "directory site, not a company"}}
]}}""",
            system="You are a lead quality analyst. Be strict — only real businesses pass.",
            max_tokens=500,
        )

        ai_results = data.get("results", [])

        for j, lead in enumerate(batch):
            # Find corresponding AI result
            ai_result = None
            for r in ai_results:
                if r.get("id") == j + 1:
                    ai_result = r
                    break

            if ai_result:
                results.append((lead, ai_result.get("valid", True), ai_result.get("reason", "")))
            else:
                # If AI didn't return a result for this lead, pass it through
                results.append((lead, True, ""))

    return results


# ── AI Lead Scoring ──────────────────────────────────────────────────

async def ai_score_leads(client: LLMClient, leads: list[Lead], icp: dict) -> list[Lead]:
    """Score leads using LLM with ICP awareness.

    Processes in batches of 5 for efficiency. Updates leads in-place.
    """
    if not leads:
        return leads

    icp_text = (
        f"We sell: {icp.get('value_proposition', 'B2B SaaS')}\n"
        f"Target industries: {', '.join(icp.get('target_industries', [])[:5])}\n"
        f"Target cities: {', '.join(icp.get('target_cities', [])[:5])}\n"
        f"Preferred size: {icp.get('min_company_size', 10)}+ employees"
    )

    for i in range(0, len(leads), 5):
        batch = leads[i:i+5]
        lead_descriptions = []
        for j, lead in enumerate(batch):
            lead_descriptions.append(
                f'{j+1}. {lead.company} — {lead.description or "no description"} | '
                f'City: {lead.city or "unknown"} | Specialization: {lead.specialization or "unknown"} | '
                f'Size: {lead.company_size or "unknown"} | '
                f'Has email: {lead.has_email} | Has phone: {lead.has_phone}'
            )

        leads_text = "\n".join(lead_descriptions)

        data = await client.extract_json(
            f"""Score each lead 0-100 based on how well they match our Ideal Customer Profile (ICP).

OUR ICP:
{icp_text}

LEADS:
{leads_text}

Scoring guide:
- 70-100 (Hot): Perfect ICP match — right industry, right city, right size, has contact info
- 50-69 (Warm): Good match — most criteria met, some gaps
- 30-49 (Cold): Partial match — related industry or city, missing key data
- 0-29 (Unqualified): Poor match — wrong industry, no useful data

Return JSON: {{"scores": [
  {{"id": 1, "score": 75, "tier": "hot", "reason": "IT staffing in Bangalore, 100+ employees, has email"}},
  {{"id": 2, "score": 35, "tier": "cold", "reason": "Related industry but no contact info"}}
]}}""",
            system="You are a B2B sales intelligence analyst. Score leads objectively based on ICP fit.",
            max_tokens=600,
        )

        ai_scores = data.get("scores", [])

        for j, lead in enumerate(batch):
            ai_score = None
            for s in ai_scores:
                if s.get("id") == j + 1:
                    ai_score = s
                    break

            if ai_score:
                lead.score = max(0, min(100, ai_score.get("score", lead.score)))
                lead.score_tier = ai_score.get("tier", "cold")
                # Store reasoning in description if empty
                reason = ai_score.get("reason", "")
                if reason and not lead.description:
                    lead.description = reason

    return leads
