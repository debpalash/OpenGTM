"""Opt-in mounted DataTable selection regression with an installed browser.

OPENGTM_BUN and OPENGTM_CHROMIUM select executables; web dependencies must be
installed. No browser or dependency is downloaded by this test.
"""
import json
import os
import select
import subprocess
from pathlib import Path

import pytest


@pytest.mark.skipif(not (os.getenv("OPENGTM_BUN") and os.getenv("OPENGTM_CHROMIUM")),
                    reason="native browser/Bun executables must be explicitly supplied")
def test_lead_table_clears_selection_when_backing_records_change():
    from playwright.sync_api import sync_playwright

    root = Path(__file__).resolve().parents[1]
    bun = os.environ["OPENGTM_BUN"]
    chrome = os.environ["OPENGTM_CHROMIUM"]
    server=subprocess.Popen([bun,str(root/'tests/fixtures/lead_selection_server.ts'),str(root)],stdout=subprocess.PIPE,text=True)
    try:
        if not select.select([server.stdout],[],[],30)[0]:raise TimeoutError('startup deadline')
        line=server.stdout.readline()
        if not line:raise RuntimeError('server exited before startup')
        origin='http://127.0.0.1:'+str(json.loads(line)['port'])
        with sync_playwright() as p:
            browser=p.chromium.launch(executable_path=chrome,headless=True)
            try:
                page=browser.new_page();errors=[]
                page.on('pageerror',lambda e:errors.append(str(e)))
                page.goto(origin);page.get_by_role('checkbox',name='Select row').first.wait_for()
                page.get_by_role('checkbox',name='Select row').first.click()
                page.wait_for_function('JSON.stringify(selected)==="[1]"')
                page.evaluate('refresh()')
                unchanged=page.evaluate('selected')==[1] and page.get_by_role('checkbox',name='Select row').first.is_checked()
                page.evaluate('replace()')
                page.get_by_text('third@example.test',exact=True).wait_for()
                snapshot={'selected_ids':page.evaluate('selected'),'first_visible_lead':'second@example.test','checked':page.get_by_role('checkbox',name='Select row').first.is_checked(),'unchanged_rows_preserve_selection':unchanged,'errors':errors}
                print(json.dumps(snapshot,indent=2))
                assert snapshot['selected_ids']==[] and not snapshot['checked'] and unchanged and not errors
                page.evaluate('unmount()')
            finally:browser.close()
    finally:
        server.terminate()
        try:server.wait(timeout=5)
        except subprocess.TimeoutExpired:server.kill();server.wait(timeout=5)
