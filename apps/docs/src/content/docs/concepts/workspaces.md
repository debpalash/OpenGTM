---
title: Workspaces and tenancy
description: Roles, the X-Workspace-Id header, and how PostgreSQL row-level security makes isolation fail closed.
sidebar:
  order: 5
---

Every lead, workbook, signal, sequence, automation and token belongs to a
**workspace**. Users are members of one or more workspaces with a role.

## Roles

| Role | Can |
|---|---|
| `owner` | Everything; implicitly satisfies any role check |
| `admin` | Manage members, tokens, outreach, automations, billing |
| `editor` / `member` | Create and run workbooks, spend within ceilings |
| `viewer` | Read only |

Admin-only MCP capabilities (`automations:write`, `outreach:send`,
`leads:delete`, `workbooks:delete`) require the `admin` role at call time,
not just at token-mint time.

## Selecting a workspace

Send `X-Workspace-Id: <id>` on any request. Without it, the user's stored
active workspace is used. Membership is always enforced. A workspace you are
not a member of returns `403` with the same message whether it exists or not;
cross-tenant object ids return `404`, so existence never leaks.

## How isolation is enforced

1. The access token resolves a user. Refresh tokens are rejected everywhere an
   access token is expected, including SSE and WebSocket query-string auth.
2. The workspace is bound to the request as a context variable and, on
   PostgreSQL, set transaction-locally with `set_config('app.workspace_id', …)`.
3. Application checks give clear 403/404 responses.
4. PostgreSQL row-level security is the final boundary. Policies are
   fail-closed: with no workspace set, every tenant table returns zero rows.
   The runtime database role must not be a superuser or hold `BYPASSRLS`;
   with `PG_RLS_REQUIRE_SAFE_ROLE=1` (default) the app refuses to boot
   otherwise.

Workers set the same scope for every claimed job. A handler with no workspace
id fails instead of falling back to a global tenant.

## Workspace secrets

Provider keys and integration credentials entered in **Settings → API Keys**
are stored per workspace, encrypted with Fernet under `SECRETS_MASTER_KEY`
(derived from `SECRET_KEY` when unset). Resolution falls back to the global
setting or environment variable, so a single-tenant install can keep using
`.env`. In a non-development environment the app refuses to encrypt secrets
under the shipped default key.

## Today's boundary

Workspace membership, active-workspace state and encrypted secrets live in a
SQLite control-plane file on the shared `data/` volume. That is reliable for
the documented single-host Compose topology; replicas on separate hosts need
those stores moved to PostgreSQL first. See
[Architecture](/reference/architecture/#scaling-rules).
