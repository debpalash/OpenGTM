"""
Tech Stack Detection Provider — Detect technologies used by company websites.

Analyzes HTTP headers, HTML meta tags, script sources, and response patterns
to detect CMS, frameworks, analytics, and business tools. Zero API cost.

Capabilities: technologies, tech_stack
Free, unlimited, no API key needed.

Fingerprints derive from the MIT-licensed Wappalyzer dataset (developit/
wappalyzer, pinned) — see data/tech_fingerprints.NOTICE + scripts/
build_tech_fingerprints.py. The per-lead homepage fetch is:
  * GATED behind TECH_STACK_WEBSITE_FETCH_ENABLED (default OFF on cloud — a
    per-lead outbound GET carries cost/politeness/legal surface);
  * SSRF-guarded (lead.website is tenant-controlled → confused-deputy risk) via
    core.url_guard.check_url(resolve=True) re-checked across redirects;
  * homepage-only (1 GET/domain/run), cached by domain for 90 days.
"""

import json
import re
import time
import logging
from pathlib import Path
from typing import Dict, List, Optional, Tuple

from apps.api.core.config import settings
from apps.api.core.url_guard import check_url, BlockedUrlError
from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.enrichment.cache import canonical_key, get_cache
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.tech_stack")

# MIT-licensed Wappalyzer fingerprint DB (developit/wappalyzer, pinned), compiled
# to this provider's flat format by scripts/build_tech_fingerprints.py.
_DB_PATH = Path(__file__).parent.parent / "data" / "tech_fingerprints.json"

# Honest, identifying UA (politeness — replaces the previous spoofed-Chrome UA).
_UA = "Yupcha-TechDetect/1.0 (+https://yupcha.com/bot)"
_HTML_CAP = 200_000  # 200 KB cap for perf
_TTL_DAYS = 90       # tech changes slowly; matches company_size TTL
_FETCH_TIMEOUT = 10.0
_MAX_REDIRECTS = 3

# Direct-hit vs implied confidence.
_CONF_DIRECT = 0.75
_CONF_IMPLIED = 0.5

# ── Technology Fingerprints ──────────────────────────────────────────────
# Lightweight FIRST-PARTY curated subset (original to this repo — no third-party
# license). Covers the most common business-relevant technologies with cleaner
# category labels than the raw upstream IDs. Merged over the bundled MIT DB.

FINGERPRINTS: List[Dict] = [
    # ── CMS ──
    {"name": "WordPress", "cat": "CMS", "headers": {"x-powered-by": r"WordPress"}, "html": [r'wp-content/', r'wp-includes/'], "meta": {"generator": r"WordPress"}},
    {"name": "Shopify", "cat": "eCommerce", "headers": {"x-shopid": r"."}, "html": [r'cdn\.shopify\.com', r'Shopify\.theme'], "cookies": ["_shopify_s"]},
    {"name": "Wix", "cat": "CMS", "html": [r'static\.wixstatic\.com', r'X-Wix-']},
    {"name": "Squarespace", "cat": "CMS", "html": [r'static\.squarespace\.com', r'squarespace-cdn']},
    {"name": "Webflow", "cat": "CMS", "html": [r'assets\.website-files\.com', r'webflow\.com'], "meta": {"generator": r"Webflow"}},
    {"name": "Drupal", "cat": "CMS", "headers": {"x-drupal-cache": r"."}, "html": [r'sites/default/files', r'Drupal\.settings'], "meta": {"generator": r"Drupal"}},
    {"name": "Joomla", "cat": "CMS", "html": [r'/media/jui/', r'Joomla!'], "meta": {"generator": r"Joomla"}},
    {"name": "Ghost", "cat": "CMS", "html": [r'ghost\.org', r'ghost-'], "meta": {"generator": r"Ghost"}},
    {"name": "HubSpot CMS", "cat": "CMS", "html": [r'js\.hs-scripts\.com', r'hs-banner\.com', r'hubspot']},

    # ── eCommerce ──
    {"name": "WooCommerce", "cat": "eCommerce", "html": [r'woocommerce', r'wc-cart']},
    {"name": "Magento", "cat": "eCommerce", "html": [r'Magento', r'mage/'], "cookies": ["frontend"]},
    {"name": "BigCommerce", "cat": "eCommerce", "html": [r'bigcommerce\.com', r'cdn\d+\.bigcommerce']},
    {"name": "PrestaShop", "cat": "eCommerce", "html": [r'prestashop', r'PrestaShop'], "meta": {"generator": r"PrestaShop"}},

    # ── JavaScript Frameworks ──
    {"name": "React", "cat": "JS Framework", "html": [r'react\.production\.min\.js', r'__NEXT_DATA__', r'_reactRootContainer', r'react-dom']},
    {"name": "Next.js", "cat": "JS Framework", "html": [r'__NEXT_DATA__', r'_next/static', r'next/dist'], "headers": {"x-powered-by": r"Next\.js"}},
    {"name": "Vue.js", "cat": "JS Framework", "html": [r'vue\.(?:min\.)?js', r'__vue__', r'Vue\.config']},
    {"name": "Nuxt.js", "cat": "JS Framework", "html": [r'__NUXT__', r'_nuxt/'], "headers": {"x-powered-by": r"Nuxt"}},
    {"name": "Angular", "cat": "JS Framework", "html": [r'ng-version', r'angular\.(?:min\.)?js', r'ng-app']},
    {"name": "Svelte", "cat": "JS Framework", "html": [r'svelte', r'__svelte']},
    {"name": "jQuery", "cat": "JS Library", "html": [r'jquery[\.-][\d\.]+\.(?:min\.)?js']},
    {"name": "Bootstrap", "cat": "CSS Framework", "html": [r'bootstrap\.(?:min\.)?(?:css|js)', r'cdn\.jsdelivr\.net/npm/bootstrap']},
    {"name": "Tailwind CSS", "cat": "CSS Framework", "html": [r'tailwindcss', r'tailwind\.min\.css']},

    # ── Analytics & Marketing ──
    {"name": "Google Analytics", "cat": "Analytics", "html": [r'google-analytics\.com/analytics', r'googletagmanager\.com', r'gtag\(', r'UA-\d{4,}']},
    {"name": "Google Tag Manager", "cat": "Tag Manager", "html": [r'googletagmanager\.com/gtm\.js', r'GTM-[A-Z0-9]+']},
    {"name": "Hotjar", "cat": "Analytics", "html": [r'hotjar\.com', r'hj\(']},
    {"name": "Mixpanel", "cat": "Analytics", "html": [r'cdn\.mxpnl\.com', r'mixpanel']},
    {"name": "Segment", "cat": "Analytics", "html": [r'cdn\.segment\.com', r'analytics\.identify']},
    {"name": "Amplitude", "cat": "Analytics", "html": [r'cdn\.amplitude\.com', r'amplitude']},
    {"name": "Plausible", "cat": "Analytics", "html": [r'plausible\.io']},
    {"name": "Heap", "cat": "Analytics", "html": [r'heap-\d+', r'heapanalytics\.com']},
    {"name": "Matomo", "cat": "Analytics", "html": [r'matomo\.js', r'piwik\.js']},
    {"name": "Facebook Pixel", "cat": "Marketing", "html": [r'connect\.facebook\.net/en_US/fbevents', r'fbq\(']},
    {"name": "LinkedIn Insight", "cat": "Marketing", "html": [r'snap\.licdn\.com', r'_linkedin_data_partner']},
    {"name": "HubSpot", "cat": "Marketing", "html": [r'js\.hs-scripts\.com', r'hs-analytics']},
    {"name": "Intercom", "cat": "Support", "html": [r'widget\.intercom\.io', r'Intercom\(', r'intercomSettings']},
    {"name": "Drift", "cat": "Support", "html": [r'js\.driftt\.com', r'drift\.com']},
    {"name": "Crisp", "cat": "Support", "html": [r'client\.crisp\.chat']},
    {"name": "Zendesk", "cat": "Support", "html": [r'static\.zdassets\.com', r'zopim', r'zendesk']},
    {"name": "Freshdesk", "cat": "Support", "html": [r'widget\.freshworks\.com', r'freshdesk']},
    {"name": "Mailchimp", "cat": "Email Marketing", "html": [r'chimpstatic\.com', r'mc\.us\d+\.list-manage']},
    {"name": "Calendly", "cat": "Scheduling", "html": [r'calendly\.com/']},

    # ── Hosting / CDN ──
    {"name": "Cloudflare", "cat": "CDN", "headers": {"server": r"cloudflare", "cf-ray": r"."}},
    {"name": "AWS", "cat": "Cloud", "headers": {"server": r"AmazonS3|Amazon|awselb", "x-amz-request-id": r"."}},
    {"name": "Vercel", "cat": "Hosting", "headers": {"server": r"Vercel", "x-vercel-id": r"."}},
    {"name": "Netlify", "cat": "Hosting", "headers": {"server": r"Netlify", "x-nf-request-id": r"."}},
    {"name": "Nginx", "cat": "Web Server", "headers": {"server": r"nginx"}},
    {"name": "Apache", "cat": "Web Server", "headers": {"server": r"Apache"}},

    # ── Backend ──
    {"name": "PHP", "cat": "Language", "headers": {"x-powered-by": r"PHP"}},
    {"name": "ASP.NET", "cat": "Language", "headers": {"x-powered-by": r"ASP\.NET", "x-aspnet-version": r"."}},
    {"name": "Ruby on Rails", "cat": "Framework", "headers": {"x-powered-by": r"Phusion Passenger"}, "html": [r'csrf-token']},
    {"name": "Laravel", "cat": "Framework", "cookies": ["laravel_session", "XSRF-TOKEN"]},
    {"name": "Django", "cat": "Framework", "cookies": ["csrftoken"], "headers": {"x-frame-options": r"SAMEORIGIN"}},

    # ── Business Tools (high ICP signal) ──
    {"name": "Salesforce", "cat": "CRM", "html": [r'force\.com', r'salesforce']},
    {"name": "Marketo", "cat": "Marketing Automation", "html": [r'marketo\.net', r'mkto']},
    {"name": "Pardot", "cat": "Marketing Automation", "html": [r'pardot\.com', r'pi\.pardot']},
    {"name": "Stripe", "cat": "Payments", "html": [r'js\.stripe\.com', r'Stripe\(']},
    {"name": "PayPal", "cat": "Payments", "html": [r'paypal\.com/sdk', r'paypalobjects\.com']},
    {"name": "Razorpay", "cat": "Payments", "html": [r'checkout\.razorpay\.com']},
    {"name": "Recaptcha", "cat": "Security", "html": [r'google\.com/recaptcha', r'grecaptcha']},
    {"name": "Cloudflare Turnstile", "cat": "Security", "html": [r'challenges\.cloudflare\.com/turnstile']},
]


# ── Compiled fingerprint index (curated subset + bundled MIT DB) ──
# Built lazily on first detection so unused imports stay cheap. Curated entries
# win on name collision (they carry better category labels).

_COMPILED: Optional[List[Dict]] = None


def _load_db() -> Dict[str, Dict]:
    try:
        with open(_DB_PATH, "r", encoding="utf-8") as fh:
            return json.load(fh)
    except FileNotFoundError:
        logger.warning("tech_stack: fingerprint DB missing at %s — using curated subset only", _DB_PATH)
        return {}
    except Exception as e:  # pragma: no cover
        logger.warning("tech_stack: failed to load fingerprint DB: %s", e)
        return {}


def _build_compiled() -> List[Dict]:
    merged: Dict[str, Dict] = {}
    # Full DB first, then curated overrides (curated has cleaner categories).
    for name, spec in _load_db().items():
        merged[name] = {**spec, "name": name}
    for fp in FINGERPRINTS:
        existing = merged.get(fp["name"])
        if existing:
            # Curated wins on signals/category, but inherit the DB's `implies`
            # (e.g. curated WordPress lacks implies; the DB entry implies PHP+MySQL).
            fp = {**fp, "implies": fp.get("implies") or existing.get("implies", [])}
        merged[fp["name"]] = fp

    compiled: List[Dict] = []
    for fp in merged.values():
        try:
            entry = {
                "name": fp["name"],
                "cat": fp.get("cat", "Other"),
                "implies": fp.get("implies", []) or [],
                "headers": [(h.lower(), re.compile(p or ".", re.I)) for h, p in fp.get("headers", {}).items()],
                "html": [re.compile(p, re.I) for p in fp.get("html", [])],
                "meta": [(m, re.compile(p or ".", re.I)) for m, p in fp.get("meta", {}).items()],
                "cookies": [c.lower() for c in fp.get("cookies", [])],
            }
        except re.error:
            continue
        compiled.append(entry)
    logger.info("tech_stack: compiled %d fingerprints", len(compiled))
    return compiled


def _get_compiled() -> List[Dict]:
    global _COMPILED
    if _COMPILED is None:
        _COMPILED = _build_compiled()
    return _COMPILED


def detect_tech_from_response(
    headers: Dict[str, str],
    html: str,
    cookies: List[str],
) -> List[Dict]:
    """Detect technologies from HTTP response data.

    Returns ``[{name, category, confidence}]``. Direct signal hits carry
    ``_CONF_DIRECT``; technologies pulled in via ``implies`` carry ``_CONF_IMPLIED``.
    """
    html_lower = html.lower() if html else ""
    headers_lower = {k.lower(): v.lower() for k, v in headers.items()}
    cookies_lower = [c.lower() for c in cookies]

    # Pre-extract meta name→content once (instead of a regex per fingerprint).
    meta_tags: Dict[str, str] = {}
    for m in re.finditer(
        r'<meta\s[^>]*name=["\']?([\w:-]+)["\']?[^>]*content=["\']([^"\']*)["\']',
        html_lower, re.I,
    ):
        meta_tags.setdefault(m.group(1).lower(), m.group(2))

    found_names: Dict[str, str] = {}    # name → category
    direct: set = set()                 # names matched on a real signal

    for fp in _get_compiled():
        if fp["name"] in found_names:
            continue
        hit = False
        for hdr, rx in fp["headers"]:
            if hdr in headers_lower and rx.search(headers_lower[hdr]):
                hit = True
                break
        if not hit:
            for rx in fp["html"]:
                if rx.search(html_lower):
                    hit = True
                    break
        if not hit:
            for meta_name, rx in fp["meta"]:
                content = meta_tags.get(meta_name.lower())
                if content is not None and rx.search(content):
                    hit = True
                    break
        if not hit:
            for cookie_name in fp["cookies"]:
                if cookie_name in cookies_lower:
                    hit = True
                    break
        if hit:
            found_names[fp["name"]] = fp["cat"]
            direct.add(fp["name"])

    # Resolve `implies` (e.g. WooCommerce ⇒ WordPress ⇒ PHP).
    by_name = {fp["name"]: fp for fp in _get_compiled()}
    queue = list(found_names.keys())
    while queue:
        cur = queue.pop()
        for imp in by_name.get(cur, {}).get("implies", []):
            imp = imp.split("\\;")[0]
            if imp and imp not in found_names:
                found_names[imp] = by_name.get(imp, {}).get("cat", "Other")
                queue.append(imp)

    return [
        {
            "name": n,
            "category": c,
            "confidence": _CONF_DIRECT if n in direct else _CONF_IMPLIED,
        }
        for n, c in found_names.items()
    ]


# ── Safe homepage fetch (SSRF-guarded, redirect-rechecked, polite) ───────────

async def _safe_get(url: str) -> Tuple[Dict[str, str], str, List[str]]:
    """Fetch a homepage safely. Returns (headers, html, cookie_names).

    SSRF guard (lead.website is tenant-controlled): every hop (initial + each
    redirect) is validated with check_url(resolve=True) BEFORE the request, and
    redirects are followed MANUALLY (capped) so a 3xx to a private/metadata host
    can't slip past. TLS verification is ON unless TECH_STACK_INSECURE_TLS.
    Raises BlockedUrlError on an unsafe URL (no request is issued for it).
    """
    import httpx
    from urllib.parse import urljoin

    verify = not bool(getattr(settings, "TECH_STACK_INSECURE_TLS", False))
    req_headers = {
        "User-Agent": _UA,
        "Accept": "text/html,application/xhtml+xml",
        "Accept-Language": "en-US,en;q=0.9",
    }

    cur = url
    async with httpx.AsyncClient(
        timeout=_FETCH_TIMEOUT,
        follow_redirects=False,
        verify=verify,
        headers=req_headers,
    ) as client:
        for _hop in range(_MAX_REDIRECTS + 1):
            check_url(cur, allow_http=True, resolve=True)  # raises BlockedUrlError
            resp = await client.get(cur)
            if resp.status_code in (301, 302, 303, 307, 308):
                loc = resp.headers.get("location")
                if not loc:
                    break
                cur = urljoin(cur, loc)
                continue
            headers = dict(resp.headers)
            html = resp.text[:_HTML_CAP]
            cookies = [
                c.split("=")[0].strip()
                for c in resp.headers.get_list("set-cookie")
                if "=" in c
            ]
            return headers, html, cookies
    raise BlockedUrlError("too many redirects")


async def _robots_allows(base_url: str) -> bool:
    """Best-effort robots.txt check for the homepage path. Fail-OPEN on any error
    (robots is politeness, not security — the SSRF guard handles safety)."""
    from urllib.parse import urlparse
    from urllib.robotparser import RobotFileParser

    try:
        parsed = urlparse(base_url)
        robots_url = f"{parsed.scheme}://{parsed.netloc}/robots.txt"
        check_url(robots_url, allow_http=True, resolve=True)
        headers, body, _ = await _safe_get(robots_url)
        rp = RobotFileParser()
        rp.parse((body or "").splitlines())
        return rp.can_fetch(_UA, base_url)
    except Exception:
        return True


def _structured_to_fields(techs: List[Dict]) -> Dict[str, str]:
    """Build the persisted fields from a structured technographics list."""
    tech_names = [t["name"] for t in techs]
    structured = [
        {
            "name": t["name"],
            "category": t.get("category", "Other"),
            "source": "website",
            "confidence": t.get("confidence", _CONF_DIRECT),
        }
        for t in techs
    ]
    return {
        "technologies": ", ".join(tech_names),
        "technographics": json.dumps(structured, separators=(",", ":")),
    }


class TechStackProvider(EnrichmentProvider):
    name = "tech_stack"
    capabilities = ["technologies"]
    default_confidence = _CONF_DIRECT

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        t0 = time.time()

        def _fail(error: str) -> EnrichmentResult:
            return EnrichmentResult(
                provider=self.name, success=False, error=error,
                duration_ms=(time.time() - t0) * 1000,
            )

        if not lead.website:
            return _fail("No website URL available")

        # Master gate: per-lead outbound fetch is OFF by default (cost/politeness/
        # legal). With it off we make ZERO network calls — byte-identical to today.
        if not bool(getattr(settings, "TECH_STACK_WEBSITE_FETCH_ENABLED", False)):
            return _fail("website_fetch_disabled")

        # ── Cache (keyed by domain, 90d) — kills duplicate fetches ──
        cache = get_cache()
        ck = canonical_key("technologies", lead)
        if ck:
            cached = cache.get(ck, "technographics")
            if cached and cached.get("value"):
                try:
                    techs = json.loads(cached["value"])
                except (ValueError, TypeError):
                    techs = None
                if techs:
                    return EnrichmentResult(
                        provider=self.name, success=True,
                        fields=_structured_to_fields(techs),
                        confidence=self.default_confidence,
                        duration_ms=(time.time() - t0) * 1000,
                    )

        try:
            from apps.api.services.leadgen.enrichment.website_scraper import normalize_website_url
            url = normalize_website_url(lead.website)

            if bool(getattr(settings, "TECH_STACK_RESPECT_ROBOTS", True)):
                if not await _robots_allows(url):
                    return _fail("robots_disallowed")

            headers, html, cookies = await _safe_get(url)
            detected = detect_tech_from_response(headers, html, cookies)

            if not detected:
                return _fail("No technologies detected")

            fields = _structured_to_fields(detected)
            # Cache the STRUCTURED result (the technographics JSON) by domain.
            if ck:
                cache.set(
                    ck, "technographics", fields["technographics"],
                    confidence=self.default_confidence, provider=self.name,
                    ttl_days=_TTL_DAYS,
                )
            return EnrichmentResult(
                provider=self.name, success=True, fields=fields,
                confidence=self.default_confidence,
                duration_ms=(time.time() - t0) * 1000,
            )

        except BlockedUrlError as e:
            logger.warning("tech_stack: blocked URL for %s: %s", lead.website, e)
            return _fail(f"blocked_url: {str(e)[:80]}")
        except Exception as e:
            logger.warning("Tech stack detection error for %s: %s", lead.website, e)
            return _fail(str(e)[:200])
