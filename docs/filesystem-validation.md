# Filesystem validation record — 2026-09-24

Environment: Linux, Go 1.26.4, FUSE development headers installed, `/dev/fuse`
absent. Native macOS and Windows hosts were not available.

| Check | Result |
| --- | --- |
| Existing transfer/shared tests, configuration, review, and web UI | Passed |
| Go race suite | Passed |
| Driver-independent FUSE adapter tests | Passed |
| Random input seeks, overlapping writes, dirty overlays, truncate/extend, sparse reads | Passed |
| Reopen, rename, unlink while open, resumable directory enumeration | Passed |
| Worker/attempt isolation, expired leases, source identity changes | Passed |
| Symlink rejection, case collision rejection, disc manifest, sidecar aliases | Passed |
| Duplicate/conflicting/out-of-order operations and lost batch acknowledgement | Passed |
| SQLite close/reopen with persisted session and sequence state | Passed |
| Destination conflict and no final path before sealed completion | Passed |
| RAM/disk cache bounds, in-flight deduplication, corrupted disk blocks | Passed |
| Dirty backpressure, cancellation, journal I/O failure | Passed |
| Fair scheduling and demand priority over prefetch | Passed |
| `go vet ./...` and adapter vet | Passed |
| Guided setup, worker restart, pairing, and revocation smoke test | Passed |
| Linux paired-binary build and archive | Passed |
| Windows main/helper cross-build | Passed; not native validation |
| macOS ARM64 main cross-build | Passed; helper/native driver untested |
| Native FUSE mount / real HandBrake over mount | Not run: `/dev/fuse` absent |
| Native macOS and Windows driver gates | Not run |
| Real disc/subtitle/multipass fixture matrix | Not run |
| Native crash/exhausted-disk fault injection | Not run |
| Transfer-vs-FUSE DVD benchmark | Not run |

The race suite uses protocol fixtures and direct adapter calls; these do not
exercise a kernel filesystem driver. The opt-in native test is skipped unless
`TRANSCOOP_FUSE_NATIVE=1`; that skip is not counted as a passed mount or media test.

See [filesystem documentation](filesystem.md) for commands, remaining gates,
known conservative behaviors, and required native runner provisioning.

# Filesystem validation record — 2026-11-17

Environments: a CachyOS x86-64 coordinator and test host (Go 1.27.4, libfuse
2.9.9 and 3.18, `/dev/fuse` present, HandBrake 1.11.2), and an Omarchy
(Arch) x86-64 worker (Ryzen 7 5825U) over gigabit LAN.

| Check | Result |
| --- | --- |
| `TRANSCOOP_FUSE_NATIVE=1 go test -race ./...` (native mount, four real HandBrake encodes) | Passed |
| `go test -tags fuse ./cmd/transcoop-fs` | Passed |
| Worker context cancelled: helper stays up until the worker unmounts | Passed (new native test) |
| Helper killed without unmounting: next start clears the stale mount | Passed (new native test, and on the worker) |
| Remote worker, three encodes through the mount | Passed; outputs complete (90 s), mode 644, no staging left |
| Worker stopped mid-encode through the mount | Passed; both jobs returned to the queue, mount removed, re-encoded |
| Encoding speed through the mount vs transfer mode | About 3× slower on the same clips (two at a time: 4m10s vs about 1m) |
| Native Windows driver gate (Windows 11 24H2 VM, WinFsp 2.1) | Passed after fixes: see below |
| Native macOS driver gate (Mac Studio, M4 Max, macOS 26.5, macFUSE 5.5.0) | Not run: see below |

macOS: macFUSE's FSKit module registered but could not be enabled; System
Settings turned the switch back off and `fskitd` logged "errno 2, retrieving
team ID" for the notarized module (team 3T5GSNBU6W), also after a restart. The
kernel extension was not tried because it requires Reduced Security. On the same
Mac the full suite passed with `-race` (after fixing two tests that assumed
Linux paths), and a transfer worker encoded x264 and VideoToolbox
(`vt_h265_10bit`) jobs, honoured `priority = "low"` (HandBrake at nice 10), and
followed worker job order against a Linux worker in both directions.

Found and fixed during this run:

- The helper was started with the worker's context, so a worker shutdown
  SIGKILLed it before it could unmount, stranding the mount. The next worker
  then failed with "mkdir …/mount: file exists".
- Workers on FUSE 3-only distributions need the FUSE 2 package (`fuse2`).
- A helper built on a distribution compiled for x86-64-v4 would not start on
  older CPUs; `scripts/build-native.py` now builds a baseline helper.
- A macOS worker started in the background lost LAN access ("no route to
  host") under Local Network privacy; the worker now logs how to allow it.

## Windows — 2026-11-18

Environment: Windows 11 Enterprise 24H2 evaluation (build 26100) in a QEMU/KVM
VM (8 vCPUs, 8 GB) on the CachyOS host, WinFsp 2.1.25156, HandBrake 1.11.2, Go
1.27.4, against the same coordinator and the Linux worker.

| Check | Result |
| --- | --- |
| `go test ./...` (no `-race`: no C compiler in the VM) | Passed after fixing test fixtures |
| `TRANSCOOP_FUSE_NATIVE=1 go test ./internal/farm` (native mount, real encodes) | Passed |
| `go test -tags fuse ./cmd/transcoop-fs` | Passed |
| FUSE worker beside the Linux worker, ranked first | Passed; outputs complete (90 s), decode without errors |
| `priority = "low"` | HandBrake Below Normal, worker Normal |
| Ctrl+C mid-encode, then restart | Passed; job returned to the queue, remounted, finished |
| Worker process ended (`Stop-Process -Force`) mid-encode | Helper unmounted on its own, nothing left running; next start mounted; job interrupted until retried (as designed) |

Found and fixed during this run:

- WinFsp refuses a mount point that already exists, and the worker created it.
  The worker now leaves the mount point for WinFsp to create (removing an empty
  leftover) and creates only its parent.
- WinFsp maps the helper's uid (−1 on Windows) to no account, so every access
  was denied; the helper mounts with `uid=-1,gid=-1` (the mounting user).
- WinFsp calls `Init` before setting the mount point. After a failed mount the
  helper still reported ready, WinFsp left a plain directory, and the worker's
  read/write check passed against it, so jobs failed with "No title found". The
  helper now waits until the mount serves its own probe file and fails otherwise.
- Ctrl+C reached the helper too and killed it before the worker had returned its
  jobs; the helper now starts in its own process group, as on Unix.
- Tests assumed Unix absolute paths, a `:` in file names and Unix permission
  bits.
- Not Windows-specific: a job waiting for a higher-ranked worker reused its
  first wait on later attempts, so a retried job went straight to a lower-ranked
  worker. The wait now restarts for each attempt.
