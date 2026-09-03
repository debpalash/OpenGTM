---
title: MCP server
description: Expose OpenGTM to Claude Desktop, Claude Code or any MCP client with scoped, auditable tokens.
sidebar:
  order: 1
---

`apps/mcp` is a Model Context Protocol server (JSON-RPC 2.0, protocol version
`2024-11-05`) that gives agents read access to leads and, when explicitly
enabled, scoped write access.

## Mint a token

Tokens are per workspace and admin-minted:

```bash
curl -X POST $OPENGTM/api/mcp/tokens -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name": "claude-desktop", "capabilities": ["leads:read"], "ttl_days": 90}'
```

The plaintext `ycp_…` token is returned once; only its hash is stored. List
with `GET /api/mcp/tokens`, revoke with `DELETE /api/mcp/tokens/{id}`. Default
TTL is `MCP_TOKEN_TTL_DAYS` (90; 0 never expires).

Capabilities:

| Capability | Unlocks | Role required at call time |
|---|---|---|
| `leads:read` | all read tools | any member |
| `leads:write` | `create_lead`, `update_lead` | editor |
| `sequences:enroll` | `enroll_leads` | editor |
| `workbooks:write` | `create_workbook` | editor |
| `automations:write` | `create_automation` | admin |
| `outreach:send` | `send_email` | admin |
| `leads:delete` | `delete_lead` | admin |
| `workbooks:delete` | `delete_workbook` | admin |

## Configure a client

Claude Desktop (`claude_desktop_config.json`) or any stdio MCP client:

```json
{
  "mcpServers": {
    "opengtm": {
      "command": "python",
      "args": ["-m", "apps.mcp.server"],
      "cwd": "/path/to/opengtm",
      "env": { "OPENGTM_MCP_TOKEN": "ycp_..." }
    }
  }
}
```

For HTTP clients, run `python -m apps.mcp.server --sse 3100`. It binds
`127.0.0.1` only (override with `OPENGTM_MCP_SSE_HOST`), serves `GET /sse`
and `POST /message`, and authenticates **every** request with
`Authorization: Bearer`. Put it behind your own TLS proxy before exposing it.

Single-tenant self-hosted installs with `MCP_REQUIRE_AUTH=0` can run keyless
against the `main` workspace.

## Tools

Read tools (`leads:read`):

| Tool | Arguments |
|---|---|
| `find_leads` | `query`, `city`, `tier` (hot/warm/cold), `limit` |
| `get_lead_detail` | `lead_id` |
| `get_pipeline_stats` | none |
| `enrich_company` | `lead_id` |
| `verify_email` | `email` |
| `get_hiring_signals` | `company`, `lead_id` |
| `score_lead` | `lead_id` |

Write tools appear in `tools/list` only when `MCP_WRITE_ENABLED=1` **and** the
token holds the capability: `create_lead`, `update_lead`, `enroll_leads`,
`create_workbook`, `create_automation`, `send_email`, `delete_lead`,
`delete_workbook`. All accept an `idempotency_key`.

## The write gate

Six checks stand between a write tool call and a database row:

1. The token must hold the capability (missing tools are not even listed).
2. `MCP_WRITE_ENABLED` must be on globally.
3. The user's role is re-checked live; a downgrade to viewer denies the call
   even with a valid token.
4. A per-workspace daily write cap (`MCP_MAX_WRITES_PER_DAY`, 0 = unlimited)
   and idempotency reservation.
5. The token's bound workspace wins; arguments naming another workspace are
   ignored.
6. Exactly one audit row per attempt, including denials.
