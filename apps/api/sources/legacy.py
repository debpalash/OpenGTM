"""
Legacy document sources — wrappers around older search functions.
These are now stubs since the original search_scraper module was removed
during the monorepo merge. They'll return empty results gracefully.
"""

from typing import List
from . import DocumentSource, SearchResult, FileType


class SlideShareSource(DocumentSource):
    @property
    def name(self) -> str:
        return "slideshare"

    @property
    def display_name(self) -> str:
        return "SlideShare"

    @property
    def requires_config(self) -> bool:
        return True

    def is_available(self) -> bool:
        return False

    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        return []


class ArchiveSource(DocumentSource):
    @property
    def name(self) -> str:
        return "archive"

    @property
    def display_name(self) -> str:
        return "Internet Archive"

    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        # TODO: re-implement with direct archive.org API
        return []


class GoogleBooksSource(DocumentSource):
    @property
    def name(self) -> str:
        return "google_books"

    @property
    def display_name(self) -> str:
        return "Google Books"

    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        # TODO: re-implement with Google Books API
        return []
