---
title: Database migrations
description: The Go migration runner owns the database schema. How deploys apply it and how to add a migration.
sidebar:
  order: 4
---

The PostgreSQL schema is owned by `opengtm-migrate` (`apps/backend-go/cmd/migrate`,
[goose](https://github.com/pressly/goose) with the migrations embedded in the
binary). The Go services refuse to start until the schema is at the version they
require.

## How it is wired

- Docker Compose runs it as the one-shot `migrate` service with the
  schema-owner role; every other service waits for it to finish.
- Outside Compose: `make migrate` (builds `bin/opengtm-migrate` and runs `up`
  with `DATABASE_URL`).
- An empty database gets the baseline. A database created by the earlier Python
  release (Alembic at its final revision) is adopted in place; an older Alembic
  revision is refused.
- Runs are serialized by a PostgreSQL advisory lock, so concurrent deploys are
  safe. With `YUPCHA_PROVISION_RUNTIME_ROLE=1` it also creates or updates the
  non-superuser runtime login (`YUPCHA_RUNTIME_DB_USER` /
  `YUPCHA_RUNTIME_DB_PASSWORD`).

## Common commands

```bash
DATABASE_URL=postgresql://owner:...@host:5432/db bin/opengtm-migrate up      # apply pending
DATABASE_URL=...                                   bin/opengtm-migrate status  # applied / pending
docker compose run --rm migrate status
```

`DATABASE_URL` needs owner credentials (DDL, `CREATE ROLE` when the app role is
missing).

## Writing a new migration

Add a numbered SQL file under `apps/backend-go/internal/migrate/migrations/`
(see the README there), then run `make schema` (regenerates
`internal/db/schema.sql`; `TestSchemaSQLMatchesMigrations` fails on drift) and
`make sqlc` in `apps/backend-go`, and bump `config.RequiredGoSchemaVersion` if
the Go code needs the change.

The full runbook is `docs/go-port/runbooks/migrations.md` in the repository.
