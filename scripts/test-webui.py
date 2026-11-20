#!/usr/bin/env python3
"""Isolated browser smoke test. Requires Python Playwright, a Chromium binary, and HandBrakeCLI.
Usage: python scripts/test-webui.py --binary /tmp/transcoop --chromium /path/to/chrome
All queue paths, database, logs, and screenshots are written under a temporary directory.
"""
import argparse
import json
import os
import pathlib
import secrets
import socket
import subprocess
import tempfile
import time
import urllib.request
from playwright.sync_api import sync_playwright

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--binary', required=True)
parser.add_argument('--chromium', required=True)
args = parser.parse_args()
root = pathlib.Path(tempfile.mkdtemp(prefix='transcoop-ui-smoke-'))
token = secrets.token_hex(32)
with socket.socket() as sock:
    sock.bind(('127.0.0.1', 0))
    port = sock.getsockname()[1]
base = f'http://127.0.0.1:{port}'
def api(path, value=None, attempt='', credential=None):
    req = urllib.request.Request(base + path, data=None if value is None else json.dumps(value).encode(), headers={'Authorization': 'Bearer ' + (credential or token), 'X-Attempt-Token': attempt})
    with urllib.request.urlopen(req, timeout=5) as response:
        data = response.read()
        return json.loads(data) if data else None

config_path = root/'config.toml'
config_path.write_text('server = ' + json.dumps(base) + '\n')
credential_path = root/'config.credentials.json'
credential_path.write_text(json.dumps({'admin': token, 'clients': {base: {'token': token}}}))
credential_path.chmod(0o600)
command = [str(pathlib.Path(args.binary).resolve()), 'serve', '-config', str(config_path), '-listen', f'127.0.0.1:{port}', '-db', str(root/'farm.db'), '-log-file', str(root/'server.log')]
proc = subprocess.Popen(command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

try:
    for _ in range(100):
        try:
            api('/api/capabilities')
            break
        except OSError:
            time.sleep(.05)
    else:
        raise RuntimeError('isolated coordinator did not start')
    queue = [{'Job': {'Source': {'Path': str(root/f'source-{i}.mkv')}, 'Destination': {'File': str(root/f'output-{i}.mkv')}, 'Video': {'Encoder': 'x264'}}} for i in range(26)]
    queue_file = root/'queue.json'
    queue_file.write_text(json.dumps(queue))
    with sync_playwright() as p:
        browser = p.chromium.launch(executable_path=args.chromium, headless=True, args=['--no-sandbox'])
        page = browser.new_page(viewport={'width':1280,'height':900})
        errors = []
        page.on('pageerror', lambda e: errors.append(str(e)))
        page.goto(base)
        pair = api('/api/pairing', {'role':'browser'})
        page.get_by_label('Pairing code').fill(pair['code'])
        page.get_by_role('button', name='Pair browser', exact=True).click()
        page.get_by_role('heading', name='Dashboard', exact=True).wait_for()
        page.get_by_role('link', name='Import', exact=True).click()
        page.get_by_label('Queue JSON (maximum 64 MiB)').set_input_files(str(queue_file))
        page.get_by_role('button', name='Preview queue', exact=True).click()
        assert '26 jobs · 26 new · 0 duplicates' in page.inner_text('main')
        assert api('/api/jobs') == []
        page.get_by_role('button', name='Submit queue', exact=True).click()
        assert 'Imported 26 jobs' in page.inner_text('main')
        page.get_by_role('navigation', name='Pagination').get_by_role('link', name='Next').click()
        assert 'page=2' in page.url and 'page 2' in page.inner_text('main')
        # Polling preserves unsent filter input and keyboard focus.
        search = page.get_by_label('Filename')
        search.fill('unsent search')
        search.focus()
        with page.expect_response(lambda r: r.request.headers.get('hx-request') == 'true'):
            pass
        assert search.input_value() == 'unsent search'
        assert search.evaluate('(el) => el === document.activeElement')
        search.fill('source-0')
        page.get_by_role('button', name='Filter', exact=True).click()
        page.wait_for_url('**/jobs?q=source-0*')
        assert '1 jobs' in page.inner_text('main'), (page.url, page.inner_text('main'))
        job = api('/api/claim', {'worker':'browser-worker','encoders':['x264']})
        telemetry = {'instance':'browser-instance-1234','phase':'video encoding','pass':1,'pass_count':2,'fraction':.25,'fps':42,'average_fps':40,'eta_seconds':30,'tail':'\n'.join(f'line {i}: diagnostic' for i in range(400)) + '\n<script>throw new Error("unsafe")</script> '+token+' '+job['token']}
        api(f"/api/jobs/{job['id']}/telemetry", telemetry, job['token'])
        api('/api/workers/browser-instance-1234/status', {'name':'browser-worker','encoders':['x264'],'mode':'shared','max_jobs':4,'scheduler':'auto','cpu_target':85,'headroom':10,'cpu':42,'active':[job['id']],'reason':'Observing CPU headroom (10/20s)'})
        page.get_by_role('link', name=pathlib.Path(job['source']).name, exact=True).click()
        assert '25%' in page.inner_text('main')
        assert token not in page.content() and job['token'] not in page.content()
        assert page.locator('.log script').count() == 0
        log = page.locator('.log')
        log.evaluate('(el) => {el.scrollTop=120; el.focus()}')
        with page.expect_response(lambda r: r.request.headers.get('hx-request') == 'true'):
            pass
        page.wait_for_timeout(100)
        assert log.evaluate('(el) => el.scrollTop') == 120
        assert log.evaluate('(el) => el === document.activeElement')
        page.get_by_label('Filter log lines').fill('line 399')
        assert 'line 399' in log.inner_text() and 'line 100:' not in log.inner_text()
        page.get_by_label('Filter log lines').fill('')
        page.get_by_label('Follow').check()
        telemetry['fraction'] = .5
        telemetry['tail'] += '\nnewest line'
        api(f"/api/jobs/{job['id']}/telemetry", telemetry, job['token'])
        with page.expect_response(lambda r: r.request.headers.get('hx-request') == 'true'):
            pass
        page.wait_for_timeout(100)
        assert '50%' in page.inner_text('main')
        assert log.evaluate('(el) => el.scrollHeight-el.clientHeight-el.scrollTop') < 2
        page.screenshot(path=str(root/'desktop.png'), full_page=True)
        page.set_viewport_size({'width':390,'height':844})
        assert page.evaluate('document.documentElement.scrollWidth <= window.innerWidth')
        page.screenshot(path=str(root/'mobile.png'), full_page=True)
        api(f"/api/jobs/{job['id']}/complete", {'error':'test failure'}, job['token'])
        page.on('dialog', lambda dialog: dialog.accept())
        page.get_by_role('button', name='Retry job', exact=True).click()
        assert 'Job queued for retry' in page.inner_text('main')
        assert api(f"/api/jobs/{job['id']}")['job']['status'] == 'pending'
        page.get_by_role('link', name='Workers', exact=True).click()
        assert 'Observing CPU headroom' in page.inner_text('main')
        assert '85%' in page.inner_text('main')
        # Exercise queue-wide and individual worker controls in the mobile layout.
        controls = page.locator('.queue-state')
        controls.get_by_role('button', name='pause').click()
        assert controls.get_by_role('form', name='Execution state: paused').is_visible()
        assert api('/api/claim', {'worker':'blocked','encoders':['x264']}) is None
        controls.get_by_role('button', name='play').click()
        pair_worker = api('/api/pairing', {'role':'worker'})
        paired = api('/api/pairing/redeem', {'role':'worker','code':pair_worker['code'],'name':'controlled-worker'})
        api('/api/workers/controlled-worker-instance/status', {'name':'controlled-worker','encoders':['x264'],'mode':'shared','max_jobs':1,'scheduler':'fixed'}, credential=paired['token'])
        page.goto(base + '/workers')
        card = page.get_by_role('article').filter(has_text='controlled-worker')
        card.get_by_role('button', name='pause').click()
        assert card.get_by_role('form', name='Execution state: paused').is_visible()
        assert api('/api/claim', {'worker':'controlled-worker','encoders':['x264']}, credential=paired['token']) is None
        card.get_by_role('button', name='play').click()
        active = api('/api/claim', {'worker':'controlled-worker','encoders':['x264']}, credential=paired['token'])
        assert active is not None
        card.get_by_role('button', name='stop').click()
        assert card.get_by_role('form', name='Execution state: stopped').is_visible()
        assert api(f"/api/jobs/{active['id']}")['job']['status'] == 'interrupted'
        assert page.evaluate('document.documentElement.scrollWidth <= window.innerWidth')
        page.screenshot(path=str(root/'worker-controls-mobile.png'), full_page=True)
        controls.get_by_role('button', name='stop').click()
        assert controls.get_by_role('form', name='Execution state: stopped').is_visible()
        controls.get_by_role('button', name='play').click()
        # Prepare media directly in the browser, with recursive folder discovery.
        media_input = root/'media-input'
        media_input.mkdir()
        (media_input/'nested').mkdir()
        clip = (pathlib.Path(__file__).resolve().parents[1]/'cmd/transcoop/testdata/encoder-probe.mp4').read_bytes()
        (media_input/'clip & one.mp4').write_bytes(clip)
        (media_input/'nested'/'two.mp4').write_bytes(clip)
        page.get_by_role('link', name='Add jobs', exact=True).click()
        page.get_by_label('Folder', exact=True).fill(str(media_input))
        page.get_by_role('button', name='Go', exact=True).click()
        page.get_by_role('link', name='Use folder as input', exact=True).click()
        assert page.get_by_label('Input file or folder', exact=True).input_value() == str(media_input)
        page.get_by_label('Output folder', exact=True).fill(str(root/'media-output'))
        page.get_by_role('combobox', name='Preset').select_option('Fast 1080p30')
        page.get_by_label('Include subfolders').check()
        before = len(api('/api/jobs'))
        page.get_by_role('button', name='Preview jobs', exact=True).click()
        page.get_by_role('button', name='Add jobs to queue', exact=True).wait_for(timeout=30000)
        assert '2 jobs' in page.locator('#media-preview').inner_text()
        assert page.locator('#media-preview').get_by_title(str(media_input/'nested'/'two.mp4'), exact=True).is_visible()
        assert len(api('/api/jobs')) == before
        assert page.evaluate('document.documentElement.scrollWidth <= window.innerWidth')
        page.screenshot(path=str(root/'add-media-mobile.png'), full_page=True)
        page.get_by_role('button', name='Add jobs to queue', exact=True).click()
        page.wait_for_url('**/jobs?result=*')
        assert len(api('/api/jobs')) == before + 2
        page.get_by_role('link', name='Devices', exact=True).click()
        page.get_by_role('button', name='Create pairing code', exact=True).click()
        assert 'worker pairing code:' in page.inner_text('main')
        # Remembered browser survives a coordinator restart.
        proc.terminate()
        proc.wait(timeout=10)
        proc = subprocess.Popen(command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for _ in range(100):
            try:
                api('/api/capabilities')
                break
            except OSError:
                time.sleep(.05)
        page.goto(base)
        page.get_by_role('heading', name='Dashboard', exact=True).wait_for()
        # A paired operator can revoke a different browser immediately.
        other = browser.new_context()
        other_page = other.new_page()
        other_page.goto(base)
        other_pair = api('/api/pairing', {'role':'browser'})
        other_page.get_by_label('Device name').fill('Second browser')
        other_page.get_by_label('Pairing code').fill(other_pair['code'])
        other_page.get_by_role('button', name='Pair browser', exact=True).click()
        other_page.get_by_role('heading', name='Dashboard', exact=True).wait_for()
        page.get_by_role('link', name='Devices', exact=True).click()
        page.get_by_role('row').filter(has_text='Second browser').get_by_role('button', name='Revoke', exact=True).click()
        other_page.reload()
        other_page.get_by_role('heading', name='Pair this browser', exact=True).wait_for()
        other.close()
        page.screenshot(path=str(root/'devices-mobile.png'), full_page=True)
        page.get_by_role('button', name='Log out', exact=True).click()
        page.get_by_role('heading', name='Pair this browser', exact=True).wait_for()
        assert not errors, errors
        browser.close()
    print(f'Browser smoke checks passed. Screenshots: {root}')
finally:
    proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait()
