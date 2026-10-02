"""A merge uses committed evidence, never a session's stale identity-map copy."""
import pytest
from sqlalchemy import create_engine, event
from sqlalchemy.orm import Session

from apps.api.database import Base
from apps.api.services.entities.graph import merge_entities, resolve_company
from apps.api.services.entities.models import CompanyEntity, EntityMergeLog
from apps.api.services.poller.models import WatchSubscription  # register watch table


@pytest.mark.parametrize("observed_side", ["kept", "merged"])
def test_merge_preserves_observations_committed_by_another_session(tmp_path, observed_side):
    engine = create_engine(f"sqlite:///{tmp_path / 'merge.db'}")
    @event.listens_for(engine, "connect")
    def foreign_keys(connection, _):
        connection.execute("PRAGMA foreign_keys=ON")
    Base.metadata.create_all(engine)
    with Session(engine, autoflush=False) as seed:
        kept, _ = resolve_company(seed, {"company": "Keep Original", "website": "keep.example"}, "source-a", workspace_id="ws")
        merged, _ = resolve_company(seed, {"company": "Separate Original", "website": "separate.example"}, "source-b", workspace_id="ws")
        seed.commit()
        k, m = kept.id, merged.id
    with Session(engine, autoflush=False) as stale:
        # Keep references alive: both rows are cached before the other commit.
        cached_kept, cached_merged = stale.get(CompanyEntity, k), stale.get(CompanyEntity, m)
        assert cached_kept.observation_count == cached_merged.observation_count == 1
        with Session(engine, autoflush=False) as writer:
            domain = "keep.example" if observed_side == "kept" else "separate.example"
            fresh, created = resolve_company(writer, {"company": "Later Observation", "website": domain,
                                                     "email": "new@fixture.example"}, "source-c", workspace_id="ws")
            assert not created and fresh.id == (k if observed_side == "kept" else m)
            writer.commit()
        merge_entities(stale, k, m, workspace_id="ws")
    with Session(engine) as db:
        survivor = db.get(CompanyEntity, k)
        assert survivor.observation_count == 3
        assert sorted(survivor.sources) == ["source-a", "source-b", "source-c"]
        assert survivor.primary_email == "new@fixture.example"
        assert any(o["source"] == "source-c" for o in survivor.fields["company"])
        log = db.query(EntityMergeLog).one()
        assert log.snapshot["entity"]["observation_count"] == (2 if observed_side == "merged" else 1)
    engine.dispose()
