"""Move the single-host metadata stores into PostgreSQL, and prove it.

    uv run python -m apps.api.scripts.multihost_backfill plan   [--data-dir DIR]
    uv run python -m apps.api.scripts.multihost_backfill run    [--data-dir DIR] [--overwrite]
    uv run python -m apps.api.scripts.multihost_backfill verify [--data-dir DIR] [--check-decrypt]

``DATABASE_URL`` must be the PostgreSQL runtime role (NOSUPERUSER NOBYPASSRLS)
after ``alembic upgrade head``; the same ``SECRETS_MASTER_KEY`` /
``SECRETS_PROVIDER`` the application uses must be set, because credential
settings are enveloped on the way in and ``--check-decrypt`` proves the key.

``run`` is idempotent and resumable (re-run after any interruption) and never
modifies the source files. It verifies when done unless ``--no-verify``.
``verify`` exits 1 on any difference. Output is JSON on stdout; progress goes to
stderr; no secret value is ever printed. Operator procedure:
docs/plans/m8-multihost-state.md.
"""
from __future__ import annotations

import argparse
import json
import os
import sys
from dataclasses import asdict

from apps.api.core.config import settings
from apps.api.services.leadgen import ledger_backfill
from apps.api.services.workspace import backfill, pg_meta

STORES = backfill.ALL_STORES + ("ledger",)


def _stores(value: str):
    chosen = tuple(s.strip() for s in value.split(",") if s.strip())
    bad = [s for s in chosen if s not in STORES]
    if bad:
        raise argparse.ArgumentTypeError(f"unknown store(s) {bad}; choose from {STORES}")
    return chosen


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(prog="multihost_backfill", description=__doc__.split("\n\n")[0])
    parser.add_argument("command", choices=["plan", "run", "verify"])
    parser.add_argument("--data-dir", default=settings.DATA_DIR, help="directory holding workspaces.db and data.db")
    parser.add_argument("--stores", type=_stores, default=STORES,
                        help="comma list of: " + ",".join(STORES))
    parser.add_argument("--main-leads-db", default=None,
                        help="the main collection ledger file (default <data-dir>/leads.db; "
                             "<LEADGEN_DATA_DIR>/leads.db when that is set)")
    parser.add_argument("--overwrite", action="store_true",
                        help="refresh rows that already exist in PostgreSQL (only before cut-over)")
    parser.add_argument("--batch", type=int, default=500)
    parser.add_argument("--no-verify", action="store_true", help="with run: skip the verification pass")
    parser.add_argument("--check-decrypt", action="store_true",
                        help="also prove every stored envelope decrypts with the current key")
    args = parser.parse_args(argv)

    def log(msg):
        print(msg, file=sys.stderr)

    try:
        pg_meta._engine()  # fails clearly on SQLite
    except RuntimeError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2

    report: dict = {"data_dir": args.data_dir, "stores": list(args.stores)}
    ok = True
    main_db = args.main_leads_db
    if main_db is None and os.getenv("LEADGEN_DATA_DIR"):
        from apps.api.services.leadgen.config import DB_PATH
        main_db = str(DB_PATH)
    meta = tuple(s for s in args.stores if s in backfill.ALL_STORES)
    ledger = "ledger" in args.stores
    if args.command in ("plan", "run"):
        dry = args.command == "plan"
        results = {}
        if meta:
            results.update(backfill.backfill(args.data_dir, stores=meta, overwrite=args.overwrite,
                                             batch=args.batch, dry_run=dry, progress=log))
        if ledger:
            results.update(ledger_backfill.backfill_ledger(
                args.data_dir, main_leads_db=main_db, overwrite=args.overwrite,
                batch=args.batch, dry_run=dry, progress=log))
        report["backfill"] = {k: asdict(v) for k, v in results.items()}
    if args.command == "verify" or (args.command == "run" and not args.no_verify):
        checks = backfill.verify(args.data_dir, stores=meta) if meta else []
        if ledger:
            checks += ledger_backfill.verify_ledger(args.data_dir, main_leads_db=main_db)
        report["verify"] = backfill.summarize(checks)
        ok = report["verify"]["ok"]
    if args.check_decrypt:
        report["decrypt"] = backfill.check_decrypt()
        ok = ok and report["decrypt"]["all_decrypt"]
    report["ok"] = ok
    print(json.dumps(report, indent=2, sort_keys=True))
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
