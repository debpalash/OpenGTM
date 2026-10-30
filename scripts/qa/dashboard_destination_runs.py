from playwright.sync_api import sync_playwright, expect
from pathlib import Path
import argparse, json, os, urllib.request, urllib.parse, time

parser = argparse.ArgumentParser(
    description="Verify destination review, run receipts, recovery, pagination, identity links and permissions. Uses native synthetic records; outbound mutations and delivery reads use controlled responses."
)
parser.add_argument("--base-url", default="http://127.0.0.1:8002")
parser.add_argument(
    "--browser-path", default=os.environ.get("PLAYWRIGHT_CHROMIUM_EXECUTABLE")
)
parser.add_argument("--output", default=".run/dashboard-destination-runs")
parser.add_argument("--engine", choices=["chromium", "webkit"], default="chromium")
args = parser.parse_args()
root = Path(__file__).resolve().parents[2]
base = args.base_url.rstrip("/")
out = root / args.output
out.mkdir(parents=True, exist_ok=True)
headers = {}
creds = {
    "USERNAME": os.environ.get("OPENGTM_QA_USERNAME"),
    "PASSWORD": os.environ.get("OPENGTM_QA_PASSWORD"),
}
report = {}
if not all(creds.values()):
    parser.error("Set OPENGTM_QA_USERNAME and OPENGTM_QA_PASSWORD.")


def req(method, path, body=None, form=False):
    data = (
        None
        if body is None
        else urllib.parse.urlencode(body).encode()
        if form
        else json.dumps(body).encode()
    )
    try:
        with urllib.request.urlopen(
            urllib.request.Request(
                base + path,
                method=method,
                data=data,
                headers={
                    **headers,
                    "Content-Type": "application/x-www-form-urlencoded"
                    if form
                    else "application/json",
                },
            ),
            timeout=60,
        ) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        raise RuntimeError(str(error.code) + " " + error.read().decode())


headers["Authorization"] = (
    "Bearer "
    + req(
        "POST",
        "/auth/token",
        {"username": creds["USERNAME"], "password": creds["PASSWORD"]},
        True,
    )["access_token"]
)
prior = req("GET", "/api/workspaces")["active_id"]
ws = req(
    "POST", "/api/workspaces", {"name": "Destination runs UI " + str(int(time.time()))}
)["id"]
errors = []
scans = []
try:
    req("POST", "/api/workspaces/switch", {"workspace_id": ws})
    headers["X-Workspace-Id"] = ws
    company = req(
        "POST",
        "/api/records/company",
        {"company": {"name": "Receipt company", "domain": "receipt.test"}},
    )
    person = req(
        "POST",
        "/api/records/person",
        {
            "person": {"full_name": "Receipt person", "email": "person@receipt.test"},
            "company_id": company["id"],
        },
    )
    audience = req(
        "POST",
        "/api/records/person/audience",
        {"ids": [person["id"]], "name": "Reviewed people"},
    )["audience_id"]
    # Refresh before adding the destination: refresh can enqueue existing enabled destinations.
    req("POST", "/api/audiences/" + audience + "/refresh", {})
    req("PATCH", "/api/audiences/" + audience, {"refresh_enabled": False})
    destination = req(
        "POST",
        "/api/audience-destinations",
        {
            "audience_id": audience,
            "name": "Review fixture",
            "destination_type": "webhook",
            "config": {
                "url": "https://receipt-review.invalid/private-token?api_key=synthetic-secret"
            },
            "field_map": {"full_name": "name", "email": "email"},
        },
    )
    report.update(
        workspace_id=ws, audience_id=audience, destination_id=destination["id"]
    )
    did = destination["id"]
    rid = "qa-run-one"
    stamp = "2026-10-29T09:00:00"

    def run(status="pending", **patch):
        return {
            "id": rid,
            "destination_id": did,
            "status": status,
            "attempted": 0,
            "succeeded": 0,
            "failed": 0,
            "skipped": 0,
            "error": None,
            "started_at": None,
            "finished_at": None,
            "created_at": stamp,
            **patch,
        }

    state = {
        "latest": None,
        "permission": True,
        "history": "ok",
        "listing": "ok",
        "delivery": "ok",
        "settled": False,
        "many": False,
        "post": "hold",
        "held": [],
        "posts": [],
        "audience": next(
            a for a in req("GET", "/api/audiences") if a["id"] == audience
        ),
        "audience_writes": [],
        "audience_fail": False,
        "extra": [],
    }
    with sync_playwright() as p:
        browser = getattr(p,args.engine).launch(headless=True,executable_path=args.browser_path if args.engine=="chromium" else None)
        context = browser.new_context(
            viewport={"width": 1440, "height": 1000},
            reduced_motion="reduce",
            has_touch=True,
        )
        context.add_init_script(
            'localStorage.setItem("yupcha_token",'
            + json.dumps(headers["Authorization"][7:])
            + ');localStorage.setItem("yupcha_workspace_id",'
            + json.dumps(ws)
            + ');localStorage.setItem("theme","light")'
        )
        page = context.new_page()
        page.on("pageerror", lambda e: errors.append(str(e)))

        def listed(route):
            if state["listing"] == "fail":
                route.fulfill(
                    status=503, json={"detail": "Synthetic destination list failure"}
                )
                return
            route.fulfill(
                json=[{**destination, "latest_run": state["latest"]}, *state["extra"]]
            )

        def history(route):
            if state["history"] == "fail":
                route.fulfill(
                    status=503, json={"detail": "Synthetic history unavailable"}
                )
                return
            if state["history"] == "malformed":
                route.fulfill(json={"runs": None})
                return
            q = urllib.parse.parse_qs(urllib.parse.urlparse(route.request.url).query)
            rows = [state["latest"]] if state["latest"] else []
            if state["many"]:
                if "cursor" in q:
                    rows = [run("completed", id="qa-older-20")]
                else:
                    rows += [
                        run("completed", id="qa-older-" + str(i)) for i in range(1, 20)
                    ]
            route.fulfill(
                json={
                    "runs": rows,
                    "limit": 20,
                    "offset": None if "cursor" in q else 0,
                    "has_more": state["many"] and "cursor" not in q,
                    "next_cursor": "older-page"
                    if state["many"] and "cursor" not in q
                    else None,
                }
            )

        def deliveries(route):
            if state["delivery"] == "fail":
                route.fulfill(
                    status=503, json={"detail": "Synthetic delivery unavailable"}
                )
                return
            q = urllib.parse.parse_qs(urllib.parse.urlparse(route.request.url).query)
            after = int(q.get("after_id", ["0"])[0])
            status = q.get("status", [""])[0]
            url_run = urllib.parse.urlparse(route.request.url).path.split("/")[-2]
            rows = []
            if state["settled"]:
                for i in range(1, 52):
                    row = {
                        "id": i,
                        "run_id": url_run,
                        "destination_id": did,
                        "lead_id": None,
                        "company_id": company["id"],
                        "operation": "upsert",
                        "status": "failed" if i % 10 == 0 else "success",
                        "attempts": 1,
                        "external_id": None,
                        "summary": None,
                        "error": "Synthetic provider rejection"
                        if i % 10 == 0
                        else None,
                        "delivered_at": None,
                        "created_at": stamp,
                    }
                    if i > 1:
                        row["person_id"] = person["id"]
                    if i > after and (not status or row["status"] == status):
                        rows.append(row)
            if state["delivery"] == "wrong-scope" and rows:
                rows[0]["destination_id"] = "other-destination"
            route.fulfill(json=rows[:50])

        def mutate(route):
            state["posts"].append(route.request.url)
            if state["post"] == "hold":
                state["held"].append(route)
                return
            if state["post"] == "lost":
                state["latest"] = run()
                route.abort("failed")
                return
            if route.request.url.endswith("/cancel"):
                state["latest"] = run(
                    "cancelling", attempted=51, succeeded=46, failed=5
                )
            else:
                state["latest"] = run(attempted=51, succeeded=46, failed=5)
            route.fulfill(status=202, json=state["latest"])

        def audience_write(route):
            state["audience_writes"].append(route.request.method)
            if state["audience_fail"]:
                route.fulfill(
                    status=503, json={"detail": "Synthetic audience write failure"}
                )
                return
            if route.request.method == "PATCH":
                state["audience"].update(route.request.post_data_json)
                route.fulfill(json=state["audience"])
            else:
                route.fulfill(
                    json={
                        "audience": state["audience"],
                        "entered": 0,
                        "exited": 0,
                        "changed": 0,
                        "unchanged": state["audience"]["member_count"],
                    }
                )

        page.route(
            "**/api/audiences", lambda route: route.fulfill(json=[state["audience"]])
        )
        page.route("**/api/audiences/" + audience + "/refresh", audience_write)
        page.route("**/api/audiences/" + audience, audience_write)
        page.route("**/api/audience-destinations?*", listed)
        page.route(
            "**/api/audience-destinations/access",
            lambda route: route.fulfill(json={"can_write": state["permission"]}),
        )
        page.route("**/api/audience-destinations/*/runs?*", history)
        page.route("**/api/audience-destinations/runs/*/deliveries?*", deliveries)
        for pattern in [
            "**/api/audience-destinations/*/sync",
            "**/api/audience-destinations/runs/*/retry",
            "**/api/audience-destinations/runs/*/cancel",
        ]:
            page.route(pattern, mutate)

        def log(name):
            report[name] = "pass"
            report["axe"] = scans
            report["page_errors"] = errors
            (out / "results.json").write_text(json.dumps(report, indent=2))
            print(name, "pass", flush=True)

        def audit(name):
            page.add_script_tag(
                path=str(root / "apps/web/node_modules/axe-core/axe.min.js")
            )
            result = page.evaluate("async()=>await axe.run(document)")
            scans.append({"name": name, "violations": result["violations"]})
            assert not result["violations"], [
                (v["id"], [n["html"] for n in v["nodes"]]) for v in result["violations"]
            ]

        def close(dialog, label):
            dialog.get_by_role("button", name=label, exact=True).click()
            expect(dialog).not_to_be_visible()

        def sync_review():
            page.get_by_role("button", name="Sync audience", exact=True).click()
            dialog = page.get_by_role("dialog", name="Review audience sync", exact=True)
            expect(dialog.get_by_text("1 record", exact=False)).to_be_visible()
            return dialog

        page.goto(base + "/companies")
        expect(
            page.get_by_role("button", name="Receipt company", exact=True)
        ).to_be_visible()
        page.get_by_role("link", name="Audiences", exact=True).click()
        page.wait_for_load_state("networkidle")
        dialog = sync_review()
        assert not state["posts"]
        expect(dialog).to_contain_text("https://receipt-review.invalid")
        assert (
            "private-token" not in dialog.inner_text()
            and "synthetic-secret" not in dialog.inner_text()
        )
        audit("sync-review-light")
        close(dialog, "Cancel")
        expect(
            page.get_by_role("button", name="Sync audience", exact=True)
        ).to_be_focused()
        log("review_cancel_and_focus_return_do_not_send")
        dialog = sync_review()
        destination["field_map"]["company"] = "organization"
        dialog.get_by_role("button", name="Confirm sync", exact=True).click()
        expect(dialog.get_by_role("alert")).to_contain_text("changed after review")
        assert not state["posts"]
        expect(
            dialog.get_by_role("button", name="Confirm sync", exact=True)
        ).to_be_disabled()
        audit("changed-review-scope")
        dialog.get_by_role("button", name="Refresh review", exact=True).click()
        expect(dialog.get_by_text("organization", exact=True)).to_be_visible()
        expect(
            dialog.get_by_role("button", name="Confirm sync", exact=True)
        ).to_be_enabled()
        close(dialog, "Cancel")
        log("changed_scope_requires_explicit_review_without_mutation")
        dialog = sync_review()
        state["permission"] = False
        dialog.get_by_role("button", name="Confirm sync", exact=True).click()
        expect(dialog.get_by_role("alert")).to_contain_text("permission changed")
        assert not state["posts"]
        close(dialog, "Cancel")
        state["permission"] = True
        page.reload()
        expect(
            page.get_by_role("button", name="Sync audience", exact=True)
        ).to_be_enabled()
        log("permission_revocation_blocks_submit_before_post")
        page.get_by_role("link", name="Companies", exact=True).click()
        page.get_by_role("link", name="Audiences", exact=True).click()
        dialog = sync_review()
        dialog.get_by_role("button", name="Confirm sync", exact=True).click()
        expect(dialog.get_by_text("Sending the request.", exact=False)).to_be_visible()
        assert len(state["held"]) == 1 and len(state["posts"]) == 1
        page.evaluate("history.back()")
        guard = page.get_by_role("dialog", name="Sync request in progress", exact=True)
        expect(guard).to_be_visible()
        expect(
            guard.get_by_role("button", name="Continue", exact=True)
        ).to_be_disabled()
        guard.get_by_role("button", name="Keep editing", exact=True).click()
        state["latest"] = run()
        state["history"] = "fail"
        state["listing"] = "fail"
        state["held"].pop().fulfill(status=202, json=state["latest"])
        panel = page.get_by_role("dialog", name="Destination run history", exact=True)
        expect(
            panel.get_by_role("region", name="Selected run receipt")
        ).to_contain_text("Delivery is not confirmed yet.")
        expect(panel.get_by_role("alert")).to_contain_text(
            "confirmed action receipt is kept"
        )
        expect(
            panel.get_by_text("No sync runs have been recorded", exact=False)
        ).to_have_count(0)
        audit("queued-receipt-kept-after-history-failure")
        page.screenshot(path=str(out / "queued-history-error.png"))

        expect(
            panel.get_by_role("heading", name="Destination run history", exact=True)
        ).to_be_focused()
        log("pending_request_is_guarded_and_queued_receipt_survives_read_failure")
        expect(page.locator(".destination-card-status")).to_contain_text("Queued")
        state["listing"] = "ok"
        state["history"] = "ok"
        panel.get_by_role("button", name="Try run history again", exact=True).click()
        expect(
            panel.get_by_role("button", name="Queued qa-run-o", exact=False)
        ).to_be_visible()
        state["latest"] = run(
            "completed_with_errors", attempted=51, succeeded=46, failed=5
        )
        state["settled"] = True
        state["many"] = True
        expect(
            panel.get_by_role("region", name="Selected run receipt")
        ).to_contain_text("Finished with errors")
        expect(
            panel.get_by_text("50 delivery receipts loaded.", exact=False)
        ).to_be_visible()
        panel.get_by_role("button", name="Load more deliveries", exact=True).click()
        expect(
            panel.get_by_text("51 delivery receipts loaded.", exact=False)
        ).to_be_visible()
        expect(
            panel.get_by_role("link", name="Company record", exact=True)
        ).to_have_attribute("href", "/companies?record=" + company["id"])
        expect(
            panel.get_by_role("link", name="Person record", exact=True).first
        ).to_have_attribute("href", "/people?record=" + person["id"])
        panel.get_by_role("button", name="Load older runs", exact=True).click()
        expect(
            panel.get_by_role("group", name="Choose run").get_by_role("button")
        ).to_have_count(21)
        audit("paged-run-and-canonical-delivery-receipts")
        log("run_and_delivery_pagination_keep_canonical_identity_and_counts")
        panel.get_by_label("Delivery status", exact=True).select_option("failed")
        expect(
            panel.get_by_text("5 delivery receipts loaded.", exact=False)
        ).to_be_visible()
        state["delivery"] = "fail"
        panel.get_by_label("Delivery status", exact=True).select_option("success")
        expect(panel.get_by_role("alert")).to_contain_text(
            "Deliveries could not be loaded"
        )
        expect(
            panel.get_by_text("No deliveries with this status", exact=False)
        ).to_have_count(0)
        audit("delivery-read-error")
        state["delivery"] = "ok"
        panel.get_by_role("button", name="Try deliveries again", exact=True).click()
        expect(
            panel.get_by_text("46 delivery receipts loaded.", exact=False)
        ).to_be_visible()
        log("delivery_filter_failure_and_retry_are_not_empty_success")
        close(panel, "Close history")
        expect(
            page.get_by_role("button", name="Run history", exact=True)
        ).to_be_focused()
        page.get_by_role("button", name="Review retry", exact=True).click()
        dialog = page.get_by_role("dialog", name="Review run retry", exact=True)
        expect(
            dialog.get_by_text("Successful delivery keys are skipped.", exact=False)
        ).to_be_visible()
        audit("retry-review")
        state["post"] = "ok"
        dialog.get_by_role("button", name="Confirm retry", exact=True).click()
        panel = page.get_by_role("dialog", name="Destination run history", exact=True)
        expect(
            panel.get_by_role("region", name="Selected run receipt")
        ).to_contain_text("Queued")
        assert len(state["posts"]) == 2 and state["posts"][-1].endswith(
            "/" + rid + "/retry"
        )
        expect(
            panel.get_by_role("region", name="Selected run receipt")
        ).to_contain_text("Counts include earlier attempts")
        close(panel, "Close history")
        log("reviewed_retry_reuses_run_identity_and_keeps_receipts")
        # Cancellation of a running run is a request, not a completed cancellation.
        state["latest"] = run("running", attempted=51, succeeded=46, failed=5)
        page.reload()
        page.get_by_role("button", name="Cancel sync", exact=True).click()
        dialog = page.get_by_role("dialog", name="Review sync cancellation", exact=True)
        expect(dialog).to_contain_text("Completed deliveries remain")
        audit("cancellation-review")
        dialog.get_by_role("button", name="Confirm cancellation", exact=True).click()
        panel = page.get_by_role("dialog", name="Destination run history", exact=True)
        expect(
            panel.get_by_role("region", name="Selected run receipt")
        ).to_contain_text("Cancellation requested")
        audit("cancellation-requested")
        state["latest"] = run("cancelled", attempted=51, succeeded=46, failed=5)
        expect(
            panel.get_by_role("region", name="Selected run receipt")
        ).to_contain_text("Completed deliveries are not undone.")
        close(panel, "Close history")
        log("cancellation_receipts_distinguish_requested_from_finished")
        # A lost response may have queued a run. Refresh history; never auto-resubmit.
        state["post"] = "lost"
        state["latest"] = run("cancelled")
        page.reload()
        dialog = sync_review()
        dialog.get_by_role("button", name="Confirm sync", exact=True).click()
        expect(dialog.get_by_role("alert")).to_contain_text(
            "may have reached the server"
        )
        expect(
            dialog.get_by_role("button", name="Confirm sync", exact=True)
        ).to_be_disabled()
        post_count = len(state["posts"])
        dialog.get_by_role("button", name="Check run history", exact=True).click()
        panel = page.get_by_role("dialog", name="Destination run history", exact=True)
        expect(
            panel.get_by_role("region", name="Selected run receipt")
        ).to_contain_text("Queued")
        assert len(state["posts"]) == post_count
        audit("uncertain-write-recovery")
        close(panel, "Close history")
        log("lost_response_requires_history_review_and_never_auto_reposts")
        page.get_by_role("button", name="Refresh now", exact=True).click()
        review = page.get_by_role("dialog", name="Review audience refresh", exact=True)
        expect(review).to_contain_text(
            "Membership changes can automatically queue syncs"
        )
        expect(review).to_contain_text("Review fixture")
        audit("membership-refresh-external-effects-review")
        close(review, "Cancel")
        assert not state["audience_writes"]
        page.get_by_role("button", name="Refresh now", exact=True).click()
        review = page.get_by_role("dialog", name="Review audience refresh", exact=True)
        expect(
            review.get_by_role("button", name="Confirm refresh", exact=True)
        ).to_be_enabled()
        state["extra"] = [
            {
                **destination,
                "id": did + "-extra",
                "name": "Newly enabled target",
                "latest_run": None,
            }
        ]
        review.get_by_role("button", name="Confirm refresh", exact=True).click()
        expect(review.get_by_role("alert")).to_contain_text("changed after review")
        assert not state["audience_writes"]
        close(review, "Cancel")
        state["extra"] = []
        page.reload()
        page.get_by_role("button", name="Refresh now", exact=True).click()
        review = page.get_by_role("dialog", name="Review audience refresh", exact=True)
        review.get_by_role("button", name="Confirm refresh", exact=True).click()
        expect(page.get_by_text("Membership refreshed", exact=True)).to_be_visible()
        assert state["audience_writes"] == ["POST"]
        log("membership_refresh_reviews_enabled_targets_and_rejects_changed_scope")
        page.get_by_role("button", name="Resume schedule", exact=True).click()
        review = page.get_by_role("dialog", name="Review schedule change", exact=True)
        expect(review).to_contain_text("Review fixture")
        close(review, "Cancel")
        assert state["audience_writes"] == ["POST"]
        page.get_by_role("button", name="Resume schedule", exact=True).click()
        page.get_by_role(
            "dialog", name="Review schedule change", exact=True
        ).get_by_role("button", name="Confirm schedule", exact=True).click()
        expect(
            page.get_by_role("button", name="Pause schedule", exact=True)
        ).to_be_enabled()
        page.get_by_label("Audience refresh interval", exact=True).select_option("15")
        review = page.get_by_role("dialog", name="Review schedule change", exact=True)
        expect(review).to_contain_text("15")
        audit("schedule-activation-review")
        review.get_by_role("button", name="Confirm schedule", exact=True).click()
        expect(
            page.get_by_label("Audience refresh interval", exact=True)
        ).to_have_value("15")
        page.get_by_role("button", name="Pause schedule", exact=True).click()
        expect(
            page.get_by_text(
                "Schedule paused. Existing sync runs are not cancelled.", exact=True
            )
        ).to_be_visible()
        assert state["audience_writes"] == ["POST", "PATCH", "PATCH", "PATCH"]
        log(
            "schedule_enable_and_interval_are_reviewed_and_pause_does_not_claim_cancellation"
        )
        state["audience_fail"] = True
        page.get_by_role("button", name="Refresh now", exact=True).click()
        review = page.get_by_role("dialog", name="Review audience refresh", exact=True)
        review.get_by_role("button", name="Confirm refresh", exact=True).click()
        expect(review.get_by_role("alert")).to_contain_text(
            "change may have reached the server"
        )
        expect(
            review.get_by_role("button", name="Confirm refresh", exact=True)
        ).to_be_disabled()
        audit("audience-write-uncertainty")
        close(review, "Cancel")
        state["audience_fail"] = False
        log("refresh_write_failure_is_not_automatic_retry_or_delivery_success")

        state["latest"] = run("failed", attempted=51, succeeded=46, failed=5)
        page.reload()
        for width in [320, 390]:
            page.set_viewport_size({"width": width, "height": 844})
            page.evaluate('window.OpenGTMTheme.setPreference("dark")')
            page.get_by_role("button", name="Review retry", exact=True).click()
            dialog = page.get_by_role("dialog", name="Review run retry", exact=True)
            expect(
                dialog.get_by_role("button", name="Confirm retry", exact=True)
            ).to_be_visible()
            audit("dark-touch-review-" + str(width))
            assert page.evaluate("document.documentElement.scrollWidth===innerWidth")
            page.screenshot(path=str(out / ("review-" + str(width) + ".png")))
            close(dialog, "Cancel")
        state["permission"] = False
        page.reload()
        expect(
            page.get_by_text(
                "View only. Activation write permission is required to change destinations or send records.",
                exact=True,
            )
        ).to_be_visible()
        expect(
            page.get_by_role("button", name="Sync audience", exact=True)
        ).to_be_disabled()
        expect(
            page.get_by_role("button", name="Review retry", exact=True)
        ).to_be_disabled()
        expect(page.get_by_role("button", name="Add destination", exact=True)).to_be_disabled()
        page.get_by_role("button", name="Run history", exact=True).click()
        panel = page.get_by_role("dialog", name="Destination run history", exact=True)
        expect(panel.get_by_role("region", name="Selected run receipt")).to_be_visible()
        audit("view-only-history")
        log("touch_reflow_and_view_only_receipts_preserve_access")
        assert not errors, errors
        report["page_errors"] = errors
        browser.close()
finally:
    report["axe"] = scans
    (out / "results.json").write_text(json.dumps(report, indent=2) + "\n")
    if prior:
        req("POST", "/api/workspaces/switch", {"workspace_id": prior})
