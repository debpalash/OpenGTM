"""Exercise native import mapping, precision, partial results and uncertain recovery.

Creates a synthetic workspace and restores the account's active workspace.
Imports synthetic .test records only; starts no enrichment or external delivery.
"""
import argparse
import hashlib
import json
import os
import re
import time
import urllib.parse
import urllib.request
from pathlib import Path
from playwright.sync_api import expect, sync_playwright

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--base-url', default='http://127.0.0.1:8002')
parser.add_argument('--engine', choices=['chromium','webkit'], default='chromium')
parser.add_argument('--browser-path', default=os.environ.get('PLAYWRIGHT_CHROMIUM_EXECUTABLE'))
parser.add_argument('--output', default='.run/import-mapping')
args=parser.parse_args()
root=Path(__file__).resolve().parents[2]
out=root/args.output; out.mkdir(parents=True,exist_ok=True)
base=args.base_url.rstrip('/')
username,password=os.environ.get('OPENGTM_QA_USERNAME'),os.environ.get('OPENGTM_QA_PASSWORD')
if not username or not password: parser.error('Set OPENGTM_QA_USERNAME and OPENGTM_QA_PASSWORD')
headers={}
def req(method,path,body=None,form=False):
    data=None if body is None else (urllib.parse.urlencode(body).encode() if form else json.dumps(body).encode())
    with urllib.request.urlopen(urllib.request.Request(base+path,method=method,data=data,headers={**headers,'Content-Type':'application/x-www-form-urlencoded' if form else 'application/json'}),timeout=180) as response: return json.load(response)
headers['Authorization']='Bearer '+req('POST','/auth/token',{'username':username,'password':password},True)['access_token']
prior=req('GET','/api/workspaces')['active_id']
report={'engine':args.engine,'base_url':base,'build_index_sha256':hashlib.sha256(urllib.request.urlopen(base).read()).hexdigest(),'checks':[],'axe':[],'page_errors':[],'requests':[]}
def save(): (out/'results.json').write_text(json.dumps(report,indent=2)+'\n')
def log(name): report['checks'].append(name); save(); print(name,flush=True)
fixture=b'Company Name,Name,Domain,Row ID,Notes\nAtlas,Atlas Labs,atlas-mapping.test,0000000000000123,North\nBeta,Beta Labs,beta-mapping.test,0000000000000124,West\nBad,Bad Labs,http://bad host,0000000000000125,South\n'
try:
    workspace=req('POST','/api/workspaces',{'name':'Import Mapping '+str(time.time_ns())})
    report['workspace_id']=workspace['id']
    req('POST','/api/workspaces/switch',{'workspace_id':workspace['id']})
    headers['X-Workspace-Id']=workspace['id']
    before_jobs=req('GET','/api/jobs?limit=100')
    with sync_playwright() as p:
        browser=getattr(p,args.engine).launch(headless=True,executable_path=args.browser_path if args.engine=='chromium' else None)
        context=browser.new_context(viewport={'width':1440,'height':1000},reduced_motion='reduce')
        context.add_init_script('localStorage.setItem("yupcha_token",'+json.dumps(headers['Authorization'][7:])+');if(!localStorage.getItem("yupcha_workspace_id"))localStorage.setItem("yupcha_workspace_id",'+json.dumps(workspace['id'])+');localStorage.setItem("theme","light");')
        page=context.new_page(); page.on('pageerror',lambda error:report['page_errors'].append(str(error)))
        def capture_request(request):
            if '/api/record-imports' in request.url:
                body=request.post_data_json if request.method=='POST' else None
                report['requests'].append({'method':request.method,'path':request.url.split(base)[-1],'key':body.get('idempotency_key') if body else None})
        page.on('request',capture_request)
        def axe(name):
            page.add_script_tag(path=str(root/'apps/web/node_modules/axe-core/axe.min.js'))
            result=page.evaluate('async()=>await axe.run(document)')
            report['axe'].append({'name':name,'violations':result['violations']});save()
            if result['violations']:
                page.screenshot(path=str(out/(name+'-failure.png')))
                (out/(name+'-failure-dom.json')).write_text(json.dumps(page.evaluate('''()=>({dialogs:[...document.querySelectorAll('[role=dialog]')].map(el=>({text:el.innerText.slice(0,80),hidden:el.getAttribute('aria-hidden'),inert:el.inert})),main:[...document.querySelectorAll('main,main h1')].map(el=>({tag:el.tagName,hidden:el.getAttribute('aria-hidden'),inert:el.inert,color:getComputedStyle(el).color,opacity:getComputedStyle(el).opacity})),body:[...document.body.children].map(el=>({tag:el.tagName,hidden:el.getAttribute('aria-hidden'),inert:el.inert}))})'''),indent=2))
            assert not result['violations'],[(v['id'],[n['target'] for n in v['nodes']]) for v in result['violations']]
        def geometry():
            data=page.get_by_role('dialog').evaluate('''el=>{const r=el.getBoundingClientRect();return {left:r.left,right:r.right,top:r.top,bottom:r.bottom,inner:el.scrollWidth,outer:el.clientWidth,width:innerWidth,height:innerHeight}}''')
            assert data['left']>=-1 and data['right']<=data['width']+1 and data['top']>=-1 and data['bottom']<=data['height']+1,data
            assert data['inner']<=data['outer']+1,data
            actions=page.get_by_role('dialog').locator('.record-import-footer button').evaluate_all('els=>els.map(el=>{const r=el.getBoundingClientRect();return {left:r.left,right:r.right,top:r.top,bottom:r.bottom}})')
            assert all(action['left']>=-1 and action['right']<=data['width']+1 and action['top']>=-1 and action['bottom']<=data['height']+1 for action in actions),actions
        def upload(data=fixture,name='clay.csv'):
            page.get_by_label('Import file',exact=True).set_input_files({'name':name,'mimeType':'application/json' if name.endswith('.json') else 'text/csv','buffer':data})
            expect(page.get_by_role('button',name='Map columns',exact=True)).to_be_enabled()
        page.goto(base+'/companies?import=1');page.wait_for_load_state('networkidle')
        axe('choose-file-light');geometry()
        upload();page.get_by_role('button',name='Map columns',exact=True).click()
        expect(page.get_by_role('heading',name='Match your columns')).to_be_visible()
        expect(page.get_by_role('button',name='Validate rows',exact=True)).to_be_disabled()
        expect(page.get_by_label('Map Company Name',exact=True)).to_have_attribute('aria-invalid','true')
        expect(page.get_by_label('Map Name',exact=True)).to_have_attribute('aria-invalid','true')
        assert req('GET','/api/records/company?limit=1')['total']==0
        log('inspection-is-stateless-and-duplicate-suggestions-remain-editable')
        page.get_by_role('region',name='Company mappings',exact=True).get_by_text('Source samples · exact ID',exact=True).click()
        expect(page.get_by_text('0000000000000123',exact=True).first).to_be_visible()
        axe('conflicting-mapping-light');page.screenshot(path=str(out/'conflicting-mapping-light.png'))
        target=page.get_by_label('Map Name',exact=True);target.fill('');target.press_sequentially('company.legal_name')
        expect(target).to_be_focused();expect(target).to_have_value('company.legal_name')
        page.get_by_label('Map Notes',exact=True).fill('company.region')
        page.get_by_role('button',name='Regroup fields',exact=True).click()
        expect(page.get_by_role('region',name='Custom mappings',exact=True).get_by_label('Map Name',exact=True)).to_be_visible()
        page.get_by_role('button',name='Validate rows',exact=True).click()
        expect(page.get_by_role('heading',name='Review your import',exact=True)).to_be_visible()
        expect(page.get_by_role('region',name='Rejected rows',exact=True)).to_contain_text('Row 3')
        expect(page.get_by_role('button',name='Import 2 rows',exact=True)).to_be_enabled()
        assert req('GET','/api/records/company?limit=1')['total']==0
        axe('partial-review-light');page.screenshot(path=str(out/'partial-review-light.png'))
        page.get_by_role('button',name='Back',exact=True).click()
        expect(page.get_by_label('Map Name',exact=True)).to_have_value('company.legal_name')
        page.get_by_role('button',name='Back',exact=True).click()
        expect(page.get_by_label('Source table name',exact=True)).to_have_value('clay:prospects')
        page.get_by_role('button',name='Map columns',exact=True).click()
        expect(page.get_by_label('Map Notes',exact=True)).to_have_value('company.region')
        page.get_by_role('button',name='Validate rows',exact=True).click()
        expect(page.get_by_role('heading',name='Review your import',exact=True)).to_be_visible()
        log('keyboard-input-focus-and-back-navigation-preserve-mapping')
        committed={}
        def lose_response(route):
            response=route.fetch();committed.update(response.json());route.abort('failed')
        page.route('**/api/record-imports',lose_response,times=1)
        page.get_by_role('button',name='Import 2 rows',exact=True).click()
        expect(page.get_by_role('alert')).to_contain_text('Check batch status')
        assert committed['imported']==2 and committed['rejected']==1,committed
        stored=page.evaluate('Object.entries(sessionStorage).filter(([k])=>k.startsWith("opengtm-import:"))')
        assert len(stored)==1
        draft=json.loads(stored[0][1]);assert draft['mapping']['Name']=='company.legal_name'
        assert 'rows' not in draft and 'csv' not in draft and 'atlas-mapping.test' not in stored[0][1]
        same_key=draft['key']
        page.get_by_role('button',name='Back',exact=True).click()
        expect(page.get_by_label('Map Name',exact=True)).to_be_disabled()
        page.route('**/api/record-imports/'+same_key,lambda route:route.fulfill(status=404,content_type='application/json',body='{"detail":"No receipt"}'),times=1)
        page.get_by_role('button',name='Check batch status',exact=True).click()
        expect(page.get_by_role('alert')).to_contain_text('may still be running')
        expect(page.get_by_label('Map Name',exact=True)).to_be_disabled()
        axe('unknown-import-locked-light')
        # A read-only recovery after reload must never POST the batch again.
        writes_before=len([r for r in report['requests'] if r['method']=='POST' and r['path']=='/api/record-imports'])
        page.reload();page.wait_for_load_state('networkidle')
        expect(page.get_by_text('Check your last batch',exact=True)).to_be_visible()
        page.get_by_role('button',name='Check batch status',exact=True).click()
        expect(page.get_by_role('heading',name='Completed batch recovered',exact=True)).to_be_visible()
        assert len([r for r in report['requests'] if r['method']=='POST' and r['path']=='/api/record-imports'])==writes_before
        assert req('GET','/api/records/company?limit=1')['total']==2
        assert page.evaluate('Object.keys(sessionStorage).filter(k=>k.startsWith("opengtm-import:"))')==[]
        axe('recovered-results-light');page.screenshot(path=str(out/'recovered-results-light.png'))
        log('lost-response-missing-receipt-and-reload-recovery-keep-one-batch-without-reposting')
        page.get_by_role('button',name='View row 1 record',exact=True).click()
        expect(page.get_by_role('dialog',name='Atlas',exact=True)).to_be_visible()
        expect(page.get_by_role('dialog',name='Atlas',exact=True)).to_contain_text('Atlas')
        first=req('GET','/api/records/company/'+committed['rows'][0]['id'])
        assert first['fields']['legal_name']=='Atlas Labs' and first['fields']['region']=='North',first
        assert first['external_ids'][0]['external_id']=='0000000000000123',first
        log('accepted-row-opens-canonical-record-with-custom-fields-and-exact-source-id')
        # Use SPA navigation so any browser full-navigation exception is independently visible.
        page.get_by_role('button',name='Close',exact=True).click()
        page.get_by_role('button',name=re.compile(r'^Import(?: from Clay)?$')).click()
        raw=b'[{"ID":9007199254740993,"Company Name":"Exact JSON","Domain":"exact-json-mapping.test","Notes":{"digits":9007199254740995}}]'
        upload(raw,'exact.json');page.get_by_role('button',name='Map columns',exact=True).click()
        expect(page.get_by_role('heading',name='Match your columns')).to_be_visible()
        page.get_by_text('Source samples · exact ID',exact=True).first.click()
        expect(page.get_by_text('9007199254740993',exact=True).first).to_be_visible()
        # Failed and malformed previews keep the file/mapping and never reach review.
        page.route('**/api/record-imports/preview',lambda route:route.fulfill(status=503,content_type='application/json',body='{"detail":"Synthetic preview unavailable"}'),times=1)
        page.get_by_role('button',name='Validate rows',exact=True).click()
        expect(page.get_by_role('alert')).to_contain_text('Synthetic preview unavailable')
        expect(page.get_by_label('Map ID',exact=True)).to_have_value('external_id')
        page.route('**/api/record-imports/preview',lambda route:route.fulfill(status=200,content_type='application/json',body='{"preview":true}'),times=1)
        page.get_by_role('button',name='Validate rows',exact=True).click()
        expect(page.get_by_role('alert')).to_contain_text('preview could not be verified')
        page.get_by_role('button',name='Validate rows',exact=True).click()
        expect(page.get_by_role('heading',name='Review your import',exact=True)).to_be_visible()
        page.get_by_role('button',name='Import 1 row',exact=True).click()
        expect(page.get_by_role('heading',name='Import complete',exact=True)).to_be_visible()
        page.get_by_role('button',name='Select records for enrichment',exact=True).click()
        expect(page.get_by_role('dialog')).to_have_count(0)
        expect(page.get_by_role('button',name='Open workbook',exact=True)).to_be_visible()
        assert req('GET','/api/jobs?limit=100')==before_jobs
        log('numeric-json-preview-errors-and-separate-enrichment-selection-preserve-data-without-running-jobs')
        # Reopen at native receipt and inspect the stored JSON through the API.
        exact=req('GET','/api/records/company?limit=100&search=Exact')['records'][0]
        exact=req('GET','/api/records/company/'+exact['id'])
        assert exact['external_ids'][0]['external_id']=='9007199254740993',exact
        assert exact['fields']['Notes']['digits']==9007199254740995,exact
        # Block only import metadata persistence, then lose the write response.
        page.evaluate("() => {window.__importStorageSet=Storage.prototype.setItem;Storage.prototype.setItem=function(key,value){if(String(key).startsWith('opengtm-import:'))throw new DOMException('Synthetic blocked recovery','QuotaExceededError');return window.__importStorageSet.call(this,key,value)}}")
        page.get_by_role('button',name=re.compile(r'^Import(?: from Clay)?$')).click()
        upload(b'Name,Domain\nStorage Recovery,storage-mapping.test\n','storage.csv')
        page.get_by_role('button',name='Map columns',exact=True).click()
        expect(page.get_by_role('heading',name='Match your columns')).to_be_visible()
        page.get_by_role('button',name='Validate rows',exact=True).click()
        expect(page.get_by_role('heading',name='Review your import',exact=True)).to_be_visible()
        page.route('**/api/record-imports',lose_response,times=1)
        page.get_by_role('button',name='Import 1 row',exact=True).click()
        expect(page.get_by_role('alert')).to_contain_text('Check batch status')
        expect(page.get_by_text('Reload recovery could not be saved in this browser.',exact=False)).to_be_visible()
        assert page.evaluate('Object.keys(sessionStorage).filter(k=>k.startsWith("opengtm-import:"))')==[]
        axe('blocked-recovery-storage-light')
        page.get_by_role('button',name='Check batch status',exact=True).click()
        expect(page.get_by_role('heading',name='Completed batch recovered',exact=True)).to_be_visible()
        expect(page.get_by_text('Reload recovery could not be saved in this browser.',exact=False)).to_have_count(0)
        page.get_by_role('button',name='Done',exact=True).click()
        expect(page.get_by_role('dialog')).to_have_count(0)
        page.evaluate('Storage.prototype.setItem=window.__importStorageSet;delete window.__importStorageSet')
        log('blocked-browser-storage-discloses-recovery-limit-and-in-memory-receipt-clears-warning')
        # Change theme through the product so root tokens and portal context agree.
        page.get_by_title('Change appearance',exact=True).click()
        page.get_by_role('menuitem',name='Dark',exact=True).click()
        expect(page.locator('html')).to_have_class(__import__('re').compile(r'\bdark\b'))
        for width,height in [(1440,760),(1024,600),(390,844),(320,568)]:
            page.set_viewport_size({'width':width,'height':height})
            page.get_by_role('button',name=re.compile(r'^Import(?: from Clay)?$')).click();upload()
            page.get_by_role('button',name='Map columns',exact=True).click()
            expect(page.get_by_role('heading',name='Match your columns')).to_be_visible();geometry()
            target=page.get_by_label('Map Name',exact=True);target.focus();target.press('Escape')
            expect(page.get_by_role('heading',name='Close import?',exact=True)).to_be_visible()
            page.get_by_role('button',name='Keep editing',exact=True).click()
            expect(page.get_by_role('heading',name='Close import?',exact=True)).not_to_be_visible()
            expect(page.get_by_role('dialog')).to_have_count(1)
            assert page.locator('h1').evaluate("el=>!!el.closest('[aria-hidden=true],[inert]')")
            axe('mapping-dark-'+str(width));page.screenshot(path=str(out/('mapping-dark-'+str(width)+'.png')))
            page.get_by_role('button',name='Cancel',exact=True).click()
            page.get_by_role('button',name='Discard changes',exact=True).click()
            expect(page.get_by_role('dialog')).to_have_count(0)
        log('short-height-and-320px-dark-dialogs-keep-footer-visible-and-guard-escape')
        touch_context=browser.new_context(storage_state=context.storage_state(),viewport={'width':390,'height':844},has_touch=True,is_mobile=True,reduced_motion='reduce')
        page=touch_context.new_page();page.on('pageerror',lambda error:report['page_errors'].append(str(error)))
        page.goto(base+'/companies?import=1');page.wait_for_load_state('networkidle')
        upload();page.get_by_role('button',name='Map columns',exact=True).click()
        expect(page.get_by_role('heading',name='Match your columns')).to_be_visible()
        controls=page.get_by_role('dialog').locator('button,input:not([type=file]),summary').evaluate_all("els=>els.filter(el=>el.getBoundingClientRect().width>0).map(el=>({tag:el.tagName,height:el.getBoundingClientRect().height}))")
        assert all(control['height']>=44 for control in controls),controls
        for width in [390,320]:
            page.set_viewport_size({'width':width,'height':844 if width==390 else 568});geometry();axe('mapping-touch-dark-'+str(width));page.screenshot(path=str(out/('mapping-touch-dark-'+str(width)+'.png')))
        page.add_style_tag(content='*{letter-spacing:.12em!important;word-spacing:.16em!important;line-height:1.5!important}p{margin-bottom:2em!important}')
        geometry();axe('mapping-touch-text-spacing');page.screenshot(path=str(out/'mapping-touch-text-spacing.png'))
        log('touch-controls-44px-and-text-spacing-reflow')
        browser.close()
    assert not report['page_errors'],report['page_errors']
    log('no-page-errors')
finally:
    save();req('POST','/api/workspaces/switch',{'workspace_id':prior})
