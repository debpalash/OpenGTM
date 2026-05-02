"""
Yupcha Lead Dashboard — Flask API + Web UI

Local dashboard for browsing, filtering, scoring, and exporting leads.
Runs on http://127.0.0.1:5050
"""

import sys
import os
import csv
import io

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from flask import Flask, send_from_directory, request, jsonify, Response
from leadgen.db import LeadDB
from leadgen.models import LEAD_STATUSES


def _clean(val):
    """Strip __all__ sentinel from React Select values."""
    if val and val != "__all__":
        return val
    return None


def create_app():
    static_dir = os.path.join(os.path.dirname(__file__), "web", "dist")
    app = Flask(__name__, static_folder=static_dir, static_url_path="")

    def get_db():
        return LeadDB()

    @app.route("/")
    def index():
        return send_from_directory(static_dir, "index.html")

    @app.errorhandler(404)
    def not_found(e):
        # SPA fallback
        return send_from_directory(static_dir, "index.html")

    # ── API Routes ─────────────────────────────────────────────────

    @app.route("/api/leads")
    def api_leads():
        db = get_db()
        leads = db.get_leads(
            status=_clean(request.args.get("status")),
            city=_clean(request.args.get("city")),
            source=_clean(request.args.get("source")),
            score_min=int(request.args["score_min"]) if request.args.get("score_min") else None,
            score_max=int(request.args["score_max"]) if request.args.get("score_max") else None,
            score_tier=_clean(request.args.get("tier")),
            search=_clean(request.args.get("search")),
            limit=int(request.args.get("limit", 200)),
            offset=int(request.args.get("offset", 0)),
            order_by=request.args.get("order_by", "score DESC"),
        )
        result = [l.to_dict() for l in leads]
        db.close()
        return jsonify(result)

    @app.route("/api/stats")
    def api_stats():
        db = get_db()
        stats = db.get_stats()
        db.close()
        return jsonify(stats)

    @app.route("/api/filters")
    def api_filters():
        db = get_db()
        data = {
            "cities": db.get_cities(),
            "sources": db.get_sources(),
            "statuses": LEAD_STATUSES,
            "tiers": ["hot", "warm", "cold", "unqualified"],
        }
        db.close()
        return jsonify(data)

    @app.route("/api/lead/<int:lead_id>", methods=["GET"])
    def api_get_lead(lead_id):
        db = get_db()
        lead = db.get_lead(lead_id)
        db.close()
        if lead:
            return jsonify(lead.to_dict())
        return jsonify({"error": "Not found"}), 404

    @app.route("/api/lead/<int:lead_id>/status", methods=["POST"])
    def api_update_status(lead_id):
        data = request.json
        db = get_db()
        db.update_status(lead_id, data["status"], data.get("note", ""))
        db.close()
        return jsonify({"ok": True})

    @app.route("/api/lead/<int:lead_id>", methods=["PUT"])
    def api_update_lead(lead_id):
        data = request.json
        db = get_db()
        db.update_lead_fields(lead_id, data)
        db.close()
        return jsonify({"ok": True})

    @app.route("/api/lead/<int:lead_id>", methods=["DELETE"])
    def api_delete_lead(lead_id):
        db = get_db()
        db.delete_lead(lead_id)
        db.close()
        return jsonify({"ok": True})

    @app.route("/api/lead", methods=["POST"])
    def api_add_lead():
        from leadgen.models import Lead
        data = request.json
        lead = Lead.from_dict(data)
        lead.source = data.get("source", "manual")
        db = get_db()
        lead_id = db.upsert_lead(lead)
        db.close()
        return jsonify({"ok": True, "id": lead_id})

    @app.route("/api/export/csv")
    def api_export_csv():
        db = get_db()
        leads = db.get_leads(
            status=request.args.get("status"),
            city=request.args.get("city"),
            score_tier=request.args.get("tier"),
            score_min=int(request.args["score_min"]) if request.args.get("score_min") else None,
            limit=10000,
        )
        db.close()

        output = io.StringIO()
        writer = csv.writer(output)
        writer.writerow(["Company","Website","Email","Phone","City","Specialization",
                         "Score","Tier","Status","Source","LinkedIn","Contact","Notes"])
        for l in leads:
            writer.writerow([l.company, l.website, l.email, l.phone, l.city,
                             l.specialization, l.score, l.score_tier, l.status,
                             l.source, l.linkedin_url, l.contact_person, l.notes])

        return Response(output.getvalue(), mimetype="text/csv",
                        headers={"Content-Disposition": "attachment; filename=yupcha_leads.csv"})

    # ── Collection & Jobs API ─────────────────────────────────────

    @app.route("/api/collect", methods=["POST"])
    def api_collect():
        """Submit a stealth collection query."""
        import uuid
        import threading

        data = request.json
        query = data.get("query", "").strip()
        if not query:
            return jsonify({"error": "query is required"}), 400

        job_id = str(uuid.uuid4())[:8]
        db = get_db()
        db.create_job(job_id, query)
        db.close()

        # Run collection in background thread
        def _run_job():
            import asyncio
            from leadgen.job_runner import JobRunner
            runner = JobRunner()
            asyncio.run(runner._process_job({"id": job_id, "query": query, "tier": 1}))

        thread = threading.Thread(target=_run_job, daemon=True)
        thread.start()

        return jsonify({"ok": True, "job_id": job_id, "query": query})

    @app.route("/api/jobs")
    def api_jobs():
        """List collection jobs."""
        db = get_db()
        status = _clean(request.args.get("status"))
        jobs = db.get_jobs(status=status)
        db.close()
        return jsonify(jobs)

    @app.route("/api/system-stats")
    def api_system_stats():
        """System stats: proxy pool, rate limiter, pipeline health."""
        from leadgen.proxy_pool import ProxyPool
        from leadgen.rate_limiter import RateLimiter

        pp = ProxyPool()
        rl = RateLimiter()
        db = get_db()
        jobs = db.get_jobs(limit=100)
        db.close()

        job_stats = {"total": len(jobs)}
        for s in ["pending", "running", "done", "failed"]:
            job_stats[s] = sum(1 for j in jobs if j["status"] == s)

        return jsonify({
            "proxy_pool": pp.stats(),
            "rate_limiter": rl.stats(),
            "jobs": job_stats,
        })

    return app


if __name__ == "__main__":
    app = create_app()
    app.run(host="127.0.0.1", port=5050, debug=True)

