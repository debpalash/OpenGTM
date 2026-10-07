"""Opt-in real WorkbookEditor clipboard handlers against an owned API fixture.

Supply OPENGTM_BUN and OPENGTM_CHROMIUM; this test downloads no dependencies.
Clipboard writes are captured inside the page, without touching the host clipboard.
"""
import json
import os
import select
import subprocess
from pathlib import Path

import pytest


@pytest.mark.skipif(not (os.getenv("OPENGTM_BUN") and os.getenv("OPENGTM_CHROMIUM")),
                    reason="native browser/Bun executables must be explicitly supplied")
def test_workbook_clipboard_preserves_cell_boundaries():
    from playwright.sync_api import sync_playwright

    root = Path(__file__).resolve().parents[1]
    server = subprocess.Popen(
        [os.environ["OPENGTM_BUN"], str(root / "tests/fixtures/workbook_clipboard_server.ts"), str(root)],
        stdout=subprocess.PIPE, text=True,
    )
    try:
        if not select.select([server.stdout], [], [], 30)[0]:
            raise TimeoutError("fixture startup deadline")
        line = server.stdout.readline()
        if not line:
            raise RuntimeError("fixture exited before startup")
        origin = "http://127.0.0.1:" + str(json.loads(line)["port"])
        with sync_playwright() as p:
            browser = p.chromium.launch(executable_path=os.environ["OPENGTM_CHROMIUM"], headless=True)
            try:
                page = browser.new_page()
                page.route("**/*", lambda route: route.continue_() if route.request.url.startswith(origin) else route.abort())
                errors = []
                page.on("pageerror", lambda error: errors.append(str(error)))
                page.goto(origin)
                cell = page.locator('[data-grid-row="0"][data-grid-column="2"]')
                cell.wait_for()
                page.evaluate('Object.defineProperty(navigator.clipboard,"writeText",{value:async text=>window.copied=text})')
                cell.focus()
                page.keyboard.press("Meta+c")
                page.wait_for_function("window.copied !== undefined")
                copied = page.evaluate("copied")
                cases = [
                    ('"first\nsecond"\tAcme', [{"row_id": 1, "fields": {"notes": "first\nsecond", "company": "Acme"}}]),
                    ('"tab\there"\t"say ""hello"""', [{"row_id": 1, "fields": {"notes": "tab\there", "company": 'say "hello"'}}]),
                    ('plain\t\r\nnext\tcompany\r\n', [
                        {"row_id": 1, "fields": {"notes": "plain", "company": ""}},
                        {"row_id": 2, "fields": {"notes": "next", "company": "company"}},
                    ]),
                ]
                observed = []
                for index, (text, expected) in enumerate(cases):
                    cell.evaluate('''(element,text)=>{const data=new DataTransfer();data.setData("text/plain",text);
                        element.dispatchEvent(new ClipboardEvent("paste",{clipboardData:data,bubbles:true,cancelable:true}));}''', text)
                    page.wait_for_function("count=>fetch('/received').then(r=>r.json()).then(items=>items.length>=count)", arg=index + 1)
                    observed.append(page.request.get(origin + "/received").json()[index]["updates"])
                print(json.dumps({"copied": copied, "updates": observed, "errors": errors}, indent=2))
                assert copied == '"first\nsecond"'
                assert observed == [expected for _, expected in cases]
                assert not errors
                page.evaluate("unmount()")
            finally:
                browser.close()
    finally:
        server.terminate()
        try:
            server.wait(timeout=5)
        except subprocess.TimeoutExpired:
            server.kill()
            server.wait(timeout=5)
