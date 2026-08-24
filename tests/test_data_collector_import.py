"""Durable Brazil CSV import: tenant stamps, limits, and safe checkpoints."""

import asyncio
import csv

import pytest

from apps.api.services.leadgen.scrapers import data_collector_import as importer


class _Store:
    def __init__(self, fail=False):
        self.fail = fail
        self.leads = []
        self.closed = False

    def bulk_upsert(self, leads):
        if self.fail:
            raise RuntimeError("write failed")
        self.leads.extend(leads)
        return len(leads)

    def close(self):
        self.closed = True


def _csv(path):
    with path.open("w", newline="", encoding="utf-8") as handle:
        writer = csv.DictWriter(
            handle,
            fieldnames=["company_name", "cnpj", "state", "city", "email"],
        )
        writer.writeheader()
        writer.writerow({
            "company_name": "Alpha", "cnpj": "1", "state": "SP",
            "city": "Sao Paulo", "email": "a@example.com",
        })
        writer.writerow({
            "company_name": "Beta", "cnpj": "2", "state": "RJ",
            "city": "Rio", "email": "b@example.com",
        })


def test_stream_limit_and_resume_are_exact_and_tenant_stamped(monkeypatch, tmp_path):
    source = tmp_path / "cnpj.csv"
    checkpoint = tmp_path / "checkpoint.json"
    _csv(source)
    monkeypatch.setattr(importer, "CNPJ_CSV", source)

    store = _Store()
    first = importer._stream_import_cnpj(
        store,
        limit=1,
        checkpoint_file=checkpoint,
        workspace_id="W1",
    )
    assert first == 1
    assert [lead.company for lead in store.leads] == ["Alpha"]
    assert store.leads[0].workspace_id == "W1"
    assert importer._load_checkpoint(checkpoint)["cnpj_row"] == 1
    second = importer._stream_import_cnpj(
        store,
        limit=1,
        checkpoint_file=checkpoint,
        workspace_id="W1",
    )
    assert second == 1
    assert [lead.company for lead in store.leads] == ["Alpha", "Beta"]
    assert importer._load_checkpoint(checkpoint)["cnpj_row"] == 2


def test_failed_batch_does_not_advance_checkpoint(monkeypatch, tmp_path):
    source = tmp_path / "cnpj.csv"
    checkpoint = tmp_path / "checkpoint.json"
    _csv(source)
    monkeypatch.setattr(importer, "CNPJ_CSV", source)
    monkeypatch.setattr(importer, "BATCH_SIZE", 1)

    with pytest.raises(RuntimeError, match="write failed"):
        importer._stream_import_cnpj(
            _Store(fail=True),
            checkpoint_file=checkpoint,
            workspace_id="W1",
        )
    assert importer._load_checkpoint(checkpoint)["cnpj_row"] == 0


def test_queue_handler_uses_tenant_store_and_checkpoint(monkeypatch, tmp_path):
    source = tmp_path / "cnpj.csv"
    checkpoint = tmp_path / "tenant-checkpoint.json"
    _csv(source)
    store = _Store()
    monkeypatch.setattr(importer, "CNPJ_CSV", source)
    monkeypatch.setattr(importer, "checkpoint_path_for", lambda slug: checkpoint)
    monkeypatch.setattr(
        "apps.api.services.leadgen.store.get_lead_store",
        lambda workspace_id, slug: store,
    )
    monkeypatch.setattr(
        "apps.api.services.leadgen.progress.ProgressBus._publish_redis",
        lambda self, workspace_id, event: None,
    )

    asyncio.run(importer.handle_data_collector_import(10, {
        "job_id": "import-one",
        "module": "cnpj",
        "limit": 1,
        "reset": False,
        "workspace_id": "W1",
        "slug": "one",
    }))
    assert store.closed is True
    assert len(store.leads) == 1 and store.leads[0].workspace_id == "W1"
    assert importer._load_checkpoint(checkpoint)["cnpj_row"] == 1
