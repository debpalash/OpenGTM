# Yupcha Lead Pipeline

Lead generation, enrichment & scoring pipeline.

## Setup

```bash
uv sync
```

## Usage

```bash
# Dashboard
uv run python cli.py dashboard

# Full pipeline (scrape → enrich → score)
uv run python cli.py pipeline

# Individual commands
uv run python cli.py scrape --source job_boards
uv run python cli.py enrich --limit 20
uv run python cli.py score
uv run python cli.py export --tier hot
uv run python cli.py stats

# Import existing CSVs
uv run python cli.py import
```

## Dashboard Dev

```bash
# Build React UI (only needed after frontend changes)
cd dashboard/web && npm install && npm run build

# Run
uv run python cli.py dashboard
# → http://127.0.0.1:5050
```

## Sources

Job Boards (Naukri, Indeed, Foundit) · Review Dirs (Clutch, GoodFirms, G2) ·
Google Maps · LinkedIn · Web Directories · Google Search · News

## Structure

```
cli.py              # CLI entry point
config.py           # ICP config
pyproject.toml      # uv deps
leadgen/            # Core pipeline (scrapers, enrichment, scoring, db)
dashboard/          # Flask API + React UI
  app.py            # Flask backend
  web/              # React + shadcn/ui + Tailwind v4
data/               # CSVs + SQLite DB
```
