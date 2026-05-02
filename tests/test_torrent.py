import asyncio
from apps.api.services.torrent import torrent_service
import logfire


async def test_search():
    print("Searching for 'linux'...")
    results = await torrent_service.search("linux")
    print(f"Found {len(results)} results")
    for r in results[:5]:
        print(f"- {r['name']} ({r['size']}) - Seeds: {r['seeds']}")
        if "url" in r:
            print(f"  URL: {r['url']}")
            # magnet = await torrent_service.get_magnet(r['url'])
            # print(f"  Magnet: {magnet[:50]}...")


if __name__ == "__main__":
    asyncio.run(test_search())
