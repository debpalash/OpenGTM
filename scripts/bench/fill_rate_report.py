"""Report for scripts/bench/first-run-fill-rate.sh: reads the cells the run left in workbook_enrichments and prints
the per-column table and three separate fill rates.

  all_filled                 every cell with a value.
  excluding_guessed_emails   all_filled minus the email cells that are a guess, not a find: the value came from
                             ddg_email and its local part is a role name (info@, contact@, ...). That provider's
                             generic fallback is confidence 0.2, "generic" (internal/providers/scrape/email.go), and
                             the cell table does not store confidence, so the rule is on provider and local part.
                             An info@ found on a page by another provider is a find and is kept.
  excluding_key_columns      cells of columns that need a key by design (AI research / ai_formula / agent columns,
                             which call an LLM) removed from numerator and denominator.

They are three numbers and are never folded into one. Email Verify cells holding the verdict "unknown" are
counted in all three (the cascade did answer) and also reported on their own.

Inputs in the work directory: runN_cells.psv (column|lead|value|provider|status|error|meta), columns.json
(workbooks.columns_config), stats.log, provider_stats.psv. Run as a script with the arguments of main().
"""
import collections
import json
import os
import re
import sys

KEY_COLUMN_TYPES = {"research", "ai_formula", "agent"}
ROLE_LOCAL_PARTS = {
    "info", "contact", "hello", "hi", "support", "sales", "admin", "office", "mail", "team", "enquiries",
    "inquiries", "help", "service", "marketing", "press", "media",
}
GUESSING_PROVIDERS = {"ddg_email"}


def rate(filled, total):
    return round(filled / total, 4) if total else None


def load_cells(path):
    cells = []
    with open(path) as f:
        for line in f:
            p = line.rstrip("\n").split("|", 6)
            if len(p) >= 6:
                p += [""] * (7 - len(p))
                cells.append(p)
    return cells


def is_guessed_email(col_target, value, provider):
    if col_target != "email" or not value or provider not in GUESSING_PROVIDERS:
        return False
    return value.split("@", 1)[0].lower() in ROLE_LOCAL_PARTS


def build_report(out, commit, rows, lf_filled, status, up_s, up_to_done_s, run_s, build_s, reruns):
    columns = []
    try:
        with open(os.path.join(out, "columns.json")) as f:
            columns = json.load(f)
    except (OSError, ValueError):
        pass
    by_id = {c.get("id"): c for c in columns}
    cells = load_cells(os.path.join(out, "run1_cells.psv"))

    def target(col_id):
        c = by_id.get(col_id, {})
        return c.get("target_field") or c.get("lead_field") or col_id

    key_cols = sorted(cid for cid, c in by_id.items() if c.get("type") in KEY_COLUMN_TYPES)
    per = collections.OrderedDict()
    guessed = unknown = 0
    for col, _lead, val, prov, st, err, meta in cells:
        c = per.setdefault(col, {"total": 0, "filled": 0, "providers": collections.Counter(),
                                 "empty_reasons": collections.Counter()})
        c["total"] += 1
        if val:
            c["filled"] += 1
            c["providers"][prov or "?"] += 1
            if is_guessed_email(target(col), val, prov):
                guessed += 1
            if target(col) == "email_verify" and val == "unknown":
                unknown += 1
        else:
            reason = err or st or "unknown"
            if "cooldown" in meta:
                reason += " (provider cooldown skip)"
            c["empty_reasons"][reason] += 1
    total = sum(c["total"] for c in per.values())
    filled = sum(c["filled"] for c in per.values())
    key_total = sum(per[c]["total"] for c in key_cols if c in per)
    key_filled = sum(per[c]["filled"] for c in key_cols if c in per)

    peak, mx, cur = 0.0, collections.defaultdict(float), 0.0
    try:
        with open(os.path.join(out, "stats.log")) as f:
            for line in f:
                if line.startswith("---"):
                    peak, cur = max(peak, cur), 0.0
                    continue
                p = line.split()
                if len(p) >= 2:
                    m = to_mib(p[1])
                    mx[p[0]] = max(mx[p[0]], m)
                    cur += m
    except OSError:
        pass
    pstats = []
    try:
        with open(os.path.join(out, "provider_stats.psv")) as f:
            for line in f:
                p = line.rstrip("\n").split("|")
                if len(p) >= 4:
                    pstats.append({"provider": p[0], "field": p[1], "attempts": int(p[2]), "hits": int(p[3])})
    except OSError:
        pass
    return {
        "commit": commit, "rows": rows, "run_status": status,
        "all_filled": {"filled": filled, "total": total, "rate": rate(filled, total)},
        "excluding_guessed_emails": {"filled": filled - guessed, "total": total, "guessed_emails": guessed,
                                     "rate": rate(filled - guessed, total)},
        "excluding_key_columns": {"filled": filled - key_filled, "total": total - key_total,
                                  "excluded_columns": key_cols, "rate": rate(filled - key_filled, total - key_total)},
        "verify_verdict_unknown": unknown,
        "lead_field_cells_filled": lf_filled,
        "per_column": {k: {"filled": v["filled"], "total": v["total"], "rate": rate(v["filled"], v["total"]),
                           "needs_key_by_design": k in key_cols,
                           "filled_by_provider": dict(v["providers"]), "empty_reasons": dict(v["empty_reasons"])}
                       for k, v in per.items()},
        "seconds": {"build": int(build_s), "compose_up": int(up_s), "up_to_run_complete": int(up_to_done_s),
                    "run_1": int(run_s)},
        "memory_mib": {"peak_sum_sampled": round(peak), "peak_per_container": {k: round(v) for k, v in mx.items()},
                       "samples": "docker stats --no-stream every 10 s"},
        "provider_stats_cumulative": pstats,
        "reruns": reruns,
    }


def to_mib(s):
    n, u = re.match(r"([\d.]+)(\w+)", s).groups()
    return float(n) * {"KiB": 1 / 1024, "MiB": 1, "GiB": 1024, "B": 1 / 1048576}[u]


def pct(x):
    d = x["rate"]
    return "not measured" if d is None else f"{d * 100:.1f}%"


def render_text(rep):
    lines = [f"First-run fill rate, commit {rep['commit'][:8]}, {rep['rows']} rows, no paid keys, no LLM key, "
             f"run status {rep['run_status']}",
             f"{'column':<14}{'filled':>8}{'total':>7}{'rate':>7}  filled by / empty because"]
    for k, v in rep["per_column"].items():
        fb = ", ".join(f"{p} {n}" for p, n in sorted(v["filled_by_provider"].items(), key=lambda x: -x[1]))
        eb = ", ".join(f"{r} x{n}" for r, n in sorted(v["empty_reasons"].items(), key=lambda x: -x[1]))
        key = " [needs a key by design]" if v["needs_key_by_design"] else ""
        lines.append(f"{k:<14}{v['filled']:>8}{v['total']:>7}{(v['rate'] or 0) * 100:>6.0f}%  "
                     f"{fb}{' | EMPTY: ' + eb if eb else ''}{key}")
    a, g, k = rep["all_filled"], rep["excluding_guessed_emails"], rep["excluding_key_columns"]
    lines += [
        f"ALL FILLED: {a['filled']}/{a['total']} = {pct(a)}",
        f"EXCLUDING GUESSED EMAILS: {g['filled']}/{g['total']} = {pct(g)} ({g['guessed_emails']} role-name guesses from ddg_email removed)",
        f"EXCLUDING KEY-REQUIRING COLUMNS: {k['filled']}/{k['total']} = {pct(k)} (columns left out: {', '.join(k['excluded_columns']) or 'none'})",
        f"Email Verify cells whose verdict is 'unknown' (counted above, the cascade could not decide): {rep['verify_verdict_unknown']}",
    ]
    s = rep["seconds"]
    lines.append(f"time: compose up {s['compose_up']} s, up to run complete {s['up_to_run_complete']} s, run 1 {s['run_1']} s "
                 f"(image build {s['build']} s, separate)")
    lines.append(f"memory: peak sum of sampled containers {rep['memory_mib']['peak_sum_sampled']} MiB; "
                 f"per container {rep['memory_mib']['peak_per_container']}")
    for r in rep["reruns"]:
        lines.append(f"re-run {r['run']}: {r['filled']}/{r['total']} filled, lost {r['lost_vs_previous']}, "
                     f"gained {r['gained_vs_previous']} vs previous run, {r['seconds']} s")
    return "\n".join(lines)


def main(argv):
    out, commit, rows, lf_filled, status, up_s, up_to_done_s, run_s, build_s, reruns = argv[1:11]
    rep = build_report(out, commit, int(rows), int(lf_filled), status, up_s, up_to_done_s, run_s, build_s,
                       json.loads(reruns))
    print(render_text(rep))
    print("JSON:")
    print(json.dumps(rep, indent=2))
    with open(os.path.join(out, "report.json"), "w") as f:
        json.dump(rep, f, indent=2)


if __name__ == "__main__":
    main(sys.argv)
