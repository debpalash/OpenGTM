"""
People-search workbook source — materialize PERSON rows into a workbook.

A `source` column with ``kind: "people_search"`` inverts the usual company
sourcing: instead of finding companies for an ICP, it finds PEOPLE at target
companies (Clay's "find people at these companies" job) and appends one row
per person. Discovery is keyless — CrossLinked-style
``site:linkedin.com/in "<company>" "<title>"`` DDG searches via
``CrossLinkedProvider.find_people_by_titles`` — so it inherits the
SearchCache/HostBackoff seam that wraps the DDGS client.

Config (stored flat on the source column, like icp/channels/target_rows):

    {
      "kind": "people_search",
      "companies": ["Acme Corp", "stripe.com"],   # names or domains
      "from_column": "company",                    # OR: read companies off rows
      "titles": ["CTO", "Founder"],
      "seniority": "senior",                       # optional extra quoted term
      "geo": "Berlin",                             # optional extra quoted term
      "max_per_company": 10,                       # capped at 25
      "max_searches": 100                          # total DDG queries per run
    }

Rows carry BOTH the lead-field vocabulary the enrichment stack keys on
(contact_person / contact_title / company / website / linkedin_url / source —
the email waterfall takes contact_person + website domain) AND explicit person
fields (full_name / first_name / last_name / title / confidence).

Dedup uses the source engine's identity mechanism: a stable
``person:<sha1>`` id in ``WorkbookRow.canonical_entity_id`` keyed on
linkedin_url (fallback name+company), so refresh re-runs append only
genuinely-new people. Rows are workspace-scoped like all source rows.

Gated by ``settings.PEOPLE_SEARCH_SOURCE_ENABLED`` (default OFF): disabled →
graceful error result, ZERO network calls.
"""

import hashlib
import logging
import re
from typing import Any, Dict, List, Optional, Tuple

from apps.api.database import SessionLocal
from apps.api.services.workbook.models import Workbook, WorkbookRow
from apps.api.services.workbook.enrichment import _make_redis, _broadcast

logger = logging.getLogger("workbook.people_search")

DEFAULT_MAX_PER_COMPANY = 10
MAX_PER_COMPANY_CAP = 25
DEFAULT_MAX_SEARCHES = 100
# Hard ceiling on total DDG queries a single run may issue, whatever the config
# says (politeness bound on the keyless search seam).
MAX_SEARCHES_CAP = 100

PERSON_SOURCE = "people_search"
PERSON_CONFIDENCE = 0.6  # CrossLinked snippet-parse confidence

# "stripe.com" / "https://www.stripe.com/" → treat the company entry as a domain
_DOMAIN_RE = re.compile(
    r"^(?:https?://)?(?:www\.)?([a-z0-9-]+(?:\.[a-z0-9-]+)+)/?$", re.IGNORECASE
)


# ── Config validation ─────────────────────────────────────────────────────

def validate_people_search_config(col: dict) -> Dict[str, Any]:
    """Validate + normalize a people_search source config. Raises ValueError.

    Mirrors how icp source configs are consumed (flat keys on the column dict);
    the API route calls this at column-create time and the engine re-validates
    at run time (configs are mutable JSON).
    """
    cfg = col or {}
    companies = [
        str(c).strip() for c in (cfg.get("companies") or [])
        if c is not None and str(c).strip()
    ]
    from_column = str(cfg.get("from_column") or "").strip()
    if not companies and not from_column:
        raise ValueError("people_search requires 'companies' or 'from_column'")

    titles = [
        str(t).strip() for t in (cfg.get("titles") or [])
        if t is not None and str(t).strip()
    ]
    seniority = str(cfg.get("seniority") or "").strip()
    geo = str(cfg.get("geo") or "").strip()

    def _int_cfg(key: str, default: int, cap: int) -> int:
        val = cfg.get(key)
        if val is None or val == "":
            return default
        try:
            val = int(val)
        except (TypeError, ValueError):
            raise ValueError(f"{key} must be an integer")
        return max(1, min(val, cap))

    max_per_company = _int_cfg("max_per_company", DEFAULT_MAX_PER_COMPANY, MAX_PER_COMPANY_CAP)
    max_searches = _int_cfg("max_searches", DEFAULT_MAX_SEARCHES, MAX_SEARCHES_CAP)

    return {
        "companies": companies,
        "from_column": from_column,
        "titles": titles,
        "seniority": seniority,
        "geo": geo,
        "max_per_company": max_per_company,
        "max_searches": max_searches,
    }


# ── Identity (source-engine dedup mechanism) ──────────────────────────────

def person_identity(name: str, company: str, linkedin_url: str = "") -> str:
    """Stable dedup id stored in WorkbookRow.canonical_entity_id.

    Keyed on linkedin_url when present (canonical for a person), else
    (name, company). Prefixed `person:` so it can never collide with company
    entity-graph ids.
    """
    if (linkedin_url or "").strip():
        basis = "li:" + linkedin_url.strip().lower().rstrip("/")
    else:
        basis = "nc:" + (name or "").strip().lower() + "|" + (company or "").strip().lower()
    return "person:" + hashlib.sha1(basis.encode("utf-8")).hexdigest()[:24]


def _split_name(full_name: str) -> Tuple[str, str]:
    """(first, last) preserving casing — display fields, not email patterns."""
    parts = (full_name or "").strip().split()
    if not parts:
        return "", ""
    if len(parts) == 1:
        return parts[0], ""
    return parts[0], parts[-1]


def _company_and_website(entry: str) -> Tuple[str, str]:
    """A companies[] entry may be a name or a domain. Domains also yield a
    website URL so downstream email finders (contact_person + website domain)
    work without an extra enrichment hop."""
    m = _DOMAIN_RE.match(entry.strip())
    if m:
        domain = m.group(1).lower()
        return entry.strip(), f"https://{domain}"
    return entry.strip(), ""


def _row_data(company_entry: str, person: Dict[str, str]) -> Dict[str, Any]:
    """Snapshot a discovered person into workbook-row data.

    Uses the lead-field vocabulary (contact_person/contact_title/company/
    website/linkedin_url/source) so lead_field columns and the email waterfall
    line up, plus explicit person fields (full_name/first_name/last_name/
    title/confidence)."""
    name = (person.get("name") or "").strip()
    title = (person.get("title") or "").strip()
    if title.upper() == "N/A":
        title = ""
    first, last = _split_name(name)
    company, website = _company_and_website(company_entry)
    data: Dict[str, Any] = {
        "company": company,
        "contact_person": name,
        "full_name": name,
        "first_name": first,
        "last_name": last,
        "source": PERSON_SOURCE,
        "confidence": PERSON_CONFIDENCE,
    }
    if title:
        data["contact_title"] = title
        data["title"] = title
    if website:
        data["website"] = website
    li = (person.get("linkedin") or "").strip()
    if li:
        data["linkedin_url"] = li
    return data


# ── from_column: read target companies off existing workbook rows ─────────

def _resolve_data_key(columns_config: List[dict], from_column: str) -> str:
    """Map a from_column reference (column id, name, or lead field) to the
    row-data key. lead_field columns store data under their lead_field name."""
    for c in columns_config or []:
        if c.get("id") == from_column or c.get("name") == from_column:
            return c.get("lead_field") or c.get("id") or from_column
    return from_column


def _companies_from_rows(db, workbook_id: str, columns_config: List[dict], from_column: str) -> List[str]:
    key = _resolve_data_key(columns_config, from_column)
    rows = (
        db.query(WorkbookRow)
        .filter(WorkbookRow.workbook_id == workbook_id)
        .order_by(WorkbookRow.position.asc())
        .all()
    )
    out: List[str] = []
    seen = set()
    for row in rows:
        val = str((row.data or {}).get(key) or "").strip()
        if val and val.lower() not in seen:
            seen.add(val.lower())
            out.append(val)
    return out


# ── Preview (no write, no searching) ──────────────────────────────────────

def preview_people_search(col: dict) -> Dict[str, Any]:
    """Dry-run: how many companies/queries a run would hit. No network."""
    try:
        cfg = validate_people_search_config(col)
    except ValueError as e:
        return {"kind": PERSON_SOURCE, "error": str(e)}
    n_companies = len(cfg["companies"])
    per_company = max(1, len(cfg["titles"]))
    estimated = min(n_companies * per_company, cfg["max_searches"]) if n_companies else None
    return {
        "kind": PERSON_SOURCE,
        "companies": n_companies or None,
        "from_column": cfg["from_column"] or None,
        "titles": cfg["titles"],
        "max_per_company": cfg["max_per_company"],
        "max_searches": cfg["max_searches"],
        "estimated_searches": estimated,
        "sources": [{
            "name": "crosslinked_ddg",
            "label": "LinkedIn profiles via DuckDuckGo (CrossLinked)",
            "category": "people",
            "region": None,
        }],
        "source_count": 1,
    }


# ── Materialization ───────────────────────────────────────────────────────

def _set_status(workbook_id: str, status: str):
    with SessionLocal() as db:
        wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
        if wb:
            wb.status = status
            db.commit()


async def materialize_people_search(
    workbook_id: str, column_id: str, workspace_id: str, col: dict
) -> Dict[str, Any]:
    """Run a people_search source column: discover people, append person rows.

    Called from source_engine._materialize_source_impl, which has already
    entered workspace_scope(workspace_id) and set wb.status = "running"; this
    function owns resetting status to "draft" on every exit path.
    """
    from apps.api.core.config import settings

    if not settings.PEOPLE_SEARCH_SOURCE_ENABLED:
        _set_status(workbook_id, "draft")
        return {"error": "people_search_disabled", "found": 0, "added": 0}

    try:
        cfg = validate_people_search_config(col)
    except ValueError as e:
        _set_status(workbook_id, "draft")
        return {"error": str(e), "found": 0, "added": 0}

    # Resolve target companies (static list, or read off existing rows).
    companies = cfg["companies"]
    if not companies and cfg["from_column"]:
        with SessionLocal() as db:
            wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
            columns_config = (wb.columns_config or []) if wb else []
            companies = _companies_from_rows(db, workbook_id, columns_config, cfg["from_column"])
    if not companies:
        _set_status(workbook_id, "draft")
        return {"error": "no_companies", "found": 0, "added": 0}

    # ── Discovery (keyless, budgeted) ──
    from apps.api.services.leadgen.enrichment.providers.crosslinked import CrossLinkedProvider

    provider = CrossLinkedProvider(max_people=cfg["max_per_company"])
    budget = cfg["max_searches"]
    searches_used = 0
    discovered: List[Tuple[str, Dict[str, str]]] = []  # (company_entry, person)
    for company in companies:
        if searches_used >= budget:
            logger.info(
                f"people_search {workbook_id}/{column_id}: search budget "
                f"({budget}) exhausted at company {company!r}"
            )
            break
        try:
            people, used = await provider.find_people_by_titles(
                company_name=company,
                titles=cfg["titles"],
                geo=cfg["geo"],
                seniority=cfg["seniority"],
                max_people=cfg["max_per_company"],
                max_searches=budget - searches_used,
            )
        except Exception as e:
            logger.warning(f"people_search discovery failed for {company!r}: {e}")
            searches_used += 1  # count the attempt against the budget
            continue
        searches_used += used
        for p in people:
            discovered.append((company, p))

    found = len(discovered)
    redis_client = _make_redis()
    added = skipped = 0
    _added_row_ids: list = []
    try:
        with SessionLocal() as db:
            # Identities already represented in THIS workbook (cross-run
            # idempotency — same mechanism as the company source path).
            present = {
                eid for (eid,) in db.query(WorkbookRow.canonical_entity_id)
                .filter(
                    WorkbookRow.workbook_id == workbook_id,
                    WorkbookRow.canonical_entity_id.isnot(None),
                ).all()
            }
            max_pos = (
                db.query(WorkbookRow.position)
                .filter(WorkbookRow.workbook_id == workbook_id)
                .order_by(WorkbookRow.position.desc())
                .limit(1)
                .scalar()
            ) or 0

            for company, person in discovered:
                name = (person.get("name") or "").strip()
                if not name:
                    skipped += 1
                    continue
                identity = person_identity(name, company, person.get("linkedin") or "")
                if identity in present:
                    skipped += 1
                    continue
                present.add(identity)

                row_data = _row_data(company, person)
                max_pos += 1
                row = WorkbookRow(
                    workbook_id=workbook_id,
                    workspace_id=workspace_id,
                    position=max_pos,
                    data=row_data,
                    enrichments={},
                    canonical_entity_id=identity,
                    corroboration_count=1,
                )
                db.add(row)
                db.flush()  # get row.id
                added += 1
                _added_row_ids.append(row.id)

                if redis_client is not None:
                    await _broadcast(redis_client, workbook_id, {
                        "type": "row_added",
                        "rowId": row.id,
                        "data": row_data,
                        "entityId": identity,
                        "corroboration": 1,
                    })

            db.commit()

            wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
            if wb:
                total = db.query(WorkbookRow).filter(
                    WorkbookRow.workbook_id == workbook_id
                ).count()
                wb.total_rows = total
                wb.status = "draft"  # sourcing done; ready to enrich
                db.commit()

        # Automations: on_row_added (same event the company source path emits).
        if _added_row_ids and workspace_id:
            try:
                from apps.api.services.automations import events as _auto_events
                _auto_events.emit_row_added(workspace_id, workbook_id, _added_row_ids)
            except Exception as _e:
                logger.warning("on_row_added emit (people_search) failed: %s", _e)
    finally:
        if redis_client is not None:
            try:
                await _broadcast(redis_client, workbook_id, {
                    "type": "source_done",
                    "columnId": column_id,
                    "found": found, "added": added, "skipped": skipped,
                })
            finally:
                try:
                    await redis_client.aclose()
                except Exception:
                    pass

    logger.info(
        f"people_search {workbook_id}/{column_id}: companies={len(companies)} "
        f"searches={searches_used} found={found} added={added} skipped={skipped}"
    )
    return {
        "found": found, "added": added, "skipped": skipped,
        "companies": len(companies), "searches_used": searches_used,
    }
