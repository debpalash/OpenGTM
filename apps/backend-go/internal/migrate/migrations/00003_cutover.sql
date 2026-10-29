-- +goose Up
-- Plan 15 WP-CUTOVER (Python-free runtime).
--
-- 1. The collection ledger marker. The Go leadgen routes (collect, jobs, lead utils, source settings) and the copilot
--    start_collection tool require app_settings __collection_ledger_backend = 'postgres', which Python's
--    scripts/migrate_collection_ledger.py wrote after moving the ledger out of SQLite. A fresh install has no SQLite
--    ledger, so the marker is written here when absent. An existing value is kept: an install whose marker still
--    names another backend has collection history in SQLite that must be migrated (or knowingly abandoned) first;
--    see docs/go-port/runbooks/15-cutover.md.
INSERT INTO app_settings (key, value) VALUES ('__collection_ledger_backend', 'postgres') ON CONFLICT (key) DO NOTHING;

-- 2. The refresh tenant fallback (migration 2) served refresh jobs Python enqueued without jobs.workspace_id. The
--    worker now takes the tenant from the job row only, so the cross-tenant lookup is dropped.
DROP FUNCTION IF EXISTS public.workbook_workspace_by_id(text);
