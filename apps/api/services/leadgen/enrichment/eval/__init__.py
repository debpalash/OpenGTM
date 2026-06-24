"""Provider accuracy / freshness eval harness.

The enrichment planner orders waterfall chains by HIT-RATE, which rewards a
provider that returns confident-but-WRONG data. This package adds the missing
CORRECTNESS signal:

  - golden datasets (data/provider_eval/golden.json): known-good ground truth
  - scorer.py: score recorded provider outputs vs golden → per-provider accuracy
    + freshness, aggregated to a single per-provider correctness score
  - the score is persisted onto ProviderStat and fed into planner ordering as a
    multiplicative correctness PRIOR (combined with hit-rate, not replacing it)

Everything in the unit path is offline/deterministic: the scorer reads RECORDED
provider outputs, never the network.
"""

from apps.api.services.leadgen.enrichment.eval.scorer import (
    ProviderScore,
    FieldVerdict,
    score_outputs,
    load_golden,
    load_recorded_outputs,
    rank_providers,
)

__all__ = [
    "ProviderScore",
    "FieldVerdict",
    "score_outputs",
    "load_golden",
    "load_recorded_outputs",
    "rank_providers",
]
