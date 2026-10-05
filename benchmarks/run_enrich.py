"""Enrichment benchmark: the Python workbook executor against the Go one.

    OPENGTM_BENCH_DATABASE_URL=postgresql://postgres:postgres@127.0.0.1:55432/postgres \
        ./benchmarks/run.sh --enrich            # or: uv run python benchmarks/run_enrich.py

What is compared
----------------
One workbook run: R rows, one enrichment column, one manifest v1 connector
(a single provider, no waterfall fallbacks), answered by a local HTTPS provider
simulator with a fixed latency. A "cell" is one completed enrichment: the
provider call, the result and evidence written, and, for the paid connector,
the spend reservation, dispatch and settlement. Same PostgreSQL database and
non-superuser role, same machine, same simulator, same payload
(``concurrency`` = rows in flight, ``provider_workers`` = provider calls in
flight).

Engines:

* ``go``              ``Worker.Handle`` of apps/server/internal/jobs/enrich.
* ``python-pool``     ``handle_run_workbook`` unmodified, including its killable
                      provider process pool: what Python really does per cell.
* ``python-threads``  the same handler with the provider call on a thread
                      instead of a pool process: the cheapest Python can be
                      (a lower bound on its cost).

Not measured, and favouring Python: the per-job child process of
``QueueService`` (about a second per job), Redis broadcasts (no Redis here;
production publishes one message per cell). Favouring neither: queue
claim/finalize, which is the M0 queue benchmark's subject.

CPU is the CPU time of the engine's whole process tree (user + system, pool
workers included) minus the same engine running a one-row job, so interpreter
start-up and imports cancel; it is a marginal cost per cell. Peak RSS is the
largest single process. Database transactions come from pg_stat_database.

Like run.py this creates a throwaway database and role, never touches ./data,
and drops everything it created, even on failure.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import resource
import shutil
import statistics
import subprocess
import sys
import tempfile
import time
import urllib.request
import uuid
from pathlib import Path

import psycopg

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from benchmarks.run import (  # noqa: E402
    RESULTS, ROOT, SCHEMA, SERVER, Database, git_info, log, machine_info, make_sandbox, snapshot_data,
)

WS = "ws-bench-enrich"
ENGINES = ["go", "python-pool", "python-threads"]

# (name, provider, rows, simulator latency ms, rows in flight, provider calls in flight)
WORKLOADS = [
    ("free-5ms", "bench_free", 3000, 5, 12, 8),
    ("free-100ms", "bench_free", 1200, 100, 12, 8),
    ("paid-5ms", "bench_paid", 600, 5, 12, 8),
    ("free-5ms-wide", "bench_free", 3000, 5, 32, 32),
]
# durable commits (PostgreSQL's default) make every cell wait for several fsyncs, so this disk
# bounds both engines; fewer rows keep that variant to minutes. `nosync` removes the flush wait so
# the numbers reflect code. See benchmarks/README.md.
DURABLE_ROWS = {"free-5ms": 600, "paid-5ms": 200}

MANIFEST = """manifest_version: "1"
name: {name}
capability: email
default_confidence: 0.8
cost_per_lookup: {cost}
request:
  method: {method}
  url: {base}{path}
{extra}  timeout: 20
response:
  error_path: error
  mappings:
    email: "$.data.email"
"""

SITECUSTOMIZE = '''"""Benchmark sandbox only: let the SSRF guard accept the loopback provider simulator.

The production guard refuses loopback by design. This wraps check_url, in this
process and in the provider process pool's workers (which import this module at
start-up), so that URLs under BENCH_SIM_BASE pass and everything else still goes
through the real guard.
"""
import importlib.abc
import importlib.util
import os
import sys

_TARGET = "apps.api.core.url_guard"


class _Finder(importlib.abc.MetaPathFinder):
    def find_spec(self, name, path, target=None):
        if name != _TARGET:
            return None
        sys.meta_path.remove(self)
        try:
            spec = importlib.util.find_spec(name)
        finally:
            sys.meta_path.insert(0, self)
        loader = spec.loader
        original = loader.exec_module

        def exec_module(module):
            original(module)
            real = module.check_url
            base = os.environ.get("BENCH_SIM_BASE", "")

            def check_url(url, *a, **kw):
                return url if base and url.startswith(base) else real(url, *a, **kw)

            module.check_url = check_url

        loader.exec_module = exec_module
        return spec


sys.meta_path.insert(0, _Finder())
'''


def write_connectors(dirs: list[Path], base: str) -> None:
    free = MANIFEST.format(name="bench_free", cost="0.0", method="GET", base=base, path="/free",
                           extra='  query:\n    domain: "{{input.domain}}"\n')
    paid = MANIFEST.format(name="bench_paid", cost="0.05", method="POST", base=base, path="/paid",
                           extra='  body:\n    domain: "{{input.domain}}"\n')
    for d in dirs:
        d.mkdir(parents=True, exist_ok=True)
        (d / "bench_free.yaml").write_text(free)
        (d / "bench_paid.yaml").write_text(paid)


class Simulator:
    def __init__(self, binary: Path, latency_ms: int, tmp: Path):
        self.cert = tmp / f"sim-{uuid.uuid4().hex[:6]}.pem"
        self.proc = subprocess.Popen([str(binary), "-latency", f"{latency_ms}ms", "-cert-out", str(self.cert)],
                                     stdout=subprocess.PIPE, text=True)
        line = self.proc.stdout.readline().strip()
        if not line.startswith("READY "):
            raise RuntimeError(f"provider simulator did not start: {line!r}")
        self.base = line.split(" ", 1)[1]

    def stats(self) -> dict:
        import ssl

        ctx = ssl.create_default_context(cafile=str(self.cert))
        with urllib.request.urlopen(self.base + "/_stats", context=ctx, timeout=5) as r:
            return json.loads(r.read())

    def close(self) -> None:
        self.proc.terminate()
        try:
            self.proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self.proc.kill()


def seed(owner_url: str, provider: str, rows: int, conc: int, workers: int) -> tuple[str, int]:
    wb = f"wb-bench-{uuid.uuid4().hex[:10]}"
    tag = uuid.uuid4().hex[:6]
    columns = [{"id": "company", "name": "Company", "type": "lead_field"},
               {"id": "website", "name": "Website", "type": "lead_field"},
               {"id": "email", "name": "Email", "type": "enrichment", "provider": provider,
                "target_field": "email", "verify": False}]
    with psycopg.connect(owner_url) as c:
        c.execute("""INSERT INTO workbooks (id, name, description, status, workspace_id, source_type, source_config,
                         filter_criteria, columns_config, total_rows, completed_rows, sync_to_leads, budget_max_usd,
                         budget_spent_usd, refresh_policy)
                     VALUES (%s, %s, '', 'draft', %s, 'csv', '{}', '{}', %s, 0, 0, true, 0, 0, '{}')""",
                  (wb, wb, WS, json.dumps(columns)))
        with c.cursor() as cur:
            cur.executemany(
                """INSERT INTO workbook_rows (workbook_id, workspace_id, position, data, enrichments, corroboration_count)
                   VALUES (%s, %s, %s, %s, '{}', 1)""",
                [(wb, WS, i, json.dumps({"company": f"Company {i}", "website": f"r{i}-{tag}.example"}))
                 for i in range(rows)])
        row_ids = [r[0] for r in c.execute("SELECT id FROM workbook_rows WHERE workbook_id = %s ORDER BY id", (wb,))]
        payload = {"workbook_id": wb, "workspace_id": WS, "run_id": uuid.uuid4().hex, "column_ids": ["email"],
                   "row_columns": None, "row_ids": row_ids, "lead_ids": None, "concurrency": conc, "max_providers": 0,
                   "retry_passes": 1, "provider_workers": workers, "provider_timeout": 10, "fill_missing": False,
                   "force": False}
        # Warm the production process pool with a provider that exists in its workers.
        payload["warm_provider"] = provider
        (job,) = c.execute(
            """INSERT INTO jobs (type, payload, workspace_id, status, priority, created_at, started_at, last_heartbeat,
                   retry_count, max_retries, worker_id, locked_at, next_run_at)
               VALUES ('run_workbook_connector', %s, %s, 'processing', 1, LOCALTIMESTAMP, LOCALTIMESTAMP,
                       LOCALTIMESTAMP, 0, 3, 'bench-worker:1:00000000', LOCALTIMESTAMP, LOCALTIMESTAMP)
               RETURNING id""", (json.dumps(payload), WS)).fetchone()
        c.commit()
    return wb, job


def verify(owner_url: str, wb: str, provider: str, rows: int) -> None:
    with psycopg.connect(owner_url) as c:
        (done,) = c.execute("SELECT count(*) FROM workbook_enrichments WHERE workbook_id = %s AND status = 'complete'",
                            (wb,)).fetchone()
        if done != rows:
            raise RuntimeError(f"{wb}: {done} of {rows} cells completed; the run is not a valid measurement")
        if provider == "bench_paid":
            (settled, spent) = c.execute(
                """SELECT (SELECT count(*) FROM workbook_spend_attempts WHERE workbook_id = %s AND status = 'settled'),
                          (SELECT budget_spent_usd FROM workbooks WHERE id = %s)""", (wb, wb)).fetchone()
            if settled != rows or abs(spent - 0.05 * rows) > 1e-6 * rows:
                raise RuntimeError(f"{wb}: {settled} settled attempts, spent {spent}, expected {rows} and {0.05 * rows}")


def xacts(owner_url: str) -> int:
    time.sleep(1.5)  # pg_stat_database is flushed asynchronously
    with psycopg.connect(owner_url, autocommit=True) as c:
        (n,) = c.execute("SELECT xact_commit + xact_rollback FROM pg_stat_database WHERE datname = current_database()").fetchone()
    return n


def cpu_seconds() -> float:
    r = resource.getrusage(resource.RUSAGE_CHILDREN)
    return r.ru_utime + r.ru_stime


def run_engine(engine: str, db: Database, tmp: Path, sandbox: Path, go_bin: Path, sim: Simulator, job: int,
               connectors: Path) -> dict:
    out = tmp / f"{engine}-{job}.json"
    env = {**os.environ, "SSL_CERT_FILE": str(sim.cert), "BENCH_SIM_BASE": sim.base}
    if engine == "go":
        cmd = [str(go_bin), "-test.run", "^TestBenchEnrich$", "-test.timeout", "30m"]
        env |= {"OPENGTM_BENCH_OUT": str(out), "OPENGTM_BENCH_DATABASE_URL": db.app_url,
                "OPENGTM_BENCH_JOB": str(job), "OPENGTM_BENCH_CONNECTORS": str(connectors)}
        cwd = SERVER / "internal" / "jobs" / "enrich"
    else:
        mode = engine.split("-", 1)[1]
        cmd = [sys.executable, str(sandbox / "benchmarks" / "bench_enrich_python.py"), "--owner-url", db.owner_url,
               "--app-url", db.app_sa_url, "--job", str(job), "--mode", mode, "--out", str(out)]
        env |= {"PYTHONPATH": str(sandbox), "DATA_DIR": str(sandbox / "data")}
        cwd = sandbox
    x0, t0 = xacts(db.owner_url), cpu_seconds()
    r = subprocess.run(cmd, cwd=cwd, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    if r.returncode:
        raise RuntimeError(f"{engine} failed:\n{r.stdout[-4000:]}")
    cpu = cpu_seconds() - t0
    x1 = xacts(db.owner_url)
    res = json.loads(out.read_text())
    res |= {"cpu_s": cpu, "xacts": x1 - x0}
    return res


def med(xs):
    return statistics.median(xs)


def spread(xs):
    return {"median": med(xs), "min": min(xs), "max": max(xs)}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--label", default=None)
    ap.add_argument("--quick", action="store_true", help="1 repetition, one tenth of the rows")
    ap.add_argument("--reps", type=int, default=3)
    ap.add_argument("--workloads", default=",".join(w[0] for w in WORKLOADS))
    ap.add_argument("--variants", default="nosync,durable", help="nosync (synchronous_commit=off), durable (on)")
    ap.add_argument("--engines", default=",".join(ENGINES))
    ap.add_argument("--database-url", default=os.environ.get("OPENGTM_BENCH_DATABASE_URL")
                    or os.environ.get("OPENGTM_TEST_DATABASE_URL"))
    args = ap.parse_args()
    if not args.database_url:
        print("error: set OPENGTM_BENCH_DATABASE_URL to a disposable PostgreSQL superuser URL", file=sys.stderr)
        return 2
    workloads = [w for w in WORKLOADS if w[0] in args.workloads.split(",")]
    engines = [e for e in ENGINES if e in args.engines.split(",")]
    variants = [v for v in args.variants.split(",") if v in ("nosync", "durable")]
    if args.quick:
        args.reps = 1
    label = args.label or "enrich-" + dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    RESULTS.mkdir(parents=True, exist_ok=True)

    started = time.time()
    tmp = Path(tempfile.mkdtemp(prefix="opengtm-bench-enrich-"))
    sim: Simulator | None = None
    try:
        log("building the Go test binary and the provider simulator")
        go_bin, sim_bin = tmp / "enrich.test", tmp / "providersim"
        subprocess.run(["go", "test", "-c", "-o", str(go_bin), "./internal/jobs/enrich"], cwd=SERVER, check=True)
        subprocess.run(["go", "build", "-o", str(sim_bin), "./internal/bench/providersim"], cwd=SERVER, check=True)

        data_before = snapshot_data()
        sandbox = make_sandbox(tmp)
        (sandbox / "sitecustomize.py").write_text(SITECUSTOMIZE)
        py_manifests = sandbox / "apps" / "api" / "services" / "leadgen" / "enrichment" / "declarative" / "manifests" / "bench"
        go_connectors = tmp / "go-connectors"
        raw: list[dict] = []
        with Database(args.database_url, sandbox) as db:
            pg_version, pg_settings = db.settings()
            machine = machine_info(pg_version, pg_settings)
            for variant in variants:
                db.set_sync_commit(variant == "durable")
                for name, provider, rows, latency, conc, workers in workloads:
                    if variant == "durable":
                        if name not in DURABLE_ROWS:
                            continue
                        rows = DURABLE_ROWS[name]
                    if args.quick:
                        rows = max(rows // 10, 30)
                    sim = Simulator(sim_bin, latency, tmp)
                    write_connectors([py_manifests, go_connectors], sim.base)
                    log(f"{variant}/{name}: {rows} rows, {latency} ms provider, {conc} rows and {workers} calls in flight")
                    # a one-row run of the same engine gives its fixed cost (start-up, imports, pool),
                    # measured once per engine and workload and subtracted from every repetition
                    fixed = {}
                    for engine in engines:
                        _, job1 = seed(db.owner_url, provider, 1, conc, workers)
                        fixed[engine] = run_engine(engine, db, tmp, sandbox, go_bin, sim, job1, go_connectors)
                    for rep in range(args.reps):
                        for engine in engines:
                            base = fixed[engine]
                            wb, job = seed(db.owner_url, provider, rows, conc, workers)
                            res = run_engine(engine, db, tmp, sandbox, go_bin, sim, job, go_connectors)
                            verify(db.owner_url, wb, provider, rows)
                            res |= {"variant": variant, "workload": name, "engine": engine, "rep": rep, "rows": rows,
                                    "cells_per_sec": rows / res["wall_s"],
                                    "cpu_ms_per_cell": 1000 * (res["cpu_s"] - base["cpu_s"]) / max(rows - 1, 1),
                                    "xacts_per_1000_cells": 1000 * (res["xacts"] - base["xacts"]) / max(rows - 1, 1)}
                            raw.append(res)
                            log(f"  rep {rep + 1} {engine}: {res['cells_per_sec']:.0f} cells/s, "
                                f"{res['cpu_ms_per_cell']:.2f} cpu-ms/cell, {res['xacts_per_1000_cells']:.0f} xacts/1000")
                    sim.close()
                    sim = None
            db.set_sync_commit(True)
        machine["load_avg_end"] = os.getloadavg()
        if snapshot_data() != data_before:
            raise RuntimeError("the benchmark modified the repository's ./data directory; this is a bug")

        summary = summarize(raw)
        result = {
            "schema": SCHEMA, "label": label, "kind": "enrichment",
            "created_at": dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds"),
            "duration_s": round(time.time() - started, 1), "git": git_info(), "machine": machine,
            "config": {"reps": args.reps, "engines": engines, "variants": variants, "quick": args.quick,
                       "workloads": [dict(zip(("name", "provider", "rows", "latency_ms", "row_concurrency", "provider_workers"), w))
                                     for w in workloads]},
            "enrichment": {"summary": summary, "raw": raw},
            "metrics": flat_metrics(summary),
        }
        out = RESULTS / f"{label}.json"
        out.write_text(json.dumps(result, indent=2) + "\n")
        (RESULTS / f"{label}.md").write_text(render(result))
        log(f"wrote {out.relative_to(ROOT)} and .md ({result['duration_s']}s)")
        print()
        print(render(result))
        return 0
    finally:
        if sim:
            sim.close()
        shutil.rmtree(tmp, ignore_errors=True)


def summarize(raw: list[dict]) -> list[dict]:
    out = []
    keys = list(dict.fromkeys((r["variant"], r["workload"]) for r in raw))
    for variant, name in keys:
        for engine in ENGINES:
            rs = [r for r in raw if r["variant"] == variant and r["workload"] == name and r["engine"] == engine]
            if not rs:
                continue
            out.append({"variant": variant, "workload": name, "engine": engine, "reps": len(rs), "rows": rs[0]["rows"],
                        "wall_s": spread([r["wall_s"] for r in rs]),
                        "cells_per_sec": spread([r["cells_per_sec"] for r in rs]),
                        "cpu_ms_per_cell": spread([r["cpu_ms_per_cell"] for r in rs]),
                        "xacts_per_1000_cells": spread([r["xacts_per_1000_cells"] for r in rs]),
                        "max_rss_mb": spread([r["max_rss_mb"] for r in rs])})
    return out


def flat_metrics(summary: list[dict]) -> dict:
    m = {}
    for r in summary:
        base = f"enrich/{r['variant']}/{r['workload']}/{r['engine']}"
        m[f"{base}/cells_per_sec"] = {"value": r["cells_per_sec"]["median"], "unit": "cells/s", "better": "higher", "gated": True}
        m[f"{base}/cpu_ms_per_cell"] = {"value": r["cpu_ms_per_cell"]["median"], "unit": "ms", "better": "lower", "gated": False}
        m[f"{base}/xacts_per_1000_cells"] = {"value": r["xacts_per_1000_cells"]["median"], "unit": "xacts", "better": "lower", "gated": False}
    return m


def _f(x: float) -> str:
    return f"{x:,.0f}" if x >= 100 else f"{x:.1f}" if x >= 10 else f"{x:.2f}"


def render(result: dict) -> str:
    m, git, cfg = result["machine"], result["git"], result["config"]
    out = []
    w = out.append
    w(f"# Enrichment benchmark: {result['label']}\n")
    w(f"- Created: {result['created_at']} (took {result['duration_s']} s)")
    w(f"- Commit: `{git['commit'][:12]}` on `{git['branch']}`{' (working tree dirty)' if git['dirty'] else ''}")
    w(f"- Machine: {m['cpu']}, {m['logical_cores']} logical cores, {m['ram_gb']} GB RAM, {m['os']}")
    la = m.get("load_avg_start"), m.get("load_avg_end")
    w(f"- Load average at start: {', '.join(f'{x:.1f}' for x in la[0])}; at end: "
      f"{', '.join(f'{x:.1f}' for x in la[1]) if la[1] else 'n/a'} (a shared machine: read ratios, not absolutes)")
    w(f"- Versions: {m['go']}; Python {m['python']}; PostgreSQL {m['postgres']}")
    w(f"- {cfg['reps']} repetition(s), engines interleaved; cells are medians, `range` is min-max.\n")
    w("One cell = one completed enrichment of one row by one connector: the provider call, the result and its "
      "evidence written, and for `paid` the spend reservation, dispatch and settlement. `cells/s` = rows / wall "
      "time of the job. `cpu ms/cell` is the marginal CPU time of the whole process tree (pool workers included). "
      "`xacts` are PostgreSQL commits per 1,000 cells. `RSS` is the largest process.\n")
    notes = {
        "nosync": "`synchronous_commit=off` for the benchmark role: the flush wait is gone, so the numbers reflect "
                  "code, SQL and driver cost.",
        "durable": "`synchronous_commit=on` (the PostgreSQL default): every commit waits for an fsync, so on this "
                   "disk both engines are bounded by flushes and fewer commits per cell is the only way to win.",
    }
    pairs = [(v, wl) for v in cfg["variants"] for wl in cfg["workloads"]]
    for variant, wl in pairs:
        rows = [r for r in result["enrichment"]["summary"] if r["workload"] == wl["name"] and r["variant"] == variant]
        if not rows:
            continue
        w(f"## {variant} / {wl['name']}: {rows[0]['rows']} rows, {wl['latency_ms']} ms provider latency, "
          f"{wl['row_concurrency']} rows and {wl['provider_workers']} provider calls in flight\n")
        w(notes[variant] + "\n")
        w("| engine | wall s | cells/s | cells/s range | cpu ms/cell | xacts/1000 cells | peak RSS MB | vs python-pool |")
        w("|---|---:|---:|---|---:|---:|---:|---:|")
        pool = next((r for r in rows if r["engine"] == "python-pool"), None)
        for r in rows:
            ratio = f"{r['cells_per_sec']['median'] / pool['cells_per_sec']['median']:.2f}x" if pool else "n/a"
            w(f"| {r['engine']} | {_f(r['wall_s']['median'])} | {_f(r['cells_per_sec']['median'])} | "
              f"{_f(r['cells_per_sec']['min'])}-{_f(r['cells_per_sec']['max'])} | {_f(r['cpu_ms_per_cell']['median'])} | "
              f"{_f(r['xacts_per_1000_cells']['median'])} | {_f(r['max_rss_mb']['median'])} | {ratio} |")
        w("")
    return "\n".join(out) + "\n"


if __name__ == "__main__":
    raise SystemExit(main())
