---
title: Spend transparency
description: The pre-run estimate, the spend ceiling, the per-provider cost ledger, and the optional billing switch.
sidebar:
  order: 4
---

OpenGTM shows you the bill before a run and keeps a ledger of what every
provider actually cost. None of this is a premium tier.

## The estimate

`GET /api/workbooks/{id}/run/estimate` counts rows, resolves the provider
chain for every enrichment column, and returns:

```json
{
  "rows": 50,
  "worst_usd": 6.00,
  "best_usd": 1.00,
  "breakdown": [
    { "column": "Email", "paid_providers": ["hunter_io", "apollo_io", "snovio", "prospeo"],
      "worst_usd": 6.00, "best_usd": 1.00 }
  ],
  "note": "worst = every paid provider tried per row; best = cheapest paid hit first. Cross-provider cache + confidence early-exit reduce actual spend."
}
```

Worst case multiplies rows by the sum of every paid provider in the chain;
best case uses the cheapest paid provider only. Free providers are not
priced. The UI gates the run behind a confirmation using these numbers.

## Provider prices

The vendor catalog carries a list price per lookup for each paid provider
(for example Hunter $0.04, Apollo $0.03, Snov.io $0.03, Prospeo $0.02,
People Data Labs $0.03, Abstract $0.01, DeBounce $0.008, NumVerify $0.005,
Google Maps $0.005, LeadMagic $0.05, Prospeo mobile $0.10). A provider's own
declared `cost_per_lookup`, including those in YAML manifests, overrides the
table. Research columns are priced at $0.05 per cell only when
`RESEARCH_VENDOR_COST=1`, so enabling it never retroactively changes an
existing estimate.

Search-style operations scale by page count and bulk operations by record
count.

## The ceiling

`PUT /api/workbooks/{id}/budget` with `{"max_usd": 5}` sets a spend ceiling
(0 means unlimited). `GET .../cost` returns the ceiling, the amount spent, and
the remainder. Once the remainder is smaller than a provider's price, the
planner drops that provider from every chain. Agent columns additionally
respect their own `policy.max_cost_usd` and never exceed the workbook's
headroom.

## The ledger

Every attempt records cost, latency, hit or miss, and confidence per provider
and field. Read it at `GET /api/workbooks/meta/provider-stats` and
`.../provider-accuracy`, or per cell through the trace endpoint. The same data
drives the [planner](/concepts/waterfalls/#the-planner).

## Billing is optional

`BILLING_ENABLED` is off by default. Off means runs are never blocked and no
debits happen, which is the right mode for a self-hosted install. On, a
workspace credit ledger, an idempotent debit, a `402` gate and a Stripe
top-up webhook exist under `/api/billing`. Platform-metered costs such as
outreach sends and poller runs default to $0.00.
