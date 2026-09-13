import io
import json
import sqlite3
import tarfile
from pathlib import Path

import pytest

from apps.api.services.backup import (
    RESTORE_CONFIRMATION,
    create_backup,
    restore_backup,
    verify_backup,
)


class FakePgTools:
    def __init__(self):
        self.commands = []

    def __call__(self, command, **kwargs):
        self.commands.append((command, kwargs))
        if command[0] == "pg_dump":
            Path(command[command.index("--file") + 1]).write_bytes(b"postgres-custom-dump")
        return object()


def _data_dir(tmp_path: Path) -> Path:
    data = tmp_path / "data"
    data.mkdir()
    with sqlite3.connect(data / "workspaces.db") as db:
        db.execute("CREATE TABLE marker (value TEXT)")
        db.execute("INSERT INTO marker VALUES ('consistent')")
    (data / "artifact.txt").write_text("tenant artifact", encoding="utf-8")
    return data


def test_backup_verify_and_restore_drill(tmp_path):
    tools = FakePgTools()
    archive = tmp_path / "backup.tar.gz"
    report = create_backup(
        archive, data_dir=_data_dir(tmp_path),
        database_url="postgresql+psycopg://owner:secret@db:5433/opengtm",
        runner=tools,
    )
    assert report["archive_sha256"]
    assert verify_backup(archive)["files"]
    dump_command, dump_kwargs = tools.commands[0]
    assert dump_command[0] == "pg_dump"
    assert "secret" not in " ".join(dump_command)
    assert dump_kwargs["env"]["PGPASSWORD"] == "secret"

    restored = tmp_path / "restored-data"
    result = restore_backup(
        archive, target_data_dir=restored,
        database_url="postgresql://restore:target@db/restored",
        confirmation=RESTORE_CONFIRMATION, runner=tools,
    )
    assert result["restored"] is True
    assert (restored / "artifact.txt").read_text(encoding="utf-8") == "tenant artifact"
    with sqlite3.connect(restored / "workspaces.db") as db:
        assert db.execute("SELECT value FROM marker").fetchone()[0] == "consistent"
    restore_command, restore_kwargs = tools.commands[-1]
    assert restore_command[0] == "pg_restore"
    assert restore_kwargs["env"]["PGPASSWORD"] == "target"


def test_restore_requires_confirmation_and_empty_target(tmp_path):
    tools = FakePgTools()
    archive = tmp_path / "backup.tar.gz"
    create_backup(archive, data_dir=_data_dir(tmp_path), database_url="postgresql://db/opengtm", runner=tools)
    with pytest.raises(ValueError, match="requires --confirm"):
        restore_backup(archive, target_data_dir=tmp_path / "restore", database_url="postgresql://db/test", confirmation="no", runner=tools)
    occupied = tmp_path / "occupied"
    occupied.mkdir()
    (occupied / "keep").write_text("safe", encoding="utf-8")
    with pytest.raises(ValueError, match="must be empty"):
        restore_backup(archive, target_data_dir=occupied, database_url="postgresql://db/test", confirmation=RESTORE_CONFIRMATION, runner=tools)


def test_verify_rejects_tampered_and_traversal_archives(tmp_path):
    bad = tmp_path / "bad.tar.gz"
    with tarfile.open(bad, "w:gz") as archive:
        info = tarfile.TarInfo("../escape")
        info.size = 1
        archive.addfile(info, io.BytesIO(b"x"))
    with pytest.raises(ValueError, match="Unsafe backup member"):
        verify_backup(bad)

    malformed = tmp_path / "malformed.tar.gz"
    with tarfile.open(malformed, "w:gz") as archive:
        payload = json.dumps({"format": "opengtm-backup", "version": 1, "files": []}).encode()
        info = tarfile.TarInfo("manifest.json")
        info.size = len(payload)
        archive.addfile(info, io.BytesIO(payload))
    with pytest.raises(ValueError, match="PostgreSQL dump"):
        verify_backup(malformed)
