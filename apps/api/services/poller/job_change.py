"""Person-level job-change detection for the ``job_change`` watch kind.

Tracks saved contacts (lead ids and/or explicit {name, company, linkedin_url}
entries in ``watch.config["contacts"]``) and detects when a person changes
companies. Detection is a conservative, keyless DDG lookup (person_intel-style):

    "<name>" <linkedin-handle-or-company> site:linkedin.com/in

The current employer is parsed ONLY from high-confidence result-title/snippet
patterns ("Name - Title - Company | LinkedIn", "Title at Company",
"Experience: Company ·"). Per-contact state lives in the watch cursor
(``cursor["job_change_contacts"]``): ``last_known_company`` + ``last_checked_at``.

Emission rules (spec'd conservative — this is Clay's flagship signal, false
positives are worse than misses):

  * NEVER emit on the first observation of a contact — baseline-set only.
  * Emit only when the previous company is known AND a new company was parsed
    with confidence AND the NORMALIZED names differ (lowercase, legal suffixes
    Inc/LLC/Ltd/Pvt/… stripped).
  * Low-confidence / failed parse → skip, keep previous state (retried next
    cycle first via least-recently-checked ordering).

Rate limiting: the DDG seam is ``proxy_client.get_ddgs`` (search cache + per-host
backoff built in); at most ``INTENT_POLLER_JOB_CHANGE_MAX_CONTACTS_PER_POLL``
contacts are checked per poll cycle (per-watch override
``config["max_contacts_per_poll"]`` can only lower it). Interval is enforced
weekly-minimum at the API layer (scraping-based check, keep volume low).
"""

from __future__ import annotations

import json
import logging
import re
import time
from typing import List, Optional, Tuple

from apps.api.core.config import settings
from apps.api.services.poller.sources import DetectedEvent

logger = logging.getLogger("poller.job_change")

JOB_CHANGE_WEIGHT = 9

# Cursor sub-key holding per-contact state (state storage reuses the watch
# cursor — no new table).
STATE_KEY = "job_change_contacts"

# ── company normalization (lowercase + strip legal suffixes) ─────────────────

# Legal suffixes stripped before comparison. Superset of sec_edgar's set with
# international forms (Pvt/Pte/GmbH/…). Token-bounded so "Cointreau" survives.
_SUFFIX_RE = re.compile(
    r"\b(inc|incorporated|corp|corporation|llc|l\.l\.c|llp|lp|ltd|limited|"
    r"co|company|plc|pvt|private|pte|pty|gmbh|ag|sa|sarl|srl|bv|nv|oy|ab|kk|"
    r"holdings|group|the)\b",
    re.IGNORECASE,
)
_NON_ALNUM_RE = re.compile(r"[^a-z0-9 ]+")


def normalize_company(name: str) -> str:
    """Lowercase, strip punctuation + legal suffixes, collapse whitespace."""
    s = (name or "").lower()
    s = _NON_ALNUM_RE.sub(" ", s)
    s = _SUFFIX_RE.sub(" ", s)
    return re.sub(r"\s+", " ", s).strip()


def _norm_person(name: str) -> str:
    s = (name or "").lower()
    s = re.sub(r"[^a-z0-9 ]+", " ", s)
    return re.sub(r"\s+", " ", s).strip()


# ── contact identity ─────────────────────────────────────────────────────────

_LI_HANDLE_RE = re.compile(r"linkedin\.com/in/([A-Za-z0-9_.%\-]+)", re.IGNORECASE)


def linkedin_handle(url: str) -> str:
    m = _LI_HANDLE_RE.search((url or "").strip())
    return m.group(1).lower().rstrip("/") if m else ""


def contact_key(contact: dict) -> str:
    """Stable per-contact state key: lead id > linkedin handle > name|company."""
    if contact.get("lead_id"):
        return f"lead:{contact['lead_id']}"
    handle = linkedin_handle(contact.get("linkedin_url", ""))
    if handle:
        return f"li:{handle}"
    return f"nm:{_norm_person(contact.get('name', ''))}|{normalize_company(contact.get('company', ''))}"


# ── conservative current-employer resolution (DDG snippets) ──────────────────

_LI_TAIL_RE = re.compile(r"\s*[|\-–—]\s*LinkedIn\s*$", re.IGNORECASE)
_TITLE_SPLIT_RE = re.compile(r"\s+[-–—|]\s+")
_AT_COMPANY_RE = re.compile(r"^(?:.+?)\s+(?:at|@)\s+(.+)$", re.IGNORECASE)
_EXPERIENCE_RE = re.compile(r"\bexperience:\s*([^·|•]+)", re.IGNORECASE)

# Never accept these as an employer (self-descriptions, junk).
_COMPANY_STOPWORDS = {
    "linkedin", "self employed", "self-employed", "freelance", "freelancer",
    "open to work", "unemployed", "student", "retired",
}


def _default_search(query: str, max_results: int = 8) -> List[dict]:
    """Keyless DDG search through the resilient seam (cache + backoff built in)."""
    from apps.api.services.leadgen.proxy_client import get_ddgs

    with get_ddgs() as ddgs:
        return list(ddgs.text(query, max_results=max_results) or [])


def build_query(name: str, linkedin_url: str = "", company: str = "") -> str:
    """`"<name>" <linkedin-handle-or-company> site:linkedin.com/in`."""
    handle = linkedin_handle(linkedin_url)
    anchor = handle or (company or "").strip()
    parts = [f'"{(name or "").strip()}"']
    if anchor:
        parts.append(anchor)
    parts.append("site:linkedin.com/in")
    return " ".join(parts)


def _plausible_company(candidate: str, person_name: str) -> bool:
    c = (candidate or "").strip()
    if not (2 <= len(c) <= 80):
        return False
    norm = normalize_company(c)
    if not norm:
        return False
    if c.lower() in _COMPANY_STOPWORDS or norm in _COMPANY_STOPWORDS:
        return False
    if "linkedin" in norm:
        return False
    if norm == _norm_person(person_name):
        return False
    return True


def _company_from_result(title: str, snippet: str, name: str) -> Tuple[Optional[str], bool]:
    """Parse (company, name_matched) from one linkedin.com/in SERP row.

    Confident patterns ONLY:
      1. "Name - Title - Company" (>=3 title segments) → last segment.
      2. "Name - Title at Company" (2 segments)        → after " at ".
      3. snippet "Experience: Company ·"               → first chunk.
    Anything else → (None, name_matched).
    """
    cleaned = _LI_TAIL_RE.sub("", (title or "").strip())
    segments = [s.strip() for s in _TITLE_SPLIT_RE.split(cleaned) if s.strip()]

    name_matched = False
    if segments:
        # First segment is the profile name, possibly with ", MBA" credentials.
        first = segments[0].split(",")[0]
        name_matched = bool(name) and _norm_person(first) == _norm_person(name)

    candidate: Optional[str] = None
    if len(segments) >= 3:
        candidate = segments[-1]
    elif len(segments) == 2:
        m = _AT_COMPANY_RE.match(segments[1])
        if m:
            candidate = m.group(1)
    if candidate is None and snippet:
        m = _EXPERIENCE_RE.search(snippet)
        if m:
            candidate = m.group(1)

    if candidate is not None:
        candidate = candidate.strip().strip("·•|").strip()
        if not _plausible_company(candidate, name):
            candidate = None
    return candidate, name_matched


def resolve_current_company(
    name: str,
    linkedin_url: str = "",
    company_hint: str = "",
    searcher=None,
) -> Tuple[Optional[str], Optional[str]]:
    """Resolve the person's CURRENT employer, conservatively.

    Returns ``(company, evidence_url)`` or ``(None, None)`` when nothing was
    parsed with confidence. Identity gate: when a linkedin handle is known the
    result URL's handle MUST match; otherwise the result-title name must equal
    the tracked name (normalized). Results that fail the gate are skipped.

    Search-transport errors PROPAGATE (the caller counts them as fetch
    failures so the contact is retried next cycle without a state bump).
    """
    if not (name or "").strip() and not linkedin_handle(linkedin_url):
        return None, None
    search = searcher or _default_search
    query = build_query(name, linkedin_url, company_hint)
    results = search(query)

    want_handle = linkedin_handle(linkedin_url)
    for r in results or []:
        url = r.get("href") or r.get("url") or ""
        got_handle = linkedin_handle(url)
        if not got_handle:
            continue  # only linkedin.com/in results count as evidence
        if want_handle and got_handle != want_handle:
            continue  # wrong person's profile
        title = r.get("title") or ""
        snippet = r.get("body") or r.get("snippet") or ""
        candidate, name_matched = _company_from_result(title, snippet, name)
        if candidate is None:
            continue
        # Identity: exact handle match OR exact (normalized) name match.
        if not (want_handle and got_handle == want_handle) and not name_matched:
            continue
        return candidate, url.split("?")[0].rstrip("/")
    return None, None


# ── contact materialization (workspace-scoped lead lookup) ───────────────────

def materialize_contacts(db, watch) -> List[dict]:
    """Expand ``watch.config["contacts"]`` into concrete contacts.

    ``lead_id`` entries are resolved from the WORKSPACE-SCOPED lead rows
    (contact_person / company / linkedin_url); entries without a usable person
    name are skipped. Explicit entries pass through. Each contact carries its
    stable ``key``.
    """
    from apps.api.services.leadgen.orm_models import LeadRow

    cfg = watch.config or {}
    out: List[dict] = []
    for entry in cfg.get("contacts") or []:
        if not isinstance(entry, dict):
            continue
        lead_id = entry.get("lead_id")
        if lead_id:
            row = (
                db.query(LeadRow.id, LeadRow.contact_person, LeadRow.company,
                         LeadRow.linkedin_url)
                .filter(LeadRow.id == int(lead_id),
                        LeadRow.workspace_id == watch.workspace_id)
                .first()
            )
            if row is None:
                continue
            name = entry.get("name") or row[1] or ""
            li = entry.get("linkedin_url") or ""
            if not linkedin_handle(li):
                li = row[3] if linkedin_handle(row[3] or "") else ""
            contact = {
                "lead_id": int(lead_id),
                "name": name.strip(),
                "company": (entry.get("company") or row[2] or "").strip(),
                "linkedin_url": li,
            }
        else:
            contact = {
                "lead_id": None,
                "name": (entry.get("name") or "").strip(),
                "company": (entry.get("company") or "").strip(),
                "linkedin_url": (entry.get("linkedin_url") or "").strip(),
            }
        if not contact["name"] and not linkedin_handle(contact["linkedin_url"]):
            continue  # nothing to search on
        contact["key"] = contact_key(contact)
        out.append(contact)
    return out


# ── the detector (sources.py contract: (events, cursor_patch)) ───────────────

def per_poll_cap(watch) -> int:
    cap = int(getattr(settings, "INTENT_POLLER_JOB_CHANGE_MAX_CONTACTS_PER_POLL", 50))
    cfg = watch.config or {}
    override = cfg.get("max_contacts_per_poll")
    if override:
        try:
            cap = max(1, min(cap, int(override)))
        except (TypeError, ValueError):
            pass
    return max(1, cap)


def fetch_job_changes(
    watch, contacts: List[dict], *, backfill: bool = False,
    searcher=None, now: Optional[float] = None,
) -> Tuple[Optional[List[DetectedEvent]], Optional[dict]]:
    """Check up to ``per_poll_cap`` tracked contacts (least-recently-checked
    first) and emit ``job_change`` events on confident employer changes.

    Follows the sources.py contract: ``(events, cursor_patch)`` on success,
    ``(None, None)`` when EVERY attempted lookup errored (cursor untouched →
    retried next poll). Baseline-only on first observation, never emits.
    ``backfill`` is intentionally ignored — a first observation can never
    prove a *change*, so there is nothing safe to backfill.
    """
    ts = now if now is not None else time.time()
    cursor = watch.cursor or {}
    state = dict(cursor.get(STATE_KEY) or {})

    # Least-recently-checked first so the cap round-robins the roster.
    ordered = sorted(
        contacts,
        key=lambda c: float((state.get(c["key"]) or {}).get("last_checked_at") or 0.0),
    )
    batch = ordered[: per_poll_cap(watch)]

    events: List[DetectedEvent] = []
    attempted = 0
    errored = 0
    for contact in batch:
        key = contact["key"]
        attempted += 1
        try:
            resolved, evidence_url = resolve_current_company(
                contact.get("name", ""),
                contact.get("linkedin_url", ""),
                contact.get("company", ""),
                searcher=searcher,
            )
        except Exception as e:  # noqa: BLE001 — transport failure → retry later
            logger.warning("job_change lookup failed for %s: %s", key, e)
            errored += 1
            continue  # no state bump → stays first in LRU order next cycle

        st = dict(state.get(key) or {})
        prev = (st.get("last_known_company") or "").strip()

        if not state.get(key):
            # First observation → baseline-set ONLY (never emit). Prefer the
            # resolved employer; fall back to the configured company so a later
            # confident resolve can still compare against something.
            st = {
                "last_known_company": resolved or contact.get("company", "") or "",
                "last_checked_at": ts,
            }
            state[key] = st
            continue

        st["last_checked_at"] = ts
        if resolved is None:
            state[key] = st  # low confidence → keep previous state
            continue
        if not prev:
            st["last_known_company"] = resolved  # late baseline, no emit
            state[key] = st
            continue

        old_n, new_n = normalize_company(prev), normalize_company(resolved)
        if old_n and new_n and old_n != new_n:
            payload = {
                "lead_id": contact.get("lead_id"),
                "person_name": contact.get("name", ""),
                "old_company": prev,
                "new_company": resolved,
                "evidence_url": evidence_url or "",
            }
            events.append(DetectedEvent(
                natural_event_id=f"{key}:{old_n}->{new_n}",
                signal_type="job_change",
                title=f"Job change: {contact.get('name', key)} — {prev} → {resolved}",
                description=json.dumps(payload),
                source="ddg_linkedin",
                source_url=evidence_url or "",
                weight=JOB_CHANGE_WEIGHT,
                lead_id=contact.get("lead_id"),
                company=resolved,
            ))
            st["last_known_company"] = resolved
        state[key] = st

    if attempted and errored == attempted:
        return None, None  # total failure → no cursor advance, retry next poll
    return events, {STATE_KEY: state}
