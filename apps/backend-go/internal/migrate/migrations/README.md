Go schema migrations after the baseline (version 1, `../baseline.go`).

Name files `NNNNN_short_name.sql` with the next free number (`00002_...`), goose annotations
(`-- +goose Up`, `-- +goose StatementBegin/End` around `$$` bodies, `-- +goose NO TRANSACTION`
for `CREATE INDEX CONCURRENTLY`). Up only, additive while the Python canary still runs.
Then run `make schema` (regenerates `internal/db/schema.sql`), `make sqlc`, and bump
`config.RequiredGoSchemaVersion` if the Go code needs the change. See
docs/go-port/runbooks/migrations.md.
