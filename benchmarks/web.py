"""Web baseline for the dashboard (apps/web): bundle sizes per route, load
metrics (FCP, LCP, CLS, TBT, load), INP and route-transition timings.

    ./benchmarks/web.sh                    full run (about 8 minutes), writes results/web-<label>.json + .md
    ./benchmarks/web.sh --quick            1 repetition, a smoke run
    ./benchmarks/web.sh --label candidate
    python benchmarks/compare.py benchmarks/results/web-baseline.json benchmarks/results/web-candidate.json

What is measured, and how it is kept honest:

* Bundle sizes come from the Vite manifest of a fresh production build. They are
  deterministic, so they are the metrics a regression check can gate tightly.
  For every lazily loaded page, "route JS" is the gzip size of the chunks that
  page needs on top of the initial bundle (the same definition
  apps/web/scripts/check-bundle-size.ts uses for the initial bundle).
* Runtime metrics come from headless Chromium driven by Playwright against the
  built SPA, served by a small local static server (gzip, no caching) with the
  backend API replaced by fixed fixtures answered instantly. That removes the
  backend and the network from the numbers on purpose: they say how the
  frontend behaves, not how the whole stack does. Two profiles run: `desktop`
  (no throttling) and `slow` (4x CPU slowdown, 1.6 Mbit/s down and 150 ms RTT,
  the Lighthouse "mobile" simulation, applied through the DevTools protocol).
* Every page load uses a fresh browser context, so the HTTP cache is cold.
  Each point is repeated and the median is reported with its range.
* INP is computed from real (trusted) pointer and keyboard interactions with
  the Event Timing API: the worst interaction of a scripted session, which is
  what INP is for fewer than 50 interactions.
* CLS follows the web-vitals definition (largest session window, 1 s gap,
  5 s cap, input-driven shifts excluded). TBT is the sum of long-task time over
  50 ms between FCP and the end of the load.
* A route transition is the time from clicking a sidebar link until the next
  page's heading is on screen and painted, on the first visit (its code chunk
  is fetched then).

It never touches ./data, the Go server, FastAPI or any database.
"""

from __future__ import annotations

import argparse
import datetime as dt
import gzip
import http.server
import json
import os
import platform
import re
import shutil
import statistics
import subprocess
import sys
import threading
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
WEB = ROOT / "apps" / "web"
RESULTS = ROOT / "benchmarks" / "results"
SCHEMA = 1

# Routes measured at runtime: (name, path, heading shown when the page is ready).
LOAD_ROUTES = [
    ("chat", "/chat", "Chat"),
    ("leads", "/leads", "Leads"),
    ("workbooks", "/workbooks", "Workbooks"),
    ("plugins", "/plugins", "Plugins"),
]
# Sidebar visits for the transition measurement, in order (first visits).
TRANSITIONS = [
    ("leads", "/leads", "Leads"),
    ("workbooks", "/workbooks", "Workbooks"),
    ("plugins", "/plugins", "Plugins"),
    ("analytics", "/analytics", "Analytics"),
    ("signals", "/signals", "Signals"),
    ("automations", "/automations", "Automations"),
]
PROFILES = {
    "desktop": {"cpu": 1, "down_kbps": 0, "up_kbps": 0, "rtt_ms": 0},
    "slow": {"cpu": 4, "down_kbps": 1600, "up_kbps": 750, "rtt_ms": 150},
}


def log(msg: str) -> None:
    print(f"[web-bench] {msg}", flush=True)


def sh(cmd: list[str], **kw) -> str:
    try:
        return subprocess.run(cmd, capture_output=True, text=True, check=True, **kw).stdout.strip()
    except Exception:
        return ""


# ── bundle sizes (deterministic) ─────────────────────────────────────────


def gz(path: Path) -> int:
    return len(gzip.compress(path.read_bytes(), compresslevel=9, mtime=0))


def kb(n: int) -> float:
    return round(n / 1000, 2)  # kB = 1000 bytes, as Vite and check-bundle-size report


def bundle_metrics(dist: Path) -> dict:
    manifest = json.loads((dist / ".vite" / "manifest.json").read_text())
    size_cache: dict[str, tuple[int, int]] = {}

    def size(file: str) -> tuple[int, int]:
        if file not in size_cache:
            p = dist / file
            size_cache[file] = (p.stat().st_size, gz(p))
        return size_cache[file]

    def closure(key: str, seen: set[str]) -> None:
        chunk = manifest.get(key)
        if not chunk or key in seen:
            return
        seen.add(key)
        for imp in chunk.get("imports", []):
            closure(imp, seen)

    def files_of(keys: set[str]) -> tuple[set[str], set[str]]:
        js, css = set(), set()
        for k in keys:
            chunk = manifest[k]
            if chunk["file"].endswith(".js"):
                js.add(chunk["file"])
            css.update(chunk.get("css", []))
        return js, css

    def total(files: set[str]) -> tuple[int, int]:
        return sum(size(f)[0] for f in files), sum(size(f)[1] for f in files)

    entries = [k for k, c in manifest.items() if c.get("isEntry") and c["file"].endswith(".js")]
    initial_keys: set[str] = set()
    for k in entries:
        closure(k, initial_keys)
    init_js, init_css = files_of(initial_keys)
    ij, ic = total(init_js), total(init_css)

    routes = {}
    for key, chunk in manifest.items():
        if not (chunk.get("isDynamicEntry") and key.startswith("src/pages/")):
            continue
        keys: set[str] = set()
        closure(key, keys)
        js, css = files_of(keys)
        js -= init_js
        css -= init_css
        rj, rc = total(js), total(css)
        routes[Path(key).stem] = {
            "js_chunks": len(js), "js_raw_kb": kb(rj[0]), "js_gzip_kb": kb(rj[1]),
            "css_gzip_kb": kb(rc[1]),
            # What a first visit to this page downloads in total, from a cold cache.
            "first_visit_js_gzip_kb": kb(ij[1] + rj[1]),
        }
    all_js = sorted((dist / "assets").glob("*.js"))
    all_css = sorted((dist / "assets").glob("*.css"))
    return {
        "initial": {"js_chunks": len(init_js), "js_raw_kb": kb(ij[0]), "js_gzip_kb": kb(ij[1]), "css_gzip_kb": kb(ic[1])},
        "total": {"js_chunks": len(all_js), "js_gzip_kb": kb(sum(gz(p) for p in all_js)),
                  "css_gzip_kb": kb(sum(gz(p) for p in all_css))},
        "routes": dict(sorted(routes.items())),
    }


def build_web(skip: bool) -> Path:
    dist = WEB / "dist"
    if not skip:
        if not shutil.which("bun"):
            sys.exit("error: 'bun' not found on PATH (needed to build apps/web); use --skip-build with an existing dist/")
        log("building apps/web (bun run build)")
        subprocess.run(["bun", "run", "build"], cwd=WEB, check=True, stdout=subprocess.DEVNULL)
    if not (dist / ".vite" / "manifest.json").exists():
        sys.exit(f"error: no build at {dist}; run `bun run build` in apps/web")
    return dist


# ── fixtures and static server ───────────────────────────────────────────


def fixtures() -> dict:
    leads = [{
        "id": i, "company": f"Acme {i}", "website": f"https://acme{i}.example", "email": f"hello@acme{i}.example",
        "email_confidence": "high", "email_provider": "x", "phone": "", "phone_provider": "", "contact_person": f"Person {i}",
        "contact_title": "VP Sales", "city": ["Austin", "Berlin", "Oslo", "Lima"][i % 4], "state": "", "address": "",
        "specialization": "software", "company_size": "51-200", "employee_count_exact": 80 + i, "description": "A company.",
        "revenue_range": "", "founded_year": "2015", "industry_tags": "saas", "technologies": "react", "funding_stage": "seed",
        "linkedin_url": "", "twitter_url": "", "facebook_url": "", "secondary_emails": "", "secondary_phones": "",
        "decision_makers": "", "glassdoor_rating": "", "hiring_signals": "", "enrichment_attempts": 1, "enrichment_waterfall": "",
        "source": "bench", "source_url": "", "collection_job_id": "", "workspace_id": "ws1", "score": 100 - i % 100,
        "score_tier": ["A", "B", "C"][i % 3], "status": "new", "yupcha_value_prop": "",
    } for i in range(500)]
    workbooks = [{
        "id": f"wb{i}", "name": f"Workbook {i}", "description": "", "status": "draft", "source_type": "empty",
        "filter_criteria": None, "columns_config": [], "total_rows": 100 + i, "completed_rows": 0,
        "created_at": "2026-10-01T10:00:00Z", "updated_at": "2026-10-01T10:00:00Z", "last_run_at": None,
    } for i in range(24)]
    plugins = [{
        "name": f"plugin_{i}", "display_name": f"Plugin {i}", "kind": ["scraper", "provider", "tool"][i % 3],
        "runtime": "declarative", "version": "1.0.0", "author": "bench", "license": "MIT", "description": "A bench plugin.",
        "tags": ["bench"], "source": "plugin", "signature": "trusted", "network": ["acme.example"], "secrets": [],
        "browser": False, "inputs": {"type": "object", "properties": {"domain": {"type": "string"}}, "required": ["domain"]},
        "outputs": "person", "runnable": True,
    } for i in range(12)]
    statuses = ["completed", "completed", "failed", "running", "cancelled"]
    runs = [{
        "id": f"00000000-0000-4000-8000-{i:012d}", "plugin": f"plugin_{i % 12}", "plugin_version": "1.0.0",
        "status": statuses[i % 5], "inputs": {"domain": f"acme{i}.example"}, "job_id": i, "created_by": "bench",
        "created_at": f"2026-10-{1 + (400 - i) // 24:02d}T{(400 - i) % 24:02d}:00:00Z", "started_at": None, "completed_at": None,
        "error": None, "stats": {"records": i % 40, "pages": 2},
    } for i in range(400)]
    return {"leads": leads, "workbooks": workbooks, "plugins": plugins, "runs": runs}


class Server:
    """Static file server for apps/web/dist: SPA fallback, gzip, no caching."""

    def __init__(self, dist: Path):
        self.dist = dist
        self._gz: dict[Path, bytes] = {}
        outer = self

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *a):  # quiet
                pass

            def do_GET(self):
                rel = self.path.split("?")[0].lstrip("/")
                p = (outer.dist / rel) if rel else outer.dist / "index.html"
                if not p.is_file() or ".." in rel:
                    p = outer.dist / "index.html"
                data = p.read_bytes()
                ctype = {".js": "text/javascript", ".css": "text/css", ".html": "text/html", ".svg": "image/svg+xml",
                         ".json": "application/json", ".woff2": "font/woff2", ".png": "image/png"}.get(p.suffix, "application/octet-stream")
                headers = {"Content-Type": ctype, "Cache-Control": "no-store"}
                if p.suffix in {".js", ".css", ".html", ".svg", ".json"}:
                    if p not in outer._gz:
                        outer._gz[p] = gzip.compress(data, compresslevel=6, mtime=0)
                    data = outer._gz[p]
                    headers["Content-Encoding"] = "gzip"
                self.send_response(200)
                for k, v in headers.items():
                    self.send_header(k, v)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        self.httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.port = self.httpd.server_address[1]
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *exc):
        self.httpd.shutdown()
        self.httpd.server_close()

    @property
    def base(self) -> str:
        return f"http://127.0.0.1:{self.port}"


def install_api(ctx, fx: dict) -> None:
    """Answer every /api and /auth call from fixtures, instantly."""
    runs = fx["runs"]

    def handle(route):
        req = route.request
        path = req.url.split("//", 1)[1].split("/", 1)[1]
        path = "/" + path
        base, _, query = path.partition("?")
        q = dict(p.split("=", 1) for p in query.split("&") if "=" in p)
        if base == "/auth/me":
            return route.fulfill(json={"id": 1, "username": "bench", "is_admin": True, "role": "admin"})
        if base == "/api/workspaces" and req.method == "GET":
            return route.fulfill(json={"workspaces": [{"id": "ws1", "name": "Bench", "slug": "bench"}], "active_id": "ws1"})
        if base == "/api/events":  # the legacy SSE stream: keep it quiet and closed
            return route.fulfill(status=204)
        if base == "/api/leads":
            return route.fulfill(json=fx["leads"])
        if base.startswith("/api/workbooks"):
            return route.fulfill(json={"workbooks": fx["workbooks"], "total": len(fx["workbooks"])})
        if base == "/api/stats":
            return route.fulfill(json={"total": 500, "by_status": {"new": 500}, "by_tier": {"A": 170, "B": 170, "C": 160},
                                       "by_city": {}, "by_source": {"bench": 500},
                                       "enrichment": {"total": 500, "with_email": 500, "with_phone": 0, "with_website": 500,
                                                      "with_linkedin": 0, "with_contact": 500, "avg_score": 70}})
        if base == "/api/v2/plugins":
            return route.fulfill(json={"plugins": fx["plugins"], "errors": []})
        if base == "/api/v2/plugin-runs" and req.method == "GET":
            limit = int(q.get("limit", 50))
            start = int(q["cursor"]) if q.get("cursor", "").isdigit() else 0
            page = runs[start:start + limit]
            more = start + limit < len(runs)
            return route.fulfill(json={"runs": page, "next_cursor": str(start + limit) if more else None})
        if base == "/api/v2/events":
            return route.fulfill(status=204)
        return route.fulfill(json=[])

    ctx.route("**/api/**", handle)
    ctx.route("**/auth/**", handle)


# ── page-side collection ─────────────────────────────────────────────────

INIT_SCRIPT = """
(() => {
  localStorage.setItem('yupcha_token', 'bench-token');
  localStorage.setItem('yupcha_workspace_id', 'ws1');
  const m = window.__m = { fcp: null, lcp: null, shifts: [], longtasks: [], events: [] };
  const obs = (type, cb, extra = {}) => { try { new PerformanceObserver(l => l.getEntries().forEach(cb)).observe({ type, buffered: true, ...extra }) } catch (e) {} };
  obs('paint', e => { if (e.name === 'first-contentful-paint') m.fcp = e.startTime });
  obs('largest-contentful-paint', e => { m.lcp = e.startTime });
  obs('layout-shift', e => m.shifts.push([e.startTime, e.value, e.hadRecentInput]));
  obs('longtask', e => m.longtasks.push([e.startTime, e.duration]));
  obs('event', e => m.events.push([e.name, e.startTime, e.duration, e.interactionId]), { durationThreshold: 16 });
  // Time until a page heading with this text is on screen and painted.
  window.__headingReady = (title, t0) => new Promise(resolve => {
    const has = () => [...document.querySelectorAll('h1')].some(h => h.textContent.trim() === title);
    const done = () => requestAnimationFrame(() => requestAnimationFrame(() => resolve(performance.now() - t0)));
    if (has()) return done();
    const mo = new MutationObserver(() => { if (has()) { mo.disconnect(); done() } });
    mo.observe(document.documentElement, { childList: true, subtree: true, characterData: true });
  });
})();
"""


def cls_of(shifts: list) -> float:
    """Largest session window of layout shifts (web-vitals definition)."""
    best = cur = 0.0
    start = last = None
    for t, value, recent in shifts:
        if recent:
            continue
        if start is None or t - last > 1000 or t - start > 5000:
            cur, start = 0.0, t
        cur += value
        last = t
        best = max(best, cur)
    return round(best, 4)


def tbt_of(longtasks: list, fcp: float | None, end: float) -> float:
    if fcp is None:
        return 0.0
    return round(sum(max(0.0, d - 50) for s, d in longtasks if fcp <= s <= end), 1)


def inp_of(events: list) -> tuple[float, int]:
    """Worst interaction duration (INP for fewer than 50 interactions) and the interaction count."""
    by_id: dict[int, float] = {}
    for _name, _start, duration, iid in events:
        if iid:
            by_id[iid] = max(by_id.get(iid, 0.0), duration)
    return (max(by_id.values()) if by_id else 0.0), len(by_id)


def throttle(ctx, page, profile: dict):
    cdp = ctx.new_cdp_session(page)
    if profile["cpu"] > 1:
        cdp.send("Emulation.setCPUThrottlingRate", {"rate": profile["cpu"]})
    if profile["down_kbps"]:
        cdp.send("Network.enable")
        cdp.send("Network.emulateNetworkConditions", {
            "offline": False, "latency": profile["rtt_ms"],
            "downloadThroughput": profile["down_kbps"] * 1000 / 8, "uploadThroughput": profile["up_kbps"] * 1000 / 8})
    return cdp


def js_bytes(page) -> tuple[int, int]:
    """Transferred JavaScript (encoded bytes) and request count so far."""
    return tuple(page.evaluate("""() => {
      const r = performance.getEntriesByType('resource');
      return [r.filter(e => e.initiatorType === 'script' || e.name.endsWith('.js')).reduce((a, e) => a + e.encodedBodySize, 0), r.length];
    }"""))


class Transient(Exception):
    """A sample spoiled by the environment, not the page (see watch_failures)."""


def watch_failures(page) -> list[str]:
    """Collect failed requests. Chromium aborts in-flight requests with
    ERR_NETWORK_CHANGED whenever the host's network interfaces change (a docker
    bridge or Wi-Fi flapping is enough, even for loopback traffic); such a
    sample says nothing about the page, so it is discarded and retaken."""
    failures: list[str] = []
    page.on("requestfailed", lambda r: failures.append(f"{r.url.rsplit('/', 1)[-1][:40]}: {r.failure}"))
    return failures


def check_clean(failures: list[str]) -> None:
    bad = [f for f in failures if "ERR_NETWORK_CHANGED" in f or "ERR_INTERNET_DISCONNECTED" in f]
    if bad:
        raise Transient(bad[0])


def retrying(fn, *args, attempts: int = 6):
    for attempt in range(1, attempts + 1):
        try:
            return fn(*args)
        except Transient as e:
            log(f"  discarding a sample ({e}); retrying ({attempt}/{attempts})")
    raise RuntimeError(f"could not get a clean sample in {attempts} attempts; the host network keeps changing")


def measure_load(browser, base: str, fx: dict, profile: dict, path: str, title: str) -> dict:
    ctx = browser.new_context(viewport={"width": 1366, "height": 800})
    ctx.add_init_script(INIT_SCRIPT)
    install_api(ctx, fx)
    page = ctx.new_page()
    failures = watch_failures(page)
    throttle(ctx, page, profile)
    t0 = time.monotonic()
    try:
        page.goto(base + path, wait_until="load")
        page.wait_for_function("t => [...document.querySelectorAll('h1')].some(h => h.textContent.trim() === t)", arg=title, timeout=30_000)
    except Exception:
        check_clean(failures)
        raise
    ready = page.evaluate("() => performance.now()")
    page.wait_for_timeout(1500)  # let late work (layout shifts, long tasks, lazy content) land
    check_clean(failures)
    nav = page.evaluate("() => { const n = performance.getEntriesByType('navigation')[0]; return {ttfb: n.responseStart, dcl: n.domContentLoadedEventEnd, load: n.loadEventEnd} }")
    m = page.evaluate("() => window.__m")
    js, reqs = js_bytes(page)
    out = {
        "ttfb_ms": nav["ttfb"], "dcl_ms": nav["dcl"], "load_ms": nav["load"], "fcp_ms": m["fcp"], "lcp_ms": m["lcp"],
        "ready_ms": ready, "cls": cls_of(m["shifts"]), "tbt_ms": tbt_of(m["longtasks"], m["fcp"], nav["load"] + 1500),
        "js_transfer_kb": kb(js), "requests": reqs, "wall_s": round(time.monotonic() - t0, 2),
    }
    ctx.close()
    return out


def measure_session(browser, base: str, fx: dict, profile: dict) -> dict:
    """One scripted session: route transitions (first visits) then interactions for INP."""
    ctx = browser.new_context(viewport={"width": 1366, "height": 800})
    ctx.add_init_script(INIT_SCRIPT)
    install_api(ctx, fx)
    page = ctx.new_page()
    failures = watch_failures(page)
    throttle(ctx, page, profile)
    try:
        result = _session(page, base)
    except Exception:
        check_clean(failures)
        raise
    check_clean(failures)
    ctx.close()
    return result


def _session(page, base: str) -> dict:
    page.goto(base + "/chat", wait_until="load")
    page.wait_for_function("() => [...document.querySelectorAll('h1')].some(h => h.textContent.trim() === 'Chat')", timeout=30_000)
    page.wait_for_timeout(1000)

    transitions = {}
    for name, path, title in TRANSITIONS:
        before_js, _ = js_bytes(page)
        link = page.locator(f'a[href="{path}"]').first
        if link.count() == 0:
            transitions[name] = None
            continue
        elapsed = page.evaluate(
            """([path, title]) => { const t0 = performance.now(); document.querySelector(`a[href="${path}"]`).click();
                 return window.__headingReady(title, t0) }""", [path, title])
        after_js, _ = js_bytes(page)
        transitions[name] = {"ms": elapsed, "js_transfer_kb": kb(after_js - before_js)}
        page.wait_for_timeout(500)

    # Interactions on the plugins page (tabs, a filter, the runs list, the command menu): real input events.
    page.locator('a[href="/plugins"]').first.click()
    page.wait_for_function("() => [...document.querySelectorAll('h1')].some(h => h.textContent.trim() === 'Plugins')", timeout=60_000)
    page.wait_for_timeout(800)
    page.evaluate("() => { window.__m.events.length = 0 }")
    steps = 0
    page.get_by_role("tab", name=re.compile("^Runs")).click(); steps += 1
    page.wait_for_selector('table[aria-label="Plugin runs"] tbody tr', timeout=30_000)
    page.wait_for_timeout(300)
    page.locator('select[aria-label="Status"]').select_option("failed"); steps += 1
    page.wait_for_timeout(300)
    page.locator('select[aria-label="Status"]').select_option("all"); steps += 1
    page.wait_for_timeout(300)
    load_more = page.get_by_role("button", name="Load more")
    if load_more.count():
        load_more.click(); steps += 1
        page.wait_for_timeout(500)
    page.get_by_role("tab", name=re.compile("^Catalog")).click(); steps += 1
    page.wait_for_timeout(300)
    page.keyboard.press("Control+k"); steps += 1
    page.wait_for_timeout(300)
    page.keyboard.type("lea", delay=40)
    page.keyboard.press("Escape"); steps += 1
    page.wait_for_timeout(300)
    page.get_by_role("tab", name=re.compile("^Runs")).click(); steps += 1
    page.wait_for_timeout(300)
    first_row = page.locator('table[aria-label="Plugin runs"] tbody tr[data-run-id]').first
    first_row.click(); steps += 1  # navigates to the run page
    page.wait_for_timeout(1500)
    m = page.evaluate("() => window.__m")
    inp, count = inp_of(m["events"])
    return {"transitions": transitions, "inp_ms": inp, "interactions": count, "scripted_steps": steps}


# ── aggregation and reporting ────────────────────────────────────────────


def spread(xs: list[float]) -> dict:
    xs = [x for x in xs if x is not None]
    if not xs:
        return {"median": 0.0, "min": 0.0, "max": 0.0}
    return {"median": round(statistics.median(xs), 2), "min": round(min(xs), 2), "max": round(max(xs), 2)}


def flat_metrics(bundle: dict, loads: dict, sessions: dict) -> dict:
    m: dict[str, dict] = {}

    def put(key, value, unit, better, gated, threshold=None, slack=None):
        e = {"value": value, "unit": unit, "better": better, "gated": gated}
        if threshold is not None:
            e["threshold"] = threshold
        if slack is not None:
            e["slack"] = slack
        m[key] = e

    # Deterministic: gated tightly (3%, and 0.5 kB of slack for hash and rounding noise).
    put("web/bundle/initial/js_gzip_kb", bundle["initial"]["js_gzip_kb"], "kB", "lower", True, 0.03, 0.5)
    put("web/bundle/initial/css_gzip_kb", bundle["initial"]["css_gzip_kb"], "kB", "lower", True, 0.03, 0.5)
    put("web/bundle/total/js_gzip_kb", bundle["total"]["js_gzip_kb"], "kB", "lower", True, 0.05, 1.0)
    for name, r in bundle["routes"].items():
        put(f"web/bundle/route/{name}/js_gzip_kb", r["js_gzip_kb"], "kB", "lower", True, 0.05, 0.5)
        put(f"web/bundle/route/{name}/first_visit_js_gzip_kb", r["first_visit_js_gzip_kb"], "kB", "lower", False)
    # Runtime: medians of repeated cold loads. Gated loosely (30% and a time floor) because a shared
    # machine moves them; informational unless the run happened on an idle one (see the README).
    for prof, routes in loads.items():
        slack = 60 if prof == "desktop" else 400
        for route, s in routes.items():
            for k, unit in (("fcp_ms", "ms"), ("lcp_ms", "ms"), ("ready_ms", "ms"), ("tbt_ms", "ms")):
                put(f"web/load/{prof}/{route}/{k}", s[k]["median"], unit, "lower", k in ("lcp_ms", "ready_ms"), 0.30, slack)
            put(f"web/load/{prof}/{route}/cls", s["cls"]["median"], "", "lower", False)
            put(f"web/load/{prof}/{route}/js_transfer_kb", s["js_transfer_kb"]["median"], "kB", "lower", True, 0.05, 1.0)
    for prof, s in sessions.items():
        slack = 60 if prof == "desktop" else 300
        put(f"web/inp/{prof}/inp_ms", s["inp_ms"]["median"], "ms", "lower", True, 0.30, slack)
        for route, t in s["transitions"].items():
            put(f"web/transition/{prof}/{route}/ms", t["ms"]["median"], "ms", "lower", True, 0.30, slack)
    return m


def render(result: dict) -> str:
    out: list[str] = []
    w = out.append
    git, mach, cfg = result["git"], result["machine"], result["config"]
    w(f"# Web benchmark results: {result['label']}\n")
    w(f"- Created: {result['created_at']} (took {result['duration_s']} s)")
    w(f"- Commit: `{git['commit'][:12]}` on `{git['branch']}`{' (working tree dirty)' if git['dirty'] else ''}")
    w(f"- Machine: {mach['cpu']}, {mach['logical_cores']} logical cores, {mach['ram_gb']} GB RAM, {mach['os']}")
    w(f"- Load average (1/5/15 min) at start: {', '.join(f'{x:.1f}' for x in mach['load_avg_start'])}; "
      f"at end: {', '.join(f'{x:.1f}' for x in mach['load_avg_end'])}")
    w(f"- Browser: {mach['chromium']}; Playwright {mach['playwright']}; bun {mach['bun']}")
    w(f"- {cfg['reps']} repetition(s) per point, fresh browser context (cold cache) each; cells are the median with the min-max range. "
      "Backend API answered from fixtures; see the README for what that does and does not cover.\n")

    b = result["web"]["bundle"]
    w("## Bundle size (gzip, deterministic)\n")
    w(f"Initial JS (entry + static imports): **{b['initial']['js_gzip_kb']} kB** in {b['initial']['js_chunks']} chunks "
      f"({b['initial']['js_raw_kb']} kB raw); initial CSS {b['initial']['css_gzip_kb']} kB. "
      f"All JS: {b['total']['js_gzip_kb']} kB in {b['total']['js_chunks']} chunks; all CSS {b['total']['css_gzip_kb']} kB.\n")
    w("| route (page chunk) | route JS kB gzip | route CSS kB | first visit JS kB (initial + route) |")
    w("|---|---:|---:|---:|")
    for name, r in b["routes"].items():
        w(f"| {name} | {r['js_gzip_kb']} | {r['css_gzip_kb']} | {r['first_visit_js_gzip_kb']} |")
    w("")
    w("Route JS is what the page needs on top of the initial bundle; chunks shared by several pages are counted for each.\n")

    for prof, routes in result["web"]["load"].items():
        p = PROFILES[prof]
        w(f"## Cold load, `{prof}` profile ({'no throttling' if p['cpu'] == 1 else str(p['cpu']) + 'x CPU slowdown, ' + str(p['down_kbps']) + ' kbit/s, ' + str(p['rtt_ms']) + ' ms RTT'})\n")
        w("| route | TTFB ms | FCP ms | LCP ms | heading ready ms | load ms | CLS | TBT ms | JS transferred kB | requests |")
        w("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
        for route, s in routes.items():
            cell = lambda k: f"{s[k]['median']:g} ({s[k]['min']:g}-{s[k]['max']:g})"  # noqa: E731
            w(f"| {route} | {s['ttfb_ms']['median']:g} | {cell('fcp_ms')} | {cell('lcp_ms')} | {cell('ready_ms')} | "
              f"{s['load_ms']['median']:g} | {s['cls']['median']:g} | {s['tbt_ms']['median']:g} | {s['js_transfer_kb']['median']:g} | {s['requests']['median']:g} |")
        w("")
    for prof, s in result["web"]["session"].items():
        w(f"## Interactions and route transitions, `{prof}` profile\n")
        w(f"INP (worst of {s['interactions']['median']:g} scripted interactions on the Plugins page): "
          f"**{s['inp_ms']['median']:g} ms** ({s['inp_ms']['min']:g}-{s['inp_ms']['max']:g}).\n")
        w("| first visit to | ms until heading painted | JS fetched kB |")
        w("|---|---:|---:|")
        for route, t in s["transitions"].items():
            w(f"| {route} | {t['ms']['median']:g} ({t['ms']['min']:g}-{t['ms']['max']:g}) | {t['js_transfer_kb']['median']:g} |")
        w("")
    return "\n".join(out) + "\n"


def machine_info(browser_version: str) -> dict:
    cpu = ""
    try:
        for line in Path("/proc/cpuinfo").read_text().splitlines():
            if line.startswith("model name"):
                cpu = line.split(":", 1)[1].strip()
                break
    except OSError:
        cpu = platform.processor()
    mem = None
    try:
        for line in Path("/proc/meminfo").read_text().splitlines():
            if line.startswith("MemTotal"):
                mem = round(int(line.split()[1]) / 1024 / 1024, 1)
                break
    except OSError:
        pass
    os_name = platform.platform()
    try:
        for line in Path("/etc/os-release").read_text().splitlines():
            if line.startswith("PRETTY_NAME="):
                os_name = line.split("=", 1)[1].strip('"') + " / " + platform.release()
    except OSError:
        pass
    import importlib.metadata as md

    return {"cpu": cpu, "logical_cores": os.cpu_count(), "ram_gb": mem, "os": os_name, "load_avg_start": os.getloadavg(),
            "chromium": browser_version, "playwright": md.version("playwright"), "python": platform.python_version(),
            "bun": sh(["bun", "--version"]), "node": sh(["node", "--version"])}


def git_info() -> dict:
    return {"commit": sh(["git", "rev-parse", "HEAD"], cwd=ROOT), "branch": sh(["git", "rev-parse", "--abbrev-ref", "HEAD"], cwd=ROOT),
            "dirty": bool(sh(["git", "status", "--porcelain", "--untracked-files=no"], cwd=ROOT))}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--label", default=None, help="result name; written to results/web-<label>.json (default: web-run-<UTC timestamp>)")
    ap.add_argument("--reps", type=int, default=5, help="repetitions per point (default 5)")
    ap.add_argument("--quick", action="store_true", help="1 repetition, desktop profile only (smoke run)")
    ap.add_argument("--profiles", default="desktop,slow", help="comma-separated: desktop,slow")
    ap.add_argument("--skip-build", action="store_true", help="measure the existing apps/web/dist instead of rebuilding")
    ap.add_argument("--bundle-only", action="store_true", help="only the deterministic bundle sizes (no browser)")
    args = ap.parse_args()
    if args.quick:
        args.reps, args.profiles = 1, "desktop"
    profiles = [p for p in args.profiles.split(",") if p]
    for p in profiles:
        if p not in PROFILES:
            ap.error(f"unknown profile {p!r}")
    started = time.time()
    label = "web-" + (args.label or "run-" + dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")).removeprefix("web-")
    RESULTS.mkdir(parents=True, exist_ok=True)

    dist = build_web(args.skip_build)
    bundle = bundle_metrics(dist)
    log(f"initial JS {bundle['initial']['js_gzip_kb']} kB gzip, {len(bundle['routes'])} page chunks")

    loads: dict = {}
    sessions: dict = {}
    browser_version = "not run"
    if not args.bundle_only:
        from playwright.sync_api import sync_playwright

        fx = fixtures()
        with Server(dist) as srv, sync_playwright() as pw:
            browser = pw.chromium.launch(args=["--disable-gpu", "--no-sandbox"])
            browser_version = browser.version
            try:
                for prof in profiles:
                    loads[prof] = {}
                    for name, path, title in LOAD_ROUTES:
                        runs = []
                        for rep in range(args.reps):
                            runs.append(retrying(measure_load, browser, srv.base, fx, PROFILES[prof], path, title))
                        loads[prof][name] = {k: spread([r[k] for r in runs]) for k in runs[0] if k != "wall_s"}
                        s = loads[prof][name]
                        log(f"load/{prof}/{name}: FCP {s['fcp_ms']['median']:.0f} ms, LCP {s['lcp_ms']['median']:.0f} ms, "
                            f"ready {s['ready_ms']['median']:.0f} ms, CLS {s['cls']['median']}, TBT {s['tbt_ms']['median']:.0f} ms")
                    reps = [retrying(measure_session, browser, srv.base, fx, PROFILES[prof]) for _ in range(args.reps)]
                    sessions[prof] = {
                        "inp_ms": spread([r["inp_ms"] for r in reps]),
                        "interactions": spread([r["interactions"] for r in reps]),
                        "transitions": {
                            name: {"ms": spread([r["transitions"][name]["ms"] for r in reps if r["transitions"].get(name)]),
                                   "js_transfer_kb": spread([r["transitions"][name]["js_transfer_kb"] for r in reps if r["transitions"].get(name)])}
                            for name, _p, _t in TRANSITIONS if all(r["transitions"].get(name) for r in reps)},
                    }
                    log(f"session/{prof}: INP {sessions[prof]['inp_ms']['median']:.0f} ms; transitions "
                        + ", ".join(f"{n} {t['ms']['median']:.0f} ms" for n, t in sessions[prof]["transitions"].items()))
            finally:
                browser.close()

    machine = machine_info(browser_version)
    machine["load_avg_end"] = os.getloadavg()
    result = {
        "schema": SCHEMA, "kind": "web", "label": label,
        "created_at": dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds"),
        "duration_s": round(time.time() - started, 1), "git": git_info(), "machine": machine,
        "config": {"reps": args.reps, "profiles": profiles, "profile_settings": {p: PROFILES[p] for p in profiles},
                   "load_routes": [r[1] for r in LOAD_ROUTES], "bundle_only": args.bundle_only},
        "web": {"bundle": bundle, "load": loads, "session": sessions},
        "metrics": flat_metrics(bundle, loads, sessions),
    }
    (RESULTS / f"{label}.json").write_text(json.dumps(result, indent=2) + "\n")
    (RESULTS / f"{label}.md").write_text(render(result))
    log(f"wrote benchmarks/results/{label}.json and .md ({result['duration_s']}s)")
    print()
    print(render(result))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
