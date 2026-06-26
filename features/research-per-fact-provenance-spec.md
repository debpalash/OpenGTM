<!-- Auto-generated research-bet spec (2026-06-26). Effort: large. -->

## Per-fact license/freshness provenance surface — BUILD-READY spec

### Goal
For every enriched FACT (workbook cell + written-back lead field) record **source, license, fetched-at/freshness, confidence**, persist it next to the value, expose it via API, and surface it in the UI (cell tooltip "provenance card"). Today we already store *part* of this (`provider`, sometimes `verify_status`) but inconsistently, never persist the numeric `confidence` we compute, and have **no license or fetched-at** anywhere except an ad-hoc GLEIF special case. Ship it default-OFF behind a flag.

---

### Grounded current state (file:line)

**Where a value + its source are both known (the write path):**
- Workbook waterfall/enrichment runs in `enrich_cell`, `apps/api/services/workbook/enrichment.py`. The provider result (`EnrichmentResult{provider, success, fields, confidence, error, duration_ms}`, defined `apps/api/services/leadgen/enrichment/provider.py:26-34`) is unpacked at `enrichment.py:361-362`. The winning provider, value, and **a numeric `result_confidence`** are captured at `enrichment.py:408-410` — but `result_confidence` is then **never persisted** (it's a local var, dropped at function end).
- Cells are written by `_set_enrichment` (`enrichment.py:495-582`). It builds the inline cell dict at `enrichment.py:571-575`: `{"value", "status", "provider", "error"}` plus optional `"verify_status"`. This is mirrored into `WorkbookRow.enrichments` JSON (`enrichment.py:563-577`) and the `WorkbookEnrichment` overlay row (`enrichment.py:534-547`, `cell_metadata` carries `{verify:{status,confidence,source}}` set at `enrichment.py:457-458`).
- Write-back to the lead: `_write_back_to_lead` (`enrichment.py:632-676`) updates only the field value plus `email_provider`/`phone_provider` (`enrichment.py:645-648`). No fetched-at, license, or confidence reaches the lead.
- The leadgen-core waterfall (`WaterfallEnricher.enrich`, `provider.py:139-238`) computes per-field `WaterfallLog{winner, final_value, final_confidence, attempts}` (`provider.py:44-60`) and `apply_results` (`provider.py:240-274`) writes `<field>_provider` attrs + the full `enrichment_waterfall` JSON blob (`provider.py:269-272`). Confidence is in the log dict but not on the field.
- Email-finder waterfall (`email_waterfall.py:240-256`) records provenance onto the **existing** `Lead.email_provider` + appends an attempt log to `Lead.enrichment_waterfall`. No license/fetched-at.
- **Precedent that proves the pattern works**: `providers/gleif.py:65-66,143-144,223-224` already emits `gleif_source="gleif"` + `gleif_license="CC0-1.0"` into `fields`. This is a one-off; nothing reads it.

**Lead/workbook schema:**
- `WorkbookRow` (`apps/api/services/workbook/models.py:256-310`): `enrichments` JSON inline (`:275-278`), `data` JSON snapshot (`:271-273`), `workspace_id NOT NULL` (`:267`), `lead_id` link (`:281`). RLS-scoped.
- `WorkbookEnrichment` (`models.py:213-251`): `value/status/provider/error` columns + `cell_metadata` JSON (`:237-241`), `workspace_id NOT NULL` (`:231`), unique `(workbook_id,lead_id,column_id)` (`:224-225`).
- `LeadRow` (`apps/api/services/leadgen/orm_models.py:42-136`): existing partial provenance — `email_confidence`/`email_provider` (`:62-63`), `phone_provider` (`:65`), `company_size_basis` (`:77-78`), `enrichment_attempts`/`enrichment_waterfall` (`:100-102`), `last_enriched_at` (`:125`). All `workspace_id NOT NULL` (`:54`), RLS-scoped.

**Read path / API:**
- `GET /api/workbooks/{id}` (`apps/api/routers/workbooks.py:414-500`) reads `WorkbookRow.enrichments` JSON → `EnrichmentOverlay(**overlay)` (`:438-443`) and the v1 path maps `WorkbookEnrichment` → `EnrichmentOverlay` (`:478-487`).
- `EnrichmentOverlay` schema (`apps/api/services/workbook/schemas.py:128-135`): `value/status/provider/error/verify_status`. This is the exact object that must gain provenance fields.
- Cell-trace endpoint precedent (agent columns): `GET .../cells/{col_id}/trace` (`workbooks.py:1088-1105`) reading `CellTrace` (`trace_models.py`), which is itself a per-cell RLS table backfilled by the `e5f6a7b8c9d0` migration.

**UI:**
- `EnrichmentOverlay` TS interface `apps/web/src/lib/workbook-api.ts:14-20`.
- Cell render in `apps/web/src/pages/workbook-editor.tsx`: `CellRenderer` (`:125-210`) already shows a provider badge (`:202-204`) and a `title=` tooltip (`:182`); overlay wired at `:614-631`. This is the natural provenance-tooltip host.

**Migrations:** alembic head `a7b8c9d0e1f2` (`migrations/versions/a7b8c9d0e1f2_source_health.py`). The RLS+denormalized-tenant+backfill pattern to copy is `migrations/versions/e5f6a7b8c9d0_workbooks_rls.py`.

---

### Design

**Storage model — JSON sidecar, NOT a provenance table (see OWNER decision D1).** A single canonical per-fact provenance object:
```
Provenance = {
  "source":     str,   # provider/source name, e.g. "hunter_io", "gleif", "website"
  "license":    str,   # SPDX-ish token from the vocabulary (D3)
  "confidence": float, # 0..1 (we already compute this — enrichment.py:410)
  "fetched_at": str,   # ISO-8601 UTC at write time
}
```

Two homes, both already RLS-tenant-scoped (zero new RLS surface):
1. **Workbook cells** — extend the existing inline cell dict in `WorkbookRow.enrichments` (and the `WorkbookEnrichment.cell_metadata` mirror). No DDL: it's already a JSON column. Add a nested `"provenance"` key to the dict built at `enrichment.py:571-575`.
2. **Lead fields** — add ONE new JSON column `LeadRow.field_provenance` (`{field_name: Provenance}`). Inherits the leads-table RLS policy automatically (`c42d0273d9bd`); a new column on an existing RLS table needs no new policy.

Rationale vs a dedicated `fact_provenance` table: a table gives cross-row queryability ("list all CC-BY facts") but costs (a) a new RLS migration with FORCE policy + GRANTs, (b) **N-row write amplification on the hot per-cell enrichment path** (one INSERT per field per provider), and (c) a JOIN on every grid read (the grid already reads the row JSON in one shot — `workbooks.py:438`). For a Clay-parity tooltip the sidecar is single-read, write-cheap, and tenancy-free. We keep the door open: the `Provenance` shape is identical in both homes, so a future ETL into a table is mechanical.

**Write path (every enriched field records provenance):**
- Add a `source_license: str = "unknown"` class attr to `EnrichmentProvider` (`provider.py:91-98`), defaulted per provider; central fallback map `PROVIDER_LICENSE` for providers not annotated. Propagate `license` through `provider_runner._provider_job` result dict (`provider_runner.py:88-94`) so the subprocess returns it alongside `provider/confidence`.
- In `enrich_cell`: build a `provenance` dict from `result_provider`, the provider's `source_license`, `result_confidence` (`enrichment.py:410` — finally persisted), and `datetime.now(timezone.utc).isoformat()`. Pass it into `_set_enrichment(..., provenance=...)`; merge into the cell dict at `enrichment.py:571-575` and into `cell_metadata`.
- In `_write_back_to_lead` (`enrichment.py:632-676`): also write `field_provenance[field] = Provenance` (read-modify-write the JSON) and stamp `last_enriched_at`. Helper centralizes the `now()`.
- Leadgen-core `apply_results` (`provider.py:253-274`): for each `WaterfallLog` with a winner, write `field_provenance[field]` from `log.winner` + `log.final_confidence` + provider license + now. Keep the existing `<field>_provider` writes for back-compat.
- `email_waterfall._record_provenance` (`email_waterfall.py:240-256`): additionally set `field_provenance["email"]`.

**API (read):**
- Extend `EnrichmentOverlay` (`schemas.py:128-135`) with optional `provenance: Provenance | None`. Populate it in both read branches (`workbooks.py:441,484`). Backward-compatible (additive optional field).
- Lead read endpoints: surface `field_provenance` on the lead detail response (additive). New thin endpoint mirroring the trace one: `GET /api/workbooks/{id}/rows/{lead_id}/cells/{col_id}/provenance` is **not** needed — provenance ships inline in the grid payload; skip it to avoid an extra round-trip.

**UI (minimal surface):**
- Add `provenance` to the TS `EnrichmentOverlay` (`workbook-api.ts:14-20`).
- In `CellRenderer` (`workbook-editor.tsx:125-210`): when `overlay.provenance` exists, replace the plain `title=` (`:182`) with a small hover "provenance card" (source, license badge, confidence %, relative fetched-at e.g. "3d ago"). Reuse the existing badge slot (`:202-204`). License rendered as a colored chip (green = open, amber = public-record/scraped, red = proprietary-no-redistribute). Staleness: if `fetched_at` older than the column's `staleness_ttl_days` (already in `Workbook.refresh_policy`, `models.py:184-186`) show a "stale" dot.

---

### File-by-file changes
- `apps/api/services/leadgen/enrichment/provider.py` — add `source_license` attr to `EnrichmentProvider`; thread `license`/`confidence` into a shared provenance builder; `apply_results` writes `field_provenance`.
- `apps/api/services/workbook/provider_runner.py` — include `license` in `_provider_job` return dict (`:88-94`).
- `apps/api/services/workbook/enrichment.py` — persist `result_confidence`; build provenance in `enrich_cell`; extend `_set_enrichment` signature + cell dict (`:571-575`) + `cell_metadata`; extend `_write_back_to_lead` (`:632-676`) to write `field_provenance` + `last_enriched_at`.
- `apps/api/services/leadgen/orm_models.py` — add `field_provenance = Column(JSON/Text, default=...)` to `LeadRow`.
- `apps/api/services/leadgen/models.py` — add `field_provenance` to the `Lead` dataclass + `to_dict`/`from_dict` round-trip.
- `apps/api/services/leadgen/store.py` — ensure `update_lead_fields` persists the JSON column (read-modify-write merge, not overwrite).
- `apps/api/services/workbook/schemas.py` — `Provenance` model + `EnrichmentOverlay.provenance` (`:128-135`).
- `apps/api/routers/workbooks.py` — populate `provenance` in both read branches (`:441,484`); lead detail response.
- New: `apps/api/services/leadgen/enrichment/licenses.py` — `PROVIDER_LICENSE` central map + `LICENSE_VOCAB` + `provenance_for(provider_name, confidence)` helper.
- New migration `migrations/versions/<rev>_field_provenance.py` (down_revision `a7b8c9d0e1f2`) — add nullable `leads.field_provenance` JSON; no RLS DDL (inherits leads policy); SQLite-safe via batch. Backfill (D4) optional/no-op.
- ~35 provider files — annotate `source_license` (default `"unknown"` if untouched; only annotate the well-known ones initially).
- `apps/web/src/lib/workbook-api.ts` — `Provenance` type + `EnrichmentOverlay.provenance`.
- `apps/web/src/pages/workbook-editor.tsx` — provenance hover card in `CellRenderer`.

---

### Data / persistence + tenancy / security
- Both homes are existing RLS-tenant-scoped rows (`WorkbookRow`/`WorkbookEnrichment` workspace_id NOT NULL, leads RLS policy). No new policy, no new FORCE-RLS table → **no new cross-tenant attack surface**. The new `leads.field_provenance` column is covered by the table's existing `current_setting('app.workspace_id')` policy.
- **Confused-deputy / MCP** (`apps/mcp/server.py`): provenance must only ever be read through the same `current_workspace`-scoped deps as the values themselves (`workbooks.py` uses `Depends(current_workspace)` throughout). The MCP server already runs lead/workbook reads under a workspace scope; since provenance is just extra keys on objects it already returns, it inherits that scoping — but add an explicit test that an MCP/agent read in workspace A never returns provenance for workspace B (it can't, since the row isn't returned, but assert it to lock the invariant).
- No secrets in provenance: `source` is a provider name, never an API key. Confirm no provider leaks credentials into `fields` that would land in the JSON.

### License / compliance
- This feature is itself a **compliance enabler** (Clay-parity "where did this data come from / can I use it"). `LICENSE_VOCAB` (D3): `CC0-1.0`, `CC-BY-4.0`, `public-record` (SEC/Companies House/GLEIF gov registries), `scraped` (website/DDG — derived data, use-at-own-risk), `proprietary-api` (Hunter/Apollo/PDL/Snov — ToS typically forbids redistribution/resale), `user-provided` (CSV import / manual edit), `unknown`. The UI red chip on `proprietary-api` is the user-facing "do not resell this fact" signal.

### Infra / deploy
None. No new services, no queue changes, no new RLS migration risk (single additive column). Works on SQLite self-host and Postgres identically.

### Feature flag / rollout (default OFF)
- Add `PROVENANCE_TRACKING_ENABLED: bool = False` to `apps/api/core/config.py` (alongside `:108` etc.).
- Write path: when OFF, skip building/persisting provenance (existing cell dict unchanged → byte-identical to today). API: `provenance` omitted/null. UI: card hidden when field absent. Migration still adds the column (cheap, inert) so flipping the flag needs no redeploy of schema.

---

### Test plan
- **Unit (no PG):** `provenance_for()` maps providers→license correctly; unknown provider → `"unknown"`; `EnrichmentOverlay` serializes/deserializes `provenance`; `Lead.to_dict/from_dict` round-trips `field_provenance`; flag OFF produces a cell dict byte-identical to pre-change.
- **Write-path:** mock a provider returning a known value → assert cell JSON gains `provenance{source,license,confidence,fetched_at}` and `result_confidence` is persisted (regression: it's dropped today); assert `_write_back_to_lead` populates `field_provenance[field]` + `last_enriched_at`.
- **PG-gated** (`PYTHONPATH=. uv run --group dev python -m pytest`, throwaway db — never touch `yupcha` db): run the `field_provenance` migration up+down on live PG; assert leads RLS still isolates rows including the new column; assert a workspace-A session cannot read workspace-B provenance via `GET /api/workbooks/{id}`.
- **API:** `GET /api/workbooks/{id}` returns `provenance` on enriched cells, null on un-enriched; flag OFF → field absent.
- **Frontend:** snapshot/RTL on `CellRenderer` showing the provenance card with a license chip + relative fetched-at; absent when `provenance` undefined.
- **Migration backfill:** assert existing leads survive with `field_provenance = {}` (or NULL) and the column is nullable.

### Acceptance criteria
1. With the flag ON, every workbook cell produced by an enrichment/waterfall run carries `provenance{source, license, confidence, fetched_at}` in `WorkbookRow.enrichments` and the `WorkbookEnrichment.cell_metadata` mirror.
2. The numeric confidence computed at `enrichment.py:410` is persisted (today it is discarded).
3. Every lead field written by `_write_back_to_lead`, leadgen-core `apply_results`, and the email waterfall records `LeadRow.field_provenance[field]` + refreshes `last_enriched_at`.
4. `GET /api/workbooks/{id}` returns `provenance` on `EnrichmentOverlay` for enriched cells; the field is optional and absent on legacy/un-enriched cells (backward-compatible).
5. License values come only from `LICENSE_VOCAB`; unmapped providers resolve to `"unknown"`, never crash.
6. The workbook grid shows a hover provenance card (source, license chip, confidence %, relative fetched-at) for cells with provenance; nothing renders for cells without it.
7. With the flag OFF the produced cell JSON and API payload are byte-identical to current behavior.
8. Tenancy: a workspace-A request never receives workspace-B provenance (PG-gated test passes); migration up/down is clean on PG and SQLite.
9. The new `leads.field_provenance` column is nullable/defaulted; existing rows are unaffected; no new RLS policy is required.

### Out of scope
- A queryable `fact_provenance` table / cross-workbook "show all CC-BY facts" reporting (revisit if compliance-reporting demand appears).
- Retroactive backfill of provenance for already-enriched historical cells/leads (they predate the data; D4).
- Provenance for non-enrichment column types (ai_formula, formula, http) beyond recording `source=<col_type>` — full LLM citation provenance is the existing research/agent trace feature (`CellTrace`).
- License *enforcement* (blocking exports of proprietary facts) — this spec only surfaces, doesn't gate.
- Per-fact provenance versioning/history (we keep latest-write only).
