"""
Library Genesis (LibGen) source adapter
"""
import aiohttp
from bs4 import BeautifulSoup
from typing import List, Optional
import urllib.parse
import logging
import re

from . import DocumentSource, SearchResult, FileType

logger = logging.getLogger(__name__)


class LibgenSource(DocumentSource):
    """Library Genesis search adapter (3M+ books)"""
    
    # Multiple mirrors for redundancy
    MIRRORS = [
        "http://libgen.rs",
        "http://libgen.is",
        "http://libgen.st"
    ]
    
    def __init__(self):
        self.base_url = self.MIRRORS[0]  # Start with first mirror
    
    @property
    def name(self) -> str:
        return "libgen"
    
    @property
    def display_name(self) -> str:
        return "Library Genesis"
    
    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        """Search LibGen"""
        results = []
        
        # Try mirrors in order until one works
        for mirror in self.MIRRORS:
            try:
                search_url = f"{mirror}/search.php?req={urllib.parse.quote(query)}&res={limit}&view=simple"
                
                async with aiohttp.ClientSession() as session:
                    async with session.get(search_url, timeout=15) as response:
                        if response.status != 200:
                            logger.warning(f"LibGen mirror {mirror} returned {response.status}")
                            continue
                        
                        html = await response.text()
                        soup = BeautifulSoup(html, 'html.parser')
                        
                        # Find result table
                        tables = soup.find_all('table')
                        if len(tables) < 3:
                            continue
                        
                        result_table = tables[2]  # Results are in 3rd table
                        rows = result_table.find_all('tr')[1:]  # Skip header
                        
                        for row in rows[:limit]:
                            try:
                                cols = row.find_all('td')
                                if len(cols) < 10:
                                    continue
                                
                                # Extract data from columns
                                # Col 2: Author, Col 1: Title, Col 8: File type, Col 7: Size
                                author = cols[1].get_text(strip=True)
                                title = cols[2].get_text(strip=True)
                                
                                # Get link
                                title_link = cols[2].find('a')
                                url = ""
                                if title_link and title_link.get('href'):
                                    url = title_link['href']
                                    if url.startswith('/'):
                                        url = f"{mirror}{url}"
                                
                                # Publisher/Year
                                publisher = cols[3].get_text(strip=True)
                                year_text = cols[4].get_text(strip=True)
                                year = None
                                try:
                                    year = int(year_text) if year_text.isdigit() else None
                                except:
                                    pass
                                
                                # File info
                                file_type_text = cols[8].get_text(strip=True).lower()
                                file_type = FileType.PDF if file_type_text == 'pdf' else FileType.OTHER
                                file_size = cols[7].get_text(strip=True)
                                
                                # Get MD5 for ID
                                md5_col = cols[0].get_text(strip=True)
                                
                                result = SearchResult(
                                    id=f"libgen_{md5_col}" if md5_col else f"libgen_{len(results)}",
                                    title=title,
                                    url=url,
                                    source=self.name,
                                    author=author if author else None,
                                    year=year,
                                    file_type=file_type,
                                    file_size=file_size,
                                    snippet=publisher if publisher else None
                                )
                                results.append(result)
                                
                            except Exception as e:
                                logger.error(f"Error parsing LibGen row: {e}")
                                continue
                        
                        # If we got results, this mirror works, so save it
                        if results:
                            self.base_url = mirror
                            break
                        
            except Exception as e:
                logger.error(f"LibGen mirror {mirror} error: {e}")
                continue
        
        return results
    
    async def get_download_url(self, result_id: str) -> Optional[str]:
        """
        Extract download URL from LibGen detail page
        
        LibGen provides multiple mirror links on the detail page.
        We'll try to get the direct download link from the first available mirror.
        """
        try:
            # Extract MD5 from result_id (format: libgen_{md5})
            if not result_id.startswith('libgen_'):
                logger.error(f"Invalid LibGen result_id: {result_id}")
                return None
            
            md5 = result_id.replace('libgen_', '')
            
            # Try each mirror to get download link
            for mirror in self.MIRRORS:
                try:
                    # LibGen detail page URL
                    detail_url = f"{mirror}/book/index.php?md5={md5}"
                    
                    async with aiohttp.ClientSession() as session:
                        async with session.get(detail_url, timeout=10) as response:
                            if response.status != 200:
                                logger.warning(f"LibGen mirror {mirror} returned {response.status} for {md5}")
                                continue
                            
                            html = await response.text()
                            soup = BeautifulSoup(html, 'html.parser')
                            
                            # Look for download links
                            # LibGen typically has links with text like "GET", "Cloudflare", "IPFS"
                            download_links = soup.find_all('a', href=True)
                            
                            for link in download_links:
                                href = link.get('href', '')
                                link_text = link.get_text(strip=True).upper()
                                
                                # Priority 1: Direct GET links
                                if 'GET' in link_text and href:
                                    if href.startswith('http'):
                                        logger.info(f"Found LibGen download URL via GET: {href}")
                                        return href
                                    elif href.startswith('/'):
                                        download_url = f"{mirror}{href}"
                                        logger.info(f"Found LibGen download URL: {download_url}")
                                        return download_url
                                
                                # Priority 2: Cloudflare links (usually reliable)
                                if 'CLOUDFLARE' in link_text and href.startswith('http'):
                                    logger.info(f"Found LibGen Cloudflare URL: {href}")
                                    return href
                                
                                # Priority 3: Library.lol links (good mirror)
                                if 'library.lol' in href or 'libgen.lc' in href:
                                    logger.info(f"Found LibGen mirror URL: {href}")
                                    return href
                            
                            # If no direct link found, try looking for specific patterns in href
                            for link in download_links:
                                href = link.get('href', '')
                                if 'download' in href.lower() or 'get.php' in href:
                                    if href.startswith('http'):
                                        return href
                                    elif href.startswith('/'):
                                        return f"{mirror}{href}"
                    
                except Exception as e:
                    logger.error(f"Error fetching from {mirror} for {md5}: {e}")
                    continue
            
            logger.warning(f"Could not find download URL for LibGen MD5: {md5}")
            return None
            
        except Exception as e:
            logger.error(f"Error extracting LibGen download URL: {e}")
            return None
