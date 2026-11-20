#!/usr/bin/env python3
"""Linux-only, opt-in real HandBrake comparison on generated media.

Requires ffmpeg and HandBrakeCLI on PATH. All media, databases and logs stay in
--output. Uses a fresh coordinator per mode, never the user's queue or server.
"""
import argparse
import json
import os
import pathlib
import platform
import secrets
import random
import signal
import socket
import statistics
import subprocess
import time
import urllib.error
import urllib.request


def cpu_times():
    fields = pathlib.Path('/proc/stat').read_text().splitlines()[0].split()[1:]
    v = [int(x) for x in fields[:8]]  # guest time is already included in user/nice
    return sum(v), v[3] + v[4]


def request(url, token, route, payload=None):
    data = None if payload is None else json.dumps(payload).encode()
    req = urllib.request.Request(url + route, data=data,
                                 headers={'Authorization': 'Bearer ' + token})
    with urllib.request.urlopen(req, timeout=10) as response:
        return json.load(response)


def stop(process):
    if process is not None and process.poll() is None:
        process.send_signal(signal.SIGINT)
        try:
            process.wait(timeout=15)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


def tree_rss(pid):
    try:
        status = pathlib.Path(f'/proc/{pid}/status').read_text()
        rss = next((int(line.split()[1])*1024 for line in status.splitlines() if line.startswith('VmRSS:')), 0)
        children = set()
        for thread in pathlib.Path(f'/proc/{pid}/task').iterdir():
            try:
                children.update((thread/'children').read_text().split())
            except OSError:
                pass
        return rss + sum(tree_rss(int(child)) for child in children)
    except (OSError, ValueError):
        return 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', required=True)
    parser.add_argument('--output', required=True)
    parser.add_argument('--seconds', type=int, default=120)
    parser.add_argument('--repeats', type=int, default=3)
    args = parser.parse_args()
    if args.repeats < 1 or args.seconds < 1:
        parser.error('seconds and repeats must be positive')
    root = pathlib.Path(args.output).resolve()
    root.mkdir(parents=True, exist_ok=False)
    binary = str(pathlib.Path(args.binary).resolve())
    specs = [(1280, 720, 30, 'x265', 'medium'),
             (1920, 1080, 30, 'x264', 'slow'),
             (1280, 720, 60, 'x264', 'medium'),
             (720, 480, 30, 'x265', 'slow')] * 2
    sources = {}
    for width, height, fps, _, _ in specs:
        key = (width, height, fps)
        if key in sources:
            continue
        source = root / ('source-%dx%d-%d.mkv' % key)
        subprocess.run(['ffmpeg', '-v', 'error', '-f', 'lavfi', '-i',
                        'testsrc2=size=%dx%d:rate=%d' % key, '-t', str(args.seconds),
                        '-c:v', 'libx264', '-preset', 'ultrafast', '-crf', '18',
                        '-threads', '4', str(source)], check=True)
        sources[key] = source
    results = []
    modes = [('fixed-1', ['-scheduler', 'fixed', '-max-jobs', '1']),
             ('fixed-2', ['-scheduler', 'fixed', '-max-jobs', '2']),
             ('fixed-4', ['-scheduler', 'fixed', '-max-jobs', '4']),
             ('auto-4', ['-scheduler', 'auto', '-max-jobs', '4'])]
    runs = []
    rng = random.Random(20260923)
    for repeat in range(args.repeats):
        order = modes.copy()
        rng.shuffle(order)
        runs.extend((repeat + 1, mode, flags) for mode, flags in order)
    for repeat, mode, flags in runs:
        folder = root / f'{repeat}-{mode}'
        folder.mkdir()
        token = secrets.token_hex(32)
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
        url = 'http://127.0.0.1:%d' % port
        config = folder/'config.toml'
        config.write_text('server = ' + json.dumps(url) + '\n')
        credentials = folder/'config.credentials.json'
        credentials.write_text(json.dumps({'admin': token, 'clients': {url: {'token': token}}}))
        credentials.chmod(0o600)
        server = worker = None
        with (folder / 'server.log').open('w') as server_log, (folder / 'worker.log').open('w') as worker_log:
            try:
                server = subprocess.Popen([binary, 'serve', '-config', str(config), '-log-file', '-', '-listen', '127.0.0.1:%d' % port,
                                           '-db', str(folder / 'farm.db')],                                           stdout=server_log, stderr=server_log)
                for _ in range(100):
                    try:
                        request(url, token, '/api/capabilities')
                        break
                    except (OSError, urllib.error.URLError):
                        time.sleep(.05)
                else:
                    raise RuntimeError('coordinator did not start')
                queue = []
                for index, (width, height, fps, encoder, preset) in enumerate(specs):
                    source = str(sources[(width, height, fps)])
                    queue.append({'Title': {'Geometry': {'Width': width, 'Height': height},
                                            'FrameRate': {'Num': fps, 'Den': 1}},
                                  'Job': {'SequenceID': 0, 'Source': {'Path': source, 'Title': 1, 'Angle': 1,
                                                     'Range': {'Type': 'chapter', 'Start': 1, 'End': 1}},
                                          'Destination': {'File': str(folder / ('out-%d.mkv' % index)), 'Mux': 'mkv', 'ChapterMarkers': False},
                                          'PAR': {'Num': 1, 'Den': 1},
                                          'Video': {'Encoder': encoder, 'Preset': preset, 'Quality': 22,
                                                    'MultiPass': False},
                                          'Audio': {'AudioList': []},
                                          'Subtitle': {'Search': {'Enable': False}, 'SubtitleList': []},
                                          'Filters': {'FilterList': []}}})
                (folder / 'queue.json').write_text(json.dumps(queue, indent=2))
                request(url, token, '/api/jobs', queue)
                pair = request(url, token, '/api/pairing', {'role': 'worker'})
                enrollment = request(url, '', '/api/pairing/redeem', {'role': 'worker', 'code': pair['code'], 'name': mode})
                credentials.write_text(json.dumps({'admin': token, 'clients': {url: enrollment}}))
                credentials.chmod(0o600)
                start = time.monotonic()
                previous = cpu_times()
                worker = subprocess.Popen([binary, 'worker', '-config', str(config), '-log-file', '-', '-server', url, '-name', mode,
                                           '-cpu-target', '85',
                                           '-encoders', 'x264,x265', '-mode', 'shared',
                                           '-work-dir', str(folder / 'work')] + flags,
                                          stdout=worker_log, stderr=worker_log)
                samples = []
                memory = []
                while True:
                    time.sleep(1)
                    memory.append(tree_rss(worker.pid))
                    current = cpu_times()
                    total, idle = current[0] - previous[0], current[1] - previous[1]
                    samples.append(100 * (1 - idle / total) if total else 0)
                    previous = current
                    jobs = request(url, token, '/api/jobs')
                    if any(j['status'] in ('failed', 'interrupted') for j in jobs):
                        raise RuntimeError('batch failed; inspect ' + str(folder))
                    if all(j['status'] == 'succeeded' for j in jobs):
                        break
                    if worker.poll() is not None or time.monotonic() - start > 900:
                        raise RuntimeError('worker stopped or comparison timed out')
                for job in jobs:
                    probe = subprocess.run(['ffprobe', '-v', 'error', '-show_entries', 'format=duration', '-of', 'json', job['destination']], capture_output=True, text=True, check=True)
                    duration = float(json.loads(probe.stdout)['format']['duration'])
                    if abs(duration - args.seconds) > 0.5:
                        raise RuntimeError(f'unexpected output duration: {duration}')
                result = {'mode': mode, 'repeat': repeat, 'failed_jobs': 0,
                          'peak_sampled_worker_tree_rss_mib': round(max(memory)/1024/1024, 2), 'elapsed_seconds': round(time.monotonic() - start, 2),
                          'cpu_mean_percent': round(statistics.mean(samples), 2),
                          'cpu_peak_percent': round(max(samples), 2),
                          'seconds_at_or_above_85': sum(s >= 85 for s in samples),
                          'cpu_samples': samples, 'admission_log': str(folder/'worker.log')}
                results.append(result)
                (root/'partial-results.json').write_text(json.dumps(results, indent=2) + '\n')
                print(json.dumps({k: v for k, v in result.items() if k != 'cpu_samples'}), flush=True)
            finally:
                stop(worker)
                stop(server)
    report = {'platform': platform.platform(), 'logical_cpus': os.cpu_count(),
              'handbrake_version': subprocess.run(['HandBrakeCLI', '--version'], capture_output=True,
                                                  text=True, check=True).stdout.strip(),
              'seconds_per_clip': args.seconds, 'specs': specs,
              'notes': 'Synthetic testsrc2 clips, warm input cache, repeated runs in seeded shuffled order; whole-machine CPU includes unrelated activity. RSS is sampled once per second for worker and descendants. Admission timelines are in worker.log.',
              'results': results}
    (root / 'results.json').write_text(json.dumps(report, indent=2) + '\n')


if __name__ == '__main__':
    main()
