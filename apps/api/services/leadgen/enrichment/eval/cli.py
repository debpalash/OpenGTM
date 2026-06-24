"""CLI for the provider accuracy/freshness eval harness.

Run the eval over the seeded golden dataset and print the public per-provider
accuracy ranking. Optionally persist the correctness prior to the DB so it feeds
the planner's chain ordering.

  PYTHONPATH=. uv run python -m apps.api.services.leadgen.enrichment.eval.cli
  PYTHONPATH=. uv run python -m apps.api.services.leadgen.enrichment.eval.cli --persist
"""

from __future__ import annotations

import argparse
import sys

from apps.api.services.leadgen.enrichment.eval.scorer import run_eval


def _print_table(ranking) -> None:
    if not ranking:
        print("No providers scored (empty golden/recorded set).")
        return
    print(f"{'rank':<5}{'provider':<24}{'score':>8}{'accuracy':>10}{'coverage':>10}{'fresh':>8}{'n':>6}")
    print("-" * 71)
    for i, s in enumerate(ranking, 1):
        print(
            f"{i:<5}{s.provider:<24}{s.score:>8.3f}{s.accuracy:>10.3f}"
            f"{s.coverage:>10.3f}{s.freshness:>8.3f}{s.asserted:>6}"
        )


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="Provider accuracy/freshness eval ranking")
    ap.add_argument("--persist", action="store_true",
                    help="Write correctness prior to ProviderStat (feeds the planner)")
    ap.add_argument("--json", action="store_true", help="Emit JSON instead of a table")
    args = ap.parse_args(argv)

    if args.persist:
        from apps.api.database import SessionLocal
        from apps.api.services.leadgen.enrichment.eval.persist import run_and_persist
        db = SessionLocal()
        try:
            ranking = run_and_persist(db)
            db.commit()
        finally:
            db.close()
        print(f"Persisted correctness prior for {len(ranking)} providers.\n")
    else:
        ranking = run_eval()

    if args.json:
        import json
        print(json.dumps([s.to_api() for s in ranking], indent=2))
    else:
        _print_table(ranking)
    return 0


if __name__ == "__main__":
    sys.exit(main())
