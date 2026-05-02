"""
ResearchGate source adapter
"""
import aiohttp
from bs4 import BeautifulSoup
from typing import List
import urllib.parse
import logging

from . import DocumentSource, SearchResult, FileType

logger = logging.getLogger(__name__)


class ResearchGateSource(DocumentSource):
    """ResearchGate search adapter (135M+ publications)"""
    
    BASE_URL = "https://www.researchgate.net"
    
    @property
    def name(self) -> str:
        return "researchgate"
    
    @property
    def display_name(self) -> str:
        return "ResearchGate"
    
    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        """Search ResearchGate"""
        results = []
        
        try:
            search_url = f"{self.BASE_URL}/search/publication?q={urllib.parse.quote(query)}"
            
            headers = {
                "User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
            }
            
            async with aiohttp.ClientSession() as session:
                async with session.get(search_url, headers=headers, timeout=15) as response:
                    if response.status != 200:
                        logger.warning(f"ResearchGate returned {response.status}")
                        return results
                    
                    html = await response.text()
                    soup = BeautifulSoup(html, 'html.parser')
                    
                    # Find publication items
                    publications = soup.find_all('div', class_='nova-legacy-o-stack__item', limit=limit)
                    
                    for idx, pub in enumerate(publications):
                        try:
                            # Extract title
                            title_elem = pub.find('a', class_='nova-legacy-e-link')
                            if not title_elem:
                                continue
                            
                            title = title_elem.get_text(strip=True)
                            href = title_elem.get('href', '')
                            url = f"{self.BASE_URL}{href}" if href.startswith('/') else href
                            
                            # Extract authors
                            author_elems = pub.find_all('a', {'data-testid': 'publication-author-link'})
                            authors = [a.get_text(strip=True) for a in author_elems[:3]]
                            author_str = ', '.join(authors)
                            if len(author_elems) > 3:
                                author_str += ' et al.'
                            
                            # Extract publication info
                            meta_elem = pub.find('div', class_='nova-legacy-v-publication-item__meta')
                            snippet = meta_elem.get_text(strip=True) if meta_elem else None
                            
                            # Try to extract year
                            year = None
                            if snippet:
                                import re
                                year_match = re.search(r'\b(19|20)\d{2}\b', snippet)
                                if year_match:
                                    year = int(year_match.group())
                            
                            result = SearchResult(
                                id=f"researchgate_{idx}_{urllib.parse.quote(title[:30])}",
                                title=title,
                                url=url,
                                source=self.name,
                                author=author_str if authors else None,
                                year=year,
                                file_type=FileType.PDF,
                                snippet=snippet[:200] if snippet else None
                            )
                            results.append(result)
                            
                        except Exception as e:
                            logger.error(f"Error parsing ResearchGate result: {e}")
                            continue
                    
        except Exception as e:
            logger.error(f"ResearchGate search error: {e}")
        
        return results
