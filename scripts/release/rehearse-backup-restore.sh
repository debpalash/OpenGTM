#!/usr/bin/env bash
# Backup and restore drill (release gate for issue #120): on a rehearsal stack with seeded data, take a
# pg_dump -Fc backup, destroy the database volume, restore into a fresh stack, and verify that every table has
# the same row count, that the admin can still log in, and that a saved provider key still decrypts (which
# needs the SAME SECRET_KEY: the drill reuses the first stack's env file, as an operator restoring on the same
# host must reuse .env). Prints PASS/FAIL per step; exits non-zero on any failure.
#
#   scripts/release/rehearse-backup-restore.sh
#
# Environment: see rehearse-install.sh (REHEARSAL_NO_BUILD, REHEARSAL_KEEP, REHEARSAL_PRUNE_IMAGES,
# REHEARSAL_HEALTH_TIMEOUT, REHEARSAL_TMPDIR). The whole run uses one project, opengtm-rehearsal-<pid>, which is
# removed on exit (`down -v`).
set -uo pipefail
. "$(dirname "${BASH_SOURCE[0]}")/rehearsal-lib.sh"

rh_init "Backup and restore drill"
START=$SECONDS
PROVIDER="anthropic"
SAVED_KEY="sk-rehearsal-$(rh_rand 12)-KEYEND"
DUMP="$RH_WORK/yupcha.dump"
# What a masked saved key looks like: first 8 chars, four bullets, last 4 (settings.MaskKey).
EXPECT_MASK="${SAVED_KEY:0:8}••••${SAVED_KEY: -4}"
RH_SECRET_NOTE="the restored stack reuses SECRET_KEY from the original .env; with another SECRET_KEY the saved key reads back as an undecryptable '••••••••'"

# Row count of every table in public, one "table=count" line each, sorted.
rh_counts() {
  rh_psql "SELECT table_name || '=' || (xpath('/row/c/text()', query_to_xml(format('SELECT count(*) AS c FROM %I.%I', table_schema, table_name), false, true, '')))[1]::text
           FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE' ORDER BY 1"
}

save_provider_key() {
  local body; body="$(python3 -I -c 'import json,sys; print(json.dumps({"api_key":sys.argv[1]}))' "$SAVED_KEY")"
  rh_api "/api/settings/providers/$PROVIDER" -X PUT -H 'Content-Type: application/json' -d "$body" >/dev/null || return 1
  check_provider_key
}

# The masked key is derived from the decrypted value, so a matching mask proves the stored secret decrypts.
check_provider_key() {
  local got
  got="$(rh_api /api/settings/providers | rh_json "[p for p in d['providers'] if p['id']=='$PROVIDER'][0]['api_key_masked']")" || return 1
  [[ "$got" == "$EXPECT_MASK" ]] || { echo "saved $PROVIDER key reads back as '$got', expected '$EXPECT_MASK'"; return 1; }
  echo "GET /api/settings/providers: $PROVIDER api_key_masked = $got (decrypted with the stack's SECRET_KEY)"
}

quiesce_and_count() {
  dc stop api-go worker-go scheduler-go || return 1
  rh_counts >"$RH_WORK/counts.before" || return 1
  [[ "$(wc -l <"$RH_WORK/counts.before")" -ge 50 ]] || { echo "unexpectedly few tables counted"; cat "$RH_WORK/counts.before"; return 1; }
  grep -E '^(users|workspaces|workspace_members|workbooks|workbook_rows|leads|app_settings|workspace_settings)=' "$RH_WORK/counts.before" | tr '\n' ' '
  echo
  [[ "$(grep '^workbooks=' "$RH_WORK/counts.before" | cut -d= -f2)" -ge 1 ]] || { echo "no workbook was seeded"; return 1; }
  [[ "$(grep '^users=' "$RH_WORK/counts.before" | cut -d= -f2)" == "1" ]] || { echo "expected exactly one user"; return 1; }
}

take_backup() {
  dc exec -T postgres pg_dump -U yupcha -d yupcha -Fc >"$DUMP" || return 1
  [[ -s "$DUMP" ]] || { echo "dump is empty"; return 1; }
  # A custom-format archive must list its own table data.
  dc exec -T postgres pg_restore --list <"$DUMP" >"$RH_WORK/dump.list" || { echo "pg_restore --list failed"; return 1; }
  grep -q 'TABLE DATA public users' "$RH_WORK/dump.list" || { echo "archive has no data for public.users"; return 1; }
  echo "pg_dump -Fc: $(du -h "$DUMP" | cut -f1), $(grep -c 'TABLE DATA' "$RH_WORK/dump.list") tables with data"
}

destroy_database_volume() {
  dc down -v --remove-orphans || return 1
  if docker volume inspect "${RH_PROJECT}_pgdata" >/dev/null 2>&1; then echo "volume ${RH_PROJECT}_pgdata still exists"; return 1; fi
  echo "docker compose down -v removed ${RH_PROJECT}_pgdata and the containers"
}

postgres_up() {
  dc up -d postgres redis || return 1
  rh_wait 90 "postgres healthy" bash -c "[[ \"\$(docker inspect -f '{{.State.Health.Status}}' \$(docker compose -p '$RH_PROJECT' ps -q postgres) 2>/dev/null)\" == healthy ]]" || return 1
  [[ "$(rh_psql "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'")" == "0" ]] || { echo "the fresh database is not empty"; return 1; }
  echo "fresh postgres is up with an empty database"
}

restore_backup() {
  # Roles are cluster-level and not part of a pg_dump of one database, but the dump's GRANT statements name
  # them, so they must exist before pg_restore. Create them as inert NOLOGIN roles; the migrate service gives
  # the runtime role its login and password again on the next start (YUPCHA_PROVISION_RUNTIME_ROLE=1).
  local r
  for r in yupcha_app yupcha_runtime; do
    rh_psql "CREATE ROLE $r NOLOGIN NOSUPERUSER NOBYPASSRLS INHERIT" >/dev/null || return 1
  done
  dc exec -T postgres pg_restore -U yupcha -d yupcha --exit-on-error <"$DUMP" 2>&1 | tail -n 20
  return "${PIPESTATUS[0]}"
}

counts_match() {
  rh_counts >"$RH_WORK/counts.after" || return 1
  if ! diff -u "$RH_WORK/counts.before" "$RH_WORK/counts.after"; then echo "row counts differ after restore"; return 1; fi
  echo "all $(wc -l <"$RH_WORK/counts.after") public tables have identical row counts (checked before any app service started)"
}

rh_step "build or reuse the stack images"                          rh_build
rh_step "start the default stack from clean volumes"               rh_up
rh_step "stack becomes healthy"                                    rh_wait_healthy
rh_step "first-run setup through the API"                          rh_first_run_setup
rh_step "log in"                                                   rh_login
rh_step "seed the demo workbook"                                   rh_seed_demo
rh_step "check the seeded workbook through the API"                rh_check_demo_workbook
rh_step "save a provider key (encrypted secret) and read it back"  save_provider_key
rh_step "stop writers and record per-table row counts"             quiesce_and_count
rh_step "take the backup (pg_dump -Fc) and list its contents"      take_backup
rh_step "destroy the database volume (down -v)"                    destroy_database_volume
rh_step "start a fresh postgres (empty database)"                  postgres_up
rh_step "restore the backup (pg_restore)"                          restore_backup
rh_step "row counts of every table match the backup source"        counts_match
rh_step "start the rest of the stack on the restored database"     rh_up
rh_step "restored stack becomes healthy (migrate is a no-op)"      rh_wait_healthy
rh_login_after_restore() {
  [[ "$(rh_setup_status | rh_json "d['setup_required']")" == "False" ]] || { echo "setup is open again after restore"; return 1; }
  rh_login
}
rh_step "restored admin logs in (setup is closed)"                 rh_login_after_restore
rh_step "demo workbook reads back after restore"                   rh_check_demo_workbook
rh_step "saved provider key still decrypts after restore"          check_provider_key
rh_step "UI index is served after restore"                         rh_check_ui
rh_step "/healthz after restore"                                   rh_check_healthz
rh_info "elapsed $((SECONDS - START))s"
rh_finish
