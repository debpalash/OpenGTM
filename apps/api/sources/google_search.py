"""
Google Custom Search source adapter
"""
import aiohttp
from typing import List, Optional
import urllib.parse
import logging
import os

from . import DocumentSource, SearchResult, FileType

logger = logging.getLogger(__name__)


class GoogleSearchSource(DocumentSource):
    """Google Custom Search API adapter"""
    
    BASE_URL = "https://www.googleapis.com/customsearch/v1"
    
    def __init__(self, api_key: Optional[str] = None, search_engine_id: Optional[str] = None):
        self.api_key = api_key or os.getenv('GOOGLE_API_KEY')
        self.cse_id = search_engine_id or os.getenv('GOOGLE_CSE_ID')
    
    @property
    def name(self) -> str:
        return "google"
    
    @property
    def display_name(self) -> str:
        return "Google Search"
    
    @property
    def requires_config(self) -> bool:
        return True
    
    def is_available(self) -> bool:
        return bool(self.api_key and self.cse_id)
    
    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        """Search using Google Custom Search API"""
        if not self.is_available():
            logger.warning("Google Search not configured (missing API key or CSE ID)")
            return []
        
        results = []
        
        try:
            # Google CSE allows max 10 results per request
            params = {
                "key": self.api_key,
                "cx": self.cse_id,
                "q": f"{query} filetype:pdf",
                "num": min(limit, 10)
            }
            
            url = f"{self.BASE_URL}?{urllib.parse.urlencode(params)}"
            
            async with aiohttp.ClientSession() as session:
                async with session.get(url, timeout=10) as response:
                    if response.status != 200:
                        logger.error(f"Google Search API returned {response.status}")
                        return results
                    
                    data = await response.json()
                    
                    items = data.get('items', [])
                    
                    for idx, item in enumerate(items):
                        try:
                            title = item.get('title', 'Untitled')
                            url = item.get('link', '')
                            snippet = item.get('snippet', '')
                            
                            # Try to extract file size from metadata
                            file_info = item.get('fileFormat', '')
                            
                            result = SearchResult(
                                id=f"google_{idx}_{urllib.parse.quote(title[:30])}",
                                title=title,
                                url=url,
                                source=self.name,
                                download_url=url,  # Direct PDF link
                                file_type=FileType.PDF,
                                snippet=snippet
                            )
                            results.append(result)
                            
                        except Exception as e:
                            logger.error(f"Error parsing Google result: {e}")
                            continue
                    
        except Exception as e:
            logger.error(f"Google Search error: {e}")
        
        return results
