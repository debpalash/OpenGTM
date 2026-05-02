import httpx
from bs4 import BeautifulSoup
import asyncio

async def test_lite():
    query = "samrat-bhardwaj"
    url = "https://lite.duckduckgo.com/lite/"
    data = {"q": query, "kl": "wt-wt"}
    headers = {
        "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/115.0.0.0 Safari/537.36",
        "Content-Type": "application/x-www-form-urlencoded",
        "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
    }

    async with httpx.AsyncClient(follow_redirects=True, timeout=15.0) as client:
        response = await client.post(url, data=data, headers=headers)
        soup = BeautifulSoup(response.text, "lxml")
        
        links = soup.find_all("a", class_="result-link")
        if links:
            tr = links[0].find_parent("tr")
            print("First result HTML:")
            print(tr.prettify())
            
            snippet_tr = tr.find_next_sibling("tr")
            print("Next sibling HTML (Snippet):")
            print(snippet_tr.prettify())

if __name__ == "__main__":
    asyncio.run(test_lite())
