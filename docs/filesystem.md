# Streaming filesystem storage

`worker.mode = "fuse"` uses a separate `transcoop-fs` process to mount assigned
inputs and stream seekable output to coordinator staging. Transfer and shared
storage retain their existing behavior and do not load FUSE libraries.

Streaming storage is experimental. Linux workers using libfuse 2 and Windows 11
workers using WinFsp 2.1 have completed real HandBrake encodes over native
mounts. Native macOS validation is pending. See the platform results in the
[validation record](filesystem-validation.md).

## Install and configure

Install the driver and build both binaries on the target operating system:

| Platform | Driver / build prerequisites |
| --- | --- |
| Linux | FUSE 2 runtime, `fusermount`, `libfuse-dev`, GCC, and access to `/dev/fuse` |
| macOS | [macFUSE](https://macfuse.github.io/), enabled in system settings, and Xcode command line tools |
| Windows | [WinFsp](https://winfsp.dev/); the helper uses cgofuse's non-cgo Windows build |

The native build requirements are documented by
[cgofuse](https://github.com/winfsp/cgofuse). Run `python scripts/build-native.py`
to build and package the main executable and helper together in `bin/`.
Install them in the same directory, or put the helper on `PATH`.

Existing configurations remain unchanged. Guided setup recommends FUSE only
after a real mount, write, seek, read, and unmount preflight succeeds. A
configured FUSE worker fails clearly if its driver, helper, mount, or coordinator
capability is unavailable; it never silently switches storage modes.

```toml
[worker]
mode = "fuse"
work_dir = "worker-data"

# Optional; defaults normally suffice. Values are bytes, not MiB.
[worker.fuse]
# mount_dir = "worker-data/mount"
# cache_dir = "worker-data/cache"
ram_bytes = 268435456
disk_bytes = 8589934592
dirty_bytes = 536870912
read_concurrency = 4
write_concurrency = 2
```

Relative configured directories resolve beside the TOML file. Default mount and
cache directories live beneath `worker.work_dir`. They must be separate and
must not contain each other. OS locks prevent workers from sharing either.
On Linux the worker clears a stale Transcoop mount after acquiring its lock.
If the worker reports an unverified mount, stop the old worker and unmount its
filesystem with the platform's mount tools before retrying.
Marked cache directories from a previous helper are removed under the cache lock.

## Behavior and safety

Each attempt gets a namespace such as `job-12-attempt-2/inputs` and `output`.
Credentials travel through private process pipes, never mounted filenames or
helper arguments. Input file aliases preserve extensions. Disc directories retain
their tree and folder review treats each disc as a single job. Only the source
and paths explicitly declared under `Job.Subtitle` are exposed. Existing preset
snapshot subtitle restrictions still apply; imported HandBrake queue jobs support
external subtitle paths.

The coordinator persists a versioned manifest, file identities, metadata, and
operation sequence state in additive SQLite tables. Reads address node IDs,
validate file identity/size/mtime, use confined `os.Root` access, and return a
SHA-256 checksum. Symlink entries, special files, nonportable tree names, and
case collisions are rejected. Sources must remain immutable throughout an encode.

The worker shares its RAM LRU and disk LRU across attempts. Reads use 1 MiB blocks,
in-flight deduplication, and up to 8 MiB speculative forward read-ahead. Scheduling
rotates between attempts and prioritizes demanded requests. OS page caching,
protocol buffers, and filesystem allocation overhead are additional to cache
payload budgets. The disk budget reserves the configured dirty limit; the rest
is available for clean blocks.

Writes are journaled before acknowledgement, then sent in ordered batches of up
to 4 MiB or 250 ms. Dirty records are never evicted to admit other work. A full
dirty budget applies backpressure. Remote acknowledgements follow staging file
sync and committed sequence state, without a database transaction over bulk I/O.
Duplicate operations must match their original digest. Output reads combine a stable acknowledged remote prefix with local journal
records, so overlapping writes, truncation, sparse regions, and header rewrites
see the current contents.

File close does not publish. After HandBrake succeeds, the helper drains and seals
output. Completion checks the live lease and seal, then uses the existing atomic
no-overwrite publication path. Filesystem sessions survive coordinator restarts.
Inactive staging sessions are cleaned every 30 seconds and on subsequent
claim/preview requests.
Worker/helper crashes interrupt attempts; old encodes and journals are not resumed.

Network retries are bounded by 45 seconds and the last confirmed lease deadline.
Explicit authorization or lease rejection stops retries. Mount health, cumulative
bytes, throughput, cache use, dirty backlog, retry count, and I/O waits are reported
in worker telemetry and shown in worker cards. Auto admission pauses for reported
backpressure or waits and excludes affected executions from workload learning.

## Validation gates

Run ordinary tests with `go test -race ./...`. Build and test the adapter with
`go test -tags fuse ./cmd/transcoop-fs` on machines with the driver SDK.

The opt-in native gate mounts through the real helper, checks seek/read/write,
encodes four concurrent MKV/MP4 outputs with real HandBrake, checks that final
paths stay absent before completion, and scans the completed outputs:

```sh
python scripts/build-native.py
PATH="$PWD/bin:$PATH" TRANSCOOP_FUSE_NATIVE=1 go test ./internal/farm -run TestFuseNativeHandBrake -v
```

On Windows, add `bin` to `PATH` and set `TRANSCOOP_FUSE_NATIVE=1` using the shell's native
syntax. `.github/workflows/filesystem-native.yml` runs on explicitly provisioned
self-hosted Linux/macOS/Windows runners with their drivers enabled.

The following release checks remain open.

- Mount and encode on macOS, verify macFUSE permissions, and check Windows
  namespace durability across failures.
- Exercise real DVD/Blu-ray fixtures, multipass encodes, external subtitles,
  disk exhaustion, and coordinator restarts during an active native encode.
- Performance comparison with transfer using the same DVD image file and settings:
  startup time, encode throughput, network burst sizes, remote bytes read, peak
  cache/dirty bytes, and completion delay. A directory source cannot use the
  existing single-file transfer mode as a direct baseline.
- Evidence that encoding starts before the complete input is fetched and that
  staging receives output while HandBrake is running.

Do not call these gates passed based on cross-compilation, protocol unit tests,
or a skipped native test. Keep deployment explicitly selected until those results
are available.
