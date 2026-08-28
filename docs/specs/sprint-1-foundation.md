# SPRINT 1: Foundation — Libraries, Tools & Technical Spec

> Everything needed to build the Workbook UI, 15+ Providers, and Docker deploy.
> This doc covers specific libraries, APIs, code patterns, and integration specs.

---

## S1.1 — Workbook UI (The Programmable Spreadsheet)

### Libraries We Already Have ✅

| Library | Version | What It Does | Status |
|---|---|---|---|
| `@tanstack/react-table` | ^8.21.3 | Headless table with sorting, filtering, column reorder, pagination | ✅ Installed |
| `@tanstack/react-virtual` | ^3.13.24 | Virtual scrolling for 10K+ rows without DOM bloat | ✅ Installed |
| `@tanstack/react-query` | ^5.100.8 | Server state management, caching, optimistic updates | ✅ Installed |
| `lucide-react` | ^1.14.0 | Icons for column types, status indicators | ✅ Installed |
| `tailwindcss` | ^4.2.4 (via vite plugin) | Styling | ✅ Installed |
| `clsx` + `cva` | latest | Conditional class names | ✅ Installed |
| `cmdk` | ^1.1.1 | Command palette (for column type picker) | ✅ Installed |

### Libraries to Add 📦

| Library | NPM | Why | Install |
|---|---|---|---|
| **`@dnd-kit`** | `@dnd-kit/core` + `@dnd-kit/sortable` + `@dnd-kit/utilities` | Drag-drop column reorder, row drag-select. Modern successor to react-dnd — smaller, touch-friendly, actively maintained | `bun add @dnd-kit/core @dnd-kit/sortable @dnd-kit/utilities` |
| **`sonner`** | `sonner` | Toast notifications for cell updates, errors | `bun add sonner` (may already be installed) |
| **`react-resizable-panels`** | `react-resizable-panels` | Resizable column config panel (sidebar) | `bun add react-resizable-panels` |
| **`papaparse`** | `papaparse` | CSV import/export (Clay CSV migration) | `bun add papaparse @types/papaparse` |
| **`jotai`** | `jotai` | Atomic state for workbook — each cell is an atom, so updating one cell doesn't re-render the entire table. Built for spreadsheet-style fine-grained reactivity | `bun add jotai` |
| **`@tanstack/react-table` plugins** | — | Already have core. Need: column pinning, column visibility, row selection | ✅ Built into v8 |

### Why TanStack Table (Not AG Grid or Handsontable)

| Option | Cost | Bundle | Fit |
|---|---|---|---|
| **AG Grid** | Free community, $1K+ enterprise | 200KB+ | Overkill — batteries-included, hard to customize |
| **Handsontable** | Commercial license required | 300KB+ | Excel-clone, wrong UX pattern for us |
| **TanStack Table** | MIT, free forever | **15KB** | ✅ Headless = total UI control. Already installed. Perfect for custom enrichment column UX |

TanStack gives us the logic (sorting, filtering, pagination, column reorder) while we build the exact Clay-like UI we need. No license restrictions for open-source.

### Real-Time Updates Architecture (WebSocket)

| Pattern | Use For | Tech |
|---|---|---|
| **WebSocket** | Bidirectional: cell updates (server→client) + user actions (client→server) | FastAPI `WebSocket` → native browser `WebSocket` |
| **REST + React Query** | Initial data load, CRUD operations | Existing `@tanstack/react-query` |
| **Optimistic Updates** | Instant UI feedback on edits | `react-query` `onMutate` + rollback |

**Why WebSocket**: Bidirectional — push cell enrichment results to client AND receive user actions (run column, stop enrichment, edit cell) in real-time. Same approach Clay uses. Enables future collaborative editing.

```typescript
// Frontend: WebSocket connection per workbook
const ws = new WebSocket(`ws://localhost:8000/api/workbooks/${id}/ws`);

// Receive cell updates from enrichment workers
ws.onmessage = (event) => {
  const msg = JSON.parse(event.data);
  if (msg.type === 'cell_update') {
    queryClient.setQueryData(['workbook', id], (old) =>
      updateCell(old, msg.rowId, msg.colId, msg.value, msg.status)
    );
  } else if (msg.type === 'job_progress') {
    setProgress(msg.completed, msg.total);
  }
};

// Send actions to server
ws.send(JSON.stringify({ type: 'run_column', colId: 'email_finder' }));
ws.send(JSON.stringify({ type: 'stop_enrichment' }));
```

```python
# Backend: WebSocket endpoint
from fastapi import WebSocket, WebSocketDisconnect
import redis.asyncio as aioredis

@app.websocket("/api/workbooks/{workbook_id}/ws")
async def workbook_ws(websocket: WebSocket, workbook_id: str):
    await websocket.accept()
    redis = aioredis.from_url("redis://redis:6379")
    pubsub = redis.pubsub()
    await pubsub.subscribe(f"workbook:{workbook_id}")
    
    try:
        # Listen for both: Redis updates + client messages
        async for message in pubsub.listen():
            if message["type"] == "message":
                await websocket.send_text(message["data"])
    except WebSocketDisconnect:
        await pubsub.unsubscribe(f"workbook:{workbook_id}")
```

### Job Queue: Redis + BullMQ

> **Why BullMQ over ARQ**: ARQ is in ["maintenance only mode"](https://github.com/python-arq/arq/issues/510) — no new features, frozen. BullMQ is actively developed (10M+ downloads/mo), has an official Python SDK (`pip install bullmq`), and provides critical features ARQ lacks: Flows (parent→child jobs for waterfall chains), job progress events, built-in rate limiting, deduplication, and dead letter queues.

Enrichment jobs (SMTP verify, API calls, DDG search) are slow (2-30s). They run in background workers, not in the API process.

| Component | Tool | Why |
|---|---|---|
| **Queue** | Redis + BullMQ | Persistent, Flows for waterfall chains, job progress, rate limiting |
| **Workers** | BullMQ Python (`pip install bullmq`) | Async-native, retries with exponential backoff, concurrency control |
| **Broadcast** | Redis Pub/Sub | Push cell updates from worker → WebSocket → browser |
| **Dashboard** | Bull Board (optional, Node) | Visual job monitoring, debugging |

```python
# apps/api/services/leadgen/workbook/worker.py
from bullmq import Queue, Worker, FlowProducer
import asyncio, json, redis.asyncio as aioredis

# --- Producer (called from FastAPI API) ---
enrichment_queue = Queue("enrichment")

async def enqueue_row_enrichment(workbook_id: str, row_id: int, columns: list):
    """Enqueue enrichment for an entire row using BullMQ Flows."""
    flow = FlowProducer()
    # Parent job waits for all column enrichments to complete
    await flow.add({
        "name": f"row-{row_id}",
        "queueName": "enrichment",
        "data": {"workbook_id": workbook_id, "row_id": row_id},
        "children": [
            {
                "name": f"cell-{row_id}-{col['id']}",
                "queueName": "enrichment",
                "data": {
                    "workbook_id": workbook_id,
                    "row_id": row_id,
                    "col_id": col["id"],
                    "provider_chain": col["waterfall"],
                },
                "opts": {
                    "attempts": 3,
                    "backoff": {"type": "exponential", "delay": 2000},
                },
            }
            for col in columns if col["type"] in ("enrichment", "waterfall")
        ],
    })

# --- Worker (runs as separate process) ---
async def process_enrichment(job, token):
    """Process a single cell enrichment job."""
    data = job.data
    r = aioredis.from_url("redis://redis:6379")
    
    for provider_name in data.get("provider_chain", []):
        provider = get_provider(provider_name)
        result = await provider.enrich(lead)
        
        # Report progress via BullMQ
        await job.updateProgress({"provider": provider_name, "status": "trying"})
        
        if result.success:
            await update_cell_in_db(data["workbook_id"], data["row_id"], data["col_id"], result)
            # Push to WebSocket via Redis pub/sub
            await r.publish(f"workbook:{data['workbook_id']}", json.dumps({
                "type": "cell_update",
                "rowId": data["row_id"],
                "colId": data["col_id"],
                "value": result.value,
                "status": "complete",
                "provider": provider_name,
            }))
            return {"success": True, "provider": provider_name}
    
    # All providers failed
    await r.publish(f"workbook:{data['workbook_id']}", json.dumps({
        "type": "cell_update", "rowId": data["row_id"], "colId": data["col_id"],
        "status": "error", "value": None,
    }))
    return {"success": False}

async def main():
    worker = Worker("enrichment", process_enrichment, {
        "concurrency": 20,
        "limiter": {"max": 10, "duration": 1000},  # Rate limit: 10 jobs/sec
    })
    # Keep worker running
    await asyncio.Future()

if __name__ == "__main__":
    asyncio.run(main())
```

```bash
# Run worker separately (or in Docker)
python -m services.leadgen.workbook.worker
```

### Workbook Data Model (Backend)

```python
# apps/api/services/leadgen/workbook/models.py

from sqlalchemy import Column, String, Integer, Text, DateTime, ForeignKey, JSON
from sqlalchemy.orm import relationship

class Workbook(Base):
    __tablename__ = "workbooks"
    id = Column(String, primary_key=True, default=lambda: str(uuid4()))
    name = Column(String, nullable=False)
    description = Column(Text, default="")
    columns_config = Column(JSON, default=list)  # [{id, name, type, provider, waterfall, condition}]
    created_at = Column(DateTime, server_default=func.now())
    updated_at = Column(DateTime, onupdate=func.now())
    rows = relationship("WorkbookRow", back_populates="workbook", cascade="all, delete-orphan")

class WorkbookRow(Base):
    __tablename__ = "workbook_rows"
    id = Column(Integer, primary_key=True, autoincrement=True)
    workbook_id = Column(String, ForeignKey("workbooks.id"), nullable=False)
    data = Column(JSON, default=dict)  # {col_id: {value, status, provider, updated_at}}
    status = Column(String, default="pending")  # pending, enriching, complete, error
    created_at = Column(DateTime, server_default=func.now())
    workbook = relationship("Workbook", back_populates="rows")
```

### Column Types

```python
COLUMN_TYPES = {
    "input": {
        "description": "User-provided data (text, number, URL)",
        "icon": "Type",
        "editable": True,
        "has_config": False,
    },
    "enrichment": {
        "description": "Single provider enrichment",
        "icon": "Sparkles", 
        "editable": False,
        "has_config": True,  # provider selection, field mapping
    },
    "waterfall": {
        "description": "Chain of providers with fallback",
        "icon": "Layers",
        "editable": False,
        "has_config": True,  # provider chain, priority order
    },
    "ai_formula": {
        "description": "LLM-powered transformation",
        "icon": "Brain",
        "editable": False,
        "has_config": True,  # prompt template, input columns
    },
    "conditional": {
        "description": "Only runs if condition is met",
        "icon": "GitBranch",
        "editable": False,
        "has_config": True,  # condition expression, then-column
    },
    "output": {
        "description": "Push to CRM, sequencer, webhook",
        "icon": "Send",
        "editable": False,
        "has_config": True,  # destination, field mapping
    },
}
```

### API Endpoints

```python
# apps/api/routes/workbooks.py

router = APIRouter(prefix="/api/workbooks", tags=["workbooks"])

@router.post("/")                           # Create workbook
@router.get("/")                            # List workbooks
@router.get("/{workbook_id}")               # Get workbook + rows
@router.put("/{workbook_id}")               # Update workbook metadata
@router.delete("/{workbook_id}")            # Delete workbook

@router.post("/{workbook_id}/columns")      # Add column
@router.put("/{workbook_id}/columns/{col}") # Update column config
@router.delete("/{workbook_id}/columns/{col}") # Remove column

@router.post("/{workbook_id}/rows")         # Add rows (import CSV, manual, from chat)
@router.put("/{workbook_id}/rows/{row_id}") # Edit row data
@router.delete("/{workbook_id}/rows/{row_id}") # Delete row

@router.post("/{workbook_id}/run")          # Execute all enrichment columns
@router.post("/{workbook_id}/rows/{row_id}/run") # Execute single row
@router.post("/{workbook_id}/stop")         # Stop running enrichments

@router.get("/{workbook_id}/stream")        # SSE stream for cell updates
@router.post("/{workbook_id}/export")       # Export to CSV/JSON
```

---

## S1.2 — Enrichment Providers (15+ Target)

### Provider Architecture

All providers implement the same interface. This is the BYOK-ready pattern:

```python
# apps/api/services/leadgen/enrichment/provider.py (already exists)

class EnrichmentProvider(ABC):
    name: str
    capability: str      # "email", "phone", "company", "person", "signal", "verify"
    requires_api_key: bool
    free_tier_limit: int  # 0 = unlimited, >0 = monthly cap
    
    @abstractmethod
    async def enrich(self, lead: Lead) -> EnrichmentResult: ...
    
    def is_available(self) -> bool:
        """Check if provider has API key (if required) and hasn't hit rate limit."""
        ...
```

### Provider Inventory: What We Have + What to Add

#### ✅ Already Built (4 providers)

| # | Provider | File | Capability | API Key? | Limit |
|---|---|---|---|---|---|
| 1 | **CrossLinked** | `crosslinked.py` | People discovery via DDG→LinkedIn | No | Unlimited |
| 2 | **MailScout** | `mailscout_verify.py` | SMTP RCPT TO email verification | No | Unlimited |
| 3 | **JobSpy** | `jobspy_signals.py` | Hiring signal detection | No | Unlimited |
| 4 | **Facebook Pages** | `facebook_pages.py` | SMB contact extraction | No | Unlimited |

#### 📦 Wrap Existing Capabilities as Providers (6 providers)

These are existing code in our codebase — just need to be wrapped in the `EnrichmentProvider` interface:

| # | Provider | Wrap From | Capability | API Key? | Limit |
|---|---|---|---|---|---|
| 5 | **DDG Email** | `email_finder.py` | Email pattern gen + MX verify | No | Unlimited |
| 6 | **DDG Company** | `search_enricher.py` | Company info via web search | No | Unlimited |
| 7 | **Website Scraper** | `website_scraper.py` | Contact page extraction | No | Unlimited |
| 8 | **Social Finder** | `social_finder.py` | LinkedIn/Twitter/Facebook profiles | No | Unlimited |
| 9 | **Decision Makers** | `decision_maker_finder.py` | Key people via DDG | No | Unlimited |
| 10 | **Lead Scorer** | `scoring.py` | Rule-based scoring | No | Unlimited |

#### 🆕 New Free-Tier API Providers (5+ providers)

| # | Provider | API | Capability | Free Tier | API Key? | Docs |
|---|---|---|---|---|---|---|
| 11 | **Hunter.io** | `https://api.hunter.io/v2/` | Email finder + verifier | 25 lookups/mo | Yes (BYOK) | [hunter.io/api](https://hunter.io/api-documentation/v2) |
| 12 | **AbstractAPI** | `https://emailvalidation.abstractapi.com/v1/` | Email validation + deliverability | 100/mo | Yes (BYOK) | [abstractapi.com](https://www.abstractapi.com/api/email-verification-validation-api) |
| 13 | **Apollo.io** | `https://api.apollo.io/api/v1/` | People + company enrichment | 50 credits/mo | Yes (BYOK) | [apollo.io/docs](https://apolloio.github.io/apollo-api-docs/) |
| 14 | **NumVerify** | `https://apilayer.com/marketplace/number_verification-api` | Phone number validation | 100/mo | Yes (BYOK) | [numverify.com](https://numverify.com/documentation) |
| 15 | **IPInfo** | `https://ipinfo.io/` | Company data from IP/domain | 50K/mo | Yes (free key) | [ipinfo.io](https://ipinfo.io/developers) |

#### 🔧 Open-Source Self-Hosted Providers (3 providers)

| # | Provider | GitHub | Capability | Self-Hosted | Stars |
|---|---|---|---|---|---|
| 16 | **Reacher** | [reacherhq/check-if-email-exists](https://github.com/reacherhq/check-if-email-exists) | Full email verification (SMTP, MX, syntax, disposable detection) | Rust binary, Docker | 4.5K+ |
| 17 | **Truemail** | [truemail-rb/truemail](https://github.com/truemail-rb/truemail) | MX/SMTP/DNS email verification | Ruby, Docker API | 1.3K+ |
| 18 | **Thethe** | Self-built | Web tech detection (like BuiltWith) via HTTP headers + DOM | Python | — |

### Provider Implementation Pattern

```python
# Example: Hunter.io provider
# apps/api/services/leadgen/enrichment/providers/hunter_io.py

class HunterProvider(EnrichmentProvider):
    name = "hunter_io"
    capability = "email"
    requires_api_key = True
    free_tier_limit = 25  # per month
    
    def __init__(self, api_key: str = None):
        self.api_key = api_key or os.getenv("HUNTER_API_KEY")
        self.base_url = "https://api.hunter.io/v2"
    
    async def enrich(self, lead: Lead) -> EnrichmentResult:
        if not self.api_key:
            return EnrichmentResult(success=False, error="No API key")
        
        domain = self._extract_domain(lead.website or lead.company)
        async with httpx.AsyncClient() as client:
            # Domain search — find emails at company
            resp = await client.get(f"{self.base_url}/domain-search", params={
                "domain": domain,
                "api_key": self.api_key,
            })
            data = resp.json()
            
            if data.get("data", {}).get("emails"):
                best = data["data"]["emails"][0]
                return EnrichmentResult(
                    success=True,
                    email=best["value"],
                    confidence=best["confidence"] / 100,
                    provider="hunter_io",
                )
        return EnrichmentResult(success=False)
```

### BYOK Settings UI

```typescript
// apps/web/src/pages/settings.tsx — new "API Keys" section

interface ProviderKey {
  provider: string;
  label: string;
  key: string;
  description: string;
  freeLimit: string;
  docsUrl: string;
}

const PROVIDER_KEYS: ProviderKey[] = [
  { provider: "hunter_io", label: "Hunter.io", key: "", description: "Email finder & verifier", freeLimit: "25/mo", docsUrl: "https://hunter.io/api" },
  { provider: "abstract_api", label: "AbstractAPI", key: "", description: "Email validation", freeLimit: "100/mo", docsUrl: "https://abstractapi.com" },
  { provider: "apollo_io", label: "Apollo.io", key: "", description: "People & company enrichment", freeLimit: "50/mo", docsUrl: "https://apollo.io" },
  { provider: "numverify", label: "NumVerify", key: "", description: "Phone validation", freeLimit: "100/mo", docsUrl: "https://numverify.com" },
  { provider: "ipinfo", label: "IPInfo", key: "", description: "Company data from domain", freeLimit: "50K/mo", docsUrl: "https://ipinfo.io" },
];
```

### Provider Backend Dependencies

```toml
# Add to pyproject.toml [project.dependencies]
"httpx>=0.28.1"           # Already installed — async HTTP client for all API providers
"dnspython>=2.8.0"        # Already installed — DNS/MX resolution
"aiosmtplib>=3.0.0"       # NEW — async SMTP for email verification (replaces sync smtplib)
"python-whois>=0.9.0"     # NEW — domain age/registrar lookup
"tldextract>=5.0.0"       # NEW — reliable domain extraction from URLs
```

---

## S1.3 — Docker One-Click Deploy

### Architecture

```
┌──────────────────────────────────────────────────────────────┐
│                        nginx :80                             │
│              (reverse proxy + WebSocket proxy)               │
│          /        → web (static React build)                 │
│          /api     → api (FastAPI REST)                       │
│          /api/ws  → api (FastAPI WebSocket)                  │
├──────────────┬───────────────┬───────────────────────────────┤
│              │               │                               │
│  ┌────────┐  │  ┌─────────┐  │  ┌──────────┐                │
│  │  web   │  │  │   api   │  │  │  worker  │                │
│  │(React) │  │  │(FastAPI)│  │  │  (ARQ)   │                │
│  │ static │  │  │ :8000   │  │  │enrichment│                │
│  └────────┘  │  └────┬────┘  │  └────┬─────┘                │
│              │       │       │       │                       │
│              │       ▼       │       ▼                       │
│              │  ┌────────────────────────┐                   │
│              │  │      Redis :6379       │                   │
│              │  │  (queue + pub/sub)     │                   │
│              │  └───────────┬────────────┘                   │
│              │              │                                │
│              │         ┌────▼─────┐                          │
│              │         │   db     │                          │
│              │         │(SQLite/  │                          │
│              │         │ Postgres)│                          │
│              │         └──────────┘                          │
└──────────────────────────────────────────────────────────────┘
```

### Files to Create

#### `docker-compose.yml` (root)

```yaml
version: "3.9"
services:
  web:
    build:
      context: ./apps/web
      dockerfile: Dockerfile
    depends_on:
      - api

  api:
    build:
      context: ./apps/api
      dockerfile: Dockerfile
    env_file: .env
    volumes:
      - ./data:/app/data  # SQLite database persistence
    depends_on:
      redis:
        condition: service_healthy
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8000/health"]
      interval: 30s
      timeout: 10s
      retries: 3

  worker:
    build:
      context: ./apps/api
      dockerfile: Dockerfile
    command: python -m services.leadgen.workbook.worker
    env_file: .env
    depends_on:
      redis:
        condition: service_healthy

  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"
    volumes:
      - redisdata:/data
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 10s
      timeout: 5s
      retries: 3

  db:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: ${DB_USER:-yupcha}
      POSTGRES_PASSWORD: ${DB_PASSWORD:-yupcha}
      POSTGRES_DB: ${DB_NAME:-yupcha}
    volumes:
      - pgdata:/var/lib/postgresql/data
    profiles: ["postgres"]  # Optional: only if user wants Postgres

  nginx:
    image: nginx:alpine
    ports:
      - "${PORT:-3000}:80"
    volumes:
      - ./nginx.conf:/etc/nginx/conf.d/default.conf
    depends_on:
      - web
      - api

volumes:
  pgdata:
  redisdata:
```

#### `apps/web/Dockerfile`

```dockerfile
# Stage 1: Build
FROM node:20-alpine AS builder
WORKDIR /app
COPY package.json bun.lockb* ./
RUN npm install
COPY . .
RUN npm run build

# Stage 2: Serve with nginx
FROM nginx:alpine
COPY --from=builder /app/dist /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf
```

#### `apps/api/Dockerfile`

```dockerfile
FROM python:3.13-slim
WORKDIR /app

# Install system deps
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl gcc libpq-dev && rm -rf /var/lib/apt/lists/*

# Install Python deps
COPY pyproject.toml ./
RUN pip install --no-cache-dir -e .

# Copy app code
COPY . .

# Health check endpoint
HEALTHCHECK CMD curl -f http://localhost:8000/health || exit 1

EXPOSE 8000
CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000", "--workers", "2"]
```

#### `.env.example`

```env
# === Required ===
OPENAI_API_KEY=sk-your-key-here

# === Optional: Database (default: SQLite) ===
# DB_TYPE=postgres
# DB_USER=yupcha
# DB_PASSWORD=yupcha
# DB_HOST=db
# DB_PORT=5432
# DB_NAME=yupcha

# === Optional: Enrichment Provider API Keys (BYOK) ===
# HUNTER_API_KEY=
# ABSTRACT_API_KEY=
# APOLLO_API_KEY=
# NUMVERIFY_API_KEY=
# IPINFO_TOKEN=

# === Optional: Server Config ===
# PORT=3000
# LOG_LEVEL=info
```

### Key Tools

| Tool | What | Why |
|---|---|---|
| **Docker Compose v2** | Multi-container orchestration | Industry standard for self-hosted apps |
| **Redis 7 Alpine** | Job queue + pub/sub for WebSocket broadcast | Required for ARQ workers + real-time updates |
| **nginx:alpine** | Reverse proxy + static file server + WebSocket proxy | 5MB image, serves React build + proxies API |
| **python:3.13-slim** | API base image | Small (~150MB), has our Python version |
| **postgres:16-alpine** | Optional production DB | If users want more than SQLite |
| **SQLite** (default) | Zero-config database | No separate service needed, just a file |

---

## Full Dependency Summary

### Frontend (bun add)

```bash
# New packages to install
bun add @dnd-kit/core @dnd-kit/sortable @dnd-kit/utilities  # Drag-drop for columns
bun add react-resizable-panels             # Resizable config panel
bun add papaparse @types/papaparse         # CSV import/export
bun add jotai                              # Atomic state for workbook cells
# sonner — check if already installed
```

### Backend (pip/pyproject.toml)

```bash
# New packages
pip install bullmq           # Job queue with Flows, progress, rate limiting
pip install redis[hiredis]   # Async Redis client for pub/sub + BullMQ
pip install aiosmtplib       # Async SMTP verification
pip install python-whois     # Domain age lookup
pip install tldextract       # Clean domain extraction
# httpx, dnspython, beautifulsoup4, duckduckgo-search — already installed
```

### Docker

```bash
# No install needed — user has Docker
docker compose up           # Start everything
docker compose up --build   # Rebuild after changes
```

---

## Implementation Order

```
Day 1-2: Workbook Backend + Job Queue
├── Create workbook models + migration
├── CRUD API endpoints
├── WebSocket endpoint (FastAPI WebSocket + Redis pub/sub)
├── ARQ worker setup + enrich_cell job
└── Wire existing providers into column execution

Day 2-3: Workbook Frontend
├── Workbook list page
├── Workbook editor page (TanStack Table)
├── Column config panel
├── CSV import (papaparse)
├── WebSocket client (cell status indicators)
└── Column type picker (cmdk)

Day 3-4: Providers
├── Wrap 6 existing capabilities as providers
├── Add Hunter.io provider
├── Add AbstractAPI provider
├── Add Apollo.io provider (BYOK)
├── BYOK settings UI
└── Provider health check / status page

Day 4-5: Docker + Polish
├── Dockerfiles (web + api + worker)
├── docker-compose.yml (with Redis)
├── nginx.conf (with WebSocket proxy)
├── .env.example
├── README quickstart section
└── Health check endpoint
```
