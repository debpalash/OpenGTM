"""Render a benchmark result JSON as Markdown tables.

    python benchmarks/report.py benchmarks/results/baseline.json
"""

from __future__ import annotations

import json
import sys

VARIANT_NOTES = {
    "durable": "`synchronous_commit=on` (PostgreSQL default, what production uses): every claim and every "
    "completion waits for an fsync, so this is mostly a measurement of the disk and PostgreSQL.",
    "nosync": "`synchronous_commit=off` for the benchmark role: removes the fsync wait so the numbers reflect "
    "queue code, SQL and driver cost rather than the disk.",
}
MODE_LABEL = {
    "go-queue": "Go queue",
    "python-inproc": "Python QueueService (handler in-process)",
    "python-subprocess": "Python QueueService (production child process)",
}
MODE_ORDER = ["go-queue", "python-inproc", "python-subprocess"]


def _f(x: float, digits: int = 2) -> str:
    if x >= 1000:
        return f"{x:,.0f}"
    if x >= 100:
        return f"{x:.0f}"
    if x >= 10:
        return f"{x:.1f}"
    return f"{x:.{digits}f}"


def _cell(spread: dict) -> str:
    return _f(spread["median"])


def _range(spread: dict) -> str:
    return f"{_f(spread['min'])}-{_f(spread['max'])}"


def render(result: dict) -> str:
    m, cfg, git = result["machine"], result["config"], result["git"]
    out: list[str] = []
    w = out.append
    w(f"# Benchmark results: {result['label']}\n")
    w(f"- Created: {result['created_at']} (took {result['duration_s']} s)")
    w(f"- Commit: `{git['commit'][:12]}` on `{git['branch']}`{' (working tree dirty)' if git['dirty'] else ''}")
    w(f"- Machine: {m['cpu']}, {m['logical_cores']} logical cores, {m['ram_gb']} GB RAM, {m['os']}")
    la = m.get("load_avg_start"), m.get("load_avg_end")
    w(f"- Load average (1/5/15 min) at start: {', '.join(f'{x:.1f}' for x in la[0])}; "
      f"at end: {', '.join(f'{x:.1f}' for x in la[1]) if la[1] else 'n/a'}")
    w(f"- Versions: {m['go']}; Python {m['python']}; PostgreSQL {m['postgres']}")
    w(f"- PostgreSQL settings: {', '.join(f'{k}={v}' for k, v in m['postgres_settings'].items())}")
    w("- Single machine, shared with other processes. Cells are the **median of N repetitions**; "
      "`range` is min-max across repetitions. Read it as an indication, not a guarantee.\n")

    q = result["queue"]["summary"]
    if q:
        w("## Queue: claim and completion of no-op jobs\n")
        w("`jobs/s` is jobs completed per second over the whole drain of a pre-filled backlog. "
          "`claim` is the time of the claim call alone; `cycle` is claim plus running the no-op handler plus "
          "the guarded completion commit, per job. Times are milliseconds. "
          "Same PostgreSQL, same role, same machine for both engines.\n")
        for variant in ("durable", "nosync"):
            rows = [r for r in q if r["variant"] == variant]
            if not rows:
                continue
            w(f"### {variant}\n")
            w(VARIANT_NOTES[variant] + "\n")
            w("| engine | slots | jobs | reps | jobs/s | jobs/s range | claim p50 | claim p95 | claim p99 | "
              "cycle p50 | cycle p95 | cycle p99 | enqueue/s |")
            w("|---|---:|---:|---:|---:|---|---:|---:|---:|---:|---:|---:|---:|")
            rows.sort(key=lambda r: (MODE_ORDER.index(r["mode"]), r["concurrency"]))
            for r in rows:
                w(f"| {MODE_LABEL[r['mode']]} | {r['concurrency']} | {r['jobs']} | {r['reps']} | "
                  f"{_cell(r['jobs_per_sec'])} | {_range(r['jobs_per_sec'])} | {_cell(r['claim_ms_p50'])} | "
                  f"{_cell(r['claim_ms_p95'])} | {_cell(r['claim_ms_p99'])} | {_cell(r['cycle_ms_p50'])} | "
                  f"{_cell(r['cycle_ms_p95'])} | {_cell(r['cycle_ms_p99'])} | {_cell(r['enqueue_per_sec'])} |")
            w("")
            ratios = _ratios(rows)
            if ratios:
                w("Go vs Python (in-process handler): throughput ratio (>1 means Go is faster) and cycle p50 ratio "
                  "(Python / Go).\n")
                w("| slots | jobs/s ratio | cycle p50 ratio |")
                w("|---:|---:|---:|")
                for conc, tp, lat in ratios:
                    w(f"| {conc} | {tp:.2f}x | {lat:.2f}x |")
                w("")

    r_sum = result.get("retention", {}).get("summary", [])
    if r_sum:
        _render_retention(w, r_sum, cfg)

    h = result["http"]["summary"]
    if h:
        w("## HTTP front door\n")
        info = cfg.get("http", {})
        w(f"Closed-loop load generator (`apps/server/internal/bench/loadgen`), keep-alive connections, "
          f"{cfg['http_duration']} s measured window after {cfg['http_warmup']} s warm-up, "
          f"{cfg['http_reps']} repetitions. FastAPI runs under uvicorn with {info.get('fastapi_workers')} workers "
          f"(the Dockerfile default). {info.get('access_logging', '')} "
          f"Plugins in the Go catalog: {info.get('plugins_in_catalog')}. `go-plugins-authed` is served from the "
          "authorization cache after the first request, so it measures the Go handler, not a FastAPI round trip.\n")
        w("| endpoint | conns | req/s | req/s range | p50 ms | p95 ms | p99 ms | errors |")
        w("|---|---:|---:|---|---:|---:|---:|---:|")
        order = ["fastapi-health-direct", "fastapi-health-via-go-proxy", "fastapi-authed-direct",
                 "fastapi-authed-via-go-proxy", "go-version", "go-plugins-authed"]
        for r in sorted(h, key=lambda r: (order.index(r["name"]) if r["name"] in order else 99, r["concurrency"])):
            w(f"| {r['name']} | {r['concurrency']} | {_cell(r['rps'])} | {_range(r['rps'])} | {_cell(r['p50_ms'])} | "
              f"{_cell(r['p95_ms'])} | {_cell(r['p99_ms'])} | {r['errors']} |")
        w("")
        over = _proxy_overhead(h)
        if over:
            w("Proxy overhead (through Go minus direct to FastAPI, medians):\n")
            w("| endpoint | conns | added p50 ms | req/s change |")
            w("|---|---:|---:|---:|")
            for name, conc, dp50, drps in over:
                w(f"| {name} | {conc} | {dp50:+.2f} | {drps:+.1f}% |")
            w("")
    return "\n".join(out) + "\n"


RETENTION_LABEL = {
    "go-handler": "Go handler (`Worker.Handle`)",
    "python-handler": "Python handler (`handle_retention_enforce`, in-process)",
    "go-job": "Go, whole job (claim, handler, finalize)",
    "python-job": "Python, whole job (claim, child process, handler, finalize)",
}
RETENTION_ORDER = ["python-handler", "go-handler", "python-job", "go-job"]


def _render_retention(w, rows, cfg) -> None:
    w("## retention_enforce: Go and Python\n")
    w("One workspace per case, seeded with `rows` expired and "
      f"{rows[0]['kept']} recent rows in **each** of the 8 tables the job purges "
      "(`benchmarks/retention_seed.sql`), default 365/180-day windows, so a run deletes `8 x rows` rows. "
      "Wall time is milliseconds from just before the handler is called (handler rows) or from the job insert "
      "(whole-job rows) until the run is `completed` and verified; seeding is excluded. Same PostgreSQL, same "
      "NOSUPERUSER NOBYPASSRLS role, engines interleaved per repetition, every case starts from empty tables. "
      "The Python whole-job rows include the child process the production queue spawns for every job (interpreter "
      "start and the full handler-registry import); the Go whole-job rows use the real queue with a 5 ms idle "
      "poll. Cells are medians; the range is min-max across repetitions.\n")
    for variant in ("durable", "nosync"):
        part = [r for r in rows if r["variant"] == variant]
        if not part:
            continue
        w(f"### {variant}\n")
        w(VARIANT_NOTES[variant] + "\n")
        w("| engine | expired rows per table | rows deleted | reps | wall ms (median) | wall ms range | rows deleted/s |")
        w("|---|---:|---:|---:|---:|---|---:|")
        part.sort(key=lambda r: (r["rows"], RETENTION_ORDER.index(r["mode"])))
        for r in part:
            w(f"| {RETENTION_LABEL[r['mode']]} | {r['rows']:,} | {r['deleted']:,} | {r['reps']} | {_cell(r['wall_ms'])} | "
              f"{_range(r['wall_ms'])} | {_f(r['rows_per_sec']['median'])} |")
        w("")
        w("Python / Go wall-time ratio of the medians (>1 means Go is faster):\n")
        w("| expired rows per table | handler | whole job |")
        w("|---:|---:|---:|")
        for n in sorted({r["rows"] for r in part}):
            cells = []
            for py, go in (("python-handler", "go-handler"), ("python-job", "go-job")):
                a, b = _find(part, mode=py, rows=n), _find(part, mode=go, rows=n)
                cells.append(f"{a['wall_ms']['median'] / b['wall_ms']['median']:.2f}x" if a and b else "-")
            w(f"| {n:,} | {cells[0]} | {cells[1]} |")
        w("")


def _find(items, **kw):
    for r in items:
        if all(r.get(k) == v for k, v in kw.items()):
            return r
    return None


def _ratios(rows):
    out = []
    for conc in sorted({r["concurrency"] for r in rows}):
        go = _find(rows, mode="go-queue", concurrency=conc)
        py = _find(rows, mode="python-inproc", concurrency=conc)
        if go and py:
            out.append((conc, go["jobs_per_sec"]["median"] / py["jobs_per_sec"]["median"],
                        py["cycle_ms_p50"]["median"] / go["cycle_ms_p50"]["median"]))
    return out


def _proxy_overhead(rows):
    out = []
    for direct, proxied, name in (("fastapi-health-direct", "fastapi-health-via-go-proxy", "/health"),
                                  ("fastapi-authed-direct", "fastapi-authed-via-go-proxy",
                                   "/api/auth/workspace-context")):
        for conc in sorted({r["concurrency"] for r in rows}):
            d, p = _find(rows, name=direct, concurrency=conc), _find(rows, name=proxied, concurrency=conc)
            if d and p:
                out.append((name, conc, p["p50_ms"]["median"] - d["p50_ms"]["median"],
                            (p["rps"]["median"] / d["rps"]["median"] - 1) * 100))
    return out


if __name__ == "__main__":
    with open(sys.argv[1]) as f:
        sys.stdout.write(render(json.load(f)))
