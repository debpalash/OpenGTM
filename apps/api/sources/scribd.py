from typing import List, Optional
from apps.api.sources import DocumentSource, SearchResult, FileType
from apps.api.scribd_client import scribd_api
import logging
import asyncio

logger = logging.getLogger(__name__)


class ScribdSource(DocumentSource):
    @property
    def name(self) -> str:
        return "scribd"

    @property
    def display_name(self) -> str:
        return "Scribd"

    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        # Scribd client is sync requests-based, run in threadpool
        loop = asyncio.get_running_loop()
        try:
            results = await loop.run_in_executor(None, scribd_api.search, query)

            return [
                SearchResult(
                    id=f"scribd_{r.get('id') or r.get('url')}",
                    title=r.get("title", "Untitled"),
                    url=r.get("url"),
                    source="scribd",
                    thumbnail=r.get("thumbnail"),
                    snippet=r.get("description"),
                    file_type=FileType.PDF,  # Default assumption
                    author=r.get("author"),
                )
                for r in results[:limit]
            ]
        except Exception as e:
            logger.error(f"Scribd Source Error: {e}")
            return []

    async def get_download_url(self, result_id: str) -> Optional[str]:
        # Implementation for direct download resolution if feasible
        # For now, Scribd usually requires the complex downloader
        return None
