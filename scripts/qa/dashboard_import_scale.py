"""Verify the 10,000-row import through the rendered native dashboard.

Creates a synthetic workspace and restores prior context. No enrichment or
external delivery runs. Files and credentials are never written into the report.
"""
import argparse
import csv
import hashlib
import io
import json
import os
import time
import urllib.parse
import urllib.request
from pathlib import Path

from playwright.sync_api import expect, sync_playwright

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--base-url', default='http://127.0.0.1:8002')
parser.add_argument('--browser-path', default=os.environ.get('PLAYWRIGHT_CHROMIUM_EXECUTABLE'))
parser.add_argument('--output', default='.run/import-scale/ui')
args = parser.parse_args()
root = Path(__file__).resolve().parents[2]
out = root / args.output
out.mkdir(parents=True, exist_ok=True)
base = args.base_url.rstrip('/')
username = os.environ.get('OPENGTM_QA_USERNAME')
password = os.environ.get('OPENGTM_QA_PASSWORD')
if not username or not password:
    parser.error('Set OPENGTM_QA_USERNAME and OPENGTM_QA_PASSWORD')
headers = {}


def req(method, path, body=None, form=False):
    data = None if body is None else (
        urllib.parse.urlencode(body).encode() if form else json.dumps(body).encode()
    )
    with urllib.request.urlopen(urllib.request.Request(
        base + path, method=method, data=data,
        headers={**headers, 'Content-Type': 'application/x-www-form-urlencoded' if form else 'application/json'},
    ), timeout=180) as response:
        return json.load(response)


headers['Authorization'] = 'Bearer ' + req('POST', '/auth/token', {'username': username, 'password': password}, True)['access_token']
prior = req('GET', '/api/workspaces')['active_id']
report = {
    'build_index_sha256': hashlib.sha256(urllib.request.urlopen(base).read()).hexdigest(),
    'checks': [], 'axe': [], 'page_errors': [], 'list_limits': [],
}


def save():
    (out / 'results.json').write_text(json.dumps(report, indent=2) + '\n')


def log(name):
    report['checks'].append(name)
    save()
    print(name, flush=True)


try:
    ws = req('POST', '/api/workspaces', {'name': 'Import Scale ' + str(time.time_ns())})
    req('POST', '/api/workspaces/switch', {'workspace_id': ws['id']})
    headers['X-Workspace-Id'] = ws['id']
    report['workspace_id'] = ws['id']
    before_jobs = req('GET', '/api/jobs?limit=100')
    source = io.StringIO()
    writer = csv.writer(source)
    writer.writerow(['Row ID', 'Company Name', 'Company Domain', 'Custom JSON'])
    for i in range(10000):
        writer.writerow([str(i).zfill(16), 'Scale Company ' + str(i).zfill(5), 'scale-company-' + str(i) + '.test', '{"number":9007199254740993}'])
    file_bytes = source.getvalue().encode()
    report['file_bytes'] = len(file_bytes)
    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True, executable_path=args.browser_path)
        context = browser.new_context(viewport={'width': 1440, 'height': 1000}, reduced_motion='reduce')
        context.add_init_script(
            'localStorage.setItem("yupcha_token",' + json.dumps(headers['Authorization'][7:]) + ');'
            'localStorage.setItem("yupcha_workspace_id",' + json.dumps(ws['id']) + ');'
            'localStorage.setItem("theme","dark");'
        )
        page = context.new_page()
        page.on('pageerror', lambda error: report['page_errors'].append(str(error)))
        attempted = {}

        def request_observer(request):
            if request.method == 'POST' and request.url == base + '/api/record-imports':
                attempted.update(request.post_data_json)
            if request.method == 'GET' and '/api/records/company?' in request.url:
                report['list_limits'].append(urllib.parse.parse_qs(urllib.parse.urlsplit(request.url).query).get('limit', []))

        page.on('request', request_observer)

        def axe(name):
            page.add_script_tag(path=str(root / 'apps/web/node_modules/axe-core/axe.min.js'))
            result = page.evaluate('async()=>await axe.run(document)')
            report['axe'].append({'name': name, 'violations': result['violations']})
            save()
            assert not result['violations'], result['violations']

        page.goto(base + '/companies?import=1')
        page.wait_for_load_state('networkidle')
        page.get_by_label('Import file', exact=True).set_input_files({'name': 'scale-companies.csv', 'mimeType': 'text/csv', 'buffer': file_bytes})
        expect(page.get_by_role('button', name='Map columns', exact=True)).to_be_enabled()
        start = time.perf_counter()
        page.get_by_role('button', name='Map columns', exact=True).click()
        expect(page.get_by_role('heading', name='Match your columns', exact=True)).to_be_visible(timeout=30000)
        report['inspection_seconds'] = round(time.perf_counter() - start, 3)
        assert page.locator('.import-mapping-row').count() == 4
        axe('ten-thousand-source-mapping')
        log('10000-source-rows-have-four-bounded-mapping-controls')
        start = time.perf_counter()
        page.get_by_role('button', name='Validate rows', exact=True).click()
        expect(page.get_by_role('heading', name='Review your import', exact=True)).to_be_visible(timeout=180000)
        report['preview_seconds'] = round(time.perf_counter() - start, 3)
        assert page.locator('.record-import-totals dd').all_text_contents() == ['10,000', '0', '10,000']
        assert req('GET', '/api/records/company?limit=1')['total'] == 0
        axe('ten-thousand-preview')
        page.screenshot(path=str(out / 'preview.png'))
        log('preview-validates-10000-rows-without-committing-records')
        start = time.perf_counter()
        page.get_by_role('button', name='Import 10,000 rows', exact=True).click()
        expect(page.get_by_role('heading', name='Import complete', exact=True)).to_be_visible(timeout=180000)
        report['commit_seconds'] = round(time.perf_counter() - start, 3)
        assert req('GET', '/api/records/company?limit=1')['total'] == 10000
        assert page.locator('.import-connected-records button').count() == 6
        assert page.locator('.record-import-errors').count() == 0
        first_id = req('GET', '/api/record-imports/' + attempted['idempotency_key'])['rows'][0]['id']
        record = req('GET', '/api/records/company/' + first_id)
        assert record['external_ids'][0]['external_id'] == '0000000000000000'
        assert record['fields']['Custom JSON']['number'] == 9007199254740993
        axe('ten-thousand-import-receipt')
        page.screenshot(path=str(out / 'receipt.png'))
        log('commit-preserves-10000-identities-and-exact-nested-number-with-bounded-results')
        start = time.perf_counter()
        replay = req('POST', '/api/record-imports', attempted)
        report['replay_seconds'] = round(time.perf_counter() - start, 3)
        assert replay['replayed'] is True and replay['imported'] == 10000 and replay['rejected'] == 0
        assert replay['rows'][0]['id'] == first_id
        assert req('GET', '/api/records/company?limit=1')['total'] == 10000
        log('same-batch-replay-returns-original-records-without-duplication')
        page.get_by_role('button', name='Select records for enrichment', exact=True).click()
        expect(page.get_by_role('dialog')).to_have_count(0)
        expect(page.get_by_role('region', name='Selected records', exact=True)).to_contain_text('10000')
        expect(page.get_by_role('table')).to_be_visible()
        expect(page.get_by_role('table').locator('tbody tr')).to_have_count(100)
        page.get_by_role('button', name='Next', exact=True).click()
        expect(page.get_by_role('table').get_by_role('button', name='Scale Company 00100', exact=True)).to_be_visible()
        assert page.get_by_role('table').locator('tbody tr').count() == 100
        assert all(limit == ['100'] for limit in report['list_limits']), report['list_limits']
        assert req('GET', '/api/jobs?limit=100') == before_jobs
        axe('ten-thousand-selected-records-paged')
        page.screenshot(path=str(out / 'selected-page-two.png'))
        log('all-imported-records-select-with-100-row-server-pages-and-no-enrichment-job')
        browser.close()
    assert not report['page_errors'], report['page_errors']
    log('no-page-errors')
finally:
    save()
    req('POST', '/api/workspaces/switch', {'workspace_id': prior})
