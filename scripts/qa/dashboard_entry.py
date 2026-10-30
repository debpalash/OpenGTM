"""Verify the actual home URL renders signed-out and signed-in entry screens."""

import argparse
import json
import os
import urllib.parse
import urllib.request
from pathlib import Path
from playwright.sync_api import expect, sync_playwright

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--base-url", default="http://localhost:8002")
parser.add_argument(
    "--browser-path", default=os.environ.get("PLAYWRIGHT_CHROMIUM_EXECUTABLE")
)
parser.add_argument("--engine", choices=["chromium", "webkit"], default="chromium")
parser.add_argument("--output", default=".run/dashboard-entry")
args = parser.parse_args()
root = Path(__file__).resolve().parents[2]
out = root / args.output
out.mkdir(parents=True, exist_ok=True)
base = args.base_url.rstrip("/")
report = {"engine": args.engine, "checks": [], "page_errors": [], "failed_assets": []}
username, password = os.environ.get("OPENGTM_QA_USERNAME"), os.environ.get(
    "OPENGTM_QA_PASSWORD"
)
if not username or not password:
    parser.error("Set OPENGTM_QA_USERNAME and OPENGTM_QA_PASSWORD.")
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
try:
    with sync_playwright() as p:
        browser = getattr(p, args.engine).launch(
            headless=True,
            executable_path=args.browser_path if args.engine == "chromium" else None,
        )
        for signed_in in [False, True]:
            context = browser.new_context(
                viewport={"width": 1440, "height": 1000}, reduced_motion="reduce"
            )
            if signed_in:
                context.add_init_script(
                    'localStorage.setItem("yupcha_token",' + json.dumps(token) + ");"
                )
            page = context.new_page()
            page.on("pageerror", lambda error: report["page_errors"].append(str(error)))
            page.on(
                "response",
                lambda response: (
                    report["failed_assets"].append(
                        {
                            "path": urllib.parse.urlsplit(response.url).path,
                            "status": response.status,
                        }
                    )
                    if "/assets/" in response.url and response.status >= 400
                    else None
                ),
            )
            response = page.goto(base + "/", wait_until="domcontentloaded")
            assert response.status == 200
            try:
                expected = "/overview" if signed_in else "/login"
                expect(page).to_have_url(base + expected, timeout=15000)
                expect(page.locator("h1")).to_have_count(1)
                expect(page.locator("h1")).to_be_visible()
                if signed_in:
                    expect(
                        page.get_by_role(
                            "heading", name="Workspace overview", exact=True
                        )
                    ).to_be_visible()
                else:
                    expect(
                        page.get_by_role("button", name="Sign in", exact=True)
                    ).to_be_visible()
                page.reload()
                expect(page.locator("h1")).to_be_visible()
                page.add_script_tag(
                    path=str(root / "apps/web/node_modules/axe-core/axe.min.js")
                )
                result = page.evaluate("async()=>await axe.run(document)")
                report["checks"].append(
                    {
                        "signed_in": signed_in,
                        "route": urllib.parse.urlsplit(page.url).path,
                        "heading": page.locator("h1").inner_text(),
                        "violations": result["violations"],
                    }
                )
                assert not result["violations"]
            finally:
                page.screenshot(
                    path=str(out / ("signed-in.png" if signed_in else "signed-out.png"))
                )
                if not page.locator("h1").count():
                    report["checks"].append(
                        {
                            "signed_in": signed_in,
                            "route": urllib.parse.urlsplit(page.url).path,
                            "failure": "Home URL did not render a page heading",
                            "root_children": page.locator("#root").evaluate(
                                "el=>el.childElementCount"
                            ),
                        }
                    )
                (out / "results.json").write_text(json.dumps(report, indent=2) + "\n")
            context.close()
        browser.close()
    assert not report["page_errors"] and not report["failed_assets"], report
finally:
    (out / "results.json").write_text(json.dumps(report, indent=2) + "\n")
print(
    json.dumps(
        {"entry_checks": len(report["checks"]), "page_errors": report["page_errors"]}
    )
)
