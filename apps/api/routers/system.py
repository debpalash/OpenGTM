"""
System Router — Stats and file management.

Auth guards are optional — skipped if the auth system is not configured.
"""

from fastapi import APIRouter, HTTPException, Path
from fastapi.responses import FileResponse
from apps.api.core.config import settings
import shutil
import os

router = APIRouter(tags=["System"])


@router.get("/api/system/stats")
def get_system_stats():
    total, used, free = shutil.disk_usage("/")

    # Calculate Project Size
    project_size = 0
    # Use settings.ROOT_DIR
    for dirpath, dirnames, filenames in os.walk(settings.ROOT_DIR):
        for f in filenames:
            fp = os.path.join(dirpath, f)
            if not os.path.islink(fp):
                project_size += os.path.getsize(fp)

    db_path = settings.DATABASE_URL.replace("sqlite:///", "")
    db_size = os.path.getsize(db_path) if os.path.exists(db_path) else 0

    return {
        "disk_total": total,
        "disk_used": used,
        "disk_free": free,
        "project_size": project_size,
        "db_size": db_size,
    }


@router.get("/api/files/list")
async def get_files(path: str = ""):
    try:
        from apps.api.services.file_manager import list_files
        return list_files(path)
    except ValueError as e:
        raise HTTPException(status_code=403, detail=str(e))
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.get("/api/files/download/{file_path:path}")
async def download_file(
    file_path: str = Path(..., description="Path relative to data directory"),
):
    try:
        safe_path = os.path.abspath(os.path.join(settings.DATA_DIR, file_path))
        if not safe_path.startswith(os.path.abspath(settings.DATA_DIR)):
            raise HTTPException(status_code=403, detail="Access denied")

        if not os.path.exists(safe_path) or not os.path.isfile(safe_path):
            raise HTTPException(status_code=404, detail="File not found")

        return FileResponse(safe_path, filename=os.path.basename(safe_path))
    except HTTPException:
        raise
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.delete("/api/files/delete/{file_path:path}")
async def delete_file_endpoint(
    file_path: str = Path(..., description="Path relative to data directory"),
):
    try:
        from apps.api.services.file_manager import list_files, delete_path
        success = delete_path(file_path)
        if not success:
            raise HTTPException(
                status_code=404, detail="File not found or could not be deleted"
            )
        return {"status": "success", "message": "File deleted"}
    except ValueError as e:
        raise HTTPException(status_code=403, detail=str(e))
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))
