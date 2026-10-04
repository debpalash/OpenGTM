# RFC: Go backend with Python specialist workers and optional Rust

Updated: 2026-10-03
Status: proposal; no backend migration has shipped.
Tracking issue: [#33](https://github.com/debpalash/OpenGTM/issues/33).

## Decision

Make **Go the primary backend language**, keep **Python for AI, browser
automation, and specialized integrations**, and introduce **Rust only when
profiling demonstrates a computational bottleneck and a worthwhile improvement**.
Go is the starting point for the throughput goal; a three-language backend is
not a prerequisite.

Deliver this through incremental replacement of job types and API routes. The
first implementation milestone is one production-compatible Go enrichment
worker, with parity tests, reproducible benchmarks, and a working rollback.

## Goals and scope

- Sustain hundreds of completed enrichments per second on a documented workload,
  subject to provider quotas and latency.
- Reduce overhead per enrichment through connection reuse, bounded concurrency,
  batched persistence, and query improvements.
- Preserve tenant isolation, cancellation, retries, provider evidence, billing,
  and existing REST/MCP behavior.
- Make AI-assisted development fast through small packages, generated contracts,
  fixtures, and reproducible checks.
- Keep self-hosting practical with Docker Compose and independently scalable
  workers.

The React frontend stays in TypeScript. New brokers, workflow platforms, vector
databases, and Kubernetes require separate evidence and decisions. Replacing
working product behavior or all Python integrations is outside the initial scope.

## Current architecture and opportunities

The [deployed architecture](../architecture.md) uses Python/FastAPI, PostgreSQL
with forced workspace RLS, a PostgreSQL durable queue, Redis progress, Python
workers, and a scheduler. Workspace metadata, secrets, and collection ledgers
still have local SQLite dependencies that constrain multi-host operation.

Concrete candidates for measurement:

| Area | Current code | Migration opportunity |
| --- | --- | --- |
| Queue | [`queue_service.py`](../../apps/api/services/queue_service.py) | Preserve claims, tenant fairness, retries, and reconciliation while measuring worker overhead |
| Job isolation | [`job_process_runner.py`](../../apps/api/services/job_process_runner.py) | Avoid a fresh Python interpreter for native Go work; retain killable isolation for specialist tasks |
| Provider isolation | [`provider_runner.py`](../../apps/api/services/workbook/provider_runner.py) | Preserve the existing warm, killable process pool behavior where needed |
| HTTP fetching | [`leadgen/http.py`](../../apps/api/services/leadgen/http.py) | `_fetch_plain` constructs a client per fetch; evaluate reusable transports while preserving proxy and redirect protections |
| Workbook refresh | [`workbook/refresh.py`](../../apps/api/services/workbook/refresh.py) | `_stale_row_ids` loads all workbook rows and overlays; move candidate selection into bounded SQL queries |
| Worker capacity | [`config.py`](../../apps/api/core/config.py) | Current defaults are one worker slot and two active jobs per workspace; record actual configuration before comparing languages |

These are hypotheses, not measured bottlenecks. Establish a baseline before
attributing throughput limitations to Python or predicting a speedup.

## Language ownership

| Language | Responsibility |
| --- | --- |
| Go | APIs, workspace authorization, enrichment orchestration, ordinary HTTP providers, job execution, scheduling, result persistence, and billing coordination |
| Python | AI research and model SDKs, browser automation, specialized extractors, and integrations retained during migration |
| Rust, optional | Proven CPU/memory hotspots such as parsing, normalization, or bulk matching; introduced behind a bounded batch interface |
| TypeScript | Existing React UI and generated public API clients |

The Go coordinator owns each migrated workflow and its final state. Specialist
tasks return typed results and evidence. Legacy Python job handlers keep their
existing persistence behavior until their job type is explicitly migrated.

## Proposed stack

Use supported stable releases, pin exact versions and image digests at
implementation time, and upgrade through compatibility checks.

| Area | Choice and purpose |
| --- | --- |
| Go | Go 1.27 release line with a pinned patch; `net/http`, `context`, pooled transports, and `pprof` |
| Database | PostgreSQL 18, current supported patch; preserve forced RLS and existing domain constraints |
| Go database access | pgx v5 + sqlc for pooled connections and generated types from explicit SQL |
| Durable execution | Existing PostgreSQL `jobs` queue, extended only with reviewed compatibility migrations |
| Progress and limits | Redis for live progress and, where needed, atomic shared provider/credential rate limits; durable state stays in PostgreSQL |
| Public API | Existing REST + SSE + MCP semantics; OpenAPI contracts and generated Go/TypeScript clients |
| Specialist boundary | Protobuf + gRPC with Buf generation/compatibility checks; bounded request sizes and authenticated internal callers |
| Python | Existing supported Python baseline, uv lockfile, Pydantic validation, pytest, and Ruff; evaluate runtime upgrades against SDK compatibility |
| Rust, if justified | Stable Rust, Cargo, Clippy, rustfmt, and Tokio where asynchronous service I/O is needed |
| Bulk transformation | Evaluate Polars before writing a custom Rust engine |
| Frontend | Existing React, TypeScript, Vite, TanStack, and Bun tooling |
| Observability | OpenTelemetry Collector, structured logs, metrics, distributed trace propagation, and Go profiling |
| Benchmarks | k6 for API load plus an enrichment completion harness and deterministic provider simulator |
| Deployment | Versioned Compose release with compatible Go/Python services, one migration owner, and a release manifest recording image digests |

The current single-application-image release rule must be revised and tested
before deploying multiple service images. Every service must declare supported
schema and contract versions; an incompatible combination must fail startup.

## Target execution model

```text
React / REST / MCP
        |
        v
Go API + legacy FastAPI during migration
        |
        v
PostgreSQL: domain data + durable jobs + billing ledger
        |
        v
Go enrichment worker
   |             |                  |
HTTP providers   Python specialists  optional Rust batch tasks
   |             |                  |
   +-------------+------------------+
                 |
       guarded result transaction
                 |
          Redis progress -> SSE
```

Proposed specialist contracts include task/contract version, job ID, workspace
ID, attempt/lease token, deadline, trace context, bounded inputs, and a result
containing normalized fields, evidence, usage, and typed errors. Authenticate
the caller and validate workspace authorization; a workspace ID in a payload
alone is not sufficient.

Credentials resolve through an authenticated workspace-scoped mechanism. Keep
secrets out of durable job payloads, logs, traces, and reusable fixtures. Provider
clients may reuse transports, but must not reuse tenant-specific credentials or
cookies across workspaces. Validate destinations and redirects, and preserve
DNS-pinning protections where required.

Keep bounded pools of specialist processes. Cancellation propagates through Go
contexts and RPC deadlines; browser tasks and uncooperative libraries also need
process termination and parent-side failure reconciliation.

## Correctness requirements

1. **Tenant boundary:** bind verified workspace identity inside each database
   transaction using transaction-local settings. Runtime roles remain
   non-superuser and `NOBYPASSRLS`; unset workspace context fails closed. Test
   pooled connections alternating tenants and all retry/worker paths.
2. **Queue ownership:** route each job type/payload version to exactly one
   executor implementation. Both claim queries enforce routing, and queued jobs
   remain compatible with rolling upgrades. Retain `SKIP LOCKED`, heartbeats,
   retry budgets, tenant fairness, and domain failure reconciliation.
3. **Lease fencing:** increment an attempt/lease token when claiming work.
   Heartbeats, domain commits, and completion validate the current token and
   cancellation state transactionally. Additive schema changes must teach both
   executors the protocol before enabling Go claims. Old workers cannot reclaim
   ownership or commit after a newer attempt takes over.
4. **Side effects:** use provider idempotency keys where supported, durable
   outcome records, and unique internal billing entries. External calls can
   repeat after an ambiguous timeout; reconcile uncertain outcomes instead of
   claiming exactly-once external execution.
5. **Cancellation:** a committed cancellation cannot be overwritten by success
   or failure. Stop admission of new provider calls and reconcile in-flight work.
6. **Isolation and limits:** bound queues, goroutines, process pools, database
   connections, response sizes, and batch sizes. Coordinate provider limits
   across replicas and credentials; tenant fairness must survive worker scaling.
7. **Compatibility:** preserve evidence provenance, connector cursors, counts,
   merge/undo semantics, API errors, and agent scopes. Keep one schema migration
   pipeline, initially the existing Alembic owner process.

## Migration milestones and exit gates

| Milestone | Deliverable | Exit gate |
| --- | --- | --- |
| M0: baseline | Fixed fixtures, provider simulator, representative workloads, resource profiles, and current queue/API measurements | Reproducible baseline with hardware, commit, configuration, and workload identity |
| M1: foundations | Go module, sqlc queries, versioned contracts, telemetry, additive executor routing/fencing, and Compose development setup | Python-only operation still passes; mixed-version claims, RLS, and lease recovery tests pass |
| M2: one worker | One declarative HTTP enrichment job type in Go, including result/evidence persistence, billing coordination, and progress | Behavioral parity, crash/cancellation tests, benchmark comparison, feature flag, and exercised rollback |
| M3: execution improvements | Bounded concurrency, transport pooling, shared limits, batching, and bounded workbook refresh queries | Measured improvement without degraded fairness, correctness, or resource bounds |
| M4: specialists | Python AI/browser task interface with process isolation and bounded capacity | Contract, timeout, secret-isolation, retry, and reconciliation tests pass |
| M5: API migration | Go routes introduced by domain, with generated clients and existing FastAPI fallback | Auth, REST/SSE/MCP contract parity and supported deployment smoke checks pass |
| M6: multi-host readiness | Workspace metadata, secrets, and collection ledgers moved to PostgreSQL; scheduler leadership defined | Backfill/restore proof, reconciliation, and replicas on independent hosts pass |
| Optional Rust | One profiled computational kernel or batch service | Documented end-to-end improvement including serialization/RPC, build cost, correctness, and operational overhead |

Choose the precise first job type during M0 based on independent scope and
available parity fixtures. M6 is mandatory before any multi-host deployment;
single-host migration does not depend on it. Run one scheduler until leadership
and failover are implemented.

Roll out per job type or route to an explicit workspace cohort. Stop new claims
before changing executor ownership, drain or recover in-flight attempts, and
keep old-compatible payloads and schema. Shadow comparisons use recorded or
simulated responses so they do not duplicate paid calls. Re-enable the legacy
executor through the same routing protocol to roll back; do not drop rollback
support until the migrated capability passes its release gates.

## Performance evidence

Define one completion as one input record finishing its specified enrichment
waterfall with the result, evidence, and internal accounting committed. Record
terminal no-match outcomes separately from successful enrichment. Jobs, API
requests, and individual provider calls are distinct counters.

Use 100, 300, and 1,000 completed enrichments per second as load points, not
promised capacity. Compare Python and Go on identical hardware, data, limits,
provider behavior, and durability settings. Run at least a 30-minute steady
load after warm-up, plus overload and recovery phases; publish the actual
achieved rate if a target is unsustainable.

Include single-provider, multi-step waterfall, slow-provider, rate-limit,
timeout, malformed-response, worker-crash, cancellation, and multi-tenant
workloads. Label simulated-provider capacity separately from quota-permitted
controlled-live throughput. AI research and browser-heavy work get separate
capacity profiles.

Report committed completions/sec; queue delay; end-to-end p50/p95/p99; API
latency; CPU/RSS; database connections, writes and lock waits; provider attempts
and 429s; timeouts/errors; tenant fairness; outstanding backlog; and cost per
completed record. Show that steady-state backlog and memory remain bounded.

Release requires parity tests, no duplicate internal accounting in replay
fixtures, no tenant leaks, exercised failure recovery and rollback, and
published evidence of improvement. Set concrete latency/resource thresholds
from M0 rather than inventing them before measuring.

## AI-assisted development and contributor workflow

Keep Go packages small and organized around domains: queue, tenancy, provider
execution, persistence, progress, and billing. Proposed additions are
`apps/backend-go`, `apps/python-workers`, and `packages/contracts`; add
`crates/` only with an accepted Rust benchmark case. Preserve existing Python
entry points until their replacement is released.

Maintain scoped contributor instructions with package ownership, commands,
invariants, and examples. Check generated contracts and SQL code into the repo
where appropriate, and have CI detect regeneration drift. Each migration PR
includes a before/after behavior fixture, acceptance checks, rollout switch,
and rollback instructions.

Extend existing CI with Go tests and race checks, contract compatibility, real
PostgreSQL RLS/queue tests, and Python specialist tests. Add Rust checks only
when Rust code exists. Keep frontend build and existing backend tests as gates
during coexistence. Run performance comparisons in a controlled environment;
shared CI timing alone is insufficient evidence of throughput.

## Co-maintainer work and first implementation checklist

We welcome a co-maintainer for Go execution, Python AI/browser integration,
PostgreSQL correctness, or reproducible performance testing. Follow
[CONTRIBUTING.md](../../CONTRIBUTING.md), including DCO sign-off.

- [ ] Record M0 baseline and select the first enrichment job type.
- [ ] Define domain ownership, task contracts, executor routing, and lease fencing.
- [ ] Add the Go development skeleton, pgx/sqlc access, and tenant transaction helper.
- [ ] Preserve Python-only operation and exercise mixed-executor claims.
- [ ] Implement one HTTP provider path with bounded concurrency and safe egress.
- [ ] Commit results, evidence, and internal billing under the active lease.
- [ ] Exercise retry, crash, cancellation, tenant isolation, and rollback fixtures.
- [ ] Publish comparable throughput/resource reports and controlled-live results.
- [ ] Release the first Go worker behind explicit routing and a feature flag.

Open decisions: first job type, numerical SLOs after baseline, exact dependency
pins, credential resolution across services, and the measured case for Rust.

## Technology references

- [Go 1.27 release notes](https://go.dev/doc/go1.27)
- [PostgreSQL version policy](https://www.postgresql.org/support/versioning/)
- [pgx v5](https://pkg.go.dev/github.com/jackc/pgx/v5)
- [sqlc](https://github.com/sqlc-dev/sqlc)
- [Buf contract tooling](https://buf.build/docs/)
- [uv](https://docs.astral.sh/uv/)
- [Pydantic](https://docs.pydantic.dev/latest/)
- [Polars](https://docs.pola.rs/)
- [OpenTelemetry Collector](https://opentelemetry.io/docs/collector/)
- [k6](https://grafana.com/docs/k6/latest/)
