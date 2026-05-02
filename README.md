# Yupcha Engine

Enterprise data intelligence platform — document search, lead generation, web scraping, and person intel.

## Architecture

```
apps/
├── api/          FastAPI backend (Python)
│   ├── routers/  API endpoints (auth, leads, search, scraper, intel)
│   ├── services/ Business logic (leadgen, scraper, queue, person_intel)
│   ├── sources/  Document source adapters (15+ sources)
│   └── workers/  Background job processors
└── web/          React frontend (TypeScript + ShadcnUI)
```

## Quick Start

```bash
# Install Python deps
uv sync

# Start API server
uv run uvicorn apps.api.main:app --reload --port 8000

# Build & serve frontend
cd apps/web && bun install && bun run build
```

## Features

- **Document Search** — Search across Scribd, LibGen, arXiv, Gutenberg, PDFDrive, and 10+ more sources
- **Lead Pipeline** — Automated business lead collection from Google Maps, web directories, and review sites
- **Web Scraper** — Universal scraper with httpx fast path and Playwright fallback
- **Person Intel** — LinkedIn profile enrichment via web OSINT
- **Download Queue** — Background document download with progress tracking
- **Workspaces** — Campaign-based organization for searches and leads

## Tech Stack

| Layer | Technology |
|-------|-----------|
| API | FastAPI, SQLAlchemy, SQLite |
| Frontend | React, TypeScript, ShadcnUI, Tailwind, Vite |
| Scraping | httpx, Playwright, curl-cffi, BeautifulSoup |
| Monorepo | Turborepo, Bun |
