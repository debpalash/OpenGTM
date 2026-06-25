"""
Legacy document sources — wrappers around older search functions.
These are now stubs since the original search_scraper module was removed
during the monorepo merge. They'll return empty results gracefully.

The ArchiveSource has been re-implemented against the public archive.org
Advanced Search API (https://archive.org/advancedsearch.php).
"""

import os
import logging
import urllib.parse
from typing import List, Optional

import aiohttp

from . import DocumentSource, SearchResult, FileType

logger = logging.getLogger(__name__)


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
    """Internet Archive search adapter (public Advanced Search API).

    Uses https://archive.org/advancedsearch.php which returns JSON of the form
    ``{"response": {"numFound": N, "docs": [...]}}``. Each doc is mapped to a
    standardized :class:`SearchResult`. The item landing page is
    ``https://archive.org/details/<identifier>`` and a best-effort direct
    download is exposed via ``https://archive.org/download/<identifier>``.
    """

    BASE_URL = "https://archive.org/advancedsearch.php"
    TIMEOUT = 15

    # archive.org mediatype / format -> our FileType
    _FORMAT_MAP = {
        "pdf": FileType.PDF,
        "epub": FileType.EPUB,
        "docx": FileType.DOCX,
        "doc": FileType.DOCX,
        "mobi": FileType.MOBI,
        "txt": FileType.TXT,
        "text": FileType.TXT,
        "html": FileType.HTML,
    }

    @property
    def name(self) -> str:
        return "archive"

    @property
    def display_name(self) -> str:
        return "Internet Archive"

    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        """Search archive.org via the Advanced Search API."""
        results: List[SearchResult] = []

        if not query or not query.strip():
            return results

        params = {
            "q": query,
            "fl[]": [
                "identifier",
                "title",
                "creator",
                "year",
                "date",
                "mediatype",
                "format",
                "downloads",
                "description",
            ],
            "rows": max(1, int(limit)),
            "page": 1,
            "output": "json",
        }

        try:
            async with aiohttp.ClientSession() as session:
                async with session.get(
                    self.BASE_URL, params=params, timeout=self.TIMEOUT
                ) as response:
                    if response.status != 200:
                        logger.error(
                            "Internet Archive API returned %s", response.status
                        )
                        return results

                    # archive.org sends JSON but with text/javascript content-type,
                    # so disable aiohttp's strict content-type check.
                    data = await response.json(content_type=None)
        except aiohttp.ClientError as e:
            logger.error("Internet Archive request failed: %s", e)
            return results
        except Exception as e:  # timeouts, JSON decode, etc.
            logger.error("Internet Archive search error: %s", e)
            return results

        docs = (data or {}).get("response", {}).get("docs", []) or []

        for doc in docs:
            try:
                result = self._map_doc(doc)
                if result is not None:
                    results.append(result)
            except Exception as e:
                logger.error("Error parsing Internet Archive result: %s", e)
                continue

        return results[:limit]

    def _map_doc(self, doc: dict) -> Optional[SearchResult]:
        """Map a single archive.org doc to a SearchResult."""
        identifier = doc.get("identifier")
        if not identifier:
            return None

        title = doc.get("title") or identifier
        # title can come back as a list when there are multiple values
        if isinstance(title, list):
            title = ", ".join(str(t) for t in title if t) or identifier

        # creator may be a string or a list of strings
        creator = doc.get("creator")
        author: Optional[str] = None
        if isinstance(creator, list):
            creator = [c for c in creator if c]
            if creator:
                author = ", ".join(str(c) for c in creator[:3])
                if len(creator) > 3:
                    author += " et al."
        elif isinstance(creator, str) and creator.strip():
            author = creator.strip()

        year = self._parse_year(doc.get("year"), doc.get("date"))

        snippet = doc.get("description")
        if isinstance(snippet, list):
            snippet = next((s for s in snippet if s), None)
        if isinstance(snippet, str):
            snippet = snippet.strip() or None
            if snippet and len(snippet) > 300:
                snippet = snippet[:297] + "..."

        ident_q = urllib.parse.quote(str(identifier), safe="")
        url = f"https://archive.org/details/{ident_q}"
        download_url = f"https://archive.org/download/{ident_q}"

        return SearchResult(
            id=f"archive_{identifier}",
            title=str(title),
            url=url,
            source=self.name,
            download_url=download_url,
            thumbnail=f"https://archive.org/services/img/{ident_q}",
            author=author,
            year=year,
            file_type=self._detect_file_type(doc),
            snippet=snippet,
        )

    @staticmethod
    def _parse_year(year_field, date_field) -> Optional[int]:
        """Best-effort extraction of a 4-digit publication year."""
        for value in (year_field, date_field):
            if value is None:
                continue
            if isinstance(value, list):
                value = next((v for v in value if v), None)
            if value is None:
                continue
            text = str(value)
            # find first 4-digit run that looks like a year
            digits = ""
            for ch in text:
                if ch.isdigit():
                    digits += ch
                    if len(digits) == 4:
                        break
                else:
                    digits = ""
            if len(digits) == 4:
                try:
                    yr = int(digits)
                    if 0 < yr <= 2100:
                        return yr
                except ValueError:
                    pass
        return None

    def _detect_file_type(self, doc: dict) -> FileType:
        """Infer a FileType from the doc's format/mediatype fields."""
        fmt = doc.get("format")
        formats: List[str] = []
        if isinstance(fmt, list):
            formats = [str(f).lower() for f in fmt if f]
        elif isinstance(fmt, str):
            formats = [fmt.lower()]

        for key, ftype in self._FORMAT_MAP.items():
            if any(key in f for f in formats):
                return ftype

        mediatype = str(doc.get("mediatype") or "").lower()
        if mediatype == "texts":
            return FileType.PDF
        return FileType.OTHER


class GoogleBooksSource(DocumentSource):
    """Google Books search adapter (public Volumes API).

    Uses https://www.googleapis.com/books/v1/volumes which returns JSON of the
    form ``{"items": [{"id": ..., "volumeInfo": {...}, "accessInfo": {...}}]}``.
    No API key is required for basic search; an optional GOOGLE_BOOKS_API_KEY
    (passed explicitly or read from the environment) is forwarded as the ``key``
    query parameter to raise the per-IP quota when present.
    """

    BASE_URL = "https://www.googleapis.com/books/v1/volumes"
    TIMEOUT = 15
    # Google Books caps maxResults at 40 per request.
    MAX_RESULTS = 40

    def __init__(self, api_key: Optional[str] = None):
        # Key is optional: basic search works unauthenticated. An explicit arg
        # wins over the environment so callers/tests can inject one.
        self.api_key = api_key or os.getenv("GOOGLE_BOOKS_API_KEY")

    @property
    def name(self) -> str:
        return "google_books"

    @property
    def display_name(self) -> str:
        return "Google Books"

    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        """Search Google Books and map volumes to SearchResult objects."""
        results: List[SearchResult] = []

        if not query or not query.strip():
            return results

        params = {
            "q": query,
            "maxResults": max(1, min(int(limit), self.MAX_RESULTS)),
            "printType": "books",
        }
        if self.api_key:
            params["key"] = self.api_key

        try:
            async with aiohttp.ClientSession() as session:
                async with session.get(
                    self.BASE_URL, params=params, timeout=self.TIMEOUT
                ) as response:
                    if response.status != 200:
                        logger.error(
                            "Google Books API returned %s", response.status
                        )
                        return results

                    data = await response.json()
        except aiohttp.ClientError as e:
            logger.error("Google Books request failed: %s", e)
            return results
        except Exception as e:  # timeouts, JSON decode, etc.
            logger.error("Google Books search error: %s", e)
            return results

        items = (data or {}).get("items", []) or []
        for item in items:
            try:
                result = self._map_volume(item)
                if result is not None:
                    results.append(result)
            except Exception as e:
                logger.error("Error parsing Google Books volume: %s", e)
                continue

        return results

    def _map_volume(self, item: dict) -> Optional[SearchResult]:
        """Map a single Google Books volume to a SearchResult, or None to skip."""
        volume_id = item.get("id")
        if not volume_id:
            return None

        info = item.get("volumeInfo", {}) or {}
        access = item.get("accessInfo", {}) or {}

        title = info.get("title") or "Untitled"
        subtitle = info.get("subtitle")
        if subtitle:
            title = f"{title}: {subtitle}"

        # Prefer the canonical info link; fall back to a constructed volume URL.
        page_url = info.get("infoLink") or item.get("selfLink") or (
            f"https://books.google.com/books?id={urllib.parse.quote(str(volume_id))}"
        )

        # Authors
        authors = info.get("authors", []) or []
        author_str = ", ".join(authors[:3])
        if len(authors) > 3:
            author_str += " et al."

        # Year — publishedDate may be "2007", "2007-05", or "2007-05-21".
        year: Optional[int] = None
        published = info.get("publishedDate")
        if published:
            head = str(published)[:4]
            if head.isdigit():
                year = int(head)

        # Thumbnail
        thumbnail = None
        image_links = info.get("imageLinks") or {}
        if image_links:
            thumbnail = image_links.get("thumbnail") or image_links.get(
                "smallThumbnail"
            )

        # Download / file type. Google Books exposes EPUB and/or PDF when the
        # volume is downloadable (typically public-domain titles).
        download_url: Optional[str] = None
        file_type = FileType.OTHER

        pdf = access.get("pdf") or {}
        epub = access.get("epub") or {}
        if pdf.get("isAvailable") and pdf.get("downloadLink"):
            download_url = pdf.get("downloadLink")
            file_type = FileType.PDF
        elif pdf.get("isAvailable"):
            file_type = FileType.PDF
        elif epub.get("isAvailable") and epub.get("downloadLink"):
            download_url = epub.get("downloadLink")
            file_type = FileType.EPUB
        elif epub.get("isAvailable"):
            file_type = FileType.EPUB

        # Snippet: prefer the search snippet, fall back to the description.
        snippet = None
        search_info = item.get("searchInfo") or {}
        raw_snippet = search_info.get("textSnippet") or info.get("description")
        if raw_snippet:
            snippet = str(raw_snippet)[:300]

        return SearchResult(
            id=f"google_books_{volume_id}",
            title=title,
            url=page_url,
            source=self.name,
            download_url=download_url,
            thumbnail=thumbnail,
            author=author_str if authors else None,
            year=year,
            file_type=file_type,
            snippet=snippet,
        )
