"""
Email Verification & Classification — the quality gate for discovered emails.

Ported from AfterShip/email-verifier (MIT) + batuhanaky/mailscout, adapted to this
codebase. Two capabilities the old pattern path lacked:

  1. SMTP deliverability + catch-all detection  → upgrades confidence from a blind
     "pattern" guess to an honest "smtp_verified" / rejects bad guesses.
  2. Classification (syntax / disposable / role / free)  → flags low-value contacts
     (info@, free webmail, throwaway domains) so scoring/dedup can act on them.

Single-connection probing (one EHLO/MAIL FROM, many RCPT) keeps it cheap, matching
AfterShip's smtp.go calibration: a random address is probed first to detect catch-all.

Graceful degradation: outbound port 25 is frequently blocked. When the mailserver is
unreachable / greylists / errors, results are *unknown* (None) — never a false reject —
and callers fall back to MX-confirmed pattern guesses. Set LEADGEN_SMTP_VERIFY=0 to
disable live SMTP entirely (classification + MX still work).
"""

import dns.resolver
import logging
import os
import random
import re
import smtplib
import string
from dataclasses import dataclass
from functools import lru_cache
from pathlib import Path
from typing import Dict, List, Optional

logger = logging.getLogger("leadgen.email_verify")

_DATA_DIR = Path(__file__).parent / "data"

# SMTP identity — keep aligned with mailscout_verify.py for consistent reputation.
_HELO_HOST = os.getenv("LEADGEN_SMTP_HELO", "verify.yupcha.com")
_MAIL_FROM = os.getenv("LEADGEN_SMTP_MAIL_FROM", "verify@yupcha.com")
_SMTP_ENABLED = os.getenv("LEADGEN_SMTP_VERIFY", "1").lower() not in ("0", "false", "no")
_DEFAULT_TIMEOUT = float(os.getenv("LEADGEN_SMTP_TIMEOUT", "6"))

EMAIL_RE = re.compile(r"^[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}$")

# SMTP reply codes that mean "the mailbox does not exist" (explicit reject).
_HARD_REJECT_PREFIXES = ("550", "551", "553", "554")
# Codes that mean "try later" / inconclusive — treat as unknown, never as reject.
_SOFT_PREFIXES = ("421", "450", "451", "452", "421")


# ── Classification data (lazy-loaded sets) ───────────────────────────────

@lru_cache(maxsize=4)
def _load_set(filename: str) -> frozenset:
    path = _DATA_DIR / filename
    try:
        with open(path, "r", encoding="utf-8") as fh:
            return frozenset(line.strip().lower() for line in fh if line.strip())
    except FileNotFoundError:
        logger.warning("email_verify: missing data file %s", path)
        return frozenset()


def _role_accounts() -> frozenset:
    return _load_set("role_accounts.txt")


def _free_providers() -> frozenset:
    return _load_set("free_providers.txt")


def _disposable_domains() -> frozenset:
    return _load_set("disposable_domains.txt")


@dataclass
class EmailClassification:
    email: str
    valid_syntax: bool = False
    local_part: str = ""
    domain: str = ""
    is_role: bool = False        # info@, sales@, hr@ … — not a person
    is_free: bool = False        # gmail/yahoo/… — weak B2B signal
    is_disposable: bool = False  # throwaway domain — junk lead

    @property
    def is_personal_corporate(self) -> bool:
        """True when this looks like a real person at a real company domain."""
        return self.valid_syntax and not (self.is_role or self.is_free or self.is_disposable)


def classify_email(email: str) -> EmailClassification:
    """Classify an email without any network call."""
    email = (email or "").strip().lower()
    if "@" not in email:
        return EmailClassification(email=email, valid_syntax=False)

    local, _, domain = email.partition("@")
    valid = bool(EMAIL_RE.match(email))
    return EmailClassification(
        email=email,
        valid_syntax=valid,
        local_part=local,
        domain=domain,
        is_role=local in _role_accounts(),
        is_free=domain in _free_providers(),
        is_disposable=domain in _disposable_domains(),
    )


# ── DNS / MX ─────────────────────────────────────────────────────────────

_mx_cache: Dict[str, Optional[str]] = {}


def resolve_mx(domain: str, timeout: float = _DEFAULT_TIMEOUT) -> Optional[str]:
    """Return the highest-priority MX host for a domain, or None. Cached."""
    if not domain:
        return None
    if domain in _mx_cache:
        return _mx_cache[domain]
    host = None
    try:
        answers = dns.resolver.resolve(domain, "MX", lifetime=timeout)
        # Lowest preference value = highest priority.
        best = min(answers, key=lambda r: r.preference)
        host = str(best.exchange).rstrip(".")
    except Exception:
        host = None
    _mx_cache[domain] = host
    return host


def has_mx(domain: str, timeout: float = _DEFAULT_TIMEOUT) -> bool:
    return resolve_mx(domain, timeout) is not None


# ── SMTP probing (single connection, many RCPT) ──────────────────────────

def _gen_random_local(n: int = 16) -> str:
    return "".join(random.choices(string.ascii_lowercase + string.digits, k=n))


@dataclass
class DomainProbe:
    domain: str
    mx_host: Optional[str] = None
    reachable: bool = False       # did we get an SMTP conversation at all?
    catch_all: bool = False       # accepts a random address → can't verify individuals
    results: Optional[Dict[str, Optional[bool]]] = None  # email → True/False/None(unknown)


_catchall_cache: Dict[str, bool] = {}


def probe_domain(
    domain: str,
    candidates: List[str],
    timeout: float = _DEFAULT_TIMEOUT,
) -> DomainProbe:
    """Open ONE SMTP session to the domain's MX and RCPT-probe a random address
    (catch-all calibration) followed by each candidate.

    Each candidate result is:
        True  → mailbox accepted (deliverable)
        False → hard reject (550-class) — mailbox does not exist
        None  → inconclusive (greylist/soft error/unreachable/catch-all)
    """
    probe = DomainProbe(domain=domain, results={c: None for c in candidates})

    if not _SMTP_ENABLED:
        return probe

    mx_host = resolve_mx(domain, timeout)
    probe.mx_host = mx_host
    if not mx_host:
        return probe

    server = None
    try:
        server = smtplib.SMTP(timeout=timeout)
        server.connect(mx_host, 25)
        server.ehlo_or_helo_if_needed()
        server.helo(_HELO_HOST)
        code, _ = server.mail(_MAIL_FROM)
        if code >= 400:
            return probe  # server refused MAIL FROM — can't probe, leave unknown
        probe.reachable = True

        # Catch-all calibration: a random address that cannot legitimately exist.
        if domain in _catchall_cache:
            probe.catch_all = _catchall_cache[domain]
        else:
            rcode, _ = server.rcpt(f"{_gen_random_local()}@{domain}")
            probe.catch_all = rcode < 400  # accepted the impossible address
            _catchall_cache[domain] = probe.catch_all
        if probe.catch_all:
            logger.debug("Domain %s is catch-all — individual SMTP checks unreliable", domain)
            return probe  # results stay None; caller treats as unverifiable

        for email in candidates:
            try:
                rcode, _ = server.rcpt(email)
            except smtplib.SMTPServerDisconnected:
                break
            except smtplib.SMTPException:
                probe.results[email] = None
                continue
            scode = str(rcode)
            if rcode < 400:
                probe.results[email] = True
            elif scode.startswith(_HARD_REJECT_PREFIXES):
                probe.results[email] = False
            else:
                probe.results[email] = None  # soft/greylist → unknown
    except (smtplib.SMTPException, OSError) as exc:
        # Connection refused / blocked port 25 / timeout → unknown, never reject.
        logger.debug("SMTP probe failed for %s via %s: %s", domain, mx_host, exc)
    finally:
        if server is not None:
            try:
                server.quit()
            except Exception:
                try:
                    server.close()
                except Exception:
                    pass
    return probe


@dataclass
class VerifyResult:
    email: str = ""
    confidence: str = ""   # smtp_verified | pattern | generic | "" (none)
    deliverable: Optional[bool] = None
    catch_all: bool = False
    reachable: bool = False
    classification: Optional[EmailClassification] = None


def verify_email(email: str, timeout: float = _DEFAULT_TIMEOUT) -> VerifyResult:
    """Classify + (optionally) SMTP-verify a single known email."""
    cls = classify_email(email)
    if not cls.valid_syntax:
        return VerifyResult(email=email, confidence="", classification=cls)

    probe = probe_domain(cls.domain, [cls.email], timeout)
    deliverable = probe.results.get(cls.email) if probe.results else None

    if deliverable is True:
        conf = "smtp_verified"
    elif probe.catch_all or deliverable is None:
        # Can't disprove it; keep prior signal strength (MX-backed guess).
        conf = "generic" if cls.is_role else "pattern"
    else:  # explicit hard reject
        conf = ""
    return VerifyResult(
        email=email if conf else "",
        confidence=conf,
        deliverable=deliverable,
        catch_all=probe.catch_all,
        reachable=probe.reachable,
        classification=cls,
    )


def pick_best_email(
    candidates: List[str],
    timeout: float = _DEFAULT_TIMEOUT,
) -> VerifyResult:
    """Given pattern candidates for ONE domain (ordered best-guess first), SMTP-probe
    them in one session and return the first deliverable address.

    Resolution order:
      • a deliverable candidate            → smtp_verified
      • catch-all / all-unknown (blocked)  → first candidate as "pattern" (MX-backed)
      • all hard-rejected                  → empty result (patterns are wrong)
    """
    candidates = [c for c in candidates if c and "@" in c]
    if not candidates:
        return VerifyResult()

    domain = candidates[0].split("@")[1]
    probe = probe_domain(domain, candidates, timeout)
    results = probe.results or {}

    # 1) A confirmed-deliverable candidate wins.
    for cand in candidates:
        if results.get(cand) is True:
            return VerifyResult(
                email=cand, confidence="smtp_verified", deliverable=True,
                catch_all=False, reachable=probe.reachable,
                classification=classify_email(cand),
            )

    # 2) Catch-all or we never reached the server / all greylisted → best guess.
    if probe.catch_all or not probe.reachable or all(v is None for v in results.values()):
        if has_mx(domain, timeout):
            top = candidates[0]
            return VerifyResult(
                email=top, confidence="pattern", deliverable=None,
                catch_all=probe.catch_all, reachable=probe.reachable,
                classification=classify_email(top),
            )
        return VerifyResult(catch_all=probe.catch_all, reachable=probe.reachable)

    # 3) Server answered and hard-rejected everything → patterns are wrong.
    return VerifyResult(deliverable=False, reachable=probe.reachable)
