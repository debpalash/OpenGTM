"""
Entity resolution service (Pillar 1) — build-on over services/dedup.py.

resolve_company() takes a sourced lead and returns the canonical CompanyEntity it
belongs to (matching via the existing compare_leads scorer + a persisted blocking
index), creating one if none matches. Each call records an *observation*, growing
corroboration_count (distinct sources) and per-field provenance.

Matching reuses dedup.compare_leads (weighted Jaro-Winkler, domain/phone boosts).
We only add: persistence, corroboration, a grey-band review queue, and merge/split.
"""

import logging
from collections import Counter
from datetime import datetime, timezone
from typing import Optional, Tuple

from sqlalchemy.orm import Session

from apps.api.services.dedup import (
    compare_leads, normalize_company, normalize_domain, normalize_phone,
)
from apps.api.services.entities.models import (
    CompanyEntity, EntityBlockingKey, EntityMergeLog, EntityReviewPair,
)
from apps.api.services.workbook.models import WorkbookRow

logger = logging.getLogger("entities.graph")

MATCH_THRESHOLD = 0.85   # >= → same entity (matches dedup.is_duplicate)
GREY_BAND = 0.70         # [GREY_BAND, MATCH_THRESHOLD) → queue for review, don't auto-merge

# Lead fields carried into entity provenance.
_PROV_FIELDS = (
    "company", "website", "email", "phone", "city", "state", "address",
    "contact_person", "contact_title", "description", "linkedin_url",
    "company_size", "specialization", "industry_tags",
)


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _blocking_keys(lead: dict) -> set:
    """Same blocking scheme as LeadDeduplicator, for a single record."""
    keys = set()
    name = normalize_company(lead.get("company", ""))
    if len(name) >= 3:
        keys.add(f"name:{name[:3]}")
    domain = normalize_domain(lead.get("website", ""))
    if domain:
        keys.add(f"domain:{domain}")
    phone = normalize_phone(lead.get("phone", ""))
    if len(phone) >= 7:
        keys.add(f"phone:{phone[-7:]}")
    city = (lead.get("city", "") or "").lower().strip()
    first_word = name.split()[0] if name else ""
    if city and first_word:
        keys.add(f"city:{city}:{first_word}")
    return keys


def _ensure_blocking_keys(db: Session, entity_id: str, keys: set):
    if not keys:
        return
    existing = {
        k for (k,) in db.query(EntityBlockingKey.key)
        .filter(EntityBlockingKey.entity_id == entity_id).all()
    }
    for k in keys - existing:
        db.add(EntityBlockingKey(key=k, entity_id=entity_id))


def _candidate_ids(db: Session, keys: set) -> set:
    if not keys:
        return set()
    rows = db.query(EntityBlockingKey.entity_id).filter(
        EntityBlockingKey.key.in_(list(keys))
    ).all()
    return {r[0] for r in rows}


def _recompute(entity: CompanyEntity):
    """Frequency-based winners → convenience columns; corroboration + agreement."""
    fields = entity.fields or {}
    agreement = {}
    for field, observations in fields.items():
        values = [o.get("value") for o in observations if o.get("value")]
        if not values:
            continue
        counts = Counter(values)
        winner, n = counts.most_common(1)[0]
        agreement[field] = round(n / len(values), 3)
        if field == "company":
            entity.canonical_name = winner
        elif field == "website":
            entity.primary_domain = normalize_domain(winner)
        elif field == "phone":
            entity.primary_phone = winner
        elif field == "email":
            entity.primary_email = winner
        elif field == "city":
            entity.primary_city = winner
    entity.source_agreement = agreement
    sources = entity.sources or []
    entity.corroboration_count = len(set(sources))


def _create_entity(db: Session, lead: dict, source: str, workspace_id: str = "") -> CompanyEntity:
    fields = {}
    for f in _PROV_FIELDS:
        v = lead.get(f)
        if v not in (None, "", "N/A"):
            fields[f] = [{"value": str(v), "source": source, "observed_at": _now()}]

    domain = normalize_domain(lead.get("website", ""))
    entity = CompanyEntity(
        workspace_id=workspace_id or "",
        canonical_name=str(lead.get("company") or "").strip() or "(unknown)",
        primary_domain=domain,
        primary_phone=str(lead.get("phone") or ""),
        primary_email=str(lead.get("email") or ""),
        primary_city=str(lead.get("city") or ""),
        identity_keys={
            "domains": [domain] if domain else [],
            "phones": [normalize_phone(lead.get("phone", ""))] if lead.get("phone") else [],
            "emails": [str(lead.get("email"))] if lead.get("email") else [],
            "name_variants": [str(lead.get("company"))] if lead.get("company") else [],
        },
        fields=fields,
        sources=[source] if source else [],
        corroboration_count=1 if source else 0,
        observation_count=1,
    )
    db.add(entity)
    db.flush()  # assign id
    _ensure_blocking_keys(db, entity.id, _blocking_keys(lead))
    return entity


def _record_observation(db: Session, entity: CompanyEntity, lead: dict, source: str):
    fields = dict(entity.fields or {})
    for f in _PROV_FIELDS:
        v = lead.get(f)
        if v in (None, "", "N/A"):
            continue
        fields.setdefault(f, [])
        fields[f] = fields[f] + [{"value": str(v), "source": source, "observed_at": _now()}]
    entity.fields = fields

    ik = dict(entity.identity_keys or {})
    def _add(key, val):
        if not val:
            return
        lst = list(ik.get(key, []))
        if val not in lst:
            lst.append(val)
            ik[key] = lst
    _add("domains", normalize_domain(lead.get("website", "")))
    _add("phones", normalize_phone(lead.get("phone", "")))
    _add("emails", str(lead.get("email")) if lead.get("email") else "")
    _add("name_variants", str(lead.get("company")) if lead.get("company") else "")
    entity.identity_keys = ik

    sources = list(entity.sources or [])
    if source and source not in sources:
        sources.append(source)
    entity.sources = sources
    entity.observation_count = (entity.observation_count or 0) + 1

    _ensure_blocking_keys(db, entity.id, _blocking_keys(lead))
    _recompute(entity)


def resolve_company(
    db: Session, lead: dict, observation_source: Optional[str] = None,
    workspace_id: str = "",
) -> Tuple[CompanyEntity, bool]:
    """Resolve a sourced lead to a canonical CompanyEntity (matching or new).

    Returns (entity, created). Records the observation either way. Grey-band near
    misses create a new entity AND an EntityReviewPair (conservative — no auto-merge).
    Matching is scoped to `workspace_id` — entities never cross tenants.
    """
    source = observation_source or str(lead.get("source") or "") or "unknown"
    ws = workspace_id or ""
    keys = _blocking_keys(lead)
    best, best_score = None, 0.0
    for eid in _candidate_ids(db, keys):
        ent = db.get(CompanyEntity, eid)
        if not ent or (ent.workspace_id or "") != ws:
            continue  # tenant isolation
        res = compare_leads(lead, ent.repr_dict())
        if res.score > best_score:
            best, best_score = ent, res.score

    if best is not None and best_score >= MATCH_THRESHOLD:
        _record_observation(db, best, lead, source)
        # Flush so this entity's (possibly new) blocking keys are visible to the
        # next resolve_company() in the same batch — SessionLocal is autoflush=False.
        db.flush()
        return best, False

    entity = _create_entity(db, lead, source, workspace_id=ws)
    if best is not None and GREY_BAND <= best_score < MATCH_THRESHOLD:
        db.add(EntityReviewPair(
            entity_id=best.id,
            candidate={"new_entity_id": entity.id, **entity.repr_dict()},
            score=round(best_score, 4),
            reason="grey_band",
        ))
    db.flush()  # make new entity + blocking keys visible to subsequent resolves
    return entity, True


# ── Merge / Split (human-in-the-loop corrections) ────────────────────────

def merge_entities(db: Session, kept_id: str, merged_id: str, reason: str = "manual") -> dict:
    """Merge `merged_id` into `kept_id`. Snapshots the merged entity for undo."""
    if kept_id == merged_id:
        return {"error": "same_entity"}
    kept = db.get(CompanyEntity, kept_id)
    merged = db.get(CompanyEntity, merged_id)
    if not kept or not merged:
        return {"error": "entity_not_found"}

    # Snapshot for split()
    merged_keys = [k for (k,) in db.query(EntityBlockingKey.key)
                   .filter(EntityBlockingKey.entity_id == merged_id).all()]
    row_ids = [r.id for r in db.query(WorkbookRow)
               .filter(WorkbookRow.canonical_entity_id == merged_id).all()]
    snapshot = {"entity": merged.to_api(), "blocking_keys": merged_keys, "row_ids": row_ids}

    # Combine provenance
    kf = dict(kept.fields or {})
    for field, obs in (merged.fields or {}).items():
        kf[field] = kf.get(field, []) + obs
    kept.fields = kf
    kept.sources = list({*(kept.sources or []), *(merged.sources or [])})
    kept.observation_count = (kept.observation_count or 0) + (merged.observation_count or 0)

    # Repoint blocking keys + workbook rows
    _ensure_blocking_keys(db, kept_id, set(merged_keys))
    db.query(WorkbookRow).filter(WorkbookRow.canonical_entity_id == merged_id).update(
        {WorkbookRow.canonical_entity_id: kept_id}, synchronize_session=False
    )

    score = compare_leads(merged.repr_dict(), kept.repr_dict()).score
    db.add(EntityMergeLog(kept_id=kept_id, merged_id=merged_id, reason=reason,
                          score=score, snapshot=snapshot))

    db.query(EntityBlockingKey).filter(EntityBlockingKey.entity_id == merged_id).delete(
        synchronize_session=False
    )
    db.delete(merged)
    _recompute(kept)
    db.commit()
    return {"kept_id": kept_id, "merged_id": merged_id, "corroboration_count": kept.corroboration_count}


def split_entity(db: Session, merge_log_id: int) -> dict:
    """Undo a merge: recreate the merged entity from snapshot and rebind its rows."""
    log = db.get(EntityMergeLog, merge_log_id)
    if not log or log.reverted:
        return {"error": "merge_log_not_found_or_reverted"}
    snap = log.snapshot or {}
    ent_data = snap.get("entity") or {}
    eid = ent_data.get("id")
    if not eid:
        return {"error": "bad_snapshot"}

    entity = CompanyEntity(
        id=eid,
        canonical_name=ent_data.get("canonical_name") or "(unknown)",
        primary_domain=ent_data.get("primary_domain", ""),
        primary_phone=ent_data.get("primary_phone", ""),
        primary_email=ent_data.get("primary_email", ""),
        primary_city=ent_data.get("primary_city", ""),
        identity_keys=ent_data.get("identity_keys", {}),
        fields=ent_data.get("fields", {}),
        sources=ent_data.get("sources", []),
        source_agreement=ent_data.get("source_agreement", {}),
        corroboration_count=ent_data.get("corroboration_count", 1),
        observation_count=ent_data.get("observation_count", 1),
    )
    db.add(entity)
    db.flush()
    _ensure_blocking_keys(db, eid, set(snap.get("blocking_keys", [])))
    for rid in snap.get("row_ids", []):
        db.query(WorkbookRow).filter(WorkbookRow.id == rid).update(
            {WorkbookRow.canonical_entity_id: eid}, synchronize_session=False
        )
    log.reverted = 1
    db.commit()
    return {"restored_id": eid, "rebound_rows": len(snap.get("row_ids", []))}


def get_entity(db: Session, entity_id: str) -> Optional[dict]:
    ent = db.get(CompanyEntity, entity_id)
    return ent.to_api() if ent else None
