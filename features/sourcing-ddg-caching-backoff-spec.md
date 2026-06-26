<!-- Auto-generated sourcing P2/P3 spec (2026-06-26). Effort: medium. -->

## Mini-spec: DDG result caching + adaptive backoff

### Goal
Cut redundant DuckDuckGo/keyless-search traffic (the dominant `no_data` cause) and replace blunt all-or-nothing failure handling with: (1) a short-TTL in-process result cache that dedupes identical search queries within a window, and (2) per-host adaptive exponential backoff driven by real rate-limit/block signals — so a transient DDG block degrades gracefully into the existing multi-engine/keyed fallback instead of either hammering or going dark for a fixed 900s.

### Grounded current state (file:line)

**The premise needs correction — there is no DDG-specific circuit breaker.**
- The only ~900s circuit breaker is `CIRCUIT_BREAKER_RECOVERY = 900` in `apps/api/services/leadgen/rate_limiter.py:30`, applied **per target domain** (`report_failure`/`is_blocked`, lines 114-129) and consumed only by the page-scraping client `StealthClient.fetch` (`apps/api/services/leadgen/http.py:233-237, 289`). It governs scraping fetches (yelp/linkedin/etc.), **not** DDG search.
- All DDG/keyless **search** flows through one chokepoint: `proxy_client.get_ddgs()` → `_ResilientDDGS` (`apps/api/services/leadgen/proxy_client.py:111-221`). `_run()` (lines 144-174) already does direct→proxy connection fallback (`SEARCH_ATTEMPT_ORDER`, line 95-97), multi-backend aggregation (`SEARCH_BACKENDS`, line 88), and keyed fallback to SerpAPI/Bing/Google-CSE/Brave via `search_engines.search_fallback` (lines 176-195, `search_engines.py:206-221`).
- **This search path has zero caching and zero backoff.** On any exception it swallows and returns `[]` (lines 159-174); rate-limit signals are not distinguished from empty results. There is no cooldown — every retry re-hits DDG immediately.
- **Heavy duplication is real.** `source_registry.py` issues ~91 `site:` queries per run; many enrichment providers re-issue near-identical queries: `job_runner.py:47-49`, `scrapers/job_boards.py:48`, `enrichment/{social_finder,search_enricher,email_finder,decision_maker_finder}.py`, and `enrichment/providers/{ddg_company_provider,jobspy_signals,facebook_pages,crosslinked,social_finder_provider,local_business,email_harvester,company_intel}.py` — all via `get_ddgs().text(...)`. Across a multi-row workspace run the same `site:linkedin.com "fintech" "VP Sales"` style query recurs many times.
- A **separate** search entry point exists: `enrichment/web_search.py` (`DDGS` over SearXNG, lines 69-93). Several enrichers import both but call `get_ddgs()` for the actual lookups. Scope below targets `_ResilientDDGS` (the dominant sourcing seam); SearXNG path is out of scope but the cache hook is written to be reusable.
- The blunt cooldown analog already exists for proxies/domains: `proxy_pool.py:28-29` (1800s blocked cooldown) and `rate_limiter.py:30` (900s) — both fixed, non-adaptive. This item introduces the adaptive curve the search layer lacks.
- Existing connection report seam: `_report_proxy_url` (`proxy_client.py:100-108`) — backoff success/failure can reuse this.

### Design

All changes live in **one new module + one wrapper edit** so the ~30 call sites are upgraded transparently (same as how `_ResilientDDGS` already upgrades them).

**1. Cache (`search_cache.py`, new)**
- Key: `sha1("text|" + normalized_query + "|" + str(max_results) + "|" + backends)`. Normalize = `query.strip().casefold()` collapse internal whitespace. `max_results` and `backends` are in the key so a `max_results=2` social lookup never serves a truncated answer to a `max_results=15` source. **Not keyed by workspace** — results are public web data; cross-workspace dedup is the entire point and there is no tenant-private input in a `site:` query (see adversarial pass).
- Store: process-local `OrderedDict` LRU with TTL per entry. Bounded by `SEARCH_CACHE_MAX_ENTRIES` (default 2000) and per-entry `SEARCH_CACHE_TTL` (default 600s / 10 min). Eviction: TTL-expired on read, LRU pop on insert past cap. Values are the already-normalized `list[dict]`.
- Negative caching: empty results cached with a **shorter** TTL `SEARCH_CACHE_EMPTY_TTL` (default 60s) so a momentarily-blocked query isn't pinned as "no data" for 10 min, but a genuinely dead `site:crunchbase.com` (per `docs/data-source-test-report.md`) isn't re-hammered every row.
- Thread-safe: `_run` is called under `asyncio.to_thread` (`job_runner.py:53-56`), so guard with a `threading.Lock`. Cheap dict ops; negligible contention.
- Memory bound math: 2000 entries × ~15 dicts × ~300B ≈ ~9 MB worst case. Documented and capped.

**2. Adaptive backoff (`search_backoff.py`, new, or co-located in `search_cache.py`)**
- Per-host state: host = the search backend identity. Since `ddgs` aggregates `SEARCH_BACKENDS`, the practical unit is the ddgs library endpoint as a whole plus each keyed engine; track under stable host labels: `"ddgs"`, `"serpapi"`, `"bing"`, `"google_cse"`, `"brave"`. `dict[host] -> (blocked_until, consecutive_blocks)`.
- Signal extraction: classify an attempt as a **block** when the raised exception is a ddgs ratelimit/timeout type or its `str()` matches `_RL_PATTERNS = ("ratelimit", "rate limit", "429", "403", "202", "too many", "blocked", "captcha")` (defensive string match since `ddgs.exceptions` types vary by version). Empty-but-no-exception is **not** a block (it's legitimate `no_data`).
- Curve: `delay = min(BASE * 2**(consecutive_blocks-1), CAP) ± jitter`. Defaults `BASE=15s`, `CAP=900s` (preserves today's worst-case ceiling so we never regress to longer outages), jitter = `random.uniform(0, 0.3*delay)`. A success resets `consecutive_blocks=0` and clears `blocked_until`.
- Interaction with fallback: when a host is in backoff, `_run` **skips that host and proceeds to the next attempt/engine** (it does not hard-fail the whole search like the current domain breaker does). So a backed-off `ddgs` immediately routes to keyed engines if configured; a backed-off keyed engine is skipped within `search_fallback`. This makes backoff *additive* to the existing chain rather than a new dead-end.

**3. Wiring in `_ResilientDDGS._run` (`proxy_client.py:144-174`)**
- On entry: compute cache key; return cached on hit (`text` method only — see scope).
- Per connection attempt: before calling `ddgs`, check `backoff.blocked("ddgs")`; if blocked, skip straight to keyed fallback. On exception, classify and `backoff.record("ddgs", exc)`; on non-empty success, `backoff.record_success("ddgs")`.
- Inside `_keyed_fallback`/`search_engines`: same backoff guard per engine (small change in `search_engines.search_fallback`, lines 206-221, to skip backed-off engines).
- On final result (cache miss path): store in cache (short TTL if empty).

### File-by-file changes
- **NEW `apps/api/services/leadgen/search_cache.py`** — `get(key)`, `put(key, results, empty=False)`, LRU+TTL `OrderedDict`, lock, env-config; plus per-host backoff: `blocked(host)`, `record(host, exc)`, `record_success(host)`. Pure, importable, mockable.
- **EDIT `apps/api/services/leadgen/proxy_client.py`** — in `_run` (144-174): cache lookup/store + per-attempt backoff guard/record for the `"ddgs"` host. Gate the whole feature behind `SEARCH_CACHE_ENABLED`/`SEARCH_BACKOFF_ENABLED` (default on; set off = byte-for-byte current behavior). Only `text`/`news` go through `_run`; `__getattr__` passthrough (203-209) unchanged.
- **EDIT `apps/api/services/leadgen/search_engines.py`** — `search_fallback` (206-221): skip engines whose host is in backoff; record block/success per engine. No signature change.
- **EDIT `apps/api/services/leadgen/config.py`** — document new env vars beside the existing search-engine block (114-123).
- **NEW tests** `apps/api/services/leadgen/tests/test_search_cache.py` (or repo's test dir).

### Data / persistence + tenancy
- **In-process only. No Postgres, no schema, no migration.** Short-TTL dedup is a hot-path concern; a DB round-trip would cost more than the DDG call it saves. PgLeadStore/workspace tenancy untouched.
- Multi-worker: each worker process keeps its own cache (acceptable; TTL is short and the win is intra-run dedup which is per-process anyway).
- Tenancy: cache is intentionally cross-workspace. Justification: keys are derived solely from public `site:`/web queries and `max_results`; no workspace data, no PII, no auth token enters the key or value. Results are public SERP rows. This matches existing global caches (`proxy_client._proxy_cache` line 27, `_warned_missing` in http.py:42).

### Backward-compat / non-destabilization
- Single chokepoint already abstracts every call site; behavior change is localized.
- **Scoring/sourcing untouched**: cache returns the *exact same list shape* (`[{"title","href","body"}]`) the pipeline/scoring already consume. A cache hit is indistinguishable from a fresh call. `scoring.py`, `dedup.py`, `lead_validator.py` see no difference.
- The 900s domain breaker in `rate_limiter.py`/`http.py` is **left intact** — it governs scraping, a different concern. We do not touch it (avoids destabilizing escalation logic). The spec *augments the search layer*, which had no breaker, rather than ripping out the scraping one.
- Worst-case backoff cap = 900s = today's ceiling, so no regression to longer blackouts; typical case is far shorter and self-clearing.
- Feature flags default-on but a single env flip restores identical legacy behavior.

### Feature-flag / rollout
- `SEARCH_CACHE_ENABLED` (default `true`), `SEARCH_BACKOFF_ENABLED` (default `true`), `SEARCH_CACHE_TTL=600`, `SEARCH_CACHE_EMPTY_TTL=60`, `SEARCH_CACHE_MAX_ENTRIES=2000`, `SEARCH_BACKOFF_BASE=15`, `SEARCH_BACKOFF_CAP=900`.
- Rollout: ship flags-off-capable; enable cache first (pure win, low risk), then backoff. Watch `no_data` rate and total DDG call count in a sourcing run.

### Test plan (`PYTHONPATH=. uv run --group dev python -m pytest`)
1. Cache hit: two identical `text()` calls → underlying `ddgs`/`search_fallback` invoked once (monkeypatch + call counter).
2. Cache key isolation: differing `max_results` or `query` → distinct entries, no cross-serve.
3. TTL expiry: non-empty entry served within TTL, re-fetched after; empty entry uses the shorter empty-TTL.
4. LRU bound: insert > MAX_ENTRIES → size capped, oldest evicted.
5. Backoff trip: simulate ratelimit exception → host `blocked()` true, delay follows exponential curve within cap.
6. Backoff routing: backed-off `"ddgs"` host → `_run` skips to keyed `search_fallback` (assert keyed adapter called) instead of returning [].
7. Success reset: success after blocks clears `consecutive_blocks`/`blocked_until`.
8. Empty != block: empty result with no exception does NOT trip backoff.
9. Flags off: `SEARCH_CACHE_ENABLED=false` + `SEARCH_BACKOFF_ENABLED=false` → behavior identical to pre-change (call counts unchanged).
10. Shape invariant: cached value equals fresh value field-for-field.
11. Thread-safety smoke: concurrent `to_thread` calls don't corrupt the LRU.

### Acceptance criteria
1. All DDG/keyless `text` searches via `get_ddgs()` consult the cache; identical (query, max_results, backends) within TTL hits zero network.
2. Cache is bounded by entry count and TTL; memory provably capped (test 4) at < ~10 MB default.
3. Empty results use a shorter TTL than non-empty; dead `site:` sources aren't re-hammered every row but recover within minutes.
4. Rate-limit/block signals (not empty results) trigger per-host exponential backoff with jitter, capped at ≤ 900s.
5. A backed-off host is skipped and the search falls through to the next connection/keyed engine — never a hard dead-end.
6. Cache value shape is byte-identical to a live call; scoring/dedup/validation unaffected.
7. `rate_limiter.py` 900s domain breaker and `StealthClient` escalation are untouched.
8. With both flags off, behavior is identical to current `main` (verified by call-count test).
9. No new DB objects, migrations, or workspace-keyed state; tenancy model unchanged.
10. Full leadgen test suite passes.

### Out of scope
- Refactoring/removing the `rate_limiter.py` domain circuit breaker or `proxy_pool` cooldowns.
- The SearXNG `enrichment/web_search.py` path (separate entry point; cache module is reusable there later).
- Persistent/cross-process (Redis/PG) search cache.
- Fixing the individual zero-result `site:` sources from `docs/data-source-test-report.md` (separate item; this only avoids re-hammering them).
- `news()`/images/videos caching beyond what `_run` already routes (text is primary; news can opt in trivially but isn't required).
