"""Verify provider save uncertainty, draft ownership, filters and accessibility.

All provider reads/writes/tests are intercepted with synthetic settings. No real
provider credentials, settings or external calls are changed.
"""

import argparse
import copy
import json
import os
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
parser.add_argument("--output", default=".run/provider-settings")
parser.add_argument("--probe-only", action="store_true")
args = parser.parse_args()
root = Path(__file__).resolve().parents[2]
out = root / args.output
out.mkdir(parents=True, exist_ok=True)
base = args.base_url.rstrip("/")
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


def provider(id, name, configured=False, default=False):
    return {
        "id": id,
        "name": name,
        "icon": "",
        "configured": configured,
        "is_default": default,
        "model": "initial-model",
        "default_model": "default-model",
        "api_key_masked": "••••••••" if configured else "",
        "base_url": "https://example.test/v1",
        "docs_url": "https://example.test/docs",
        "free_tier": "Synthetic provider fixture",
        "openai_compatible": True,
    }


fixture = {
    "providers": [
        provider("alpha", "Synthetic Alpha", True),
        provider("beta", "Synthetic Beta"),
        provider("gamma", "Synthetic Gamma", True, True),
    ],
    "default_provider": "gamma",
}
mode = {
    "write": "partial",
    "read": "ok",
    "test": "malformed",
    "writes": 0,
    "tests": 0,
    "held": [],
    "viewer": False,
}
report = {"engine": args.engine, "axe": [], "page_errors": [], "checks": []}

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
        + ');localStorage.setItem("theme","light");'
    )

    def settings(route):
        if route.request.method == "GET":
            if mode["read"] == "fail":
                route.fulfill(status=503, json={"detail": "Synthetic unavailable"})
            elif mode["read"] == "malformed":
                route.fulfill(
                    json={"providers": [{"id": "bad"}], "default_provider": "gamma"}
                )
            else:
                route.fulfill(json=fixture)
            return
        mode["writes"] += 1
        body = route.request.post_data_json
        target = fixture["providers"][0]
        if mode["write"] == "hold":
            mode["held"].append(route)
            return
        if mode["write"] == "partial":
            target["configured"] = True
            route.fulfill(status=503, json={"detail": "Synthetic second-field failure"})
            return
        if mode["write"] == "refused":
            route.fulfill(status=422, json={"detail": "Synthetic refused update"})
            return
        if mode["write"] in ["ok", "committed-lost", "ok-read-fail"]:
            if "model" in body:
                target["model"] = body["model"] or target["default_model"]
            if body.get("api_key"):
                target["configured"] = True
            if body.get("set_default"):
                fixture["default_provider"] = target["id"]
                for item in fixture["providers"]:
                    item["is_default"] = item["id"] == target["id"]
            if mode["write"] == "committed-lost":
                route.abort("failed")
                return
            if mode["write"] == "ok-read-fail":
                mode["read"] = "fail"
        route.fulfill(
            json={
                "status": "ok",
                "provider": "other" if mode["write"] == "wrong-receipt" else "alpha",
            }
        )

    def test(route):
        mode["tests"] += 1
        if mode["test"] == "malformed":
            route.fulfill(json={"status": "ok"})
        elif mode["test"] == "fail":
            route.fulfill(status=503, json={})
        else:
            route.fulfill(
                json={"status": "ok", "response": "Synthetic connected result"}
            )

    def auth(route):
        response = route.fetch()
        data = response.json()
        if mode["viewer"]:
            data["is_admin"] = False
        route.fulfill(response=response, json=data)

    context.route("**/api/settings/providers", settings)
    context.route("**/api/settings/providers/alpha", settings)
    context.route("**/api/settings/providers/alpha/test", test)
    context.route("**/auth/me", auth)
    page = context.new_page()
    page.on("pageerror", lambda error: report["page_errors"].append(str(error)))

    def log(name):
        report["checks"].append(name)
        (out / "results.json").write_text(json.dumps(report, indent=2) + "\n")
        print(name, flush=True)

    def audit(name):
        page.add_script_tag(
            path=str(root / "apps/web/node_modules/axe-core/axe.min.js")
        )
        result = page.evaluate("async()=>await axe.run(document)")
        report["axe"].append({"name": name, "violations": result["violations"]})
        assert not result["violations"], name

    try:
        page.goto(base + "/settings")
        # Exact IDs work on the old implementation too, making this a red-capable
        # probe for the former claim that a 503 meant nothing was saved.
        model = page.locator("#provider-alpha-model")
        key = page.locator("#provider-alpha-key")
        expect(model).to_be_visible()
        key.fill("synthetic-draft-key")
        model.fill("retained-model")
        card = model.locator("xpath=ancestor::*[@data-slot='card']")
        card.get_by_role("button", name="Save", exact=True).click()
        expect(page.get_by_role("alert")).to_contain_text(
            "Some changes may have been saved"
        )
        expect(model).to_have_value("retained-model")
        expect(key).to_have_value("synthetic-draft-key")
        assert mode["writes"] == 1
        if args.probe_only:
            log("partial-write-is-never-described-as-nothing-saved")
        else:
            card = page.get_by_role(
                "region", name="Synthetic Alpha provider settings", exact=True
            )
            expect(card.get_by_role("button", name="Test", exact=True)).to_be_disabled()
            key.fill("")
            model.fill("initial-model")
            expect(
                card.get_by_role("button", name="Set default", exact=True)
            ).to_be_disabled()
            key.fill("synthetic-draft-key")
            model.fill("retained-model")
            audit("unknown-multi-field-save")
            page.screenshot(path=str(out / "unknown-save.png"))
            mode["read"] = "fail"
            card.get_by_role("button", name="Check saved settings", exact=True).click()
            expect(card.get_by_role("alert")).to_contain_text(
                "could not be loaded (503)"
            )
            expect(key).to_have_value("synthetic-draft-key")
            mode["read"] = "ok"
            card.get_by_role("button", name="Check saved settings", exact=True).click()
            expect(card.get_by_role("status")).to_contain_text(
                "API key cannot be verified"
            )
            expect(model).to_have_value("retained-model")
            expect(
                card.get_by_text("Saved model: initial-model.", exact=False)
            ).to_be_visible()
            assert mode["writes"] == 1
            audit("read-only-reconciliation-retains-unverifiable-key")
            log("partial-or-unknown-write-recovery-keeps-draft-without-reposting")
            page.get_by_role("link", name="Companies", exact=True).click()
            dialog = page.get_by_role(
                "dialog", name="Discard your changes?", exact=True
            )
            expect(dialog).to_be_visible()
            expect(dialog).to_contain_text("does not undo any saved settings")
            dialog.get_by_role("button", name="Keep editing", exact=True).click()
            mode["write"] = "hold"
            card.get_by_role("button", name="Save", exact=True).click()
            expect(model).to_be_disabled()
            page.get_by_role("tab", name="Email", exact=True).click()
            dialog = page.get_by_role("dialog", name="Save in progress", exact=True)
            expect(
                dialog.get_by_role("button", name="Discard changes", exact=True)
            ).to_be_disabled()
            audit("pending-provider-write-protects-section")
            dialog.get_by_role("button", name="Keep editing", exact=True).click()
            assert len(mode["held"]) == 1
            mode["held"].pop().fulfill(status=503, json={})
            expect(card.get_by_role("alert")).to_contain_text("Save outcome unknown")
            mode["write"] = "ok-read-fail"
            card.get_by_role("button", name="Save", exact=True).click()
            expect(key).to_have_value("")
            expect(card.get_by_role("status")).to_contain_text("Changes saved")
            expect(card.get_by_role("button", name="Save", exact=True)).to_be_disabled()
            expect(
                page.get_by_text(
                    "The latest provider settings could not be loaded.", exact=False
                )
            ).to_be_visible()
            expect(model).to_have_value("retained-model")
            audit("confirmed-receipt-survives-followup-read-failure")
            log("confirmed-save-settles-draft-independently-of-refresh")
            mode["read"] = "ok"
            page.get_by_role("link", name="Companies", exact=True).click()
            page.wait_for_url("**/companies")
            page.goto(base + "/settings")
            expect(model).to_have_value("retained-model")
            mode["write"] = "committed-lost"
            model.fill("committed-model")
            before = mode["writes"]
            card.get_by_role("button", name="Save", exact=True).click()
            expect(card.get_by_role("alert")).to_contain_text("Save outcome unknown")
            card.get_by_role("button", name="Check saved settings", exact=True).click()
            expect(card.get_by_role("status")).to_contain_text(
                "Saved model matches your draft"
            )
            expect(card.get_by_role("button", name="Save", exact=True)).to_be_disabled()
            assert mode["writes"] == before + 1
            audit("lost-model-receipt-reconciles-without-write")
            log("lost-committed-model-response-reconciles-on-read-only")
            mode["write"] = "wrong-receipt"
            model.fill("draft-after-wrong-receipt")
            card.get_by_role("button", name="Save", exact=True).click()
            expect(card.get_by_role("alert")).to_contain_text("Save outcome unknown")
            expect(model).to_have_value("draft-after-wrong-receipt")
            card.get_by_role("button", name="Check saved settings", exact=True).click()
            expect(card.get_by_role("status")).to_contain_text(
                "Your model draft is kept"
            )
            mode["write"] = "refused"
            card.get_by_role("button", name="Save", exact=True).click()
            expect(card.get_by_role("alert")).to_contain_text("Save was refused (422)")
            audit("refused-save-retains-editable-draft")
            log("wrong-success-receipt-and-validation-refusal-never-settle-draft")
            search = page.get_by_label("Search AI providers", exact=True)
            search.fill("Synthetic Beta")
            expect(card).not_to_be_visible()
            search.fill("no-match-fixture")
            expect(
                page.get_by_text("No providers match these filters.", exact=True)
            ).to_be_visible()
            page.get_by_role(
                "button", name="Clear provider filters", exact=True
            ).click()
            expect(model).to_have_value("draft-after-wrong-receipt")
            page.get_by_label("Configuration", exact=True).select_option("configured")
            expect(
                page.get_by_role("region", name="Synthetic Beta provider settings")
            ).not_to_be_visible()
            page.get_by_label("Configuration", exact=True).select_option("all")
            audit("filtered-provider-draft-survives")
            log("search-empty-and-configuration-filters-preserve-owned-drafts")
            mode["write"] = "ok-read-fail"
            model.fill("")
            card.get_by_role("button", name="Save", exact=True).click()
            expect(model).to_have_value("default-model")
            expect(card.get_by_role("button", name="Save", exact=True)).to_be_disabled()
            log("empty-model-confirmation-uses-effective-default-without-refresh")
            mode["read"] = "ok"
            mode["write"] = "committed-lost"
            before = mode["writes"]
            card.get_by_role("button", name="Set default", exact=True).click()
            expect(card.get_by_role("alert")).to_contain_text("Save outcome unknown")
            card.get_by_role("button", name="Check saved settings", exact=True).click()
            expect(card.get_by_role("status")).to_contain_text(
                "currently the default provider"
            )
            assert mode["writes"] == before + 1
            assert card.get_by_role("status").evaluate(
                "el=>el===document.activeElement"
            )
            audit("default-recovery-keeps-focus-on-receipt")
            log("lost-default-response-reconciles-without-second-write-and-keeps-focus")
            card.get_by_role("button", name="Test", exact=True).click()
            expect(
                card.get_by_role("status").filter(has_text="could not be verified")
            ).to_be_visible()
            mode["test"] = "ok"
            card.get_by_role("button", name="Test", exact=True).click()
            expect(
                card.get_by_role("status").filter(has_text="Synthetic connected result")
            ).to_be_visible()
            model.fill("not-yet-saved")
            expect(
                card.get_by_text("Synthetic connected result", exact=True)
            ).to_have_count(0)
            expect(card.get_by_role("button", name="Test", exact=True)).to_be_disabled()
            log(
                "tests-use-saved-settings-and-never-invent-success-or-retain-stale-result"
            )
            page.screenshot(path=str(out / "providers-light.png"))
            desktop = page
            for width in [320, 390, 768]:
                touch = browser.new_context(
                    viewport={"width": width, "height": 900},
                    has_touch=True,
                    is_mobile=True,
                    reduced_motion="reduce",
                )
                touch.add_init_script(
                    'localStorage.setItem("yupcha_token",'
                    + json.dumps(token)
                    + ');localStorage.setItem("theme","dark");'
                )
                touch.route("**/api/settings/providers", settings)
                touch.route("**/api/settings/providers/alpha", settings)
                touch.route("**/api/settings/providers/alpha/test", test)
                page = touch.new_page()
                page.on(
                    "pageerror", lambda error: report["page_errors"].append(str(error))
                )
                page.goto(base + "/settings")
                expect(page.locator("#provider-alpha-model")).to_be_visible()
                assert page.evaluate(
                    "document.documentElement.classList.contains('dark') && !document.documentElement.classList.contains('light')"
                )
                assert page.evaluate(
                    "document.documentElement.scrollWidth<=innerWidth"
                ), width
                controls = page.locator(
                    ".provider-settings input, .provider-settings select, .provider-settings button"
                ).evaluate_all(
                    "els=>els.filter(el=>el.getClientRects().length).map(el=>({height:el.getBoundingClientRect().height, width:el.getBoundingClientRect().width,font:parseFloat(getComputedStyle(el).fontSize),tag:el.tagName}))"
                )
                assert all(
                    c["height"] >= 44 and c["width"] >= 44 for c in controls
                ), controls
                assert all(
                    c["font"] >= 16 for c in controls if c["tag"] in ["INPUT", "SELECT"]
                ), controls
                audit("dark-touch-reflow-" + str(width))
                page.screenshot(
                    path=str(out / ("providers-dark-" + str(width) + ".png"))
                )
                touch.close()
            page = desktop
            log("narrow-dark-touch-forms-reflow-with-readable-inputs-and-44px-targets")
            before = mode["writes"]
            card.get_by_role("button", name="Discard draft", exact=True).click()
            dialog = page.get_by_role(
                "dialog", name="Discard provider draft?", exact=True
            )
            expect(dialog).to_contain_text("does not undo an earlier save")
            audit("explicit-provider-draft-discard")
            dialog.get_by_role("button", name="Keep editing", exact=True).click()
            expect(model).to_have_value("not-yet-saved")
            card.get_by_role("button", name="Discard draft", exact=True).click()
            page.get_by_role(
                "dialog", name="Discard provider draft?", exact=True
            ).get_by_role("button", name="Discard draft", exact=True).click()
            expect(model).to_have_value("default-model")
            expect(key).to_have_value("")
            expect(
                card.get_by_role("status").filter(has_text="Draft discarded")
            ).to_be_focused()
            assert mode["writes"] == before
            log("explicit-draft-discard-never-sends-or-claims-to-undo-a-save")
            mode["viewer"] = True
            readonly = context.new_page()
            readonly.on(
                "pageerror", lambda error: report["page_errors"].append(str(error))
            )
            readonly.goto(base + "/settings")
            expect(readonly.locator("#provider-alpha-model")).to_be_disabled()
            expect(
                readonly.get_by_role(
                    "region", name="Synthetic Alpha provider settings"
                ).get_by_role("button", name="Save", exact=True)
            ).to_be_disabled()
            readonly.close()
            log("instance-viewer-cannot-write-provider-settings")
            assert not report["page_errors"]
    finally:
        (out / "results.json").write_text(json.dumps(report, indent=2) + "\n")
        browser.close()
