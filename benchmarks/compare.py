#!/usr/bin/env python3
"""Compare a candidate benchmark result against a baseline.

    python benchmarks/compare.py [--threshold 0.15] [BASELINE] CANDIDATE

Exit status: 0 no regression, 1 at least one gated metric regressed by more
than the threshold, 2 usage or input error.

A regression is a worse value by more than ``threshold`` (a fraction, default
0.15 = 15%): throughput lower, latency higher. Only *gated* metrics fail the
comparison (throughput and p50/p95 latencies); p99 and component latencies are
informational unless ``--all`` is given, because tail values on a shared
machine are too noisy to fail a build on.

Metrics present in only one file are reported but do not fail the run unless
``--strict`` is set. Use ``--only GLOB`` (repeatable) to restrict the check,
for example ``--only 'queue/nosync/go-queue/*'`` after a change that only
touches the Go queue. The comparison is only meaningful for results produced
on the same machine with the same configuration; a mismatch is warned about.
"""

from __future__ import annotations

import argparse
import fnmatch
import json
import os
import sys

DEFAULT_BASELINE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "results", "baseline.json")


def load(path: str) -> dict:
    try:
        with open(path) as f:
            data = json.load(f)
    except (OSError, json.JSONDecodeError) as e:
        print(f"error: cannot read {path}: {e}", file=sys.stderr)
        sys.exit(2)
    if "metrics" not in data:
        print(f"error: {path} has no 'metrics' (not a benchmark result?)", file=sys.stderr)
        sys.exit(2)
    return data


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("files", nargs="+", metavar="[BASELINE] CANDIDATE",
                    help="candidate result, optionally preceded by the baseline (default: results/baseline.json)")
    ap.add_argument("--threshold", type=float, default=float(os.environ.get("BENCH_REGRESSION_THRESHOLD", 0.15)),
                    help="allowed relative regression, default 0.15 (env: BENCH_REGRESSION_THRESHOLD)")
    ap.add_argument("--all", action="store_true", help="also gate informational metrics (p99, claim latency)")
    ap.add_argument("--strict", action="store_true", help="fail when a baseline metric is missing in the candidate")
    ap.add_argument("--only", action="append", default=[], metavar="GLOB", help="restrict to matching metric keys")
    ap.add_argument("--verbose", action="store_true", help="print every compared metric, not just regressions")
    args = ap.parse_args()

    if len(args.files) == 1:
        base_path, cand_path = DEFAULT_BASELINE, args.files[0]
    elif len(args.files) == 2:
        base_path, cand_path = args.files
    else:
        ap.error("pass CANDIDATE or BASELINE CANDIDATE")
    if args.threshold < 0:
        ap.error("--threshold must be >= 0")
    base, cand = load(base_path), load(cand_path)

    for key in ("cpu", "logical_cores"):
        if base["machine"].get(key) != cand["machine"].get(key):
            print(f"warning: {key} differs (baseline {base['machine'].get(key)!r}, candidate "
                  f"{cand['machine'].get(key)!r}); results are not comparable across machines", file=sys.stderr)

    regressions, improvements, missing, compared = [], [], [], 0
    for key, b in sorted(base["metrics"].items()):
        if args.only and not any(fnmatch.fnmatch(key, g) for g in args.only):
            continue
        if not (b["gated"] or args.all):
            continue
        c = cand["metrics"].get(key)
        if c is None:
            missing.append(key)
            continue
        if b["value"] <= 0:
            continue
        compared += 1
        # Positive change = worse.
        change = (b["value"] - c["value"]) / b["value"] if b["better"] == "higher" else (c["value"] - b["value"]) / b["value"]
        line = (f"{key}: {b['value']:.4g} -> {c['value']:.4g} {b['unit']} "
                f"({'worse' if change > 0 else 'better'} by {abs(change) * 100:.1f}%)")
        if change > args.threshold:
            regressions.append(line)
        elif change < -args.threshold:
            improvements.append(line)
        elif args.verbose:
            print("  ok  " + line)

    for line in improvements:
        print(" FAST " + line)
    for key in missing:
        print(f" MISS  {key}: not in candidate")
    for line in regressions:
        print(f" SLOW  {line}")
    print(f"\ncompared {compared} metrics at a {args.threshold * 100:.0f}% threshold: "
          f"{len(regressions)} regressed, {len(improvements)} improved, {len(missing)} missing")
    if regressions or (args.strict and missing):
        return 1
    if compared == 0:
        print("error: no metrics were compared (wrong --only filter or mismatched configuration?)", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
