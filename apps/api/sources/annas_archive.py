from typing import List, Optional
from apps.api.sources import DocumentSource, SearchResult, FileType
from apps.api.annas_client import annas_client
import logging
import asyncio

logger = logging.getLogger(__name__)


class AnnasArchiveSource(DocumentSource):
    @property
    def name(self) -> str:
        return "annas_archive"

    @property
    def display_name(self) -> str:
        return "Anna's Archive"

    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        # Call sync search in threadpool
        loop = asyncio.get_running_loop()
        try:
            results = await loop.run_in_executor(None, annas_client.search, query)
            return [
                SearchResult(
                    id=f"annas_{idx}",
                    title=r.get("title", "Untitled"),
                    url=r.get("link", ""),
                    source=self.name,
                    author=r.get("snippet") if r.get("snippet") != "PDF/EPUB" else None,
                    snippet=r.get("snippet"),
                    file_type=FileType.PDF,
                    thumbnail=r.get("thumbnail"),
                )
                for idx, r in enumerate(results[:limit])
            ]
        except Exception as e:
            logger.error(f"Anna's Archive Search Error: {e}")
            return []

    async def get_download_url(self, result_id: str) -> Optional[str]:
        # Note: result_id for anna's is usually the link itself based on search implementation
        # The id in SearchResult is 'annas_X', but the url is passed in resolving usually?
        # Actually api.py passes 'result_id' to this function.
        # But our SearchResult.id logic above is synthesized.
        # Ideally, result_id should be enough to resolve.
        # For Anna's, it's easier if we just used the URL.
        # But source interface uses ID.
        # For now, return None as api.py handles Anna's resolution specially in process_link
        # OR we can implement it here properly if we change how IDs are stored.
        return None
