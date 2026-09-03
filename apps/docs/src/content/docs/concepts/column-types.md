---
title: Column types
description: Every workbook column type, what it does, and the config it takes.
sidebar:
  order: 2
---

Column metadata is served live at `GET /api/workbooks/meta/column-types`.
The full config schema is the `ColumnConfig` model in the
[API reference](/api/).

| Type | What it does | Key config |
|---|---|---|
| `lead_field` | Mapped to a lead field. Editable; writes back to the lead. | `lead_field` |
| `source` | Materialises new rows from the sourcing engine using an ICP query | `icp`, `channels`, `target_rows`, optional nested `source.kind` (`crm_import`, `people_search`) |
| `enrichment` | One provider, one target field | `provider`, `target_field`, `condition` |
| `waterfall` | A chain of providers with fallback, ordered by the planner | `waterfall` (provider ids), `target_field`, `verify` |
| `ai_formula` | LLM transformation over row data (classify, rewrite, extract) | `prompt`, `input_columns`, `output_format` |
| `research` | Web-research agent that browses to answer a question per row and cites sources | `prompt`, `max_steps` (1-6), `output_format` |
| `agent` | Goal-directed enrichment: picks tools dynamically within a step and cost budget, records a reasoning trace | `goal`, `tools`, `policy.max_steps`, `policy.max_cost_usd` |
| `http` | Call any HTTP API per row and extract a value with JSONPath | `http_url`, `http_method`, `http_headers`, `http_body`, `http_extract` |
| `formula` | Compute a value from other columns with a safe expression | `formula` |
| `conditional` | Only runs when a condition is met | `condition` |
| `output` | Push the row to a CRM, sequencer, sheet or webhook (run-once) | `destination`, `destination_config` |

## Templates and conditions

Prompts, HTTP fields and webhook bodies accept `{Column Name}` placeholders
that are filled from the row.

Conditions use a small expression language: `{email} == ""`, comparisons,
`AND` / `OR`, and the literals `true`, `false`, `always`, `never`.

Formulas are evaluated by an AST-whitelisted evaluator. There is no `eval`;
attribute access and calls are restricted to a safe set, so
`{Email}.split("@")[1]` works and file or network access does not.

## Which columns cost money

`enrichment`, `waterfall`, `agent` and `research` may call paid providers.
`ai_formula` and `research` use your LLM key. `http`, `formula`,
`conditional` and `lead_field` are free. The
[spend estimate](/concepts/spend/) prices only paid providers.

## Which columns have side effects

Only `output`. It is guarded by a run-once flag per row, so re-running a
workbook does not re-push rows unless you clear the cell.
