"""
OpenLibrary source adapter - Internet Archive's book database
"""
import aiohttp
from typing import List, Optional
import urllib.parse
import logging

from . import DocumentSource, SearchResult, FileType

logger = logging.getLogger(__name__)


class OpenLibrarySource(DocumentSource):
    """OpenLibrary search adapter (official API)"""
    
    BASE_URL = "https://openlibrary.org/search.json"
    
    @property
    def name(self) -> str:
        return "openlibrary"
    
    @property
    def display_name(self) -> str:
        return "OpenLibrary"
    
    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        """Search OpenLibrary using official API"""
        results = []
        
        try:
            params = {
                "q": query,
                "limit": limit,
                "fields": "key,title,author_name,first_publish_year,isbn,ebook_access,cover_i"
            }
            
            async with aiohttp.ClientSession() as session:
                async with session.get(self.BASE_URL, params=params, timeout=15) as response:
                    if response.status != 200:
                        logger.error(f"OpenLibrary API returned {response.status}")
                        return results
                    
                    data = await response.json()
                    docs = data.get('docs', [])
                    
                    for doc in docs:
                        try:
                            title = doc.get('title', 'Untitled')
                            key = doc.get('key', '')
                            
                            # Build URL
                            url = f"https://openlibrary.org{key}" if key else ''
                            
                            # Get authors
                            authors = doc.get('author_name', [])
                            author_str = ', '.join(authors[:3])
                            if len(authors) > 3:
                                author_str += ' et al.'
                            
                            # Get year
                            year = doc.get('first_publish_year')
                            
                            # Check if ebook is available
                            ebook_access = doc.get('ebook_access', 'no_ebook')
                            download_url = None
                            if ebook_access in ['public', 'borrowable']:
                                # Construct potential Internet Archive download URL
                                isbn = doc.get('isbn', [None])[0] if doc.get('isbn') else None
                                if isbn:
                                    download_url = f"https://archive.org/download/{isbn}/{isbn}.pdf"
                            
                            result = SearchResult(
                                id=f"openlibrary_{key.replace('/', '_')}",
                                title=title,
                                url=url,
                                source=self.name,
                                download_url=download_url,
                                author=author_str if authors else None,
                                year=year,
                                file_type=FileType.PDF,
                                snippet=f"eBook access: {ebook_access}"
                            )
                            results.append(result)
                            
                        except Exception as e:
                            logger.error(f"Error parsing OpenLibrary result: {e}")
                            continue
                    
        except Exception as e:
            logger.error(f"OpenLibrary search error: {e}")
        
        return results
