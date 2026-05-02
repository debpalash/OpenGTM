import httpx
from bs4 import BeautifulSoup
import asyncio
import urllib.parse
from pydantic import BaseModel
from typing import List, Dict

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
        print(f"Status: {response.status_code}")
        
        soup = BeautifulSoup(response.text, "lxml")
        
        results = []
        links = soup.find_all("a", class_="result-url")
        print(f"Found {len(links)} a.result-url tags")
        
        if len(links) == 0:
            print("Trying to find any <a> tags...")
            all_a = soup.find_all("a")
            for a in all_a[:5]:
                print(f" - {a.get('class')} {a.get('href')[:30]} {a.get_text(strip=True)[:20]}")
        
        for a_tag in links:
            title = a_tag.get_text(strip=True)
            actual_url = a_tag.get("href", "")
            
            if not actual_url.startswith("http"):
                actual_url = f"https:{actual_url}" if actual_url.startswith("//") else actual_url
                
            snippet = ""
            tr = a_tag.find_parent("tr")
            if tr:
                snippet_tr = tr.find_next_sibling("tr")
                if snippet_tr:
                    snippet_td = snippet_tr.find("td", class_="result-snippet")
                    if snippet_td:
                        snippet = snippet_td.get_text(strip=True)

            if not title or not actual_url:
                continue
                
            print(f"Found: {title[:30]} -> {actual_url[:40]}")
            print(f"  Snippet: {snippet[:40]}...")

if __name__ == "__main__":
    asyncio.run(test_lite())
