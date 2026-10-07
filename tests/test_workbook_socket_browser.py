"""Opt-in native React/WebSocket lifecycle regression.

Run with OPENGTM_BUN and OPENGTM_CHROMIUM pointing to installed executables,
plus the frozen web dependency graph. No browser or package is downloaded.
"""
import json
import os
import select
import subprocess
from pathlib import Path

import pytest


@pytest.mark.skipif(not (os.getenv("OPENGTM_BUN") and os.getenv("OPENGTM_CHROMIUM")),
                    reason="native browser/Bun executables must be explicitly supplied")
def test_workbook_socket_updates_after_navigation_with_pending_frame():
    from playwright.sync_api import sync_playwright

    root = Path(__file__).resolve().parents[1]
    bun = os.environ["OPENGTM_BUN"]
    chrome = os.environ["OPENGTM_CHROMIUM"]
    server=subprocess.Popen([bun,str(root/'tests/fixtures/workbook_socket_server.ts'),str(root)],stdout=subprocess.PIPE,text=True)
    try:
        if not select.select([server.stdout],[],[],30)[0]: raise TimeoutError("native server startup deadline")
        line=server.stdout.readline()
        if not line: raise RuntimeError('native server exited before startup')
        origin='http://127.0.0.1:'+str(json.loads(line)['port'])
        results=[]
        with sync_playwright() as p:
            browser=p.chromium.launch(executable_path=chrome,headless=True)
            try:
                for index,mode in enumerate(['same-workbook','navigate-idle','navigate-with-pending-frame']):
                    first='A'+str(index);second='B'+str(index)
                    page=browser.new_page(); errors=[]
                    page.on('pageerror',lambda e:errors.append(str(e)))
                    page.goto(origin);page.wait_for_function('typeof window.mount === "function"')
                    page.evaluate('(ids)=>{seed(ids[0]);seed(ids[1]);mount(ids[0]);}',[first,second])
                    page.wait_for_function('(id)=>socketReady(id)',arg=first,polling=10)
                    if mode=='navigate-with-pending-frame':
                        assert page.request.get(origin+f'/send?id={first}&column=old&value=old-A').status==200
                        page.wait_for_function('pending()===1',polling=10)
                    target=first if mode=='same-workbook' else second
                    if target==second:
                        page.evaluate('(id)=>mount(id)',second)
                        page.wait_for_function('(pair)=>socketReady(pair[1])&&!socketReady(pair[0])',arg=[first,second],polling=10)
                    assert page.request.get(origin+f'/send?id={target}&column=current&value=current-{target}').status==200
                    # A real subsequent message is a protocol acknowledgement that
                    # queued earlier WebSocket events have reached the mounted hook.
                    page.request.get(origin+f'/send?id={target}&column=ack&value=seen')
                    page.wait_for_function('acks===1',polling=10)
                    pending=page.evaluate('pending()');page.evaluate('flush()')
                    snapshot=page.evaluate('(id)=>snapshot(id)',target)
                    enrichments=snapshot['rows'][0]['enrichments']
                    passed=enrichments.get('current',{}).get('value')=='current-'+target and 'old' not in enrichments and not errors
                    results.append({'mode':mode,'pass':passed,'scheduled_frames':pending,'enrichments':enrichments,'page_errors':errors})
                    page.evaluate('unmount()');page.close()
            finally:browser.close()
        print(json.dumps(results,indent=2))
        assert all(item['pass'] for item in results), results
    finally:
        server.terminate()
        try:server.wait(timeout=5)
        except subprocess.TimeoutExpired:server.kill();server.wait(timeout=5)
