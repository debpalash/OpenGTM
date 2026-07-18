# SWARM.md — the GTM engineering harness

> **v1 · 2026-07-16**
> Read [`GOAL.md`](GOAL.md) first (what we're doing and why) and
> [`RULES-OF-ENGAGEMENT.md`](RULES-OF-ENGAGEMENT.md) (what's forbidden).
> This file is **how the machine is built**.

---

## 1. The design decision, and why

The brief asked for "a GTM engineering team of experts" — an org chart of agents.
**We didn't build that, and the evidence is why.**

We surveyed four GTM-agent projects. Star count was **inversely correlated with
substance**:

| Project | ★ | What it actually is |
|---|---|---|
| `gtmagents/gtm-agents` | **334** | 723 of 751 files are markdown. Median "agent" = **1.2 KB**. All 17 Python files are repo linters. **No runtime at all.** Claims 92 agents; tree has 204 |
| `shawnla90/gtm-coding-agent` | 106 | A course. Good idea (`gtm-os/` filesystem-as-context-schema). No orchestration |
| `@SuperCorks/gtm-manager` | 0 | **Google Tag Manager.** Different "GTM" entirely |
| `cgallic/kai-cmo-harness` | **24** | **The only real harness.** 169 commits, typed contracts, blocking gates, a mandate ledger |

**The 334★ repo has 92 personas and no runtime. The 24★ repo has ~7 task types
and actually works.**

The lesson generalizes: **a persona is a job title; a task type is something the
runtime can execute and measure.** You cannot measure "CMO Agent." You can
measure `audit-property`. So:

> **The unit of work is a verb the harness can run, not a role it can roleplay.**

Three independent projects converged on the same law, and it is the most
important sentence in this document:

| kai-cmo-harness | `missing_sources_action: block_and_request_sources` · "No brief, no write" · data-gap ≠ zero |
| gtm-os | "Never fabricate data. If a file is empty, say so." |
| gtm-agents | *(no such rule — and no runtime to enforce one)* |

**The load-bearing parts are inputs and gates, not personas.**

---

## 2. Architecture

```
                     GOAL.md  ─────────────┐
                  (objectives O1–O6)       │
                                           ▼
   ┌──────────────────────── PLANNER ────────────────────────┐
   │  may ONLY emit verbs in the closed set below.           │
   │  no hallucinated capabilities. cycle-checked in code.   │
   └────────────────────────────┬────────────────────────────┘
                                ▼
   ┌─────────────────── CONTRACT (per verb) ─────────────────┐
   │  required_sources: [...]   ← loaded BEFORE generating   │
   │  missing_sources_action: block_and_request_sources      │
   │  output_schema: {...}      ← validated, not parsed      │
   │  risk_tier: low | med | HIGH                            │
   └────────────────────────────┬────────────────────────────┘
                                ▼
   ┌──────────────────────── GATES ──────────────────────────┐
   │  1. cite-gate    deterministic  ← §8 refusal gate       │
   │  2. rules-gate   deterministic  ← FORBIDDEN list        │
   │  3. voice-gate   LLM judge      ← does it sound like us │
   │  4. human        risk_tier: HIGH only                   │
   └────────────────────────────┬────────────────────────────┘
                                ▼
   ┌──────────────── CAPABILITY BOUNDARY ────────────────────┐
   │  MCP token WITHOUT outreach:send / leads:delete /       │
   │  workbooks:delete.                                      │
   │  Not a rule the agent follows — a power it lacks.       │
   └────────────────────────────┬────────────────────────────┘
                                ▼
                       EXECUTE → MEASURE → replan
                    (data gaps stay gaps, never zeros)
```

**Gates run cheap→expensive:** deterministic mechanics first, LLM judge second,
human last. Never burn a human on something a regex can reject.

---

## 3. The closed set of verbs

Seven. Each is something the harness can **run and measure**. The planner cannot
emit anything else — if a goal needs a verb that doesn't exist, that is a
**planning failure surfaced to a human**, not an improvisation.

| Verb | Does | Risk | Human? |
|---|---|---|---|
| `research` | ICP, competitors, channels, keywords, prior art | low | no |
| `audit` | Drive our own properties and report what's broken | low | no |
| `draft` | Docs, comparison pages, release notes, READMEs, issues | med | no (gate) |
| `measure` | Pull real numbers; diff vs. GOAL §2; **mark gaps as gaps** | low | no |
| `prepare` | Build a launch/listing artifact **up to the send button** | med | no |
| `publish` | Anything leaving the machine in our voice | **HIGH** | **YES** |
| `escalate` | Refuse + hand to a human with the reason | — | — |

**Note what is absent: there is no `outreach` verb.** Not because an agent might
misbehave, but because [GOAL §5](GOAL.md) says cold outreach is the wrong move
for all three products, and [RULES §7](RULES-OF-ENGAGEMENT.md) removes the
capability. **The architecture makes the strategy physically true.**

`publish` is the only verb that crosses the boundary, and it always stops for a
human. That is the whole safety design in one line.

---

## 4. Contracts

Every verb has a typed contract in [`contracts/`](contracts/). The pattern is
lifted from `kai-cmo-harness/harness/skill-contracts/`, which got it right:

```yaml
verb: draft
risk_tier: med
required_sources:
  - gtm/GOAL.md
  - gtm/RULES-OF-ENGAGEMENT.md
source_policy:
  load_before_writing: true
  claims_must_trace_to_evidence: true
  missing_sources_action: block_and_request_sources   # ← the primitive
output_schema:
  required: [body, sources, claims_ledger, risk_notes]
```

**`missing_sources_action: block_and_request_sources` is the anti-hallucination
primitive.** A missing input is a **hard block**, not a creative opportunity.
That single line is the difference between the harness that works and the 334★
one that doesn't.

---

## 5. Why the gates are real

**Prompts are not a control surface.** An agent that "knows" the rules violates
them fluently under pressure. Three layers that don't read English:

1. **The capability token** — the swarm's MCP token lacks `outreach:send`,
   `leads:delete`, `workbooks:delete`. `send_email` is documented in-tree as
   *"Admin only — HIGHEST RISK."*
2. **The caps ledger** — every write reserves against
   `trigger_cap_reservations` with `UniqueConstraint(workspace_id,
   idempotency_key)`. Can't exceed the daily budget; a retry is a detected
   replay, not a double-send.
3. **The audit log** — RLS-scoped, immutable. CASL and GDPR put the burden of
   proof on us.

### The failure mode this is actually defending against

Not malice. **Confident inference.** In building this harness, research agents
produced five legal errors, all in the same direction — *plausible gap-filling,
stated in the same register as verified fact.* None announced itself as a guess.
One blog surfaced a **fabricated case** (*"RetailScrape v. MegaMart"*) that an
agent optimizing for a confident answer would happily cite.

Two live examples from this session, both caught **only by checking the artifact
instead of the claim**:

- *"137k downloads"* — **true**, but that's asset downloads across 563 assets,
  not people. Right number, wrong implication.
- *"Ship audio watermarking before Aug 2 — highest priority across all three
  products."* — **OmniVoice shipped it months ago, default-on.** Two weeks would
  have gone to building something that already existed.

> **The rule that catches all of these: check the artifact, not the claim about
> the artifact.**

Which is exactly why `audit` is a first-class verb and why `research` never
terminates a decision on its own.

---

## 6. Running it

```bash
# one-shot verb
claude "/gtm research 'who is actually shipping Clay alternatives in 2026'"

# audit our own properties — the highest-value autonomous loop
claude "/gtm audit --all"

# what changed since GOAL.md was written?
scripts/gtm-verify.sh
```

The skill lives at `.claude/skills/gtm/SKILL.md`; contracts in
`gtm/contracts/`. `SKILL.md` follows the house style already used in
`OmniVoice-Studio/.claude/skills/omnivoice/` — frontmatter with rich triggers,
a Task Index table, and `scripts/`.

---

## 7. What the swarm is not for

From [GOAL §7](GOAL.md), restated because it's the thing most likely to be
forgotten:

**The swarm does:** research · draft · audit · measure · prepare.
**A human does:** anything that speaks to a stranger in the founder's voice.

The channels that actually work for a solo OSS founder — a Show HN that has
never happened, the right subreddit, **median 8.5h maintainer response latency →
71% higher odds a newcomer resolves an issue**, genuine standing in a community
— are precisely the ones a swarm cannot do for you.

**Automating them is how you lose them.** The swarm exists to buy back the hours
so a human can spend them where they compound.
