"""
DOAJ (Directory of Open Access Journals) source adapter
"""
import aiohttp
from typing import List
import urllib.parse
import logging

from . import DocumentSource, SearchResult, FileType

logger = logging.getLogger(__name__)


class DOAJSource(DocumentSource):
    """DOAJ search adapter (verified open access articles)"""
    
    BASE_URL = "https://doaj.org/api/search/articles"
    
    @property
    def name(self) -> str:
        return "doaj"
    
    @property
    def display_name(self) -> str:
        return "DOAJ"
    
    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        """Search DOAJ using official API"""
        results = []
        
        try:
            # DOAJ API endpoint
            params = {
                "q": query,
                "pageSize": limit,
                "sort": "relevance"
            }
            
            url = f"{self.BASE_URL}/{urllib.parse.quote(query)}"
            
            async with aiohttp.ClientSession() as session:
                async with session.get(url, params={"pageSize": limit}, timeout=15) as response:
                    if response.status != 200:
                        logger.error(f"DOAJ API returned {response.status}")
                        return results
                    
                    data = await response.json()
                    
                    # Parse results
                    items = data.get('results', [])
                    
                    for idx, item in enumerate(items):
                        try:
                            bibjson = item.get('bibjson', {})
                            
                            # Extract title
                            title = bibjson.get('title', 'Untitled')
                            
                            # Get link - prefer PDF full text
                            links = bibjson.get('link', [])
                            url = ''
                            download_url = None
                            
                            for link in links:
                                link_url = link.get('url', '')
                                link_type = link.get('type', '').lower()
                                
                                if not url:
                                    url = link_url
                                
                                # Look for fulltext PDF
                                if 'fulltext' in link_type or 'pdf' in link_type:
                                    download_url = link_url
                                    break
                            
                            # Extract authors
                            authors_list = bibjson.get('author', [])
                            authors = [a.get('name', '') for a in authors_list[:3]]
                            author_str = ', '.join(authors)
                            if len(authors_list) > 3:
                                author_str += ' et al.'
                            
                            # Extract year
                            year = None
                            year_str = bibjson.get('year')
                            if year_str:
                                try:
                                    year = int(year_str)
                                except:
                                    pass
                            
                            # Abstract as snippet
                            snippet = bibjson.get('abstract', '')[:200]
                            
                            # Journal info
                            journal = bibjson.get('journal', {}).get('title', '')
                            
                            result = SearchResult(
                                id=f"doaj_{item.get('id', idx)}",
                                title=title,
                                url=url,
                                source=self.name,
                                author=author_str if authors else None,
                                year=year,
                                file_type=FileType.PDF,
                                snippet=f"{journal}: {snippet}" if journal else snippet
                            )
                            results.append(result)
                            
                        except Exception as e:
                            logger.error(f"Error parsing DOAJ result: {e}")
                            continue
                    
        except Exception as e:
            logger.error(f"DOAJ search error: {e}")
        
        return results
