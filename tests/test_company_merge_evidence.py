"""Undo removes only the company evidence contributed by that merge."""
from copy import deepcopy

import pytest
from sqlalchemy import create_engine, event
from sqlalchemy.orm import Session

from apps.api.database import Base
from apps.api.services.entities.graph import merge_entities, resolve_company, split_entity
from apps.api.services.entities.models import CompanyEntity, EntityMergeLog
from apps.api.services.poller.models import WatchSubscription  # register watch table


@pytest.fixture
def db():
    engine = create_engine("sqlite:///:memory:")
    @event.listens_for(engine, "connect")
    def foreign_keys(connection, _):
        connection.execute("PRAGMA foreign_keys=ON")
    Base.metadata.create_all(engine)
    with Session(engine, autoflush=False) as session:
        yield session
    engine.dispose()


def _state(entity):
    result = deepcopy(entity.to_api())
    result["sources"] = sorted(result["sources"])
    return result


@pytest.mark.parametrize("legacy_snapshot", [False, True])
@pytest.mark.parametrize("same_source", [False, True])
@pytest.mark.parametrize("later_merge", [False, True])
def test_undo_restores_exact_company_evidence(db, same_source, later_merge, legacy_snapshot):
    kept, _ = resolve_company(db, {"company": "Keep Original", "website": "keep.example", "city": "Keep City"},
                              "source-a", workspace_id="ws")
    other, _ = resolve_company(db, {"company": "Separate Original", "website": "separate.example", "phone": "5550001234"},
                               "source-a" if same_source else "source-b", workspace_id="ws")
    db.commit()
    k, m = kept.id, other.id
    before_kept, before_other = _state(kept), _state(other)
    # Fresh creation leaves agreement empty; undo recomputes the surviving evidence.
    before_kept["source_agreement"] = {field: 1.0 for field in before_kept["fields"]}
    merge_entities(db, k, m, workspace_id="ws")
    log_id = db.query(EntityMergeLog).filter_by(merged_id=m).one().id
    if later_merge:
        third, _ = resolve_company(db, {"company": "Third Original", "website": "third.example"}, "source-c", workspace_id="ws")
        db.commit()
        third_id = third.id
        merge_entities(db, k, third_id, workspace_id="ws")
    if legacy_snapshot:
        for log in db.query(EntityMergeLog).all():
            log.snapshot = {key: value for key, value in log.snapshot.items() if key != "kept_entity"}
        db.commit()
    split_entity(db, log_id, workspace_id="ws")
    if later_merge:
        split_entity(db, db.query(EntityMergeLog).filter_by(merged_id=third_id).one().id, workspace_id="ws")
    db.expire_all()
    assert _state(db.get(CompanyEntity, k)) == before_kept
    assert _state(db.get(CompanyEntity, m)) == before_other


def test_undo_keeps_later_observations_including_the_merged_source(db):
    kept, _ = resolve_company(db, {"company": "Keep Original", "website": "keep.example"}, "source-a", workspace_id="ws")
    other, _ = resolve_company(db, {"company": "Separate Original", "website": "separate.example", "phone": "5550001234"},
                               "source-b", workspace_id="ws")
    db.commit()
    k, m = kept.id, other.id
    before_kept, before_other = _state(kept), _state(other)
    # Fresh creation leaves agreement empty; undo recomputes the surviving evidence.
    before_kept["source_agreement"] = {field: 1.0 for field in before_kept["fields"]}
    merge_entities(db, k, m, workspace_id="ws")
    later, created = resolve_company(db, {"company": "Keep Original", "website": "keep.example", "email": "new@keep.example"},
                                     "source-b", workspace_id="ws")
    assert not created and later.id == k
    db.commit()
    later_observations = {f: deepcopy(obs[-1]) for f, obs in later.fields.items()
                          if f in ("company", "website", "email")}
    split_entity(db, db.query(EntityMergeLog).one().id, workspace_id="ws")
    db.expire_all()
    kept = db.get(CompanyEntity, k)
    assert kept.observation_count == before_kept["observation_count"] + 1
    assert sorted(kept.sources) == ["source-a", "source-b"]
    assert kept.corroboration_count == 2
    assert kept.primary_phone == ""
    assert kept.primary_email == "new@keep.example"
    assert kept.fields == {**before_kept["fields"], **{f: before_kept["fields"].get(f, []) + [obs]
                                                     for f, obs in later_observations.items()}}
    assert _state(db.get(CompanyEntity, m)) == before_other
