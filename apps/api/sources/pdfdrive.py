"""
PDF Drive source adapter with download URL extraction
"""
import aiohttp
from bs4 import BeautifulSoup
from typing import List, Optional
import urllib.parse
import logging

from . import DocumentSource, SearchResult, FileType

logger = logging.getLogger(__name__)


class PDFDriveSource(DocumentSource):
    """PDF Drive search adapter (90M+ PDFs)"""
    
    BASE_URL = "https://www.pdfdrive.com"
    
    @property
    def name(self) -> str:
        return "pdfdrive"
    
    @property
    def display_name(self) -> str:
        return "PDF Drive"
    
    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        """Search PDF Drive"""
        results = []
        
        try:
            search_url = f"{self.BASE_URL}/search?q={urllib.parse.quote(query)}"
            
            async with aiohttp.ClientSession() as session:
                async with session.get(search_url, timeout=10) as response:
                    if response.status != 200:
                        logger.error(f"PDF Drive returned {response.status}")
                        return results
                    
                    html = await response.text()
                    soup = BeautifulSoup(html, 'html.parser')
                    
                    # Find result items
                    items = soup.find_all('div', class_='file-right', limit=limit)
                    
                    for idx, item in enumerate(items):
                        try:
                            # Extract title and link
                            title_elem = item.find('h2')
                            if not title_elem:
                                continue
                            
                            link_elem = title_elem.find('a')
                            if not link_elem:
                                continue
                            
                            title = link_elem.get_text(strip=True)
                            relative_url = link_elem.get('href', '')
                            url = f"{self.BASE_URL}{relative_url}" if relative_url.startswith('/') else relative_url
                            
                            # Extract metadata
                            author = None
                            year = None
                            file_size = None
                            
                            # Look for pages/year info
                            info_elem = item.find('span', class_='fi-pagecount')
                            if info_elem:
                                text = info_elem.get_text()
                                # Extract year if present (format: "XXX Pages · YYYY")
                                if '·' in text:
                                    parts = text.split('·')
                                    if len(parts) > 1:
                                        try:
                                            year = int(parts[1].strip())
                                        except:
                                            pass
                            
                            # Get file size
                            size_elem = item.find('span', class_='fi-size')
                            if size_elem:
                                file_size = size_elem.get_text(strip=True)
                            
                            # Store URL in ID for download resolution
                            result_id = f"pdfdrive_{urllib.parse.quote(url)}"
                            
                            result = SearchResult(
                                id=result_id,
                                title=title,
                                url=url,
                                source=self.name,
                                author=author,
                                year=year,
                                file_type=FileType.PDF,
                                file_size=file_size
                            )
                            results.append(result)
                            
                        except Exception as e:
                            logger.error(f"Error parsing PDF Drive result: {e}")
                            continue
                    
        except Exception as e:
            logger.error(f"PDF Drive search error: {e}")
        
        return results
    
    async def get_download_url(self, result_id: str) -> Optional[str]:
        """
        Extract download URL from PDF Drive detail page
        
        Note: PDF Drive uses JavaScript to generate download links, making it
        challenging to extract with simple HTTP requests. This would ideally
        require browser automation (Playwright/Selenium).
        
        For now, this returns None with a note that browser automation is needed.
        """
        try:
            logger.info(f"PDF Drive download requires browser automation (JavaScript-heavy site)")
            
            # Could implement with Playwright in the future:
            # - Launch headless browser
            # - Navigate to detail page
            # - Click "Download" button
            # - Wait for final download URL
            # - Extract and return URL
            
            return None
            
        except Exception as e:
            logger.error(f"Error extracting PDF Drive download URL: {e}")
            return None
