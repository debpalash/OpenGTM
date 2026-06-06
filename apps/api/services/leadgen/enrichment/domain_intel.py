"""
Domain Intelligence Provider — RDAP + DNS analysis.

Inspired by CompanyScope MCP's get_domain_intel tool.
Uses free public APIs (RDAP, Cloudflare DoH) — no API keys needed.

Provides:
  - Domain age & registration date
  - Registrar information
  - Nameservers & hosting provider
  - MX records (email service detection)
  - TXT records (SPF/DKIM = legitimate business signal)
"""

import asyncio
import logging
from typing import Dict, Optional
from datetime import datetime

logger = logging.getLogger("leadgen.domain_intel")

# Known hosting providers by nameserver patterns
HOSTING_PROVIDERS = {
    "cloudflare": "Cloudflare",
    "awsdns": "Amazon AWS",
    "googledomains": "Google Domains",
    "google.com": "Google Cloud",
    "azure-dns": "Microsoft Azure",
    "digitalocean": "DigitalOcean",
    "vercel-dns": "Vercel",
    "netlify": "Netlify",
    "godaddy": "GoDaddy",
    "namecheap": "Namecheap",
    "hostgator": "HostGator",
    "bluehost": "Bluehost",
    "squarespace": "Squarespace",
    "wix": "Wix",
    "shopify": "Shopify",
}

# Known email providers by MX patterns
EMAIL_PROVIDERS = {
    "google.com": "Google Workspace",
    "googlemail.com": "Google Workspace",
    "outlook.com": "Microsoft 365",
    "protection.outlook": "Microsoft 365",
    "pphosted.com": "Proofpoint",
    "mimecast": "Mimecast",
    "zoho.com": "Zoho Mail",
    "mailgun": "Mailgun",
    "sendgrid": "SendGrid",
    "amazonaws": "Amazon SES",
    "secureserver.net": "GoDaddy Email",
    "yahoodns": "Yahoo Mail",
    "privateemail": "Namecheap Email",
}


async def get_rdap_info(domain: str) -> Dict:
    """Query RDAP for domain registration data (free, no auth)."""
    import aiohttp

    result = {
        "registrar": "",
        "registration_date": "",
        "expiration_date": "",
        "domain_age_days": 0,
        "nameservers": [],
        "status": [],
    }

    try:
        url = f"https://rdap.org/domain/{domain}"
        async with aiohttp.ClientSession() as session:
            async with session.get(url, timeout=aiohttp.ClientTimeout(total=10)) as resp:
                if resp.status != 200:
                    return result
                data = await resp.json()

        # Registrar
        entities = data.get("entities", [])
        for entity in entities:
            roles = entity.get("roles", [])
            if "registrar" in roles:
                vcard = entity.get("vcardArray", [None, []])[1] if entity.get("vcardArray") else []
                for item in vcard:
                    if item[0] == "fn":
                        result["registrar"] = item[3]
                        break

        # Dates
        events = data.get("events", [])
        for event in events:
            action = event.get("eventAction", "")
            date_str = event.get("eventDate", "")
            if action == "registration":
                result["registration_date"] = date_str[:10]
                try:
                    reg_date = datetime.fromisoformat(date_str[:10])
                    result["domain_age_days"] = (datetime.now() - reg_date).days
                except Exception:
                    pass
            elif action == "expiration":
                result["expiration_date"] = date_str[:10]

        # Nameservers
        ns_list = data.get("nameservers", [])
        result["nameservers"] = [ns.get("ldhName", "") for ns in ns_list if ns.get("ldhName")]

        # Status
        result["status"] = data.get("status", [])

    except Exception as e:
        logger.debug(f"RDAP lookup failed for {domain}: {e}")

    return result


async def get_dns_records(domain: str) -> Dict:
    """Query DNS via Cloudflare DoH (free, fast, no auth)."""
    import aiohttp

    result = {
        "a_records": [],
        "mx_records": [],
        "ns_records": [],
        "txt_records": [],
        "hosting_provider": "",
        "email_provider": "",
        "has_spf": False,
        "has_dmarc": False,
    }

    base_url = "https://cloudflare-dns.com/dns-query"
    headers = {"Accept": "application/dns-json"}

    async def query_dns(name: str, record_type: str) -> list:
        try:
            async with aiohttp.ClientSession() as session:
                async with session.get(
                    base_url,
                    params={"name": name, "type": record_type},
                    headers=headers,
                    timeout=aiohttp.ClientTimeout(total=5),
                ) as resp:
                    if resp.status != 200:
                        return []
                    data = await resp.json()
                    return [a.get("data", "") for a in data.get("Answer", [])]
        except Exception:
            return []

    # Query all record types in parallel
    a_task = query_dns(domain, "A")
    mx_task = query_dns(domain, "MX")
    ns_task = query_dns(domain, "NS")
    txt_task = query_dns(domain, "TXT")
    dmarc_task = query_dns(f"_dmarc.{domain}", "TXT")

    a_records, mx_records, ns_records, txt_records, dmarc_records = await asyncio.gather(
        a_task, mx_task, ns_task, txt_task, dmarc_task
    )

    result["a_records"] = a_records[:5]
    result["mx_records"] = [r.split(" ", 1)[1] if " " in r else r for r in mx_records[:5]]
    result["ns_records"] = [r.rstrip(".") for r in ns_records[:5]]
    result["txt_records"] = txt_records[:10]

    # Detect hosting provider from nameservers
    for ns in result["ns_records"]:
        ns_lower = ns.lower()
        for pattern, provider in HOSTING_PROVIDERS.items():
            if pattern in ns_lower:
                result["hosting_provider"] = provider
                break
        if result["hosting_provider"]:
            break

    # Detect email provider from MX records
    for mx in result["mx_records"]:
        mx_lower = mx.lower()
        for pattern, provider in EMAIL_PROVIDERS.items():
            if pattern in mx_lower:
                result["email_provider"] = provider
                break
        if result["email_provider"]:
            break

    # Check for SPF and DMARC
    for txt in txt_records:
        if "v=spf1" in txt:
            result["has_spf"] = True
    result["has_dmarc"] = len(dmarc_records) > 0

    return result


# Subdomain prefixes that signal contactable / hiring infrastructure.
INTERESTING_SUBDOMAINS = ("mail", "careers", "jobs", "hr", "recruit", "apply", "portal", "app", "shop", "store")


async def get_crtsh_subdomains(domain: str, timeout: float = 12.0) -> Dict:
    """Discover subdomains from Certificate Transparency logs (crt.sh) — no API key.

    Ported from theHarvester's SearchCrtsh. Subdomains reveal a company's real
    web/mail footprint (careers.*, jobs.*, mail.*) — extra hosts to crawl for
    contacts and a hiring/size signal.
    """
    import httpx

    result = {"subdomains": [], "subdomain_count": 0, "interesting": []}
    url = f"https://crt.sh/?q=%25.{domain}&exclude=expired&deduplicate=Y&output=json"
    rows = None
    # crt.sh frequently 502s under load — retry briefly before giving up.
    try:
        async with httpx.AsyncClient(timeout=timeout, follow_redirects=True) as client:
            for attempt in range(3):
                try:
                    resp = await client.get(url, headers={"User-Agent": "Mozilla/5.0"})
                except Exception:
                    resp = None
                if resp is not None and resp.status_code == 200 and resp.text.strip():
                    rows = resp.json()
                    break
                if attempt < 2:
                    await asyncio.sleep(1.5 * (attempt + 1))
    except Exception as e:
        logger.debug("crt.sh lookup failed for %s: %s", domain, e)
    if not rows:
        return result

    names = set()
    for row in rows if isinstance(rows, list) else []:
        for nv in str(row.get("name_value", "")).splitlines():
            nv = nv.strip().lower().lstrip("*.")
            # Keep real subdomains of the target domain only; drop wildcards/IPs.
            if nv.endswith("." + domain) and not nv[:1].isdigit() and " " not in nv:
                names.add(nv)

    subs = sorted(names)
    result["subdomains"] = subs[:200]
    result["subdomain_count"] = len(subs)
    result["interesting"] = sorted(
        s for s in subs
        if s.split(".")[0] in INTERESTING_SUBDOMAINS
    )[:25]
    return result


async def analyze_domain(domain: str) -> Dict:
    """Full domain intelligence analysis — RDAP + DNS + Certificate Transparency."""
    rdap_task = get_rdap_info(domain)
    dns_task = get_dns_records(domain)
    crt_task = get_crtsh_subdomains(domain)

    rdap_data, dns_data, crt_data = await asyncio.gather(
        rdap_task, dns_task, crt_task, return_exceptions=False,
    )

    # Legitimacy score (0-100) based on domain signals
    legitimacy = 0
    if rdap_data.get("domain_age_days", 0) > 365:
        legitimacy += 25
    if rdap_data.get("domain_age_days", 0) > 1095:  # 3+ years
        legitimacy += 15
    if dns_data.get("has_spf"):
        legitimacy += 15
    if dns_data.get("has_dmarc"):
        legitimacy += 15
    if dns_data.get("email_provider"):
        legitimacy += 10
    if dns_data.get("mx_records"):
        legitimacy += 10
    if dns_data.get("hosting_provider"):
        legitimacy += 10
    if crt_data.get("subdomain_count", 0) > 5:  # real infra footprint
        legitimacy += 5

    return {
        "domain": domain,
        "rdap": rdap_data,
        "dns": dns_data,
        "cert_transparency": crt_data,
        "legitimacy_score": min(legitimacy, 100),
        "summary": {
            "registrar": rdap_data.get("registrar", "Unknown"),
            "age_days": rdap_data.get("domain_age_days", 0),
            "hosting": dns_data.get("hosting_provider", "Unknown"),
            "email_service": dns_data.get("email_provider", "Unknown"),
            "has_email_auth": dns_data.get("has_spf") and dns_data.get("has_dmarc"),
            "subdomains": crt_data.get("subdomain_count", 0),
            "interesting_subdomains": crt_data.get("interesting", []),
        },
    }
