import httpx
import asyncio

async def test_jina():
    url = "https://r.jina.ai/https://www.linkedin.com/in/samrat-bhardwaj/"
    headers = {
        "User-Agent": "Mozilla/5.0",
        "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
    }

    async with httpx.AsyncClient(follow_redirects=True, timeout=30.0) as client:
        response = await client.get(url, headers=headers)
        print(f"Status: {response.status_code}")
        print(response.text[:2000])

if __name__ == "__main__":
    asyncio.run(test_jina())
