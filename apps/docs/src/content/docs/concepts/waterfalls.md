---
title: Waterfalls and the planner
description: How a column tries providers in sequence, why the order is learned, and how caching and budgets shorten the chain.
sidebar:
  order: 3
---

A `waterfall` column tries providers one after another until one returns a
confident value. The point of the design is to spend nothing when a free source
answers, and to spend the cheapest paid dollar first when it does not.

## Default chains

Every target field has a default chain, free and open-source providers first,
paid APIs as fallback. For `email`:

```
deep_scraper → jsonld_firmographics → website_scraper → email_harvester
→ ddg_email → mailscout → hunter_io → apollo_io → snovio → prospeo
```

For `email_verify`: `mailscout → holehe → abstract_api → debounce`. For
`mobile_phone`, which has no free tier: `leadmagic_mobile → prospeo_mobile`,
cost-ordered. Chains exist for phone, description, decision makers, contact
person and title, social URLs, company size, industry tags, address, founding
year, funding, news, technologies, hiring signals and score.

When a column lists an explicit `waterfall`, the free defaults that are not
already present are prepended, and then the whole chain is reordered by the
planner.

## The planner

The order at run time is learned, not static. For each provider and field the
planner keeps attempts, hits, confidence, latency and cost in a ledger
(`GET /api/workbooks/meta/provider-stats`) and scores each provider as

```
score = hit_rate / (cost + 0.001) × correctness_prior
```

Unseen providers get an optimistic prior until they have three attempts. The
correctness prior comes from the golden-dataset accuracy evaluation and can
lower a provider's score by up to 60%; it has no effect until a provider has
been evaluated.

Before sorting, the planner drops providers that are cooling down (five
minutes after a rate limit, three minutes after a hard timeout) and paid
providers whose cost exceeds the workbook's remaining budget.

## Stopping early

- A hit with confidence above the threshold ends the chain.
- Results are cached across rows and columns, so a second column asking a
  provider about the same company does not pay twice.
- `max_providers` caps chain depth (0 means unlimited).
- A spend ceiling on the workbook removes paid providers from every chain once
  it is exhausted; free providers keep running.

## Email verification cascade

Verification is a separate cascade that normalises every verifier to
`valid`, `invalid`, `catch_all` or `unknown`. The first three are definitive
and stop the chain; `unknown` cascades to the next verifier. Confidence is
0.95 for valid, 0.9 for invalid, 0.5 for catch-all. The built-in SMTP probe
runs first because it is free; a verifier with no key is skipped. The optional
self-hosted [Reacher](/guides/providers/#reacher-self-hosted-verification)
service becomes the strongest tier when enabled.
