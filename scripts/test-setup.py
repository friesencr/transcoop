#!/usr/bin/env python3
"""Exercise guided setup and CLI pairing with real HandBrake, isolated temporary state.
Requires HandBrakeCLI on PATH. Hardware encoder checks use a tiny bundled clip; no existing farm is contacted.
"""
import argparse
import json
import pathlib
import re
import secrets
import socket
import subprocess
import tempfile
import time
import urllib.request

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--binary', required=True)
args = parser.parse_args()
binary = str(pathlib.Path(args.binary).resolve())
root = pathlib.Path(tempfile.mkdtemp(prefix='transcoop-setup-smoke-'))
with socket.socket() as sock:
    sock.bind(('127.0.0.1', 0))
    port = sock.getsockname()[1]
url = f'http://127.0.0.1:{port}'
admin = secrets.token_hex(32)
server_config = root/'server.toml'
server_config.write_text('server = ' + json.dumps(url) + '\n')
credentials = root/'server.credentials.json'
credentials.write_text(json.dumps({'admin': admin, 'clients': {url: {'token': admin}}}))
credentials.chmod(0o600)

def command(*arguments, input=None, check=True):
    return subprocess.run([binary, *arguments], input=input, text=True,
                          capture_output=True, timeout=600 if arguments[0] == "setup" else 30, check=check)

def api(path):
    req = urllib.request.Request(url + '/api/' + path,
                                 headers={'Authorization': 'Bearer ' + admin})
    with urllib.request.urlopen(req, timeout=5) as response:
        return json.load(response)

def pair(role):
    text = command('pair', '-config', str(server_config), '-role', role).stdout
    return re.search(r'pairing code: ([A-Z0-9]{8})', text).group(1)

def stop(process):
    if process is None or process.poll() is not None:
        return
    process.terminate()
    try:
        process.wait(timeout=10)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait()

server = subprocess.Popen([binary, 'serve', '-config', str(server_config),
                           '-listen', f'127.0.0.1:{port}', '-log-file', str(root/'server.log')],
                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
worker = None
try:
    for _ in range(100):
        try:
            api('capabilities')
            break
        except OSError:
            time.sleep(.05)
    else:
        raise RuntimeError('coordinator did not start')
    worker_config = root/'worker.toml'
    answers = '\n'.join(['worker', url, '', 'setup-test-worker', '',
                         'transfer', 'auto', '4', '85', pair('worker'), 'no']) + '\n'
    result = command('setup', '-config', str(worker_config), input=answers)
    assert 'Worker ready:' in result.stdout, result.stdout
    assert 'Worker will accept:' in result.stdout, result.stdout
    assert 'Encoders this worker should accept' not in result.stdout, result.stdout
    command('config', 'check', '-config', str(worker_config))
    identities = []
    instances = set()
    for _ in range(2):
        worker = subprocess.Popen([binary, 'worker', '-config', str(worker_config)],
                                  stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for _ in range(100):
            reports = [r for r in api('workers') if r['instance'] not in instances]
            if reports:
                report = reports[0]
                assert report['scheduler'] == 'auto' and report['max_jobs'] == 4
                identities.append(report['device_id'])
                instances.add(report['instance'])
                break
            if worker.poll() is not None:
                raise RuntimeError('worker failed; inspect temporary worker logs')
            time.sleep(.1)
        else:
            raise RuntimeError('worker did not report presence')
        stop(worker)
        worker = None
    assert identities[0] == identities[1] and identities[0]
    operator_config = root/'operator.toml'
    operator_config.write_text('')
    command('join', '-config', str(operator_config), '-server', url,
            '-name', 'setup-test-operator', input=pair('operator') + '\n')
    assert json.loads(command('jobs', '-config', str(operator_config)).stdout) == []
    operator = next(d for d in api('devices') if d['name'] == 'setup-test-operator')
    command('devices', '-config', str(server_config), 'revoke', operator['id'])
    assert command('jobs', '-config', str(operator_config), check=False).returncode != 0
    print(f'Guided setup, worker restart, operator pairing, and revocation passed: {root}')
finally:
    stop(worker)
    stop(server)
