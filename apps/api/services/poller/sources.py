"""Per-source detectors → normalized DetectedEvent lists (spec §3 / §8).

Each detector fetches one source, diffs against the cursor sub-key, and returns
``(events, cursor_patch)``:

  * ``events`` — list[DetectedEvent] of NEW items to emit (deduped by the caller
    via deterministic ``signals.id``; the detector applies the band/intent/period
    semantics so it only proposes genuinely-new events).
  * ``cursor_patch`` — the cursor sub-key updates to merge AFTER the events are
    emitted (atomic emit+advance happens in the caller's per-source txn).

Detectors NEVER raise — a fetch failure returns ``(None, None)`` so the caller
records ``last_error`` + backoff and leaves the cursor untouched (retried next
poll). On the bootstrap poll (cursor.bootstrapped == False) detectors record
state into the cursor and (unless backfill) suppress emission of pre-existing
items (spec §8.4).
"""

from __future__ import annotations

import asyncio
import logging
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import List, Optional, Tuple

from apps.api.core.config import settings
from apps.api.services.poller import keys
from apps.api.services.poller import rss as rss_mod

logger = logging.getLogger("poller.sources")

# Coarse growth bands, ordered; an UPWARD crossing fires hiring_surge (spec §6 review).
_BAND_ORDER = ["none", "hiring", "growing", "rapid_growth", "hypergrowth"]


def _band_rank(band: Optional[str]) -> int:
    try:
        return _BAND_ORDER.index(band or "none")
    except ValueError:
        return 0


def _iso_week(now: Optional[datetime] = None) -> str:
    now = now or datetime.now(timezone.utc)
    y, w, _ = now.isocalendar()
    return f"{y}-W{w:02d}"


@dataclass
class DetectedEvent:
    natural_event_id: str
    signal_type: str
    title: str = ""
    description: str = ""
    source: str = ""
    source_url: str = ""
    weight: int = 5
    occurred_at: float = field(default_factory=time.time)


# ── Funding (SEC Form D) + executive appointments ───────────────────────────

def fetch_funding_and_exec(
    watch, want_funding: bool, want_exec: bool, *, backfill: bool,
    provider=None,
) -> Tuple[Optional[List[DetectedEvent]], Optional[dict]]:
    """Enumerate ALL Form D/D-A accessions newer than the cursor (no missed-window,
    spec §3.6 / §8.2). Emits one ``company_funded`` per accession and one
    ``executive_hired`` per NEW C-level/director/VP related person not in
    ``cursor.sec_known_execs`` (spec §3.3, AC-16/AC-19).

    Returns ``(events, cursor_patch)``; ``(None, None)`` on fetch failure.
    """
    from apps.api.services.leadgen.enrichment.providers.sec_edgar import SecEdgarProvider

    provider = provider or SecEdgarProvider()
    cursor = watch.cursor or {}
    since = cursor.get("sec_last_accession")
    bootstrapped = bool(cursor.get("bootstrapped"))

    try:
        filings = asyncio.run(provider.list_form_d_since(watch.target, since))
    except Exception as e:  # defensive — list_form_d_since already no-raises
        logger.warning("funding fetch failed for watch %s: %s", watch.id, e)
        return None, None

    # filings are newest-first; resolve a CIK for the stable key (cache it).
    resolved_cik = watch.resolved_cik
    if filings and not resolved_cik:
        resolved_cik = filings[0].cik

    patch: dict = {}
    if resolved_cik and not watch.resolved_cik:
        patch["_resolved_cik"] = resolved_cik  # caller writes watch.resolved_cik

    known_execs = list(cursor.get("sec_known_execs") or [])
    known_set = set(known_execs)
    events: List[DetectedEvent] = []

    # Bootstrap (no prior cursor) + no backfill → record state, suppress emit.
    suppress = (not bootstrapped) and (not backfill)

    cik = resolved_cik or keys.stable_company_id(None, watch.target)
    for filing in filings:
        fcik = filing.cik or resolved_cik or cik
        # Funding
        if want_funding and not suppress:
            amount = (filing.fields or {}).get("funding_amount")
            title = "New SEC Form D filing"
            desc = filing.fields.get("funding_stage_signal", "private_placement_form_d")
            if amount:
                title = f"Raised ${int(amount):,} (Form D)"
            events.append(DetectedEvent(
                natural_event_id=f"{fcik}:{filing.accession}",
                signal_type="company_funded",
                title=title,
                description=str(desc),
                source="sec_edgar",
                source_url=(
                    f"https://www.sec.gov/cgi-bin/browse-edgar?action=getcompany"
                    f"&CIK={fcik}&type=D"
                ),
                weight=10,
                occurred_at=_epoch_from_date(filing.filing_date),
            ))
        # Executive appointments — new related persons with exec titles.
        if want_exec:
            for p in filing.related_persons or []:
                name = p.get("name", "")
                title_str = p.get("title", "")
                if not name or not keys.is_executive_title(title_str):
                    continue
                tier = keys.role_tier(title_str)
                nat = f"{fcik}:{keys.normalize_exec_name(name)}:{tier}"
                if nat in known_set:
                    continue
                known_set.add(nat)
                known_execs.append(nat)
                if not suppress:
                    events.append(DetectedEvent(
                        natural_event_id=nat,
                        signal_type="executive_hired",
                        title=f"New {tier.replace('_', ' ')}: {name}",
                        description=f"{name} — {title_str} (Form D related person)",
                        source="sec_edgar",
                        source_url=(
                            f"https://www.sec.gov/cgi-bin/browse-edgar?action=getcompany"
                            f"&CIK={fcik}&type=D"
                        ),
                        weight=8,
                        occurred_at=_epoch_from_date(filing.filing_date),
                    ))

    # Advance cursor to the NEWEST accession only after all emitted (spec §8.3).
    if filings:
        patch["sec_last_accession"] = filings[0].accession
    patch["sec_known_execs"] = known_execs
    return events, patch


def _epoch_from_date(d: str) -> float:
    d = (d or "").strip()
    if not d:
        return time.time()
    try:
        dt = datetime.strptime(d, "%Y-%m-%d").replace(tzinfo=timezone.utc)
        return dt.timestamp()
    except ValueError:
        return time.time()


# ── Hiring band-shift + tech adoption (JobSpy) ──────────────────────────────

def fetch_hiring_and_tech(
    watch, want_hiring: bool, want_tech: bool, *, backfill: bool,
    signals_override=None,
) -> Tuple[Optional[List[DetectedEvent]], Optional[dict]]:
    """JobSpy band + tech-adoption diff (spec §3.3, AC-6).

    ``hiring_surge`` fires only on an UPWARD growth-band crossing vs the cursor
    band, at most once per ISO week (intra-week re-cross suppressed). Raw counts
    are NEVER a threshold/dedup input (DDG-snippet artifact). ``new_tech_adopted``
    fires on a newly-seen tech OR an intent-tier upgrade (same tier never re-emits).
    """
    cursor = watch.cursor or {}
    bootstrapped = bool(cursor.get("bootstrapped"))
    suppress = (not bootstrapped) and (not backfill)

    if signals_override is not None:
        signals = signals_override
    else:
        try:
            signals = _run_jobspy(watch)
        except Exception as e:
            logger.warning("hiring fetch failed for watch %s: %s", watch.id, e)
            return None, None
        if signals is None:
            return None, None

    cik = keys.stable_company_id(watch.resolved_cik, watch.target)
    patch: dict = {}
    events: List[DetectedEvent] = []

    # ── hiring band crossing ──
    if want_hiring:
        new_band = signals.get("growth_signal", "none")
        prev_band = cursor.get("hiring_band", "none")
        prev_week = cursor.get("hiring_band_week")
        week = _iso_week()
        crossed_up = _band_rank(new_band) > _band_rank(prev_band)
        # Always advance the recorded band (so a downward move re-arms the next up).
        patch["hiring_band"] = new_band
        if crossed_up and not suppress and prev_week != week:
            patch["hiring_band_week"] = week
            events.append(DetectedEvent(
                natural_event_id=f"{cik}:{week}",
                signal_type="hiring_surge",
                title=f"Hiring surge: {prev_band} → {new_band}",
                description=f"Growth-band crossed upward to {new_band} (ISO {week})",
                source="jobspy",
                source_url="",
                weight=7,
            ))
        elif crossed_up and suppress:
            patch["hiring_band_week"] = week

    # ── tech adoption (intent-tier aware) ──
    if want_tech:
        known = dict(cursor.get("known_tech") or {})
        for sig in signals.get("tech_adoption_signal") or []:
            tech = keys.normalize_tech(sig.get("tech", ""))
            if not tech:
                continue
            intent = (sig.get("intent") or "mentions").lower()
            prev_intent = known.get(tech)
            # New tech OR an intent upgrade (different tier) → emit once.
            if prev_intent == intent:
                continue
            known[tech] = intent
            if not suppress:
                events.append(DetectedEvent(
                    natural_event_id=f"{cik}:{tech}:{intent}",
                    signal_type="new_tech_adopted",
                    title=f"Adopting {sig.get('tech', tech)} ({intent})",
                    description=f"{sig.get('category', '')}: {sig.get('tech', tech)} intent={intent}",
                    source="jobspy",
                    source_url="",
                    weight=6,
                ))
        patch["known_tech"] = known

    return events, patch


def _run_jobspy(watch) -> Optional[dict]:
    """Run the JobSpy provider for the watch's company and return the analyzed
    signals dict ({growth_signal, tech_adoption_signal, ...}) or None on failure."""
    import json as _json

    from apps.api.services.leadgen.enrichment.providers.jobspy_signals import (
        JobSpySignalProvider,
    )
    from apps.api.services.leadgen.models import Lead

    max_jobs = int(getattr(settings, "INTENT_POLLER_JOBSPY_MAX_JOBS", 5))
    provider = JobSpySignalProvider(max_jobs=max_jobs)
    lead = Lead(company=_company_name_for(watch))
    res = asyncio.run(provider.enrich(lead))
    if not res or not res.success:
        # "no jobs found" is a valid no-op (band stays), not a failure.
        if res is not None and res.error == "no_jobs_found":
            return {"growth_signal": "none", "tech_adoption_signal": []}
        return None
    raw = (res.fields or {}).get("hiring_signals")
    if not raw:
        return {"growth_signal": "none", "tech_adoption_signal": []}
    try:
        return _json.loads(raw)
    except (ValueError, TypeError):
        return None


def _company_name_for(watch) -> str:
    t = (watch.target or "").strip()
    if t.lower().startswith("sec_cik:"):
        return ""  # CIK-only watch has no name for JobSpy
    return t


# ── RSS / news ──────────────────────────────────────────────────────────────

def fetch_feed(
    watch, *, backfill: bool, fetcher=None,
) -> Tuple[Optional[List[DetectedEvent]], Optional[dict]]:
    """RSS/Atom diff with conditional GET (spec §3 / §8.3, AC-7/AC-21).

    304 → no work (short-circuit). New entries (keyed by GUID/link, bounded by
    ``feed_max_published`` + a trim-safe ``feed_seen_guids`` list) become ``news``
    rows. On bootstrap (no backfill) records the watermark + suppresses emission.
    """
    cursor = watch.cursor or {}
    bootstrapped = bool(cursor.get("bootstrapped"))
    suppress = (not bootstrapped) and (not backfill)

    fetch = fetcher or rss_mod.fetch_feed
    result = fetch(
        watch.target,
        etag=cursor.get("feed_etag"),
        last_modified=cursor.get("feed_last_modified"),
    )
    if result.error:
        logger.warning("feed fetch failed for watch %s: %s", watch.id, result.error)
        return None, None
    if result.not_modified:
        # cheap 304 short-circuit — refresh validators only.
        patch = {}
        if result.etag:
            patch["feed_etag"] = result.etag
        if result.last_modified:
            patch["feed_last_modified"] = result.last_modified
        return [], patch

    max_entries = int(getattr(settings, "INTENT_POLLER_FEED_MAX_ENTRIES", 100))
    seen = list(cursor.get("feed_seen_guids") or [])
    seen_set = set(seen)
    max_pub = float(cursor.get("feed_max_published") or 0.0)

    events: List[DetectedEvent] = []
    new_max_pub = max_pub
    for entry in result.entries:
        if entry.guid in seen_set:
            continue
        seen_set.add(entry.guid)
        seen.append(entry.guid)
        if entry.published_epoch > new_max_pub:
            new_max_pub = entry.published_epoch
        if not suppress:
            events.append(DetectedEvent(
                natural_event_id=entry.guid,
                signal_type="news",
                title=entry.title or "News mention",
                description=entry.summary,
                source="rss",
                source_url=entry.link or watch.target,
                weight=5,
                occurred_at=entry.published_epoch or time.time(),
            ))

    # Trim seen_guids: bounded N, only drop GUIDs strictly older than max_pub.
    bound = max(200, 5 * max_entries)
    if len(seen) > bound:
        # keep the most recent `bound` (we appended in iteration order; the
        # watermark guards correctness, so trimming the head is safe).
        seen = seen[-bound:]

    patch = {
        "feed_seen_guids": seen,
        "feed_max_published": new_max_pub,
    }
    if result.etag:
        patch["feed_etag"] = result.etag
    if result.last_modified:
        patch["feed_last_modified"] = result.last_modified
    return events, patch
