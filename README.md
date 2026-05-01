# Yupcha Lead Generation Pipeline

A self-hosted, automated pipeline for sourcing, enriching, scoring, and managing B2B sales leads for Yupcha.

## Quick Start

```bash
# Setup
python3 -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt

# Import existing CSV data
python migrate.py

# Launch dashboard
python cli.py dashboard
# → http://127.0.0.1:5050
```

## Pipeline Commands

```bash
# Run full pipeline (scrape all sources → enrich → score)
python cli.py pipeline

# Scrape specific source
python cli.py scrape --source job_boards
python cli.py scrape --source review_dirs
python cli.py scrape --source google_maps --city Bangalore

# Enrich leads (find missing phone/email/LinkedIn)
python cli.py enrich
python cli.py enrich --limit 20

# Re-score all leads
python cli.py score

# Export filtered leads
python cli.py export --format csv --min-score 50
python cli.py export --tier hot --format json

# View stats
python cli.py stats
```

## Lead Sources (9 active sources)

| Source | Type | What it finds |
|--------|------|--------------|
| CSV Import | Existing data | Your current 128 leads |
| Google Maps | Local search | Companies by city + category |
| Naukri | Job board | Companies actively hiring |
| Indeed | Job board | Employer profiles |
| Foundit | Job board | Active recruiters |
| Clutch | B2B reviews | Verified, reviewed companies |
| GoodFirms | B2B reviews | Rated companies |
| AmbitionBox | Reviews | Employee-reviewed companies |
| LinkedIn | Professional | Company profiles + decision makers |
| Web Directories | JustDial/Sulekha | Local business listings |
| Google Search | General web | Smaller/local agencies |
| News | Mentions | Growing/fundraising companies |

## Lead Scoring (0-100)

Leads are scored based on ICP fit:
- **Hot (75-100)**: Ready to contact — has contact info + ICP match
- **Warm (50-74)**: Good fit, needs more data
- **Cold (25-49)**: Partial data, lower priority
- **Unqualified (<25)**: Missing critical info

## Dashboard

Local web UI at `http://127.0.0.1:5050` with:
- Stats cards & charts
- Searchable, filterable lead table
- Status workflow tracking
- CSV export
- Manual lead entry

## Project Structure

```
leadgen/                    # Core pipeline
├── models.py               # Lead data model
├── db.py                   # SQLite database manager
├── scoring.py              # Lead scoring engine
├── pipeline.py             # Orchestrator
├── export.py               # CSV/JSON export
├── scrapers/               # Lead sources
│   ├── csv_import.py       # Existing CSV import
│   ├── google_maps.py      # Google Maps
│   ├── job_boards.py       # Naukri/Indeed/Foundit
│   ├── review_directories.py # Clutch/GoodFirms/G2
│   ├── web_directories.py  # JustDial/Sulekha
│   ├── linkedin.py         # LinkedIn discovery
│   └── google_search.py    # Web search + news
└── enrichment/             # Data enrichment
    ├── website_scraper.py  # Company website extraction
    ├── email_finder.py     # Email discovery
    ├── search_enricher.py  # DuckDuckGo fallback
    └── social_finder.py    # LinkedIn/Twitter finder
dashboard/                  # Web UI
├── app.py                  # Flask backend
├── templates/index.html    # Dashboard page
└── static/                 # CSS + JS
```
