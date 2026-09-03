---
title: Research and agent columns
description: Web-browsing research per row with citations, and goal-directed agents that pick their own tools within a budget.
sidebar:
  order: 4
---

## Research columns

A research column browses the web to answer a question about each row, for
example "Does {Company} use Kubernetes? Cite a source." Unlike an AI column,
which reasons over data already in the row, it searches and fetches pages.

Config:

| Field | Meaning |
|---|---|
| `prompt` | The question, with `{Column}` placeholders |
| `input_columns` | Columns to expose to the prompt |
| `max_steps` | Search/fetch steps, 1 to 6 (hard cap 6) |
| `output_format` | `text` or `json` |

Bounds that apply regardless of config: 5 search results per step, 1,500
characters of page text per fetch, and a per-cell dollar budget
(`RESEARCH_CELL_BUDGET_USD`, default $0.05) of which 40% is reserved for the
final synthesis call so the budget is a true ceiling.

The result carries `answer`, `citations`, `cost_usd`, `steps_used` and
`stopped_reason` in the cell metadata. Citations are validated against the set
of URLs the harness actually fetched; anything else is dropped.

Two execution paths exist: the default ReAct loop that works with any
provider, and a native Claude tool-use loop enabled by
`RESEARCH_NATIVE_TOOLS=1` when Anthropic is the default provider. Both use
in-house `search` and `fetch` tools behind the SSRF guard and the
prompt-injection fence. The native path degrades to the default loop on any
failure, so a cell never crashes. An offline evaluation gate compares the two
paths on fixtures and fails the build if the native path regresses on
correctness or drops below 90% citation presence.

## Agent columns

Where a waterfall runs a fixed provider order, an agent column is
goal-directed. Each step it re-plans with the same cost-aware planner, picks
the next provider, observes the result, reroutes on rate limits instead of
failing, and stops when the goal is met, the budget is spent, or the tools are
exhausted.

Config:

```json
{
  "goal": "Find a verified work email for the CTO",
  "tools": ["crosslinked", "hunter_io", "apollo_io"],
  "policy": { "max_steps": 6, "max_cost_usd": 0.10 }
}
```

`tools` defaults to the target field's default waterfall; `max_steps` defaults
to 6 and `max_cost_usd` to $0.10. The per-cell budget is the smaller of the
policy cap and the workbook's remaining ceiling.

Every cell stores a trace you can audit:

```json
{
  "goal": "Find a verified work email for the CTO",
  "steps": [
    { "step": 1, "provider": "crosslinked", "success": true, "value": "Jane Doe — CTO", "cost": 0, "reason": "free first" },
    { "step": 2, "provider": "hunter_io", "success": true, "value": "jane@…", "cost": 0.04, "reason": "best hit-rate per $" }
  ],
  "outcome": "found",
  "spent": 0.04
}
```

Outcomes are `found`, `budget` or `exhausted`. Traces are stored per workspace
under row-level security. Chat can add an agent column for you with the
`add_agent_column` action after confirmation.
