"""SEC EDGAR provider — keyless US funding/intent + decision-maker signals.

The U.S. Securities and Exchange Commission publishes a free, keyless, legally
mandated dataset that Clay/Apollo resell as premium "funding" and "intent" data.
This provider taps it directly:

  * Form D ("Notice of Exempt Offering of Securities") — what a startup files
    when it raises private capital. It exposes the OFFERING AMOUNT, the date of
    first sale, the industry group, and the RELATED PERSONS (executive officers,
    directors, promoters) — i.e. real decision-makers, by name, with title.
  * Company submissions / metadata — the issuer's CIK, canonical name, SIC
    industry description, and incorporation state.

Resolution pipeline (works from company NAME or DOMAIN-derived name):
  1. Exact CIK lookup against the keyless company_tickers.json (public filers).
  2. Fallback: EDGAR full-text search (efts.sec.gov) restricted to Form D —
     catches the private/VC-backed companies that have no ticker but DID file a
     Form D when they raised. This is the high-value path for sales intent.

Then we pull the most recent Form D's primary_doc.xml and map it to fields.

Emits (all flat scalars per the EnrichmentResult data contract, except the
designated JSON-string field `decision_makers`):
  sec_cik, funding_amount, funding_date, funding_stage_signal, industry,
  decision_makers (JSON string), and (when found) description / founded fallbacks.

Skeptic constraints (mandatory, per SEC's fair-access policy):
  * Every request carries an identifying `User-Agent: OpenGTM <email>` header. SEC blocks
    requests without a descriptive UA. Configure via SEC_EDGAR_USER_AGENT.
  * Self-rate-limited to <= ~8 req/s (SEC's documented ceiling is 10 req/s) via
    a process-wide async throttle, so concurrent enrichments can't exceed it.
  * 404 / no-match / parse errors return an empty (success=False) result and
    NEVER raise — enrichment must degrade gracefully.

Refs:
  https://www.sec.gov/search-filings/edgar-application-programming-interfaces
  https://www.sec.gov/info/edgar/specifications/form-d-xml-tech-specs-10.htm
"""
import asyncio
import json
import logging
import os
import re
import time as _time
from dataclasses import dataclass, field
from typing import Dict, List, Optional, Tuple
from xml.etree import ElementTree as ET

import httpx

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("enrichment.sec_edgar")

# Keyless endpoints (no API key, ever — these are public filings).
_TICKERS_URL = "https://www.sec.gov/files/company_tickers.json"
_FTS_URL = "https://efts.sec.gov/LATEST/search-index"
_SUBMISSIONS_URL = "https://data.sec.gov/submissions/CIK{cik}.json"
_ARCHIVES_BASE = "https://www.sec.gov/Archives/edgar/data/{cik}/{acc}/primary_doc.xml"

# SEC requires a descriptive User-Agent with contact info, or it 403s. Make it
# configurable (ops should set their own contact) but ship a safe default.
_UA = os.getenv("SEC_EDGAR_USER_AGENT", "OpenGTM Enrichment admin@yupcha.com")
_HEADERS = {"User-Agent": _UA, "Accept-Encoding": "gzip, deflate"}

_HTTP_TIMEOUT = httpx.Timeout(8.0, connect=5.0)

# Legal-suffix / punctuation noise stripped before name comparison.
_SUFFIX_RE = re.compile(
    r"\b(inc|incorporated|corp|corporation|llc|l\.l\.c|llp|ltd|limited|co|company|"
    r"plc|lp|holdings|group|the)\b",
    re.IGNORECASE,
)
_NON_ALNUM_RE = re.compile(r"[^a-z0-9 ]+")


# ── Self rate-limiter (process-wide, <= ~8 req/s) ──────────────────────────

class _RateLimiter:
    """Async token-spacer enforcing a minimum interval between requests across
    ALL concurrent enrichments in this process. SEC's ceiling is 10 req/s; we
    target 8 req/s (125 ms spacing) to keep a safety margin."""

    def __init__(self, max_per_sec: float = 8.0):
        self._min_interval = 1.0 / max_per_sec
        self._lock = asyncio.Lock()
        self._last = 0.0

    async def acquire(self) -> None:
        async with self._lock:
            now = _time.monotonic()
            wait = self._min_interval - (now - self._last)
            if wait > 0:
                await asyncio.sleep(wait)
            self._last = _time.monotonic()


# Module-level singleton so the limit is global, not per-instance.
_LIMITER = _RateLimiter(max_per_sec=8.0)


@dataclass
class FormDFiling:
    """One Form D / D-A filing, returned by :meth:`list_form_d_since`.

    ``accession`` is the dashless accession number (monotonic by SEC issuance, so
    a string compare orders filings). ``fields`` is the parsed Form D scalar map
    (funding_amount, funding_date, …). ``related_persons`` are the named
    officers/directors with titles — the executive_hired source (spec §3.6)."""

    cik: str
    accession: str
    form: str = "D"
    filing_date: str = ""
    fields: Dict[str, object] = field(default_factory=dict)
    related_persons: List[Dict[str, str]] = field(default_factory=list)


class SecEdgarProvider(EnrichmentProvider):
    name = "sec_edgar"
    capabilities = [
        "funding_amount", "funding_date", "funding_stage_signal",
        "decision_makers", "industry", "sec_cik",
    ]
    # Government primary-source filings: high trust when a match is found, but
    # name→CIK matching has false-negative risk, so it sits as a free signal.
    default_confidence = 0.8
    requires_api_key = False

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        company = (lead.company or "").strip()
        if not company:
            return EnrichmentResult(provider=self.name, success=False, error="no_company")
        try:
            async with httpx.AsyncClient(timeout=_HTTP_TIMEOUT, headers=_HEADERS,
                                         follow_redirects=True) as client:
                cik = await self._resolve_cik(client, company)
                if not cik:
                    return EnrichmentResult(provider=self.name, success=False,
                                            error="no_cik_match")
                acc = await self._latest_form_d_accession(client, cik)
                fields: Dict[str, object] = {"sec_cik": cik}
                if acc:
                    form_d = await self._fetch_form_d(client, cik, acc)
                    if form_d:
                        fields.update(form_d)
            # Drop empties; require more than just the CIK to count as a hit.
            fields = {k: v for k, v in fields.items() if v not in (None, "", [])}
            if len(fields) <= 1:  # only sec_cik → not useful enrichment
                return EnrichmentResult(provider=self.name, success=False,
                                        error="no_filing_data", fields=fields)
            return EnrichmentResult(provider=self.name, success=True, fields=fields,
                                    confidence=self.default_confidence)
        except Exception as e:  # never crash the waterfall
            logger.debug(f"sec_edgar enrich failed for {company!r}: {e}")
            return EnrichmentResult(provider=self.name, success=False, error=str(e)[:120])

    # ── CIK resolution ─────────────────────────────────────────────────────

    async def _resolve_cik(self, client: httpx.AsyncClient, company: str) -> Optional[str]:
        """Resolve company name → zero-padded 10-digit CIK.

        Path 1: exact normalized-name match against company_tickers.json.
        Path 2: EDGAR full-text search restricted to Form D (private raisers)."""
        cik = await self._cik_from_tickers(client, company)
        if cik:
            return cik
        return await self._cik_from_fulltext(client, company)

    async def _cik_from_tickers(self, client: httpx.AsyncClient, company: str) -> Optional[str]:
        data = await self._get_json(client, _TICKERS_URL)
        if not data:
            return None
        target = _norm_name(company)
        if not target:
            return None
        # company_tickers.json is {"0": {"cik_str": 789019, "title": "MICROSOFT CORP"}, ...}
        for row in data.values():
            if _norm_name(str(row.get("title", ""))) == target:
                return _pad_cik(row.get("cik_str"))
        return None

    async def _cik_from_fulltext(self, client: httpx.AsyncClient, company: str) -> Optional[str]:
        params = {"q": f'"{company}"', "forms": "D"}
        data = await self._get_json(client, _FTS_URL, params=params)
        if not data:
            return None
        hits = (((data or {}).get("hits") or {}).get("hits")) or []
        target = _norm_name(company)
        best: Optional[str] = None
        for hit in hits:
            src = hit.get("_source", {}) or {}
            ciks = src.get("ciks") or []
            if not ciks:
                continue
            names = src.get("display_names") or []
            # display_names look like "Stripe Milton LLC  (CIK 0002000934)" —
            # prefer an exact normalized match, else fall back to the top hit.
            for nm in names:
                stripped = re.sub(r"\s*\(cik\s+\d+\)\s*$", "", nm, flags=re.IGNORECASE)
                if _norm_name(stripped) == target:
                    return _pad_cik(ciks[0])
            if best is None:
                best = _pad_cik(ciks[0])
        return best  # top relevance hit when no exact-name match

    # ── Filing discovery ───────────────────────────────────────────────────

    async def _latest_form_d_accession(self, client: httpx.AsyncClient,
                                       cik: str) -> Optional[str]:
        """Most recent Form D accession number (dashless) from submissions JSON.
        Filings come back newest-first in parallel arrays."""
        data = await self._get_json(client, _SUBMISSIONS_URL.format(cik=cik))
        if not data:
            return None
        recent = (((data or {}).get("filings") or {}).get("recent")) or {}
        forms = recent.get("form") or []
        accs = recent.get("accessionNumber") or []
        for form, acc in zip(forms, accs):
            if form == "D":
                return acc.replace("-", "")
        return None

    async def _fetch_form_d(self, client: httpx.AsyncClient, cik: str,
                            acc_nodash: str) -> Dict[str, object]:
        url = _ARCHIVES_BASE.format(cik=int(cik), acc=acc_nodash)
        xml = await self._get_text(client, url)
        if not xml:
            return {}
        return _parse_form_d(xml)

    # ── Incremental Form D enumeration (intent-poller funding/exec, §3.6) ─────

    async def resolve_cik(self, company_or_cik: str) -> Optional[str]:
        """Public CIK resolver. Accepts a ``sec_cik:<cik>`` literal, a bare CIK,
        or a company name/domain-derived name. Returns the 10-digit padded CIK or
        None. Used by the poller to cache ``resolved_cik`` on the watch."""
        s = (company_or_cik or "").strip()
        if not s:
            return None
        low = s.lower()
        if low.startswith("sec_cik:"):
            return _pad_cik(low.split(":", 1)[1].strip())
        # A bare numeric CIK.
        if re.fullmatch(r"\d{1,10}", s):
            return _pad_cik(s)
        async with httpx.AsyncClient(timeout=_HTTP_TIMEOUT, headers=_HEADERS,
                                     follow_redirects=True) as client:
            return await self._resolve_cik(client, s)

    async def list_form_d_since(
        self, company_or_cik: str, since_accession: Optional[str] = None
    ) -> List[FormDFiling]:
        """All "D"/"D-A" filings strictly NEWER than ``since_accession`` (spec §3.6).

        Walks ``submissions.recent`` parallel arrays (newest-first), collecting
        every Form D/D-A whose dashless accession compares strictly greater than
        the dashless ``since_accession`` cursor; stops at the cursor (accessions
        are monotonic by SEC issuance). Fetches + parses each ``primary_doc.xml``
        and returns the accession + filing_date + parsed fields + related persons.
        Returns ``[]`` (newest-first order preserved) and NEVER raises — a 5xx /
        parse error degrades to an empty list (caller treats as "no change").
        """
        cik = await self.resolve_cik(company_or_cik)
        if not cik:
            return []
        cursor = (since_accession or "").replace("-", "")
        out: List[FormDFiling] = []
        try:
            async with httpx.AsyncClient(timeout=_HTTP_TIMEOUT, headers=_HEADERS,
                                         follow_redirects=True) as client:
                data = await self._get_json(client, _SUBMISSIONS_URL.format(cik=cik))
                if not data:
                    return []
                recent = (((data or {}).get("filings") or {}).get("recent")) or {}
                forms = recent.get("form") or []
                accs = recent.get("accessionNumber") or []
                dates = recent.get("filingDate") or []
                for i, form in enumerate(forms):
                    if form not in ("D", "D/A"):
                        continue
                    acc_raw = accs[i] if i < len(accs) else ""
                    acc = (acc_raw or "").replace("-", "")
                    if not acc:
                        continue
                    # Newest-first; stop once we reach/pass the cursor.
                    if cursor and acc <= cursor:
                        break
                    filing_date = dates[i] if i < len(dates) else ""
                    xml = await self._get_text(
                        client, _ARCHIVES_BASE.format(cik=int(cik), acc=acc)
                    )
                    fields = _parse_form_d(xml) if xml else {}
                    persons = []
                    if xml:
                        try:
                            persons = _extract_related_persons(ET.fromstring(xml))
                        except ET.ParseError:
                            persons = []
                    out.append(FormDFiling(
                        cik=cik, accession=acc, form=form,
                        filing_date=filing_date, fields=fields,
                        related_persons=persons,
                    ))
        except Exception as e:  # never crash the poller
            logger.debug("list_form_d_since failed for %r: %s", company_or_cik, e)
            return out
        return out

    # ── Rate-limited HTTP helpers (graceful on any non-200 / error) ─────────

    async def _get_json(self, client: httpx.AsyncClient, url: str,
                        params: Optional[dict] = None) -> Optional[dict]:
        await _LIMITER.acquire()
        try:
            r = await client.get(url, params=params)
            if r.status_code != 200:
                return None
            return r.json()
        except (httpx.HTTPError, ValueError, json.JSONDecodeError):
            return None

    async def _get_text(self, client: httpx.AsyncClient, url: str) -> Optional[str]:
        await _LIMITER.acquire()
        try:
            r = await client.get(url)
            if r.status_code != 200:
                return None
            return r.text
        except httpx.HTTPError:
            return None


# ── Form D XML → fields ────────────────────────────────────────────────────

def _localname(tag: str) -> str:
    return tag.rsplit("}", 1)[-1]


def _find(elem, name):
    """Namespace-agnostic first descendant matching a local tag name."""
    if elem is None:
        return None
    for child in elem.iter():
        if _localname(child.tag) == name:
            return child
    return None


def _text(elem, name) -> str:
    node = _find(elem, name)
    return (node.text or "").strip() if node is not None and node.text else ""


def _parse_form_d(xml: str) -> Dict[str, object]:
    """Map a Form D primary_doc.xml to enrichment fields. Returns {} on any
    parse failure — callers treat that as 'no data', never an error."""
    try:
        root = ET.fromstring(xml)
    except ET.ParseError:
        return {}

    out: Dict[str, object] = {}

    amount = _text(root, "totalAmountSold") or _text(root, "totalOfferingAmount")
    if amount and amount.isdigit() and int(amount) > 0:
        out["funding_amount"] = int(amount)

    # dateOfFirstSale wraps the date in a <value> child.
    dof = _find(root, "dateOfFirstSale")
    date_val = _text(dof, "value") if dof is not None else ""
    if not date_val:
        date_val = _text(root, "dateOfFirstSale")
    m = re.search(r"\d{4}-\d{2}-\d{2}", date_val)
    if m:
        out["funding_date"] = m.group(0)

    industry = _text(root, "industryGroupType")
    if industry and industry.lower() != "other":
        out["industry"] = industry

    # A Form D filing IS the funding-stage signal — a private securities raise.
    out["funding_stage_signal"] = "private_placement_form_d"

    decision_makers = _extract_related_persons(root)
    if decision_makers:
        # Stored as a JSON string per the EnrichmentResult data contract; the
        # engine writes this to Lead.decision_makers and never to a raw cell.
        out["decision_makers"] = json.dumps(decision_makers)

    return out


def _extract_related_persons(root) -> List[Dict[str, str]]:
    """Related persons = executive officers / directors / promoters named on the
    Form D — i.e. decision-makers with titles. Matches the shape the rest of the
    app uses: [{"name", "title", ...}]."""
    people: List[Dict[str, str]] = []
    rpl = _find(root, "relatedPersonsList")
    if rpl is None:
        return people
    for info in rpl.iter():
        if _localname(info.tag) != "relatedPersonInfo":
            continue
        name_node = _find(info, "relatedPersonName")
        first = _text(name_node, "firstName")
        middle = _text(name_node, "middleName")
        last = _text(name_node, "lastName")
        # Form D uses "N/A" as a placeholder for entity-typed related persons.
        parts = [p for p in (first, middle, last) if p and p.upper() != "N/A"]
        name = " ".join(parts).strip()
        if not name:
            continue
        rels = [
            (r.text or "").strip()
            for r in info.iter()
            if _localname(r.tag) == "relationship" and r.text and r.text.strip()
        ]
        people.append({
            "name": name,
            "title": ", ".join(dict.fromkeys(rels)),  # de-dup, preserve order
            "source": "sec_form_d",
        })
    return people


# ── name + cik helpers ─────────────────────────────────────────────────────

def _norm_name(name: str) -> str:
    """Lowercase, strip legal suffixes + punctuation, collapse whitespace."""
    s = (name or "").lower()
    s = _NON_ALNUM_RE.sub(" ", s)
    s = _SUFFIX_RE.sub(" ", s)
    return re.sub(r"\s+", " ", s).strip()


def _pad_cik(cik) -> Optional[str]:
    """Zero-pad a CIK to the 10 digits the data.sec.gov endpoints require."""
    if cik is None or cik == "":
        return None
    try:
        return f"{int(cik):010d}"
    except (TypeError, ValueError):
        return None
