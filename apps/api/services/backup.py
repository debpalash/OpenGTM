"""Integrity-checked deployment backup and restore primitives."""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import sqlite3
import subprocess
import tarfile
import tempfile
from datetime import datetime, timezone
from pathlib import Path
from typing import Callable

from sqlalchemy.engine import make_url

FORMAT_VERSION = 1
RESTORE_CONFIRMATION = "RESTORE OPENGTM BACKUP"
Runner = Callable[..., subprocess.CompletedProcess]


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _pg_command(tool: str, database_url: str, *args: str) -> tuple[list[str], dict[str, str]]:
    url = make_url(database_url)
    if url.get_backend_name() != "postgresql":
        raise ValueError("Backup and restore require a PostgreSQL DATABASE_URL")
    command = [tool, "--host", url.host or "localhost", "--port", str(url.port or 5432)]
    if url.username:
        command.extend(["--username", url.username])
    command.extend([*args, url.database or "postgres"])
    env = os.environ.copy()
    if url.password:
        env["PGPASSWORD"] = url.password
    return command, env


def _snapshot_data(source: Path, destination: Path) -> None:
    destination.mkdir(parents=True)
    for item in source.rglob("*"):
        relative = item.relative_to(source)
        target = destination / relative
        if item.is_symlink():
            raise ValueError(f"Refusing to back up symlink: {relative.as_posix()}")
        if item.is_dir():
            target.mkdir(parents=True, exist_ok=True)
        elif item.suffix == ".db":
            target.parent.mkdir(parents=True, exist_ok=True)
            source_db = sqlite3.connect(item)
            target_db = sqlite3.connect(target)
            try:
                source_db.backup(target_db)
            finally:
                target_db.close()
                source_db.close()
        elif not item.name.endswith(("-wal", "-shm")):
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(item, target)


def _manifest(root: Path) -> dict:
    files = []
    for path in sorted(p for p in root.rglob("*") if p.is_file()):
        relative = path.relative_to(root).as_posix()
        files.append({"path": relative, "size": path.stat().st_size, "sha256": _sha256(path)})
    return {
        "format": "opengtm-backup",
        "version": FORMAT_VERSION,
        "created_at": datetime.now(timezone.utc).isoformat(),
        "files": files,
    }


def create_backup(
    output: Path,
    *,
    data_dir: Path,
    database_url: str,
    runner: Runner = subprocess.run,
) -> dict:
    output = output.resolve()
    if output.exists():
        raise FileExistsError(f"Backup already exists: {output}")
    if not data_dir.is_dir():
        raise FileNotFoundError(f"Data directory not found: {data_dir}")
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="opengtm-backup-") as temp_name:
        root = Path(temp_name) / "payload"
        root.mkdir()
        dump = root / "postgres.dump"
        command, env = _pg_command("pg_dump", database_url, "--format=custom", "--no-owner", "--file", str(dump))
        runner(command, env=env, check=True, capture_output=True)
        _snapshot_data(data_dir.resolve(), root / "data")
        manifest = _manifest(root)
        (root / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
        with tarfile.open(output, "w:gz") as archive:
            for path in sorted(root.rglob("*")):
                archive.add(path, arcname=path.relative_to(root).as_posix(), recursive=False)
    try:
        output.chmod(0o600)
    except OSError:
        pass
    return {**manifest, "archive": str(output), "archive_sha256": _sha256(output)}


def _safe_members(archive: tarfile.TarFile) -> list[tarfile.TarInfo]:
    members = archive.getmembers()
    names: set[str] = set()
    for member in members:
        path = Path(member.name)
        if (
            path.is_absolute() or ".." in path.parts or member.name in names
            or not (member.isfile() or member.isdir())
        ):
            raise ValueError(f"Unsafe backup member: {member.name}")
        names.add(member.name)
    return members


def verify_backup(archive_path: Path) -> dict:
    with tempfile.TemporaryDirectory(prefix="opengtm-verify-") as temp_name:
        target = Path(temp_name)
        with tarfile.open(archive_path, "r:gz") as archive:
            archive.extractall(target, members=_safe_members(archive), filter="data")
        manifest_path = target / "manifest.json"
        if not manifest_path.is_file():
            raise ValueError("Backup has no manifest.json")
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        if manifest.get("format") != "opengtm-backup" or manifest.get("version") != FORMAT_VERSION:
            raise ValueError("Unsupported OpenGTM backup format")
        expected = {record["path"] for record in manifest.get("files", [])}
        actual = {
            path.relative_to(target).as_posix()
            for path in target.rglob("*")
            if path.is_file() and path.name != "manifest.json"
        }
        if actual != expected:
            raise ValueError("Backup file inventory does not match its manifest")
        for record in manifest.get("files", []):
            path = target / record["path"]
            if not path.is_file() or path.stat().st_size != record["size"] or _sha256(path) != record["sha256"]:
                raise ValueError(f"Backup integrity check failed: {record['path']}")
        if not (target / "postgres.dump").is_file():
            raise ValueError("Backup has no PostgreSQL dump")
        return {**manifest, "archive": str(archive_path.resolve()), "archive_sha256": _sha256(archive_path.resolve())}


def restore_backup(
    archive_path: Path,
    *,
    target_data_dir: Path,
    database_url: str,
    confirmation: str,
    runner: Runner = subprocess.run,
) -> dict:
    if confirmation != RESTORE_CONFIRMATION:
        raise ValueError(f'Restore requires --confirm "{RESTORE_CONFIRMATION}"')
    verify_backup(archive_path)
    target_data_dir = target_data_dir.resolve()
    if target_data_dir.exists() and any(target_data_dir.iterdir()):
        raise ValueError("Restore data directory must not exist or must be empty")
    with tempfile.TemporaryDirectory(prefix="opengtm-restore-") as temp_name:
        extracted = Path(temp_name)
        with tarfile.open(archive_path, "r:gz") as archive:
            archive.extractall(extracted, members=_safe_members(archive), filter="data")
        command, env = _pg_command(
            "pg_restore", database_url, "--exit-on-error", "--clean", "--if-exists", "--no-owner", "--dbname"
        )
        # pg_restore expects the archive after options and database selection.
        command.append(str(extracted / "postgres.dump"))
        runner(command, env=env, check=True, capture_output=True)
        target_data_dir.mkdir(parents=True, exist_ok=True)
        shutil.copytree(extracted / "data", target_data_dir, dirs_exist_ok=True)
    return {"restored": True, "data_dir": str(target_data_dir), "archive": str(archive_path.resolve())}
