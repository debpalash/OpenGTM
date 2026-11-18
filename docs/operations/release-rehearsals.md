# Release rehearsals

Scripted gates for the release-readiness checklist in [issue #120](https://github.com/debpalash/OpenGTM/issues/120).
Each script runs against a throwaway Docker Compose project, prints `PASS` or `FAIL` per step, exits non-zero on
the first failure, and removes everything it created.

| Script | What it proves | Status today |
|---|---|---|
| `scripts/release/rehearse-install.sh` | A clean checkout starts with the default Compose stack, first-run setup works through the API, login works, the demo workbook seeds and reads back, the UI and `/healthz` answer. | passes |
| `scripts/release/rehearse-backup-restore.sh` | A `pg_dump -Fc` backup restores into a fresh stack with identical row counts in every table, the admin logs in with the old password, and a saved provider key still decrypts. | passes |
| `scripts/release/rehearse-upgrade.sh` | A real v3.0.0 installation (built from the tag's own code and migrations, seeded through its API) is bridged by the Go migrator, its SQLite stores are imported, and the same admin logs in and sees every seeded item through the Go API. | passes (run it locally; see "Upgrade from v3.0.0") |

CI runs all three on every push to `main` and on demand (`.github/workflows/release-rehearsals.yml`). They are
not in the pull request workflow because building every image and running two stacks takes far longer than the
~5 minutes a PR should wait.

## Running them

Needs Docker with Compose v2.24 or newer, `curl`, `python3` and several GB of free disk for the images (the first
run builds them; later runs reuse the layer cache). Run from anywhere inside the checkout:

```sh
scripts/release/rehearse-install.sh
REHEARSAL_NO_BUILD=1 scripts/release/rehearse-backup-restore.sh   # reuse the images the first run built
```

| Variable | Effect |
|---|---|
| `REHEARSAL_NO_BUILD=1` | do not build; require the images from an earlier rehearsal |
| `REHEARSAL_KEEP=1` | leave the stack running afterwards (the script prints the `down -v` command) |
| `REHEARSAL_PRUNE_IMAGES=1` | also delete the rehearsal images at the end |
| `REHEARSAL_HEALTH_TIMEOUT` | seconds to wait for health (default 240) |
| `REHEARSAL_TMPDIR` | where the generated env file lives (default `~/.cache`; `/tmp` may be RAM-backed) |

### How they stay out of the way

- The Compose project is `opengtm-rehearsal-<pid>`. Teardown is a trap that runs `docker compose -p <project>
  down -v --remove-orphans`, so it cannot touch another project, and it reports how many containers and volumes
  are left (0).
- Host ports (web, API, Postgres, Redis) are random free loopback ports, passed through the `PORT`, `API_GO_PORT`,
  `POSTGRES_PORT` and `REDIS_PORT` variables the root `docker-compose.yml` already reads. The defaults are
  unchanged.
- `lead_net` is pinned in the root file (`LEAD_NET_SUBNET`, default `172.29.250.0/24`). The rehearsal picks an unused
  `10.1xx.x.0/24` instead, outside Docker's own default pools, because Compose creates the project's other
  networks in parallel and a 172.x pin can race with them.
- `scripts/release/rehearsal.compose.yml` resets the fixed `container_name`s, uses separate image tags
  (`opengtm-rehearsal-*:local`), ignores a repo-root `.env`, and seeds the demo workbook without starting its
  enrichment run (that run calls external providers).
- Secrets are generated per run with the same variables `scripts/install.sh` writes into `.env`.

## What each step checks

### `rehearse-install.sh`

1. **Build or reuse the images** and **start the default stack** from empty volumes (postgres, redis, `migrate`,
   `api-go`, `worker-go`, `scheduler-go`, `browser`, `nginx`).
2. **Healthy**: `migrate` exited 0, the long-running services are running and not unhealthy, `/healthz` answers
   through nginx.
3. **First-run setup**: `GET /auth/setup/status` says setup is required; the one-time setup token is read from the
   `api-go` log (as the README tells a user to); `POST /auth/setup` creates the admin and the `Main` workspace; the
   status then reports setup closed.
4. **Login**: `POST /auth/token`, then `GET /auth/me`.
5. **Seed and read back**: the compose `demo` profile (`cmd/seed`) creates the demo workbook;
   `GET /api/workbooks/` lists it and `GET /api/workbooks/demo-zero-key-firstrun` returns 25 rows and 10 columns.
6. **UI and health**: `GET /` returns the HTML shell, the referenced `/assets/*.js` bundle is served, `/healthz`
   returns 200.

Expected tail:

```
== Fresh-install rehearsal summary (project opengtm-rehearsal-3750326) ==
  PASS build or reuse the stack images
  ...
  PASS /healthz
RESULT: PASS
teardown: project opengtm-rehearsal-3750326 removed (containers and volumes left: 0)
```

### `rehearse-backup-restore.sh`

Steps 1 to 5 of the install rehearsal, then:

1. Saves a provider key through `PUT /api/settings/providers/anthropic` and reads it back. The API returns the key
   masked from its decrypted value (first 8 characters, last 4), so a matching mask proves the stored secret
   decrypts.
2. Stops `api-go`, `worker-go` and `scheduler-go` (a consistent backup needs no writers) and records the row count of
   every table in `public`.
3. `pg_dump -Fc`, and checks the archive lists data for `public.users`.
4. `docker compose down -v`: the database volume is destroyed.
5. Starts a fresh Postgres, creates the `yupcha_app` and `yupcha_runtime` roles (roles are cluster-level and not in a
   dump of one database, but the dump's `GRANT`s name them), runs `pg_restore --exit-on-error`, and compares the
   row counts of all tables **before any application service starts**.
6. Starts the rest of the stack (`migrate` is a no-op on a restored database and re-provisions the runtime login),
   waits for health, and checks: setup is still closed, the admin logs in with the old password, the demo workbook
   reads back, the saved provider key still decrypts, the UI and `/healthz` answer.

**The restored stack must use the same `SECRET_KEY`.** Provider keys and other stored secrets are encrypted with a
key derived from it, and sessions are signed with it. The drill reuses the first stack's env file, as an operator
restoring on the same host reuses `.env`. With a different `SECRET_KEY` the database restores, but every saved secret
reads back as an undecryptable `••••••••` and has to be entered again. Back up `.env` together with the database,
and keep it separate from the database dump.

### `rehearse-upgrade.sh`

Builds a real v3.0.0 installation in a container from `git archive v3.0.0` with that tag's locked dependencies:
its Alembic builds the database (head `3f4051627384`), then `scripts/release/v3-seed.py` starts the v3 API and
creates an admin, a second workspace, a saved provider key, an encrypted workspace secret (HubSpot token), two leads
in two workspaces and a workbook function through HTTP, and a chat conversation and queue rows with v3's own code,
leaving its SQLite data directory in a host directory. The script then runs, as an operator would,
`docker compose run --rm migrate` (bridge, adopt), `migrate import-v3 --dry-run`, `migrate import-v3` twice (the
second run must insert nothing), starts the stack, logs in with the v3 admin password and checks through the Go API
that the workspace, the lead (visible only in its own workspace), the function, the conversation and both messages,
the provider key (masked) and the workspace secret (integration listed, value never echoed) are
there. `--python-ref <ref>` builds another ref (the checks assume v3.0.0's data model).

The seed follows the default deployment path (the v3 application connects as a non-superuser role), and the script
fails if any SQLite lead/outreach/signal store appears. It also runs in the CI workflow after the other two, reusing
their images (about 90 s locally with cached images, including building the v3 Python environment).

## Upgrade from v3.0.0

The supported path is v3.0.0 directly to 4.x, with no intermediate release. It is forward-only: take and verify a
backup first (see "Back up"); rolling back means restoring that backup and running the v3.0.0 images. Keep the
v3.0.0 `SECRET_KEY` (workspace secrets are copied still encrypted). The commands are in the README section "Upgrade
an existing Python installation" and the mechanism, including the table of what each SQLite store maps to, is in
`docs/go-port/runbooks/migrations.md`.

Known limits, all reported by `import-v3` rather than hidden:

- A default v3.0.0 install keeps leads, outreach history and signals in PostgreSQL, not in SQLite files. Established
  by running the quick start from the tag (`scripts/install.sh`, then `docker compose up`, the seed service, then a
  lead and an outreach sequence with an enrolled lead through the API): the api connects as `yupcha_runtime`
  (a non-superuser, from `APP_DATABASE_URL`), the rows land in `leads` and `outreach_sequences`, and `data/` holds only
  `workspaces.db`, `data.db` and `chat_history.db`. `signals.db` is documented in the v3.0.0 source as no longer
  written or read. The bridge upgrades those PostgreSQL rows in place; `import-v3` has nothing to copy for them.
- v3.0.0 falls back to per-workspace SQLite `leads.db` files (and `outreach.db`/`signals.db`) only when `PG_LEAD_STORE`
  is off or its database login is a superuser/BYPASSRLS role (the rehearsal hit this when its first seed ran as the
  database owner). `import-v3` does not copy those files. It therefore **stops before writing anything** when one has
  rows, naming the file and the v3.0.0 step to run first (`scripts/migrate_sqlite_to_pg.py`, in `apps/api/scripts/`
  of the v3.0.0 tree); `--skip-unmigrated` imports the rest and leaves those rows behind in the SQLite files.
  Empty files do not stop it.
- `enrichment_cache.db` (a provider-result cache with an `expires_at` per row) and `openmemory.db` (an optional
  third-party memory backend) are not imported and never stop the import; the cache is rebuilt on demand.
- Pending jobs with no derivable workspace are failed with a reason by the bridge; running jobs are re-queued.
- Not covered by the rehearsal: SCIM/OIDC rows, per-workspace SQLite stores, and real provider traffic. The
  integration tests cover the import of the SCIM/OIDC/permission tables from committed fixtures.

## Operator procedures

Migrations are forward-only. There is no down migration and no supported schema downgrade. **Rollback means
restoring the backup taken before the upgrade and running the previous images.**

### Back up

Before every upgrade, and on a schedule:

```sh
docker compose stop api-go worker-go scheduler-go     # consistent backup: no writers (add mcp if you run it)
docker compose exec -T postgres pg_dump -U "${POSTGRES_USER:-yupcha}" -d "${POSTGRES_DB:-yupcha}" -Fc > opengtm-$(date +%F).dump
docker compose exec -T postgres pg_restore --list < opengtm-$(date +%F).dump | head   # the archive must list tables
cp .env opengtm-env-$(date +%F).bak                    # SECRET_KEY lives here: store it apart from the dump
docker compose start api-go worker-go scheduler-go
```

Also copy any bind-mounted data directory you use (`data/`). Record the image tags or commit you were running.
A backup you have not restored is not a backup: run `rehearse-backup-restore.sh` on the release candidate, and
restore your own dump into a scratch project at least once.

### Restore (same host or a new one)

1. Put the original `.env` in place (same `SECRET_KEY`, `POSTGRES_PASSWORD`, `YUPCHA_RUNTIME_DB_PASSWORD`).
2. `docker compose up -d postgres redis` on an empty database volume (`docker compose down -v` first if you are
   replacing a damaged one; that deletes the old data).
3. Create the roles the dump's grants name:

   ```sh
   docker compose exec -T postgres psql -U "${POSTGRES_USER:-yupcha}" -d "${POSTGRES_DB:-yupcha}" \
     -c 'CREATE ROLE yupcha_app NOLOGIN' -c 'CREATE ROLE yupcha_runtime NOLOGIN'
   ```

4. `docker compose exec -T postgres pg_restore -U "${POSTGRES_USER:-yupcha}" -d "${POSTGRES_DB:-yupcha}" --exit-on-error < opengtm-YYYY-MM-DD.dump`
5. `docker compose up -d`. The `migrate` service re-provisions the runtime login and applies migrations newer than
   the dump, if any.
6. Check `/healthz`, log in, open a workbook, and confirm a saved provider key still shows as configured (not
   `••••••••`).

### Roll back a failed upgrade

1. Stop everything that writes: `docker compose down` (keep volumes).
2. Check out the previous release tag (or pull the previous image tags) and restore the pre-upgrade `.env`.
3. Replace the database with the pre-upgrade backup: remove the database volume
   (`docker compose down -v` deletes **all** volumes of the project, so only do this once you hold a verified
   backup) and follow "Restore" above, starting the **previous** images.
4. Do not start the newer images against the restored database again until the cause is understood. Data written
   after the backup (including after a partial upgrade) is lost; export anything you need from the failed stack
   first.

Do not run the newer `migrate` against a database and then switch back to older binaries: the older Go binaries
check `goose_db_version` and accept a newer schema, but compatibility is not verified for any migration (schema 5
in particular needs all writers stopped and replaced together; see `docs/go-port/runbooks/migrations.md`).
