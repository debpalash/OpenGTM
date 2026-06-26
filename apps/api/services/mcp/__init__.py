"""MCP server-side support: scoped tokens, per-call auth, audit, RLS-scoped tools.

This package is the trusted server-side half of the MCP bridge (``apps/mcp/server.py``).
It mirrors the REST tenancy model so every MCP tool call resolves a workspace and
runs through the same RLS-scoped stores as the REST API — no tool may read across
tenants.
"""
