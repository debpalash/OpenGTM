"""
CSV Import Scraper — Imports existing CSV files into the lead database.

Handles column mapping and normalization from the various CSV formats
present in the repo (leads.csv, leads2.csv, leads_final.csv).
"""

import pandas as pd
from pathlib import Path
from typing import List

from leadgen.models import Lead


def _clean(val) -> str:
    """Normalize a cell value to clean string."""
    if pd.isna(val):
        return ""
    s = str(val).strip()
    if s.lower() in ("n/a", "nan", "none", ""):
        return ""
    return s


def import_csv(filepath: str, source_tag: str = "csv_import") -> List[Lead]:
    """
    Import leads from a CSV file.

    Supports the following column sets:
    - leads.csv:       Company, Website, Email, Phone, City, Notes
    - leads2.csv:      Company, Website, City, Specialization
    - leads_final.csv: All of the above + Yupcha Value Prop, Company Need

    Returns a list of Lead objects ready for database upsert.
    """
    filepath = Path(filepath)
    if not filepath.exists():
        print(f"  ⚠ File not found: {filepath}")
        return []

    df = pd.read_csv(filepath)
    print(f"  📄 Reading {filepath.name}: {len(df)} rows, columns: {list(df.columns)}")

    # Normalize column names to lowercase
    df.columns = [c.strip().lower().replace(" ", "_") for c in df.columns]

    leads = []
    for _, row in df.iterrows():
        lead = Lead(
            company=_clean(row.get("company", "")),
            website=_clean(row.get("website", "")),
            email=_clean(row.get("email", "")),
            phone=_clean(row.get("phone", "")),
            city=_clean(row.get("city", "")),
            specialization=_clean(row.get("specialization", "")),
            notes=_clean(row.get("notes", "")),
            yupcha_value_prop=_clean(row.get("yupcha_value_prop", "")),
            company_need=_clean(row.get("company_need", "")),
            source=source_tag,
        )

        # Skip empty rows
        if not lead.company:
            continue

        leads.append(lead)

    print(f"  ✅ Parsed {len(leads)} leads from {filepath.name}")
    return leads


def import_all_csvs(data_dir: str = ".") -> List[Lead]:
    """Import all known CSV files from the data directory."""
    data_dir = Path(data_dir)
    all_leads = []

    # Priority order: leads_final has the most data, then leads2, then leads
    csv_files = ["leads_final.csv", "leads2.csv", "leads.csv"]

    for fname in csv_files:
        fpath = data_dir / fname
        if fpath.exists():
            leads = import_csv(str(fpath), source_tag="csv_import")
            all_leads.extend(leads)

    print(f"\n📊 Total leads imported from CSVs: {len(all_leads)}")
    return all_leads
