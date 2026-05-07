# Chat-to-Action Agentic SaaS Blueprint

> **Purpose:** Reusable architectural reference for building chat-driven agentic SaaS products.
> Extracted from the Yupcha lead intelligence codebase. Applies to any domain where a user
> types natural language and the system discovers data, executes actions, and persists results.

---

## 1. Architecture Overview

```
┌──────────────────────────────────────────────────────────────────────┐
│                         FRONTEND (React)                            │
│  ┌──────────┐   ┌───────────┐   ┌──────────┐   ┌────────────────┐  │
│  │ Chat UI  │──▶│ streamChat│──▶│ SSE Parse│──▶│ State Updates  │  │
│  │ (input)  │   │ (fetch)   │   │ (events) │   │ (tool results) │  │
│  └──────────┘   └───────────┘   └──────────┘   └────────────────┘  │
│        │                                              │             │
│        │              ┌───────────┐                   ▼             │
│        └─────────────▶│ Workbook  │◀──── "Merge to Workbook" ──────┘│
│                       │  Editor   │                                 │
└──────────────────────────────────────────────────────────────────────┘
                              │ SSE stream
                              ▼
┌──────────────────────────────────────────────────────────────────────┐
│                     API LAYER (FastAPI)                              │
│                                                                     │
│  ┌─────────────────────────────────────────────────────────────────┐ │
│  │              /api/copilotkit  (POST, SSE)                      │ │
│  │                                                                │ │
│  │  1. Parse messages + conversation_id                           │ │
│  │  2. Build system prompt (ICP + live stats + capabilities)      │ │
│  │  3. Inject memory context (semantic search of past exchanges)  │ │
│  │  4. Sanitize messages for provider compatibility               │ │
│  │  5. Stream to LLM provider with auto-failover                 │ │
│  │  6. Intercept tool_calls → execute → inject results → recurse │ │
│  │  7. Persist conversation + extract memories                    │ │
│  └─────────────────────────────────────────────────────────────────┘ │
│                                                                     │
│  ┌──────────┐  ┌──────────┐  ┌───────────┐  ┌────────────────────┐ │
│  │ Tool     │  │ Provider │  │ Chat      │  │ Memory             │ │
│  │ Registry │  │ Chain    │  │ History   │  │ (OpenMemory)       │ │
│  └──────────┘  └──────────┘  └───────────┘  └────────────────────┘ │
│                                                                     │
│  ┌──────────────────────────────────────────────────────────────┐   │
│  │                     DOMAIN SERVICES                          │   │
│  │  LeadDB · JobRunner · EmailFinder · WebsiteScraper ·        │   │
│  │  AmbitionBox · CrossLinked · WorkbookEnrichment             │   │
│  └──────────────────────────────────────────────────────────────┘   │
└──────────────────────────────────────────────────────────────────────┘
```

---

## 2. The Core Loop: Chat → Tool → Action → Result

This is the heart of the system. Every interaction follows this cycle:

```
User message
    │
    ▼
┌─────────────────────────────┐
│ LLM receives:              │
│  • System prompt (context)  │
│  • Tool definitions (JSON)  │
│  • Conversation history     │
│  • Memory recall            │
└────────────┬────────────────┘
             │
     ┌───────┴────────┐
     │                │
  Content          Tool Call
  (stream to       (intercept)
   client)              │
                        ▼
              ┌─────────────────┐
              │ _execute_tool() │  ← Server-side, NOT client
              │  • Query DB     │
              │  • Call API     │
              │  • Spawn job    │
              │  • Scrape site  │
              └────────┬────────┘
                       │
                       ▼
              ┌─────────────────┐
              │ Tool result     │
              │ injected back   │
              │ into messages[] │
              └────────┬────────┘
                       │
                       ▼
              ┌─────────────────┐
              │ Recursive call  │
              │ to _stream_chat │  ← LLM sees tool result,
              │ with results    │     generates human response
              └────────┬────────┘
                       │
                       ▼
              Final content streamed to client
```

### Key Design Decision: Server-Side Tool Execution

Tools execute **on the backend**, not the frontend. The LLM never gets raw DB access.
The server intercepts `finish_reason: "tool_calls"`, runs the function, and feeds results
back. The client only sees:
- `tool_call` events (for UX indicators: "Running search_leads...")
- `tool_result` events (for inline data cards)
- `content` events (the LLM's final human-readable response)

---

## 3. Component-by-Component Breakdown

### 3.1 System Prompt (Dynamic Context Injection)

```python
# The system prompt is NOT static. It's rebuilt per-request with live data.
def _build_system_prompt() -> str:
    # 1. ICP (Ideal Customer Profile) — domain config
    # 2. Live pipeline stats — fetched from DB right now
    # 3. Capability list — what tools can do, in plain English
    # 4. Response guidelines — formatting, tone, actionability
```

**Pattern:** Your system prompt should include:
- **Who the AI is** (domain persona)
- **What it knows** (live data snapshot from your DB)
- **What it can do** (tool summary in plain English — the LLM reads this to decide when to call tools)
- **How to respond** (formatting rules, tone, actionability directives)

### 3.2 Tool Definitions (OpenAI Function Calling Schema)

Each tool is defined using the [OpenAI function calling format](https://platform.openai.com/docs/guides/function-calling):

```python
{
    "type": "function",
    "function": {
        "name": "search_leads",
        "description": "Search for leads in the database by company name, city...",
        "parameters": {
            "type": "object",
            "properties": {
                "query": {"type": "string", "description": "Search query"},
                "city":  {"type": "string", "description": "Filter by city"},
                "limit": {"type": "integer", "default": 10},
            },
            "required": ["query"],
        },
    },
}
```

**Tool taxonomy** (what types of tools you need):

| Category | Examples | Purpose |
|----------|----------|---------|
| **Query** | `search_leads`, `get_lead_detail`, `get_lead_stats` | Read data from your domain DB |
| **Discovery** | `ambitionbox_search`, `start_collection` | Find NEW data from external sources |
| **Action** | `update_lead_status`, `enrich_lead` | Mutate state in your system |
| **Analysis** | `compare_leads`, `get_enrichment_gaps`, `find_similar_leads` | Compute insights over data |
| **Generate** | `suggest_outreach`, `create_workbook` | Create new artifacts (emails, workbooks) |

### 3.3 Tool Execution (_execute_tool)

A single dispatch function that maps tool names to actual Python functions:

```python
async def _execute_tool(name: str, args: dict) -> str:
    """Execute a backend tool and return the result as JSON string."""
    db = LeadDB()
    try:
        if name == "search_leads":
            leads = db.get_leads(search=args.get("query"), ...)
            return json.dumps({"leads": [...], "count": len(result)})

        elif name == "start_collection":
            job_id = str(uuid.uuid4())[:8]
            # Spawn background thread — don't block the chat
            threading.Thread(target=_run, daemon=True).start()
            return json.dumps({"ok": True, "job_id": job_id, ...})

        elif name == "enrich_lead":
            # Heavy I/O work in a new event loop to not block
            loop = asyncio.new_event_loop()
            result = loop.run_until_complete(_scrape_via_http(client, url))
            loop.close()
            return json.dumps({"enriched_fields": [...]})
    finally:
        db.close()
```

**Patterns:**
- Always return **JSON strings** (the LLM needs to parse them)
- For long-running work, **spawn a background job** and return a `job_id` immediately
- Return **structured summaries**, not raw data dumps (the LLM will format for the user)
- Always close DB connections in `finally`

### 3.4 Streaming Protocol (SSE)

The API uses Server-Sent Events (SSE) with a custom event format:

```
data: {"conversation_id": "abc123"}     ← First event: session ID
data: {"content": "Let me search..."}   ← Text chunks (streamed)
data: {"tool_call": {"name": "search_leads", "args": {"query": "IT"}}}
data: {"tool_result": {"name": "search_leads", "result": {...}}}
data: {"content": "I found 15 leads..."} ← LLM response after tool execution
data: [DONE]                             ← Stream complete
```

**Frontend parsing:**

```typescript
const reader = res.body.getReader()
let buffer = ""
while (true) {
  const { done, value } = await reader.read()
  if (done) break
  buffer += decoder.decode(value, { stream: true })
  const lines = buffer.split("\n")
  buffer = lines.pop() || ""       // Keep incomplete line in buffer
  for (const line of lines) {
    if (line.startsWith("data: ")) {
      const raw = line.slice(6)
      if (raw === "[DONE]") return
      onEvent(JSON.parse(raw))     // Dispatch to state handlers
    }
  }
}
```

### 3.5 Provider Chain (Multi-LLM Failover)

```python
def _get_provider_chain() -> list:
    """Build ordered list: [active_provider, fallback_1, fallback_2, ...]"""
    # Each provider is a dict with: id, name, api_key, base_url, model
```

On 429/5xx, the system automatically:
1. Yields a user-facing message: `> ⚡ Google AI rate limited — switching to OpenRouter...`
2. Recursively calls `_stream_chat()` with the next provider

**Pattern:** Always have ≥2 providers. Free tiers get exhausted fast.

### 3.6 Message Sanitization

Different LLM providers have different strictness levels. Gemini rejects:
- Missing `content` keys
- Extra fields like `id`, `createdAt` (injected by frontend SDKs)
- Empty string arguments in tool calls
- `role: "function"` (legacy OpenAI format)

```python
for m in messages:
    clean_m = {"role": m.get("role", "user")}
    # Only keep: role, content, tool_calls, tool_call_id, name
    # Default empty content to " " for Gemini
    # Map "function" role to "tool"
    # Skip tool messages without tool_call_id
    cleaned_messages.append(clean_m)
```

**Pattern:** Always sanitize before sending to any provider. Future-proofs against SDK changes.

---

## 4. Chat → Task Creation Pipeline

This is the flow from "Find 50 IT companies in Bangalore" to a running background job:

```
User: "Find 50 IT staffing companies in Bangalore"
  │
  ▼
LLM decides to call: start_collection(query="IT staffing companies in Bangalore")
  │
  ▼
_execute_tool("start_collection", {...})
  │
  ├─ Creates a job record in DB (job_id, query, status=running)
  ├─ Spawns a daemon thread running JobRunner
  ├─ Returns immediately: {"job_id": "abc123", "message": "Collection started..."}
  │
  ▼
LLM receives tool result → generates human response:
  "I've started collecting IT staffing companies in Bangalore using 6 strategies..."
  │
  ▼
Frontend receives tool_result with job_id
  ├─ Renders inline <TaskDetailCard jobId="abc123" />
  ├─ Card polls /api/jobs for progress updates
  └─ When complete, shows "Merge to Workbook" button
```

### 4.1 Background Job Architecture

```python
class JobRunner:
    """Orchestrates multi-source lead collection."""

    async def _process_job(self, job):
        # Runs 6 strategies in parallel:
        # Maps, Web, Directories, LinkedIn, Job Boards, Review Sites
        # Each strategy:
        #   1. Searches for companies matching the query
        #   2. Scrapes company details
        #   3. Validates and deduplicates
        #   4. Scores leads (0-100)
        #   5. Upserts into LeadDB
        #   6. Updates job progress
```

### 4.2 Job → Workbook Bridge

After discovery, users can push results into the Workbook Editor for enrichment:

```
Chat (discovery) ──▶ Job (background) ──▶ Workbook (enrichment + export)
       │                     │                      │
  "Find leads"         Leads created          Enrichment columns
                       in LeadDB              run providers against
                                              each lead (email, phone,
                                              decision makers, etc.)
```

The "Merge all to Workbook" button in the chat UI:
```typescript
const wb = await createWorkbookFromJobs({ job_ids: allJobIds })
// Creates a Workbook with filter_criteria: { job_ids: [...] }
// The workbook is a LIVE VIEW over the Lead DB — no data duplication
navigate(`/workbooks/${wb.id}`)
```

---

## 5. Persistence Layers

### 5.1 Chat History (SQLite)

```
conversations: id, title, created_at, updated_at
messages:      id, conversation_id, role, content, tool_data, created_at
```

- `tool_data` stores JSON from tool results (e.g., `{"job_id": "abc123"}`) for rendering task cards on reload
- Title is auto-generated from the first user message
- Messages are persisted **after** the full stream completes (not per-chunk)

### 5.2 Memory (OpenMemory / Semantic)

```python
# After each exchange:
memory.add_memory(
    f"User asked: {user_msg[:200]}\nAssistant answered about: {response[:200]}",
    user_id="default",
    metadata={"conversation_id": conv_id, "type": "episodic"},
)

# Before each request:
memories = memory.search_memory(user_msg, limit=5)  # Semantic search
# Injected as a system message: "Relevant memories from past conversations: ..."
```

**Pattern:** Memory gives the AI continuity across conversations without replaying full history.

### 5.3 Domain Data (LeadDB — SQLite with FTS5)

```
leads:     id, company, email, phone, website, score, score_tier, ...
leads_fts: FTS5 virtual table for full-text search
jobs:      id, query, status, progress, created_at
```

### 5.4 Workbook Layer (SQLAlchemy + SQLite overlay)

```
workbooks:            id, name, filter_criteria, columns_config
workbook_enrichments: workbook_id, lead_id, column_id, value, status, provider
```

The workbook is a **virtual view** — it doesn't copy leads. `filter_criteria` determines
which leads from LeadDB appear. Enrichment results that map to known Lead fields write
**back to the Lead record** (source of truth).

---

## 6. Enrichment Architecture

### 6.1 Provider Abstraction

```python
class EnrichmentProvider:
    """Base class for all enrichment providers."""
    async def enrich(self, lead: Lead) -> EnrichmentResult:
        ...

class EnrichmentResult:
    success: bool
    fields: dict[str, str]   # {"email": "...", "phone": "...", "decision_makers": "[...]"}
    provider: str
    confidence: float
```

### 6.2 Waterfall Pattern

Columns can define a `waterfall` — an ordered list of providers to try:

```json
{
  "id": "email",
  "type": "waterfall",
  "waterfall": ["hunter_io", "clearbit", "pattern_guess"],
  "target_field": "email"
}
```

The system tries each provider in order, stops at the first success.

### 6.3 Write-Back vs. Overlay

| Field Type | Storage | Example |
|-----------|---------|---------|
| Known Lead field | Writes back to Lead record | email, phone, linkedin_url |
| AI/computed | WorkbookEnrichment overlay | sentiment score, custom AI analysis |
| Structured JSON | Lead record (JSON column) | decision_makers, hiring_signals |

---

## 7. Frontend State Machine

```
IDLE ──▶ LOADING ──▶ STREAMING ──▶ TOOL_EXECUTING ──▶ STREAMING ──▶ DONE
                        │                                  │
                  content chunks              LLM response after tool
                  (typing effect)             results are injected
```

State variables:
```typescript
const [isLoading, setIsLoading] = useState(false)
const [streamingContent, setStreamingContent] = useState("")      // Accumulates content
const [streamingTool, setStreamingTool] = useState<string | null>(null)  // "Running tool: ..."
const [streamingJobIds, setStreamingJobIds] = useState<string[]>([])     // For task cards
```

The UI renders different components based on state:
- `TypingIndicator` → loading, no content yet
- `ToolIndicator` → tool executing (animated spinner + tool name)
- `MarkdownMessage` → streaming or completed content
- `TaskDetailCard` → inline progress card for background jobs

---

## 8. Replication Checklist

To replicate this pattern in a new domain:

### Step 1: Define Your Domain
- [ ] What is your data model? (Lead → your entity)
- [ ] What are your data sources? (Maps, APIs, scrapers → your sources)
- [ ] What enrichment do you need? (email → your enrichment types)

### Step 2: Build the Backend
- [ ] Create your domain DB (SQLite + FTS5 for search)
- [ ] Implement 3-5 core tools as Python functions
- [ ] Write a `_build_tools()` that returns OpenAI function-calling JSON
- [ ] Write a `_execute_tool()` dispatch function
- [ ] Write a `_build_system_prompt()` with live data injection
- [ ] Copy the `_stream_chat()` function (provider-agnostic, reusable as-is)
- [ ] Copy the message sanitizer (provider compatibility)
- [ ] Set up the provider chain (≥2 LLM providers)
- [ ] Set up chat history persistence (SQLite)

### Step 3: Build the Frontend
- [ ] Create a chat page with SSE streaming
- [ ] Parse `content`, `tool_call`, `tool_result`, `error` events
- [ ] Render tool execution indicators
- [ ] Render inline data cards from tool results
- [ ] Add conversation management (list, create, delete, edit+resubmit)

### Step 4: Add Background Jobs
- [ ] Design your job model (id, query, status, progress)
- [ ] Return `job_id` immediately from long-running tools
- [ ] Frontend polls or subscribes (SSE/WS) for job progress
- [ ] "Merge results" bridge to your structured editor (workbook/table/kanban)

### Step 5: Add Enrichment
- [ ] Define provider interface: `async enrich(entity) -> Result`
- [ ] Implement waterfall pattern for multi-provider fallback
- [ ] Add write-back logic (enrichment results → source of truth DB)
- [ ] Add overlay table for AI/computed columns

---

## 9. File Map (Reference Implementation)

```
apps/api/routers/
  copilotkit.py          ← Chat endpoint, tool defs, _stream_chat, sanitizer
  leads.py               ← Domain CRUD API
  workbooks.py           ← Workbook editor API (filtered views + enrichment)
  settings.py            ← Provider registry + settings persistence

apps/api/services/
  chat_history.py        ← Conversation + message persistence (SQLite)
  memory.py              ← Semantic memory (OpenMemory wrapper)
  leadgen/
    db.py                ← Domain DB (LeadDB, FTS5)
    models.py            ← Lead dataclass
    job_runner.py         ← Background job orchestrator
    enrichment/
      provider.py        ← EnrichmentProvider base + WaterfallEnricher
      email_finder.py    ← Email discovery provider
      website_scraper.py ← Website scraping provider
      decision_maker_finder.py ← LinkedIn/DDG decision maker discovery
  workbook/
    enrichment.py        ← Workbook enrichment orchestrator
    providers.py         ← Provider registry for workbook columns
    ai_column.py         ← AI formula column execution
    conditions.py        ← Conditional run logic
    worker.py            ← BullMQ background worker

apps/web/src/
  pages/chat.tsx         ← Chat UI (SSE stream, tool indicators, task cards)
  lib/api.ts             ← streamChat() SSE client
  components/
    task-detail-card.tsx  ← Inline job progress card
    markdown-content.tsx  ← Markdown renderer for AI responses
```

---

## 10. Anti-Patterns & Lessons Learned

| Anti-Pattern | What Happened | Fix |
|---|---|---|
| Raw user input in FTS5 MATCH | Periods, quotes, special chars crash SQLite FTS5 | Wrap all FTS queries in double-quotes via parameterized `?` |
| Frontend SDK fields in LLM messages | CopilotKit injects `id`, `createdAt` — Gemini rejects them | Sanitizer strips all non-standard fields before sending |
| Missing `content` on assistant tool_call messages | Recursive `_stream_chat` sends `{role: "assistant", tool_calls: [...]}` without `content` — Gemini 400s | Always include `"content": ""` on assistant messages with tool_calls |
| Debug code left in production | `open("debug.json", "w")` writing every request to disk | Remove all debug I/O before merging; use proper logging |
| String interpolation in SQL | FTS query built with f-strings → SQL injection | Always use `?` parameterized queries |
| Single LLM provider | Free tier exhaustion crashes the chat | Provider chain with auto-failover on 429/5xx |
| Blocking tool execution | `enrich_lead` blocks the SSE stream for 30s+ | Spawn background threads; return `job_id` immediately |
| Permanent proxy removal on 429 | Proxy pool drained within minutes | Cooldown-based benchmarking (65s) instead of permanent removal |
