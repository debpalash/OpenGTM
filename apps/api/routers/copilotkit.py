"""
CopilotKit Runtime — FastAPI endpoint

Provides the /api/copilotkit endpoint that CopilotKit's frontend connects to.
Uses the configured AI provider from the settings database.
Supports the CopilotKit protocol: /info (GET+POST) and chat (POST).
Includes: conversation history, OpenMemory integration, tool execution.
"""

import json
import os
import httpx
import uuid
import asyncio
import threading
from typing import AsyncGenerator
from fastapi import APIRouter, Request
from fastapi.responses import StreamingResponse, JSONResponse

from apps.api.routers.settings import _db_get, PROVIDERS
from apps.api.services.leadgen.db import LeadDB
from apps.api.services import chat_history, memory

router = APIRouter(prefix="/api/copilotkit", tags=["CopilotKit"])


def _get_active_provider() -> dict:
    """Get the currently configured AI provider and its credentials."""
    default_id = _db_get("LLM_DEFAULT_PROVIDER", "openrouter")
    prov = PROVIDERS.get(default_id)
    if not prov:
        prov = PROVIDERS.get("openrouter", PROVIDERS[list(PROVIDERS.keys())[0]])
        default_id = "openrouter"

    api_key = _db_get(prov["env_key"], "")
    base_url = _db_get(prov.get("env_url", ""), "") or prov.get("default_url", "")
    model = _db_get(prov.get("env_model", ""), "") or prov.get("default_model", "")

    return {
        "id": default_id,
        "api_key": api_key,
        "base_url": base_url,
        "model": model,
        "openai_compatible": prov.get("openai_compatible", True),
    }


# ── Runtime info — handles CopilotKit SDK handshake ──────────────

_INFO_RESPONSE = {
    "actions": [
        {"name": "search_leads", "description": "Search leads in the database"},
        {"name": "get_lead_stats", "description": "Get pipeline statistics"},
        {"name": "update_lead_status", "description": "Update a lead's status"},
        {"name": "start_collection", "description": "Start collecting new leads"},
    ],
}


@router.get("/info")
def copilot_info_get():
    """Runtime info — GET handler."""
    return JSONResponse(content=_INFO_RESPONSE)


@router.post("/info")
async def copilot_info_post():
    """Runtime info — POST handler (CopilotKit SDK sends POST)."""
    return JSONResponse(content=_INFO_RESPONSE)


# ── Conversation endpoints ───────────────────────────────────────


@router.get("/conversations")
def list_conversations():
    """List all chat conversations."""
    convs = chat_history.list_conversations()
    return JSONResponse(content={"conversations": convs})


@router.get("/conversations/{conv_id}")
def get_conversation(conv_id: str):
    """Get messages for a conversation."""
    conv = chat_history.get_conversation(conv_id)
    if not conv:
        return JSONResponse(content={"error": "Not found"}, status_code=404)
    messages = chat_history.get_messages(conv_id)
    return JSONResponse(content={"conversation": conv, "messages": messages})


@router.delete("/conversations/{conv_id}")
def delete_conversation(conv_id: str):
    """Delete a conversation."""
    chat_history.delete_conversation(conv_id)
    return JSONResponse(content={"ok": True})


@router.get("/memories")
def list_memories():
    """List all stored memories (debug/transparency)."""
    memories = memory.get_all_memories()
    return JSONResponse(content={
        "memories": memories,
        "available": memory.is_available(),
    })


# ── System prompt ────────────────────────────────────────────────

SYSTEM_PROMPT = """You are LeadEngine AI, an expert sales intelligence assistant.

You help users:
- Find and analyze business leads
- Update lead statuses and information
- Plan outreach campaigns
- Score and qualify prospects
- Export and manage lead data

You have access to tools that let you interact with the lead database directly.
When users ask about leads, use your tools to fetch real data.
Be concise, data-driven, and actionable in your responses.

Current context will be provided by the application state (visible leads, filters, selected rows).
"""


# ── Server-side tool definitions ─────────────────────────────────

def _build_tools():
    """Define the backend tools available to the copilot."""
    return [
        {
            "type": "function",
            "function": {
                "name": "search_leads",
                "description": "Search for leads in the database by company name, city, status, or any keyword",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "query": {"type": "string", "description": "Search query (company name, keyword)"},
                        "city": {"type": "string", "description": "Filter by city"},
                        "status": {"type": "string", "description": "Filter by status: new, contacted, qualified, dead"},
                        "limit": {"type": "integer", "description": "Max results to return", "default": 10},
                    },
                    "required": ["query"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "get_lead_stats",
                "description": "Get summary statistics about all leads (total count, by tier, by channel)",
                "parameters": {"type": "object", "properties": {}},
            },
        },
        {
            "type": "function",
            "function": {
                "name": "update_lead_status",
                "description": "Update the status of a lead by ID",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "lead_id": {"type": "integer", "description": "The lead ID"},
                        "status": {"type": "string", "enum": ["new", "contacted", "qualified", "dead"]},
                        "note": {"type": "string", "description": "Optional note about the status change"},
                    },
                    "required": ["lead_id", "status"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "start_collection",
                "description": "Start collecting new leads for a given search query (e.g. 'IT staffing companies in Pune')",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "query": {"type": "string", "description": "The lead collection query"},
                    },
                    "required": ["query"],
                },
            },
        },
    ]


def _execute_tool(name: str, args: dict) -> str:
    """Execute a backend tool and return the result as a string."""
    db = LeadDB()
    try:
        if name == "search_leads":
            leads = db.get_leads(
                search=args.get("query"),
                city=args.get("city"),
                status=args.get("status"),
                limit=args.get("limit", 10),
            )
            result = [
                {
                    "id": l.id, "company": l.company, "city": l.city,
                    "email": l.email, "phone": l.phone, "score": l.score,
                    "tier": l.score_tier, "status": l.status,
                    "website": l.website,
                }
                for l in leads
            ]
            return json.dumps({"leads": result, "count": len(result)})

        elif name == "get_lead_stats":
            stats = db.get_stats()
            return json.dumps(stats)

        elif name == "update_lead_status":
            db.update_status(args["lead_id"], args["status"], args.get("note", ""))
            return json.dumps({"ok": True, "lead_id": args["lead_id"], "new_status": args["status"]})

        elif name == "start_collection":
            job_id = str(uuid.uuid4())[:8]
            query = args["query"]
            db.create_job(job_id, query)

            def _run():
                from apps.api.services.leadgen.job_runner import JobRunner
                runner = JobRunner()
                asyncio.run(runner._process_job({"id": job_id, "query": query, "tier": 1}))

            threading.Thread(target=_run, daemon=True).start()
            return json.dumps({"ok": True, "job_id": job_id, "query": query})

        return json.dumps({"error": f"Unknown tool: {name}"})
    finally:
        db.close()


# ── Chat completion proxy ────────────────────────────────────────

async def _stream_chat(messages: list, tools: list, provider: dict) -> AsyncGenerator[str, None]:
    """Stream chat completion from the configured AI provider."""
    api_key = provider["api_key"]
    base_url = provider["base_url"].rstrip("/")
    model = provider["model"]

    if not api_key:
        yield f'data: {json.dumps({"error": "No AI provider configured. Go to Settings to add an API key."})}\n\n'
        return

    headers = {
        "Authorization": f"Bearer {api_key}",
        "Content-Type": "application/json",
    }

    body = {
        "model": model,
        "messages": messages,
        "tools": tools,
        "stream": True,
        "temperature": 0.7,
    }

    url = f"{base_url}/chat/completions"

    async with httpx.AsyncClient(timeout=60.0) as client:
        async with client.stream("POST", url, json=body, headers=headers) as response:
            if response.status_code != 200:
                error_body = await response.aread()
                yield f'data: {json.dumps({"error": f"Provider error {response.status_code}: {error_body.decode()[:200]}"})}\n\n'
                return

            accumulated_tool_calls = {}

            async for line in response.aiter_lines():
                if not line.startswith("data: "):
                    continue
                data = line[6:]
                if data == "[DONE]":
                    yield "data: [DONE]\n\n"
                    return

                try:
                    chunk = json.loads(data)
                    delta = chunk.get("choices", [{}])[0].get("delta", {})

                    # Handle tool calls
                    if "tool_calls" in delta:
                        for tc in delta["tool_calls"]:
                            idx = tc.get("index", 0)
                            if idx not in accumulated_tool_calls:
                                accumulated_tool_calls[idx] = {
                                    "id": tc.get("id", ""),
                                    "type": "function",
                                    "function": {"name": "", "arguments": ""},
                                }
                            if "id" in tc and tc["id"]:
                                accumulated_tool_calls[idx]["id"] = tc["id"]
                            func = tc.get("function", {})
                            if "name" in func:
                                accumulated_tool_calls[idx]["function"]["name"] += func["name"]
                            if "arguments" in func:
                                accumulated_tool_calls[idx]["function"]["arguments"] += func["arguments"]

                    # Forward content chunks to client
                    if delta.get("content"):
                        yield f"data: {json.dumps({'content': delta['content']})}\n\n"

                    # Detect finish_reason for tool calls
                    finish = chunk.get("choices", [{}])[0].get("finish_reason")
                    if finish == "tool_calls" and accumulated_tool_calls:
                        tool_results = []
                        for idx in sorted(accumulated_tool_calls.keys()):
                            tc = accumulated_tool_calls[idx]
                            fn_name = tc["function"]["name"]
                            try:
                                fn_args = json.loads(tc["function"]["arguments"])
                            except json.JSONDecodeError:
                                fn_args = {}

                            yield f"data: {json.dumps({'tool_call': {'name': fn_name, 'args': fn_args}})}\n\n"
                            result = _execute_tool(fn_name, fn_args)
                            yield f"data: {json.dumps({'tool_result': {'name': fn_name, 'result': json.loads(result)}})}\n\n"

                            tool_results.append({
                                "role": "tool",
                                "tool_call_id": tc["id"],
                                "content": result,
                            })

                        follow_up = messages + [
                            {"role": "assistant", "tool_calls": list(accumulated_tool_calls.values())},
                            *tool_results,
                        ]

                        async for chunk_line in _stream_chat(follow_up, tools, provider):
                            yield chunk_line
                        return

                except json.JSONDecodeError:
                    continue


# ── Main chat endpoint ───────────────────────────────────────────

@router.post("")
async def copilot_chat(request: Request):
    """Main CopilotKit chat endpoint — streams AI responses with tool use.

    Accepts:
        messages: list of {role, content}
        conversation_id: optional, to continue an existing conversation
        context: optional application context
    """
    body = await request.json()

    user_messages = body.get("messages", [])
    context = body.get("context", "")
    conv_id = body.get("conversation_id")

    # Get the last user message for memory operations
    last_user_msg = ""
    for m in reversed(user_messages):
        if m.get("role") == "user":
            last_user_msg = m.get("content", "")
            break

    # Create or continue conversation
    if not conv_id:
        title = chat_history.auto_title_from_message(last_user_msg)
        conv = chat_history.create_conversation(title)
        conv_id = conv["id"]

    # Store user message in history
    if last_user_msg:
        chat_history.add_message(conv_id, "user", last_user_msg)

    # Search memory for relevant context
    memory_context = ""
    if last_user_msg and memory.is_available():
        memories = memory.search_memory(last_user_msg, user_id="default", limit=5)
        if memories:
            memory_texts = []
            for m in memories:
                if isinstance(m, dict):
                    memory_texts.append(m.get("memory", m.get("text", str(m))))
                else:
                    memory_texts.append(str(m))
            if memory_texts:
                memory_context = "Relevant memories from past conversations:\n" + "\n".join(f"- {t}" for t in memory_texts)

    provider = _get_active_provider()
    tools = _build_tools()

    messages = [{"role": "system", "content": SYSTEM_PROMPT}]

    if memory_context:
        messages.append({
            "role": "system",
            "content": memory_context,
        })

    if context:
        messages.append({
            "role": "system",
            "content": f"Current application context:\n{context}",
        })

    messages.extend(user_messages)

    # Collect the full response for storage
    full_response = []

    async def _stream_with_storage():
        """Wrap the stream to capture the full response."""
        nonlocal full_response

        # Send conversation_id first
        yield f"data: {json.dumps({'conversation_id': conv_id})}\n\n"

        async for chunk in _stream_chat(messages, tools, provider):
            yield chunk

            # Parse content from the chunk for storage
            if chunk.startswith("data: ") and chunk.strip() != "data: [DONE]":
                try:
                    data = json.loads(chunk[6:])
                    if "content" in data:
                        full_response.append(data["content"])
                except (json.JSONDecodeError, KeyError):
                    pass

        # After stream completes, store the assistant response and extract memories
        response_text = "".join(full_response)
        if response_text:
            chat_history.add_message(conv_id, "assistant", response_text)

            # Extract and store memories from the conversation
            if memory.is_available() and last_user_msg:
                try:
                    # Store the exchange as episodic memory
                    memory.add_memory(
                        f"User asked: {last_user_msg[:200]}\nAssistant answered about: {response_text[:200]}",
                        user_id="default",
                        metadata={"conversation_id": conv_id, "type": "episodic"},
                    )
                except Exception:
                    pass  # Memory is best-effort

    return StreamingResponse(
        _stream_with_storage(),
        media_type="text/event-stream",
        headers={
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
            "X-Accel-Buffering": "no",
        },
    )
