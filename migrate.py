#!/usr/bin/env python3
"""Quick migration script to import existing CSVs into the lead database."""
import sys, os
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from leadgen.scrapers.csv_import import import_all_csvs
from leadgen.db import LeadDB
from leadgen.scoring import score_and_update_db

if __name__ == "__main__":
    print("🚀 Migrating existing CSV data into lead database...\n")
    leads = import_all_csvs(".")
    db = LeadDB()
    count = db.bulk_upsert(leads)
    print(f"\n💾 Imported {count} leads")
    score_and_update_db(db)
    from leadgen.export import print_stats
    print_stats(db)
    db.close()
    print("\n✅ Migration complete! Run 'python cli.py dashboard' to browse.")
