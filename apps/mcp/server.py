"""
Yupcha MCP Server — Expose GTM tools to Claude Desktop, Cursor, and Windsurf.

Usage:
  python -m apps.mcp.server          # stdio mode (Claude Desktop)
  python -m apps.mcp.server --sse    # SSE mode (Cursor/Windsurf)

Claude Desktop config (~/.claude/claude_desktop_config.json):
  {
    "mcpServers": {
      "yupcha": {
        "command": "python",
        "args": ["-m", "apps.mcp.server"],
        "cwd": "/path/to/lead-data"
      }
    }
  }
"""

import sys
import json
import asyncio
import logging
from typing import Any

logger = logging.getLogger("mcp.server")

# MCP protocol types
JSONRPC_VERSION = "2.0"
MCP_PROTOCOL_VERSION = "2024-11-05"

# ── Tool Definitions ──────────────────────────────────────────

TOOLS = [
    {
        "name": "find_leads",
        "description": "Search for leads in the Yupcha database by company, city, industry, tier, or keyword. Returns matching leads with contact info and scores.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "query": {"type": "string", "description": "Search query (company name, keyword, industry)"},
                "city": {"type": "string", "description": "Filter by city"},
                "tier": {"type": "string", "description": "Filter by score tier: hot, warm, cold"},
                "limit": {"type": "integer", "description": "Max results (default: 10)", "default": 10},
            },
            "required": ["query"],
        },
    },
    {
        "name": "get_lead_detail",
        "description": "Get complete profile of a specific lead by ID — company info, contacts, enrichment data, scores, decision makers.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "lead_id": {"type": "integer", "description": "The lead ID"},
            },
            "required": ["lead_id"],
        },
    },
    {
        "name": "get_pipeline_stats",
        "description": "Get GTM pipeline statistics: total leads, breakdown by tier/status/city, enrichment coverage rates.",
        "inputSchema": {
            "type": "object",
            "properties": {},
        },
    },
    {
        "name": "enrich_company",
        "description": "Trigger on-demand enrichment for a lead — scrape website, find emails, social profiles, decision makers.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "lead_id": {"type": "integer", "description": "The lead ID to enrich"},
            },
            "required": ["lead_id"],
        },
    },
    {
        "name": "verify_email",
        "description": "Verify an email address using SMTP RCPT TO validation. Returns deliverable/undeliverable status.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "email": {"type": "string", "description": "Email address to verify"},
            },
            "required": ["email"],
        },
    },
    {
        "name": "create_workbook",
        "description": "Create a new workbook from a description. Auto-generates columns. Example: 'SaaS CTOs in SF with email and LinkedIn'",
        "inputSchema": {
            "type": "object",
            "properties": {
                "description": {"type": "string", "description": "Natural language description of the workbook"},
                "name": {"type": "string", "description": "Optional name for the workbook"},
            },
            "required": ["description"],
        },
    },
    {
        "name": "get_hiring_signals",
        "description": "Check for active job postings at a company. Returns job titles, counts, and hiring velocity as buying signals.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "company": {"type": "string", "description": "Company name to check"},
                "lead_id": {"type": "integer", "description": "Optional lead ID for enriched context"},
            },
            "required": ["company"],
        },
    },
    {
        "name": "score_lead",
        "description": "Calculate a lead quality score (0-100) based on data completeness, company signals, and contact depth.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "lead_id": {"type": "integer", "description": "The lead ID to score"},
            },
            "required": ["lead_id"],
        },
    },
]


# ── Tool Execution ────────────────────────────────────────────

async def execute_tool(name: str, arguments: dict) -> str:
    """Execute an MCP tool and return the result as text."""
    try:
        if name == "find_leads":
            from apps.api.services.leadgen.db import LeadDB
            db = LeadDB()
            leads = db.get_leads(
                search=arguments.get("query"),
                city=arguments.get("city"),
                score_tier=arguments.get("tier"),
                limit=arguments.get("limit", 10),
            )
            result = [
                {
                    "id": l.id, "company": l.company, "city": l.city,
                    "email": l.email, "phone": l.phone, "score": l.score,
                    "tier": l.score_tier, "status": l.status,
                    "website": l.website, "specialization": l.specialization,
                    "company_size": l.company_size, "contact_person": l.contact_person,
                }
                for l in leads
            ]
            db.close()
            return json.dumps({"leads": result, "count": len(result)}, indent=2)

        elif name == "get_lead_detail":
            from apps.api.services.leadgen.db import LeadDB
            db = LeadDB()
            lead = db.get_lead(arguments["lead_id"])
            db.close()
            if not lead:
                return json.dumps({"error": "Lead not found"})
            d = lead.to_dict()
            if d.get("decision_makers"):
                try:
                    d["decision_makers"] = json.loads(d["decision_makers"])
                except Exception:
                    pass
            return json.dumps(d, indent=2, default=str)

        elif name == "get_pipeline_stats":
            from apps.api.services.leadgen.db import LeadDB
            db = LeadDB()
            stats = db.get_stats()
            db.close()
            return json.dumps(stats, indent=2)

        elif name == "enrich_company":
            from apps.api.services.leadgen.db import LeadDB
            from apps.api.services.leadgen.enrichment.provider import WaterfallEnricher
            db = LeadDB()
            lead = db.get_lead(arguments["lead_id"])
            if not lead:
                db.close()
                return json.dumps({"error": "Lead not found"})
            enricher = WaterfallEnricher()
            result = await enricher.enrich(lead)
            db.close()
            return json.dumps({"enriched_fields": result.fields, "provider": result.provider}, indent=2)

        elif name == "verify_email":
            email = arguments["email"]
            try:
                from apps.api.services.leadgen.enrichment.providers.mailscout_verify import MailScoutVerifyProvider
                provider = MailScoutVerifyProvider()
                # Create a mock lead with the email
                from apps.api.services.leadgen.models import Lead
                mock_lead = Lead(email=email, company="")
                result = await provider.enrich(mock_lead)
                return json.dumps({
                    "email": email,
                    "valid": result.success,
                    "confidence": result.confidence,
                    "fields": result.fields,
                }, indent=2)
            except Exception as e:
                return json.dumps({"email": email, "error": str(e)})

        elif name == "create_workbook":
            # Reuse the copilot tool implementation
            import uuid
            description = arguments["description"]
            wb_name = arguments.get("name", f"MCP — {description[:40]}")
            desc_lower = description.lower()

            column_defs = [{"key": "company", "name": "Company", "type": "text"}]
            if any(w in desc_lower for w in ["email", "contact"]):
                column_defs.append({"key": "email", "name": "Email", "type": "email"})
            if any(w in desc_lower for w in ["phone", "call"]):
                column_defs.append({"key": "phone", "name": "Phone", "type": "phone"})
            if any(w in desc_lower for w in ["linkedin", "social"]):
                column_defs.append({"key": "linkedin_url", "name": "LinkedIn", "type": "url"})
            if any(w in desc_lower for w in ["title", "cto", "ceo", "vp", "founder"]):
                column_defs.append({"key": "contact_person", "name": "Contact", "type": "text"})
                column_defs.append({"key": "contact_title", "name": "Title", "type": "text"})
            if any(w in desc_lower for w in ["city", "location"]):
                column_defs.append({"key": "city", "name": "City", "type": "text"})
            keys = [c["key"] for c in column_defs]
            if "email" not in keys:
                column_defs.append({"key": "email", "name": "Email", "type": "email"})
            if "contact_person" not in keys:
                column_defs.append({"key": "contact_person", "name": "Contact", "type": "text"})

            return json.dumps({
                "name": wb_name,
                "columns": [c["name"] for c in column_defs],
                "message": f"Workbook '{wb_name}' ready with {len(column_defs)} columns.",
            }, indent=2)

        elif name == "get_hiring_signals":
            company = arguments["company"]
            try:
                from apps.api.services.leadgen.enrichment.providers.jobspy_signals import JobSpySignalProvider
                from apps.api.services.leadgen.models import Lead
                provider = JobSpySignalProvider()
                mock_lead = Lead(company=company, id=arguments.get("lead_id", 0))
                result = await provider.enrich(mock_lead)
                return json.dumps({
                    "company": company,
                    "hiring": result.success,
                    "data": result.fields,
                }, indent=2, default=str)
            except Exception as e:
                return json.dumps({"company": company, "error": str(e)})

        elif name == "score_lead":
            from apps.api.services.leadgen.db import LeadDB
            from apps.api.services.leadgen.scoring import score_lead
            db = LeadDB()
            lead = db.get_lead(arguments["lead_id"])
            db.close()
            if not lead:
                return json.dumps({"error": "Lead not found"})
            score_result = score_lead(lead)
            return json.dumps(score_result, indent=2, default=str)

        return json.dumps({"error": f"Unknown tool: {name}"})

    except Exception as e:
        return json.dumps({"error": str(e)})


# ── MCP Server (stdio) ───────────────────────────────────────

async def handle_message(msg: dict) -> dict:
    """Handle a JSON-RPC message."""
    method = msg.get("method", "")
    msg_id = msg.get("id")
    params = msg.get("params", {})

    if method == "initialize":
        return {
            "jsonrpc": JSONRPC_VERSION,
            "id": msg_id,
            "result": {
                "protocolVersion": MCP_PROTOCOL_VERSION,
                "capabilities": {
                    "tools": {"listChanged": False},
                },
                "serverInfo": {
                    "name": "yupcha",
                    "version": "3.0.0",
                },
            },
        }

    elif method == "notifications/initialized":
        return None  # No response for notifications

    elif method == "tools/list":
        return {
            "jsonrpc": JSONRPC_VERSION,
            "id": msg_id,
            "result": {"tools": TOOLS},
        }

    elif method == "tools/call":
        tool_name = params.get("name", "")
        tool_args = params.get("arguments", {})
        result_text = await execute_tool(tool_name, tool_args)
        return {
            "jsonrpc": JSONRPC_VERSION,
            "id": msg_id,
            "result": {
                "content": [{"type": "text", "text": result_text}],
                "isError": False,
            },
        }

    elif method == "ping":
        return {"jsonrpc": JSONRPC_VERSION, "id": msg_id, "result": {}}

    else:
        return {
            "jsonrpc": JSONRPC_VERSION,
            "id": msg_id,
            "error": {"code": -32601, "message": f"Method not found: {method}"},
        }


async def run_stdio():
    """Run MCP server over stdio (for Claude Desktop)."""
    logger.info("Yupcha MCP Server starting (stdio mode)")

    reader = asyncio.StreamReader()
    protocol = asyncio.StreamReaderProtocol(reader)
    await asyncio.get_event_loop().connect_read_pipe(lambda: protocol, sys.stdin.buffer)

    writer_transport, writer_protocol = await asyncio.get_event_loop().connect_write_pipe(
        asyncio.streams.FlowControlMixin, sys.stdout.buffer
    )
    writer = asyncio.StreamWriter(writer_transport, writer_protocol, None, asyncio.get_event_loop())

    while True:
        try:
            # Read Content-Length header
            header = await reader.readline()
            if not header:
                break

            header_str = header.decode("utf-8").strip()
            if header_str.startswith("Content-Length:"):
                content_length = int(header_str.split(":")[1].strip())
                await reader.readline()  # Empty line
                body = await reader.readexactly(content_length)
                msg = json.loads(body.decode("utf-8"))

                response = await handle_message(msg)
                if response:
                    resp_bytes = json.dumps(response).encode("utf-8")
                    header_bytes = f"Content-Length: {len(resp_bytes)}\r\n\r\n".encode("utf-8")
                    writer.write(header_bytes + resp_bytes)
                    await writer.drain()

        except (asyncio.IncompleteReadError, ConnectionResetError):
            break
        except Exception as e:
            logger.error(f"MCP error: {e}")
            continue


def main():
    """Entry point for MCP server."""
    logging.basicConfig(level=logging.INFO)

    if "--sse" in sys.argv:
        # SSE mode for Cursor/Windsurf — requires a separate HTTP server
        try:
            from fastapi import FastAPI
            from fastapi.responses import StreamingResponse
            import uvicorn

            sse_app = FastAPI(title="Yupcha MCP SSE")

            @sse_app.get("/sse")
            async def sse_endpoint():
                async def event_stream():
                    yield f"data: {json.dumps({'type': 'endpoint', 'url': '/message'})}\n\n"
                return StreamingResponse(event_stream(), media_type="text/event-stream")

            @sse_app.post("/message")
            async def message_endpoint(msg: dict):
                response = await handle_message(msg)
                return response

            port = int(sys.argv[sys.argv.index("--sse") + 1]) if len(sys.argv) > sys.argv.index("--sse") + 1 else 3100
            print(f"Yupcha MCP SSE server on http://localhost:{port}")
            uvicorn.run(sse_app, host="0.0.0.0", port=port, log_level="warning")
        except ImportError:
            print("SSE mode requires FastAPI + uvicorn: pip install fastapi uvicorn")
            sys.exit(1)
    else:
        asyncio.run(run_stdio())


if __name__ == "__main__":
    main()
