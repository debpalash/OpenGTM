-- +goose Up
-- Tenant lookup for refresh jobs enqueued by the Python canary, which carry workspace_id only in the payload
-- (jobs.workspace_id NULL). The workbooks table is FORCE RLS, so the app role cannot read it without a tenant; this
-- narrow SECURITY DEFINER function returns only the owning workspace id (one text value, no other column) of one
-- workbook id. Safe: it discloses nothing beyond "which tenant owns this id" to a caller that already holds the id,
-- takes no tenant input, pins search_path, and is executable by the app role only (PUBLIC revoked). Drop it at cutover.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.workbook_workspace_by_id(p_workbook_id text) RETURNS text
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public', 'pg_temp'
    AS $$
        SELECT w.workspace_id::text FROM public.workbooks w WHERE w.id = p_workbook_id
    $$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION public.workbook_workspace_by_id(p_workbook_id text) FROM PUBLIC;

-- The app role name is configurable (YUPCHA_APP_DB_ROLE), which SQL cannot read: grant to every role the baseline
-- already granted the sibling lookup function to (that is the app role), else to the default name when it exists.
-- +goose StatementBegin
DO $$
DECLARE r text;
BEGIN
    FOR r IN
        SELECT DISTINCT pg_get_userbyid(a.grantee)
        FROM pg_proc p, LATERAL aclexplode(p.proacl) a
        WHERE p.oid = to_regprocedure('public.outreach_send_by_message_id(text)')
          AND a.grantee <> 0 AND a.grantee <> p.proowner
    LOOP
        EXECUTE format('GRANT EXECUTE ON FUNCTION public.workbook_workspace_by_id(text) TO %I', r);
    END LOOP;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'yupcha_app') THEN
        GRANT EXECUTE ON FUNCTION public.workbook_workspace_by_id(text) TO yupcha_app;
    END IF;
END
$$;
-- +goose StatementEnd
