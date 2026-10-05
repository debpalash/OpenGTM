# Runbook: backup, restore, upgrade and rollback

For installs created with `opengtm init` (Docker Compose). Every command takes
`--dir DIR` (default: the current directory) and works on that install. See the
[self-hosting guide](../self-hosting.md) for installation and the files
involved.

Two rules explain most of what follows:

1. **A backup is the only thing that can undo a migration.** Rollback restores
   a backup; it does not run Alembic downgrades, which are not guaranteed to
   preserve data and cannot be tested the way a restore can.
2. **Anything written after a backup is lost if you restore it.** Rollback
   therefore takes a second, safety backup of the current state first.

## What a backup contains

`opengtm backup` writes a directory `backups/opengtm-<UTC time>[-label]/`:

| File | Content |
| --- | --- |
| `db.dump` | `pg_dump --format=custom` of the application database (run inside the postgres container, so the client always matches the server) |
| `roles.sql` | Idempotent `CREATE ROLE` / `GRANT` script for non-superuser roles, without passwords (policies and grants in the dump reference them) |
| `data.tar.gz` | The data directory (workspace metadata, files). Symlinks and special files are skipped and listed in the manifest |
| `config.tar.gz` | `.env`, `opengtm.yaml`, `compose.yml`. **This holds secrets**, including the key that decrypts provider credentials stored in the database. `--no-config` omits it; then keep `SECRETS_MASTER_KEY` somewhere safe yourself |
| `manifest.json` | Versions, schema revision, table count, and a SHA-256 for every file |

Files are mode 0600. A backup contains personal data and secrets: encrypt it
before it leaves the host (`age`, `gpg`, or an encrypted bucket).

## Back up

```bash
opengtm backup --dir ~/opengtm --quiesce        # consistent
opengtm backup --dir ~/opengtm                  # stack keeps running
opengtm backup list --dir ~/opengtm
opengtm backup verify ~/opengtm/backups/opengtm-20261005T031500Z
opengtm backup verify BACKUP --dir ~/opengtm --scratch   # also restore into a throwaway database
```

- `--quiesce` stops the application services (PostgreSQL stays up), takes the
  backup, and restarts them. Without it the database dump is consistent (it is
  one snapshot) but the data directory is archived while services may still be
  writing to it. For routine backups of a busy system, schedule `--quiesce` in
  a quiet window.
- `backup verify` recomputes every checksum. `--scratch` additionally creates a
  temporary database on the install's PostgreSQL, restores into it, checks the
  schema revision and table count against the manifest, and drops it. Do this
  after your first backup and periodically afterwards: **a backup you have not
  restored is a hope, not a backup**.
- A failed backup leaves nothing behind (the partial directory is removed).

Schedule it (cron, systemd timer) and copy `backups/` off the host:

```cron
15 3 * * *  opengtm backup --dir /srv/opengtm --quiesce --label nightly && rclone copy /srv/opengtm/backups remote:opengtm-backups
```

Pruning is manual: delete old directories under `backups/`.

For an external PostgreSQL, run against it directly with an **owner** role (the
runtime role cannot read RLS-protected tables):
`opengtm backup --database-url postgresql://owner:pw@db.example.com/opengtm --out /backups`
(needs `pg_dump`, `pg_restore` and `psql` of the server's major version or newer on the host).

## Restore

Into the existing install (replaces its database and data directory):

```bash
opengtm restore --dir ~/opengtm --wipe --yes ~/opengtm/backups/opengtm-20261005T031500Z --start
```

- The backup is verified first; nothing is touched if a checksum fails.
- Without `--wipe` the target database must be empty and the command refuses
  before stopping anything. `--wipe` drops every schema in the database (so
  tables created after the backup disappear too) and requires `--yes` in a
  script.
- The database is restored in **one transaction**: a failed `pg_restore` leaves
  what the wipe left, not a half-loaded schema. Take a fresh backup first if you
  are restoring over data you might still want.
- The existing data directory is **moved aside** to `data.pre-restore-<time>`,
  never deleted. Remove it once you are satisfied.
- The application services are stopped for the restore and restarted with
  `--start` (otherwise start them with `docker compose up -d`).

Onto a new host (disaster recovery):

```bash
# 1. Install the same release of the binary (scripts/install.sh --version X.Y.Z).
# 2. Recreate the install files from the backup, keeping the original secrets:
mkdir ~/opengtm && tar -xzf BACKUP/config.tar.gz -C ~/opengtm
mkdir ~/opengtm/data
# 3. Start an empty PostgreSQL with those credentials, restore, start everything:
cd ~/opengtm && docker compose up -d postgres
opengtm restore --dir ~/opengtm BACKUP
docker compose up -d
opengtm doctor --dir ~/opengtm
```

Restoring into a **different database name or URL** works the same way with
`--database-url`; objects are restored with the connecting role as owner
(`--no-owner`). The runtime login role's password is re-applied by the `migrate`
service on the next `up`, from `.env`.

Without `opengtm` at all, the dump is a standard custom-format archive:
`pg_restore --no-owner --single-transaction --exit-on-error -d DB BACKUP/db.dump`
into an empty database (after running `roles.sql` with `psql -f`).

## Upgrade

Install the new binary first, because `opengtm upgrade` ships with the Compose
file the release was tested with:

```bash
scripts/install.sh --version 3.2.0                 # verifies checksum and signature
opengtm upgrade --dir ~/opengtm --dry-run --to 3.2.0
opengtm upgrade --dir ~/opengtm --to 3.2.0
```

Before you start: read the release notes; make sure there is disk space for a
backup (the size of `opengtm backup list`, again); and have a way to reach the
host if something goes wrong. Plan a short outage: the application stops while
the backup and migration run.

What `upgrade` does, in order, stopping at the first failure:

1. Validates the Compose file; starts PostgreSQL; reads the current schema
   revision.
2. **Stops the application services** (PostgreSQL and Redis stay up).
3. **Backs up** (database, data directory, config) and **verifies** it. If this
   fails nothing has changed and the stack is restarted.
4. **Journals** the intent in `.opengtm/state.json` (before anything changes),
   so a crash can still be rolled back.
5. Refreshes `compose.yml` to the version shipped with this binary, keeping a
   copy for rollback. If you edited `compose.yml`, it is left alone and the new
   version is written to `compose.yml.new` for you to merge.
6. Switches the image tags in `.env` and pulls the images (`--no-pull` for
   locally built images).
7. Runs the **migration owner** from the new image: refuses a database that is
   newer than the release, applies Alembic, verifies the head.
8. Starts the stack and waits for every container to be healthy and for the
   front door to answer `/readyz`, to proxy the legacy API, and to report the
   new version.

If step 6 or later fails you are offered a rollback; use `--auto-rollback` in
automation (`--yes` does **not** consent to a rollback, because that discards
data). With neither, the install is left as it is, the error is printed with the
exact rollback command, and the upgrade is recorded as failed.

`opengtm upgrade` refuses to start while a previous upgrade is recorded as
`in_progress` (it was interrupted): run `opengtm rollback`, or inspect the
install and edit the journal by hand.

Skipping the backup (`--no-backup`) is possible and makes rollback impossible.

## Rollback

```bash
opengtm rollback --dir ~/opengtm            # latest upgrade not yet rolled back
opengtm rollback --dir ~/opengtm --yes      # when that upgrade had succeeded
```

It verifies the backup, stops the application services, takes a **safety
backup** (`...-pre-rollback`) of the current state, wipes and restores the
database, moves the current data directory aside and restores the backed-up
one, puts back the previous `compose.yml` and image tags, starts the previous
release, and waits for it to be healthy. Then it marks the upgrade
`rolled_back`.

- If the migration never started (a failed pull, say), the database is not
  touched and only the images are reverted.
- Rolling back an upgrade that **succeeded** discards everything written since
  its backup, so it needs an explicit yes. The safety backup is your way back.
- `--no-safety-backup` exists for when the database is too damaged to dump.
- After a rollback you can retry the upgrade; it takes a new backup.

## Failure reference

| Symptom | Meaning and next step |
| --- | --- |
| `database schema is newer than this release` | The database was migrated by a newer release than the images you are starting. Upgrade the release, or restore a backup from before. Never run an older release against a newer schema. |
| `pre-upgrade backup failed, nothing was changed` | The stack was restarted. Fix the cause (disk space, PostgreSQL) and retry. |
| `pull images` failed | Images not published, or no registry access. Nothing in the database changed; `opengtm rollback` reverts the tags, or fix the access and re-run the upgrade. |
| `health check ... not healthy` | A container is unhealthy or the new version is not what answers. `docker compose logs <service>`, then roll back. |
| `another opengtm ... is running` | The install lock (`.opengtm/lock`) is held; a backup or upgrade is in progress. |
| `target database is not empty` | Use `--wipe` (and `--yes`) if you mean to replace it. |
| `PostgreSQL client tool not found` | Direct-URL mode needs `psql`, `pg_dump`, `pg_restore` on the host; Compose installs use the container's tools. |

## Proven in CI

`scripts/packaging/e2e.sh` runs, on amd64 and arm64, against real containers:
fresh install; refusal to overwrite; backup, verify and restore into a scratch
database; in-place restore with a stray file and a dropped table; upgrade from
the previous release across real schema revisions; manual rollback (database,
data directory, schema revision, tags, safety backup); a deliberately broken
upgrade that rolls back automatically; and a repeat upgrade. See
[`.github/workflows/packaging.yml`](../../.github/workflows/packaging.yml).
What it does not cover: a production-sized database (restore time grows with
data), external PostgreSQL, multi-host setups (M8), and the Kubernetes example.
