# OpenGTM documentation

Start with the [95% recovery plan](plans/opengtm-95-recovery-plan.md). It is the active execution plan for making Chat complete real GTM work reliably before publishing.

## Directory map

| Path | Purpose |
| --- | --- |
| [`architecture.md`](architecture.md) | Current system architecture and major boundaries |
| [`plans/`](plans/) | Active and historical delivery plans |
| [`specs/`](specs/) | Build-ready feature and hardening specifications |
| [`research/`](research/) | Product evaluations, source audits, and OSS research |
| [`council/`](council/) | Historical strategy outputs |

## Current plan

- [`plans/opengtm-95-recovery-plan.md`](plans/opengtm-95-recovery-plan.md): release contract and implementation sequence for reaching a measured 95/100.

## Supporting material

- [`specs/chat-to-action-blueprint.md`](specs/chat-to-action-blueprint.md): Chat action architecture.
- [`specs/workbook-v2-source-engine-spec.md`](specs/workbook-v2-source-engine-spec.md): source-backed workbook behavior.
- [`research/leadgen-oss-ecosystem.md`](research/leadgen-oss-ecosystem.md): current OSS ecosystem findings.
- [`research/sources-master-inventory.md`](research/sources-master-inventory.md): source inventory and enhancement plan.
- [`research/data-source-test-report.md`](research/data-source-test-report.md): source test observations.
- [`plans/clay-parity-specs.md`](plans/clay-parity-specs.md): older Clay parity work items.

## Repository placement rule

Keep deployable code and operational configuration at the repository root. Put prose under `docs/`, external research inputs under `reference/`, maintenance utilities under `scripts/`, and generated or runtime data under `data/`. Do not add new standalone planning or research files at the root.
