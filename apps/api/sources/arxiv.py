"""
ArXiv.org source adapter - Scientific papers
"""
import aiohttp
import xml.etree.ElementTree as ET
from typing import List
import urllib.parse
import logging

from . import DocumentSource, SearchResult, FileType

logger = logging.getLogger(__name__)


class ArxivSource(DocumentSource):
    """ArXiv.org search adapter (2M+ scientific papers)"""
    
    BASE_URL = "http://export.arxiv.org/api/query"
    
    @property
    def name(self) -> str:
        return "arxiv"
    
    @property
    def display_name(self) -> str:
        return "arXiv"
    
    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        """Search ArXiv using official API"""
        results = []
        
        try:
            params = {
                "search_query": f"all:{query}",
                "start": 0,
                "max_results": limit,
                "sortBy": "relevance",
                "sortOrder": "descending"
            }
            
            url = f"{self.BASE_URL}?{urllib.parse.urlencode(params)}"
            
            async with aiohttp.ClientSession() as session:
                async with session.get(url, timeout=15) as response:
                    if response.status != 200:
                        logger.error(f"ArXiv API returned {response.status}")
                        return results
                    
                    xml_content = await response.text()
                    root = ET.fromstring(xml_content)
                    
                    # Namespace for Atom feed
                    ns = {'atom': 'http://www.w3.org/2005/Atom'}
                    
                    entries = root.findall('atom:entry', ns)
                    
                    for entry in entries:
                        try:
                            # Extract metadata
                            title_elem = entry.find('atom:title', ns)
                            title = title_elem.text.strip().replace('\n', ' ') if title_elem is not None else "Untitled"
                            
                            # Get arXiv ID and URL
                            id_elem = entry.find('atom:id', ns)
                            url = id_elem.text if id_elem is not None else ""
                            
                            # Extract PDF link
                            pdf_link = None
                            for link in entry.findall('atom:link', ns):
                                if link.get('title') == 'pdf':
                                    pdf_link = link.get('href')
                                    break
                            
                            # Authors
                            authors = []
                            for author in entry.findall('atom:author', ns):
                                name_elem = author.find('atom:name', ns)
                                if name_elem is not None:
                                    authors.append(name_elem.text)
                            author_str = ', '.join(authors[:3])  # First 3 authors
                            if len(authors) > 3:
                                author_str += ' et al.'
                            
                            # Summary/Abstract
                            summary_elem = entry.find('atom:summary', ns)
                            snippet = summary_elem.text.strip()[:200] if summary_elem is not None else None
                            
                            # Published date
                            published_elem = entry.find('atom:published', ns)
                            year = None
                            if published_elem is not None:
                                try:
                                    year = int(published_elem.text[:4])
                                except:
                                    pass
                            
                            result = SearchResult(
                                id=f"arxiv_{url.split('/')[-1]}",
                                title=title,
                                url=url,
                                source=self.name,
                                download_url=pdf_link,
                                author=author_str if authors else None,
                                year=year,
                                file_type=FileType.PDF,
                                snippet=snippet
                            )
                            results.append(result)
                            
                        except Exception as e:
                            logger.error(f"Error parsing ArXiv entry: {e}")
                            continue
                    
        except Exception as e:
            logger.error(f"ArXiv search error: {e}")
        
        return results
