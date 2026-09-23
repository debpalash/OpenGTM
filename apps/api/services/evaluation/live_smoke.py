"""Exercise the actual HTTP Chat journey and read back its saved workbook.

This is a live smoke test, not the G1–G7 release gauntlet. It records observable
outcomes without attesting to source accuracy, provider health, or blocked sends
on the server. No fixture or model-generated summary can supply passing evidence.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import subprocess
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import quote, urlsplit

import httpx


class SmokeFailure(Exception):
    """A failed observable contract; messages must not contain credentials."""


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _ids(people: list[dict]) -> list[str]:
    ids = [person.get("person_id") for person in people]
    if not ids or any(not isinstance(value, str) or not value for value in ids):
        raise SmokeFailure("missing_person_identity")
    if len(ids) != len(set(ids)):
        raise SmokeFailure("duplicate_person_identity")
    return sorted(ids)


class LiveSmoke:
    def __init__(self, client: httpx.AsyncClient, workspace_id: str, *, turn_timeout: float = 180):
        self.client = client
        self.workspace_id = workspace_id
        self.turn_timeout = turn_timeout
        self.conversation_id = None
        self.report = {
            "schema_version": "1.0", "run_id": str(uuid.uuid4()),
            "validation_tier": "live_smoke", "mode": "live",
            "started_at": _now(), "workspace_id": workspace_id,
            "release_eligible": False, "production_run_history": [],
            "checks": [], "turns": [], "status": "running",
            "limitations": [
                "Not a full G1–G7 gauntlet run; cannot count toward release.",
                "Source accuracy requires independent review of the captured people.",
                "Only expected workbook/contact actions are approved by this client; server send blocking is not attested.",
                "Configured credentials do not prove provider health or deliverability.",
            ],
        }

    def check(self, name: str, passed: bool) -> None:
        self.report["checks"].append({"name": name, "passed": bool(passed)})
        if not passed:
            raise SmokeFailure(name)

    async def get(self, path: str) -> dict:
        response = await self.client.get(path)
        if response.status_code != 200:
            raise SmokeFailure(f"http_{response.status_code}:{path}")
        value = response.json()
        if not isinstance(value, dict):
            raise SmokeFailure("invalid_json_response")
        return value

    async def preflight(self) -> None:
        health = await self.get("/health")
        self.check("api_healthy", health.get("status") == "healthy")
        context = await self.get("/api/me/context")
        self.check("workspace_binding", context.get("workspace_id") == self.workspace_id)
        self.check("workspace_can_write", context.get("role") in {"owner", "admin", "editor"})
        workspaces = await self.get("/api/workspaces")
        self.check("dedicated_workspace", any(
            workspace.get("id") == self.workspace_id and workspace.get("slug") == "gtm-release-eval"
            for workspace in workspaces.get("workspaces", [])
        ))
        self.report["flags"] = await self.get("/api/flags")

    async def turn(self, prompt: str, *, approval: dict | None = None) -> list[dict]:
        body = {"messages": [{"role": "user", "content": prompt}]}
        if self.conversation_id:
            body["conversation_id"] = self.conversation_id
        if approval:
            body["approved_tool_calls"] = [{
                "tool_call": approval["tool_call"], "decision": "approve",
            }]
        started = time.monotonic()
        entry = {"prompt": prompt, "events": [], "started_at": _now()}
        self.report["turns"].append(entry)
        done = False
        size = 0
        async with asyncio.timeout(self.turn_timeout):
            async with self.client.stream("POST", "/api/copilotkit", json=body) as response:
                if response.status_code != 200:
                    raise SmokeFailure(f"chat_http_{response.status_code}")
                async for line in response.aiter_lines():
                    size += len(line)
                    if size > 2_000_000:
                        raise SmokeFailure("chat_stream_size_limit")
                    if not line.startswith("data:"):
                        continue
                    payload = line[5:].strip()
                    if payload == "[DONE]":
                        done = True
                        break
                    event = json.loads(payload)
                    if not isinstance(event, dict):
                        raise SmokeFailure("invalid_chat_event")
                    if "acknowledgement_ms" not in entry:
                        entry["acknowledgement_ms"] = round((time.monotonic() - started) * 1000)
                    if event.get("error"):
                        raise SmokeFailure("chat_error")
                    if event.get("conversation_id"):
                        conv_id = event["conversation_id"]
                        if self.conversation_id and self.conversation_id != conv_id:
                            raise SmokeFailure("conversation_identity_changed")
                        self.conversation_id = conv_id
                        self.report["conversation_id"] = conv_id
                    # Prose is not evaluation evidence. Only retain structured events.
                    selected = {key: event[key] for key in (
                        "tool_call", "tool_result", "confirmation_required", "tool_denied",
                    ) if key in event}
                    if selected:
                        entry["events"].append(selected)
                    if len(entry["events"]) > 1000:
                        raise SmokeFailure("chat_event_limit")
        entry["latency_ms"] = round((time.monotonic() - started) * 1000)
        if not done:
            raise SmokeFailure("chat_stream_incomplete")
        return entry["events"]

    def result(self, events: list[dict], name: str) -> dict:
        results = [event["tool_result"].get("result") for event in events
                   if event.get("tool_result", {}).get("name") == name]
        self.check(f"{name}:one_result", len(results) == 1 and isinstance(results[0], dict))
        result = results[0]
        self.check(f"{name}:ok", result.get("ok") is True)
        return result

    async def action(self, prompt: str, name: str, person_ids: list[str]) -> dict:
        events = await self.turn(prompt)
        proposals = [event["confirmation_required"] for event in events if "confirmation_required" in event]
        self.check(f"{name}:one_proposal", len(proposals) == 1)
        proposal = proposals[0]
        call = proposal.get("tool_call", {})
        function = call.get("function", {})
        args = json.loads(function.get("arguments") or "{}")
        # Approve only the exact saved selection; no sends, exports, or sourcing.
        self.check(f"{name}:expected_action", name in {"create_people_workbook", "enrich_people_contacts"}
                   and proposal.get("name") == name and function.get("name") == name)
        self.check(f"{name}:bound_conversation", args.get("conversation_id") == self.conversation_id)
        self.check(f"{name}:exact_selection", sorted(args.get("person_ids") or []) == person_ids)
        self.check(f"{name}:idempotency_key", bool(args.get("idempotency_key")))
        if "action_keys" not in self.report:
            self.report["action_keys"] = {}
        previous = self.report["action_keys"].get(name)
        self.check(f"{name}:stable_retry_key", previous is None or previous == args["idempotency_key"])
        self.report["action_keys"][name] = args["idempotency_key"]
        events = await self.turn(prompt, approval=proposal)
        self.check(f"{name}:no_additional_approval", not any("confirmation_required" in event for event in events))
        return self.result(events, name)

    async def run(self, *, execute: bool, company: str, paid_contacts: bool = False) -> dict:
        try:
            await self.preflight()
            if not execute:
                self.report["status"] = "preflight_passed"
                return self.report
            prompt = f"Find named people currently working on {company}'s partnership team. Require evidence for current employment and their partnership remit."
            research = self.result(await self.turn(prompt), "find_people_at_company")
            people_ids = _ids(research.get("people") or [])
            self.report["research"] = research
            verified = self.result(await self.turn("verify them"), "verify_people_at_company")
            self.report["verification"] = verified
            self.check("verification_preserves_selection", _ids(verified.get("people") or []) == people_ids)
            if paid_contacts:
                contacts = await self.action("find their work emails", "enrich_people_contacts", people_ids)
                self.report["contacts"] = contacts
                self.check("contacts_preserve_selection", _ids(contacts.get("people") or []) == people_ids)
            saved = await self.action("make a workbook with them", "create_people_workbook", people_ids)
            retry = await self.action("make a workbook with them", "create_people_workbook", people_ids)
            workbook_id = saved.get("workbook_id")
            self.check("workbook_persisted_id", isinstance(workbook_id, str) and bool(workbook_id))
            self.check("retry_reuses_workbook", retry.get("workbook_id") == workbook_id and retry.get("reused") is True)
            self.check("exact_workbook_link", saved.get("url") == f"/workbooks/{workbook_id}")
            snapshot = await self.get(f"/api/workbooks/{quote(workbook_id, safe='')}?page_size=100")
            self.report["workbook"] = snapshot
            self.check("readback_workbook_identity", snapshot.get("workbook", {}).get("id") == workbook_id)
            self.check("readback_exact_people", _ids([row.get("data") or {} for row in snapshot.get("rows", [])]) == people_ids)
            self.check("readback_row_count", snapshot.get("total_rows") == len(people_ids))
            self.report["status"] = "passed"
        except (SmokeFailure, httpx.HTTPError, TimeoutError, ValueError, TypeError, KeyError) as exc:
            self.report["status"] = "failed"
            # Transport/provider exception strings can include tokens or URLs.
            self.report["failure"] = str(exc) if isinstance(exc, SmokeFailure) else type(exc).__name__
        finally:
            self.report["finished_at"] = _now()
        return self.report


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default="http://127.0.0.1:8000")
    parser.add_argument("--workspace-id", required=True)
    parser.add_argument("--company", default="PayPal")
    parser.add_argument("--execute", action="store_true", help="Run real research and create/retry one people workbook")
    parser.add_argument("--paid-contacts", action="store_true", help="Also approve exact contact lookup using configured paid providers")
    parser.add_argument("--output", type=Path, required=True, help="New JSON evidence file; existing files are never overwritten")
    args = parser.parse_args(argv)
    url = urlsplit(args.base_url)
    if url.scheme not in {"http", "https"} or not url.hostname or url.username or url.password or url.query or url.fragment:
        parser.error("base URL must be HTTP(S), without credentials, query, or fragment")
    if url.scheme == "http" and url.hostname not in {"localhost", "127.0.0.1", "::1"}:
        parser.error("remote API connections require HTTPS")
    token = os.environ.get("OPENGTM_EVAL_TOKEN", "")
    if not token:
        parser.error("set OPENGTM_EVAL_TOKEN to a token for the dedicated evaluation workspace")
    if args.paid_contacts and not args.execute:
        parser.error("--paid-contacts requires --execute")
    # Reserve the output before mutations; an existing report cannot be lost.
    try:
        descriptor = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    except OSError as exc:
        parser.error(f"cannot create output: {type(exc).__name__}")

    async def execute() -> dict:
        async with httpx.AsyncClient(
            base_url=args.base_url.rstrip("/"), timeout=30,
            headers={"Authorization": f"Bearer {token}", "X-Workspace-Id": args.workspace_id},
            follow_redirects=False,
        ) as client:
            return await LiveSmoke(client, args.workspace_id).run(
                execute=args.execute, company=args.company, paid_contacts=args.paid_contacts,
            )

    with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
        report = asyncio.run(execute())
        try:
            report["build_sha"] = subprocess.check_output(
                ["git", "rev-parse", "HEAD"], text=True, stderr=subprocess.DEVNULL,
            ).strip()
            report["working_tree_dirty"] = bool(subprocess.check_output(
                ["git", "status", "--porcelain"], text=True, stderr=subprocess.DEVNULL,
            ).strip())
        except (OSError, subprocess.CalledProcessError):
            report["build_sha"] = "unknown"
        json.dump(report, handle, indent=2)
        handle.write("\n")
    print(f"{report['status']}: {args.output}")
    return 0 if report["status"] in {"passed", "preflight_passed"} else 1


if __name__ == "__main__":
    raise SystemExit(main())
