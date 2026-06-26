"""Manual live smoke-script for Anna's Archive search.

Hits the real Anna's Archive site, so it is SKIPPED by default. Run on demand:

    RUN_LIVE_SCRAPER_TESTS=1 PYTHONPATH=. uv run --group dev \
        python -m pytest tests/test_annas.py -s
"""
import os

import pytest


@pytest.mark.skipif(
    os.environ.get("RUN_LIVE_SCRAPER_TESTS") != "1",
    reason="live network test; set RUN_LIVE_SCRAPER_TESTS=1 to enable",
)
def test_annas():
    from apps.api.annas_client import annas_client

    print("Testing Anna's Archive search for 'python programming'...")
    results = annas_client.search("python programming")
    print(f"Results found: {len(results)}")
    for r in results:
        print(f"- {r['title']} ({r['link']})")
    assert isinstance(results, list)


if __name__ == "__main__":
    test_annas()
