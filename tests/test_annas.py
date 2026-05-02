
import asyncio
from apps.api.annas_client import annas_client

async def test_annas():
    print("Testing Anna's Archive search for 'python programming'...")
    results = annas_client.search("python programming")
    print(f"Results found: {len(results)}")
    for r in results:
        print(f"- {r['title']} ({r['link']})")

if __name__ == "__main__":
    asyncio.run(test_annas())
