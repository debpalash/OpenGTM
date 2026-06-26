"""
Yupcha MCP Server — Expose GTM tools to Claude Desktop, Cursor, and Windsurf.

Usage:
  python -m apps.mcp.server          # stdio mode (Claude Desktop)
  python -m apps.mcp.server --sse    # SSE/HTTP mode (Cursor/Windsurf), loopback only

Security model (Phase 1 — security hardening):
  * The MCP surface is now AUTHENTICATED and TENANT-SCOPED. Every tool call
    resolves a single workspace via a scoped MCP token and runs through the same
    RLS-protected stores as the REST API — no tool can read across tenants.
  * stdio presents the token via the ``YUPCHA_MCP_TOKEN`` env var.
  * SSE/HTTP presents it via ``Authorization: Bearer <token>`` and binds to
    127.0.0.1 ONLY (the previous unauthenticated 0.0.0.0 bind was a live
    cross-tenant exposure and has been removed). For remote access, front it
    with a reverse proxy that forwards the bearer token.
  * Self-host (SQLite / ``MCP_REQUIRE_AUTH`` false) stays keyless and binds to
    the ``main`` workspace.

Claude Desktop config (~/.claude/claude_desktop_config.json):
  {
    "mcpServers": {
      "yupcha": {
        "command": "python",
        "args": ["-m", "apps.mcp.server"],
        "cwd": "/path/to/lead-data",
        "env": {"YUPCHA_MCP_TOKEN": "ycp_..."}
      }
    }
  }
"""

import os
import sys
import json
import asyncio
import logging
from typing import Any, Optional

from apps.api.services.mcp import tools as mcp_tools
from apps.api.services.mcp.auth import MCPAuthError, MCPCtx, resolve_mcp_token

logger = logging.getLogger("mcp.server")

# MCP protocol types
JSONRPC_VERSION = "2.0"
MCP_PROTOCOL_VERSION = "2024-11-05"


def _error_response(msg_id: Any, code: int, message: str) -> dict:
    return {"jsonrpc": JSONRPC_VERSION, "id": msg_id, "error": {"code": code, "message": message}}


# ── MCP message handling (auth-scoped) ───────────────────────────────────────

async def handle_message(msg: dict, ctx: MCPCtx) -> Optional[dict]:
    """Handle a JSON-RPC message for an already-authenticated context.

    ``ctx`` is the resolved :class:`MCPCtx` for the session — the transport layer
    authenticates BEFORE calling this, so an unauthenticated caller never reaches
    a tool. ``tools/list`` is filtered to the token's capabilities and every
    ``tools/call`` runs scoped to ``ctx.workspace_id``.
    """
    method = msg.get("method", "")
    msg_id = msg.get("id")
    params = msg.get("params", {})

    if method == "initialize":
        return {
            "jsonrpc": JSONRPC_VERSION,
            "id": msg_id,
            "result": {
                "protocolVersion": MCP_PROTOCOL_VERSION,
                "capabilities": {"tools": {"listChanged": False}},
                "serverInfo": {"name": "yupcha", "version": "3.0.0"},
            },
        }

    if method == "notifications/initialized":
        return None  # No response for notifications

    if method == "tools/list":
        return {
            "jsonrpc": JSONRPC_VERSION,
            "id": msg_id,
            "result": {"tools": mcp_tools.list_tools(ctx)},
        }

    if method == "tools/call":
        tool_name = params.get("name", "")
        tool_args = params.get("arguments", {})
        result_text = await mcp_tools.execute_tool(ctx, tool_name, tool_args)
        return {
            "jsonrpc": JSONRPC_VERSION,
            "id": msg_id,
            "result": {
                "content": [{"type": "text", "text": result_text}],
                "isError": False,
            },
        }

    if method == "ping":
        return {"jsonrpc": JSONRPC_VERSION, "id": msg_id, "result": {}}

    return _error_response(msg_id, -32601, f"Method not found: {method}")


# ── stdio transport ──────────────────────────────────────────────────────────

async def run_stdio():
    """Run MCP server over stdio (for Claude Desktop)."""
    logger.info("Yupcha MCP Server starting (stdio mode)")

    # Authenticate ONCE at startup from the env token. In cloud this fails closed:
    # no/invalid token → refuse to start (no unauthenticated tool executor).
    try:
        ctx = resolve_mcp_token(os.environ.get("YUPCHA_MCP_TOKEN"))
    except MCPAuthError as e:
        logger.error("MCP stdio refused: %s. Set YUPCHA_MCP_TOKEN to a valid token.", e)
        sys.exit(1)

    reader = asyncio.StreamReader()
    protocol = asyncio.StreamReaderProtocol(reader)
    await asyncio.get_event_loop().connect_read_pipe(lambda: protocol, sys.stdin.buffer)

    writer_transport, writer_protocol = await asyncio.get_event_loop().connect_write_pipe(
        asyncio.streams.FlowControlMixin, sys.stdout.buffer
    )
    writer = asyncio.StreamWriter(writer_transport, writer_protocol, None, asyncio.get_event_loop())

    while True:
        try:
            header = await reader.readline()
            if not header:
                break

            header_str = header.decode("utf-8").strip()
            if header_str.startswith("Content-Length:"):
                content_length = int(header_str.split(":")[1].strip())
                await reader.readline()  # Empty line
                body = await reader.readexactly(content_length)
                msg = json.loads(body.decode("utf-8"))

                response = await handle_message(msg, ctx)
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


# ── SSE/HTTP transport (loopback + bearer) ───────────────────────────────────

def build_sse_app():
    """Build the FastAPI SSE app. Auth is enforced per-request on ``/message``."""
    from fastapi import FastAPI, Request
    from fastapi.responses import JSONResponse, StreamingResponse

    sse_app = FastAPI(title="Yupcha MCP SSE")

    @sse_app.get("/sse")
    async def sse_endpoint():
        async def event_stream():
            yield f"data: {json.dumps({'type': 'endpoint', 'url': '/message'})}\n\n"
        return StreamingResponse(event_stream(), media_type="text/event-stream")

    @sse_app.post("/message")
    async def message_endpoint(request: Request):
        # Authenticate EVERY request from the bearer header. Fail closed: an
        # unauthenticated call never reaches a tool (this closes the previous
        # open executor).
        auth_header = request.headers.get("Authorization", "")
        scheme, _, raw = auth_header.partition(" ")
        token = raw if scheme.lower() == "bearer" else None
        try:
            ctx = resolve_mcp_token(token)
        except MCPAuthError as e:
            return JSONResponse(
                status_code=401,
                content={"error": "unauthorized", "detail": str(e)},
                headers={"WWW-Authenticate": "Bearer"},
            )
        msg = await request.json()
        return await handle_message(msg, ctx)

    return sse_app


def main():
    """Entry point for MCP server."""
    logging.basicConfig(level=logging.INFO)

    if "--sse" in sys.argv:
        try:
            import uvicorn
        except ImportError:
            print("SSE mode requires FastAPI + uvicorn: pip install fastapi uvicorn")
            sys.exit(1)

        sse_app = build_sse_app()
        idx = sys.argv.index("--sse")
        port = int(sys.argv[idx + 1]) if len(sys.argv) > idx + 1 and sys.argv[idx + 1].isdigit() else 3100
        # Loopback ONLY. The previous 0.0.0.0 bind exposed an unauthenticated
        # tool executor to the whole network — do NOT reintroduce it. For remote
        # access, put a reverse proxy in front that forwards the bearer token.
        host = os.environ.get("YUPCHA_MCP_SSE_HOST", "127.0.0.1")
        print(f"Yupcha MCP SSE server on http://{host}:{port} (loopback, bearer-auth)")
        uvicorn.run(sse_app, host=host, port=port, log_level="warning")
    else:
        asyncio.run(run_stdio())


if __name__ == "__main__":
    main()
