"""
Free-eBooks.net source adapter
"""
import aiohttp
from bs4 import BeautifulSoup
from typing import List, Optional
import urllib.parse
import logging

from . import DocumentSource, SearchResult, FileType

logger = logging.getLogger(__name__)


class FreeEbooksSource(DocumentSource):
    """Free-eBooks.net search adapter"""
    
    BASE_URL = "https://www.free-ebooks.net"
    
    @property
    def name(self) -> str:
        return "freeebooks"
    
    @property
    def display_name(self) -> str:
        return "Free-eBooks.net"
    
    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        """Search Free-eBooks.net"""
        results = []
        
        try:
            search_url = f"{self.BASE_URL}/search/{urllib.parse.quote(query)}"
            
            headers = {
                "User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36"
            }
            
            async with aiohttp.ClientSession() as session:
                async with session.get(search_url, headers=headers, timeout=15) as response:
                    if response.status != 200:
                        logger.warning(f"Free-eBooks.net returned {response.status}")
                        return results
                    
                    html = await response.text()
                    soup = BeautifulSoup(html, 'html.parser')
                    
                    # Find book listings 
                    books = soup.find_all('div', class_='book-item', limit=limit)
                    
                    if not books:
                        # Try alternative selector
                        books = soup.find_all('article', limit=limit)
                    
                    for idx, book in enumerate(books):
                        try:
                            # Extract title and link
                            title_elem = book.find('h3') or book.find('h2') or book.find('a', class_='title')
                            if not title_elem:
                                continue
                            
                            link = title_elem.find('a') if title_elem.name != 'a' else title_elem
                            if not link:
                                continue
                            
                            title = link.get_text(strip=True) or title_elem.get_text(strip=True)
                            href = link.get('href', '')
                            url = f"{self.BASE_URL}{href}" if href.startswith('/') else href
                            
                            # Extract author
                            author_elem = book.find('span', class_='author') or book.find('p', class_='author')
                            author = author_elem.get_text(strip=True).replace('by ', '') if author_elem else None
                            
                            # Extract description
                            desc_elem = book.find('p', class_='description') or book.find('div', class_='excerpt')
                            snippet = desc_elem.get_text(strip=True)[:200] if desc_elem else None
                            
                            # Store URL in ID for later download resolution
                            result_id = f"freeebooks_{urllib.parse.quote(url)}"
                            
                            result = SearchResult(
                                id=result_id,
                                title=title,
                                url=url,
                                source=self.name,
                                author=author,
                                file_type=FileType.PDF,
                                snippet=snippet
                            )
                            results.append(result)
                            
                        except Exception as e:
                            logger.error(f"Error parsing Free-eBooks result: {e}")
                            continue
                    
        except Exception as e:
            logger.error(f"Free-eBooks.net search error: {e}")
        
        return results
    
    async def get_download_url(self, result_id: str) -> Optional[str]:
        """
        Extract download URL from Free-eBooks.net detail page
        
        The download button/link is usually on the book detail page.
        """
        try:
            # Extract URL from result_id (format: freeebooks_{url})
            if not result_id.startswith('freeebooks_'):
                logger.error(f"Invalid Free-eBooks result_id: {result_id}")
                return None
            
            detail_url = urllib.parse.unquote(result_id.replace('freeebooks_', ''))
            
            headers = {
                "User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36"
            }
            
            async with aiohttp.ClientSession() as session:
                async with session.get(detail_url, headers=headers, timeout=10) as response:
                    if response.status != 200:
                        logger.warning(f"Free-eBooks detail page returned {response.status}")
                        return None
                    
                    html = await response.text()
                    soup = BeautifulSoup(html, 'html.parser')
                    
                    # Look for download links
                    # Common patterns: "Download", "Read", "Get PDF", etc.
                    download_links = soup.find_all('a', href=True)
                    
                    for link in download_links:
                        href = link.get('href', '')
                        link_text = link.get_text(strip=True).lower()
                        
                        # Look for download-related text
                        if any(keyword in link_text for keyword in ['download', 'pdf', 'get book', 'read now']):
                            if '.pdf' in href or 'download' in href:
                                if href.startswith('http'):
                                    logger.info(f"Found Free-eBooks download URL: {href}")
                                    return href
                                elif href.startswith('/'):
                                    download_url = f"{self.BASE_URL}{href}"
                                    logger.info(f"Found Free-eBooks download URL: {download_url}")
                                    return download_url
                    
                    # Alternative: look for direct PDF links
                    pdf_links = [link for link in download_links if '.pdf' in link.get('href', '')]
                    if pdf_links:
                        href = pdf_links[0].get('href')
                        if href.startswith('http'):
                            return href
                        elif href.startswith('/'):
                            return f"{self.BASE_URL}{href}"
                    
                    logger.warning(f"Could not find download URL for Free-eBooks page: {detail_url}")
                    return None
                    
        except Exception as e:
            logger.error(f"Error extracting Free-eBooks download URL: {e}")
            return None
