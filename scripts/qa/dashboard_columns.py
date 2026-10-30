"""Verify native record columns, coverage, scoped preferences and large pages.

Creates synthetic workspaces/records and restores the account's active workspace.
No enrichment or external deliveries run. --large imports 10,000 Companies and
10,000 People through the native import API in the test workspace.
"""

import argparse
import json
import os
import time
import urllib.parse
import urllib.request
from pathlib import Path
from playwright.sync_api import expect, sync_playwright

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--base-url", default="http://127.0.0.1:8002")
parser.add_argument(
    "--browser-path", default=os.environ.get("PLAYWRIGHT_CHROMIUM_EXECUTABLE")
)
parser.add_argument("--engine", choices=["chromium", "webkit"], default="chromium")
parser.add_argument("--output", default=".run/dashboard-columns")
parser.add_argument("--large", action="store_true")
parser.add_argument(
    "--fixture-report",
    help="Reuse native synthetic fixture IDs from an earlier results.json",
)
args = parser.parse_args()
root = Path(__file__).resolve().parents[2]
out = root / args.output
out.mkdir(parents=True, exist_ok=True)
base = args.base_url.rstrip("/")
headers = {}
username, password = os.environ.get("OPENGTM_QA_USERNAME"), os.environ.get(
    "OPENGTM_QA_PASSWORD"
)
if not username or not password:
    parser.error("Set OPENGTM_QA_USERNAME and OPENGTM_QA_PASSWORD")


def req(method, path, body=None, form=False):
    data = (
        None
        if body is None
        else (
            urllib.parse.urlencode(body).encode() if form else json.dumps(body).encode()
        )
    )
    with urllib.request.urlopen(
        urllib.request.Request(
            base + path,
            method=method,
            data=data,
            headers={
                **headers,
                "Content-Type": (
                    "application/x-www-form-urlencoded" if form else "application/json"
                ),
            },
        ),
        timeout=180,
    ) as response:
        return json.load(response)


headers["Authorization"] = (
    "Bearer "
    + req("POST", "/auth/token", {"username": username, "password": password}, True)[
        "access_token"
    ]
)
user = req("GET", "/auth/me")
prior = req("GET", "/api/workspaces")["active_id"]
report = {
    "engine": args.engine,
    "axe": [],
    "checks": [],
    "page_errors": [],
    "collection_requests": [],
}


def save():
    (out / "results.json").write_text(json.dumps(report, indent=2) + "\n")


def log(name):
    report["checks"].append(name)
    save()
    print(name, flush=True)


try:
    if args.fixture_report:
        previous = json.loads((root / args.fixture_report).read_text())
        a = {"id": previous["workspace_id"], "name": previous["workspace_name"]}
        b = {
            "id": previous["second_workspace_id"],
            "name": previous["second_workspace_name"],
        }
        assert a["name"].startswith("Columns Alpha ") and b["name"].startswith(
            "Columns Bravo "
        )
        stamp = previous["fixture_stamp"]
        req("POST", "/api/workspaces/switch", {"workspace_id": a["id"]})
        headers["X-Workspace-Id"] = a["id"]
        rich = req("GET", "/api/records/company/" + previous["company_id"])
        ada = req("GET", "/api/records/person/" + previous["person_id"])
        assert (
            rich["fields"]["name"] == "Atlas Columns"
            and ada["fields"]["full_name"] == "Ada Columns"
        )
    else:
        stamp = str(time.time_ns())
        a = req("POST", "/api/workspaces", {"name": "Columns Alpha " + stamp})
        b = req("POST", "/api/workspaces", {"name": "Columns Bravo " + stamp})
        req("POST", "/api/workspaces/switch", {"workspace_id": a["id"]})
        headers["X-Workspace-Id"] = a["id"]
        rich = req(
            "POST",
            "/api/records/company",
            {
                "company": {
                    "name": "Atlas Columns",
                    "domain": "atlas-columns.test",
                    "industry": "Software",
                    "city": "Paris",
                    "country": "France",
                    "description": "Native column fixture",
                    "employee_count_exact": 0,
                    "score_tier": "hot",
                    "score": 93,
                    "linkedin_url": "https://www.linkedin.com/company/fixture",
                }
            },
        )
        sparse = req(
            "POST", "/api/records/company", {"company": {"name": "Blank Columns"}}
        )
        ada = req(
            "POST",
            "/api/records/person",
            {
                "person": {
                    "full_name": "Ada Columns",
                    "email": "ada@atlas-columns.test",
                    "email_verify": {"status": "valid"},
                    "phone": "+33100000001",
                    "linkedin_url": "https://www.linkedin.com/in/ada-fixture",
                },
                "company_id": rich["id"],
                "employment": {"title": "CTO"},
            },
        )
        bo_receipt = req(
            "POST",
            "/api/record-imports",
            {
                "record_type": "person",
                "source": "qa:columns:" + stamp,
                "idempotency_key": "columns-bo-" + stamp,
                "rows": [
                    {
                        "external_id": "bo-columns",
                        "person.full_name": "Bo Columns",
                        "employment.title": "Independent analyst",
                    }
                ],
            },
        )
        assert bo_receipt["imported"] == 1 and bo_receipt["rejected"] == 0, bo_receipt
        req(
            "POST",
            "/api/records/person",
            {
                "person": {
                    "full_name": "Cy Columns",
                    "email": "cy@atlas-columns.test",
                    "email_verify_status": "invalid",
                }
            },
        )
        report.update(
            workspace_id=a["id"],
            second_workspace_id=b["id"],
            company_id=rich["id"],
            person_id=ada["id"],
        )
    report.update(
        workspace_id=a["id"],
        workspace_name=a["name"],
        second_workspace_id=b["id"],
        second_workspace_name=b["name"],
        company_id=rich["id"],
        person_id=ada["id"],
        fixture_stamp=stamp,
    )
    if args.large:
        for kind, count in [("company", 9998), ("person", 9997)]:
            existing = req("GET", "/api/records/" + kind + "?limit=1")["total"]
            if existing == 10000:
                log("native-10000-" + kind + "-fixture-reused")
                continue
            assert existing + count == 10000, (kind, existing)
            rows = [
                (
                    {
                        "company.name": "Z Fixture " + str(index).zfill(5),
                        "company.domain": "column-" + str(index) + ".test",
                    }
                    if kind == "company"
                    else {
                        "person.full_name": "Z Person " + str(index).zfill(5),
                        "person.email": "person-" + str(index) + "@columns.test",
                    }
                )
                for index in range(count)
            ]
            start = time.monotonic()
            receipt = req(
                "POST",
                "/api/record-imports",
                {
                    "record_type": kind,
                    "source": "qa:columns:" + stamp,
                    "idempotency_key": "columns-" + kind + "-" + stamp,
                    "rows": rows,
                },
            )
            assert receipt["imported"] == count and receipt["rejected"] == 0
            report[kind + "_import_seconds"] = round(time.monotonic() - start, 3)
            assert req("GET", "/api/records/" + kind + "?limit=1")["total"] == 10000
            log("native-10000-" + kind + "-fixture-ready")
    with sync_playwright() as p:
        browser = getattr(p, args.engine).launch(
            headless=True,
            executable_path=args.browser_path if args.engine == "chromium" else None,
        )

        def new_context(
            width=1440, height=1000, touch=False, theme="light", blocked=False
        ):
            context = browser.new_context(
                viewport={"width": width, "height": height},
                reduced_motion="reduce",
                has_touch=touch,
                is_mobile=touch,
            )
            context.add_init_script(
                'localStorage.setItem("yupcha_token",'
                + json.dumps(headers["Authorization"][7:])
                + ');if(!localStorage.getItem("yupcha_workspace_id"))localStorage.setItem("yupcha_workspace_id",'
                + json.dumps(a["id"])
                + ');localStorage.setItem("theme",'
                + json.dumps(theme)
                + ");"
            )
            if blocked:
                context.add_init_script(
                    "const originalSet=Storage.prototype.setItem;Storage.prototype.setItem=function(key,value){if(String(key).startsWith('opengtm:record-columns:'))throw new DOMException('Synthetic blocked preference','QuotaExceededError');return originalSet.call(this,key,value)}"
                )
            return context

        context = new_context()
        page = context.new_page()
        page.on("pageerror", lambda error: report["page_errors"].append(str(error)))

        def record_request(request):
            parsed = urllib.parse.urlsplit(request.url)
            if (
                parsed.path in ["/api/records/company", "/api/records/person"]
                and request.method == "GET"
            ):
                report["collection_requests"].append(
                    {
                        "path": parsed.path,
                        "query": dict(urllib.parse.parse_qsl(parsed.query)),
                    }
                )

        page.on("request", record_request)

        def audit(name):
            page.add_script_tag(
                path=str(root / "apps/web/node_modules/axe-core/axe.min.js")
            )
            result = page.evaluate("async()=>await axe.run(document)")
            report["axe"].append({"name": name, "violations": result["violations"]})
            save()
            assert not result["violations"], name

        def heads():
            return page.locator(".records-table thead th").all_text_contents()[1:]

        def columns():
            page.get_by_role("button", name="Columns", exact=True).click()
            dialog = page.get_by_role(
                "dialog",
                name=(
                    "Company columns" if "/companies" in page.url else "People columns"
                ),
                exact=True,
            )
            expect(dialog).to_be_visible()
            return dialog

        company_defaults = [
            "Company",
            "Domain",
            "Industry",
            "Fit",
            "Location",
            "Data completeness",
            "Updated",
        ]
        person_defaults = ["Person", "Company & role", "Work email", "Fit", "Updated"]
        page.goto(base + "/companies")
        expect(
            page.get_by_role("button", name="Atlas Columns", exact=True)
        ).to_be_visible()
        assert heads() == company_defaults, heads()
        if args.large:
            expect(page.locator(".records-table tbody tr")).to_have_count(100)
            expect(page.get_by_role("button", name="Next", exact=True)).to_be_enabled()
        page.get_by_role(
            "button",
            name="Data completeness for Atlas Columns: 6/6 core facts present",
            exact=True,
        ).click()
        popup = page.get_by_role(
            "dialog", name="Core facts for Atlas Columns", exact=True
        )
        expect(popup).to_contain_text("does not verify accuracy, freshness, or fit")
        expect(popup.get_by_text("Present", exact=True)).to_have_count(6)
        audit("native-complete-record-definition")
        page.screenshot(path=str(out / "completeness-definition.png"))
        page.keyboard.press("Escape")
        expect(
            page.get_by_role(
                "button",
                name="Data completeness for Blank Columns: 1/6 core facts present",
                exact=True,
            )
        ).to_be_visible()
        audit("default-company-columns")
        log("coverage-is-accepted-core-fact-presence-and-not-a-quality-claim")
        dialog = columns()
        for label in ["Employee count", "Phone", "Description", "LinkedIn"]:
            dialog.get_by_role("checkbox", name=label, exact=True).check()
        dialog.get_by_role("button", name="Apply columns", exact=True).click()
        rich_row = page.locator(".records-table tbody tr").filter(
            has=page.get_by_role("button", name="Atlas Columns", exact=True)
        )
        expect(rich_row.locator('[data-column="employees"]')).to_have_text("0")
        expect(rich_row.locator('[data-column="description"]')).to_have_text(
            "Native column fixture"
        )
        expect(
            rich_row.get_by_role(
                "link",
                name="LinkedIn for Atlas Columns (opens in a new tab)",
                exact=True,
            )
        ).to_have_attribute("href", "https://www.linkedin.com/company/fixture")
        audit("optional-company-fields-retain-zero-and-safe-profile-link")
        dialog = columns()
        dialog.get_by_role("button", name="Reset to default", exact=True).click()
        dialog.get_by_role("button", name="Apply columns", exact=True).click()
        page.get_by_role("checkbox", name="Select Atlas Columns", exact=True).check()
        dialog = columns()
        expect(
            dialog.get_by_role("checkbox", name="Company Pinned", exact=True)
        ).to_be_disabled()
        dialog.get_by_role("checkbox", name="Domain", exact=True).uncheck()
        assert heads() == company_defaults
        audit("column-preview-and-pinned-identity")
        dialog.get_by_role("button", name="Cancel", exact=True).click()
        expect(page.get_by_role("button", name="Columns", exact=True)).to_be_focused()
        assert heads() == company_defaults
        expect(
            page.get_by_role("checkbox", name="Select Atlas Columns", exact=True)
        ).to_be_checked()
        dialog = columns()
        dialog.get_by_role("checkbox", name="Domain", exact=True).uncheck()
        dialog.get_by_role("checkbox", name="Description", exact=True).check()
        dialog.get_by_role("button", name="Move Industry up", exact=True).click()
        expect(
            dialog.get_by_role("checkbox", name="Industry", exact=True)
        ).to_be_focused()
        audit("column-reorder-retains-keyboard-focus")
        dialog.get_by_role("button", name="Apply columns", exact=True).click()
        customized = [
            "Company",
            "Industry",
            "Fit",
            "Location",
            "Data completeness",
            "Updated",
            "Description",
        ]
        assert heads() == customized, heads()
        expect(
            page.get_by_role("checkbox", name="Select Atlas Columns", exact=True)
        ).to_be_checked()
        page.reload()
        expect(
            page.get_by_role("button", name="Atlas Columns", exact=True)
        ).to_be_visible()
        assert heads() == customized
        log("column-preview-cancel-apply-order-selection-and-reload")
        page.get_by_role("button", name="Atlas Columns", exact=True).click()
        expect(page.get_by_role("dialog")).to_contain_text("Atlas Columns")
        page.keyboard.press("Escape")
        expect(
            page.get_by_role("button", name="Atlas Columns", exact=True)
        ).to_be_focused()
        page.get_by_role("combobox", name="Filter companies", exact=True).select_option(
            "qualified"
        )
        expect(page.locator(".records-table tbody tr")).to_have_count(1)
        page.get_by_role("combobox", name="Sort records", exact=True).select_option(
            "score"
        )
        before = urllib.parse.urlsplit(page.url).query
        dialog = columns()
        dialog.get_by_role("checkbox", name="Phone", exact=True).check()
        dialog.get_by_role("button", name="Apply columns", exact=True).click()
        assert urllib.parse.urlsplit(page.url).query == before
        expect(
            page.get_by_role("button", name="Atlas Columns", exact=True)
        ).to_be_visible()
        page.get_by_role("combobox", name="Filter companies", exact=True).select_option(
            "all"
        )
        log("column-changes-preserve-inspector-and-server-query-state")
        page.get_by_role("link", name="People", exact=True).click()
        expect(
            page.get_by_role("button", name="Ada Columns", exact=True)
        ).to_be_visible()
        assert heads() == person_defaults
        ada_row = page.locator(".records-table tbody tr").filter(
            has=page.get_by_role("button", name="Ada Columns", exact=True)
        )
        bo_row = page.locator(".records-table tbody tr").filter(
            has=page.get_by_role("button", name="Bo Columns", exact=True)
        )
        cy_row = page.locator(".records-table tbody tr").filter(
            has=page.get_by_role("button", name="Cy Columns", exact=True)
        )
        expect(ada_row.get_by_text("Verified", exact=True)).to_be_visible()
        expect(cy_row.get_by_text("Invalid", exact=True)).to_be_visible()
        expect(bo_row.get_by_text("Company unknown", exact=True)).to_be_visible()
        expect(bo_row.get_by_text("Independent analyst", exact=True)).to_be_visible()
        expect(bo_row.locator("a")).to_have_count(0)
        expect(
            ada_row.get_by_role("link", name="Atlas Columns", exact=True)
        ).to_have_attribute("href", "/companies?record=" + rich["id"])
        dialog = columns()
        dialog.get_by_role("checkbox", name="Phone", exact=True).check()
        dialog.get_by_role("checkbox", name="Data completeness", exact=True).check()
        dialog.get_by_role("checkbox", name="LinkedIn", exact=True).check()
        dialog.get_by_role("button", name="Apply columns", exact=True).click()
        expect(
            ada_row.get_by_role(
                "button",
                name="Data completeness for Ada Columns: 6/6 core facts present",
                exact=True,
            )
        ).to_be_visible()
        expect(
            bo_row.get_by_role(
                "button",
                name="Data completeness for Bo Columns: 2/6 core facts present",
                exact=True,
            )
        ).to_be_visible()
        audit("people-owned-facts-and-email-verification")
        page.screenshot(path=str(out / "people-columns.png"))
        log("people-preferences-relationships-verification-and-unknown-employer")
        page.get_by_title("Switch workspace", exact=True).click()
        page.get_by_role("menuitem", name=b["name"]).click()
        page.get_by_role("link", name="Companies", exact=True).click()
        expect(
            page.get_by_role("heading", name="Companies", exact=True)
        ).to_be_visible()
        dialog = columns()
        expect(
            dialog.get_by_role("checkbox", name="Domain", exact=True)
        ).to_be_checked()
        expect(
            dialog.get_by_role("checkbox", name="Description", exact=True)
        ).not_to_be_checked()
        dialog.get_by_role("button", name="Cancel", exact=True).click()
        page.get_by_title("Switch workspace", exact=True).click()
        page.get_by_role("menuitem", name=a["name"]).click()
        page.get_by_role("link", name="Companies", exact=True).click()
        expect(
            page.get_by_role("button", name="Atlas Columns", exact=True)
        ).to_be_visible()
        assert "Description" in heads() and "Domain" not in heads()
        expect(
            page.get_by_role("checkbox", name="Select Atlas Columns", exact=True)
        ).not_to_be_checked()
        log("workspace-switch-scopes-columns-and-clears-old-record-selection")
        other = context.new_page()
        other.goto(base + "/companies")
        expect(
            other.get_by_role("button", name="Atlas Columns", exact=True)
        ).to_be_visible()
        other.get_by_role("button", name="Columns", exact=True).click()
        other.get_by_role("dialog", name="Company columns", exact=True).get_by_role(
            "button", name="Reset to default", exact=True
        ).click()
        other.get_by_role("dialog", name="Company columns", exact=True).get_by_role(
            "button", name="Apply columns", exact=True
        ).click()
        expect(
            page.get_by_role("columnheader", name="Domain", exact=True)
        ).to_be_visible()
        assert heads() == company_defaults
        other.close()
        log("cross-tab-layout-preferences-update-without-reloading-records")
        dialog = columns()
        for checkbox in dialog.get_by_role("checkbox").all():
            if checkbox.is_enabled():
                checkbox.uncheck()
        dialog.get_by_role("button", name="Apply columns", exact=True).click()
        assert heads() == ["Company"]
        expect(
            page.get_by_role("button", name="Atlas Columns", exact=True)
        ).to_be_visible()
        audit("identity-and-selection-remain-when-all-optional-columns-are-hidden")
        dialog = columns()
        dialog.get_by_role("button", name="Reset to default", exact=True).click()
        dialog.get_by_role("button", name="Apply columns", exact=True).click()
        if args.large:
            timings = []
            for index in range(5):
                start = time.monotonic()
                page.reload()
                expect(page.locator(".records-table tbody tr")).to_have_count(100)
                timings.append(round((time.monotonic() - start) * 1000, 1))
            report["company_ready_ms"] = timings
            page.get_by_role(
                "checkbox", name="Select all records on this page", exact=True
            ).check()
            page.get_by_role("button", name="Next", exact=True).click()
            expect(page).to_have_url(base + "/companies?offset=100")
            expect(page.locator(".records-table tbody tr")).to_have_count(100)
            expect(
                page.get_by_text("100 records selected", exact=False)
            ).to_be_visible()
            expect(
                page.get_by_role(
                    "checkbox", name="Select all records on this page", exact=True
                )
            ).not_to_be_checked()
            page.get_by_role("button", name="Previous", exact=True).click()
            expect(
                page.get_by_role(
                    "checkbox", name="Select all records on this page", exact=True
                )
            ).to_be_checked()
            page.get_by_role(
                "combobox", name="Filter companies", exact=True
            ).select_option("qualified")
            expect(page.locator(".records-selection")).to_have_count(0)
            expect(page.locator(".records-table tbody tr")).to_have_count(1)
            audit("large-server-filter-and-explicit-page-selection")
            page.get_by_role(
                "combobox", name="Filter companies", exact=True
            ).select_option("all")
            log("10000-record-pagination-selection-and-server-filter-remain-bounded")
        audit("final-company-layout")
        page.screenshot(path=str(out / "company-columns.png"))
        for width, height in [(320, 740), (390, 600), (768, 1000), (1280, 360)]:
            touch = new_context(width, height, width < 1000, "dark")
            mobile = touch.new_page()
            mobile.on(
                "pageerror", lambda error: report["page_errors"].append(str(error))
            )
            mobile.goto(base + "/companies")
            expect(
                mobile.get_by_role("button", name="Atlas Columns", exact=True)
            ).to_be_visible()
            desktop = page
            page = mobile
            assert page.evaluate("document.documentElement.scrollWidth<=innerWidth")
            dialog = columns()
            expect(
                dialog.get_by_role("button", name="Apply columns", exact=True)
            ).to_be_in_viewport()
            expect(
                dialog.get_by_role("button", name="Cancel", exact=True)
            ).to_be_in_viewport()
            if width < 1000:
                sizes = dialog.locator(
                    ".record-column-choice, .record-column-move button"
                ).evaluate_all("els=>els.map(el=>el.getBoundingClientRect().height)")
                assert min(sizes) >= 44
            audit("column-controls-dark-" + str(width) + "x" + str(height))
            page.screenshot(path=str(out / ("columns-dark-" + str(width) + ".png")))
            dialog.get_by_role("button", name="Cancel", exact=True).click()
            page.locator(".records-table-scroll").evaluate(
                "el=>el.scrollLeft=el.scrollWidth"
            )
            identity = page.locator(".records-table tbody tr").first.locator(
                ".records-identity"
            )
            region = page.locator(".records-table-scroll")
            assert identity.bounding_box()["x"] < region.bounding_box()["x"] + 60
            touch.close()
            page = desktop
        log("touch-short-height-dark-controls-scroll-and-identity-remains-pinned")
        blocked = new_context(blocked=True)
        blocked_page = blocked.new_page()
        blocked_page.goto(base + "/companies")
        expect(
            blocked_page.get_by_role("button", name="Atlas Columns", exact=True)
        ).to_be_visible()
        desktop = page
        page = blocked_page
        dialog = columns()
        dialog.get_by_role("checkbox", name="Domain", exact=True).uncheck()
        dialog.get_by_role("button", name="Apply columns", exact=True).click()
        expect(
            page.get_by_text("This browser could not save the preference", exact=False)
        ).to_be_visible()
        assert "Domain" not in heads()
        audit("blocked-preference-storage-keeps-current-layout")
        blocked.close()
        page = desktop
        log("blocked-browser-storage-is-honest-and-does-not-block-column-use")
        assert not report["page_errors"]
        assert all(
            int(request["query"]["limit"]) <= 100
            for request in report["collection_requests"]
        )
        browser.close()
finally:
    if prior:
        headers.pop("X-Workspace-Id", None)
        req("POST", "/api/workspaces/switch", {"workspace_id": prior})
    save()
