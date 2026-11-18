# Release rehearsals

Scripted gates for the release-readiness checklist in [issue #120](https://github.com/debpalash/OpenGTM/issues/120).
Each script runs against a throwaway Docker Compose project, prints `PASS` or `FAIL` per step, exits non-zero on
the first failure, and removes everything it created.

| Script | What it proves | Status today |
|---|---|---|
| `scripts/release/rehearse-install.sh` | A clean checkout starts with the default Compose stack, first-run setup works through the API, login works, the demo workbook seeds and reads back, the UI and `/healthz` answer. | passes |
| `scripts/release/rehearse-backup-restore.sh` | A `pg_dump -Fc` backup restores into a fresh stack with identical row counts in every table, the admin logs in with the old password, and a saved provider key still decrypts. | passes |
| `scripts/release/rehearse-upgrade.sh` | A database built by the Python release (`v3.0.0`) is adopted by the Go migrator and the stack starts on it. | **fails by design**: see "Upgrade from v3.0.0" |

CI runs the first two on every push to `main` and on demand (`.github/workflows/release-rehearsals.yml`). They are
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

Builds a real Python-era database by running the legacy ref's own Alembic (`git archive v3.0.0`, then
`uv sync --frozen && alembic upgrade head` in a container, with that ref's locked dependencies), then runs the Go
migrator (the compose `migrate` service) against it, then starts the stack on the result. Run
`scripts/release/rehearse-upgrade.sh --python-ref <ref>` to use another ref.

## Upgrade from v3.0.0: why it fails today

Checked on 2026-11-18, by running the script:

```
PASS build a Python-era database with v3.0.0's own Alembic
     v3.0.0 built a Python-era database with 69 tables at Alembic revision 3f4051627384
FAIL Go migrator adopts the Python-era database
     opengtm-migrate: migrate: refusing: database is at Alembic revision "3f4051627384", but the Go runner can
     only adopt "e7b2c4d6f8a0". Upgrade it with the legacy tool first (legacy/python at commit 86f41c6 or later:
     `alembic upgrade e7b2c4d6f8a0`), then run opengtm-migrate again
```

- The only published release, v3.0.0, has Alembic head `3f4051627384` (59 revisions). `opengtm-migrate` adopts
  exactly `e7b2c4d6f8a0` and refuses everything else, by design (`docs/go-port/runbooks/migrations.md`).
- The Python revisions between the two are not available here. The runbooks point at `legacy/python` commit
  `86f41c6`; that object is not in this repository, not on any remote ref, and no tag contains
  `e7b2c4d6f8a0`. `b8bfc57e` (used for the rehearsal described in PR #115) is only on `origin/rewrite/hybrid-platform`
  and does not contain that revision either.
- The schema gap is not small: the `e7b2c4d6f8a0` baseline has 20 tables that a v3.0.0 Alembic head does not
  (`workspaces`, `workspace_members`, `workspace_settings`, `app_settings`, `collection_jobs`, the `chat_*`
  tables, the SCIM and OIDC tables, and others). They come either from the missing revisions or from the Python
  application creating them at startup; this was not determined.
- Beyond the schema, a faithful upgrade fixture also needs Python-era rows: users with Python password hashes,
  workspaces, workbooks, queued jobs with a NULL `workspace_id`, and an encrypted secret. Those came from running
  the Python application, which this repository no longer contains.

So, as things stand, **a v3.0.0 installation has no scripted or documented-and-tested path to the Go runtime**. To
close it, someone has to publish (or restore into the repository history) the Python release at Alembic
`e7b2c4d6f8a0` and rehearse `v3.0.0 -> that release -> 4.0.0`. Then run `rehearse-upgrade.sh --python-ref <that
ref>`; the script is expected to pass its adoption step and then continue to the stack checks, which have not been
exercised on an adopted Python database yet. Seeding Python-era data (users, jobs, secrets) and the tenant/job
backfill from `docs/go-port/runbooks/15-cutover.md` are not part of the script and still need a staging rehearsal.

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
