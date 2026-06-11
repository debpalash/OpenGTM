# searxng-pool

A high-concurrency **search gateway in Go** that fronts a pool of SearXNG
instances and exposes one JSON API for Yupcha's website-independent enrichment
providers (`ddg_company`, `social_finder`, `crosslinked`, `local_business`,
`email_harvester`, `company_intel`).

It replaces direct DuckDuckGo calls — which get CAPTCHA'd from a single server
IP — with a health-checked, load-balanced pool that fans each query out across
several instances concurrently and merges the results.

## Why a pool

- A single SearXNG instance's IP gets blocked by Google/DDG/Brave. A **pool**
  spreads load and gives IP diversity.
- Go's goroutines make the fan-out / hedge / merge cheap and concurrent.
- Health checks track **liveness AND JSON support AND latency**, so dead or
  JSON-disabled instances are skipped automatically.

## ⚠️ Critical operational finding (from the eval)

**Public SearXNG instances are NOT usable as a JSON backend.** Probed live: every
public instance returns **HTTP 429/403 on the first JSON request** (they
rate-limit/bot-gate the JSON API) or disable `format=json` entirely. So:

> **The pool must be made of OUR OWN self-hosted instances.** Public instances
> are a best-effort bonus tier (`SEARXPOOL_PUBLIC_FEED=true`) that will almost
> always be skipped as not-JSON-capable.

The way to scale this is **horizontal self-hosting**: run N SearXNG containers,
each optionally behind a different egress proxy/IP, and let this pool balance
across them. Engine reality from our IP: **Bing/Qwant/Yahoo work; Google/DDG/
Brave CAPTCHA** → set `SEARXPOOL_DEFAULT_ENGINES=bing,qwant,yahoo` (or add a
keyed Brave/Serper engine inside SearXNG to recover Google-quality results).

## API

| Endpoint | Purpose |
|---|---|
| `GET /search?q=&engines=&n=` | normalized, deduped results: `{query, results:[{title,url,content,engines}], instances_used, elapsed_ms}` |
| `GET /health` | `{ok, instances, healthy, json_capable}` |
| `GET /pool` | full instance table (url, grade, healthy, json_ok, latency, fails) |

## Run

```bash
# point at our self-hosted SearXNG (see infra/searxng), working engines, own pool only
SEARXPOOL_DEFAULT_ENGINES="bing,qwant,yahoo" \
SEARXPOOL_PUBLIC_FEED=false \
SEARXPOOL_SEED="http://localhost:8888" \
go run .            # listens on :8889
```

## Config (env)

| Var | Default | Meaning |
|---|---|---|
| `SEARXPOOL_ADDR` | `:8889` | listen address |
| `SEARXPOOL_SEED` | `http://localhost:8888` | always-included instances (our own), CSV |
| `SEARXPOOL_PROXIES` | — | outbound proxies, round-robined across instances, CSV |
| `SEARXPOOL_PUBLIC_FEED` | `true` | also pull searx.space public instances (mostly JSON-blocked) |
| `SEARXPOOL_MIN_GRADE` | `B` | min searx.space HTTP grade to admit |
| `SEARXPOOL_MAX_INSTANCES` | `40` | pool size cap |
| `SEARXPOOL_DEFAULT_ENGINES` | — | engines passed to SearXNG (empty = instance default) |
| `SEARXPOOL_FANOUT` | `4` | instances queried per search |
| `SEARXPOOL_MIN_RESPONSES` | `2` | return once this many instances answered |
| `SEARXPOOL_PER_INSTANCE_GAP_MS` | `1500` | politeness gap between hits to one instance |
| `SEARXPOOL_*_TIMEOUT_S` | 8/12/9 | health / search / per-try budgets |

## Design (files)

- `config.go` — env config
- `pool.go` — `Instance` + `Pool`: bootstrap (seed + searx.space feed), background
  health loop (probes liveness + JSON), latency/fail-aware `Pick()`
- `search.go` — concurrent fan-out, merge + URL-dedup, engine attribution
- `main.go` — HTTP server, graceful shutdown

Stdlib only — no external deps, so `go build .` produces a single static binary.

## Roadmap to scale (our own pool)

1. `infra/searxng` runs 1 instance today. Add a `docker-compose` that runs
   **N instances** (`searxng-1..N`), seed them all into `SEARXPOOL_SEED`.
2. Give each instance a distinct **egress proxy** (`SEARXPOOL_PROXIES`) to
   diversify IPs → recover Google/DDG/Brave engines.
3. Optionally add a **Brave Search API / Serper** engine *inside* each SearXNG
   (keyed engines don't CAPTCHA) for Google-quality results without proxies.
