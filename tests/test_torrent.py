"""Manual live smoke-script for the torrent search service.

Hits real torrent indexers over the network, so it is SKIPPED by default.
Run on demand:

    RUN_LIVE_SCRAPER_TESTS=1 PYTHONPATH=. uv run --group dev \
        python -m pytest tests/test_torrent.py -s
"""
import asyncio
import os

import pytest


async def _run_search():
    from apps.api.services.torrent import torrent_service

    print("Searching for 'linux'...")
    results = await torrent_service.search("linux")
    print(f"Found {len(results)} results")
    for r in results[:5]:
        print(f"- {r['name']} ({r['size']}) - Seeds: {r['seeds']}")
        if "url" in r:
            print(f"  URL: {r['url']}")
    return results


@pytest.mark.skipif(
    os.environ.get("RUN_LIVE_SCRAPER_TESTS") != "1",
    reason="live network test; set RUN_LIVE_SCRAPER_TESTS=1 to enable",
)
def test_search():
    results = asyncio.run(_run_search())
    assert isinstance(results, list)


if __name__ == "__main__":
    asyncio.run(_run_search())
