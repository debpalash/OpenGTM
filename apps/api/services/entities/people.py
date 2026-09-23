"""Persisted person identity and employment history (Phase 1).

A person is identified by their LinkedIn profile (or email), not by the
company they were found at, so a job change keeps the same person and adds
employment history. Earlier ids — Chat's company-scoped ``person_…`` and the
workbook people source's ``person:…`` — are registered as ``legacy_id``
identifiers, so existing actions and rows keep resolving to the same person.

Resolution order: LinkedIn profile, then legacy id, then email. Creation
claims the strongest identifier in a savepoint, so concurrent saves of the
same person converge on one entity (same pattern as company identity).
"""

from datetime import datetime, timezone
from typing import Iterable, Optional, Tuple
from urllib.parse import unquote, urlparse

from sqlalchemy.exc import IntegrityError
from sqlalchemy.orm import Session

from apps.api.services.entities.models import (
    CompanyIdentifier, PersonEmployment, PersonEntity, PersonIdentifier,
)


def linkedin_key(url: str) -> str:
    """Normalize a LinkedIn profile URL to ``linkedin.com/in/<slug>``, else ""."""
    raw = (url or "").strip()
    if not raw:
        return ""
    parsed = urlparse(raw if "://" in raw else f"https://{raw}")
    host = (parsed.hostname or "").lower()
    if not (host == "linkedin.com" or host.endswith(".linkedin.com")):
        return ""
    parts = [p for p in unquote(parsed.path).split("/") if p]
    if len(parts) < 2 or parts[0].lower() != "in":
        return ""
    return f"linkedin.com/in/{parts[1].lower()}"


def _email_key(email: str) -> str:
    value = (email or "").strip().lower()
    return value if "@" in value and "." in value.split("@")[-1] else ""


def _company_key(domain: str, name: str) -> str:
    """Stable per-company key. Deliberately not the entity id: a company may gain
    an entity after the first observation, which must not split the history."""
    if domain:
        return f"domain:{domain}"
    return "name:" + " ".join((name or "").lower().split())


def _owner(db: Session, ws: str, kind: str, value: str) -> Optional[str]:
    row = db.query(PersonIdentifier.person_id).filter_by(
        workspace_id=ws, kind=kind, value=value).first()
    return row[0] if row else None


def _claim(db: Session, ws: str, kind: str, value: str, person_id: str) -> str:
    """Claim an identifier; returns the actual owner (another person keeps theirs)."""
    owner = _owner(db, ws, kind, value)
    if owner:
        return owner
    try:
        with db.begin_nested():
            db.add(PersonIdentifier(workspace_id=ws, kind=kind, value=value, person_id=person_id))
            db.flush()
        return person_id
    except IntegrityError:
        return _owner(db, ws, kind, value) or person_id


def _lock(db: Session, person: PersonEntity):
    """Row-lock then reload before JSON read-modify-write (see company graph)."""
    db.query(PersonEntity).filter(PersonEntity.id == person.id).update(
        {PersonEntity.corroboration_count: PersonEntity.corroboration_count},
        synchronize_session=False)
    db.refresh(person)


def resolve_person(
    db: Session,
    *,
    workspace_id: str,
    name: str,
    company: str,
    source: str,
    title: str = "",
    company_domain: str = "",
    linkedin_url: str = "",
    email: str = "",
    evidence_url: str = "",
    legacy_ids: Iterable[str] = (),
    observed_at: Optional[datetime] = None,
) -> Tuple[PersonEntity, bool]:
    """Resolve (or create) the canonical person and record this employment observation.

    Returns (person, created). The caller owns the transaction.
    """
    if not workspace_id:
        raise ValueError("workspace_id is required")
    ws = workspace_id
    observed = observed_at or datetime.now(timezone.utc).replace(tzinfo=None)
    keys: list[tuple[str, str]] = []
    li = linkedin_key(linkedin_url)
    if li:
        keys.append(("linkedin", li))
    keys.extend(("legacy_id", str(v)) for v in dict.fromkeys(legacy_ids) if str(v or "").strip())
    em = _email_key(email)
    if em:
        keys.append(("email", em))
    if not keys:
        raise ValueError("A person needs a LinkedIn profile, email or legacy id to be resolved")

    person: Optional[PersonEntity] = None
    for kind, value in keys:
        owner = _owner(db, ws, kind, value)
        if owner:
            person = db.get(PersonEntity, owner)
            if person is not None:
                break

    created = False
    if person is None:
        kind, value = keys[0]
        try:
            with db.begin_nested():
                person = PersonEntity(workspace_id=ws, full_name=(name or "").strip() or "(unknown)",
                                      identity_keys={}, fields={}, corroboration_count=0)
                db.add(person)
                db.flush()
                db.add(PersonIdentifier(workspace_id=ws, kind=kind, value=value, person_id=person.id))
                db.flush()
            created = True
        except IntegrityError:
            owner = _owner(db, ws, kind, value)
            person = db.get(PersonEntity, owner) if owner else None
            if person is None:
                raise

    for kind, value in keys:
        _claim(db, ws, kind, value, person.id)

    _lock(db, person)
    identity = dict(person.identity_keys or {})

    def _add(key: str, value: str):
        if value and value not in identity.get(key, []):
            identity[key] = [*identity.get(key, []), value]

    _add("linkedin", li)
    _add("emails", em)
    _add("name_variants", (name or "").strip())
    person.identity_keys = identity
    if (not person.full_name or person.full_name == "(unknown)") and (name or "").strip():
        person.full_name = name.strip()
    sources = list((person.fields or {}).get("sources") or [])
    if source and source not in sources:
        sources.append(source)
    person.fields = {**(person.fields or {}), "sources": sources}
    person.corroboration_count = len(sources)

    _record_employment(db, ws, person, company=company, domain=company_domain, title=title,
                       source=source, evidence_url=evidence_url, observed=observed)
    db.flush()
    return person, created


def _record_employment(db: Session, ws: str, person: PersonEntity, *, company: str, domain: str,
                       title: str, source: str, evidence_url: str, observed: datetime):
    from apps.api.services.entities.graph import identity_domain

    domain = identity_domain(domain) if domain else ""
    company_entity_id = None
    if domain:
        row = db.query(CompanyIdentifier.entity_id).filter_by(
            workspace_id=ws, kind="domain", value=domain).first()
        company_entity_id = row[0] if row else None
    key = _company_key(domain, company)
    title = (title or "").strip()
    title_entry = {"title": title, "observed_at": observed.isoformat(), "source": source}

    employment = db.query(PersonEmployment).filter_by(
        workspace_id=ws, person_id=person.id, company_key=key).first()
    inserted = False
    if employment is None:
        try:
            with db.begin_nested():
                employment = PersonEmployment(
                    workspace_id=ws, person_id=person.id, company_key=key,
                    company_entity_id=company_entity_id, company_name=(company or "").strip(),
                    company_domain=domain, title=title, titles=[title_entry] if title else [],
                    source=source, evidence_url=evidence_url or "",
                    first_observed_at=observed, last_observed_at=observed, is_current=1)
                db.add(employment)
                db.flush()
            inserted = True
        except IntegrityError:  # a concurrent save recorded it first
            employment = db.query(PersonEmployment).filter_by(
                workspace_id=ws, person_id=person.id, company_key=key).one()
    if not inserted:
        employment.last_observed_at = max(employment.last_observed_at, observed)
        employment.first_observed_at = min(employment.first_observed_at, observed)
        if title and title != employment.title:
            employment.title = title
            employment.titles = [*(employment.titles or []), title_entry]
        if evidence_url:
            employment.evidence_url = evidence_url
        if company_entity_id and not employment.company_entity_id:
            employment.company_entity_id = company_entity_id

    # The most recently observed employment is current; older ones become history.
    latest = db.query(PersonEmployment).filter_by(workspace_id=ws, person_id=person.id).order_by(
        PersonEmployment.last_observed_at.desc(), PersonEmployment.id.desc()).all()
    for index, item in enumerate(latest):
        item.is_current = 1 if index == 0 else 0
    person.company_entity_id = latest[0].company_entity_id if latest else None


def person_profile(db: Session, person_id: str, workspace_id: str) -> Optional[dict]:
    """Person with identifiers and employment history (newest first)."""
    person = db.query(PersonEntity).filter_by(id=person_id, workspace_id=workspace_id).first()
    if person is None:
        return None
    identifiers = db.query(PersonIdentifier).filter_by(
        workspace_id=workspace_id, person_id=person_id).order_by(PersonIdentifier.id).all()
    jobs = db.query(PersonEmployment).filter_by(workspace_id=workspace_id, person_id=person_id).order_by(
        PersonEmployment.last_observed_at.desc(), PersonEmployment.id.desc()).all()
    return {
        "id": person.id,
        "full_name": person.full_name,
        "current_company_entity_id": person.company_entity_id,
        "identifiers": [{"kind": i.kind, "value": i.value} for i in identifiers],
        "sources": (person.fields or {}).get("sources") or [],
        "employments": [{
            "company_name": j.company_name, "company_domain": j.company_domain,
            "company_entity_id": j.company_entity_id, "title": j.title,
            "titles": j.titles or [], "is_current": bool(j.is_current), "source": j.source,
            "evidence_url": j.evidence_url,
            "first_observed_at": j.first_observed_at.isoformat() if j.first_observed_at else None,
            "last_observed_at": j.last_observed_at.isoformat() if j.last_observed_at else None,
        } for j in jobs],
    }
