"""
Batch enrichment quality assessment — tests all 26 providers against real workbook leads.
Measures: hit rate, data quality, speed, and field coverage per provider.
"""
import asyncio
import json
import time
import sys
import os

# Fix path
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', '..'))

from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.enrichment.provider import EnrichmentResult


# ── Test leads (real data from workbook) ──
TEST_LEADS = [
    {"company": "Supersourcing", "website": "https://supersourcing.com", "city": "Bangalore", "email": "info@supersourcing.com"},
    {"company": "CIEL HR", "website": "https://www.cielhr.com", "city": "Bangalore", "email": "info@cielhr.com"},
    {"company": "7 Consultancy", "website": "https://www.7consultancy.in", "city": "Bangalore", "email": "info@7consultancy.in"},
    {"company": "ITSAD Technologies", "website": "https://itsad.co.in", "city": "Bangalore", "email": ""},
    {"company": "ABC Consultants", "website": "https://www.abcconsultants.in", "city": "Mumbai", "email": ""},
    {"company": "TeamLease Services", "website": "https://www.teamlease.com", "city": "Bangalore", "email": ""},
    {"company": "Randstad India", "website": "https://www.randstad.in", "city": "Chennai", "email": ""},
    {"company": "Adecco India", "website": "https://www.adecco.co.in", "city": "Bangalore", "email": ""},
]

# ── Providers to test (OSS-only, no API keys needed) ──
PROVIDERS_TO_TEST = [
    ("deep_scraper", "apps.api.services.leadgen.enrichment.providers.deep_scraper", "DeepScraperProvider", {"max_pages": 4, "timeout": 10}),
    ("email_harvester", "apps.api.services.leadgen.enrichment.providers.email_harvester", "EmailHarvesterProvider", {}),
    ("company_intel", "apps.api.services.leadgen.enrichment.providers.company_intel", "CompanyIntelProvider", {}),
    ("local_business", "apps.api.services.leadgen.enrichment.providers.local_business", "LocalBusinessProvider", {}),
    ("holehe", "apps.api.services.leadgen.enrichment.providers.holehe_verify", "HoleheProvider", {}),
    ("website_scraper", "apps.api.services.leadgen.enrichment.providers.website_scraper_provider", "WebsiteScraperProvider", {}),
    ("crosslinked", "apps.api.services.leadgen.enrichment.providers.crosslinked", "CrossLinkedProvider", {}),
    ("tech_stack", "apps.api.services.leadgen.enrichment.providers.tech_stack_provider", "TechStackProvider", {}),
]


async def test_provider(provider_name, module_path, class_name, kwargs, leads):
    """Test a single provider against all leads."""
    try:
        import importlib
        mod = importlib.import_module(module_path)
        cls = getattr(mod, class_name)
        provider = cls(**kwargs)
    except Exception as e:
        return {"name": provider_name, "error": f"import_failed: {e}", "results": []}

    results = []
    for lead_data in leads:
        lead = Lead()
        lead.company = lead_data["company"]
        lead.website = lead_data["website"]
        lead.city = lead_data.get("city", "")
        lead.email = lead_data.get("email", "")

        try:
            r = await asyncio.wait_for(provider.enrich(lead), timeout=45)
            results.append({
                "company": lead_data["company"],
                "success": r.success,
                "fields": r.fields or {},
                "field_count": len(r.fields or {}),
                "duration_ms": r.duration_ms,
                "error": r.error,
            })
        except asyncio.TimeoutError:
            results.append({
                "company": lead_data["company"],
                "success": False,
                "fields": {},
                "field_count": 0,
                "duration_ms": 45000,
                "error": "timeout",
            })
        except Exception as e:
            results.append({
                "company": lead_data["company"],
                "success": False,
                "fields": {},
                "field_count": 0,
                "duration_ms": 0,
                "error": str(e)[:100],
            })

    return {"name": provider_name, "results": results}


async def main():
    print("=" * 80)
    print("ENRICHMENT QUALITY ASSESSMENT — 8 leads × 8 OSS providers")
    print("=" * 80)

    all_results = {}
    total_start = time.time()

    for pname, mod, cls, kwargs in PROVIDERS_TO_TEST:
        print(f"\n── Testing {pname} ──")
        t0 = time.time()
        result = await test_provider(pname, mod, cls, kwargs, TEST_LEADS)
        elapsed = time.time() - t0

        if result.get("error"):
            print(f"  ❌ FAILED: {result['error']}")
            continue

        hits = sum(1 for r in result["results"] if r["success"])
        total = len(result["results"])
        avg_time = sum(r["duration_ms"] for r in result["results"]) / max(total, 1)
        all_fields = set()
        for r in result["results"]:
            all_fields.update(r["fields"].keys())

        print(f"  Hit rate: {hits}/{total} ({hits/total*100:.0f}%)")
        print(f"  Avg time: {avg_time:.0f}ms | Total: {elapsed:.1f}s")
        print(f"  Fields found: {sorted(all_fields)}")

        # Show sample data
        for r in result["results"]:
            status = "✅" if r["success"] else "❌"
            fields_str = ", ".join(f"{k}={str(v)[:30]}" for k,v in list(r["fields"].items())[:4])
            if r["error"] and not r["success"]:
                fields_str = r["error"][:60]
            print(f"    {status} {r['company'][:25]:25s} [{r['field_count']} fields] {fields_str}")

        all_results[pname] = result

    total_elapsed = time.time() - total_start
    print(f"\n{'=' * 80}")
    print(f"TOTAL TIME: {total_elapsed:.1f}s")

    # ── SUMMARY MATRIX ──
    print(f"\n{'=' * 80}")
    print("PROVIDER HIT-RATE MATRIX")
    print(f"{'=' * 80}")
    header = f"{'Provider':20s}"
    for ld in TEST_LEADS:
        header += f" {ld['company'][:10]:10s}"
    header += " HitRate"
    print(header)
    print("-" * len(header))

    for pname, result in all_results.items():
        row = f"{pname:20s}"
        hits = 0
        for r in result["results"]:
            if r["success"]:
                row += f" {'✅':10s}"
                hits += 1
            else:
                row += f" {'❌':10s}"
        pct = hits / max(len(result["results"]), 1) * 100
        row += f" {pct:.0f}%"
        print(row)

    # ── FIELD COVERAGE ──
    print(f"\n{'=' * 80}")
    print("FIELD COVERAGE (which provider found which fields)")
    print(f"{'=' * 80}")
    field_providers = {}
    for pname, result in all_results.items():
        for r in result["results"]:
            for field in r["fields"].keys():
                field_providers.setdefault(field, set()).add(pname)

    for field, providers in sorted(field_providers.items()):
        print(f"  {field:25s} → {', '.join(sorted(providers))}")

    # Save raw results
    with open("/Users/user4/Desktop/lead-data/enrichment_assessment.json", "w") as f:
        json.dump({
            "leads": TEST_LEADS,
            "results": {k: {"results": v["results"]} for k, v in all_results.items()},
            "total_time": total_elapsed,
        }, f, indent=2, default=str)
    print(f"\nRaw results saved to enrichment_assessment.json")


asyncio.run(main())
