"""
Canonical Entity Graph models (Pillar 1).

A sourced lead from any of the 91 sources is resolved to a canonical
`CompanyEntity` — a *persisted* dedup cluster (build-on over the existing
services/dedup.py::LeadDeduplicator matcher). Multiple observations of the same
company across sources/runs collapse into one entity whose `corroboration_count`
(distinct sources that have seen it) is the trust signal Apollo's single DB and
Clay's import-only model can't produce.

See docs/specs/workbook-v2-source-engine-spec.md (Pillar 4 / Reuse Map).
"""

import uuid
from sqlalchemy import (
    Column, String, Integer, Text, DateTime, Float, JSON, ForeignKey, Index,
    UniqueConstraint,
)
from sqlalchemy.sql import func

from apps.api.database import Base


def _uuid() -> str:
    return str(uuid.uuid4())


class CompanyEntity(Base):
    """A canonical company — one resolved identity across many source observations."""
    __tablename__ = "company_entities"

    id = Column(String, primary_key=True, default=_uuid)
    # Tenant isolation: entities never resolve/corroborate across workspaces.
    # "" = global/unscoped (matches today's not-yet-partitioned leads).
    workspace_id = Column(String, nullable=False, default="", index=True)
    canonical_name = Column(String(512), nullable=False)
    # Convenience "winning" values (for fast compare/display); full provenance in `fields`
    primary_domain = Column(String(255), index=True, default="")
    primary_phone = Column(String(64), default="")
    primary_email = Column(String(255), default="")
    primary_city = Column(String(128), default="")

    # {domains:[], phones:[], emails:[], name_variants:[]}
    identity_keys = Column(JSON, default=dict)
    # {field: [{value, source, observed_at}]} — full per-field provenance
    fields = Column(JSON, default=dict)
    # distinct source strings that have observed this entity (lineage)
    sources = Column(JSON, default=list)
    # {field: agreement_ratio} — how strongly observations agree on the winning value
    source_agreement = Column(JSON, default=dict)

    corroboration_count = Column(Integer, default=1, index=True)  # = len(distinct sources)
    observation_count = Column(Integer, default=1)                # total observations

    first_seen = Column(DateTime, server_default=func.now())
    last_seen = Column(DateTime, server_default=func.now(), onupdate=func.now())

    def repr_dict(self) -> dict:
        """Lead-shaped dict for compare_leads()."""
        return {
            "id": self.id,
            "company": self.canonical_name,
            "website": self.primary_domain,
            "phone": self.primary_phone,
            "email": self.primary_email,
            "city": self.primary_city,
        }

    def to_api(self) -> dict:
        return {
            "id": self.id,
            "workspace_id": self.workspace_id or "",
            "canonical_name": self.canonical_name,
            "primary_domain": self.primary_domain,
            "primary_phone": self.primary_phone,
            "primary_email": self.primary_email,
            "primary_city": self.primary_city,
            "identity_keys": self.identity_keys or {},
            "fields": self.fields or {},
            "sources": self.sources or [],
            "source_agreement": self.source_agreement or {},
            "corroboration_count": self.corroboration_count,
            "observation_count": self.observation_count,
        }


class PersonEntity(Base):
    """A canonical person (modeled for completeness; resolution is company-first in P1)."""
    __tablename__ = "person_entities"

    id = Column(String, primary_key=True, default=_uuid)
    workspace_id = Column(String, nullable=False, index=True)
    full_name = Column(String(255), nullable=False)
    company_entity_id = Column(String, ForeignKey("company_entities.id"), nullable=True, index=True)
    identity_keys = Column(JSON, default=dict)
    fields = Column(JSON, default=dict)
    corroboration_count = Column(Integer, default=1)
    first_seen = Column(DateTime, server_default=func.now())
    last_seen = Column(DateTime, server_default=func.now(), onupdate=func.now())


class PersonIdentifier(Base):
    """An exact identity key owned by one person per workspace.

    Kinds: "linkedin" (normalized linkedin.com/in/<slug>), "email", and
    "legacy_id" (the pre-persistence person_… / person:… ids, so earlier chat
    actions and workbook rows keep resolving to the same person).
    """
    __tablename__ = "person_identifiers"
    __table_args__ = (
        UniqueConstraint("workspace_id", "kind", "value", name="uq_person_identifier"),
    )

    id = Column(Integer, primary_key=True, autoincrement=True)
    workspace_id = Column(String, nullable=False, index=True)
    kind = Column(String(32), nullable=False)
    value = Column(String(512), nullable=False)
    person_id = Column(String, ForeignKey("person_entities.id", ondelete="CASCADE"),
                       nullable=False, index=True)
    created_at = Column(DateTime, server_default=func.now())


class PersonEmployment(Base):
    """Observed employment of a person at one company (history, not a snapshot).

    One row per person and company key; repeated observations extend
    last_observed_at. The most recently observed employment is current, so a
    job change keeps the person and moves `is_current` to the new company.
    """
    __tablename__ = "person_employments"
    __table_args__ = (
        UniqueConstraint("workspace_id", "person_id", "company_key", name="uq_person_employment"),
    )

    id = Column(Integer, primary_key=True, autoincrement=True)
    workspace_id = Column(String, nullable=False, index=True)
    person_id = Column(String, ForeignKey("person_entities.id", ondelete="CASCADE"),
                       nullable=False, index=True)
    company_key = Column(String(512), nullable=False)
    company_entity_id = Column(String, ForeignKey("company_entities.id", ondelete="SET NULL"),
                               nullable=True, index=True)
    company_name = Column(String(512), default="")
    company_domain = Column(String(255), default="")
    title = Column(String(512), default="")
    # [{title, observed_at, source}] — title changes at the same company
    titles = Column(JSON, default=list)
    source = Column(String(255), default="")
    evidence_url = Column(String(1024), default="")
    first_observed_at = Column(DateTime, nullable=False)
    last_observed_at = Column(DateTime, nullable=False)
    is_current = Column(Integer, nullable=False, default=1)


class EntityBlockingKey(Base):
    """Index from a blocking key → entity, so resolution is O(block) not O(all entities)."""
    __tablename__ = "entity_blocking_keys"

    id = Column(Integer, primary_key=True, autoincrement=True)
    workspace_id = Column(String, nullable=False, index=True)
    key = Column(String(255), nullable=False, index=True)
    entity_id = Column(String, ForeignKey("company_entities.id", ondelete="CASCADE"), nullable=False, index=True)


Index("ix_entity_blocking_key_entity", EntityBlockingKey.key, EntityBlockingKey.entity_id)


class CompanyIdentifier(Base):
    """An exact identity key owned by exactly one company entity per workspace.

    The unique constraint is what makes resolution converge under concurrency:
    two imports of the same domain cannot both create an owner. Kinds: "domain"
    (a company's own registrable host, never a shared platform host).
    """
    __tablename__ = "company_identifiers"
    __table_args__ = (
        UniqueConstraint("workspace_id", "kind", "value", name="uq_company_identifier"),
    )

    id = Column(Integer, primary_key=True, autoincrement=True)
    workspace_id = Column(String, nullable=False, index=True)
    kind = Column(String(32), nullable=False)
    value = Column(String(255), nullable=False)
    entity_id = Column(String, ForeignKey("company_entities.id", ondelete="CASCADE"),
                       nullable=False, index=True)
    created_at = Column(DateTime, server_default=func.now())


class EntityMergeLog(Base):
    """Audit + undo support. `snapshot` holds the merged entity's full state for split()."""
    __tablename__ = "entity_merge_log"

    id = Column(Integer, primary_key=True, autoincrement=True)
    workspace_id = Column(String, nullable=False, index=True)
    kept_id = Column(String, index=True)
    merged_id = Column(String, index=True)
    reason = Column(String(255), default="")
    score = Column(Float, default=0.0)
    snapshot = Column(JSON, default=dict)   # merged entity + its blocking keys + bound row ids
    reverted = Column(Integer, default=0)   # 1 after a split() restores it
    created_at = Column(DateTime, server_default=func.now())


class EntityReviewPair(Base):
    """Grey-band (0.70–0.85) ambiguous match queued for human review."""
    __tablename__ = "entity_review_pairs"

    id = Column(Integer, primary_key=True, autoincrement=True)
    workspace_id = Column(String, nullable=False, index=True)
    entity_id = Column(String, index=True)        # existing entity
    candidate = Column(JSON, default=dict)        # the other record (new entity repr)
    score = Column(Float, default=0.0)
    reason = Column(String(255), default="grey_band")
    status = Column(String(32), default="pending", index=True)  # pending | merged | rejected
    created_at = Column(DateTime, server_default=func.now())
