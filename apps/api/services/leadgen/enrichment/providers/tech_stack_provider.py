"""
Tech Stack Detection Provider — Detect technologies used by company websites.

Analyzes HTTP headers, HTML meta tags, script sources, and response patterns
to detect CMS, frameworks, analytics, and business tools. Zero API cost.

Capabilities: technologies, tech_stack
Free, unlimited, no API key needed.

Based on Wappalyzer fingerprint patterns (lightweight subset).
"""

import json
import re
import time
import logging
from pathlib import Path
from typing import Dict, List, Optional

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult
from apps.api.services.leadgen.models import Lead

logger = logging.getLogger("leadgen.tech_stack")

# Full webappanalyzer (open Wappalyzer) fingerprint DB, compiled to this provider's
# format. ~5k passive fingerprints vs the curated subset (FINGERPRINTS) below.
_DB_PATH = Path(__file__).parent.parent / "data" / "tech_fingerprints.json"

# ── Technology Fingerprints ──────────────────────────────────────────────
# Lightweight subset of Wappalyzer patterns — covers the most common
# business-relevant technologies. Organized by category.

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


# ── Compiled fingerprint index (curated subset + full webappanalyzer DB) ──
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
) -> List[Dict[str, str]]:
    """Detect technologies from HTTP response data."""
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

    found_names: Dict[str, str] = {}  # name → category

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

    return [{"name": n, "category": c} for n, c in found_names.items()]


class TechStackProvider(EnrichmentProvider):
    name = "tech_stack"
    capabilities = ["technologies"]
    default_confidence = 0.75

    async def enrich(self, lead: Lead) -> EnrichmentResult:
        t0 = time.time()

        if not lead.website:
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error="No website URL available",
                duration_ms=(time.time() - t0) * 1000,
            )

        try:
            from apps.api.services.leadgen.enrichment.website_scraper import normalize_website_url
            import httpx

            url = normalize_website_url(lead.website)

            async with httpx.AsyncClient(
                timeout=15.0,
                follow_redirects=True,
                headers={
                    "User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
                    "Accept": "text/html,application/xhtml+xml",
                    "Accept-Language": "en-US,en;q=0.9",
                },
                verify=False,
            ) as client:
                resp = await client.get(url)

                headers = dict(resp.headers)
                html = resp.text[:200_000]  # Cap at 200KB for perf
                cookies = [
                    c.split("=")[0].strip()
                    for c in resp.headers.get_list("set-cookie")
                    if "=" in c
                ]

                detected = detect_tech_from_response(headers, html, cookies)

            if detected:
                # Group by category
                by_cat: Dict[str, List[str]] = {}
                for tech in detected:
                    by_cat.setdefault(tech["category"], []).append(tech["name"])

                # Format as readable string
                parts = []
                for cat, techs in sorted(by_cat.items()):
                    parts.append(f"{cat}: {', '.join(techs)}")
                summary = " | ".join(parts)

                # Also store as pipe-separated list for filtering
                tech_names = [t["name"] for t in detected]

                return EnrichmentResult(
                    provider=self.name,
                    success=True,
                    fields={
                        "technologies": ", ".join(tech_names),
                    },
                    confidence=self.default_confidence,
                    duration_ms=(time.time() - t0) * 1000,
                )

            return EnrichmentResult(
                provider=self.name,
                success=False,
                error="No technologies detected",
                duration_ms=(time.time() - t0) * 1000,
            )

        except Exception as e:
            logger.warning(f"Tech stack detection error for {lead.website}: {e}")
            return EnrichmentResult(
                provider=self.name,
                success=False,
                error=str(e)[:200],
                duration_ms=(time.time() - t0) * 1000,
            )
