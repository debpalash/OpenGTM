"""
Person Intelligence Service
Enriches a LinkedIn profile URL by running web searches and scraping results
to compile a comprehensive dossier about a person.
"""

import asyncio
import re
import logging
import urllib.parse
from typing import List, Dict, Optional, Callable, Any
from bs4 import BeautifulSoup
from pydantic import BaseModel

logger = logging.getLogger(__name__)

# Common junk domains to skip when scraping results
SKIP_DOMAINS = {
    "google.com", "duckduckgo.com", "bing.com", "yahoo.com",
    "facebook.com", "instagram.com", "pinterest.com", "tiktok.com",
    "youtube.com", "wikipedia.org", "amazon.com", "ebay.com",
}

HEADERS = {
    "User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
    "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
    "Accept-Language": "en-US,en;q=0.9",
}

EMAIL_PATTERN = r"[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}"


class PersonProfile(BaseModel):
    linkedin_url: str = ""
    username: str = ""
    name: str = ""
    headline: str = ""
    location: str = ""
    summary: str = ""
    emails: List[str] = []
    social_links: Dict[str, str] = {}
    articles: List[Dict[str, str]] = []
    mentions: List[Dict[str, str]] = []
    companies: List[str] = []
    education: List[str] = []
    skills: List[str] = []
    raw_sources: List[Dict[str, Any]] = []
    status: str = "pending"


class PersonIntelService:
    """Orchestrates person enrichment from a LinkedIn URL."""

    def __init__(self):
        self.email_pattern = re.compile(EMAIL_PATTERN)

    def parse_linkedin_username(self, url: str) -> str:
        """Extract username from LinkedIn URL."""
        # Handle various formats:
        # https://www.linkedin.com/in/anilkdas/
        # https://linkedin.com/in/anilkdas
        # linkedin.com/in/anilkdas?param=1
        url = url.strip().rstrip("/")
        match = re.search(r"linkedin\.com/in/([a-zA-Z0-9_-]+)", url)
        if match:
            return match.group(1)
        raise ValueError(f"Could not parse LinkedIn username from: {url}")

    async def enrich(
        self,
        linkedin_url: str,
        deep: bool = False,
        ws_callback: Optional[Callable] = None,
    ) -> PersonProfile:
        """
        Main enrichment pipeline.
        
        Args:
            linkedin_url: LinkedIn profile URL
            deep: If True, scrape result pages for more data (slower)
            ws_callback: async function to send progress updates
        """
        profile = PersonProfile(linkedin_url=linkedin_url, status="running")

        async def notify(step: str, data: dict = None):
            if ws_callback:
                msg = {"step": step, **(data or {})}
                try:
                    await ws_callback(msg)
                except Exception:
                    pass

        try:
            # Step 1: Parse username
            username = self.parse_linkedin_username(linkedin_url)
            profile.username = username
            await notify("parsed", {"username": username})
            logger.info(f"Parsed LinkedIn username: {username}")

            # Step 2: Run parallel search queries
            await notify("searching", {"message": "Running web searches..."})

            search_queries = self._build_search_queries(username)
            search_results = await self._run_parallel_searches(search_queries, notify)

            all_results = []
            for category, results in search_results.items():
                for r in results:
                    r["category"] = category
                    all_results.append(r)

            await notify("search_done", {
                "results_count": len(all_results),
                "categories": list(search_results.keys()),
            })

            # Step 3: Extract LinkedIn profile data from search snippets
            await notify("extracting", {"message": "Extracting profile data..."})
            linkedin_results = search_results.get("linkedin", [])
            self._extract_linkedin_data(profile, linkedin_results)
            await notify("profile_extracted", {
                "name": profile.name,
                "headline": profile.headline,
            })

            # Step 4: Find social profiles
            social_results = {
                "twitter": search_results.get("twitter", []),
                "github": search_results.get("github", []),
                "blog": search_results.get("blog", []),
            }
            self._extract_social_links(profile, social_results)
            await notify("socials_found", {"social_links": profile.social_links})

            # Step 5: Collect articles and mentions
            self._extract_articles_and_mentions(
                profile,
                search_results.get("articles", []),
                search_results.get("general", []),
            )
            await notify("articles_found", {
                "articles_count": len(profile.articles),
                "mentions_count": len(profile.mentions),
            })

            # Step 6: Deep scrape if requested (scrape actual pages for emails/content)
            if deep:
                await notify("deep_scraping", {
                    "message": "Deep scraping result pages...",
                    "total": min(len(all_results), 15),
                })
                await self._deep_scrape_results(profile, all_results, notify)

            # Step 7: Extract emails from all collected data
            self._extract_all_emails(profile, all_results)
            await notify("emails_found", {"email_count": len(profile.emails)})

            # Store raw sources
            profile.raw_sources = all_results[:50]  # Cap at 50 to avoid huge payloads
            profile.status = "completed"
            await notify("completed", {"profile": profile.model_dump()})

        except Exception as e:
            logger.error(f"Enrichment failed for {linkedin_url}: {e}")
            profile.status = "failed"
            await notify("error", {"message": str(e)})

        return profile

    def _build_search_queries(self, username: str) -> Dict[str, str]:
        """Build categorized search queries for the username."""
        return {
            "linkedin": f'"{username}" site:linkedin.com/in',
            "general": f'"{username}" linkedin',
            "email": f'"{username}" email contact',
            "twitter": f'"{username}" site:twitter.com OR site:x.com',
            "github": f'"{username}" site:github.com',
            "blog": f'"{username}" site:medium.com OR site:substack.com OR blog',
            "articles": f'"{username}" article OR presentation OR conference OR speaker',
        }

    async def _run_parallel_searches(
        self, queries: Dict[str, str], notify: Callable
    ) -> Dict[str, List[Dict]]:
        """Run all search queries in parallel using DuckDuckGo Lite."""
        results = {}

        import httpx
        
        # We can share a single httpx client to reduce connection overhead
        async with httpx.AsyncClient(follow_redirects=True, timeout=15.0) as client:
            async def search_one(category: str, query: str):
                try:
                    r = await self._search_duckduckgo(client, query)
                    results[category] = r
                    await notify("search_progress", {
                        "category": category,
                        "count": len(r),
                    })
                except Exception as e:
                    logger.error(f"Search failed for {category}: {e}")
                    results[category] = []

            # Stagger batches of 3 to avoid extreme rate limiting
            tasks = []
            for category, query in queries.items():
                tasks.append(search_one(category, query))
                
            for i in range(0, len(tasks), 3):
                batch = tasks[i : i + 3]
                await asyncio.gather(*batch)
                if i + 3 < len(tasks):
                    await asyncio.sleep(1)

        return results

    async def _search_duckduckgo(self, client, query: str, max_results: int = 20) -> List[Dict]:
        """Scrape DuckDuckGo Lite search results."""
        results = []
        url = "https://lite.duckduckgo.com/lite/"
        
        # POST method bypasses many DuckDuckGo bot protection checks
        data = {"q": query, "kl": "wt-wt"}
        headers = {
            "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/115.0.0.0 Safari/537.36",
            "Content-Type": "application/x-www-form-urlencoded",
            "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
        }

        try:
            response = await client.post(url, data=data, headers=headers)
            
            if response.status_code != 200:
                logger.warning(f"DDG Lite returned {response.status_code} for: {query}")
                return results

            soup = BeautifulSoup(response.text, "lxml")
            
            # Find all result rows
            # In lite.duckduckgo, titles are in 'tr' with 'a.result-snippet' or 'a.result-url'
            # Snippets are in 'td.result-snippet'
            
            # The structure is usually 4 rows per result:
            # 1. Spacer
            # 2. Title & Link
            # 3. Snippet
            # 4. URL string
            
            links = soup.find_all("a", class_="result-url")
            
            for a_tag in links:
                try:
                    title = a_tag.get_text(strip=True)
                    actual_url = a_tag.get("href", "")
                    
                    if not actual_url.startswith("http"):
                        actual_url = f"https:{actual_url}" if actual_url.startswith("//") else actual_url
                        
                    # Find snippet in the next rows
                    snippet = ""
                    tr = a_tag.find_parent("tr")
                    if tr:
                        snippet_tr = tr.find_next_sibling("tr")
                        if snippet_tr:
                            snippet_td = snippet_tr.find("td", class_="result-snippet")
                            if snippet_td:
                                snippet = snippet_td.get_text(strip=True)

                    if not title or not actual_url:
                        continue
                        
                    try:
                        domain = urllib.parse.urlparse(actual_url).netloc.replace("www.", "")
                        if "duckduckgo.com" in domain or domain in SKIP_DOMAINS:
                            continue
                    except Exception:
                        pass

                    # Add parameter-less linkedin URLs to prevent duplicate counting
                    if "linkedin.com/in" in actual_url:
                        actual_url = actual_url.split("?")[0].rstrip("/")

                    # Prevent duplicates and junks
                    if not any(r["url"] == actual_url for r in results):
                        results.append({
                            "title": title[:200],
                            "url": actual_url,
                            "snippet": snippet[:500],
                            "source": "duckduckgo",
                        })
                    
                    if len(results) >= max_results:
                        break
                        
                except Exception as e:
                    logger.debug(f"Error parsing DDG Lite result: {e}")
                    continue

        except Exception as e:
            logger.error(f"DuckDuckGo search error for '{query}': {e}")

        return results

    def _extract_linkedin_data(self, profile: PersonProfile, results: List[Dict]):
        """Extract LinkedIn profile data from search result snippets."""
        for r in results:
            url = r.get("url", "")
            title = r.get("title", "")
            snippet = r.get("snippet", "")

            # LinkedIn titles often appear as "Name - Title - Company | LinkedIn"
            if "linkedin.com/in/" in url:
                parts = title.replace(" | LinkedIn", "").replace(" - LinkedIn", "")

                # Try splitting by " - " to get name, headline
                segments = [s.strip() for s in parts.split(" - ") if s.strip()]
                if segments:
                    if not profile.name:
                        profile.name = segments[0]
                    if len(segments) > 1 and not profile.headline:
                        profile.headline = " - ".join(segments[1:])

                # Extract info from snippet
                if snippet and not profile.summary:
                    profile.summary = snippet

                # Try to find location patterns in snippet
                loc_match = re.search(
                    r"(?:based in|located in|from)\s+([A-Z][a-zA-Z\s,]+)",
                    snippet,
                    re.IGNORECASE,
                )
                if loc_match and not profile.location:
                    profile.location = loc_match.group(1).strip()

                # Extract companies from snippet
                company_patterns = [
                    r"(?:at|@)\s+([A-Z][a-zA-Z\s&]+?)(?:\s*[-·|]|\s*$)",
                    r"(?:works?\s+(?:at|for))\s+([A-Z][a-zA-Z\s&]+)",
                ]
                for pattern in company_patterns:
                    matches = re.findall(pattern, snippet)
                    for m in matches:
                        company = m.strip()
                        if company and company not in profile.companies and len(company) > 2:
                            profile.companies.append(company)

    def _extract_social_links(
        self, profile: PersonProfile, social_results: Dict[str, List[Dict]]
    ):
        """Extract social media profile links."""
        platform_patterns = {
            "twitter": [r"(?:twitter|x)\.com/([a-zA-Z0-9_]+)"],
            "github": [r"github\.com/([a-zA-Z0-9_-]+)"],
            "blog": [
                r"medium\.com/@?([a-zA-Z0-9_-]+)",
                r"([a-zA-Z0-9_-]+)\.substack\.com",
            ],
        }

        for platform, results in social_results.items():
            for r in results:
                url = r.get("url", "")
                for pattern in platform_patterns.get(platform, []):
                    match = re.search(pattern, url)
                    if match:
                        if platform not in profile.social_links:
                            profile.social_links[platform] = url
                        break

    def _extract_articles_and_mentions(
        self,
        profile: PersonProfile,
        article_results: List[Dict],
        general_results: List[Dict],
    ):
        """
        Categorize search results into articles (authored by person) and
        mentions (about the person).
        """
        seen_urls = set()

        for r in article_results:
            url = r.get("url", "")
            if url in seen_urls:
                continue
            seen_urls.add(url)

            entry = {
                "title": r.get("title", ""),
                "url": url,
                "snippet": r.get("snippet", ""),
                "source": r.get("source", "web"),
            }

            # Heuristic: if the title/snippet suggests authorship, it's an article
            text = f"{entry['title']} {entry['snippet']}".lower()
            is_authored = any(
                kw in text
                for kw in [
                    "by " + profile.username.lower(),
                    "author",
                    "published",
                    "wrote",
                    "presentation by",
                    "talk by",
                ]
            )

            if is_authored:
                profile.articles.append(entry)
            else:
                profile.mentions.append(entry)

        for r in general_results:
            url = r.get("url", "")
            if url in seen_urls or "linkedin.com" in url:
                continue
            seen_urls.add(url)

            profile.mentions.append({
                "title": r.get("title", ""),
                "url": url,
                "snippet": r.get("snippet", ""),
                "source": r.get("source", "web"),
            })

    async def _deep_scrape_results(
        self,
        profile: PersonProfile,
        all_results: List[Dict],
        notify: Callable,
    ):
        """Scrape actual result pages for deeper data extraction."""
        from apps.api.services.scraper import UniversalScraper

        scraper = UniversalScraper()
        urls_to_scrape = []
        seen = set()

        for r in all_results:
            url = r.get("url", "")
            if url in seen or not url.startswith("http"):
                continue
            # Skip large sites that are slow to scrape
            if any(
                d in url
                for d in ["linkedin.com", "google.com", "duckduckgo.com"]
            ):
                continue
            seen.add(url)
            urls_to_scrape.append(r)
            if len(urls_to_scrape) >= 10:
                break

        total = len(urls_to_scrape)
        for i, r in enumerate(urls_to_scrape):
            url = r.get("url", "")
            try:
                await notify("scrape_progress", {
                    "url": url,
                    "progress": f"{i + 1}/{total}",
                })
                scraped = await scraper.scrape(url)
                if scraped:
                    r["scraped_text"] = scraped.get("preview_text", "")[:1000]
                    r["scraped_emails"] = scraped.get("extracted_emails", [])
                    r["scraped_word_count"] = scraped.get("word_count", 0)

                    # Merge emails
                    for email in scraped.get("extracted_emails", []):
                        if email not in profile.emails:
                            profile.emails.append(email)
            except Exception as e:
                logger.debug(f"Deep scrape failed for {url}: {e}")

            # Small delay between scrapes
            await asyncio.sleep(0.5)

    def _extract_all_emails(self, profile: PersonProfile, all_results: List[Dict]):
        """Extract emails from all search snippets & scraped content."""
        all_text = " ".join(
            f"{r.get('snippet', '')} {r.get('scraped_text', '')}"
            for r in all_results
        )

        found_emails = set(self.email_pattern.findall(all_text))

        # Filter out obvious false positives
        junk_patterns = [
            "example.com",
            "email.com",
            "test.com",
            "domain.com",
            "yourname",
            "sentry.io",
            "wixpress",
        ]

        for email in found_emails:
            email_lower = email.lower()
            if any(junk in email_lower for junk in junk_patterns):
                continue
            if email not in profile.emails:
                profile.emails.append(email)


# Singleton instance
person_intel_service = PersonIntelService()
