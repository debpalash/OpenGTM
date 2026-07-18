---
name: gtm
description: "Go-to-market engineering for the Yupcha portfolio (OmniVoice-Studio, the Clay-alternative in this repo, ResuBird). A closed set of executable verbs — research, audit, draft, measure, prepare, publish, escalate — each with a typed contract, required sources, and a risk tier. Use when: (1) researching ICP/competitors/channels/prior art, (2) auditing our own properties for what's actually broken (dead pricing pages, 404ing clone URLs, unfilled demos, licence detection, watermark coverage), (3) drafting docs/comparison pages/release notes/launch copy, (4) measuring real numbers against GOAL.md, (5) preparing a Show HN or a listing up to the send button, (6) any question about whether a GTM action is allowed. Triggers: 'gtm', 'go to market', 'launch', 'show hn', 'growth', 'marketing', 'positioning', 'icp', 'competitors', 'outreach', 'cold email', 'audit our', 'is it ok to', 'can we post', 'promote'."
---

# GTM

## Overview

The GTM harness for this portfolio. **Read [`gtm/GOAL.md`](../../../gtm/GOAL.md)
and [`gtm/RULES-OF-ENGAGEMENT.md`](../../../gtm/RULES-OF-ENGAGEMENT.md) before
acting — they are `required_sources` for every verb, not background reading.**
[`gtm/SWARM.md`](../../../gtm/SWARM.md) explains why it's shaped this way.

## The one thing to know first

**The bottleneck is not that we lack customers. It's that three doors are
closed.**

| Product | Wedge | Distribution | Monetization |
|---|---|---|---|
| OmniVoice-Studio | strong | **8,509★ · 137k downloads** | **none — nothing to buy** |
| lead-data (Clay alt) | **excellent** | **zero — repo is private** | none |
| ResuBird | decent | live | **`/pricing` 404s, PRO is ₹0** |

The product with world-class distribution has no way to pay us; the product with
a world-class wedge cannot be seen. **Open doors. Don't knock on others'.**

## Task Index — pick the right verb

| Task | Verb | Contract | Human? |
|---|---|---|---|
| Find out what's true | `research` | [research.yaml](../../../gtm/contracts/research.yaml) | no |
| Check our own stuff by *running* it | `audit` | [audit.yaml](../../../gtm/contracts/audit.yaml) | no |
| Write something for review | `draft` | [draft.yaml](../../../gtm/contracts/draft.yaml) | no (gates) |
| Pull real numbers, diff vs GOAL §2 | `measure` | [measure.yaml](../../../gtm/contracts/measure.yaml) | no |
| Build a launch artifact, stop at send | `prepare` | [prepare.yaml](../../../gtm/contracts/prepare.yaml) | no |
| Anything leaving in the founder's voice | `publish` | [publish.yaml](../../../gtm/contracts/publish.yaml) | **YES** |
| Refuse and hand up, with the reason | `escalate` | — | — |

**There is no `outreach` verb.** Not an oversight — see GOAL §5 and RULES §7.
The swarm's MCP token is minted without `outreach:send`, so it is a capability
this harness does not possess, not a rule it chooses to obey.

## Usage

```bash
scripts/gtm-verify.sh            # re-check every number in GOAL.md §2
```

```
/gtm audit --all                 # highest-value autonomous loop
/gtm research "who ships Clay alternatives in 2026"
/gtm measure omnivoice
```

## The three rules that carry the weight

**1. No primary-source URL, no action.** A vendor blog, an SEO article, or your
own recollection is not a source. If you can't produce one, refuse and escalate.

⚠️ **You cannot verify a statistic with AI search** — search summaries
reproduced a known misattribution *live, twice*, during this project's research.
They confirm errors rather than catch them.

**2. Check the artifact, not the claim about the artifact.** Every real finding
in this project came from running the thing:

- The README's own clone URL **404s anonymously** — every user who followed our
  quickstart failed on line one.
- `resubird.com/pricing` returns **"Page Not Found"** while linked from nav *and*
  footer.
- The first-run demo filled **0 of 175 cells** — one missing dict key.
- A research agent's #1 priority was "ship audio watermarking." **It shipped
  months ago, default-on.** Two weeks nearly went to building a thing that
  existed.

**3. Data gaps are not zeros.** A KPI with no measured value is a **DATA GAP** —
leave it untouched, never write `0`. A zeroed metric is indistinguishable from
failure and corrupts every decision downstream, including the planner's.

## Hard stops

Full list in RULES §1. The ones you'll actually hit:

- **Never call the Clay alternative "Yupcha."** `yupcha.com` is a live AI hiring
  platform. The name is spent. Use a placeholder and escalate.
- **Never market OmniVoice as an outbound/cold-calling tool**, and **never demo
  with a famous voice.** Our legal posture (general-purpose, local, offline,
  open-source) is strong, and **our own marketing copy is the only thing that
  can destroy it.**
- **Never email stargazers.** 8,509 supporters → 8,509 detractors, in one send.
- **Never solicit upvotes** anywhere, including off-platform. Bannable, and the
  HN trigger is *pattern-based* — one founder lost posting privileges for
  "repeatedly posting from a single site," with no vote manipulation at all.
- **Never claim a metric we haven't measured or a testimonial we didn't
  receive.** *FTC v. TruHeight*: unsubstantiated efficacy claims + seeded
  reviews = $4M judgment, suspended to $750k. ResuBird's `4.8/1250` vs `4.8/12`
  markup is that thread.
- **Never publish autonomously.** Ever.

## Don't bother

Evidence in GOAL §5. `llms.txt` (97% of 137K domains got **zero requests**;
zero AI bots ever probed for one) · schema markup for AI citation (causally
tested, effect zero) · GEO tactics (fail replication; the top methods literally
instruct fabrication) · chasing stars (HTTPie lost **54,000 stars** overnight
with zero revenue impact) · Product Hunt (91.2% of 500 launches got <100 users) ·
MCP directory listings as acquisition.

## What this harness is for

**It does:** research · audit · draft · measure · prepare.
**A human does:** anything that speaks to a stranger in the founder's voice.

The channels that work for a solo OSS founder — the Show HN that has never
happened, the right subreddit, **median 8.5h response latency → 71% higher odds
a newcomer resolves an issue** — are precisely the ones a swarm can't do for
you. Automating them is how you lose them. **This exists to buy back the hours
so a human can spend them where they compound.**
