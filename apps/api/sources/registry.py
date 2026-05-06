"""
Source registry - manages all document sources
"""

import asyncio
import logging
from typing import List, Dict, Optional, Any

from . import DocumentSource, SearchResult
from .pdfdrive import PDFDriveSource
from .libgen import LibgenSource
from .google_search import GoogleSearchSource
from .arxiv import ArxivSource
from .academia import AcademiaSource
from .gutenberg import GutenbergSource
from .researchgate import ResearchGateSource
from .doaj import DOAJSource
from .freeebooks import FreeEbooksSource
from .openlibrary import OpenLibrarySource
from .pubmed import PubMedSource
from .legacy import SlideShareSource, ArchiveSource, GoogleBooksSource
from .scribd import ScribdSource
from .annas_archive import AnnasArchiveSource
from .semantic_scholar import SemanticScholarSource
from .crossref import CrossrefSource

logger = logging.getLogger(__name__)


class SourceRegistry:
    """Registry for managing all document sources"""

    def __init__(self):
        self.sources: Dict[str, DocumentSource] = {}
        self._register_sources()

    def _register_sources(self):
        """Register all available sources"""
        # Always available sources (API-based, reliable)
        self.sources["arxiv"] = ArxivSource()
        self.sources["gutenberg"] = GutenbergSource()
        self.sources["doaj"] = DOAJSource()
        self.sources["openlibrary"] = OpenLibrarySource()
        self.sources["pubmed"] = PubMedSource()
        self.sources["semantic_scholar"] = SemanticScholarSource()
        self.sources["crossref"] = CrossrefSource()

        # New Plugins (Migrated)
        self.sources["scribd"] = ScribdSource()
        self.sources["annas_archive"] = AnnasArchiveSource()

        # Legacy Wrappers
        self.sources["slideshare"] = SlideShareSource()
        self.sources["archive"] = ArchiveSource()
        self.sources["google_books"] = GoogleBooksSource()

        # Experimental sources (may be slow/unreliable)
        self.sources["pdfdrive"] = PDFDriveSource()
        self.sources["libgen"] = LibgenSource()
        self.sources["academia"] = AcademiaSource()
        self.sources["researchgate"] = ResearchGateSource()
        self.sources["freeebooks"] = FreeEbooksSource()

        # Sources that require configuration
        google = GoogleSearchSource()
        if google.is_available():
            self.sources["google"] = google
            logger.info("Google Search configured and available")
        else:
            logger.info(
                "Google Search not configured (set GOOGLE_API_KEY and GOOGLE_CSE_ID)"
            )

        logger.info(
            f"Registered {len(self.sources)} document sources: {list(self.sources.keys())}"
        )

    async def search_all(
        self,
        query: str,
        sources: Optional[List[str]] = None,
        limit_per_source: int = 20,
    ) -> Dict[str, List[SearchResult]]:
        """
        Search across multiple sources concurrently

        Args:
            query: Search query
            sources: List of source names to search (None = all)
            limit_per_source: Max results per source

        Returns:
            Dict mapping source name to list of results
        """
        # Determine which sources to search
        sources_to_search = sources or list(self.sources.keys())

        # Filter to only available sources
        available_sources = {
            name: source
            for name, source in self.sources.items()
            if name in sources_to_search and source.is_available()
        }

        if not available_sources:
            logger.warning(f"No available sources for query: {query}")
            return {}

        logger.info(f"Searching {len(available_sources)} sources for: {query}")

        # Create concurrent tasks
        # We must wrap coroutines in create_task to schedule them immediately
        running_tasks = [
            asyncio.create_task(source.search(query, limit_per_source))
            for source in available_sources.values()
        ]

        source_names = list(available_sources.keys())

        # Execute all searches concurrently with a global timeout safety
        # Individual sources should have their own timeouts too
        try:
            # Wait for all to complete, with a 8 second global timeout
            # We use return_exceptions=True so we don't crash if one fails
            # But wait_for will raise TimeoutError if the whole bundle takes too long.
            # However, wait_for on gather needs care.
            # Better strategy: wrap gather in wait_for

            all_results = await asyncio.wait_for(
                asyncio.gather(*running_tasks, return_exceptions=True), timeout=8.0
            )

            results = {}
            for name, res in zip(source_names, all_results):
                if isinstance(res, Exception):
                    logger.error(f"Error searching {name}: {res}")
                    results[name] = []
                else:
                    results[name] = res
                    logger.info(f"{name}: {len(res)} results")

        except asyncio.TimeoutError:
            logger.warning(
                "Search timed out - returning partial results not possible with gather+wait_for easily without refactor, returning empty or handled"
            )
            # If we timeout, we might want to salvage what finished, but gather doesn't give us partials easily if cancelled.
            # Actually, asyncio.as_completed is better for streaming/partial, but for simple aggregation:
            logger.error("Unified search global timeout (8s)")
            return {}

        except Exception as e:
            logger.error(f"Critical error in search_all: {e}")
            return {}

        return results

    def get_source(self, name: str) -> Optional[DocumentSource]:
        """Get a specific source by name"""
        return self.sources.get(name)

    def list_sources(self) -> List[Dict[str, any]]:
        """List all registered sources with their status"""
        return [
            {
                "name": name,
                "display_name": source.display_name,
                "available": source.is_available(),
                "requires_config": source.requires_config,
            }
            for name, source in self.sources.items()
        ]

    async def search_unified(
        self,
        query: str,
        sources: Optional[List[str]] = None,
        limit_per_source: int = 10,
        sort_by: str = "relevance",
        file_type: Optional[str] = None,
    ) -> Dict[str, Any]:
        """
        Unified search with aggregation, deduplication, sorting and filtering
        """
        # 1. Fetch from all sources
        results_map = await self.search_all(query, sources, limit_per_source)

        # 2. Flatten and Filter
        all_results: List[SearchResult] = []
        for source_results in results_map.values():
            for result in source_results:
                # Filter by file type if requested
                if file_type and result.file_type.value != file_type:
                    continue
                all_results.append(result)

        # 3. Deduplication (by Title + Author fuzzy match)
        unique_results = []
        seen_titles = set()

        def normalize(text):
            return "".join(c.lower() for c in text if c.isalnum()) if text else ""

        for r in all_results:
            # Simple dedupe key: Normalized Title + First 10 chars of Author
            key = normalize(r.title)
            if r.author:
                key += normalize(r.author)[:10]

            if key not in seen_titles:
                seen_titles.add(key)
                unique_results.append(r)

        # 4. Sorting
        if sort_by == "date":
            # Sort by year desc, nulls last
            unique_results.sort(key=lambda x: x.year or 0, reverse=True)
        elif sort_by == "title":
            unique_results.sort(key=lambda x: x.title.lower())
        else:  # relevance (interleaved/default order preservation)
            # For now, we trust the source's ranking and existing interleaving via "first come" isn't great.
            # But search_all returns a dict, so order is lost.
            # We should probably prioritize certain sources or just keep them mixed.
            # A simple relevance could be: match query in title > match in snippet
            pass

        return {"query": query, "total": len(unique_results), "results": unique_results}
