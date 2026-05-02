import httpx
from bs4 import BeautifulSoup
import asyncio

async def test_lite():
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

if __name__ == "__main__":
    asyncio.run(test_lite())
