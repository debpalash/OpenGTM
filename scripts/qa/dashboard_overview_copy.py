"""Compare overview copy and verify concise metrics, actions and error states.

Reads local native workspace context. Summary probes are intercepted; no data
writes run. --previous-url optionally captures the prior build on the same data.
"""

import argparse
import json
import os
import urllib.parse
import urllib.request
from pathlib import Path
from playwright.sync_api import expect, sync_playwright

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--base-url", default="http://127.0.0.1:8002")
parser.add_argument("--previous-url")
parser.add_argument("--engine", choices=["chromium", "webkit"], default="chromium")
parser.add_argument(
    "--browser-path", default=os.environ.get("PLAYWRIGHT_CHROMIUM_EXECUTABLE")
)
parser.add_argument("--output", default=".run/overview-copy")
args = parser.parse_args()
base = args.base_url.rstrip("/")
root = Path(__file__).resolve().parents[2]
out = root / args.output
out.mkdir(parents=True, exist_ok=True)
username, password = os.environ.get("OPENGTM_QA_USERNAME"), os.environ.get(
    "OPENGTM_QA_PASSWORD"
)
if not username or not password:
    parser.error("Set OPENGTM_QA_USERNAME and OPENGTM_QA_PASSWORD")
with urllib.request.urlopen(
    urllib.request.Request(
        base + "/auth/token",
        data=urllib.parse.urlencode(
            {"username": username, "password": password}
        ).encode(),
        headers={"Content-Type": "application/x-www-form-urlencoded"},
    ),
    timeout=15,
) as response:
    token = json.load(response)["access_token"]
report = {"engine": args.engine, "axe": [], "page_errors": []}
fixture = {
    "companies": 25,
    "people": 0,
    "people_with_email": 0,
    "verified_emails": 0,
    "companies_needing_domain": 0,
    "running_workbooks": 0,
    "failed_workbooks": 1,
    "sampled_at": "2026-10-29T12:00:00Z",
    "imports": [],
    "workbooks": [
        {
            "id": "synthetic-copy-workbook",
            "name": "Demo — Zero-Key Enrichment",
            "status": "failed",
            "total_rows": 25,
            "completed_rows": 0,
            "updated_at": "2026-10-29T11:57:00Z",
        }
    ],
}
mode = {"read": "ok"}
with sync_playwright() as p:
    browser = getattr(p, args.engine).launch(
        headless=True,
        executable_path=args.browser_path if args.engine == "chromium" else None,
    )
    context = browser.new_context(
        viewport={"width": 1440, "height": 1000}, reduced_motion="reduce"
    )
    context.add_init_script(
        'localStorage.setItem("yupcha_token",'
        + json.dumps(token)
        + ');localStorage.setItem("theme","dark");'
    )

    def summary(route):
        if mode["read"] == "fail":
            route.fulfill(status=503, json={"detail": "Synthetic unavailable"})
        else:
            route.fulfill(json=fixture)

    context.route("**/api/records/summary", summary)
    page = context.new_page()
    page.on("pageerror", lambda error: report["page_errors"].append(str(error)))

    def audit(name):
        page.add_script_tag(
            path=str(root / "apps/web/node_modules/axe-core/axe.min.js")
        )
        result = page.evaluate("async()=>await axe.run(document)")
        report["axe"].append({"name": name, "violations": result["violations"]})
        assert not result["violations"], name

    try:
        if args.previous_url:
            page.goto(args.previous_url.rstrip("/") + "/overview")
            expect(page.locator(".overview-metric")).to_have_count(4)
            report["previous_visible_words"] = len(
                page.locator(".overview-page").inner_text().split()
            )
            page.screenshot(path=str(out / "overview-before.png"))
        page.goto(base + "/overview")
        expect(page.locator(".overview-metric")).to_have_count(4)
        expect(page.locator(".overview-next-step")).to_have_count(0)
        expect(page.locator(".overview-metric-foot")).to_have_count(0)
        expect(page.locator(".overview-context")).to_contain_text("Updated")
        expect(
            page.get_by_role("link", name="Import records", exact=True)
        ).to_have_attribute("href", "/companies?import=1")
        expect(
            page.get_by_role("link", name="1 failed workbook", exact=True)
        ).to_have_attribute("href", "/workbooks?status=failed")
        report["visible_words"] = len(
            page.locator(".overview-page").inner_text().split()
        )
        if args.previous_url:
            report["copy_reduction_percent"] = round(
                100 * (1 - report["visible_words"] / report["previous_visible_words"]),
                1,
            )
            assert report["visible_words"] < report["previous_visible_words"] * 0.75
        audit("concise-populated-overview")
        page.screenshot(path=str(out / "overview-after.png"))
        mode["read"] = "fail"
        page.get_by_role(
            "button", name="Refresh workspace overview", exact=True
        ).click()
        expect(page.get_by_role("alert")).to_contain_text(
            "Showing the last successful summary", timeout=15000
        )
        expect(page.locator(".overview-metric").first).to_contain_text("25")
        audit("stale-summary-retains-explicit-warning")
        mode["read"] = "ok"
        fixture.update(
            people=10,
            people_with_email=7,
            verified_emails=3,
            companies_needing_domain=1,
        )
        page.get_by_role("button", name="Try again", exact=True).click()
        expect(page.locator(".overview-metric").nth(2)).to_contain_text(
            "30% of 10 people"
        )
        expect(
            page.get_by_role("link", name="3 people without email", exact=True)
        ).to_have_attribute("href", "/people?filter=needs_email")
        expect(
            page.get_by_role("link", name="1 company without domain", exact=True)
        ).to_have_attribute("href", "/companies?filter=needs_domain")
        audit("concise-counts-retain-denominators-and-filtered-actions")
        for width in [320, 390, 768]:
            page.set_viewport_size({"width": width, "height": 900})
            expect(
                page.get_by_role("link", name="Import records", exact=True)
            ).to_be_in_viewport()
            assert page.evaluate(
                "document.documentElement.scrollWidth<=innerWidth"
            ), width
            audit("concise-overview-dark-" + str(width))
            page.screenshot(path=str(out / ("overview-dark-" + str(width) + ".png")))
        page.set_viewport_size({"width": 1440, "height": 1000})
        page.evaluate("document.documentElement.style.fontSize='200%'")
        assert page.evaluate("document.documentElement.scrollWidth<=innerWidth")
        audit("concise-overview-enlarged-text")
        page.evaluate("document.documentElement.style.fontSize=''")
        fixture.update(
            companies=0,
            people=0,
            people_with_email=0,
            verified_emails=0,
            companies_needing_domain=0,
            running_workbooks=0,
            failed_workbooks=0,
            workbooks=[],
        )
        page.get_by_role(
            "button", name="Refresh workspace overview", exact=True
        ).click()
        expect(
            page.get_by_role("heading", name="No workbooks yet", exact=True)
        ).to_be_visible()
        expect(page.get_by_text("No people yet", exact=True)).to_be_visible()
        expect(
            page.get_by_text(
                "No missing domains/emails or failed workbooks.", exact=True
            )
        ).to_be_visible()
        audit("concise-empty-overview-remains-actionable")
        assert not report["page_errors"]
        print(
            json.dumps(
                {
                    "axe_states": len(report["axe"]),
                    "visible_words": report["visible_words"],
                    "reduction_percent": report.get("copy_reduction_percent"),
                }
            )
        )
    finally:
        (out / "results.json").write_text(json.dumps(report, indent=2) + "\n")
        browser.close()
