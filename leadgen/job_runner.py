"""
Job Runner — Background processor for collection queries.

Picks up pending jobs from the database, runs them through the
stealth pipeline, and stores results.

Usage:
    from leadgen.job_runner import JobRunner
    runner = JobRunner()
    await runner.process_pending()    # Process all pending jobs
    await runner.submit("HR staffing agency Bangalore")  # Submit + process
"""

import asyncio
import uuid
from datetime import datetime
from typing import Optional

from leadgen.db import LeadDB
from leadgen.http import StealthClient
from leadgen.models import Lead
from leadgen.proxy_pool import ProxyPool
from leadgen.rate_limiter import RateLimiter
from leadgen.pipeline import deduplicate_leads
from leadgen.scoring import score_leads


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
        print(f"  📋 Job {job_id}: '{query}' submitted")
        await self._process_job({"id": job_id, "query": query, "tier": 1})
        return job_id

    async def process_pending(self):
        """Process all pending jobs in the queue."""
        while True:
            job = self.db.claim_job()
            if not job:
                break
            print(f"\n  🔄 Processing job {job['id']}: '{job['query']}'")
            await self._process_job(job)

    async def _process_job(self, job: dict):
        """Execute a single collection job."""
        job_id = job["id"]
        query = job["query"]

        try:
            leads = []

            # Strategy 1: Google Maps search (for location-based queries)
            if self._is_location_query(query):
                maps_leads = await self._search_maps(query)
                leads.extend(maps_leads)

            # Strategy 2: Web search + scrape results
            search_leads = await self._search_and_scrape(query)
            leads.extend(search_leads)

            # Deduplicate
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
            print(f"  ✅ Job {job_id}: {count} leads found and stored")

        except Exception as e:
            self.db.fail_job(job_id, str(e))
            print(f"  ❌ Job {job_id} failed: {e}")

    def _is_location_query(self, query: str) -> bool:
        """Check if query mentions a city/location."""
        from config import ICP
        query_lower = query.lower()
        for city in ICP["target_cities"]:
            if city.lower() in query_lower:
                return True
        return False

    async def _search_maps(self, query: str) -> list[Lead]:
        """Search Google Maps via the existing scraper."""
        try:
            from leadgen.scrapers.google_maps import scrape_google_maps
            # Extract city from query
            from config import ICP
            city = ""
            for c in ICP["target_cities"]:
                if c.lower() in query.lower():
                    city = c
                    break
            if city:
                clean_query = query.lower().replace(city.lower(), "").strip()
                return await scrape_google_maps(clean_query or query, city, max_results=15)
        except Exception as e:
            print(f"    ⚠ Maps search: {e}")
        return []

    async def _search_and_scrape(self, query: str) -> list[Lead]:
        """Search the web and scrape result pages for leads."""
        leads = []
        try:
            from ddgs import DDGS

            with DDGS() as ddgs:
                results = list(ddgs.text(query, max_results=10))

            for result in results:
                url = result.get("href", "")
                title = result.get("title", "")
                if not url:
                    continue

                # Fetch the page with stealth
                resp = await self.client.fetch(url, tier=2)
                if not resp.ok:
                    continue

                # Extract contact info
                emails = resp.extract_emails()
                phones = resp.extract_phones()

                if emails or phones:
                    lead = Lead(
                        company=title[:100],
                        website=url,
                        email=emails[0] if emails else "",
                        phone=phones[0] if phones else "",
                        description=result.get("body", "")[:300],
                        source="web_search",
                    )
                    leads.append(lead)
                    print(f"    📍 {title[:50]}: {emails[:1]} {phones[:1]}")

        except Exception as e:
            print(f"    ⚠ Web search: {e}")

        return leads
