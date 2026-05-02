"""
Job Runner — Background processor for collection queries.

Enhanced with:
- Real-time progress events via ProgressBus
- Multiple search strategies (Maps + DuckDuckGo + directory patterns)
- Broader query expansion for more leads per search
- Parallel website enrichment for discovered leads
"""

import asyncio
import uuid
from datetime import datetime
from typing import Optional
from urllib.parse import urlparse

from leadgen.db import LeadDB
from leadgen.http import StealthClient
from leadgen.models import Lead
from leadgen.proxy_pool import ProxyPool
from leadgen.rate_limiter import RateLimiter
from leadgen.pipeline import deduplicate_leads
from leadgen.scoring import score_leads
from leadgen.progress import progress


class JobRunner:
    """Processes collection jobs with stealth tier escalation."""

    def __init__(self, db: Optional[LeadDB] = None):
        self.db = db or LeadDB()
        self.proxy_pool = ProxyPool()
        self.rate_limiter = RateLimiter()
        self.client = StealthClient(
            proxy_pool=self.proxy_pool,
            rate_limiter=self.rate_limiter,
        )

    async def submit(self, query: str) -> str:
        """Submit a new collection job and process it."""
        job_id = str(uuid.uuid4())[:8]
        self.db.create_job(job_id, query)
        progress.emit("job_created", {"job_id": job_id, "query": query})
        await self._process_job({"id": job_id, "query": query, "tier": 1})
        return job_id

    async def process_pending(self):
        """Process all pending jobs in the queue."""
        while True:
            job = self.db.claim_job()
            if not job:
                break
            progress.emit("job_started", {"job_id": job["id"], "query": job["query"]})
            await self._process_job(job)

    async def _process_job(self, job: dict):
        """Execute a single collection job."""
        job_id = job["id"]
        query = job["query"]

        try:
            leads = []
            total_strategies = 3

            # Strategy 1: Google Maps search
            progress.emit("job_progress", {
                "job_id": job_id, "stage": "maps",
                "message": f"🗺️ Searching Google Maps for '{query}'...",
                "step": 1, "total": total_strategies,
            })
            if self._is_location_query(query):
                maps_leads = await self._search_maps(query)
                leads.extend(maps_leads)
                progress.emit("job_progress", {
                    "job_id": job_id, "stage": "maps",
                    "message": f"📍 Found {len(maps_leads)} leads from Maps",
                    "leads_found": len(maps_leads),
                    "step": 1, "total": total_strategies,
                })

            # Strategy 2: DuckDuckGo web search (expanded queries)
            progress.emit("job_progress", {
                "job_id": job_id, "stage": "web_search",
                "message": f"🔍 Searching the web for '{query}'...",
                "step": 2, "total": total_strategies,
            })
            search_leads = await self._search_and_scrape(job_id, query)
            leads.extend(search_leads)
            progress.emit("job_progress", {
                "job_id": job_id, "stage": "web_search",
                "message": f"🌐 Found {len(search_leads)} leads from web search",
                "leads_found": len(search_leads),
                "step": 2, "total": total_strategies,
            })

            # Strategy 3: Directory scraping (Clutch, GoodFirms patterns)
            progress.emit("job_progress", {
                "job_id": job_id, "stage": "directories",
                "message": f"📂 Searching business directories...",
                "step": 3, "total": total_strategies,
            })
            dir_leads = await self._search_directories(job_id, query)
            leads.extend(dir_leads)
            progress.emit("job_progress", {
                "job_id": job_id, "stage": "directories",
                "message": f"📂 Found {len(dir_leads)} leads from directories",
                "leads_found": len(dir_leads),
                "step": 3, "total": total_strategies,
            })

            # Deduplicate
            progress.emit("job_progress", {
                "job_id": job_id, "stage": "dedup",
                "message": f"🔄 Deduplicating {len(leads)} leads...",
            })
            unique = deduplicate_leads(leads)

            # Score
            scored = score_leads(unique)

            # Store
            count = 0
            for lead in scored:
                lead.source = f"job:{job_id}"
                self.db.upsert_lead(lead)
                count += 1

            self.db.complete_job(job_id, leads_found=count)
            progress.emit("job_completed", {
                "job_id": job_id, "query": query,
                "leads_found": count, "raw_total": len(leads),
                "message": f"✅ Done! {count} unique leads stored",
            })

        except Exception as e:
            self.db.fail_job(job_id, str(e))
            progress.emit("job_failed", {
                "job_id": job_id, "error": str(e),
                "message": f"❌ Failed: {e}",
            })

    def _is_location_query(self, query: str) -> bool:
        """Check if query mentions a city/location."""
        try:
            from config import ICP
            query_lower = query.lower()
            for city in ICP["target_cities"]:
                if city.lower() in query_lower:
                    return True
        except Exception:
            pass
        # Common India city names
        cities = ["bangalore", "mumbai", "delhi", "hyderabad", "pune", "chennai",
                  "kolkata", "noida", "gurgaon", "ahmedabad", "bengaluru", "gurugram"]
        return any(c in query.lower() for c in cities)

    async def _search_maps(self, query: str) -> list[Lead]:
        """Search Google Maps via the existing scraper."""
        try:
            from leadgen.scrapers.google_maps import scrape_google_maps
            city = self._extract_city(query)
            if city:
                clean_query = query.lower().replace(city.lower(), "").strip()
                return await scrape_google_maps(clean_query or query, city, max_results=20)
        except Exception as e:
            progress.emit("job_progress", {
                "stage": "maps", "message": f"⚠️ Maps: {e}",
            })
        return []

    def _extract_city(self, query: str) -> str:
        """Extract city name from a query string."""
        try:
            from config import ICP
            for c in ICP["target_cities"]:
                if c.lower() in query.lower():
                    return c
        except Exception:
            pass
        cities = {"bangalore": "Bangalore", "mumbai": "Mumbai", "delhi": "Delhi",
                  "hyderabad": "Hyderabad", "pune": "Pune", "chennai": "Chennai",
                  "kolkata": "Kolkata", "noida": "Noida", "gurgaon": "Gurgaon",
                  "ahmedabad": "Ahmedabad", "bengaluru": "Bangalore", "gurugram": "Gurgaon"}
        for key, val in cities.items():
            if key in query.lower():
                return val
        return ""

    async def _search_and_scrape(self, job_id: str, query: str) -> list[Lead]:
        """Search the web with expanded queries and scrape results."""
        leads = []

        # Expand the query into multiple search variations
        expanded = self._expand_query(query)

        try:
            from ddgs import DDGS

            for i, q in enumerate(expanded):
                progress.emit("job_progress", {
                    "job_id": job_id, "stage": "web_search",
                    "message": f"🔎 Searching: '{q}' ({i+1}/{len(expanded)})",
                })
                try:
                    with DDGS() as ddgs:
                        results = list(ddgs.text(q, max_results=15))

                    for result in results:
                        url = result.get("href", "")
                        title = result.get("title", "")
                        if not url:
                            continue

                        # Skip known non-company pages
                        domain = urlparse(url).netloc.lower()
                        skip_domains = ["wikipedia.org", "youtube.com", "facebook.com",
                                       "twitter.com", "instagram.com", "reddit.com",
                                       "quora.com", "medium.com"]
                        if any(s in domain for s in skip_domains):
                            continue

                        # Fetch the page with stealth
                        resp = await self.client.fetch(url, tier=2, timeout=10)
                        if not resp.ok:
                            continue

                        # Extract contact info
                        emails = resp.extract_emails()
                        phones = resp.extract_phones()

                        if emails or phones or title:
                            city = self._extract_city(query)
                            lead = Lead(
                                company=self._clean_company_name(title),
                                website=url,
                                email=emails[0] if emails else "",
                                phone=phones[0] if phones else "",
                                city=city,
                                description=result.get("body", "")[:300],
                                source="web_search",
                            )
                            leads.append(lead)
                            progress.emit("job_progress", {
                                "job_id": job_id, "stage": "web_search",
                                "message": f"  📍 {lead.company[:40]} — {lead.email or lead.phone or 'website only'}",
                            })

                    await asyncio.sleep(2)  # Rate limit between searches

                except Exception as e:
                    progress.emit("job_progress", {
                        "job_id": job_id, "stage": "web_search",
                        "message": f"  ⚠️ Search error: {e}",
                    })

        except ImportError:
            progress.emit("job_progress", {
                "stage": "web_search",
                "message": "⚠️ ddgs package not installed",
            })

        return leads

    async def _search_directories(self, job_id: str, query: str) -> list[Lead]:
        """Scrape business directory search results."""
        leads = []
        city = self._extract_city(query)

        # Directory search URLs
        directory_queries = [
            f"{query} site:clutch.co",
            f"{query} site:goodfirms.co",
            f"{query} companies list India",
            f"{query} top firms {city}" if city else f"{query} top firms India",
        ]

        try:
            from ddgs import DDGS

            for dq in directory_queries:
                try:
                    with DDGS() as ddgs:
                        results = list(ddgs.text(dq, max_results=10))

                    for result in results:
                        url = result.get("href", "")
                        title = result.get("title", "")
                        body = result.get("body", "")
                        if not url or not title:
                            continue

                        # For Clutch/GoodFirms listings, extract company from title
                        company = self._clean_company_name(title)
                        if len(company) < 3:
                            continue

                        lead = Lead(
                            company=company,
                            website=url,
                            city=city,
                            description=body[:300],
                            source="directory",
                        )
                        leads.append(lead)

                    await asyncio.sleep(2)

                except Exception as e:
                    continue

        except ImportError:
            pass

        return leads

    def _expand_query(self, query: str) -> list[str]:
        """Expand a single query into multiple search variations."""
        city = self._extract_city(query)
        base = query.lower()

        # Remove city from base for recombination
        if city:
            base = base.replace(city.lower(), "").strip()

        variations = [query]  # Original query first

        # Add variations
        suffixes = ["companies", "agencies", "firms", "consultancies"]
        for s in suffixes:
            if s not in base:
                q = f"{base} {s}"
                if city:
                    q += f" {city}"
                variations.append(q)
                break  # Only add one variation

        # Add "top" variant
        variations.append(f"top {base} {city or 'India'}")

        # Add "list" variant
        variations.append(f"{base} list {city or 'India'} 2024")

        return variations[:4]  # Cap at 4 search queries

    def _clean_company_name(self, title: str) -> str:
        """Clean a company name extracted from a search result title."""
        # Remove common suffixes
        for sep in [" - ", " | ", " — ", " – ", " · "]:
            if sep in title:
                title = title.split(sep)[0]

        # Remove common noise words
        noise = ["Reviews", "Company Profile", "LinkedIn", "Glassdoor",
                 "Clutch.co", "GoodFirms", "Careers", "Jobs"]
        for n in noise:
            title = title.replace(n, "")

        return title.strip()[:100]
