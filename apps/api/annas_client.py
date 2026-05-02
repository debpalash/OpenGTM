
import requests
from bs4 import BeautifulSoup
import re

class AnnasClient:
    def __init__(self):
        self.base_url = "https://annas-archive.org"
        self.headers = {
            "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36"
        }

    def search(self, query):
        """
        Searches Anna's Archive and returns a list of results.
        """
        search_url = f"{self.base_url}/search?q={query}"
        try:
            response = requests.get(search_url, headers=self.headers, timeout=15)
            response.raise_for_status()
            soup = BeautifulSoup(response.text, 'html.parser')
            
            results = []
            links = soup.find_all('a', href=True)
            
            for link in links:
                href = link['href']
                if '/md5/' in href:
                    # Heuristic: verify if it looks like a result item
                    # Usually title link has text
                    title = link.get_text(strip=True)
                    if not title:
                         # Try to find an img if it's a cover link
                         img = link.find('img')
                         if img:
                             # It's a cover link, skipping for now, we want the title link usually
                             continue
                    
                    item = {
                        "title": title if title else "Unknown Title",
                        "link": f"{self.base_url}{href}",
                        "source": "Anna's Archive",
                        "snippet": "PDF/EPUB",
                        "thumbnail": None
                    }
                    
                    # Try to find author
                    parent = link.find_parent('div')
                    if parent:
                        author_link = parent.find('a', href=re.compile(r'/search\?q='))
                        if author_link:
                            item['snippet'] = author_link.get_text(strip=True)
                            
                    results.append(item)
            
            # Deduplicate by link
            unique_results = []
            seen_links = set()
            for r in results:
                if r['link'] not in seen_links and r['title'] != "Unknown Title":
                    seen_links.add(r['link'])
                    unique_results.append(r)
            
            return unique_results[:10] # Return top 10

        except Exception as e:
            print(f"Annas Search Error: {e}")
            return []

    def resolve_download_link(self, detail_url):
        """
        Scrapes the detail page efficiently to find the direct download link.
        Prioritizes direct mirrors (library.lol, libgen, etc.) over slow_download.
        """
        try:
            response = requests.get(detail_url, headers=self.headers, timeout=15)
            response.raise_for_status()
            soup = BeautifulSoup(response.text, 'html.parser')
            
            fast_downloads = []
            direct_mirrors = []
            slow_downloads = []
            
            # Find all links
            for a in soup.find_all('a', href=True):
                text = a.get_text(strip=True).lower()
                href = a['href']
                
                # Priority 1: Fast partner servers (recommended)
                if 'fast partner' in text or '/fast_download/' in href:
                    if href.startswith('/'):
                        href = f"https://annas-archive.org{href}"
                    fast_downloads.append(href)
                # Priority 2: Direct library/libgen mirrors
                elif any(domain in href for domain in ['library.lol', 'libgen.li', 'libgen.rs', 'libgen.st']):
                    direct_mirrors.append(href)
                # Priority 3: Slow download (requires CAPTCHA)
                elif 'slow partner' in text or 'slow download' in text:
                    if href.startswith('/'):
                        href = f"https://annas-archive.org{href}"
                    slow_downloads.append(href)
                    
            # Return first available in priority order
            if fast_downloads:
                return fast_downloads[0]
            elif direct_mirrors:
                return direct_mirrors[0]
            elif slow_downloads:
                return slow_downloads[0]
            
            return None

        except Exception as e:
            print(f"Annas Resolve Error: {e}")
            return None

annas_client = AnnasClient()
