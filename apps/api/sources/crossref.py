"""
Crossref source adapter - Academic metadata and DOIs
"""
import aiohttp
from typing import List
import urllib.parse
import logging
import re

from . import DocumentSource, SearchResult, FileType

logger = logging.getLogger(__name__)


class CrossrefSource(DocumentSource):
    """Crossref API adapter (150M+ scholarly records)"""
    
    BASE_URL = "https://api.crossref.org/works"
    
    @property
    def name(self) -> str:
        return "crossref"
    
    @property
    def display_name(self) -> str:
        return "Crossref"
    
    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        """Search Crossref using REST API"""
        results = []
        
        try:
            params = {
                "query": query,
                "rows": limit,
                "select": "DOI,title,author,URL,published-print,published-online,abstract,link"
            }
            
            url = f"{self.BASE_URL}?{urllib.parse.urlencode(params)}"
            
            headers = {
                "User-Agent": "Yupcha-Engine/3.0 (mailto:admin@yupcha.com)" # Crossref politely requests user-agent
            }
            
            async with aiohttp.ClientSession() as session:
                async with session.get(url, headers=headers, timeout=15) as response:
                    if response.status != 200:
                        logger.error(f"Crossref API returned {response.status}")
                        return results
                    
                    data = await response.json()
                    items = data.get("message", {}).get("items", [])
                    
                    for item in items:
                        try:
                            titles = item.get("title", [])
                            if not titles:
                                continue
                            title = titles[0]
                            
                            doi = item.get("DOI", "")
                            paper_url = item.get("URL", f"https://doi.org/{doi}")
                            
                            # Extract PDF link if available in text-mining links
                            pdf_link = None
                            for link in item.get("link", []):
                                if link.get("content-type") == "application/pdf":
                                    pdf_link = link.get("URL")
                                    break
                                    
                            # Extract authors
                            authors = []
                            for author in item.get("author", []):
                                family = author.get("family", "")
                                given = author.get("given", "")
                                if family and given:
                                    authors.append(f"{given} {family}")
                                elif family:
                                    authors.append(family)
                                    
                            author_str = ', '.join(authors[:3])
                            if len(authors) > 3:
                                author_str += ' et al.'
                                
                            # Extract year
                            year = None
                            published = item.get("published-print") or item.get("published-online")
                            if published and "date-parts" in published:
                                date_parts = published["date-parts"]
                                if date_parts and date_parts[0]:
                                    year = date_parts[0][0]
                                    
                            # Abstract (sometimes present as jats XML)
                            abstract = item.get("abstract", "")
                            snippet = None
                            if abstract:
                                # Clean XML tags if present
                                clean_abstract = re.sub(r'<[^>]+>', '', abstract)
                                snippet = clean_abstract[:200] + "..."
                            
                            result = SearchResult(
                                id=f"crossref_{doi.replace('/', '_')}",
                                title=title,
                                url=paper_url,
                                source=self.name,
                                download_url=pdf_link,
                                author=author_str if authors else None,
                                year=year,
                                file_type=FileType.PDF if pdf_link else FileType.UNKNOWN,
                                snippet=snippet
                            )
                            results.append(result)
                            
                        except Exception as e:
                            logger.error(f"Error parsing Crossref entry: {e}")
                            continue
                    
        except Exception as e:
            logger.error(f"Crossref search error: {e}")
        
        return results
