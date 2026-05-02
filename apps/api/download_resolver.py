"""
Download resolver endpoint - resolves download URLs on-demand
"""
from fastapi import APIRouter, HTTPException
from typing import Optional
import logging

from apps.api.sources.registry import SourceRegistry

logger = logging.getLogger(__name__)

router = APIRouter()


@router.get("/api/resolve_download")
async def resolve_download(source: str, result_id: str) -> dict:
    """
    Resolve download URL for a specific result
    
    Args:
        source: Source name (e.g., 'libgen', 'pdfdrive')
        result_id: Result ID from search
        
    Returns:
        {
            "download_url": str or None,
            "success": bool,
            "message": str
        }
    """
    try:
        registry = SourceRegistry()
        source_obj = registry.get_source(source)
        
        if not source_obj:
            raise HTTPException(status_code=404, detail=f"Source '{source}' not found")
        
        # Get download URL
        download_url = await source_obj.get_download_url(result_id)
        
        if download_url:
            return {
                "download_url": download_url,
                "success": True,
                "message": "Download URL resolved successfully"
            }
        else:
            return {
                "download_url": None,
                "success": False,
                "message": "Could not resolve download URL"
            }
            
    except Exception as e:
        logger.error(f"Error resolving download URL: {e}")
        raise HTTPException(status_code=500, detail=str(e))
