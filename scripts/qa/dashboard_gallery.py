"""Check shared primitives without workspace data or external provider calls."""

import argparse
import json
import os
from pathlib import Path

from playwright.sync_api import expect, sync_playwright

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--base-url", default="http://127.0.0.1:8002")
parser.add_argument(
    "--browser-path", default=os.environ.get("PLAYWRIGHT_CHROMIUM_EXECUTABLE")
)
parser.add_argument("--output", default=".run/dashboard-gallery")
args = parser.parse_args()
root = Path(__file__).resolve().parents[2]
out = root / args.output
out.mkdir(parents=True, exist_ok=True)
report = {"checks": [], "page_errors": []}


def audit(page, label):
    page.add_script_tag(path=str(root / "apps/web/node_modules/axe-core/axe.min.js"))
    result = page.evaluate("async()=>await axe.run(document)")
    report["checks"].append({"name": label, "violations": result["violations"]})
    (out / "results.json").write_text(json.dumps(report, indent=2) + "\n")
    assert not result["violations"], label


with sync_playwright() as p:
    browser = p.chromium.launch(headless=True, executable_path=args.browser_path)
    page = browser.new_page(
        viewport={"width": 1440, "height": 1100}, reduced_motion="reduce"
    )
    page.on("pageerror", lambda e: report["page_errors"].append(str(e)))
    for theme in ["light", "dark"]:
        page.goto(args.base_url.rstrip("/") + "/ui-preview.html")
        page.get_by_label("Color theme", exact=True).select_option(theme)
        audit(page, theme)
        page.screenshot(path=str(out / (theme + ".png")), full_page=True)
        save = page.get_by_role("button", name="Try saving", exact=True)
        before = save.bounding_box()["width"]
        save.click()
        expect(save).to_have_attribute("aria-busy", "true")
        assert abs(before - save.bounding_box()["width"]) < 1
        expect(save).to_be_enabled()
        page.get_by_role("button", name="Review import", exact=True).click()
        dialog = page.get_by_role("dialog")
        expect(dialog.get_by_label("Receipt email", exact=True)).to_be_visible()
        dialog.get_by_role("button", name="Confirm preview", exact=True).click()
        expect(dialog.get_by_role("alert")).to_have_text(
            "Enter a complete email address."
        )
        expect(dialog.get_by_label("Receipt email", exact=True)).to_have_attribute(
            "aria-invalid", "true"
        )
        audit(page, theme + "-invalid-dialog")
        page.keyboard.press("Escape")
        expect(
            page.get_by_role("button", name="Review import", exact=True)
        ).to_be_focused()
    page.set_viewport_size({"width": 320, "height": 844})
    page.reload()
    expect(
        page.get_by_role("heading", name="One system. Every workflow.")
    ).to_be_visible()
    assert page.evaluate("document.documentElement.scrollWidth<=innerWidth")
    audit(page, "dark-320")
    browser.close()
assert not report["page_errors"], report["page_errors"]
print(
    json.dumps(
        {"axe_scans": len(report["checks"]), "page_errors": report["page_errors"]}
    )
)
