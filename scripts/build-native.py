#!/usr/bin/env python3
"""Build coordinated transcoop binaries on the target OS; requires its FUSE SDK."""
import os
import pathlib
import platform
import subprocess
import zipfile

root = pathlib.Path(__file__).resolve().parents[1]
out = root / 'bin'
out.mkdir(exist_ok=True)
suffix = '.exe' if os.name == 'nt' else ''
env = os.environ.copy()
env['CGO_ENABLED'] = '0'
subprocess.run(['go', 'build', '-trimpath', '-o', str(out / ('transcoop' + suffix)), './cmd/transcoop'], cwd=root, env=env, check=True)
env['CGO_ENABLED'] = '0' if os.name == 'nt' else '1'
if platform.system() == 'Linux' and platform.machine() in ('x86_64', 'AMD64'):
    # Portable helper: distributions such as CachyOS default to newer x86-64
    # levels, which would stop the helper from starting on older CPUs.
    env.setdefault('CGO_CFLAGS', '-O2 -march=x86-64 -mtune=generic')
subprocess.run(['go', 'build', '-trimpath', '-tags', 'fuse', '-o', str(out / ('transcoop-fs' + suffix)), './cmd/transcoop-fs'], cwd=root, env=env, check=True)
if platform.system() == 'Linux':
    # The C runtime objects of such distributions also mark the binary with
    # their ISA level; the helper's own code is baseline, so drop the marker.
    subprocess.run(['objcopy', '--remove-section', '.note.gnu.property', str(out / 'transcoop-fs')], check=False)
archive = out / f'transcoop-{platform.system().lower()}-{platform.machine().lower()}.zip'
with zipfile.ZipFile(archive, 'w', zipfile.ZIP_DEFLATED) as package:
    for name in ('transcoop', 'transcoop-fs'):
        package.write(out / (name + suffix), name + suffix)
    package.write(root / 'docs' / 'filesystem.md', 'filesystem.md')
print(archive)
