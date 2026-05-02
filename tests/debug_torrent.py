import sys
from unittest.mock import MagicMock

# Mock logfire before importing torrent_v3
sys.modules["logfire"] = MagicMock()

import asyncio
from apps.api.services.torrent_v3 import smart_torrent_engine


async def test():
    print("Testing search with stealth arguments...")
    # forcing mirrors again just to be sure we hit 1337x
    smart_torrent_engine.validator.healthy_mirrors = {
        "1337x": ["https://1337x.st"],
        "tpb": ["https://tpb.party"],
    }

    try:
        results = await smart_torrent_engine.search("ubuntu")
        print(f"Found {len(results)} results")
        for r in results[:5]:
            print(f"- {r['name']} ({r['source']}) {r['seeds']}/{r['leechers']}")
    except Exception as e:
        print(f"Error: {e}")
        import traceback

        traceback.print_exc()


if __name__ == "__main__":
    asyncio.run(test())
