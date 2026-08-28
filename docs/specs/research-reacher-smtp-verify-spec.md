<!-- Auto-generated research-bet spec (2026-06-26). Effort: medium. -->

## Spec: Self-hosted Reacher unlimited SMTP verification

### Goal
Add a self-hosted **Reacher** (`reacherhq/check-if-email-exists`) service as the strongest tier of Yupcha's email-verification cascade, giving **unlimited, $0-per-email** deliverability checks (safe/risky/invalid/unknown + catch-all + disposable + role) that a metered SaaS (Clay) structurally can't match. Reacher augments — does not replace — the existing port-25 SMTP probe and HTTP-vendor verifiers, slotting in as the front of the existing cascade, with a per-email result cache, graceful fallback when Reacher is down, and per-workspace/global config. **Default OFF.**

### Grounded current state (file:line)
- **Cascade contract** — `apps/api/services/leadgen/enrichment/email_verify_cascade.py`. `Verifier` interface (`is_available()` + `async verify(email) -> VerifyResult`) at L58-65; 4-status contract `valid|invalid|catch_all|unknown` at L28-33; `_status_from_vendor` mapper L105-114; **registry `_VERIFIERS = [SmtpVerifier()]` at L118**; `register_verifier(v, front=)` at L121-125; `available_verifiers()` L128-136; `verify_email(email, verifiers=None)` cascade driver L139-158 ("first definitive wins; unknown/error cascades").
- **Key finding:** `register_verifier` is **never called anywhere** in `apps/` (grep confirms only the def). The module's docstring/comments (L101-114) describe HTTP-vendor fallbacks "for cloud envs where port 25 is blocked," but **no HTTP verifier is actually wired** — today the cascade is SMTP-only. Reacher is the first real second tier.
- **SMTP tier** — `SmtpVerifier` L70-98 wraps `email_verify.probe_domain`; availability gated by `_SMTP_ENABLED` (env `LEADGEN_SMTP_VERIFY`, `email_verify.py:40`). Catch-all → `CATCH_ALL`, RCPT accept → `VALID`, reject → `INVALID`, soft/unreachable → `UNKNOWN`.
- **Deliverability tagger** — `apps/api/services/leadgen/enrichment/email_deliverability.py`: `verify_deliverability(email)` L53-101 (no workspace param) calls `cascade.verify_email` L79, maps to `verified|risky|unknown|""(drop)` on `Lead.email_confidence`; `tag_email_confidence(lead)` L104-123; batch L126-143. Role mailbox demotes VALID→risky (L97); catch_all→risky (L93); invalid/disposable→drop (L67-76, L90-91).
- **Pipeline callers (no workspace_id threaded today):**
  - `job_runner.py` L708-789: a `smtp_verify` stage (`MailScoutVerifyProvider`, L715-716) then a `deliverability` stage calling `tag_email_confidence` (L763-781), budget `DELIVERABILITY_BUDGET` default 20 (L766).
  - `workbook/enrichment.py` L452-460: per-cell auto-verify via `cascade.verify_email(str(result_value))` (L455-456), writes `cell_metadata.verify = {status, confidence, source}`.
  - `apps/mcp/server.py` L80-87, L190-206: `verify_email` MCP tool calls `MailScoutVerifyProvider` directly (bypasses the cascade entirely).
- **Existing cache** — `apps/api/services/leadgen/enrichment/cache.py`: SQLite (`DATA_DIR/enrichment_cache.db`), `canonical_key` is **identity-keyed (name+domain / domain)** for field *values* (L67-92), TTL table L27-34, `get_cache()` singleton L171-177. It is **not** suitable as-is for verification (we need to cache by the literal email + verifier, not by lead identity).
- **Secrets/config** — `apps/api/services/workspace/secrets.py` `get_secret(workspace_id, key, default)` L210-215: workspace-encrypted → global (`routers/settings.py:_db_get` L49) → default. Existing flags are plain `os.getenv` (`email_verify.py:38-41`, `job_runner.py:766`).
- **Infra** — `docker-compose.yml`: services on `lead_net` bridge (L160-162), profiles not yet used; no verification service. `RUN_INLINE_WORKER=0`, standalone `worker` runs the pipeline.
- **License** — repo is **AGPL-3.0** (`LICENSE` header). Reacher is dual-licensed **AGPL-3.0 OR commercial**; the AGPL path is fully compatible. (`docs/research/improvement-roadmap-2026-06-25.md:57,121` already flags this.)

### Design
Reacher's self-hosted backend is a standalone HTTP service (`POST /v0/check_email`, `{"to_email": "..."}` → `{is_reachable: safe|risky|invalid|unknown, smtp:{is_catch_all,is_deliverable,...}, misc:{is_disposable,is_role_account}, mx, syntax}`; newer builds gate on a `Bearer` header secret). We treat it as one more `Verifier` registered at the **front** of the existing cascade.

1. **`ReacherClient`** — thin async `httpx` wrapper. Resolves `REACHER_URL` + `REACHER_API_KEY` via `get_secret(workspace_id, ...)`. Bounded timeout (default 12s; Reacher can stall on greylisting). Maps `is_reachable` → 4-status: `safe→VALID`, `invalid→INVALID`, `risky→CATCH_ALL if smtp.is_catch_all else (RISKY-as-CATCH_ALL/UNKNOWN)`, `unknown→UNKNOWN`. Disposable→INVALID, role flag surfaced in detail. A tiny **circuit breaker** (process-global): after N consecutive failures, report unavailable for a cooldown so a dead Reacher doesn't add latency to every check.
2. **`ReacherVerifier(Verifier)`** — `is_available()` returns True only when enabled-flag set AND config resolves AND breaker closed. `verify()` calls the client, returns a `VerifyResult` (Gmail/Microsoft etc. → `unknown`, so the cascade falls through to SMTP/corroboration — matches Reacher's real-world behavior).
3. **Per-email verification cache** — new `email_verify_cache.py` (SQLite table in the existing `enrichment_cache.db`, separate table `email_verify_cache`, keyed by `sha256(lower(email))`+verifier-name, columns status/source/detail/expires_at). Cascade checks cache before calling Reacher; writes definitive results with TTL (default 14 days — deliverability decays slower than discovery but catch-all/greylist states should re-check). UNKNOWN results cached only briefly (e.g. 1 day) so transient failures self-heal.
4. **Cascade wiring** — lazy `_bootstrap_verifiers(workspace_id)` (idempotent) registers `ReacherVerifier` at front when configured; `verify_email` gains an optional `workspace_id=None` kwarg (backward-compatible) and consults the cache. Order: cache → Reacher → SMTP probe → (future HTTP vendors).
5. **Graceful fallback** — every Reacher error/timeout is swallowed (cascade already does this, L150-154) and yields a non-definitive `UNKNOWN`, so the cascade continues to the SMTP tier; if everything is unknown, callers tag `unknown` (never a false `verified`). This preserves the existing "degrade safely" guarantee in `email_deliverability.py:18-20`.
6. **Config** — workspace-overridable enable flag + cloud API key via `get_secret`; the **URL is global/env-only** (SSRF mitigation, see security).

### File-by-file changes
- **NEW `apps/api/services/leadgen/enrichment/providers/reacher_verify.py`** — `ReacherClient` + `ReacherVerifier(Verifier)` + status mapping + circuit breaker. ~150 LOC.
- **NEW `apps/api/services/leadgen/enrichment/email_verify_cache.py`** — `EmailVerifyCache` (SQLite, by email-hash+verifier, TTL) + `get_verify_cache()` singleton, mirroring `cache.py`'s pattern.
- **EDIT `email_verify_cascade.py`** — add `_bootstrap_verifiers(workspace_id)`; add `workspace_id: Optional[str]=None` to `verify_email`; integrate cache get/set; register Reacher at front when `is_available`. Keep `register_verifier`/`_VERIFIERS` and the existing single-arg call sites working.
- **EDIT `email_deliverability.py`** — thread `workspace_id` through `verify_deliverability` / `tag_email_confidence` / `tag_email_confidence_batch` (default None → global config), pass to `cascade.verify_email`. Surface `is_role`/catch-all unchanged.
- **EDIT `job_runner.py`** (L763-781) — pass the run's `workspace_id` into `tag_email_confidence`. (Job context already has the workspace.)
- **EDIT `workbook/enrichment.py`** (L455-456) — pass `workspace_id` into `verify_email`.
- **EDIT `apps/mcp/server.py`** (L190-206) — route `verify_email` through `email_verify_cascade.verify_email(email, workspace_id=...)` so MCP benefits from Reacher + cache (and resolves the workspace from the MCP auth context, not arbitrary input — see security).
- **EDIT `docker-compose.yml`** — add optional `reacher` service under a `profiles: [reacher]` (NOT started by default), `image: reacherhq/backend:<pinned-tag>`, on `lead_net`, env `RCH__BACKEND_NAME`, `RCH__SMTP__FROM_EMAIL`, header secret, optional `RCH__PROXY__*` SOCKS5; `api`/`worker` get `REACHER_URL=http://reacher:8080` only when the profile is active.
- **EDIT `.env.example`** — document `REACHER_ENABLED`, `REACHER_URL`, `REACHER_API_KEY`, `REACHER_TIMEOUT`, `REACHER_VERIFY_TTL_DAYS`, proxy vars.
- **EDIT `apps/api/routers/settings.py`** (+ secrets allowlist) — register the new keys so they resolve via `_db_get`/`get_secret` and can be set per-workspace (key, not URL). Settings-page UI toggle is **out of scope** (backend-config-driven for v1).

### Data / persistence + tenancy / security
- **New persistence:** `email_verify_cache` table in the existing `enrichment_cache.db` (SQLite, not Postgres/RLS). It stores only `email-hash → status` — a property of the *address*, not tenant data — identical trust model to the existing global `enrichment_cache` (`cache.py`). No lead/workbook rows or PII beyond the hashed email; cross-workspace sharing is a deliberate cost win, not a leak of tenant content. **Document explicitly.**
- **Tenancy/RLS adversarial pass:** the *config resolution* is the tenant-sensitive part. Threading `workspace_id` into the cascade is what makes per-workspace Reacher selection possible; the resolved value comes only from `get_secret(workspace_id, ...)` (already RLS/encryption-aware), never from caller-supplied URLs. Leads/workbooks stay RLS-scoped; we touch only `Lead.email_confidence` in-memory before the existing persist path. No new RLS surface in Postgres.
- **Security / confused-deputy / SSRF:** a self-hosted verifier makes **outbound requests to a URL we control** — if a workspace could set `REACHER_URL` to an arbitrary internal address (e.g. `http://169.254.169.254/...` or another tenant's service) the API becomes a confused deputy. **Mitigation: `REACHER_URL` is global/env-only; per-workspace config is limited to the boolean enable flag + an optional *cloud* API key (used only against the fixed `api.reacher.email` host).** Validate the global URL against a scheme/host allowlist at load. The MCP path must resolve `workspace_id` from authenticated session context, never from tool arguments.
- **Header secret:** Reacher's `RCH__HEADER_SECRET` stored via `get_secret`, sent as `Bearer`; never logged. Reacher itself must not be exposed on a host port in prod (internal `lead_net` only).

### Infra / deploy
- Reacher ships as an **optional** compose service behind `profiles: [reacher]`: `docker compose --profile reacher up` for operators who want it; default `up` is unchanged. Pin the image tag (don't track `latest`).
- **Availability reality:** Reacher does real port-25 SMTP RCPT probes. On most clouds outbound :25 is blocked, so a bundled Reacher is only useful with **SOCKS5 proxies** (the #1 reason naive SMTP fails — roadmap L57). Wire `RCH__PROXY__*` envs and document that without port-25 egress or proxies, Reacher returns mostly `unknown` and the cascade correctly falls back. For Yupcha-hosted SaaS, the realistic path is the **commercial Reacher cloud API** (BYO key) rather than the bundled container.
- Resource note: Reacher is a Rust service, low idle footprint; per-check latency dominated by remote SMTP (greylisting can take seconds) — hence the bounded timeout + circuit breaker.

### License / compliance
- Yupcha is **AGPL-3.0**; Reacher is **AGPL-3.0 OR commercial**. Running the AGPL build as a **separate networked process** (not linked) is unambiguously compliant — even more so than linking. Bundle it as a separate optional container, keep its source attribution, and in docs describe it as **"dual-licensed AGPL/commercial,"** not "free" (per roadmap skeptic note L57). The optional cloud API path falls under Reacher's commercial terms — the user supplies their own key/account. No GPL contamination of Yupcha's own code.

### Feature flag / rollout (default OFF)
- `REACHER_ENABLED` (env or `get_setting`/per-workspace) defaults **off**. With it off, `ReacherVerifier.is_available()` returns False, the cascade is byte-for-byte today's behavior, and the compose `reacher` profile isn't started. Rollout: ship dark → enable on one internal workspace → measure verified/risky/unknown deltas vs. SMTP-only → broaden.

### Test plan
- **Unit (no network):** `ReacherClient` status mapping for every `is_reachable` × catch_all/disposable/role combination → 4-status contract; timeout/HTTP-error → UNKNOWN; circuit breaker opens after N failures and reports unavailable. Mock `httpx` with `respx`/monkeypatch.
- **Cascade:** with a stub `ReacherVerifier` returning VALID, `verify_email` returns it without invoking SMTP (definitive-first); with Reacher UNKNOWN, falls through to a stub SMTP verifier; with Reacher raising, swallowed and cascades. Confirm `verify_email("a@b.com")` (no kwarg) still works (backward-compat).
- **Cache:** definitive result cached and short-circuits a second call; UNKNOWN has short TTL; lazy expiry returns None after TTL.
- **Deliverability mapping:** Reacher catch_all → `risky`; role+valid → `risky`; invalid → drop (`keep==False`); all-unknown → `unknown` (never false `verified`).
- **Config/security:** per-workspace enable flag honored via `get_secret`; workspace-supplied URL is ignored (global-only); allowlist rejects a bad URL.
- **PG-gated integration** (live throwaway PG, never the `yupcha` db): run `job_runner` deliverability stage end-to-end with Reacher stubbed, assert `Lead.email_confidence` written and RLS scoping intact. Gate behind the existing PG fixture; `PYTHONPATH=. uv run --group dev python -m pytest`.
- **MCP:** `verify_email` tool returns cascade result and resolves workspace from session, not args.

### Acceptance criteria
1. With `REACHER_ENABLED` unset, behavior, output tags, and `docker compose up` are identical to today (no Reacher container, SMTP-only cascade).
2. With Reacher enabled+reachable, an address Reacher marks `safe` is tagged `verified` (non-role) end-to-end through `tag_email_confidence`.
3. A catch-all domain → `risky`; a Reacher `invalid`/disposable → email dropped (`keep==False`); Gmail/Microsoft → `unknown` and cascade falls through.
4. Reacher down/timeout never raises to the caller and never produces a false `verified`; cascade falls back to the SMTP tier; circuit breaker stops hammering a dead endpoint.
5. Identical email verified twice hits the cache the second time (no second Reacher call within TTL).
6. `REACHER_URL` is resolvable only from global env/settings; a per-workspace attempt to override the URL is ignored; an off-allowlist URL is rejected at load.
7. `verify_email(email)` (legacy single-arg) and the workbook per-cell verify still work unchanged.
8. Reacher compose service starts only under `--profile reacher`, is not published on a host port, and authenticates with the header secret.
9. Docs state Reacher is dual-licensed AGPL/commercial and explain the port-25/proxy caveat.
10. Full suite green incl. PG-gated tests via the documented command.

### Out of scope
- Settings-page UI for Reacher config (backend/env-driven in v1).
- Bulk async Reacher batch endpoint / its own job-queue stage (reuses the existing deliverability budget loop).
- Migrating the SQLite verification cache to Postgres.
- Provisioning/rotating SOCKS5 proxies (documented as operator responsibility).
- Wiring the still-unwired HTTP-vendor verifiers (Debounce etc.) into the cascade — separate bet, though this work makes it a one-liner via `register_verifier`.
