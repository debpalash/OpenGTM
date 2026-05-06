"""
Social Finder — Discover LinkedIn and Twitter profiles for companies.

Uses DuckDuckGo search to find social media profiles for companies
that don't already have them populated.
"""

import re
import time
from typing import List

from ddgs import DDGS
from apps.api.services.leadgen.proxy_client import get_ddgs
from apps.api.services.leadgen.models import Lead


def find_social_profiles(
    leads: List[Lead],
    delay: float = 1.5,
) -> List[Lead]:
    """
    Find LinkedIn and Twitter profiles for leads missing them.

    Updates leads in-place.
    """
    needs_social = [l for l in leads if l.company and not l.has_linkedin]
    print(f"  🔗 Finding social profiles for {len(needs_social)} leads...")

    found = 0

    with get_ddgs() as ddgs:
        for lead in needs_social:
            # LinkedIn
            if not lead.has_linkedin:
                query = f'site:linkedin.com/company "{lead.company}"'
                try:
                    results = list(ddgs.text(query, max_results=2))
                    for r in results:
                        href = r.get("href", "")
                        if "linkedin.com/company" in href:
                            lead.linkedin_url = href
                            found += 1
                            print(f"    🔗 {lead.company}: {href}")
                            break
                except Exception:
                    pass

            # Twitter / X
            if not lead.twitter_url:
                query = f'site:twitter.com OR site:x.com "{lead.company}" staffing OR HR'
                try:
                    results = list(ddgs.text(query, max_results=2))
                    for r in results:
                        href = r.get("href", "")
                        if "twitter.com/" in href or "x.com/" in href:
                            lead.twitter_url = href
                            print(f"    🐦 {lead.company}: {href}")
                            break
                except Exception:
                    pass

            time.sleep(delay)

    print(f"  📊 Found {found} LinkedIn profiles")
    return leads
