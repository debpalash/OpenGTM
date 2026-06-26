"""Manual live smoke-script for the Jina reader proxy (r.jina.ai).

Hits the real network, so it is SKIPPED by default. Run on demand:

    RUN_LIVE_SCRAPER_TESTS=1 PYTHONPATH=. uv run --group dev \
        python -m pytest tests/test_jina.py -s
"""
import asyncio
import os

import httpx
import pytest


async def _run_jina():
    url = "https://r.jina.ai/https://www.linkedin.com/in/samrat-bhardwaj/"
    headers = {
        "User-Agent": "Mozilla/5.0",
        "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
    }

    async with httpx.AsyncClient(follow_redirects=True, timeout=30.0) as client:
        response = await client.get(url, headers=headers)
        print(f"Status: {response.status_code}")
        print(response.text[:2000])
        return response


@pytest.mark.skipif(
    os.environ.get("RUN_LIVE_SCRAPER_TESTS") != "1",
    reason="live network test; set RUN_LIVE_SCRAPER_TESTS=1 to enable",
)
def test_jina():
    response = asyncio.run(_run_jina())
    assert response.status_code


if __name__ == "__main__":
    asyncio.run(_run_jina())
