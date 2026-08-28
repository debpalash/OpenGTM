"""
Base classes and models for document sources
"""
from abc import ABC, abstractmethod
from typing import List, Optional
from pydantic import BaseModel, ConfigDict
from enum import Enum


class FileType(str, Enum):
    PDF = "pdf"
    DOCX = "docx"
    EPUB = "epub"
    MOBI = "mobi"
    TXT = "txt"
    HTML = "html"
    OTHER = "other"


class SearchResult(BaseModel):
    """Standardized search result across all sources"""
    model_config = ConfigDict(use_enum_values=True)

    id: str
    title: str
    url: str
    source: str
    download_url: Optional[str] = None
    thumbnail: Optional[str] = None
    author: Optional[str] = None
    year: Optional[int] = None
    file_type: FileType = FileType.PDF
    file_size: Optional[str] = None
    snippet: Optional[str] = None


class DocumentSource(ABC):
    """Abstract base class for all document sources"""
    
    @property
    @abstractmethod
    def name(self) -> str:
        """Unique identifier for this source"""
        pass
    
    @property
    @abstractmethod
    def display_name(self) -> str:
        """Human-readable name for this source"""
        pass
    
    @abstractmethod
    async def search(self, query: str, limit: int = 20) -> List[SearchResult]:
        """
        Search for documents matching the query
        
        Args:
            query: Search query string
            limit: Maximum number of results to return
            
        Returns:
            List of SearchResult objects
        """
        pass
    
    async def get_download_url(self, result_id: str) -> Optional[str]:
        """
        Get the direct download URL for a specific result
        
        Args:
            result_id: The unique ID from SearchResult
            
        Returns:
            Direct download URL or None if not available
        """
        return None
    
    def is_available(self) -> bool:
        """
        Check if this source is currently accessible
        
        Returns:
            True if source is available, False otherwise
        """
        return True
    
    @property
    def requires_config(self) -> bool:
        """Whether this source requires API keys or configuration"""
        return False
