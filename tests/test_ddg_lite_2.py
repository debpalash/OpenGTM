"""Manual live smoke-script: DuckDuckGo Lite endpoint (result-url parsing).

Hits lite.duckduckgo.com over the network, so it is SKIPPED by default.
Run on demand:

    RUN_LIVE_SCRAPER_TESTS=1 PYTHONPATH=. uv run --group dev \
        python -m pytest tests/test_ddg_lite_2.py -s
"""
import asyncio
import os

import httpx
import pytest
from bs4 import BeautifulSoup


async def _run_lite():
    query = "samrat-bhardwaj"
    url = "https://lite.duckduckgo.com/lite/"
    data = {"q": query, "kl": "wt-wt"}
    headers = {
        "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/115.0.0.0 Safari/537.36",
        "Content-Type": "application/x-www-form-urlencoded",
        "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
    }

    async with httpx.AsyncClient(follow_redirects=True, timeout=15.0) as client:
        response = await client.post(url, data=data, headers=headers)
        print(f"Status: {response.status_code}")

        soup = BeautifulSoup(response.text, "lxml")

        links = soup.find_all("a", class_="result-url")
        print(f"Found {len(links)} a.result-url tags")

        if len(links) == 0:
            print("Trying to find any <a> tags...")
            all_a = soup.find_all("a")
            for a in all_a[:5]:
                print(f" - {a.get('class')} {a.get('href')[:30]} {a.get_text(strip=True)[:20]}")

        for a_tag in links:
            title = a_tag.get_text(strip=True)
            actual_url = a_tag.get("href", "")

            if not actual_url.startswith("http"):
                actual_url = f"https:{actual_url}" if actual_url.startswith("//") else actual_url

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

            print(f"Found: {title[:30]} -> {actual_url[:40]}")
            print(f"  Snippet: {snippet[:40]}...")
        return response


@pytest.mark.skipif(
    os.environ.get("RUN_LIVE_SCRAPER_TESTS") != "1",
    reason="live network test; set RUN_LIVE_SCRAPER_TESTS=1 to enable",
)
def test_lite():
    response = asyncio.run(_run_lite())
    assert response.status_code


if __name__ == "__main__":
    asyncio.run(_run_lite())
