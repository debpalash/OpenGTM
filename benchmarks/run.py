"""M0 baseline benchmark orchestrator. Run it through benchmarks/run.sh.

Creates a throwaway PostgreSQL database and role, runs the queue and HTTP
benchmarks against it for the Python stack and the Go server, writes
benchmarks/results/<label>.json (+ .md), and drops everything it created.
It never touches the repository's ./data directory or any other database.
"""

from __future__ import annotations

import argparse
import contextlib
import datetime as dt
import json
import os
import platform
import secrets
import shutil
import signal
import socket
import statistics
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path
from urllib.parse import urlsplit, urlunsplit

import psycopg

# Before the pool fix (docs/internal or benchmarks/README.md, "The FastAPI
# wedge") the authenticated FastAPI route stopped answering at 64 concurrent
# connections against two uvicorn workers: every request timed out and the
# workers stayed wedged. Baselines taken before that fix cap those endpoints at
# 32 connections; the default is now uncapped (the highest level driven) and
# ``--fastapi-authed-max-conns`` restores a cap to benchmark an older checkout.

ROOT = Path(__file__).resolve().parent.parent
SERVER = ROOT / "apps" / "server"
RESULTS = ROOT / "benchmarks" / "results"
SCHEMA = 1

sys.path.insert(0, str(ROOT))
from benchmarks import report  # noqa: E402


def log(msg: str) -> None:
    print(f"[bench] {msg}", flush=True)


# ── environment ──────────────────────────────────────────────────────────


def sh(cmd: list[str], **kw) -> str:
    try:
        return subprocess.run(cmd, capture_output=True, text=True, check=True, **kw).stdout.strip()
    except Exception:
        return ""


def machine_info(pg_version: str, pg_settings: dict) -> dict:
    cpu = ""
    try:
        for line in Path("/proc/cpuinfo").read_text().splitlines():
            if line.startswith("model name"):
                cpu = line.split(":", 1)[1].strip()
                break
    except OSError:
        cpu = platform.processor()
    mem_gb = None
    try:
        for line in Path("/proc/meminfo").read_text().splitlines():
            if line.startswith("MemTotal"):
                mem_gb = round(int(line.split()[1]) / 1024 / 1024, 1)
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
    return {
        "cpu": cpu,
        "logical_cores": os.cpu_count(),
        "ram_gb": mem_gb,
        "os": os_name,
        "load_avg_start": os.getloadavg(),
        "go": sh(["go", "version"]),
        "python": platform.python_version(),
        "uv": sh(["uv", "--version"]),
        "postgres": pg_version,
        "postgres_settings": pg_settings,
    }


def git_info() -> dict:
    return {
        "commit": sh(["git", "rev-parse", "HEAD"], cwd=ROOT),
        "branch": sh(["git", "rev-parse", "--abbrev-ref", "HEAD"], cwd=ROOT),
        "dirty": bool(sh(["git", "status", "--porcelain", "--untracked-files=no"], cwd=ROOT)),
    }


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def with_db(url: str, dbname: str, user: str | None = None, password: str | None = None, scheme: str | None = None) -> str:
    p = urlsplit(url)
    host = p.hostname or "127.0.0.1"
    netloc = f"{user}:{password}@" if user else (f"{p.username}:{p.password}@" if p.username else "")
    netloc += host + (f":{p.port}" if p.port else "")
    return urlunsplit((scheme or p.scheme, netloc, "/" + dbname, "", ""))


# ── sandbox ──────────────────────────────────────────────────────────────


def make_sandbox(tmp: Path) -> Path:
    """Copy the Python sources the benchmark needs into a throwaway root.

    The FastAPI app writes SQLite files under <root>/data, resolves paths from
    ``__file__`` and reads <root>/.env. Running a copy keeps all of that inside
    the sandbox, so the benchmark can never touch the repository's ./data or
    pick up a developer's .env. The copy is made fresh from the working tree on
    every run, so it measures exactly the code you have checked out.
    """
    sb = tmp / "sandbox"
    ignore = shutil.ignore_patterns("__pycache__", "*.pyc", "node_modules", ".venv", "results")
    shutil.copytree(ROOT / "apps" / "api", sb / "apps" / "api", ignore=ignore)
    shutil.copytree(ROOT / "migrations", sb / "migrations", ignore=ignore)
    shutil.copytree(ROOT / "benchmarks", sb / "benchmarks", ignore=ignore)
    shutil.copy(ROOT / "alembic.ini", sb / "alembic.ini")
    (sb / "data").mkdir()
    return sb


def snapshot_data() -> dict:
    d = ROOT / "data"
    if not d.exists():
        return {}
    return {str(p.relative_to(d)): (p.stat().st_size, p.stat().st_mtime_ns) for p in d.rglob("*") if p.is_file()}


# ── throwaway database ───────────────────────────────────────────────────


class Database(contextlib.AbstractContextManager):
    def __init__(self, admin_url: str, sandbox: Path):
        self.admin_url = admin_url
        self.sandbox = sandbox
        tag = secrets.token_hex(4)
        self.name = f"opengtm_bench_{tag}"
        self.role = f"opengtm_bench_app_{tag}"
        self.password = "bench_only_" + secrets.token_hex(6)
        self.owner_url = with_db(admin_url, self.name, scheme="postgresql")
        self.app_url = with_db(admin_url, self.name, self.role, self.password, scheme="postgresql")
        self.app_sa_url = with_db(admin_url, self.name, self.role, self.password, scheme="postgresql+psycopg")
        self.owner_sa_url = with_db(admin_url, self.name, scheme="postgresql+psycopg")

    def __enter__(self):
        with psycopg.connect(self.admin_url, autocommit=True) as c:
            c.execute(f'CREATE DATABASE "{self.name}"')
        log(f"created throwaway database {self.name}")
        env = {**os.environ, "DATABASE_URL": self.owner_sa_url}
        subprocess.run([sys.executable, "-m", "alembic", "upgrade", "head"], cwd=self.sandbox, env=env, check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        with psycopg.connect(self.admin_url, autocommit=True) as c:
            c.execute(
                f"CREATE ROLE \"{self.role}\" LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '{self.password}'"
            )
        with psycopg.connect(self.owner_url, autocommit=True) as c:
            c.execute(f'GRANT USAGE ON SCHEMA public TO "{self.role}"')
            c.execute(f'GRANT yupcha_app TO "{self.role}"')
        return self

    def set_sync_commit(self, on: bool) -> None:
        with psycopg.connect(self.admin_url, autocommit=True) as c:
            c.execute(f'ALTER ROLE "{self.role}" SET synchronous_commit = {"on" if on else "off"}')

    def settings(self) -> tuple[str, dict]:
        with psycopg.connect(self.owner_url) as c:
            version = c.execute("SHOW server_version").fetchone()[0]
            keys = ["fsync", "synchronous_commit", "shared_buffers", "max_connections", "wal_sync_method"]
            cfg = {k: c.execute(f"SHOW {k}").fetchone()[0] for k in keys}
        return version, cfg

    def __exit__(self, *exc):
        try:
            with psycopg.connect(self.admin_url, autocommit=True) as c:
                c.execute(f'DROP DATABASE IF EXISTS "{self.name}" WITH (FORCE)')
                c.execute(f'DROP ROLE IF EXISTS "{self.role}"')
            log(f"dropped {self.name} and role {self.role}")
        except Exception as e:  # never mask the original error
            log(f"WARNING: cleanup of {self.name} failed: {e}")
        return False


# ── processes ────────────────────────────────────────────────────────────


class Proc:
    def __init__(self, name: str, cmd: list[str], env: dict, logfile: Path, cwd: Path = ROOT):
        self.name = name
        self.log = open(logfile, "wb")
        self.p = subprocess.Popen(cmd, cwd=cwd, env=env, stdout=self.log, stderr=subprocess.STDOUT,
                                  start_new_session=True)

    def stop(self) -> None:
        if self.p.poll() is None:
            with contextlib.suppress(ProcessLookupError):
                os.killpg(self.p.pid, signal.SIGTERM)
            try:
                self.p.wait(timeout=20)
            except subprocess.TimeoutExpired:
                with contextlib.suppress(ProcessLookupError):
                    os.killpg(self.p.pid, signal.SIGKILL)
        self.log.close()


def wait_http(url: str, proc: Proc, timeout: float = 180) -> None:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if proc.p.poll() is not None:
            raise RuntimeError(f"{proc.name} exited early (see its log); code {proc.p.returncode}")
        try:
            with urllib.request.urlopen(url, timeout=2) as r:
                if r.status == 200:
                    return
        except (urllib.error.URLError, OSError):
            pass
        time.sleep(0.3)
    raise RuntimeError(f"{proc.name} not ready at {url} after {timeout}s")


# ── aggregation ──────────────────────────────────────────────────────────


def med(xs):
    return statistics.median(xs)


def spread(xs):
    return {"median": med(xs), "min": min(xs), "max": max(xs)}


def summarize_queue(raw: list[dict]) -> list[dict]:
    groups: dict[tuple, list[dict]] = {}
    for c in raw:
        groups.setdefault((c["variant"], c["mode"], c["concurrency"]), []).append(c)
    out = []
    for (variant, mode, conc), cs in groups.items():
        row = {"variant": variant, "mode": mode, "concurrency": conc, "reps": len(cs), "jobs": cs[0]["jobs"],
               "jobs_per_sec": spread([c["jobs_per_sec"] for c in cs]),
               "enqueue_per_sec": spread([c["enqueue_per_sec"] for c in cs])}
        for part in ("claim_ms", "process_ms", "cycle_ms"):
            for q in ("p50", "p95", "p99"):
                row[f"{part}_{q}"] = spread([c[part][q] for c in cs])
        out.append(row)
    return out


def summarize_http(raw: list[dict]) -> list[dict]:
    groups: dict[tuple, list[dict]] = {}
    for r in raw:
        groups.setdefault((r["name"], r["concurrency"]), []).append(r)
    out = []
    for (name, conc), rs in groups.items():
        out.append({"name": name, "concurrency": conc, "reps": len(rs), "errors": sum(r["errors"] for r in rs),
                    "rps": spread([r["rps"] for r in rs]),
                    **{k: spread([r[k] for r in rs]) for k in ("p50_ms", "p95_ms", "p99_ms")}})
    return out


def summarize_retention(raw: list[dict]) -> list[dict]:
    groups: dict[tuple, list[dict]] = {}
    for c in raw:
        groups.setdefault((c["variant"], c["mode"], c["rows"]), []).append(c)
    out = []
    for (variant, mode, rows), cs in groups.items():
        wall = spread([c["wall_ms"] for c in cs])
        out.append({"variant": variant, "mode": mode, "rows": rows, "kept": cs[0]["kept"], "deleted": cs[0]["deleted"],
                    "reps": len(cs),
                    "wall_ms": wall,
                    "rows_per_sec": {k: cs[0]["deleted"] / (v / 1000) for k, v in
                                     (("median", wall["median"]), ("min", wall["max"]), ("max", wall["min"]))}})
    return out


def flat_metrics(queue: list[dict], http: list[dict], retention: list[dict] | None = None) -> dict:
    """Flat key -> {value, unit, better, gated} map that compare.py consumes.

    Throughput and p50/p95 are gated; p99 and component latencies are recorded
    but informational because they are too noisy on a shared machine to fail a
    build on.
    """
    m: dict[str, dict] = {}

    def put(key, value, unit, better, gated):
        m[key] = {"value": value, "unit": unit, "better": better, "gated": gated}

    for r in queue:
        base = f"queue/{r['variant']}/{r['mode']}/c{r['concurrency']}"
        put(f"{base}/jobs_per_sec", r["jobs_per_sec"]["median"], "jobs/s", "higher", True)
        put(f"{base}/enqueue_per_sec", r["enqueue_per_sec"]["median"], "jobs/s", "higher", False)
        for part in ("cycle_ms", "claim_ms"):
            for q in ("p50", "p95", "p99"):
                put(f"{base}/{part}_{q}", r[f"{part}_{q}"]["median"], "ms", "lower",
                    part == "cycle_ms" and q in ("p50", "p95"))
    for r in retention or []:
        base = f"retention/{r['variant']}/{r['mode']}/n{r['rows']}"
        put(f"{base}/wall_ms", r["wall_ms"]["median"], "ms", "lower", True)
        put(f"{base}/rows_per_sec", r["rows_per_sec"]["median"], "rows/s", "higher", False)
    for r in http:
        base = f"http/{r['name']}/c{r['concurrency']}"
        put(f"{base}/rps", r["rps"]["median"], "req/s", "higher", True)
        for q in ("p50_ms", "p95_ms", "p99_ms"):
            put(f"{base}/{q}", r[q]["median"], "ms", "lower", q != "p99_ms")
    return m


# ── queue benchmark ──────────────────────────────────────────────────────


def run_queue(args, db: Database, tmp: Path, go_test_bin: Path) -> list[dict]:
    raw: list[dict] = []
    variants = []
    if "durable" in args.variants:
        variants.append(("durable", True, args.durable_jobs, args.durable_warmup))
    if "nosync" in args.variants:
        variants.append(("nosync", False, args.nosync_jobs, args.nosync_warmup))
    sb = db.sandbox

    def run_go(variant, jobs, warmup, rep):
        out = tmp / f"go_{variant}_{rep}.json"
        env = {**os.environ, "OPENGTM_TEST_DATABASE_URL": db.owner_url, "OPENGTM_BENCH_OUT": str(out),
               "OPENGTM_BENCH_ROLE": db.role, "OPENGTM_BENCH_ROLE_PASSWORD": db.password, "OPENGTM_BENCH_JOBS": str(jobs),
               "OPENGTM_BENCH_WARMUP": str(warmup), "OPENGTM_BENCH_REPS": "1", "OPENGTM_BENCH_REP_OFFSET": str(rep),
               "OPENGTM_BENCH_CONCURRENCY": args.concurrency}
        subprocess.run([str(go_test_bin), "-test.run", "^TestBenchQueue$", "-test.v", "-test.timeout", "60m"],
                       cwd=SERVER / "internal" / "queue", env=env, check=True, stdout=subprocess.PIPE,
                       stderr=subprocess.STDOUT, text=True)
        return json.loads(out.read_text())["cases"]

    def run_py(mode, variant, jobs, warmup, rep):
        out = tmp / f"py_{mode}_{variant}_{rep}.json"
        cmd = [sys.executable, str(sb / "benchmarks" / "bench_queue_python.py"), "--owner-url", db.owner_url,
               "--app-url", db.app_sa_url, "--mode", mode, "--jobs", str(jobs), "--warmup", str(warmup),
               "--concurrency", args.concurrency, "--reps", "1", "--rep-offset", str(rep), "--out", str(out)]
        r = subprocess.run(cmd, cwd=sb, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        if r.returncode:
            raise RuntimeError(f"python {mode} benchmark failed:\n{r.stdout[-3000:]}")
        return json.loads(out.read_text())["cases"]

    for variant, sync_on, jobs, warmup in variants:
        db.set_sync_commit(sync_on)
        log(f"queue/{variant}: synchronous_commit={'on' if sync_on else 'off'}, {jobs} jobs, c={args.concurrency}")
        for rep in range(args.queue_reps):
            # Engines are interleaved within a repetition so slow drift in
            # machine load hits all of them alike instead of favouring one.
            log(f"  rep {rep + 1}/{args.queue_reps}: go")
            for c in run_go(variant, jobs, warmup, rep):
                raw.append({**c, "variant": variant})
            log(f"  rep {rep + 1}/{args.queue_reps}: python-inproc")
            for c in run_py("inproc", variant, jobs, warmup, rep):
                raw.append({**c, "variant": variant})
        if variant == "durable" and not args.skip_subprocess:
            for rep in range(args.queue_reps):
                log(f"  rep {rep + 1}/{args.queue_reps}: python-subprocess ({args.subprocess_jobs} jobs)")
                for c in run_py("subprocess", variant, args.subprocess_jobs, args.subprocess_warmup, rep):
                    raw.append({**c, "variant": variant})
    db.set_sync_commit(True)
    return raw


# ── retention_enforce benchmark ──────────────────────────────────────────


def run_retention(args, db: Database, tmp: Path, go_test_bin: Path) -> list[dict]:
    """Go and Python retention_enforce on identical, freshly seeded workspaces.

    Everything runs against the same throwaway database and role. `durable`
    is PostgreSQL's default synchronous_commit=on (what production uses);
    `nosync` removes the fsync wait so the numbers reflect code and SQL cost
    (a retention job commits three times, and a stalled fsync on a shared disk
    can add hundreds of milliseconds to any single case). The engines are
    interleaved within a repetition so slow drift hits both alike.
    """
    seed_sql = ROOT / "benchmarks" / "retention_seed.sql"
    raw: list[dict] = []
    for variant in args.retention_variants:
        db.set_sync_commit(variant == "durable")
        for rep in range(args.retention_reps):
            out = tmp / f"go_retention_{variant}_{rep}.json"
            env = {**os.environ, "OPENGTM_TEST_DATABASE_URL": db.owner_url, "OPENGTM_BENCH_OUT": str(out),
                   "OPENGTM_BENCH_ROLE": db.role, "OPENGTM_BENCH_ROLE_PASSWORD": db.password,
                   "OPENGTM_BENCH_SEED_SQL": str(seed_sql), "OPENGTM_BENCH_ROWS": args.retention_rows,
                   "OPENGTM_BENCH_REPS": "1", "OPENGTM_BENCH_REP_OFFSET": str(rep)}
            log(f"retention/{variant} rep {rep + 1}/{args.retention_reps}: go ({args.retention_rows} expired rows per table)")
            subprocess.run([str(go_test_bin), "-test.run", "^TestBenchRetention$", "-test.v", "-test.timeout", "120m"],
                           cwd=SERVER / "internal" / "jobs" / "retention", env=env, check=True,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            raw += [{**c, "variant": variant} for c in json.loads(out.read_text())["cases"]]

            out = tmp / f"py_retention_{variant}_{rep}.json"
            cmd = [sys.executable, str(db.sandbox / "benchmarks" / "bench_retention_python.py"),
                   "--owner-url", db.owner_url, "--app-url", db.app_sa_url, "--seed-sql", str(seed_sql),
                   "--rows", args.retention_rows, "--reps", "1", "--rep-offset", str(rep), "--out", str(out)]
            log(f"retention/{variant} rep {rep + 1}/{args.retention_reps}: python")
            r = subprocess.run(cmd, cwd=db.sandbox, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
            if r.returncode:
                raise RuntimeError(f"python retention benchmark failed:\n{r.stdout[-3000:]}")
            raw += [{**c, "variant": variant} for c in json.loads(out.read_text())["cases"]]
    db.set_sync_commit(True)
    return raw


# ── HTTP benchmark ───────────────────────────────────────────────────────


def run_http(args, db: Database, tmp: Path, opengtm_bin: Path, loadgen_bin: Path) -> tuple[list[dict], dict]:
    sb = db.sandbox
    env = {**os.environ, "DATABASE_URL": db.app_sa_url, "RUN_INLINE_WORKER": "0"}
    seed = subprocess.run([sys.executable, str(sb / "benchmarks" / "seed_http.py")], cwd=sb, env=env,
                          capture_output=True, text=True)
    if seed.returncode:
        raise RuntimeError("seeding failed:\n" + seed.stderr[-3000:])
    token = json.loads(seed.stdout.strip().splitlines()[-1])["token"]
    auth = f"Authorization: Bearer {token}"

    api_port, go_port = free_port(), free_port()
    procs: list[Proc] = []
    try:
        api = Proc("fastapi", [sys.executable, "-m", "uvicorn", "apps.api.main:app", "--host", "127.0.0.1",
                               "--port", str(api_port), "--workers", str(args.api_workers)],
                   env, tmp / "fastapi.log", cwd=sb)
        procs.append(api)
        wait_http(f"http://127.0.0.1:{api_port}/health", api)
        go_env = {**os.environ, "DATABASE_URL": db.app_url, "OPENGTM_LISTEN": f"127.0.0.1:{go_port}",
                  "OPENGTM_LEGACY_API_URL": f"http://127.0.0.1:{api_port}",
                  "OPENGTM_PLUGIN_DIRS": str(ROOT / "plugins" / "examples")}
        go = Proc("opengtm", [str(opengtm_bin), "serve"], go_env, tmp / "opengtm.log")
        procs.append(go)
        wait_http(f"http://127.0.0.1:{go_port}/healthz", go)

        a, g = f"http://127.0.0.1:{api_port}", f"http://127.0.0.1:{go_port}"
        endpoints = [
            ("fastapi-health-direct", f"{a}/health", []),
            ("fastapi-health-via-go-proxy", f"{g}/health", []),
            ("fastapi-authed-direct", f"{a}/api/auth/workspace-context", [auth], args.fastapi_authed_max_conns),
            ("fastapi-authed-via-go-proxy", f"{g}/api/auth/workspace-context", [auth], args.fastapi_authed_max_conns),
            ("go-version", f"{g}/api/v2/version", []),
            ("go-plugins-authed", f"{g}/api/v2/plugins", [auth]),
        ]
        if args.endpoints:
            wanted = set(args.endpoints.split(","))
            unknown = wanted - {e[0] for e in endpoints}
            if unknown:
                raise RuntimeError(f"unknown --endpoints {sorted(unknown)}")
            endpoints = [e for e in endpoints if e[0] in wanted]
        levels = [int(x) for x in args.http_concurrency.split(",")]
        raw: list[dict] = []
        for rep in range(args.http_reps):
            log(f"http rep {rep + 1}/{args.http_reps}")
            for name, url, hdrs, *cap in endpoints:
                for c in levels:
                    if cap and cap[0] and c > cap[0]:
                        continue
                    cmd = [str(loadgen_bin), "-url", url, "-name", name, "-c", str(c),
                           "-duration", f"{args.http_duration}s", "-warmup", f"{args.http_warmup}s"]
                    for h in hdrs:
                        cmd += ["-H", h]
                    r = subprocess.run(cmd, capture_output=True, text=True)
                    if r.returncode:
                        raise RuntimeError(f"loadgen failed for {name}: {r.stderr.strip()}")
                    res = json.loads(r.stdout)
                    if res["errors"] and r.stderr.strip():
                        log(f"  {name} c={c}: {r.stderr.strip()}")
                    res["rep"] = rep
                    res.pop("url", None)  # ports are random; keep the file reproducible
                    raw.append(res)
                    log(f"  {name} c={c}: {res['rps']:.0f} req/s p50={res['p50_ms']:.2f}ms p99={res['p99_ms']:.2f}ms"
                        f"{' ERRORS=' + str(res['errors']) if res['errors'] else ''}")
        plugins = len(json.loads(urllib.request.urlopen(urllib.request.Request(
            f"{g}/api/v2/plugins", headers={"Authorization": f"Bearer {token}"})).read())["plugins"])
        info = {"fastapi_workers": args.api_workers, "plugins_in_catalog": plugins,
                "access_logging": "Access logging is enabled on both servers (written to a file), as in the default deployment."}
        return raw, info
    except BaseException:
        # Keep the server logs of a failed run; the temp dir is deleted on exit.
        for p in procs:
            p.log.flush()
            shutil.copy(p.log.name, RESULTS / f"last-failure-{p.name}.log")
        raise
    finally:
        for p in reversed(procs):
            p.stop()


# ── main ─────────────────────────────────────────────────────────────────


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--label", default=None, help="result name (default: run-<UTC timestamp>)")
    ap.add_argument("--quick", action="store_true", help="tiny smoke run (1 rep, few jobs, short windows)")
    ap.add_argument("--only", choices=["queue", "http", "retention"],
                    help="run only one part (retention: retention_enforce on Go and Python; not part of a default run)")
    ap.add_argument("--retention", action="store_true", help="also run the retention_enforce benchmark")
    ap.add_argument("--retention-rows", default="1000,20000,100000",
                    help="expired rows seeded into each of the 8 purged tables, per case")
    ap.add_argument("--retention-reps", type=int, default=5)
    ap.add_argument("--retention-variants", default="durable,nosync", help="durable (synchronous_commit=on), nosync")
    ap.add_argument("--concurrency", default="1,4,16", help="queue slot counts")
    ap.add_argument("--variants", default="durable,nosync", help="queue DB variants: durable,nosync")
    ap.add_argument("--queue-reps", type=int, default=3)
    ap.add_argument("--durable-jobs", type=int, default=300)
    ap.add_argument("--durable-warmup", type=int, default=50)
    ap.add_argument("--nosync-jobs", type=int, default=2000)
    ap.add_argument("--nosync-warmup", type=int, default=200)
    ap.add_argument("--subprocess-jobs", type=int, default=24)
    ap.add_argument("--subprocess-warmup", type=int, default=4)
    ap.add_argument("--skip-subprocess", action="store_true")
    ap.add_argument("--http-reps", type=int, default=3)
    ap.add_argument("--http-concurrency", default="1,16,64")
    ap.add_argument("--http-duration", type=float, default=4.0, help="seconds per measured window")
    ap.add_argument("--http-warmup", type=float, default=1.5)
    ap.add_argument("--fastapi-authed-max-conns", type=int, default=0,
                    help="cap the connections used against the authenticated FastAPI endpoints (0 = no cap; "
                         "use 32 to benchmark a checkout from before the pool fix, which wedges at 64)")
    ap.add_argument("--endpoints", default="",
                    help="comma-separated endpoint labels to drive (default: all); e.g. fastapi-authed-direct")
    ap.add_argument("--api-workers", type=int, default=2, help="uvicorn workers (the Dockerfile uses 2)")
    ap.add_argument("--database-url", default=os.environ.get("OPENGTM_BENCH_DATABASE_URL")
                    or os.environ.get("OPENGTM_TEST_DATABASE_URL"),
                    help="superuser URL of a DISPOSABLE PostgreSQL server (env: OPENGTM_BENCH_DATABASE_URL, "
                         "then OPENGTM_TEST_DATABASE_URL). Only throwaway databases are created in it.")
    args = ap.parse_args()
    args.variants = [v for v in args.variants.split(",") if v]
    args.retention_variants = [v for v in args.retention_variants.split(",") if v]
    if args.quick:
        args.queue_reps = args.http_reps = args.retention_reps = 1
        args.retention_rows = "500,2000"
        args.durable_jobs, args.durable_warmup = 60, 10
        args.nosync_jobs, args.nosync_warmup = 200, 20
        args.subprocess_jobs, args.subprocess_warmup = 4, 1
        args.concurrency, args.http_concurrency = "1,4", "1,16"
        args.http_duration, args.http_warmup = 1.5, 0.5
    if not args.database_url:
        print("error: set OPENGTM_BENCH_DATABASE_URL (or OPENGTM_TEST_DATABASE_URL) to a disposable "
              "PostgreSQL superuser URL, e.g. postgresql://postgres:postgres@127.0.0.1:55432/postgres",
              file=sys.stderr)
        return 2
    label = args.label or "run-" + dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    RESULTS.mkdir(parents=True, exist_ok=True)

    started = time.time()
    tmp = Path(tempfile.mkdtemp(prefix="opengtm-bench-"))
    try:
        log("building Go binaries")
        go_test_bin, opengtm_bin, loadgen_bin = tmp / "queue.test", tmp / "opengtm", tmp / "loadgen"
        retention_test_bin = tmp / "retention.test"
        want_retention = args.only == "retention" or args.retention
        if args.only != "retention":
            subprocess.run(["go", "test", "-c", "-o", str(go_test_bin), "./internal/queue"], cwd=SERVER, check=True)
        if want_retention:
            subprocess.run(["go", "test", "-c", "-o", str(retention_test_bin), "./internal/jobs/retention"],
                           cwd=SERVER, check=True)
        if args.only != "retention":
            subprocess.run(["go", "build", "-o", str(opengtm_bin), "./cmd/opengtm"], cwd=SERVER, check=True)
            subprocess.run(["go", "build", "-o", str(loadgen_bin), "./internal/bench/loadgen"], cwd=SERVER, check=True)

        data_before = snapshot_data()
        sandbox = make_sandbox(tmp)
        with Database(args.database_url, sandbox) as db:
            pg_version, pg_settings = db.settings()
            machine = machine_info(pg_version, pg_settings)
            queue_raw: list[dict] = []
            http_raw: list[dict] = []
            http_info: dict = {}
            retention_raw: list[dict] = []
            if args.only in (None, "queue"):
                queue_raw = run_queue(args, db, tmp, go_test_bin)
            if args.only in (None, "http"):
                http_raw, http_info = run_http(args, db, tmp, opengtm_bin, loadgen_bin)
            if want_retention:
                retention_raw = run_retention(args, db, tmp, retention_test_bin)
        machine["load_avg_end"] = os.getloadavg()
        if snapshot_data() != data_before:
            raise RuntimeError("the benchmark modified the repository's ./data directory; this is a bug")

        queue_sum, http_sum = summarize_queue(queue_raw), summarize_http(http_raw)
        retention_sum = summarize_retention(retention_raw)
        result = {
            "schema": SCHEMA,
            "label": label,
            "created_at": dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds"),
            "duration_s": round(time.time() - started, 1),
            "git": git_info(),
            "machine": machine,
            "config": {k: v for k, v in vars(args).items() if k != "database_url"} | {"http": http_info},
            "queue": {"summary": queue_sum, "raw": queue_raw},
            "http": {"summary": http_sum, "raw": http_raw},
            "retention": {"summary": retention_sum, "raw": retention_raw},
            "metrics": flat_metrics(queue_sum, http_sum, retention_sum),
        }
        out = RESULTS / f"{label}.json"
        out.write_text(json.dumps(result, indent=2) + "\n")
        (RESULTS / f"{label}.md").write_text(report.render(result))
        log(f"wrote {out.relative_to(ROOT)} and .md ({result['duration_s']}s)")
        print()
        print(report.render(result))
        return 0
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    raise SystemExit(main())
