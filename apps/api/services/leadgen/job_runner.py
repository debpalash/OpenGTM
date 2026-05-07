"""
Job Runner — Background processor for collection queries.

Enhanced with:
- Parallel execution via asyncio.gather() (3x faster)
- Lead validation gate (rejects article titles, placeholders)
- Real business name extraction from fetched pages
- Real-time SSE progress with lead_discovered events
- Workspace support for campaign organization
"""

import asyncio
import json
import re
import uuid
from datetime import datetime, timezone
from typing import Optional
from urllib.parse import urlparse

from apps.api.services.leadgen.db import LeadDB
from apps.api.services.leadgen.http import StealthClient
from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.proxy_pool import ProxyPool
from apps.api.services.leadgen.rate_limiter import RateLimiter
from apps.api.services.leadgen.pipeline import deduplicate_leads
from apps.api.services.leadgen.scoring import score_leads
from apps.api.services.leadgen.progress import progress
from apps.api.services.leadgen.lead_validator import validate_lead, validate_and_clean_leads, validate_and_clean_leads_light
from apps.api.services.leadgen.llm import LLMClient
from apps.api.services.leadgen.ai_stages import (
    ai_expand_query, ai_extract_company, ai_score_leads,
    clean_page_text,
)


def _ddg_text_sync(query: str, max_results: int = 15) -> list:
    """Synchronous DDG text search — meant to be called via asyncio.to_thread."""
    from ddgs import DDGS
    from apps.api.services.leadgen.proxy_client import get_ddgs
    with get_ddgs() as ddgs:
        return list(ddgs.text(query, max_results=max_results))


async def _ddg_search(query: str, max_results: int = 15) -> list:
    """Async DDG search that doesn't block the event loop."""
    try:
        return await asyncio.to_thread(_ddg_text_sync, query, max_results)
    except Exception:
        return []


class JobRunner:
    """Processes collection jobs with parallel strategies and quality validation."""

    _cancelled: set[str] = set()  # class-level cancel registry

    def __init__(self, db: Optional[LeadDB] = None):
        self.db = db or LeadDB()
        self.proxy_pool = ProxyPool()
        self.rate_limiter = RateLimiter()
        self.client = StealthClient(
            proxy_pool=self.proxy_pool,
            rate_limiter=self.rate_limiter,
        )
        self.llm = LLMClient()

    def _is_cancelled(self, job_id: str) -> bool:
        """Check if job was cancelled (checks both memory flag and DB)."""
        if job_id in self._cancelled:
            return True
        # Also check DB in case cancel came from API
        try:
            row = self.db.conn.execute(
                "SELECT status FROM jobs WHERE id = ?", (job_id,)
            ).fetchone()
            if row and row["status"] in ("cancelled",):
                self._cancelled.add(job_id)
                return True
        except Exception:
            pass
        return False

    async def submit(self, query: str, workspace_id: str = "") -> str:
        """Submit a new collection job and process it."""
        job_id = str(uuid.uuid4())[:8]
        self.db.create_job(job_id, query)
        if workspace_id:
            self.db.conn.execute(
                "UPDATE jobs SET workspace_id = ? WHERE id = ?",
                (workspace_id, job_id)
            )
            self.db.conn.commit()
        progress.emit("job_created", {"job_id": job_id, "query": query, "workspace_id": workspace_id})
        await self._process_job({"id": job_id, "query": query, "tier": 1, "workspace_id": workspace_id})
        return job_id

    async def process_pending(self):
        """Process all pending jobs in the queue."""
        while True:
            job = self.db.claim_job()
            if not job:
                break
            progress.emit("job_started", {"job_id": job["id"], "query": job["query"]})
            await self._process_job(job)

    async def _process_job(self, job: dict):
        """Execute a single collection job with parallel strategies."""
        job_id = job["id"]
        query = job["query"]
        workspace_id = job.get("workspace_id", "")

        try:
            # Mark job as running
            from datetime import datetime
            self.db.conn.execute(
                "UPDATE jobs SET status = 'running', started_at = ?, attempts = attempts + 1 WHERE id = ?",
                (datetime.now(timezone.utc).isoformat(), job_id)
            )
            self.db.conn.commit()

            progress.emit("job_started", {
                "job_id": job_id, "query": query,
                "message": f"🚀 Starting collection: '{query}'",
            })

            # ── Run all 6 strategies in PARALLEL ─────────────────
            is_location = self._is_location_query(query)

            # Check which sources are enabled
            from apps.api.routers.settings import get_source_enabled

            tasks = []

            # Maps task (only for location queries)
            if is_location and get_source_enabled("google_maps"):
                tasks.append(("maps", self._search_maps(job_id, query)))
            else:
                tasks.append(("maps", self._noop_strategy("maps")))

            # Web search task
            if get_source_enabled("duckduckgo"):
                tasks.append(("web", self._search_and_scrape(job_id, query)))
            else:
                tasks.append(("web", self._noop_strategy("web")))

            # Directory task
            if get_source_enabled("directories"):
                tasks.append(("directories", self._search_directories(job_id, query)))
            else:
                tasks.append(("directories", self._noop_strategy("directories")))

            # LinkedIn company discovery
            if get_source_enabled("linkedin"):
                tasks.append(("linkedin", self._search_linkedin(job_id, query)))
            else:
                tasks.append(("linkedin", self._noop_strategy("linkedin")))

            # Job boards (companies actively hiring = buying signal)
            if get_source_enabled("job_boards"):
                tasks.append(("job_boards", self._search_job_boards(job_id, query)))
            else:
                tasks.append(("job_boards", self._noop_strategy("job_boards")))

            # Review sites (AmbitionBox — established employers)
            if get_source_enabled("ambitionbox"):
                tasks.append(("review_sites", self._search_review_sites(job_id, query)))
            else:
                tasks.append(("review_sites", self._noop_strategy("review_sites")))

            # Registry sources — 30+ directories, B2B marketplaces, startup trackers
            if get_source_enabled("directories"):
                tasks.append(("registry_sources", self._search_registry_sources(job_id, query)))

            progress.emit("job_progress", {
                "job_id": job_id, "stage": "parallel",
                "message": f"⚡ Running {len(tasks)} strategies in parallel...",
            })

            # ── Pre-create all stages so UI shows them as "running" immediately ──
            stage_ids = {}
            for stage_name, _ in tasks:
                sid = self.db.create_stage(job_id, stage_name)
                stage_ids[stage_name] = sid

            # Execute all in parallel with per-strategy timeout (3 min each)
            STRATEGY_TIMEOUT = 180

            async def _run_strategy(stage_name: str, coro):
                """Wrap each strategy with timeout and immediately persist results."""
                sid = stage_ids[stage_name]
                try:
                    result = await asyncio.wait_for(coro, timeout=STRATEGY_TIMEOUT)
                    if isinstance(result, list):
                        samples = [l.company for l in result[:10]]
                        self.db.complete_stage(sid, output_count=len(result),
                            details=json.dumps({"samples": samples}))
                        progress.emit("job_progress", {
                            "job_id": job_id, "stage": stage_name,
                            "message": f"✅ {stage_name}: {len(result)} leads",
                            "leads_found": len(result),
                        })
                        return result
                    else:
                        self.db.complete_stage(sid, status="skipped")
                        return []
                except asyncio.TimeoutError:
                    self.db.complete_stage(sid, status="failed",
                        details=json.dumps({"error": f"Timed out after {STRATEGY_TIMEOUT}s"}))
                    progress.emit("job_progress", {
                        "job_id": job_id, "stage": stage_name,
                        "message": f"⚠️ {stage_name} timed out",
                    })
                    return []
                except Exception as e:
                    self.db.complete_stage(sid, status="failed",
                        details=json.dumps({"error": str(e)}))
                    progress.emit("job_progress", {
                        "job_id": job_id, "stage": stage_name,
                        "message": f"⚠️ {stage_name} failed: {e}",
                    })
                    return []

            results = await asyncio.gather(
                *[_run_strategy(name, coro) for name, coro in tasks],
                return_exceptions=True,
            )

            # Collect leads from all strategies
            all_leads = []
            for result in results:
                if isinstance(result, list):
                    all_leads.extend(result)

            # ── Check cancellation ────────────────────────────────
            if self._is_cancelled(job_id):
                progress.emit("job_progress", {"job_id": job_id, "stage": "cancelled", "message": "🛑 Job cancelled by user"})
                return

            # ── Light Validate: reject garbage names only ─────────
            # (keeps leads without contact data — they'll be enriched)
            validate_sid = self.db.create_stage(job_id, "validate")
            progress.emit("job_progress", {
                "job_id": job_id, "stage": "validate",
                "message": f"🔍 Light-validating {len(all_leads)} leads (names only)...",
            })
            valid_leads, rejected = validate_and_clean_leads_light(all_leads)
            reasons = {}
            if rejected:
                for _, reason in rejected:
                    reasons[reason] = reasons.get(reason, 0) + 1
                progress.emit("job_progress", {
                    "job_id": job_id, "stage": "validate",
                    "message": f"🗑️ Rejected {len(rejected)} garbage: {dict(reasons)}",
                })
            self.db.complete_stage(validate_sid,
                input_count=len(all_leads), output_count=len(valid_leads),
                rejected_count=len(rejected),
                details=json.dumps({"reasons": reasons,
                    "rejected_names": [l.company for l, _ in rejected[:20]]}))

            # ── Deduplicate (in-batch + cross-job) ────────────────
            dedup_sid = self.db.create_stage(job_id, "dedup")
            unique = deduplicate_leads(valid_leads)

            # Cross-job dedup: remove leads already in the database
            existing_names = set()
            try:
                existing = self.db.get_leads(limit=10000)
                existing_names = {l.company.lower().strip() for l in existing if l.company}
            except Exception:
                pass
            if existing_names:
                pre_count = len(unique)
                dedup_removed_names = [l.company for l in unique if l.company.lower().strip() in existing_names]
                unique = [l for l in unique if l.company.lower().strip() not in existing_names]
                cross_removed = pre_count - len(unique)
            else:
                cross_removed = 0
                dedup_removed_names = []

            # Names removed in in-batch dedup
            unique_set = {l.company.lower().strip() for l in unique}
            batch_removed = [l.company for l in valid_leads if l.company.lower().strip() not in unique_set and l.company not in dedup_removed_names]

            all_dedup_rejected = (batch_removed + dedup_removed_names)[:20]

            self.db.complete_stage(dedup_sid,
                input_count=len(valid_leads), output_count=len(unique),
                rejected_count=len(valid_leads) - len(unique),
                details=json.dumps({"cross_job_removed": cross_removed,
                    "rejected_names": all_dedup_rejected}))
            progress.emit("job_progress", {
                "job_id": job_id, "stage": "dedup",
                "message": f"🔄 {len(valid_leads)} → {len(unique)} after dedup ({cross_removed} already in DB)",
            })

            if self._is_cancelled(job_id):
                progress.emit("job_progress", {"job_id": job_id, "stage": "cancelled", "message": "🛑 Job cancelled by user"})
                return

            # ── AI Score + heuristic fallback ─────────────────────
            score_sid = self.db.create_stage(job_id, "score")
            progress.emit("job_progress", {
                "job_id": job_id, "stage": "score",
                "message": f"🤖 AI scoring {len(unique)} leads...",
            })
            ai_used = False
            try:
                from apps.api.services.leadgen.config import ICP
                scored = await ai_score_leads(self.llm, unique, ICP)
                ai_used = True
            except Exception as e:
                progress.emit("job_progress", {
                    "job_id": job_id, "stage": "score",
                    "message": f"⚠️ AI scoring failed, using heuristic: {e}",
                })
                scored = score_leads(unique)

            # Always apply heuristic as fallback for any leads that scored 0
            zero_scored = [l for l in scored if l.score == 0]
            if zero_scored:
                progress.emit("job_progress", {
                    "job_id": job_id, "stage": "score",
                    "message": f"📊 Heuristic fallback for {len(zero_scored)} zero-scored leads...",
                })
                score_leads(zero_scored)  # mutates in-place

            tiers = {}
            for l in scored:
                t = l.score_tier or "unknown"
                tiers[t] = tiers.get(t, 0) + 1
            self.db.complete_stage(score_sid,
                input_count=len(unique), output_count=len(scored),
                details=json.dumps({"tiers": tiers, "ai": ai_used,
                    "tokens": self.llm.usage.to_dict() if ai_used else {}}))

            # ── Enrich: discover websites for leads without one ──
            enrich_sid = self.db.create_stage(job_id, "enrich")
            progress.emit("job_progress", {
                "job_id": job_id, "stage": "enrich",
                "message": f"🌐 Enriching {len(scored)} leads...",
            })

            # Step 1: DDG website discovery for leads missing a website
            needs_website = [l for l in scored if not l.website or "linkedin.com" in (l.website or "")]
            if needs_website:
                progress.emit("job_progress", {
                    "job_id": job_id, "stage": "enrich",
                    "message": f"🔍 Finding websites for {len(needs_website)} leads...",
                })
                for lead in needs_website[:15]:  # Cap at 15 to avoid rate limits
                    try:
                        search_q = f'"{lead.company}" official website'
                        if lead.city:
                            search_q += f' {lead.city}'
                        results = await _ddg_search(search_q, max_results=3)
                        for r in results:
                            href = r.get("href", "")
                            if not href:
                                continue
                            domain = urlparse(href).netloc.lower()
                            if not self._is_skip_domain(domain):
                                lead.website = href
                                # Also try to extract emails/phones from the page
                                try:
                                    resp = await self.client.fetch(href, tier=2, timeout=8)
                                    if resp.ok:
                                        found_emails = self._extract_emails_from_html(resp.text)
                                        found_phones = self._extract_phones_from_html(resp.text)
                                        if found_emails and not lead.email:
                                            lead.email = found_emails[0]
                                        if found_phones and not lead.phone:
                                            lead.phone = found_phones[0]
                                except Exception:
                                    pass
                                break
                        await asyncio.sleep(0.5)
                    except Exception:
                        continue

            # Step 2: Website enrichment (scrape contact/about pages)
            try:
                from apps.api.services.leadgen.enrichment.website_scraper import enrich_leads_from_websites
                scored = await enrich_leads_from_websites(scored, batch_size=5)
            except Exception as e:
                progress.emit("job_progress", {
                    "job_id": job_id, "stage": "enrich",
                    "message": f"⚠️ Website enrichment error: {e}",
                })
            self.db.complete_stage(enrich_sid,
                input_count=len(scored), output_count=len(scored))

            # ── Post-enrichment validation: reject leads still without contact ──
            post_val_sid = self.db.create_stage(job_id, "post_validate")
            pre_count = len(scored)
            scored_valid, post_rejected = validate_and_clean_leads(scored)
            post_reasons = {}
            if post_rejected:
                for _, reason in post_rejected:
                    post_reasons[reason] = post_reasons.get(reason, 0) + 1
                progress.emit("job_progress", {
                    "job_id": job_id, "stage": "post_validate",
                    "message": f"🔍 Post-enrichment: removed {len(post_rejected)} leads still without contact data",
                })
            scored = scored_valid
            self.db.complete_stage(post_val_sid,
                input_count=pre_count, output_count=len(scored),
                rejected_count=len(post_rejected),
                details=json.dumps({"reasons": post_reasons}))

            # ── Decision Makers ───────────────────────────────────
            dm_sid = self.db.create_stage(job_id, "decision_makers")
            progress.emit("job_progress", {
                "job_id": job_id, "stage": "decision_makers",
                "message": f"👤 Finding decision makers for {len(scored)} leads...",
            })
            try:
                from apps.api.services.leadgen.enrichment.decision_maker_finder import enrich_decision_makers
                # Find DMs for all leads that have a website (lowered from score >= 40)
                dm_candidates = [l for l in scored if l.has_website][:15]
                if dm_candidates:
                    await enrich_decision_makers(dm_candidates, concurrency=2, max_contacts=2)
                    dm_found = sum(1 for l in dm_candidates if l.decision_makers)
                    progress.emit("job_progress", {
                        "job_id": job_id, "stage": "decision_makers",
                        "message": f"👤 Found decision makers for {dm_found}/{len(dm_candidates)} leads",
                    })
            except Exception as e:
                progress.emit("job_progress", {
                    "job_id": job_id, "stage": "decision_makers",
                    "message": f"⚠️ Decision maker search error: {e}",
                })
            self.db.complete_stage(dm_sid,
                input_count=len(scored), output_count=len(scored))

            # ── Personal Emails ───────────────────────────────────
            pe_sid = self.db.create_stage(job_id, "personal_emails")
            progress.emit("job_progress", {
                "job_id": job_id, "stage": "personal_emails",
                "message": f"📧 Finding personal emails...",
            })
            try:
                from apps.api.services.leadgen.enrichment.email_finder import enrich_personal_emails
                scored = enrich_personal_emails(scored, delay=1.0)
                pe_found = sum(1 for l in scored if l.email_confidence in ("verified", "pattern"))
                progress.emit("job_progress", {
                    "job_id": job_id, "stage": "personal_emails",
                    "message": f"📧 Found {pe_found} personal emails",
                })
            except Exception as e:
                progress.emit("job_progress", {
                    "job_id": job_id, "stage": "personal_emails",
                    "message": f"⚠️ Personal email search error: {e}",
                })
            pe_count = sum(1 for l in scored if l.email_confidence in ("verified", "pattern", "smtp_verified"))
            self.db.complete_stage(pe_sid,
                input_count=len(scored), output_count=pe_count,
                details=json.dumps({"verified": sum(1 for l in scored if l.email_confidence == "verified"),
                                    "pattern": sum(1 for l in scored if l.email_confidence == "pattern"),
                                    "smtp_verified": sum(1 for l in scored if l.email_confidence == "smtp_verified")}))

            # ── CrossLinked: LinkedIn People Discovery ─────────────
            cl_sid = self.db.create_stage(job_id, "crosslinked")
            progress.emit("job_progress", {
                "job_id": job_id, "stage": "crosslinked",
                "message": f"🔗 Finding LinkedIn people...",
            })
            try:
                from apps.api.services.leadgen.enrichment.providers.crosslinked import CrossLinkedProvider
                cl_provider = CrossLinkedProvider(max_people=3, delay=1.5)
                cl_found = 0
                # Only enrich top leads that don't already have decision makers
                cl_candidates = [l for l in scored if l.score >= 40 and not l.decision_makers][:10]
                for lead in cl_candidates:
                    try:
                        result = await cl_provider.enrich(lead)
                        if result.success:
                            if result.fields.get("decision_makers"):
                                lead.decision_makers = result.fields["decision_makers"]
                            if result.fields.get("contact_person") and not lead.contact_person:
                                lead.contact_person = result.fields["contact_person"]
                            if result.fields.get("contact_title") and not lead.contact_title:
                                lead.contact_title = result.fields["contact_title"]
                            cl_found += 1
                        await asyncio.sleep(0.5)
                    except Exception:
                        continue
                progress.emit("job_progress", {
                    "job_id": job_id, "stage": "crosslinked",
                    "message": f"🔗 Found LinkedIn people for {cl_found}/{len(cl_candidates)} leads",
                })
            except Exception as e:
                progress.emit("job_progress", {
                    "job_id": job_id, "stage": "crosslinked",
                    "message": f"⚠️ CrossLinked error: {e}",
                })
            self.db.complete_stage(cl_sid,
                input_count=len(scored), output_count=len(scored))

            # ── JobSpy: Hiring Signal Detection ────────────────────
            js_sid = self.db.create_stage(job_id, "hiring_signals")
            progress.emit("job_progress", {
                "job_id": job_id, "stage": "hiring_signals",
                "message": f"📊 Detecting hiring signals...",
            })
            try:
                from apps.api.services.leadgen.enrichment.providers.jobspy_signals import JobSpySignalProvider
                js_provider = JobSpySignalProvider(max_jobs=3, delay=1.5)
                js_found = 0
                js_candidates = [l for l in scored if l.score >= 50][:8]
                for lead in js_candidates:
                    try:
                        result = await js_provider.enrich(lead)
                        if result.success and result.fields.get("hiring_signals"):
                            lead.hiring_signals = result.fields["hiring_signals"]
                            # Apply score boost from hiring signals
                            import json as _json
                            try:
                                signals = _json.loads(lead.hiring_signals)
                                boost = signals.get("score_boost", 0)
                                lead.score = min(100, lead.score + boost)
                            except Exception:
                                pass
                            js_found += 1
                        await asyncio.sleep(0.5)
                    except Exception:
                        continue
                progress.emit("job_progress", {
                    "job_id": job_id, "stage": "hiring_signals",
                    "message": f"📊 Found hiring signals for {js_found}/{len(js_candidates)} leads",
                })
            except Exception as e:
                progress.emit("job_progress", {
                    "job_id": job_id, "stage": "hiring_signals",
                    "message": f"⚠️ JobSpy error: {e}",
                })
            self.db.complete_stage(js_sid,
                input_count=len(scored), output_count=len(scored))

            # ── SMTP Email Verification ────────────────────────────
            smtp_sid = self.db.create_stage(job_id, "smtp_verify")
            progress.emit("job_progress", {
                "job_id": job_id, "stage": "smtp_verify",
                "message": f"✉️ Verifying emails via SMTP...",
            })
            try:
                from apps.api.services.leadgen.enrichment.providers.mailscout_verify import MailScoutVerifyProvider
                smtp_provider = MailScoutVerifyProvider(timeout=5, max_verify=2)
                smtp_verified_count = 0
                # Only verify top leads with emails not yet SMTP-verified
                smtp_candidates = [l for l in scored
                    if l.email and '@' in l.email
                    and l.email_confidence != "smtp_verified"
                    and l.score >= 40][:10]
                for lead in smtp_candidates:
                    try:
                        result = await smtp_provider.enrich(lead)
                        if result.success:
                            if result.fields.get("email"):
                                lead.email = result.fields["email"]
                            if result.fields.get("email_confidence"):
                                lead.email_confidence = result.fields["email_confidence"]
                                lead.email_provider = "mailscout"
                            if lead.email_confidence == "smtp_verified":
                                smtp_verified_count += 1
                    except Exception:
                        continue
                progress.emit("job_progress", {
                    "job_id": job_id, "stage": "smtp_verify",
                    "message": f"✉️ SMTP verified {smtp_verified_count}/{len(smtp_candidates)} emails",
                })
            except Exception as e:
                progress.emit("job_progress", {
                    "job_id": job_id, "stage": "smtp_verify",
                    "message": f"⚠️ SMTP verification error: {e}",
                })
            smtp_total = sum(1 for l in scored if l.email_confidence == "smtp_verified")
            self.db.complete_stage(smtp_sid,
                input_count=len(scored), output_count=smtp_total,
                details=json.dumps({"smtp_verified": smtp_total}))

            # ── Store ────────────────────────────────────────────
            store_sid = self.db.create_stage(job_id, "store")
            count = 0
            for lead in scored:
                lead.source = f"job:{job_id}"
                lead.workspace_id = workspace_id
                self.db.upsert_lead(lead)
                count += 1

                # Stream each stored lead to UI
                progress.emit("lead_stored", {
                    "job_id": job_id,
                    "lead": {
                        "company": lead.company,
                        "email": lead.email,
                        "phone": lead.phone,
                        "city": lead.city,
                        "score": lead.score,
                        "score_tier": lead.score_tier,
                    },
                })

            self.db.complete_stage(store_sid,
                input_count=len(scored), output_count=count)

            self.db.complete_job(job_id, leads_found=count)
            progress.emit("job_completed", {
                "job_id": job_id, "query": query,
                "leads_found": count, "raw_total": len(all_leads),
                "rejected": len(rejected),
                "message": f"✅ Done! {count} quality leads stored ({len(rejected)} rejected)",
            })

        except Exception as e:
            self.db.fail_job(job_id, str(e))
            progress.emit("job_failed", {
                "job_id": job_id, "error": str(e),
                "message": f"❌ Failed: {e}",
            })

    async def _noop_strategy(self, name: str) -> list[Lead]:
        """Placeholder for skipped strategies."""
        return []

    # ── Strategy: Google Maps ────────────────────────────────────────

    async def _search_maps(self, job_id: str, query: str) -> list[Lead]:
        """Search Google Maps via the existing scraper."""
        progress.emit("job_progress", {
            "job_id": job_id, "stage": "maps",
            "message": f"🗺️ Searching Google Maps...",
        })
        try:
            from apps.api.services.leadgen.scrapers.google_maps import scrape_google_maps
            city = self._extract_city(query)
            if city:
                clean_query = query.lower().replace(city.lower(), "").strip()
                leads = await scrape_google_maps(clean_query or query, city, max_results=20)
                for lead in leads:
                    progress.emit("lead_discovered", {
                        "job_id": job_id, "stage": "maps",
                        "lead": {"company": lead.company, "email": lead.email, "phone": lead.phone},
                    })
                return leads
        except Exception as e:
            progress.emit("job_progress", {
                "job_id": job_id, "stage": "maps",
                "message": f"⚠️ Maps error: {e}",
            })
        return []

    # ── Strategy: Web Search ─────────────────────────────────────────

    async def _search_and_scrape(self, job_id: str, query: str) -> list[Lead]:
        """Search the web and extract company data.

        Uses regex-first approach: extract emails/phones from HTML directly.
        AI extraction is only used on the first few high-value pages to stay
        within rate limits and avoid timeouts.
        """
        leads = []
        city = self._extract_city(query)
        AI_BUDGET = 3  # Max pages to run through AI extraction
        ai_used = 0

        # AI-powered query expansion (with fast fallback)
        try:
            expanded = await asyncio.wait_for(
                ai_expand_query(self.llm, query), timeout=15
            )
            progress.emit("job_progress", {
                "job_id": job_id, "stage": "web",
                "message": f"🤖 AI generated {len(expanded)} search queries",
            })
        except Exception:
            expanded = self._expand_query(query)

        progress.emit("job_progress", {
            "job_id": job_id, "stage": "web",
            "message": f"🔍 Web search: {len(expanded)} queries...",
        })

        seen_domains = set()

        try:
            for i, q in enumerate(expanded):
                progress.emit("job_progress", {
                    "job_id": job_id, "stage": "web",
                    "message": f"🔎 [{i+1}/{len(expanded)}] '{q}'",
                })
                try:
                    results = await _ddg_search(q, max_results=15)

                    for result in results:
                        url = result.get("href", "")
                        title = result.get("title", "")
                        body = result.get("body", "")
                        if not url:
                            continue

                        domain = urlparse(url).netloc.lower()
                        base_domain = ".".join(domain.split(".")[-2:])

                        # Skip aggregators, social media, already seen
                        if self._is_skip_domain(domain):
                            continue
                        if base_domain in seen_domains:
                            continue
                        seen_domains.add(base_domain)

                        # Fetch the page
                        try:
                            resp = await self.client.fetch(url, tier=2, timeout=8)
                            if not resp.ok:
                                continue

                            html = resp.text

                            # ── Regex-first extraction ───────────────
                            emails = self._extract_emails_from_html(html)
                            phones = self._extract_phones_from_html(html)
                            company_name = self._company_from_domain(domain)

                            # If regex found contact data, use it directly (fast path)
                            if company_name and len(company_name) >= 3 and (emails or phones):
                                lead = Lead(
                                    company=company_name,
                                    website=url,
                                    email=emails[0] if emails else "",
                                    phone=phones[0] if phones else "",
                                    city=city or "",
                                    description=body[:300],
                                    specialization=query,
                                    secondary_emails=", ".join(emails[1:4]) if len(emails) > 1 else "",
                                    secondary_phones=", ".join(phones[1:3]) if len(phones) > 1 else "",
                                    source="web_search",
                                )
                                leads.append(lead)
                                progress.emit("lead_discovered", {
                                    "job_id": job_id, "stage": "web",
                                    "lead": {"company": lead.company, "email": lead.email, "phone": lead.phone},
                                })

                            # For pages without regex hits, try AI (within budget)
                            elif ai_used < AI_BUDGET:
                                try:
                                    ai_data = await asyncio.wait_for(
                                        ai_extract_company(self.llm, html, url, query),
                                        timeout=20,
                                    )
                                    ai_used += 1

                                    if ai_data and ai_data.get("is_real_company", True):
                                        ai_name = ai_data.get("company_name", "")
                                        if not ai_name or len(ai_name) < 3:
                                            ai_name = company_name
                                        if ai_name and len(ai_name) >= 3:
                                            lead = Lead(
                                                company=ai_name,
                                                website=url,
                                                email=ai_data.get("email") or "",
                                                phone=ai_data.get("phone") or "",
                                                city=ai_data.get("city") or city or "",
                                                state=ai_data.get("state") or "",
                                                address=ai_data.get("address") or "",
                                                description=ai_data.get("description") or body[:300],
                                                specialization=ai_data.get("specialization") or query,
                                                industry_tags=ai_data.get("industry_tags") or "",
                                                company_size=ai_data.get("employee_count") or "",
                                                employee_count_exact=ai_data.get("employee_count_exact") or 0,
                                                founded_year=ai_data.get("founded_year") or "",
                                                technologies=ai_data.get("technologies") or "",
                                                contact_person=ai_data.get("contact_person") or "",
                                                source="web_search",
                                            )
                                            leads.append(lead)
                                            progress.emit("lead_discovered", {
                                                "job_id": job_id, "stage": "web",
                                                "lead": {"company": lead.company, "email": lead.email},
                                            })
                                except (asyncio.TimeoutError, Exception):
                                    ai_used += 1  # Count failed attempts too

                        except Exception:
                            continue

                    await asyncio.sleep(0.5)

                except Exception as e:
                    progress.emit("job_progress", {
                        "job_id": job_id, "stage": "web",
                        "message": f"⚠️ Search error: {e}",
                    })

        except Exception:
            pass

        progress.emit("job_progress", {
            "job_id": job_id, "stage": "web",
            "message": f"🔍 Web search done: {len(leads)} leads ({ai_used} AI calls)",
        })
        return leads

    def _extract_emails_from_html(self, html: str) -> list[str]:
        """Extract emails from HTML via regex, filtering out junk."""
        raw = re.findall(r'[\w.\-+]+@[\w.\-]+\.\w{2,}', html)
        seen = set()
        good = []
        skip_domains = {
            "example.com", "domain.com", "email.com", "sentry.io",
            "wixpress.com", "w3.org", "schema.org", "googleapis.com",
            "gravatar.com", "wordpress.org", "cloudflare.com",
        }
        skip_ext = {".png", ".jpg", ".svg", ".gif", ".css", ".js", ".woff", ".webp"}
        for e in raw:
            low = e.lower()
            domain = low.split("@")[-1]
            if domain in skip_domains:
                continue
            if any(low.endswith(ext) for ext in skip_ext):
                continue
            if self._is_publisher_email(e):
                continue
            if low not in seen:
                seen.add(low)
                good.append(e)
        return good[:5]

    def _extract_phones_from_html(self, html: str) -> list[str]:
        """Extract Indian phone numbers from HTML via regex."""
        patterns = [
            r'\+?91[\-\s]?\d{5}[\-\s]?\d{5}',
            r'\+?91[\-\s]?\d{10}',
            r'1800[\-\s]?\d{2,3}[\-\s]?\d{4,6}',
            r'0\d{2,4}[\-\s]?\d{6,8}',
        ]
        # Also check tel: links
        tel_pattern = r'tel:([+\d\-\s]{7,15})'
        phones = []
        seen = set()
        for pat in patterns + [tel_pattern]:
            for m in re.finditer(pat, html):
                phone = m.group(1) if m.lastindex else m.group()
                phone = phone.strip().replace("tel:", "")
                digits = re.sub(r'\D', '', phone)
                if 7 <= len(digits) <= 13 and digits not in seen:
                    if not self._is_valid_phone(phone):
                        continue
                    seen.add(digits)
                    phones.append(phone)
        return phones[:5]

    # ── Strategy: Directory Search ───────────────────────────────────

    async def _search_directories(self, job_id: str, query: str) -> list[Lead]:
        """Search business directories and extract individual company listings.

        Instead of using DDG search result titles (which are category pages),
        we follow the directory URLs and scrape individual company cards from
        listing pages like Clutch, GoodFirms, etc.
        """
        leads = []
        city = self._extract_city(query)

        progress.emit("job_progress", {
            "job_id": job_id, "stage": "directories",
            "message": f"📂 Searching directories...",
        })

        core_query = query.lower()
        if city:
            core_query = core_query.replace(city.lower(), '').replace(' in ', ' ').strip()

        directory_queries = [
            f"{query} site:clutch.co",
            f"{query} site:goodfirms.co",
            f"site:clutch.co {core_query} companies {city or 'India'}",
            f"site:goodfirms.co {core_query} companies {city or 'India'}",
            f"{query} companies list India",
            f"{query} top firms {city}" if city else f"{query} top firms India",
            f"site:justdial.com {core_query} {city}" if city else "",
        ]
        directory_queries = [q for q in directory_queries if q]  # filter empties

        try:
            for dq in directory_queries:
                try:
                    results = await _ddg_search(dq, max_results=10)

                    for result in results:
                        url = result.get("href", "")
                        title = result.get("title", "")
                        body = result.get("body", "")
                        if not url or not title:
                            continue

                        domain = urlparse(url).netloc.lower()

                        # If this is a directory listing page, try to scrape
                        # individual companies FROM the page
                        if any(d in domain for d in ["clutch.co", "goodfirms.co", "g2.com", "softwaresuggest.com"]):
                            try:
                                page_leads = await self._extract_companies_from_directory(
                                    job_id, url, domain, city, query
                                )
                                leads.extend(page_leads)
                            except Exception as e:
                                progress.emit("job_progress", {
                                    "job_id": job_id, "stage": "directories",
                                    "message": f"⚠️ Directory scrape error: {e}",
                                })
                            continue

                        # For non-directory pages, try to extract a business name
                        # but run through validation first
                        company = self._extract_business_name(title)
                        if not company or len(company) < 3:
                            continue

                        # Pre-validate before adding
                        test_lead = Lead(company=company)
                        from apps.api.services.leadgen.lead_validator import validate_lead
                        is_valid, _ = validate_lead(test_lead)
                        if not is_valid:
                            continue

                        lead = Lead(
                            company=company,
                            website=url,
                            city=city,
                            description=body[:300],
                            source="directory",
                        )
                        leads.append(lead)
                        progress.emit("lead_discovered", {
                            "job_id": job_id, "stage": "directories",
                            "lead": {"company": lead.company},
                        })

                    await asyncio.sleep(1.5)

                except Exception:
                    continue

        except ImportError:
            pass

        return leads

    async def _extract_companies_from_directory(
        self, job_id: str, url: str, domain: str, city: str, query: str
    ) -> list[Lead]:
        """Scrape a directory listing page for individual company cards.

        Each directory has its own HTML structure for company cards.
        We fetch the page and parse out company names, descriptions, etc.
        """
        leads = []
        try:
            resp = await self.client.fetch(url, tier=2, timeout=12)
            if not resp.ok:
                return []

            html = resp.text
            from bs4 import BeautifulSoup
            soup = BeautifulSoup(html, "html.parser")

            companies_found = []

            if "clutch.co" in domain:
                # Clutch: company cards use data-url and h3.company_info a
                for card in soup.select("[data-url], .provider-row, .directory-list li"):
                    name_el = card.select_one("h3 a, .company_info a, .company-name a, a[data-link_text]")
                    if name_el:
                        name = name_el.get_text(strip=True)
                        link = name_el.get("href", "")
                        if link and not link.startswith("http"):
                            link = f"https://clutch.co{link}"
                        companies_found.append((name, link))

            elif "goodfirms.co" in domain:
                # GoodFirms: company cards
                for card in soup.select(".company-profile-row, .firm-wrapper, .agency-list-card"):
                    name_el = card.select_one("a.company-name, h3 a, .profile-name a")
                    if name_el:
                        name = name_el.get_text(strip=True)
                        link = name_el.get("href", "")
                        if link and not link.startswith("http"):
                            link = f"https://www.goodfirms.co{link}"
                        companies_found.append((name, link))

            elif "g2.com" in domain:
                for card in soup.select(".product-listing__card, [data-product-name]"):
                    name = card.get("data-product-name", "")
                    if not name:
                        name_el = card.select_one("a.product-listing__product-name, h3 a")
                        name = name_el.get_text(strip=True) if name_el else ""
                    if name:
                        companies_found.append((name, ""))

            # Fallback: look for any structured company-like elements
            if not companies_found:
                # Try common card patterns
                for card in soup.select("li.company, .card, .listing-item, article"):
                    name_el = card.select_one("h2, h3, h4, .title, .name")
                    if name_el:
                        name = name_el.get_text(strip=True)
                        if name and 3 < len(name) < 60:
                            companies_found.append((name, ""))

            # Convert to leads with validation
            from apps.api.services.leadgen.lead_validator import validate_lead
            for name, website in companies_found[:30]:  # Cap at 30 per page
                name = self._clean_name(name)
                if not name or len(name) < 3:
                    continue
                test_lead = Lead(company=name)
                is_valid, _ = validate_lead(test_lead)
                if not is_valid:
                    continue

                lead = Lead(
                    company=name,
                    website=website,
                    city=city,
                    specialization=query,
                    source="directory",
                )
                leads.append(lead)
                progress.emit("lead_discovered", {
                    "job_id": job_id, "stage": "directories",
                    "lead": {"company": lead.company},
                })

        except Exception as e:
            progress.emit("job_progress", {
                "job_id": job_id, "stage": "directories",
                "message": f"⚠️ Directory page parse error: {e}",
            })

        return leads

    # ── Strategy: LinkedIn Company Discovery ─────────────────────────────

    async def _search_linkedin(self, job_id: str, query: str) -> list[Lead]:
        """Search DDG for LinkedIn company pages matching the query."""
        leads = []
        city = self._extract_city(query)

        progress.emit("job_progress", {
            "job_id": job_id, "stage": "linkedin",
            "message": f"🔗 Searching LinkedIn companies...",
        })

        try:
            linkedin_queries = [
                f'site:linkedin.com/company "{query}"',
                f'site:linkedin.com/company "{query}" {city}' if city else f'site:linkedin.com/company "{query}" India',
            ]

            seen_urls = set()
            for lq in linkedin_queries:
                try:
                    results = await _ddg_search(lq, max_results=10)

                    for r in results:
                        href = r.get("href", "")
                        title = r.get("title", "")
                        body = r.get("body", "")

                        if "/company/" not in href:
                            continue

                        # Normalize URL
                        from urllib.parse import urlparse
                        parsed = urlparse(href)
                        company_path = parsed.path.rstrip("/")
                        linkedin_url = f"https://www.linkedin.com{company_path}"

                        if linkedin_url in seen_urls:
                            continue
                        seen_urls.add(linkedin_url)

                        # Extract company name from title
                        company_name = title.split(" | ")[0].split(" - ")[0].strip()
                        if not company_name or company_name.lower() == "linkedin":
                            continue

                        # Extract employee count if mentioned
                        company_size = ""
                        size_match = re.search(r'(\d[\d,]+)\s*(?:employees|followers)', body, re.IGNORECASE)
                        if size_match:
                            count = int(size_match.group(1).replace(",", ""))
                            if count < 50:
                                company_size = "1-50"
                            elif count < 200:
                                company_size = "51-200"
                            elif count < 500:
                                company_size = "201-500"
                            else:
                                company_size = "500+"

                        lead = Lead(
                            company=company_name,
                            city=city or "India",
                            linkedin_url=linkedin_url,
                            company_size=company_size,
                            description=body[:300] if body else "",
                            specialization=query,
                            source="linkedin",
                        )
                        leads.append(lead)
                        progress.emit("lead_discovered", {
                            "job_id": job_id, "stage": "linkedin",
                            "lead": {"company": lead.company, "linkedin": linkedin_url},
                        })

                    await asyncio.sleep(1.5)
                except Exception:
                    continue

        except ImportError:
            pass

        return leads

    # ── Strategy: Job Boards ─────────────────────────────────────────────

    async def _search_job_boards(self, job_id: str, query: str) -> list[Lead]:
        """Find companies actively hiring via Naukri/Indeed — strong buying signal."""
        leads = []
        city = self._extract_city(query)

        progress.emit("job_progress", {
            "job_id": job_id, "stage": "job_boards",
            "message": f"💼 Searching job boards for employers...",
        })

        try:
            core_query = query.lower()
            if city:
                core_query = core_query.replace(city.lower(), '').replace(' in ', ' ').strip()

            board_queries = [
                f'site:naukri.com {core_query} jobs {city or "India"}',
                f'site:indeed.com {core_query} company {city or "India"}',
                f'naukri.com {core_query} employer {city or "India"}',
                f'{core_query} hiring {city or "India"} company',
                f'site:foundit.in {core_query} {city or "India"}',
            ]

            seen = set()
            skip_words = {"naukri", "indeed", "jobs", "careers", "foundit", "hiring",
                          "search", "results", "apply", "login", "register", "salary"}

            for bq in board_queries:
                try:
                    results = await _ddg_search(bq, max_results=10)

                    for r in results:
                        title = r.get("title", "")
                        body = r.get("body", "")
                        href = r.get("href", "")

                        # Try URL-based extraction first (most reliable)
                        name = ""
                        if href:
                            parsed_url = urlparse(href)
                            path = parsed_url.path.strip("/")
                            # naukri.com/company-name-jobs-123456
                            if "naukri" in parsed_url.netloc:
                                slug = path.split("/")[-1] if path else ""
                                slug = re.sub(r'-jobs?-?\d*$', '', slug)
                                slug = re.sub(r'-careers?$', '', slug)
                                name = slug.replace("-", " ").title().strip()
                            # indeed.com/cmp/Company-Name
                            elif "indeed" in parsed_url.netloc and "/cmp/" in path:
                                slug = path.split("/cmp/")[-1].split("/")[0]
                                name = slug.replace("-", " ").title().strip()

                        # Fall back to title parsing
                        if not name or len(name) < 3:
                            name = title.split(" Jobs")[0].split(" Careers")[0].split(" Hiring")[0]
                            name = name.split(" - ")[0].split(" | ")[0].strip()
                            name = re.sub(r'\s*(Pvt|Ltd|Private|Limited|India)\.?\s*$', '', name, flags=re.IGNORECASE).strip()

                        if not name or len(name) < 3:
                            continue
                        if name.lower() in skip_words or name.lower() in seen:
                            continue

                        seen.add(name.lower())

                        # Extract employee count
                        company_size = self._extract_size(body)

                        lead = Lead(
                            company=name,
                            city=city or "India",
                            company_size=company_size,
                            specialization=query,
                            description=body[:200] if body else "",
                            source="job_board",
                        )
                        leads.append(lead)
                        progress.emit("lead_discovered", {
                            "job_id": job_id, "stage": "job_boards",
                            "lead": {"company": lead.company},
                        })

                    await asyncio.sleep(1)
                except Exception:
                    continue

        except Exception:
            pass

        return leads

    # ── Strategy: Review Sites ───────────────────────────────────────────

    async def _search_review_sites(self, job_id: str, query: str) -> list[Lead]:
        """Find companies on AmbitionBox/Glassdoor — established employers."""
        leads = []
        city = self._extract_city(query)

        progress.emit("job_progress", {
            "job_id": job_id, "stage": "review_sites",
            "message": f"⭐ Searching review sites...",
        })

        try:
            core_query = query.lower()
            if city:
                core_query = core_query.replace(city.lower(), '').replace(' in ', ' ').strip()

            review_queries = [
                f'site:ambitionbox.com {core_query} {city or "India"} reviews',
                f'ambitionbox.com {core_query} company reviews {city or "India"}',
                f'site:glassdoor.co.in {core_query} {city or "India"}',
                f'{core_query} company reviews {city or "India"}',
            ]

            seen = set()
            skip_words = {"ambitionbox", "glassdoor", "reviews", "review", "salary",
                          "interview", "questions", "jobs", "login", "compare"}

            for rq in review_queries:
                try:
                    results = await _ddg_search(rq, max_results=10)

                    for r in results:
                        title = r.get("title", "")
                        body = r.get("body", "")
                        href = r.get("href", "")

                        # Try URL-based extraction (most reliable)
                        name = ""
                        if href:
                            parsed_url = urlparse(href)
                            path = parsed_url.path.strip("/")
                            # ambitionbox.com/reviews/company-name-reviews
                            if "ambitionbox" in parsed_url.netloc and "/reviews/" in path:
                                slug = path.split("/reviews/")[-1].split("/")[0]
                                slug = re.sub(r'-reviews?$', '', slug)
                                name = slug.replace("-", " ").title().strip()
                            # glassdoor.co.in/Reviews/Company-Name-Reviews-EXXXXX
                            elif "glassdoor" in parsed_url.netloc and "/Reviews/" in path:
                                slug = path.split("/Reviews/")[-1].split("/")[0]
                                slug = re.sub(r'-Reviews?-E\d+.*$', '', slug)
                                name = slug.replace("-", " ").title().strip()

                        # Fall back to title parsing
                        if not name or len(name) < 3:
                            name = title.split(" Reviews")[0].split(" Review")[0]
                            name = name.split(" | ")[0].split(" - ")[0].strip()
                            name = re.sub(r'\s*(Pvt|Ltd|Private|Limited|India)\.?\s*$', '', name, flags=re.IGNORECASE).strip()

                        if not name or len(name) < 3:
                            continue
                        if name.lower() in skip_words or name.lower() in seen:
                            continue

                        seen.add(name.lower())

                        # Extract rating
                        rating = ""
                        rating_match = re.search(r'(\d\.\d)\s*(?:/5|out of 5|stars?|rating|★)', body, re.IGNORECASE)
                        if rating_match:
                            rating = rating_match.group(1)

                        company_size = self._extract_size(body)

                        lead = Lead(
                            company=name,
                            city=city or "India",
                            company_size=company_size,
                            glassdoor_rating=rating,
                            specialization=query,
                            description=body[:200] if body else "",
                            notes=f"Review profile: {href}" if href else "",
                            source="review_site",
                        )
                        leads.append(lead)
                        progress.emit("lead_discovered", {
                            "job_id": job_id, "stage": "review_sites",
                            "lead": {"company": lead.company, "rating": rating},
                        })

                    await asyncio.sleep(1)
                except Exception:
                    continue

        except Exception:
            pass

        return leads

    # ── Strategy: Registry Sources (30+ directories) ────────────────────

    async def _search_registry_sources(self, job_id: str, query: str) -> list[Lead]:
        """Search 30+ directories/marketplaces from the source registry.

        Uses the source_registry module for data-driven source definitions.
        Runs sources in batched parallel to avoid rate limits.
        """
        from apps.api.services.leadgen.source_registry import get_all_sources, build_queries

        leads = []
        city = self._extract_city(query)

        # Get all enabled sources (auto-detect region from city/query)
        region = self._detect_region(query, city)
        sources = get_all_sources(region=region)

        # Also include global sources
        if region != "global":
            global_sources = get_all_sources(region="global")
            seen_names = {s["name"] for s in sources}
            for gs in global_sources:
                if gs["name"] not in seen_names:
                    sources.append(gs)

        # Skip sources that overlap with existing strategies
        skip_names = {"clutch", "goodfirms", "ambitionbox", "linkedin_companies"}
        sources = [s for s in sources if s["name"] not in skip_names]

        progress.emit("job_progress", {
            "job_id": job_id, "stage": "registry_sources",
            "message": f"🌐 Scanning {len(sources)} directories & marketplaces...",
        })

        # Limit to top-20 highest-priority sources to avoid timeout
        sources = sorted(sources, key=lambda s: s.get("priority", 999))[:20]

        progress.emit("job_progress", {
            "job_id": job_id, "stage": "registry_sources",
            "message": f"🌐 Narrowed to top {len(sources)} sources by priority...",
        })

        # Run in batches of 10 with 1 query per source
        BATCH_SIZE = 10
        for batch_idx in range(0, len(sources), BATCH_SIZE):
            batch = sources[batch_idx:batch_idx + BATCH_SIZE]

            async def _search_single_source(source):
                """Search a single registry source."""
                source_leads = []
                try:
                    queries = build_queries(source, query, city)
                    for dq in queries[:1]:  # Max 1 query per source (speed)
                        try:
                            results = await _ddg_search(dq, max_results=8)
                            for result in results:
                                href = result.get("href", "")
                                title = result.get("title", "")
                                body = result.get("body", "")
                                if not href or not title:
                                    continue

                                domain = urlparse(href).netloc.lower()
                                site_domain = source.get("site_domain", "")

                                # Extract company name
                                company = self._extract_business_name(title)
                                if not company or len(company) < 3:
                                    continue

                                # Validate
                                from apps.api.services.leadgen.lead_validator import validate_lead
                                test_lead = Lead(company=company)
                                is_valid, _ = validate_lead(test_lead)
                                if not is_valid:
                                    continue

                                lead = Lead(
                                    company=company,
                                    website=href if not site_domain or site_domain not in domain else "",
                                    city=city,
                                    description=body[:300],
                                    source=f"registry:{source['name']}",
                                    specialization=query,
                                )

                                # Try to extract contact info from body text
                                emails = self._extract_emails_from_html(body)
                                phones = self._extract_phones_from_html(body)
                                if emails:
                                    lead.email = emails[0]
                                if phones:
                                    lead.phone = phones[0]

                                source_leads.append(lead)

                            await asyncio.sleep(0.5)
                        except Exception:
                            continue
                except Exception as e:
                    logger.debug(f"Source {source['name']} error: {e}")
                return source_leads

            # Run batch in parallel
            batch_results = await asyncio.gather(
                *[_search_single_source(s) for s in batch],
                return_exceptions=True,
            )

            batch_count = 0
            for source, result in zip(batch, batch_results):
                if isinstance(result, list):
                    leads.extend(result)
                    batch_count += len(result)

            if batch_count > 0:
                progress.emit("job_progress", {
                    "job_id": job_id, "stage": "registry_sources",
                    "message": f"📂 Batch {batch_idx // BATCH_SIZE + 1}: +{batch_count} leads from {', '.join(s['label'] for s in batch[:3])}...",
                })

            # Brief pause between batches
            await asyncio.sleep(1)

        progress.emit("job_progress", {
            "job_id": job_id, "stage": "registry_sources",
            "message": f"🌐 Registry sources done: {len(leads)} leads from {len(sources)} sources",
        })

        return leads

    def _detect_region(self, query: str, city: str = "") -> str:
        """Detect target region from query and city."""
        text = f"{query} {city}".lower()

        indian_cities = {
            "mumbai", "delhi", "bangalore", "bengaluru", "hyderabad", "chennai",
            "kolkata", "pune", "ahmedabad", "jaipur", "lucknow", "kanpur",
            "nagpur", "indore", "thane", "bhopal", "visakhapatnam", "patna",
            "vadodara", "ghaziabad", "ludhiana", "agra", "nashik", "faridabad",
            "meerut", "rajkot", "varanasi", "srinagar", "noida", "gurgaon",
            "gurugram", "coimbatore", "kochi", "chandigarh", "mysore", "mysuru",
            "surat", "ranchi", "bhubaneswar", "tiruchirappalli", "trivandrum",
            "thiruvananthapuram", "salem", "hubli", "mangalore",
        }
        indian_keywords = {"india", "indian", "pvt ltd", "private limited", "nse", "bse"}

        us_cities = {
            "new york", "los angeles", "chicago", "houston", "phoenix", "philadelphia",
            "san antonio", "san diego", "dallas", "san jose", "austin", "seattle",
            "denver", "boston", "nashville", "portland", "las vegas", "atlanta",
            "miami", "san francisco", "charlotte", "minneapolis",
        }

        eu_keywords = {"london", "berlin", "paris", "amsterdam", "munich", "barcelona", "europe", "uk", "germany", "france"}

        if any(c in text for c in indian_cities) or any(k in text for k in indian_keywords):
            return "india"
        if any(c in text for c in us_cities) or "usa" in text or "united states" in text:
            return "us"
        if any(k in text for k in eu_keywords):
            return "eu"
        return "global"

    # ── Helpers ───────────────────────────────────────────────────────

    def _extract_size(self, text: str) -> str:
        """Extract employee count from text and bucket it."""
        size_match = re.search(r'(\d[\d,]+)\s*(?:employees|people|staff|workers)', text, re.IGNORECASE)
        if size_match:
            count = int(size_match.group(1).replace(",", ""))
            if count < 50:
                return "1-50"
            elif count < 200:
                return "51-200"
            elif count < 500:
                return "201-500"
            else:
                return "500+"
        return ""

    def _is_location_query(self, query: str) -> bool:
        """Check if query mentions a city/location."""
        try:
            from apps.api.services.leadgen.config import ICP
            for city in ICP["target_cities"]:
                if city.lower() in query.lower():
                    return True
        except Exception:
            pass
        cities = ["bangalore", "mumbai", "delhi", "hyderabad", "pune", "chennai",
                  "kolkata", "noida", "gurgaon", "ahmedabad", "bengaluru", "gurugram"]
        return any(c in query.lower() for c in cities)

    def _extract_city(self, query: str) -> str:
        """Extract city name from a query string."""
        try:
            from apps.api.services.leadgen.config import ICP
            for c in ICP["target_cities"]:
                if c.lower() in query.lower():
                    return c
        except Exception:
            pass
        cities = {"bangalore": "Bangalore", "mumbai": "Mumbai", "delhi": "Delhi",
                  "hyderabad": "Hyderabad", "pune": "Pune", "chennai": "Chennai",
                  "kolkata": "Kolkata", "noida": "Noida", "gurgaon": "Gurgaon",
                  "ahmedabad": "Ahmedabad", "bengaluru": "Bangalore", "gurugram": "Gurgaon"}
        for key, val in cities.items():
            if key in query.lower():
                return val
        return ""

    def _expand_query(self, query: str) -> list[str]:
        """Expand a single query into search variations."""
        city = self._extract_city(query)
        base = query.lower()
        if city:
            base = base.replace(city.lower(), "").strip()

        variations = [query]

        suffixes = ["companies", "agencies", "firms", "consultancies"]
        for s in suffixes:
            if s not in base:
                q = f"{base} {s}"
                if city:
                    q += f" {city}"
                variations.append(q)
                break

        variations.append(f"top {base} {city or 'India'}")
        variations.append(f"{base} list {city or 'India'} 2024")

        return variations[:4]

    def _extract_business_name(self, title: str) -> str:
        """Extract a clean business name from a search result title.

        The key insight: split on separators (| - —) and take the FIRST
        part that looks like a company name (not an article title).
        """
        if not title:
            return ""

        # Split on common title separators
        for sep in [" | ", " - ", " — ", " – ", " · "]:
            if sep in title:
                parts = title.split(sep)
                # Try each part
                for part in parts:
                    part = part.strip()
                    # Reject article-like parts
                    if re.search(r"(?i)\b(top|best|guide|list|ranking)\s+\d*", part):
                        continue
                    if re.search(r"(?i)\b20(2[3-9]|3\d)\b", part):
                        continue
                    if len(part) > 60:
                        continue
                    if len(part) < 3:
                        continue
                    return self._clean_name(part)

        # No separator — clean the whole title
        cleaned = self._clean_name(title)
        return cleaned if len(cleaned) <= 60 else ""

    def _clean_name(self, name: str) -> str:
        """Remove noise words from a company name."""
        noise = ["Reviews", "Company Profile", "LinkedIn", "Glassdoor",
                 "Clutch.co", "GoodFirms", "Careers", "Jobs", "Hiring",
                 "| Clutch", "| GoodFirms", "Company", "Profile"]
        for n in noise:
            name = name.replace(n, "")
        return name.strip()[:80]

    def _company_from_domain(self, domain: str) -> str:
        """Try to derive a company name from a domain.

        e.g., 'www.datamatics.com' → 'Datamatics'
              'talentleads.co.in' → 'Talent Leads'
              'v3staffing.in' → 'V3 Staffing'
        """
        if not domain:
            return ""

        # Remove www. prefix
        if domain.startswith("www."):
            domain = domain[4:]

        # Remove TLD
        for tld in [".co.in", ".com", ".in", ".net", ".org", ".io", ".co"]:
            if domain.endswith(tld):
                domain = domain[:-len(tld)]
                break

        # Skip if it looks like a publisher/aggregator
        if self._is_skip_domain(domain + ".com"):
            return ""

        if not domain or len(domain) < 3:
            return ""

        # Split on hyphens/underscores
        parts = re.split(r'[-_]', domain)

        # Also try to split CamelCase or concatenated words
        expanded = []
        for part in parts:
            # Insert spaces before uppercase letters in camelCase
            # e.g., "claviusSolutions" → "clavius Solutions"
            split = re.sub(r'([a-z])([A-Z])', r'\1 \2', part)
            # Split numbers from words: "v3staffing" → "v3 staffing"
            split = re.sub(r'([a-zA-Z])(\d)', r'\1 \2', split)
            split = re.sub(r'(\d)([a-zA-Z])', r'\1 \2', split)
            expanded.append(split)

        name = " ".join(expanded).title()
        return name

    def _is_skip_domain(self, domain: str) -> bool:
        """Check if a domain should be skipped (aggregators, social, directories)."""
        skip = [
            # Social media
            "wikipedia.org", "youtube.com", "facebook.com",
            "twitter.com", "instagram.com", "reddit.com",
            "quora.com", "medium.com", "linkedin.com",
            "pinterest.com", "tiktok.com",
            # Job boards
            "glassdoor.com", "glassdoor.co.in", "ambitionbox.com",
            "indeed.com", "naukri.com", "shine.com",
            "timesjobs.com", "monster.com", "foundit.in",
            # Directories / aggregators
            "clutch.co", "goodfirms.co", "g2.com", "capterra.com",
            "softwaresuggest.com", "themanifest.com", "techbehemoths.com",
            "trustpilot.com", "mouthshut.com",
            # Indian directories
            "justdial.com", "sulekha.com", "indiamart.com",
            "placementindia.com", "tradeindia.com", "exportersindia.com",
            "grotal.com", "fundoodata.com", "freelistingindia.com",
            "urbanpro.com", "dial4trade.com",
            # Lead gen tools
            "aeroleads.com", "lusha.com", "apollo.io", "zoominfo.com",
            "rocketreach.co", "clearbit.com", "snov.io", "hunter.io",
            # Startup/VC databases
            "crunchbase.com", "owler.com", "tracxn.com",
            "wellfound.com", "angellist.com", "yourstory.com",
            # News / generic
            "mordorintelligence.com", "rankexdigital.com",
        ]
        return any(s in domain for s in skip)

    def _is_publisher_email(self, email: str) -> bool:
        """Check if an email belongs to a publisher/aggregator."""
        if "@" not in email:
            return True
        domain = email.split("@")[-1].lower()
        publishers = {"softwaresuggest.com", "goodfirms.co", "clutch.co",
                      "g2.com", "capterra.com", "ambitionbox.com",
                      "glassdoor.com", "mordorintelligence.com",
                      "rankexdigital.com", "trustpilot.com"}
        return domain in publishers

    def _is_valid_phone(self, phone: str) -> bool:
        """Check if a phone number is valid for India leads."""
        clean = re.sub(r"[^\d+]", "", phone)
        # Reject US/EU numbers
        for prefix in ["+1", "+44", "+61", "+49", "+33"]:
            if clean.startswith(prefix):
                return False
        # Reject obviously fake
        if clean in ("1234567890", "0000000000", "9999999999"):
            return False
        # Reject timestamps/IDs (too many digits)
        digits_only = re.sub(r"[^\d]", "", clean)
        if len(digits_only) > 13 or len(digits_only) < 7:
            return False
        return True
