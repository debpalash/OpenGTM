"""
Analytics Router — Dashboard metrics and trend data.

Aggregates data from leads, jobs, job_stages, and llm_usage tables
to power the analytics dashboard.
"""

from datetime import datetime, timedelta, timezone
from fastapi import APIRouter
from apps.api.services.leadgen.db import LeadDB

router = APIRouter(prefix="/api/analytics", tags=["Analytics"])


def _db() -> LeadDB:
    return LeadDB()


# ── Overview KPIs ────────────────────────────────────────────────

@router.get("/overview")
async def analytics_overview():
    """Top-level KPI metrics for the dashboard."""
    db = _db()
    c = db.conn.cursor()

    # Total leads
    total = c.execute("SELECT COUNT(*) FROM leads").fetchone()[0]

    # Leads this week / month
    now = datetime.now(timezone.utc)
    week_ago = (now - timedelta(days=7)).isoformat()
    month_ago = (now - timedelta(days=30)).isoformat()

    this_week = c.execute(
        "SELECT COUNT(*) FROM leads WHERE created_at >= ?", (week_ago,)
    ).fetchone()[0]
    this_month = c.execute(
        "SELECT COUNT(*) FROM leads WHERE created_at >= ?", (month_ago,)
    ).fetchone()[0]

    # Score stats
    score_row = c.execute(
        "SELECT AVG(score), MAX(score), MIN(score) FROM leads WHERE score > 0"
    ).fetchone()
    avg_score = round(score_row[0] or 0, 1)

    # Tier distribution
    tiers = {}
    for row in c.execute("SELECT score_tier, COUNT(*) FROM leads GROUP BY score_tier"):
        tiers[row[0]] = row[1]

    # Enrichment coverage
    with_email = c.execute("SELECT COUNT(*) FROM leads WHERE email != '' AND email IS NOT NULL").fetchone()[0]
    with_phone = c.execute("SELECT COUNT(*) FROM leads WHERE phone != '' AND phone IS NOT NULL").fetchone()[0]
    with_website = c.execute("SELECT COUNT(*) FROM leads WHERE website != '' AND website IS NOT NULL").fetchone()[0]
    with_contact = c.execute("SELECT COUNT(*) FROM leads WHERE contact_person != '' AND contact_person IS NOT NULL").fetchone()[0]
    with_linkedin = c.execute("SELECT COUNT(*) FROM leads WHERE linkedin_url != '' AND linkedin_url IS NOT NULL").fetchone()[0]

    # Email confidence breakdown
    email_confidence = {}
    for row in c.execute(
        "SELECT COALESCE(NULLIF(email_confidence, ''), 'unknown'), COUNT(*) FROM leads WHERE email != '' GROUP BY 1"
    ):
        email_confidence[row[0]] = row[1]

    # Jobs stats
    total_jobs = c.execute("SELECT COUNT(*) FROM jobs").fetchone()[0]
    done_jobs = c.execute("SELECT COUNT(*) FROM jobs WHERE status = 'done'").fetchone()[0]
    failed_jobs = c.execute("SELECT COUNT(*) FROM jobs WHERE status = 'failed'").fetchone()[0]

    return {
        "total_leads": total,
        "leads_this_week": this_week,
        "leads_this_month": this_month,
        "avg_score": avg_score,
        "tiers": tiers,
        "enrichment": {
            "total": total,
            "with_email": with_email,
            "with_phone": with_phone,
            "with_website": with_website,
            "with_contact": with_contact,
            "with_linkedin": with_linkedin,
            "email_pct": round(with_email / total * 100, 1) if total else 0,
            "phone_pct": round(with_phone / total * 100, 1) if total else 0,
            "website_pct": round(with_website / total * 100, 1) if total else 0,
            "contact_pct": round(with_contact / total * 100, 1) if total else 0,
        },
        "email_confidence": email_confidence,
        "jobs": {
            "total": total_jobs,
            "completed": done_jobs,
            "failed": failed_jobs,
            "success_rate": round(done_jobs / total_jobs * 100, 1) if total_jobs else 0,
        },
    }


# ── Pipeline Funnel ──────────────────────────────────────────────

@router.get("/pipeline")
async def analytics_pipeline():
    """Lead pipeline funnel — statuses and conversion."""
    db = _db()
    c = db.conn.cursor()

    # Status distribution
    statuses = {}
    for row in c.execute("SELECT status, COUNT(*) FROM leads GROUP BY status ORDER BY COUNT(*) DESC"):
        statuses[row[0]] = row[1]

    # Status + tier breakdown
    status_tiers = {}
    for row in c.execute(
        "SELECT status, score_tier, COUNT(*) FROM leads GROUP BY status, score_tier"
    ):
        if row[0] not in status_tiers:
            status_tiers[row[0]] = {}
        status_tiers[row[0]][row[1]] = row[2]

    return {
        "statuses": statuses,
        "status_tiers": status_tiers,
    }


# ── Collection Trends ────────────────────────────────────────────

@router.get("/collection")
async def analytics_collection():
    """Collection job trends over last 30 days."""
    db = _db()
    c = db.conn.cursor()

    # Jobs per day (last 30 days)
    cutoff = (datetime.now(timezone.utc) - timedelta(days=30)).strftime("%Y-%m-%d")
    jobs_by_day = []
    for row in c.execute("""
        SELECT DATE(created_at) as day, COUNT(*) as count,
               SUM(leads_found) as leads,
               SUM(CASE WHEN status = 'done' THEN 1 ELSE 0 END) as completed
        FROM jobs
        WHERE created_at >= ?
        GROUP BY DATE(created_at)
        ORDER BY day
    """, (cutoff,)):
        jobs_by_day.append({
            "day": row[0],
            "jobs": row[1],
            "leads": row[2] or 0,
            "completed": row[3],
        })

    # Leads per day (last 30 days)
    leads_by_day = []
    for row in c.execute("""
        SELECT DATE(created_at) as day, COUNT(*) as count
        FROM leads
        WHERE created_at >= ?
        GROUP BY DATE(created_at)
        ORDER BY day
    """, (cutoff,)):
        leads_by_day.append({"day": row[0], "count": row[1]})

    # Avg leads per job
    avg_row = c.execute(
        "SELECT AVG(leads_found) FROM jobs WHERE status = 'done' AND leads_found > 0"
    ).fetchone()
    avg_leads_per_job = round(avg_row[0] or 0, 1)

    return {
        "jobs_by_day": jobs_by_day,
        "leads_by_day": leads_by_day,
        "avg_leads_per_job": avg_leads_per_job,
    }


# ── Enrichment Quality ──────────────────────────────────────────

@router.get("/enrichment")
async def analytics_enrichment():
    """Enrichment quality and source comparison."""
    db = _db()
    c = db.conn.cursor()

    # Source quality — avg score by source
    source_quality = []
    for row in c.execute("""
        SELECT source, COUNT(*) as count, ROUND(AVG(score), 1) as avg_score,
               SUM(CASE WHEN email != '' THEN 1 ELSE 0 END) as with_email,
               SUM(CASE WHEN phone != '' THEN 1 ELSE 0 END) as with_phone,
               SUM(CASE WHEN contact_person != '' THEN 1 ELSE 0 END) as with_contact
        FROM leads
        GROUP BY source
        ORDER BY count DESC
        LIMIT 20
    """):
        source_quality.append({
            "source": row[0] or "unknown",
            "count": row[1],
            "avg_score": row[2],
            "with_email": row[3],
            "with_phone": row[4],
            "with_contact": row[5],
        })

    # City distribution (top 15)
    by_city = []
    for row in c.execute("""
        SELECT city, COUNT(*) as count, ROUND(AVG(score), 1) as avg_score
        FROM leads WHERE city != ''
        GROUP BY city ORDER BY count DESC LIMIT 15
    """):
        by_city.append({"city": row[0], "count": row[1], "avg_score": row[2]})

    # Score distribution histogram (buckets of 10)
    score_dist = []
    for row in c.execute("""
        SELECT
            CASE
                WHEN score >= 90 THEN '90-100'
                WHEN score >= 80 THEN '80-89'
                WHEN score >= 70 THEN '70-79'
                WHEN score >= 60 THEN '60-69'
                WHEN score >= 50 THEN '50-59'
                WHEN score >= 40 THEN '40-49'
                WHEN score >= 30 THEN '30-39'
                WHEN score >= 20 THEN '20-29'
                WHEN score >= 10 THEN '10-19'
                ELSE '0-9'
            END as bucket, COUNT(*)
        FROM leads
        GROUP BY bucket
        ORDER BY MIN(score) DESC
    """):
        score_dist.append({"range": row[0], "count": row[1]})

    return {
        "source_quality": source_quality,
        "by_city": by_city,
        "score_distribution": score_dist,
    }


# ── LLM Usage ────────────────────────────────────────────────────

@router.get("/llm")
async def analytics_llm():
    """LLM token usage and cost trends."""
    db = _db()
    c = db.conn.cursor()

    # Usage by day (last 30 days)
    cutoff = (datetime.now(timezone.utc) - timedelta(days=30)).strftime("%Y-%m-%d")
    by_day = []
    for row in c.execute("""
        SELECT date, SUM(total_tokens) as tokens, SUM(calls) as calls,
               GROUP_CONCAT(DISTINCT provider) as providers
        FROM llm_usage
        WHERE date >= ?
        GROUP BY date ORDER BY date
    """, (cutoff,)):
        by_day.append({
            "day": row[0],
            "tokens": row[1] or 0,
            "calls": row[2] or 0,
            "providers": row[3] or "",
        })

    # By provider (totals)
    by_provider = []
    for row in c.execute("""
        SELECT provider, SUM(total_tokens) as tokens, SUM(calls) as calls
        FROM llm_usage GROUP BY provider ORDER BY tokens DESC
    """):
        by_provider.append({
            "provider": row[0],
            "tokens": row[1] or 0,
            "calls": row[2] or 0,
        })

    # Totals
    totals = c.execute(
        "SELECT SUM(total_tokens), SUM(calls) FROM llm_usage"
    ).fetchone()

    return {
        "by_day": by_day,
        "by_provider": by_provider,
        "total_tokens": totals[0] or 0,
        "total_calls": totals[1] or 0,
    }
