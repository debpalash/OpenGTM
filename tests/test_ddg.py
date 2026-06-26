"""Manual live smoke-script: DuckDuckGo HTML endpoint via headless Playwright.

Launches a real browser and hits duckduckgo.com, so it is SKIPPED by default.
Run on demand (requires `uv run playwright install chromium`):

    RUN_LIVE_SCRAPER_TESTS=1 PYTHONPATH=. uv run --group dev \
        python -m pytest tests/test_ddg.py -s
"""
import asyncio
import os
import urllib.parse

import pytest
from bs4 import BeautifulSoup


async def _run_ddg():
    from playwright.async_api import async_playwright

    query = "samrat-bhardwaj"
    encoded_query = urllib.parse.quote_plus(query)
    url = f"https://html.duckduckgo.com/html/?q={encoded_query}"

    print(f"Testing URL: {url}")

    async with async_playwright() as p:
        browser = await p.chromium.launch(headless=True)
        context = await browser.new_context(
            user_agent="Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
        )
        page = await context.new_page()

        await page.route("**/*", lambda route: route.abort() if route.request.resource_type in ["image", "media", "font"] else route.continue_())

        print("Navigating...")
        await page.goto(url, wait_until="domcontentloaded", timeout=25000)

        print("Waiting for results...")
        try:
            await page.wait_for_selector('article, .result, [data-testid="result"]', timeout=8000)
        except Exception as e:
            print(f"Timeout: {e}")

        content = await page.content()
        await browser.close()

    soup = BeautifulSoup(content, "lxml")

    articles = soup.find_all("article")
    print(f"\n<article> tags: {len(articles)}")
    print(f".result elements: {len(soup.select('.result'))}")
    testid_count = len(soup.select('[data-testid="result"]'))
    print(f"[data-testid='result']: {testid_count}")

    # Try all link extraction approaches
    all_links = soup.find_all("a", href=True)
    external = [a for a in all_links if a["href"].startswith("http") and "duckduckgo" not in a["href"]]
    print(f"\nAll <a> tags: {len(all_links)}, External: {len(external)}")

    # Show what we found
    base = articles if articles else soup.select(".result")

    if not base:
        print("\nNo article/.result elements. Trying all <a> with h2...")
        for h2 in soup.find_all("h2"):
            a = h2.find("a", href=True)
            if a and a["href"].startswith("http"):
                print(f"  H2 link: {a.get_text(strip=True)[:40]} -> {a['href'][:60]}")

    print(f"\n--- Parsed Results ({len(base)}) ---")
    for i, el in enumerate(base[:10]):
        h2 = el.find("h2")
        a = h2.find("a", href=True) if h2 else el.find("a", href=True)
        if a:
            print(f"[{i}] {a.get_text(strip=True)[:50]}")
            print(f"    URL: {a['href'][:80]}")
            snippet = el.select_one('[data-testid="result-snippet"], .result__snippet')
            if snippet:
                print(f"    Snippet: {snippet.get_text(strip=True)[:60]}...")
        print()

    # Dump a snippet of raw HTML for debugging if nothing found
    if not base:
        print("\n--- RAW HTML (first 3000 chars) ---")
        print(content[:3000])

    return content


@pytest.mark.skipif(
    os.environ.get("RUN_LIVE_SCRAPER_TESTS") != "1",
    reason="live network test; set RUN_LIVE_SCRAPER_TESTS=1 to enable",
)
def test_ddg():
    content = asyncio.run(_run_ddg())
    assert content


if __name__ == "__main__":
    asyncio.run(_run_ddg())
