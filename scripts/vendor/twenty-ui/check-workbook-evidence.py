"""Evidence inspector and live delivery; all business APIs/sockets are fixtures."""
import json
from urllib.parse import urlparse
from playwright.sync_api import expect, sync_playwright

evidence = {"answer": "Confirmed partnerships remit", "citations": [
    {"url": "https://example.com/team", "title": "Team", "quoted_text": "Partnerships team", "fetched_at": "2026-09-22T00:00:00+00:00"},
    {"url": "javascript:alert(1)", "title": "Unsafe fixture", "quoted_text": "<script>not executable</script>"},
], "cost_usd": 0.01, "stopped_reason": "answered", "synthesis_fallback": False}
empty = {**evidence, "answer": "", "citations": [], "stopped_reason": "no_answer"}
rows = [{"row_id": i, "lead_id": None, "data": {"company": f"Company {i}"}, "lead": {"company": f"Company {i}"},
         "enrichments": {"research": {"value": item["answer"], "status": "complete" if i == 1 else "error", "research": item}}}
        for i, item in [(1, evidence), (2, empty)]]
payloads = {
    "/auth/me": {"id": 1, "username": "Fixture", "is_admin": True, "role": "admin"},
    "/api/workspaces": {"workspaces": [{"id": "fixture", "name": "Fixture", "slug": "fixture"}], "active_id": "fixture"},
    "/api/copilotkit/conversations": {"conversations": []},
    "/api/settings/llm-usage": {"providers": [], "today_calls": 0, "today_tokens": 0},
    "/api/jobs": [], "/api/leads": [],
    "/api/workbooks/evidence": {"workbook": {"id": "evidence", "name": "Evidence fixture", "status": "draft", "columns_config": [
        {"id": "company", "name": "Company", "type": "lead_field", "lead_field": "company", "width": 150},
        {"id": "research", "name": "Research", "type": "research", "width": 400}], "total_rows": 2, "completed_rows": 0}, "rows": rows, "total_rows": 2, "page": 1},
    "/api/workbooks/meta/providers": {"providers": []},
    "/api/workbooks/meta/ai-column-presets": {"presets": [], "categories": []},
    "/api/v2/workbooks/evidence/views": {"views": []},
    "/api/workbooks/evidence/connector-runs": {"runs": []},
    "/api/workbooks/evidence/cost": {"budget_max_usd": 0, "budget_spent_usd": 0},
    "/api/workbooks/evidence/run/estimate": {"rows": 2, "best_usd": 0, "worst_usd": 0, "breakdown": [], "note": "Fixture"},
}
unexpected, errors, sockets = [], [], []

def route_request(route):
    path = urlparse(route.request.url).path.rstrip("/")
    if not path.startswith(("/api/", "/auth/")):
        route.continue_()
    elif route.request.method != "GET":
        unexpected.append(f"Mutation: {path}")
        route.abort()
    elif path == "/api/events":
        route.fulfill(content_type="text/event-stream", body=": fixture\n\n")
    else:
        if path not in payloads:
            unexpected.append(path)
        route.fulfill(json=payloads.get(path, {}))

with sync_playwright() as p:
    browser = p.chromium.launch(executable_path="/usr/bin/chromium", headless=True)
    context = browser.new_context(viewport={"width": 1440, "height": 900}, reduced_motion="reduce")
    context.route("**/*", route_request)
    context.route_web_socket("**/*", lambda ws: sockets.append(ws))
    context.add_init_script("localStorage.setItem('yupcha_token','fixture-only'); localStorage.setItem('yupcha_workspace_id','fixture')")
    page = context.new_page()
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.goto("http://127.0.0.1:4399/workbooks/evidence")
    triggers = page.get_by_role("button", name="Inspect research evidence", exact=True)
    expect(triggers).to_have_count(2)
    triggers.first.focus()
    page.keyboard.press("Enter")
    dialog = page.get_by_role("dialog", name="Research evidence", exact=True)
    expect(dialog).to_be_visible()
    expect(dialog.get_by_role("link")).to_have_count(1)
    expect(dialog.get_by_role("link")).to_have_attribute("href", "https://example.com/team")
    expected_date = page.evaluate("new Date('2026-09-22T00:00:00+00:00').toLocaleString()")
    expect(dialog.get_by_text(f"Fetched: {expected_date}", exact=True)).to_be_visible()
    expect(dialog.get_by_text("Fetched: Not recorded", exact=True)).to_be_visible()
    expect(dialog.get_by_text("<script>not executable</script>", exact=True)).to_be_visible()
    for theme in ("light", "dark"):
        page.evaluate("theme => window.OpenGTMTheme.setPreference(theme)", theme)
        for width, height in [(375, 667), (1440, 900)]:
            page.set_viewport_size({"width": width, "height": height})
            expect(dialog.get_by_role("button", name="Close", exact=True)).to_be_in_viewport()
            bounds = dialog.bounding_box()
            assert bounds and bounds["x"] >= 0 and bounds["x"] + bounds["width"] <= width
    page.keyboard.press("Escape")
    expect(triggers.first).to_be_focused()
    triggers.nth(1).click()
    expect(dialog.get_by_text("No answer found.", exact=True)).to_be_visible()
    expect(dialog.get_by_role("link")).to_have_count(0)
    dialog.get_by_role("button", name="Close", exact=True).click()
    workbook_socket = next(ws for ws in sockets if "workbook" in ws.url)
    workbook_socket.send(json.dumps({"type": "cell_update", "rowId": 2, "leadId": None, "colId": "research", "value": "Live answer", "status": "complete", "research": {**evidence, "answer": "Live answer"}}))
    expect(triggers.nth(1)).to_contain_text("2 sources")
    triggers.nth(1).click()
    expect(dialog.get_by_text("Live answer", exact=True)).to_be_visible()
    assert not errors, errors
    assert not unexpected, unexpected
    browser.close()
print("PASS: evidence keyboard, themes, small viewport, unsafe links, no-answer, live socket fixtures")
