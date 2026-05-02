"""
Academia.edu source adapter
"""
import aiohttp
from bs4 import BeautifulSoup
from typing import List
import urllib.parse
import logging

from . import DocumentSource, SearchResult, FileType

logger = logging.getLogger(__name__)


class AcademiaSource(DocumentSource):
    """Academia.edu search adapter (37M+ academic papers)"""
    
    BASE_URL = "https://www.academia.edu"
    
    @property
    def name(self) -> str:
        return "academia"
    
    @property
    def display_name(self) -> str:
        return "Academia.edu"
    
    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        """Search Academia.edu"""
        results = []
        
        try:
            search_url = f"{self.BASE_URL}/search?q={urllib.parse.quote(query)}"
            
            headers = {
                "User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
            }
            
            async with aiohttp.ClientSession() as session:
                async with session.get(search_url, headers=headers, timeout=15) as response:
                    if response.status != 200:
                        logger.error(f"Academia.edu returned {response.status}")
                        return results
                    
                    html = await response.text()
                    soup = BeautifulSoup(html, 'html.parser')
                    
                    # Find paper cards (structure may vary, this is a best-effort attempt)
                    papers = soup.find_all('div', class_='work-card', limit=limit)
                    
                    if not papers:
                        # Try alternative selectors
                        papers = soup.find_all('div', {'data-work-id': True}, limit=limit)
                    
                    for idx, paper in enumerate(papers):
                        try:
                            # Extract title
                            title_elem = paper.find('div', class_='work-card--title') or paper.find('a', class_='work-title')
                            if not title_elem:
                                continue
                            
                            title = title_elem.get_text(strip=True)
                            
                            # Get link
                            link_elem = title_elem.find('a') if title_elem.name != 'a' else title_elem
                            relative_url = link_elem.get('href', '') if link_elem else ''
                            url = f"{self.BASE_URL}{relative_url}" if relative_url.startswith('/') else relative_url
                            
                            # Extract author
                            author_elem = paper.find('div', class_='work-card--authors') or paper.find('span', class_='author-text')
                            author = author_elem.get_text(strip=True) if author_elem else None
                            
                            # Extract snippet/abstract
                            snippet_elem = paper.find('div', class_='work-card--abstract')
                            snippet = snippet_elem.get_text(strip=True)[:200] if snippet_elem else None
                            
                            result = SearchResult(
                                id=f"academia_{idx}_{urllib.parse.quote(title[:30])}",
                                title=title,
                                url=url,
                                source=self.name,
                                author=author,
                                file_type=FileType.PDF,
                                snippet=snippet
                            )
                            results.append(result)
                            
                        except Exception as e:
                            logger.error(f"Error parsing Academia.edu result: {e}")
                            continue
                    
        except Exception as e:
            logger.error(f"Academia.edu search error: {e}")
        
        return results
