"""Real editor selection UI, isolated API fixtures. No live data or provider calls."""
import argparse
import json
from pathlib import Path
from urllib.parse import urlparse, parse_qs
from playwright.sync_api import expect, sync_playwright

parser = argparse.ArgumentParser()
parser.add_argument("--url", default="http://127.0.0.1:4399")
parser.add_argument("--screenshots", type=Path)
parser.add_argument("--benchmark-only", action="store_true")
parser.add_argument("--benchmark-columns", type=int, default=6)
parser.add_argument("--check-wide-interactions", action="store_true")
parser.add_argument("--fail-first-width-save", action="store_true")
parser.add_argument("--check-budget", action="store_true")
parser.add_argument("--check-csv-loading", action="store_true")
parser.add_argument("--check-csv-save", action="store_true")
parser.add_argument("--check-add-column", action="store_true")
parser.add_argument("--check-delete-column", action="store_true")
args = parser.parse_args()
if args.check_wide_interactions and not args.benchmark_only:
    parser.error("--check-wide-interactions requires --benchmark-only")
if args.fail_first_width_save and not args.check_wide_interactions:
    parser.error("--fail-first-width-save requires --check-wide-interactions")
if args.benchmark_columns != 6 and not args.benchmark_only:
    parser.error("--benchmark-columns requires --benchmark-only")
if sum((args.benchmark_only, args.check_budget, args.check_csv_loading, args.check_csv_save, args.check_add_column, args.check_delete_column)) > 1:
    parser.error("choose only one of benchmark, budget, CSV loading or CSV save mode")
requests, errors, unexpected = [], [], []
edits = []
bulk_edits = []
width_saves = []
column_saves = []
order_attempts = []
budget_writes = []
settings_writes = []
column_adds = []
column_deletes = []
csv_writes = []
budget = {"budget_max_usd": 1, "budget_spent_usd": 0.1, "reserved_usd": 0.2, "uncertain_usd": 0.3,
          "remaining_usd": 0.4, "unlimited": False, "accounting_basis": "catalog_estimates_and_recorded_charges"}
names = {}
state = {"fail": True, "legacy": False, "deleted": set()}
wb = {"id": "selection", "name": "Selection fixture", "status": "draft", "source_type": "empty", "columns_config": [{"id": "company", "name": "Company", "type": "lead_field", "lead_field": "company", "width": 220}], "total_rows": 1001, "completed_rows": 0, "filter_criteria": None, "updated_at": "2026-09-22T00:00:00Z"}
computed_types = ["research", "http", "formula", "agent", "source"]
wb["columns_config"].extend({"id": kind, "name": kind.title(), "type": kind, "width": 100} for kind in computed_types)
if args.benchmark_only:
    if not 6 <= args.benchmark_columns <= 200:
        parser.error("--benchmark-columns must be between 6 and 200")
    wb["columns_config"].extend({"id": f"extra_{i}", "name": f"Field {i}", "type": "lead_field", "width": 160}
                                for i in range(args.benchmark_columns - 6))
    # Common supported columns/data in both revisions for an apples-to-apples
    # rendering comparison; do not benchmark old empty cells against new values.
    for column in wb["columns_config"]:
        column.update(type="lead_field", lead_field=column["id"])

def fixture(route):
    url = urlparse(route.request.url)
    path = url.path.rstrip("/")
    method = route.request.method
    if not path.startswith(("/api/", "/auth/")):
        route.continue_()
        return
    if path == "/api/workbooks/selection/import" and method == "POST":
        csv_writes.append(route.request.post_data_json)
        if len(csv_writes) == 1:
            return route.fulfill(status=503, json={"detail": "CSV storage unavailable"})
        route.fulfill(json={"created": 1, "added": 1, "skipped_duplicates": 1, "total_rows": 1002,
                            "columns_added": [], "mapping": csv_writes[-1]["mapping"],
                            "analysis": {"source_system": "generic", "detection_reason": "Fixture",
                                         "input_rows": 2, "importable_rows": 2, "custom_columns": [],
                                         "skipped_columns": ["Notes"], "collisions": []}})
    elif path == "/api/workbooks/selection/cost":
        if state.pop("cost_failure", False):
            return route.fulfill(status=503, json={"detail": "Unavailable"})
        route.fulfill(json=budget)
    elif path == "/api/workbooks/selection/budget" and method == "PUT":
        budget_writes.append(route.request.post_data_json)
        if len(budget_writes) == 1:
            return route.fulfill(status=503, json={"detail": "Unavailable"})
        budget["budget_max_usd"] = budget_writes[-1]["max_usd"]
        budget["remaining_usd"] = budget["budget_max_usd"] - 0.6
        if len(budget_writes) == 3:
            state["cost_failure"] = True
        route.fulfill(json={"budget_max_usd": budget["budget_max_usd"], "budget_spent_usd": 0.1})
    elif path == "/api/workbooks/selection" and method == "GET":
        params = parse_qs(url.query)
        page = int(params.get("page", [1])[0])
        ids = [n for n in range(1, 1002) if n not in state["deleted"]]
        current = ids[(page - 1) * 1000:page * 1000]
        rows = [{"lead_id": n if state["legacy"] else None, **({} if state["legacy"] else {"row_id": n}), "lead": {"company": names.get(n, f"Company {n}")} if state["legacy"] else {}, "data": {"company": names.get(n, f"Company {n}")}, "enrichments": {}} for n in current]
        for row in rows:
            row["enrichments"] = {kind: {"value": 0 if kind == "formula" else False if kind == "http" else "Result", "status": "complete"} for kind in computed_types}
            if args.benchmark_only:
                row["data"].update({column["id"]: "Benchmark value" for column in wb["columns_config"] if column["id"] != "company"})
                row["lead"] = dict(row["data"])
        route.fulfill(json={"workbook": wb, "rows": rows, "total_rows": len(ids), "query_total_rows": len(ids), "page": page, "page_size": 1000, "next_cursor": "page-2" if page == 1 else None, "has_more": page == 1})
    elif path == "/api/workbooks/selection/columns/order" and method == "PATCH":
        payload = route.request.post_data_json
        assert set(payload) == {"column_ids", "expected_column_ids"}
        assert payload["expected_column_ids"] == [column["id"] for column in wb["columns_config"]]
        order_attempts.append(payload)
        failure = state.pop("order_failure", None)
        if failure == "conflict":
            # Another client changes a different pair after this client's read.
            wb["columns_config"][2:4] = reversed(wb["columns_config"][2:4])
            return route.fulfill(status=409, json={"detail": "Column order changed"})
        if failure == "unavailable":
            return route.fulfill(status=503, json={"detail": "Order storage unavailable"})
        by_id = {column["id"]: column for column in wb["columns_config"]}
        column_saves.append([by_id[column_id] for column_id in payload["column_ids"]])
        wb["columns_config"] = column_saves[-1]
        route.fulfill(json={"column_ids": payload["column_ids"]})
    elif path == "/api/workbooks/selection/columns/company" and method == "DELETE":
        payload = route.request.post_data_json
        column_deletes.append(payload)
        current = next(column for column in wb["columns_config"] if column["id"] == "company")
        assert payload == {"expected_column": current}
        failure = state.pop("delete_failure", None)
        if failure == "unavailable":
            return route.fulfill(status=503, json={"detail": "Unavailable"})
        if failure == "conflict":
            current["width"] = 400
            return route.fulfill(status=409, json={"detail": "Changed"})
        wb["columns_config"] = [column for column in wb["columns_config"] if column["id"] != "company"]
        route.fulfill(json=wb)
    elif path == "/api/workbooks/selection/columns" and method == "POST":
        payload = route.request.post_data_json
        assert set(payload) == {"column"}
        column_adds.append(payload)
        if len(column_adds) == 1:
            wb["columns_config"][0]["width"] = 400  # Another client changes existing layout.
            return route.fulfill(status=503, json={"detail": "Unavailable"})
        if any(column["id"] == payload["column"]["id"] for column in wb["columns_config"]):
            return route.fulfill(status=409, json={"detail": "Duplicate"})
        wb["columns_config"].append(payload["column"])
        route.fulfill(json=wb)
    elif path == "/api/workbooks/selection/columns/company/settings" and method == "PATCH":
        payload = route.request.post_data_json
        settings_writes.append(payload)
        column = next(column for column in wb["columns_config"] if column["id"] == "company")
        failure = state.pop("settings_failure", None)
        if failure == "unavailable":
            return route.fulfill(status=503, json={"detail": "Unavailable"})
        if failure == "conflict":
            column["name"] = "Remote company"
            return route.fulfill(status=409, json={"detail": "Column settings changed. Refresh before saving again."})
        if failure == "dependency":
            return route.fulfill(status=409, json={"detail": "Rename breaks column-name references in: Research. Change those references to stable column IDs first."})
        assert set(payload) == {"changes", "expected"}, payload
        assert all(column.get(key) == value for key, value in payload["expected"].items()), payload
        column.update(payload["changes"])
        route.fulfill(json={"column_id": "company", "changes": payload["changes"]})
    elif path == "/api/workbooks/selection/columns/company/width" and method == "PATCH":
        width = route.request.post_data_json["width"]
        width_saves.append(width)
        if (args.fail_first_width_save and len(width_saves) == 1) or state.pop("width_failure", False):
            return route.fulfill(status=503, json={"detail": "Width storage unavailable"})
        next(column for column in wb["columns_config"] if column["id"] == "company")["width"] = width
        route.fulfill(json={"column_id": "company", "width": width})
    elif path == "/api/workbooks/selection/rows/1" and method == "PATCH":
        edits.append(route.request.post_data_json)
        names[1] = edits[-1]["company"]
        route.fulfill(json={"status": "ok"})
    elif path == "/api/workbooks/selection/rows" and method == "PATCH":
        bulk_edits.append(route.request.post_data_json)
        route.fulfill(json={"status": "ok", "updated_rows": len(bulk_edits[-1]["updates"]), "reactive_columns": []})
    elif path in ("/api/workbooks/selection/rows", "/api/workbooks/selection/rows/delete-query"):
        body = route.request.post_data_json
        requests.append({"path": path, "method": method, "body": body})
        if state["fail"]:
            route.fulfill(status=409, json={"detail": "Selection changed; review before retrying"})
        else:
            ids = body.get("row_ids", list(range(1, 1002)))
            state["deleted"].update(ids)
            route.fulfill(json={"deleted": len(ids)})
    elif path == "/api/events":
        route.fulfill(content_type="text/event-stream", body=": fixture\n\n")
    else:
        payloads = {
            "/auth/me": {"id": 1, "username": "UI fixture", "is_admin": True, "role": "admin"},
            "/api/workspaces": {"workspaces": [{"id": "fixture", "name": "UI fixture", "slug": "fixture"}], "active_id": "fixture"},
            "/api/copilotkit/conversations": {"conversations": []},
            "/api/settings/llm-usage": {"providers": [], "today_calls": 0, "today_tokens": 0},
            "/api/jobs": [], "/api/leads": [],
            "/api/workbooks/meta/providers": {"providers": []},
            "/api/workbooks/meta/ai-column-presets": {"presets": [], "categories": []},
            "/api/v2/workbooks/selection/views": {"views": []},
            "/api/workbooks/selection/connector-runs": {"runs": []},
            "/api/workbooks/selection/cost": {"budget_max_usd": 0, "budget_spent_usd": 0},
            "/api/workbooks/selection/run/estimate": {"best_usd": 0, "worst_usd": 0, "rows": 1001, "breakdown": [], "note": "Fixture only"},
        }
        if path not in payloads:
            unexpected.append(f"{method} {path}")
        route.fulfill(json=payloads.get(path, {}))

with sync_playwright() as p:
    browser = p.chromium.launch(executable_path="/usr/bin/chromium", headless=True)
    context = browser.new_context(viewport={"width": 1440, "height": 900}, reduced_motion="reduce")
    context.route("**/*", fixture)
    context.grant_permissions(["clipboard-read", "clipboard-write"])
    context.route_web_socket("**/*", lambda ws: None)
    context.add_init_script("localStorage.setItem('yupcha_token','fixture-only'); localStorage.setItem('yupcha_workspace_id','fixture')")
    page = context.new_page()
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.goto(args.url + "/workbooks/selection", wait_until="domcontentloaded")
    if args.check_delete_column:
        others = json.loads(json.dumps(wb["columns_config"][1:]))
        def open_delete():
            page.get_by_role("button", name="Configure Company column", exact=True).click()
            page.get_by_role("button", name="Delete", exact=True).click()
            dialog = page.get_by_role("alertdialog", name="Delete Company column?", exact=True)
            expect(dialog).to_be_visible()
            expect(dialog).to_contain_text("legacy enrichment records")
            expect(dialog).to_contain_text("row data and evidence")
            return dialog
        dialog = open_delete()
        dialog.get_by_role("button", name="Cancel", exact=True).click()
        assert not column_deletes
        for failure in ("unavailable", "conflict"):
            dialog = open_delete()
            state["delete_failure"] = failure
            before = len(column_deletes)
            dialog.get_by_role("button", name="Delete column", exact=True).evaluate("button => { button.click(); button.click() }")
            expect(dialog.get_by_role("alert")).to_be_visible()
            expect(dialog.get_by_role("button", name="Delete column", exact=True)).to_be_disabled()
            assert len(column_deletes) == before + 1
            assert len(wb["columns_config"]) == 6
            dialog.get_by_role("button", name="Cancel", exact=True).click()
            page.reload(wait_until="domcontentloaded")
        dialog = open_delete()
        dialog.get_by_role("button", name="Delete column", exact=True).click()
        expect(dialog).not_to_be_visible()
        expect(page.get_by_role("button", name="Configure Company column", exact=True)).not_to_be_visible()
        assert column_deletes[-1]["expected_column"]["width"] == 400
        assert wb["columns_config"] == others
        page.reload(wait_until="domcontentloaded")
        expect(page.get_by_role("button", name="Configure Research column", exact=True)).to_be_visible()
        expect(page.get_by_role("button", name="Configure Company column", exact=True)).not_to_be_visible()
        assert not errors, errors
        assert not unexpected, unexpected
        print("PASS: column delete cancel, failure, double-submit guard, conflict/review, exact raw snapshot and reload; fixture APIs")
        browser.close()
        raise SystemExit(0)
    if args.check_add_column:
        page.get_by_title("Add column", exact=True).click()
        field = page.get_by_placeholder("Column name...", exact=True)
        field.fill("Customer fit")
        page.get_by_role("button", name="Add Column", exact=True).evaluate("button => { button.click(); button.click() }")
        expect(page.get_by_role("alert").filter(has_text="Your draft has been kept")).to_be_visible()
        expect(field).to_have_value("Customer fit")
        assert len(column_adds) == 1
        page.get_by_role("button", name="Add Column", exact=True).click()
        expect(field).not_to_be_visible()
        assert column_adds == [{"column": {"id": "customer_fit", "name": "Customer fit", "type": "lead_field", "width": 200}}] * 2
        page.reload(wait_until="domcontentloaded")
        expect(page.get_by_role("button", name="Configure Customer fit column", exact=True)).to_be_visible()
        expect(page.locator('[data-grid-row="0"][data-col-id="company"]')).to_have_css("width", "400px")
        page.get_by_title("Add column", exact=True).click()
        field.fill("Customer fit")
        page.get_by_role("button", name="Add Column", exact=True).click()
        expect(page.get_by_role("alert")).to_contain_text("already exists")
        expect(field).to_have_value("Customer fit")
        assert len(wb["columns_config"]) == 7
        page.get_by_role("button", name="Cancel", exact=True).click()
        for label, kind in (("Find Email", "waterfall"), ("ICP Match", "ai_formula")):
            page.get_by_title("Add column", exact=True).click()
            page.get_by_role("dialog", name="Add column", exact=True).get_by_role("button").filter(has_text=label).click()
            expect(page.get_by_role("dialog", name="Add column", exact=True)).not_to_be_visible()
            assert column_adds[-1]["column"]["type"] == kind
            assert column_adds[-1]["column"]["name"] == label
        assert len(wb["columns_config"]) == 9
        assert not errors, errors
        assert not unexpected, unexpected
        print("PASS: targeted column creation, failure/draft/retry, duplicate guard, remote width preservation and reload; fixture APIs")
        browser.close()
        raise SystemExit(0)
    if args.check_csv_save:
        expect(page.locator('[data-grid-row="0"][data-grid-column="0"]')).to_be_visible()
        page.locator('input[type="file"]').set_input_files({"name": "mapped.csv", "mimeType": "text/csv",
            "buffer": b'Company,Email,Notes\nExample,person@example.test,Private note\nExample,person@example.test,Repeated\n'})
        dialog = page.get_by_role("dialog", name="Import CSV", exact=True)
        dialog.get_by_role("combobox", name="Map Notes", exact=True).select_option("__skip__")
        dialog.get_by_role("button", name="Import 2 rows", exact=True).evaluate("button => { button.click(); button.click() }")
        expect(page.get_by_text("CSV storage unavailable", exact=True)).to_be_visible()
        expect(dialog).to_be_visible()
        expect(dialog.get_by_role("combobox", name="Map Notes", exact=True)).to_have_value("__skip__")
        assert len(csv_writes) == 1, csv_writes
        expected = {"rows": [{"Company": "Example", "Email": "person@example.test", "Notes": note}
                             for note in ("Private note", "Repeated")],
                    "mapping": {"Company": "company", "Email": "email", "Notes": None},
                    "dedupe": True, "create_columns": True, "file_name": "mapped.csv", "source_system": "auto"}
        assert csv_writes[0] == expected, csv_writes
        dialog.get_by_role("button", name="Import 2 rows", exact=True).click()
        expect(page.get_by_text("Imported 1 row · 1 duplicate skipped", exact=True)).to_be_visible()
        expect(dialog).not_to_be_visible()
        assert csv_writes == [expected, expected], csv_writes
        assert not errors, errors
        assert not unexpected, unexpected
        print("CSV mapped save, failure draft retention, explicit retry and duplicate receipt pass (intercepted API)")
        browser.close()
        raise SystemExit(0)
    if args.check_csv_loading:
        expect(page.locator('[data-grid-row="0"][data-grid-column="0"]')).to_be_visible()
        assert not page.evaluate("performance.getEntriesByType('resource').some(r => r.name.includes('/papaparse'))")
        upload = page.locator('input[type="file"]')
        csv = {"name": "contacts.csv", "mimeType": "text/csv", "buffer": b'Company,Email\n"Example, Inc",person@example.test\n'}
        writes = []
        page.on("request", lambda request: writes.append(request.url) if request.method not in ("GET", "HEAD", "OPTIONS") else None)
        page.route("**/assets/papaparse*.js", lambda route: route.abort())
        upload.set_input_files(csv)
        expect(page.get_by_text("Failed to load CSV importer. Reload the page and try again.", exact=True)).to_be_visible()
        expect(page.get_by_role("dialog", name="Import CSV", exact=True)).not_to_be_visible()
        expect(page.locator('[data-grid-row="0"][data-grid-column="0"]')).to_be_visible()
        assert upload.input_value() == ""
        assert not writes, writes
        # Failed module imports are cached by the browser: recovery explicitly
        # reloads the document, matching the message instead of promising retry.
        page.unroute("**/assets/papaparse*.js")
        page.reload(wait_until="domcontentloaded")
        expect(page.locator('[data-grid-row="0"][data-grid-column="0"]')).to_be_visible()
        for _ in range(2):
            upload.set_input_files(csv)
            dialog = page.get_by_role("dialog", name="Import CSV", exact=True)
            expect(dialog).to_be_visible()
            expect(dialog.get_by_role("combobox", name="Map Company", exact=True)).to_have_value("company")
            expect(dialog.get_by_role("combobox", name="Map Email", exact=True)).to_have_value("email")
            expect(dialog.get_by_text("Example, Inc", exact=True)).to_be_visible()
            dialog.get_by_role("button", name="Close", exact=True).click()
            expect(dialog).not_to_be_visible()
        for contents, message in (
            (b'Company,Email\n"Broken,person@example.test\n', "Invalid CSV: Quoted field unterminated"),
            (b'Company,Email\nExample,person@example.test,extra\n', "Invalid CSV: Too many fields: expected 2 fields but parsed 3"),
        ):
            upload.set_input_files({"name": "invalid.csv", "mimeType": "text/csv", "buffer": contents})
            expect(page.get_by_text(message, exact=True)).to_be_visible()
            expect(page.get_by_role("dialog", name="Import CSV", exact=True)).not_to_be_visible()
        upload.set_input_files({"name": "single.csv", "mimeType": "text/csv", "buffer": b'Company\nSingle company\n'})
        dialog = page.get_by_role("dialog", name="Import CSV", exact=True)
        expect(dialog).to_be_visible()
        expect(dialog.get_by_text("Single company", exact=True)).to_be_visible()
        dialog.get_by_role("button", name="Close", exact=True).click()
        assert page.evaluate("performance.getEntriesByType('resource').some(r => r.name.includes('/papaparse'))")
        assert not errors, errors
        assert not unexpected, unexpected
        assert not writes, writes
        print("CSV deferred loading, download failure/reload recovery, mapping and repeat selection pass without writes (fixture only)")
        browser.close()
        raise SystemExit(0)
    if args.check_budget:
        expect(page.locator('[data-grid-row="0"][data-col-id="company"]')).to_be_visible()
        assert not page.evaluate("performance.getEntriesByType('resource').some(r => r.name.includes('/source-engine-panel-'))")
        page.route("**/assets/source-engine-panel-*.js", lambda route: route.abort())
        page.get_by_role("button", name="Source Engine", exact=True).click()
        expect(page.get_by_text("Source Engine could not load. Reload the page and try again.", exact=True)).to_be_visible()
        expect(page.get_by_role("button", name="Source Engine", exact=True)).to_be_enabled()
        expect(page.locator('[data-grid-row="0"][data-col-id="company"]')).to_be_visible()
        page.unroute("**/assets/source-engine-panel-*.js")
        page.reload(wait_until="domcontentloaded")
        held = []
        page.route("**/assets/source-engine-panel-*.js", lambda route: held.append(route))
        page.get_by_role("button", name="Source Engine", exact=True).evaluate("button => { button.click(); button.click() }")
        expect(page.get_by_role("button", name="Loading Source Engine…", exact=True)).to_be_disabled()
        page.wait_for_timeout(100)
        assert len(held) == 1, len(held)
        held[0].continue_()
        page.unroute("**/assets/source-engine-panel-*.js")
        state["cost_failure"] = True
        page.get_by_role("tab", name="Budget", exact=True).click()
        panel = page.get_by_role("tabpanel")
        expect(panel.get_by_text("Budget balances could not be loaded.", exact=True)).to_be_visible()
        expect(panel.get_by_role("button", name="Save", exact=True)).to_be_disabled()
        panel.get_by_role("button", name="Refresh balances").click()
        field = panel.get_by_label("Spend ceiling (USD, 0 = unlimited)")
        expect(field).to_have_value("1")
        expect(panel.get_by_text("$0.2000", exact=True)).to_be_visible()
        expect(panel.get_by_text("$0.3000", exact=True)).to_be_visible()
        field.fill("")
        panel.get_by_role("button", name="Save", exact=True).click()
        expect(panel.get_by_text("Enter a nonnegative amount. Use 0 for unlimited.")).to_be_visible()
        assert not budget_writes
        field.fill("2")
        panel.get_by_role("button", name="Save", exact=True).evaluate("button => { button.click(); button.click(); }")
        expect(panel.get_by_text("Failed to set budget", exact=True)).to_be_visible()
        expect(field).to_have_value("2")
        assert budget_writes == [{"max_usd": 2}]
        panel.get_by_role("button", name="Save", exact=True).click()
        expect(page.get_by_text("Budget updated", exact=True)).to_be_visible()
        expect(panel.get_by_text("$1.400", exact=True)).to_be_visible()
        assert budget_writes == [{"max_usd": 2}, {"max_usd": 2}]
        field.fill("3")
        panel.get_by_role("button", name="Save", exact=True).click()
        expect(panel.get_by_text("Budget saved, but balances could not refresh. Refresh before running.", exact=True)).to_be_visible()
        expect(field).to_have_value("3")
        expect(panel.get_by_role("button", name="Save", exact=True)).to_be_disabled()
        assert budget["budget_max_usd"] == 3
        assert len(budget_writes) == 3
        panel.get_by_role("button", name="Refresh balances").click()
        expect(panel.get_by_text("$2.400", exact=True)).to_be_visible()
        expect(panel.get_by_role("button", name="Save", exact=True)).to_be_enabled()
        expect(panel.get_by_text("Budget saved, but balances could not refresh. Refresh before running.", exact=True)).not_to_be_visible()
        assert len(budget_writes) == 3
        for name in ["Source", "Budget", "Refresh policy", "Entities", "Activity"]:
            expect(page.get_by_role("tab", name=name, exact=True)).to_be_visible()
        for theme in ["light", "dark"]:
            page.evaluate("theme => window.OpenGTMTheme.setPreference(theme)", theme)
            for width in [375, 768, 1440]:
                page.set_viewport_size({"width": width, "height": 900})
                dialog = page.get_by_role("dialog")
                box = dialog.bounding_box()
                assert box and box["x"] >= -1 and box["x"] + box["width"] <= width + 1, box
                assert dialog.evaluate("el => el.scrollWidth <= el.clientWidth + 1")
                field.focus()
                expect(field).to_be_focused()
                expect(panel.get_by_role("button", name="Save", exact=True)).to_be_in_viewport()
                if args.screenshots:
                    args.screenshots.mkdir(parents=True, exist_ok=True)
                    page.screenshot(path=str(args.screenshots / f"budget-{theme}-{width}.png"), animations="disabled")
        assert not errors, errors
        print(json.dumps({"passed": True, "scope": "budget load failure, retry, exposure, validation, save failure and retry; fixture APIs only"}))
        browser.close()
        raise SystemExit(0)
    try:
        if args.benchmark_only:
            expect(page.locator('[data-grid-row="0"][data-col-id="company"]')).to_be_visible()
        else:
            expect(page.get_by_role("checkbox", name="Select row 1", exact=True)).to_be_visible()
    except AssertionError:
        print(json.dumps({"errors": errors, "unexpected": unexpected, "body": page.locator("body").inner_text()}))
        raise
    cell = page.locator('[data-grid-row="0"][data-col-id="company"]')
    grid = page.get_by_role("grid", name="Workbook data grid", exact=True)
    if args.benchmark_only:
        last_column = wb["columns_config"][-1]["id"]
        grid.evaluate("el => { el.parentElement.scrollLeft = el.parentElement.scrollWidth }")
        last_cell = page.locator(f'[data-grid-row="0"][data-col-id="{last_column}"]')
        expect(last_cell).to_be_visible()
        last_cell.focus()
        last_cell.press("Home")
        if args.check_wide_interactions:
            first_cell = page.locator('[data-grid-row="0"][data-grid-column="0"]')
            expect(first_cell).to_be_focused()
            first_cell.press("End")
            expect(last_cell).to_be_focused()
            last_cell.press("F2")
            draft = last_cell.get_by_role("textbox")
            expect(draft).to_be_focused()
            draft.fill("Unsaved wide-column draft")
            grid.evaluate("el => { el.parentElement.scrollLeft = 0 }")
            expect(draft).to_have_value("Unsaved wide-column draft")
            expect(draft).to_be_focused()
            draft.press("Escape")
            expect(last_cell).to_be_focused()
            assert not edits, edits
            last_cell.press("Home")
            first_cell.press("ArrowRight")
            page.locator('[data-grid-row="0"][data-grid-column="1"]').press("ArrowRight")
            expect(cell).to_be_focused()
            cell.press("Shift+End")
            expect(last_cell).to_be_focused()
            last_cell.press("Control+c")
            page.wait_for_function("navigator.clipboard.readText().then(text => text.startsWith('Company 1\\t'))")
            copied = page.evaluate("navigator.clipboard.readText()")
            assert copied.split("\t") == ["Company 1"] + ["Benchmark value"] * (args.benchmark_columns - 1), copied
            last_cell.press("Home")
            first_cell.press("ArrowRight")
            page.locator('[data-grid-row="0"][data-grid-column="1"]').press("ArrowRight")
            expect(cell).to_be_focused()
            pasted = "\t".join(f"Pasted {i}" for i in range(args.benchmark_columns))
            cell.evaluate("""(el, text) => {
              const clipboardData = new DataTransfer(); clipboardData.setData('text/plain', text);
              el.dispatchEvent(new ClipboardEvent('paste', {bubbles: true, clipboardData}));
            }""", pasted)
            expect(last_cell).to_be_focused()
            assert bulk_edits == [{"updates": [{"row_id": 1, "fields": {column["id"]: f"Pasted {i}" for i, column in enumerate(wb["columns_config"])}}]}], bulk_edits
            last_cell.press("Home")
            header = page.locator('thead [data-col-id="company"]')
            handle = header.locator('.cursor-col-resize')
            before_width = round(header.bounding_box()["width"])
            box = handle.bounding_box()
            assert box
            page.mouse.move(box["x"] + box["width"] / 2, box["y"] + box["height"] / 2)
            page.mouse.down()
            page.mouse.move(box["x"] + box["width"] / 2 + 80, box["y"] + box["height"] / 2, steps=5)
            page.mouse.up()
            expect(header).to_have_css("width", f"{int(before_width + 80)}px")
            expect(cell).to_have_css("width", f"{int(before_width + 80)}px")
            first_cell.press("End")
            expect(last_cell).to_be_focused()
            last_cell.press("Home")
            expect(cell).to_have_css("width", f"{int(before_width + 80)}px")
            final_width = int(before_width + 80)
            if args.fail_first_width_save:
                expect(page.get_by_text("Column width was not saved. Resize again to retry; reload may restore the old width.", exact=True)).to_be_visible()
                assert wb["columns_config"][0]["width"] == 220, (wb["columns_config"][0]["width"], before_width, width_saves)
                box = handle.bounding_box()
                assert box
                page.mouse.move(box["x"] + box["width"] / 2, box["y"] + box["height"] / 2)
                page.mouse.down()
                page.mouse.move(box["x"] + box["width"] / 2 + 20, box["y"] + box["height"] / 2, steps=3)
                with page.expect_response(lambda response: response.url.endswith("/columns/company/width") and response.status == 200):
                    page.mouse.up()
                final_width += 20
                expect(cell).to_have_css("width", f"{final_width}px")
            page.reload(wait_until="domcontentloaded")
            expect(cell).to_have_css("width", f"{final_width}px")
            assert width_saves == ([int(before_width + 80), final_width] if args.fail_first_width_save else [final_width]), width_saves
            header.focus()
            header.press("Space")
            expect(header).to_have_attribute("aria-pressed", "true")
            page.evaluate("new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))")
            header.press("ArrowRight")
            page.evaluate("new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))")
            header.press("Space")
            expect(page.locator('thead [data-col-id="research"]')).to_be_visible()
            page.wait_for_function("document.querySelector('[data-grid-row=\"0\"][data-grid-column=\"2\"]')?.dataset.colId === 'research'")
            assert len(column_saves) == 1
            assert [column["id"] for column in column_saves[0]][:3] == ["research", "company", "http"]
            assert next(column for column in column_saves[0] if column["id"] == "company")["width"] == final_width
            expect(page.locator('[data-grid-row="0"][data-col-id="company"]')).to_contain_text("Company 1")
            page.reload(wait_until="domcontentloaded")
            expect(page.locator('[data-grid-row="0"][data-grid-column="2"]')).to_have_attribute("data-col-id", "research")
            expect(page.locator('[data-grid-row="0"][data-col-id="company"]')).to_have_css("width", f"{final_width}px")
            # Failed writes must not appear saved; refresh and retry safely.
            for failure, message in [("unavailable", "Column order was not saved"), ("conflict", "Column order changed. Refresh before reordering.")]:
                state["order_failure"] = failure
                before_saves, before_attempts = len(column_saves), len(order_attempts)
                header.focus()
                header.press("Space")
                expect(header).to_have_attribute("aria-pressed", "true")
                page.evaluate("new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))")
                header.press("ArrowLeft")
                page.evaluate("new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))")
                with page.expect_response(lambda response: urlparse(response.url).path == "/api/workbooks/selection" and response.request.method == "GET"):
                    header.press("Space")
                expect(page.get_by_text(message, exact=True)).to_be_visible()
                expect(page.locator('[data-grid-row="0"][data-grid-column="2"]')).to_have_attribute("data-col-id", "research")
                assert len(column_saves) == before_saves
                assert len(order_attempts) == before_attempts + 1
                if failure == "conflict":
                    expect(page.locator('[data-grid-row="0"][data-grid-column="4"]')).to_have_attribute("data-col-id", "formula")
            # Cancellation must not persist the proposed leftward move.
            header.focus()
            header.press("Space")
            expect(header).to_have_attribute("aria-pressed", "true")
            page.evaluate("new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))")
            header.press("ArrowLeft")
            page.evaluate("new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))")
            header.press("Escape")
            expect(header).not_to_have_attribute("aria-pressed", "true")
            assert len(column_saves) == 1
            expect(page.locator('[data-grid-row="0"][data-grid-column="2"]')).to_have_attribute("data-col-id", "research")
            # Commit the same reverse move, preserving identity and width.
            header.focus()
            header.press("Space")
            expect(header).to_have_attribute("aria-pressed", "true")
            page.evaluate("new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))")
            header.press("ArrowLeft")
            page.evaluate("new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))")
            header.press("Space")
            expect(page.locator('[data-grid-row="0"][data-grid-column="2"]')).to_have_attribute("data-col-id", "company")
            assert len(column_saves) == 2
            assert [column["id"] for column in column_saves[1]][:4] == ["company", "research", "formula", "http"]
            assert column_saves[1][0]["width"] == final_width
            page.reload(wait_until="domcontentloaded")
            expect(page.locator('[data-grid-row="0"][data-grid-column="2"]')).to_have_attribute("data-col-id", "company")
            origin = header.bounding_box()
            destination = page.locator('thead [data-col-id="research"]').bounding_box()
            assert origin and destination
            # Use the header body, never the resize handle.
            page.mouse.move(origin["x"] + origin["width"] / 2, origin["y"] + origin["height"] / 2)
            page.mouse.down()
            page.mouse.move(destination["x"] + destination["width"] / 2, destination["y"] + destination["height"] / 2, steps=12)
            page.mouse.up()
            expect(page.locator('[data-grid-row="0"][data-grid-column="2"]')).to_have_attribute("data-col-id", "research")
            assert len(column_saves) == 3
            assert [column["id"] for column in column_saves[2]][:4] == ["research", "company", "formula", "http"]
            assert next(column for column in column_saves[2] if column["id"] == "company")["width"] == final_width
            assert len(width_saves) == (2 if args.fail_first_width_save else 1)
            page.reload(wait_until="domcontentloaded")
            expect(page.locator('[data-grid-row="0"][data-grid-column="2"]')).to_have_attribute("data-col-id", "research")
            expect(page.locator('[data-grid-row="0"][data-col-id="company"]')).to_have_css("width", f"{final_width}px")
            resize = page.get_by_role("separator", name="Resize Company column", exact=True)
            resize.focus()
            expect(resize).to_be_focused()
            prior_saves = len(width_saves)
            for key, width in (("ArrowRight", final_width + 10), ("Shift+ArrowLeft", final_width - 40),
                               ("Home", 80), ("End", 600)):
                resize.press(key)
                expect(resize).to_have_attribute("aria-valuenow", str(width))
                expect(page.locator('[data-grid-row="0"][data-col-id="company"]')).to_have_css("width", f"{width}px")
                expect(resize).to_be_focused()
            resize.press("ArrowRight")  # At maximum: no redundant save.
            resize.press("Space")  # Must not activate header reordering.
            resize.press("Escape")
            expect(page.get_by_role("status")).not_to_contain_text("picked up")
            page.wait_for_timeout(300)
            assert width_saves[prior_saves:] == [final_width + 10, final_width - 40, 80, 600], width_saves
            assert len(column_saves) == 3, column_saves
            page.reload(wait_until="domcontentloaded")
            expect(page.get_by_role("separator", name="Resize Company column", exact=True)).to_have_attribute("aria-valuenow", "600")
            expect(page.locator('[data-grid-row="0"][data-col-id="company"]')).to_have_css("width", "600px")
            page.get_by_role("button", name="Configure Company column", exact=True).click()
            settings = page.get_by_role("dialog", name="Company column settings", exact=True)
            expect(settings).to_be_visible()
            for key in ["Tab"] * 12 + ["Shift+Tab"] * 12:
                page.keyboard.press(key)
                page.wait_for_function("() => [...document.querySelectorAll('[role=dialog]')].some(el => el.contains(document.activeElement))", timeout=2000)
            page.set_viewport_size({"width": 375, "height": 812})
            bounds = settings.bounding_box()
            assert bounds and bounds["x"] >= 0 and bounds["x"] + bounds["width"] <= 376, bounds
            page.keyboard.press("Escape")
            expect(settings).not_to_be_visible()
            expect(page.get_by_role("button", name="Configure Company column", exact=True)).to_be_focused()
            page.set_viewport_size({"width": 1440, "height": 900})
            page.get_by_role("button", name="Configure Company column", exact=True).click()
            width_field = page.get_by_label("Column width (pixels)", exact=True)
            expect(width_field).to_have_value("600")
            before_form = len(width_saves)
            for invalid in ("", "79", "601", "100.5"):
                width_field.fill(invalid)
                page.get_by_role("button", name="Save width", exact=True).click()
                expect(page.get_by_text("Enter a whole number from 80 to 600.", exact=True)).to_be_visible()
                expect(width_field).to_have_attribute("aria-invalid", "true")
            assert len(width_saves) == before_form
            width_field.fill("240")
            state["width_failure"] = True
            page.get_by_role("button", name="Save width", exact=True).evaluate("button => { button.click(); button.click() }")
            expect(page.get_by_text("Width was not saved. Your value is kept; try saving again.", exact=True)).to_be_visible()
            expect(width_field).to_have_value("240")
            assert width_saves[before_form:] == [240]
            expect(page.locator('[data-grid-row="0"][data-col-id="company"]')).to_have_css("width", "600px")
            page.get_by_role("button", name="Save width", exact=True).click()
            expect(page.get_by_text("Column width saved.", exact=True)).to_be_visible()
            expect(page.locator('[data-grid-row="0"][data-col-id="company"]')).to_have_css("width", "240px")
            assert width_saves[before_form:] == [240, 240]
            assert len(column_saves) == 3
            page.get_by_role("button", name="Close column settings", exact=True).click()
            expect(settings).not_to_be_visible()
            expect(page.get_by_role("button", name="Configure Company column", exact=True)).to_be_focused()
            page.reload(wait_until="domcontentloaded")
            expect(page.locator('[data-grid-row="0"][data-col-id="company"]')).to_have_css("width", "240px")
            page.get_by_role("button", name="Configure Company column", exact=True).click()
            name_field = page.get_by_label("Column Name", exact=True)
            name_field.fill("My company")
            state["settings_failure"] = "unavailable"
            name_field.press("Tab")
            expect(page.get_by_role("alert").filter(has_text="Submitted values for column company")).to_be_visible()
            expect(name_field).to_have_value("My company")
            assert settings_writes == [{"changes": {"name": "My company"}, "expected": {"name": "Company"}}]
            state["settings_failure"] = "conflict"
            page.get_by_role("button", name="Retry settings save", exact=True).click()
            expect(page.get_by_role("alert").filter(has_text="Column settings changed. Refresh before saving again.")).to_be_visible()
            assert len(settings_writes) == 2
            page.get_by_role("button", name="Discard submitted edit and reload settings", exact=True).click()
            expect(page.get_by_role("dialog", name="My company column settings", exact=True)).not_to_be_visible()
            page.get_by_role("button", name="Configure Remote company column", exact=True).click()
            expect(name_field).to_have_value("Remote company")
            name_field.fill("Company")
            name_field.press("Tab")
            expect(page.get_by_role("dialog", name="Company column settings", exact=True)).to_be_visible()
            assert settings_writes[-1] == {"changes": {"name": "Company"}, "expected": {"name": "Remote company"}}
            assert len(settings_writes) == 3
            assert next(column for column in wb["columns_config"] if column["id"] == "company")["width"] == 240
            page.get_by_role("button", name="Close column settings", exact=True).click()
            page.locator('thead [data-col-id="company"]').click(button="right")
            page.get_by_role("button", name="Rename", exact=True).click()
            rename_dialog = page.get_by_role("dialog", name="Rename column", exact=True)
            expect(rename_dialog).to_be_visible()
            rename_field = rename_dialog.get_by_label("Column name", exact=True)
            rename_field.fill("Renamed company")
            before_rename = len(settings_writes)
            state["settings_failure"] = "unavailable"
            rename_dialog.get_by_role("button", name="Rename", exact=True).evaluate("button => { button.click(); button.click() }")
            expect(rename_dialog.get_by_role("alert")).to_contain_text("could not be saved")
            expect(rename_field).to_have_value("Renamed company")
            assert len(settings_writes) == before_rename + 1
            state["settings_failure"] = "dependency"
            rename_dialog.get_by_role("button", name="Rename", exact=True).click()
            expect(rename_dialog.get_by_role("alert")).to_contain_text("stable column IDs first")
            expect(rename_field).to_have_value("Renamed company")
            assert next(column for column in wb["columns_config"] if column["id"] == "company")["name"] == "Company"
            # Subsequent fixture success represents dependency repair elsewhere.
            rename_dialog.get_by_role("button", name="Rename", exact=True).click()
            expect(rename_dialog).not_to_be_visible()
            expect(page.get_by_role("button", name="Configure Renamed company column", exact=True)).to_be_focused()
            assert settings_writes[before_rename:] == [
                {"changes": {"name": "Renamed company"}, "expected": {"name": "Company"}}] * 3
            page.reload(wait_until="domcontentloaded")
            expect(page.get_by_role("button", name="Configure Renamed company column", exact=True)).to_be_visible()
            expect(page.locator('[data-grid-row="0"][data-col-id="company"]')).to_have_css("width", "240px")
        grid.evaluate("el => { el.parentElement.scrollLeft = 0 }")
        expect(cell).to_be_visible()
        measurements = grid.evaluate("""async el => {
          let scroller = el.parentElement;
          while (scroller && !(scroller.scrollHeight > scroller.clientHeight && /auto|scroll/.test(getComputedStyle(scroller).overflowY))) scroller = scroller.parentElement;
          if (!scroller) throw new Error('Missing scroll container');
          const samples = [];
          for (let i = 0; i < 24; i++) {
            const atEnd = i % 2 === 0;
            const target = atEnd ? 999 : 0;
            const start = performance.now();
            scroller.scrollTop = atEnd ? scroller.scrollHeight : 0;
            await new Promise((resolve, reject) => {
              function inspect() {
                if (el.querySelector(`[data-grid-row="${target}"]`)) return requestAnimationFrame(resolve);
                if (performance.now() - start > 3000) return reject(new Error('Scroll render timed out'));
                requestAnimationFrame(inspect);
              }
              requestAnimationFrame(inspect);
            });
            if (i >= 4) samples.push(performance.now() - start);
          }
          return samples;
        }""")
        ordered = sorted(measurements)
        print(json.dumps({"scroll_render_benchmark": {"samples_ms": measurements, "median_ms": ordered[len(ordered)//2], "p95_ms": ordered[int(len(ordered)*.95)-1], "loaded_rows": 1000, "columns": args.benchmark_columns, "rendered_cells": grid.get_by_role("gridcell").count(), "scope": "headless Chromium, local fixture, scroll-to-row plus animation frame; not field INP"}}))
        assert not errors, errors
        assert not unexpected, unexpected
        print(json.dumps({"passed": True, "wide_interactions": args.check_wide_interactions,
                          "width_failure_recovery": args.fail_first_width_save,
                          "columns": args.benchmark_columns, "scope": "intercepted API fixtures only"}))
        browser.close()
        raise SystemExit(0)
    rendered_counts = []
    for position, expected_row in [(0.5, 500), (1, 1000), (0, 1)]:
        grid.evaluate("""(el, fraction) => {
          let scroller = el.parentElement;
          while (scroller && !(scroller.scrollHeight > scroller.clientHeight && /auto|scroll/.test(getComputedStyle(scroller).overflowY))) scroller = scroller.parentElement;
          if (!scroller) throw new Error('Workbook scroll container not found');
          scroller.scrollTop = fraction * (scroller.scrollHeight - scroller.clientHeight);
        }""", position)
        if position == 0.5:
            # The midpoint depends on viewport/header height; assert a middle
            # row range rather than assuming an exact pixel-to-index mapping.
            expect(page.locator('[data-grid-row="500"][data-col-id="company"]')).to_be_attached()
        else:
            expect(page.get_by_role("checkbox", name=f"Select row {expected_row}", exact=True)).to_be_visible()
        rendered_counts.append(grid.get_by_role("row").count())
        assert rendered_counts[-1] < 100, rendered_counts
    print(json.dumps({"virtualization": {"loaded_rows": 1000, "rendered_rows_at_middle_end_start": rendered_counts}}))
    for kind in computed_types:
        computed = page.locator(f'[data-grid-row="0"][data-col-id="{kind}"]')
        expect(computed.locator('[data-editable-cell="true"]')).to_have_count(0)
        expect(computed).to_contain_text("0" if kind == "formula" else "false" if kind == "http" else "Result")
    expect(cell).to_contain_text("Company 1")
    cell.dblclick()
    cell.get_by_role("textbox").fill("Discard draft")
    cell.get_by_role("textbox").press("Escape")
    assert not edits
    cell.dblclick()
    cell.get_by_role("textbox").fill("Edited company")
    cell.get_by_role("textbox").press("Enter")
    expect(cell).to_contain_text("Edited company")
    assert edits == [{"company": "Edited company"}], edits
    page.get_by_role("checkbox", name="Select row 1", exact=True).check()
    bar = page.get_by_role("region", name="Selected workbook rows")
    expect(bar).to_contain_text("1 rows selected · 1 on this page")
    page.get_by_role("button", name="Next workbook page").click()
    expect(page.get_by_role("checkbox", name="Select row 1001", exact=True)).to_be_visible()
    expect(bar).to_contain_text("1 rows selected · 0 on this page")
    page.get_by_role("checkbox", name="Select row 1001", exact=True).check()
    expect(bar).to_contain_text("2 rows selected · 1 on this page")
    trigger = bar.get_by_role("button", name="Delete selected", exact=True)
    trigger.click()
    alert = page.get_by_role("alertdialog")
    expect(alert).to_contain_text("Delete 2 workbook rows?")
    for theme in ("light", "dark"):
        page.evaluate("theme => window.OpenGTMTheme.setPreference(theme)", theme)
        for width in (375, 1440):
            page.set_viewport_size({"width": width, "height": 900})
            bounds = alert.bounding_box()
            assert bounds and bounds["x"] >= 0 and bounds["x"] + bounds["width"] <= width
            if args.screenshots:
                args.screenshots.mkdir(parents=True, exist_ok=True)
                page.screenshot(path=str(args.screenshots / f"selection-dialog-{theme}-{width}.png"), animations="disabled")
    alert.get_by_role("button", name="Cancel", exact=True).click()
    expect(trigger).to_be_focused()
    assert not requests
    trigger.click()
    alert.get_by_role("button", name="Delete 2 rows", exact=True).evaluate("button => { button.click(); button.click(); }")
    expect(alert.get_by_role("alert")).to_contain_text("Failed to delete workbook rows")
    assert len(requests) == 1 and requests[0]["body"] == {"row_ids": [1, 1001]}, requests
    state["fail"] = False
    alert.get_by_role("button", name="Delete 2 rows", exact=True).click()
    expect(alert).not_to_be_visible()
    # Twenty resolves the grid fallback to its first focusable control.
    expect(page.get_by_role("checkbox", name="Select all rows on this page", exact=True)).to_be_focused()
    expect(page.get_by_role("checkbox", name="Select row 2", exact=True)).to_be_visible()
    assert len(requests) == 2
    page.get_by_role("checkbox", name="Select row 2", exact=True).check()
    bar.get_by_role("button", name="Select all 999 matching rows").click()
    expect(bar).to_contain_text("999 matching rows selected across all pages")
    bar.get_by_role("button", name="Delete selected", exact=True).click()
    expect(alert).to_contain_text("Delete 999 workbook rows?")
    state["fail"] = True
    alert.get_by_role("button", name="Delete 999 rows", exact=True).click()
    expect(alert.get_by_role("alert")).to_contain_text("Selection changed; review before retrying")
    assert requests[-1]["body"] == {"expected_count": 999, "confirmation": "DELETE 999 ROWS"}
    alert.get_by_role("button", name="Cancel", exact=True).click()
    # A row checkbox explicitly exits all-matching scope.
    page.get_by_role("checkbox", name="Select row 2", exact=True).check()
    expect(bar).to_contain_text("1 rows selected · 1 on this page")
    state["legacy"] = True
    page.reload()
    page.get_by_role("checkbox", name="Select row 2", exact=True).check()
    bar.get_by_role("button", name="Delete selected", exact=True).click()
    expect(bar.get_by_role("alert")).to_contain_text("legacy lead rows")
    assert len(requests) == 3
    assert not unexpected, unexpected
    assert not errors, errors
    browser.close()
print(json.dumps({"passed": True, "scope": "editor cross-page selection and deletion UI; fixture APIs only", "mutation_attempts": len(requests)}))
