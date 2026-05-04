"""
Semantic Scholar source adapter - Scientific papers and citations
"""
import aiohttp
from typing import List
import urllib.parse
import logging

from . import DocumentSource, SearchResult, FileType

logger = logging.getLogger(__name__)


class SemanticScholarSource(DocumentSource):
    """Semantic Scholar search adapter (200M+ scientific papers)"""
    
    BASE_URL = "https://api.semanticscholar.org/graph/v1/paper/search"
    
    @property
    def name(self) -> str:
        return "semantic_scholar"
    
    @property
    def display_name(self) -> str:
        return "Semantic Scholar"
    
    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        """Search Semantic Scholar using official Graph API"""
        results = []
        
        try:
            params = {
                "query": query,
                "limit": limit,
                "fields": "title,url,authors,year,abstract,openAccessPdf,venue,citationCount"
            }
            
            url = f"{self.BASE_URL}?{urllib.parse.urlencode(params)}"
            
            async with aiohttp.ClientSession() as session:
                # Semantic Scholar limit is 100 req/5min without API key
                async with session.get(url, timeout=15) as response:
                    if response.status != 200:
                        logger.error(f"Semantic Scholar API returned {response.status}")
                        return results
                    
                    data = await response.json()
                    
                    for entry in data.get("data", []):
                        try:
                            title = entry.get("title", "Untitled")
                            paper_url = entry.get("url", "")
                            abstract = entry.get("abstract")
                            year = entry.get("year")
                            citation_count = entry.get("citationCount", 0)
                            
                            # Extract PDF link if open access
                            pdf_link = None
                            oa_pdf = entry.get("openAccessPdf")
                            if oa_pdf and isinstance(oa_pdf, dict):
                                pdf_link = oa_pdf.get("url")
                                
                            # Extract authors
                            authors = []
                            for author in entry.get("authors", []):
                                authors.append(author.get("name"))
                                
                            author_str = ', '.join(authors[:3])
                            if len(authors) > 3:
                                author_str += ' et al.'
                                
                            snippet = abstract[:200] + "..." if abstract else None
                            if citation_count > 0:
                                citation_info = f"[{citation_count} citations] "
                                snippet = citation_info + (snippet or "")
                            
                            result = SearchResult(
                                id=f"s2_{entry.get('paperId')}",
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
                            logger.error(f"Error parsing Semantic Scholar entry: {e}")
                            continue
                    
        except Exception as e:
            logger.error(f"Semantic Scholar search error: {e}")
        
        return results
