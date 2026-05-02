import os
import shutil
from typing import List, Dict, Optional
from datetime import datetime
from apps.api.core.config import settings

DATA_DIR = settings.DATA_DIR


def get_file_type(filename: str) -> str:
    ext = os.path.splitext(filename)[1].lower()
    if ext in [".mp4", ".mkv", ".webm", ".mov"]:
        return "video"
    elif ext in [".mp3", ".wav", ".aac", ".ogg"]:
        return "audio"
    elif ext in [".pdf", ".epub", ".mobi", ".txt", ".md"]:
        return "document"
    elif ext in [".jpg", ".jpeg", ".png", ".gif", ".webp"]:
        return "image"
    return "other"


def list_files(path: str = "") -> List[Dict]:
    """
    List files in the data directory.
    Security: Ensures path does not traverse outside DATA_DIR.
    """
    # Normalize path to prevent traversal
    safe_path = os.path.abspath(os.path.join(DATA_DIR, path))
    if not safe_path.startswith(os.path.abspath(DATA_DIR)):
        raise ValueError("Invalid path: Access denied")

    if not os.path.exists(safe_path):
        return []

    results = []
    with os.scandir(safe_path) as entries:
        for entry in entries:
            if entry.name.startswith("."):
                continue

            stats = entry.stat()
            file_info = {
                "name": entry.name,
                "path": os.path.relpath(entry.path, DATA_DIR),
                "is_dir": entry.is_dir(),
                "size": stats.st_size if not entry.is_dir() else 0,
                "modified": datetime.fromtimestamp(stats.st_mtime).isoformat(),
                "type": "folder" if entry.is_dir() else get_file_type(entry.name),
            }
            results.append(file_info)

    # Sort: Folders first, then by modified date desc
    results.sort(key=lambda x: (not x["is_dir"], x["modified"]), reverse=True)
    return results


def delete_path(path: str) -> bool:
    """
    Delete a file or directory.
    Security: Ensures path does not traverse outside DATA_DIR.
    """
    safe_path = os.path.abspath(os.path.join(DATA_DIR, path))
    if not safe_path.startswith(os.path.abspath(DATA_DIR)):
        raise ValueError("Invalid path: Access denied")

    if not os.path.exists(safe_path):
        return False

    if os.path.isdir(safe_path):
        shutil.rmtree(safe_path)
    else:
        os.remove(safe_path)
    return True
