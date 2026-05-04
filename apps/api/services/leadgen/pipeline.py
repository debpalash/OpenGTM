"""
Pipeline Orchestrator — End-to-end lead generation pipeline.
Coordinates: SCRAPE → DEDUPE → ENRICH → SCORE → STORE → REPORT
"""
import asyncio
from datetime import datetime
from typing import List, Optional
from thefuzz import fuzz
from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.db import LeadDB
from apps.api.services.leadgen.scoring import score_leads, score_and_update_db
from apps.api.services.leadgen.config import ICP, SCRAPER_CONFIG


def deduplicate_leads(leads: List[Lead], threshold: int = 85) -> List[Lead]:
    """Deduplicate leads by fuzzy-matching company names."""
    if not leads:
        return leads
    print(f"  🔄 Deduplicating {len(leads)} leads...")
    def data_score(l):
        return sum([bool(l.website), bool(l.email), bool(l.phone),
                     bool(l.linkedin_url), bool(l.contact_person)])
    leads.sort(key=data_score, reverse=True)
    unique, seen_names = [], []
    for lead in leads:
        is_dup = False
        for name in seen_names:
            if fuzz.token_sort_ratio(lead.company.lower(), name.lower()) >= threshold:
                is_dup = True
                break
        if not is_dup:
            unique.append(lead)
            seen_names.append(lead.company)
    print(f"  ✅ {len(leads) - len(unique)} duplicates removed → {len(unique)} unique")
    return unique


async def run_scrapers(sources=None, cities=None, queries=None):
    """Run all or selected scrapers."""
    if cities is None:
        cities = ICP["target_cities"]
    if queries is None:
        queries = SCRAPER_CONFIG["google_maps_queries"]
    all_sources = sources or ["csv", "job_boards", "review_dirs", "directories", "linkedin", "google_search", "news", "crunchbase"]
    all_leads = []
    for source in all_sources:
        print(f"\n{'='*50}\n  🚀 Scraper: {source}\n{'='*50}")
        try:
            if source == "csv":
                from apps.api.services.leadgen.scrapers.csv_import import import_all_csvs
                all_leads.extend(import_all_csvs("."))
            elif source == "google_maps":
                from apps.api.services.leadgen.scrapers.google_maps import scrape_maps_multi_city
                all_leads.extend(await scrape_maps_multi_city(queries[:3], cities[:5], headless=SCRAPER_CONFIG["headless"]))
            elif source == "directories":
                from apps.api.services.leadgen.scrapers.web_directories import scrape_via_search
                all_leads.extend(scrape_via_search(SCRAPER_CONFIG["directory_queries"], cities[:5]))
            elif source == "linkedin":
                from apps.api.services.leadgen.scrapers.linkedin import discover_linkedin_companies
                all_leads.extend(discover_linkedin_companies(queries[:3], cities[:5]))
            elif source == "job_boards":
                from apps.api.services.leadgen.scrapers.job_boards import scrape_naukri_employers, scrape_indeed_employers, scrape_foundit_employers
                jq = ["staffing", "HR consultancy", "recruitment agency"]
                all_leads.extend(scrape_naukri_employers(jq, cities[:5]))
                all_leads.extend(scrape_indeed_employers(jq, cities[:5]))
                all_leads.extend(scrape_foundit_employers(jq, cities[:3]))
            elif source == "review_dirs":
                from apps.api.services.leadgen.scrapers.review_directories import scrape_clutch, scrape_goodfirms, scrape_g2, scrape_ambitionbox
                rq = ["staffing", "HR", "recruitment", "talent acquisition"]
                all_leads.extend(scrape_clutch(rq))
                all_leads.extend(scrape_goodfirms(rq))
                all_leads.extend(scrape_g2(rq[:2]))
                all_leads.extend(scrape_ambitionbox(rq[:2], cities[:5]))
            elif source == "google_search":
                from apps.api.services.leadgen.scrapers.google_search import scrape_google_search
                all_leads.extend(scrape_google_search(queries[:3], cities[:5]))
            elif source == "news":
                from apps.api.services.leadgen.scrapers.google_search import scrape_news_mentions
                all_leads.extend(scrape_news_mentions(["staffing company India", "HR tech startup India"]))
            elif source == "crunchbase":
                from apps.api.services.leadgen.scrapers.crunchbase import scrape_via_search as scrape_crunchbase
                all_leads.extend(scrape_crunchbase(queries[:3], cities[:5]))
        except Exception as e:
            print(f"  ❌ Scraper '{source}' failed: {e}")
    print(f"\n📊 Total raw leads: {len(all_leads)}")
    return all_leads


async def run_enrichment(db, limit=None):
    """Run enrichment pipeline on DB leads."""
    leads = db.get_leads(limit=limit or 10000)
    print(f"\n  🔧 Enriching {len(leads)} leads...")
    try:
        from apps.api.services.leadgen.enrichment.website_scraper import enrich_leads_from_websites
        leads = await enrich_leads_from_websites(leads, batch_size=SCRAPER_CONFIG["batch_size"])
    except Exception as e:
        print(f"  ⚠ Website enrichment: {e}")
    try:
        from apps.api.services.leadgen.enrichment.search_enricher import enrich_via_search
        leads = enrich_via_search(leads)
    except Exception as e:
        print(f"  ⚠ Search enrichment: {e}")
    try:
        from apps.api.services.leadgen.enrichment.email_finder import enrich_emails
        leads = enrich_emails(leads)
    except Exception as e:
        print(f"  ⚠ Email enrichment: {e}")
    try:
        from apps.api.services.leadgen.enrichment.social_finder import find_social_profiles
        leads = find_social_profiles(leads)
    except Exception as e:
        print(f"  ⚠ Social enrichment: {e}")
    now = datetime.utcnow().isoformat()
    for lead in leads:
        if lead.id:
            db.update_lead_fields(lead.id, {
                "email": lead.email, "phone": lead.phone, "website": lead.website,
                "linkedin_url": lead.linkedin_url, "twitter_url": lead.twitter_url,
                "description": lead.description, "last_enriched_at": now,
            })
    print("  ✅ Enrichment complete")


async def run_full_pipeline(sources=None, cities=None, enrich=True):
    """Full pipeline: SCRAPE → DEDUPE → ENRICH → SCORE → STORE"""
    db = LeadDB()
    print("\n" + "=" * 60 + "\n  🚀 YUPCHA LEAD PIPELINE\n" + "=" * 60)
    raw = await run_scrapers(sources=sources, cities=cities)
    unique = deduplicate_leads(raw)
    for l in unique:
        if not l.yupcha_value_prop:
            l.yupcha_value_prop = ICP["value_proposition"]
        if not l.company_need:
            l.company_need = "Centralized Candidate, Attendance, and Client Management System"
    print(f"\n  💾 Storing {len(unique)} leads...")
    db.bulk_upsert(unique)
    if enrich:
        await run_enrichment(db)
    score_and_update_db(db)
    from apps.api.services.leadgen.export import print_stats
    print_stats(db)
    db.close()
    print("\n  ✅ Pipeline complete!")
