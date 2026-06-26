"""Manual live smoke-script: DuckDuckGo Lite HTML endpoint.

Hits lite.duckduckgo.com over the network, so it is SKIPPED by default.
Run on demand:

    RUN_LIVE_SCRAPER_TESTS=1 PYTHONPATH=. uv run --group dev \
        python -m pytest tests/test_ddg_lite.py -s
"""
import asyncio
import os

import httpx
import pytest
from bs4 import BeautifulSoup


async def _run_lite():
    query = "samrat-bhardwaj"
    url = "https://lite.duckduckgo.com/lite/"
    data = {"q": query, "kl": "us-en"}
    headers = {
        "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/115.0.0.0 Safari/537.36",
        "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8",
        "Content-Type": "application/x-www-form-urlencoded"
    }

    async with httpx.AsyncClient(follow_redirects=True) as client:
        r = await client.post(url, data=data, headers=headers)
        print(f"Status: {r.status_code}")

        soup = BeautifulSoup(r.text, "lxml")
        results = soup.find_all("tr")
        print(f"Found {len(results)} rows.")

        count = 0
        for tr in results:
            td = tr.find("td", class_="result-snippet")
            if td:
                a_tag = tr.previous_sibling.find("a", class_="result-url")
                if not a_tag:
                    a_tag = tr.previous_sibling.find("a")

                print(f"Snippet: {td.get_text(strip=True)[:50]}")
                if a_tag:
                    print(f"URL: {a_tag.get('href')}")
                print("---")
                count += 1

        if count == 0:
            print("\nRAW HTML:")
            print(r.text[:2000])
        return r


@pytest.mark.skipif(
    os.environ.get("RUN_LIVE_SCRAPER_TESTS") != "1",
    reason="live network test; set RUN_LIVE_SCRAPER_TESTS=1 to enable",
)
def test_lite():
    r = asyncio.run(_run_lite())
    assert r.status_code


if __name__ == "__main__":
    asyncio.run(_run_lite())
