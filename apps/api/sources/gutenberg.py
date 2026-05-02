"""
Project Gutenberg source adapter
"""
import aiohttp
from bs4 import BeautifulSoup
from typing import List
import urllib.parse
import logging

from . import DocumentSource, SearchResult, FileType

logger = logging.getLogger(__name__)


class GutenbergSource(DocumentSource):
    """Project Gutenberg search adapter (70K+ public domain books)"""
    
    BASE_URL = "https://www.gutenberg.org"
    
    @property
    def name(self) -> str:
        return "gutenberg"
    
    @property
    def display_name(self) -> str:
        return "Project Gutenberg"
    
    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        """Search Project Gutenberg"""
        results = []
        
        try:
            search_url = f"{self.BASE_URL}/ebooks/search/?query={urllib.parse.quote(query)}"
            
            async with aiohttp.ClientSession() as session:
                async with session.get(search_url, timeout=15) as response:
                    if response.status != 200:
                        logger.error(f"Gutenberg returned {response.status}")
                        return results
                    
                    html = await response.text()
                    soup = BeautifulSoup(html, 'html.parser')
                    
                    # Find book listings
                    books = soup.find_all('li', class_='booklink', limit=limit)
                    
                    for book in books:
                        try:
                            # Extract title and link
                            title_elem = book.find('span', class_='title')
                            if not title_elem:
                                continue
                            
                            title = title_elem.get_text(strip=True)
                            
                            # Get book ID and construct URL
                            link_elem = book.find('a', class_='link')
                            if not link_elem:
                                continue
                            
                            href = link_elem.get('href', '')
                            url = f"{self.BASE_URL}{href}" if href.startswith('/') else href
                            
                            # Extract book ID for direct download
                            book_id = None
                            if '/ebooks/' in href:
                                try:
                                    book_id = href.split('/ebooks/')[-1].strip('/')
                                except:
                                    pass
                            
                            # Extract author
                            author_elem = book.find('span', class_='subtitle')
                            author = author_elem.get_text(strip=True) if author_elem else None
                            
                            # Construct PDF download URL if we have book ID
                            download_url = None
                            if book_id:
                                # Gutenberg PDFs are usually at /files/{id}/{id}-pdf.pdf
                                download_url = f"https://www.gutenberg.org/files/{book_id}/{book_id}-pdf.pdf"
                            
                            result = SearchResult(
                                id=f"gutenberg_{book_id or len(results)}",
                                title=title,
                                url=url,
                                source=self.name,
                                download_url=download_url,
                                author=author,
                                file_type=FileType.PDF,
                                snippet="Public domain classic"
                            )
                            results.append(result)
                            
                        except Exception as e:
                            logger.error(f"Error parsing Gutenberg result: {e}")
                            continue
                    
        except Exception as e:
            logger.error(f"Gutenberg search error: {e}")
        
        return results
