# Changelog

All notable changes are recorded here. Transcoop follows
[semantic versioning](https://semver.org/); until 1.0, any alpha or beta release
may change configuration, the database schema, or the API.

## [Unreleased]

## v0.1.0-alpha.2

- Linux and macOS releases include an `install.sh` for first-time installs.
  It verifies the signed archive checksums and installs Transcoop in `~/.local/bin`.
- Official binaries can check for a new release with `transcoop update --check`
  and install it locally with `transcoop update`. Release archives now ship
  with an Ed25519 signature for their SHA-256 checksums.
- To upgrade from v0.1.0-alpha.1, replace the executable manually once. The
  new binary can install future signed releases with `transcoop update`.

## v0.1.0-alpha.1

First public alpha.

- Worker setup uses port 8765 when the coordinator address has no port, and
  when a loopback address is refused it points to the LAN address where this
  machine's coordinator is listening.
- Coordinator with a SQLite job queue, HTTP API (`/api`), and embedded browser
  console: dashboard, queue, workers, devices, job import, and folder-based job
  creation with installed HandBrake presets.
- Console themes based on the Omarchy palettes (Tokyo Night by default, light
  themes available, <kbd>T</kbd> to switch), live progress and results in the
  queue, and per-worker job slots and encoder lists.
- Upload exported HandBrake presets on the Add jobs page; they are stored on the
  coordinator and carried to workers inside each job.
- `transcoop service install|uninstall|status` runs the coordinator or a worker
  as a systemd user service on Linux; `setup` offers it.
- Stopping a worker returns its running jobs to the queue instead of failing them.
- Watch folders: new videos in a watched folder are queued with its preset once
  they stop changing; works on network shares.
- Worker priority: `worker.priority` (normal, low, idle; default low) sets
  HandBrake's CPU and I/O priority, and a per-worker job order (first, normal,
  last) on the Workers page decides which idle worker gets jobs first.
- FUSE mode: stopping a worker no longer kills the helper before it unmounts,
  and a stale mount left by a crash is cleared on the next start (Linux).
  Validated on Linux with real encodes.
- macOS: workers log how to grant Local Network access when macOS blocks it;
  README documents macOS workers and VideoToolbox presets.
- Windows: FUSE mode works with WinFsp (the mount point is left for WinFsp to
  create, files are owned by the worker's account, and Ctrl+C no longer reaches
  the helper before the worker returns its jobs). The helper now confirms the
  mount is serving before the worker uses it on every platform. README documents
  Windows workers and starting one from Task Scheduler.
- Workers with fixed or experimental CPU-driven concurrency, leases and
  heartbeats, progress telemetry, and play/pause/stop controls.
- Shared-storage and file-transfer media modes; experimental FUSE streaming mode
  with a separate `transcoop-fs` helper (validated on Linux and Windows).
- Guided `setup` for coordinators and workers, including hardware encoder checks.
- Device pairing for browsers, workers, and operator CLIs, with revocation.
- Terminal UI for reviewing audio and subtitle tracks across a batch, from a
  HandBrake queue export or a folder scan.
- `transcoop version`; Transcoop and HandBrake versions in startup logs and
  worker cards, with a dashboard warning when the farm runs mixed versions.
- Unauthenticated `GET /api/health` for connectivity checks; `pair -json`.
- Setup checks coordinator reachability, detects ufw/firewalld, offers an
  all-interfaces listener, and re-prompts for mistyped pairing codes.
- Workers prune finished attempt folders (`[worker.retention]`: 14 days,
  100 attempts, failed-attempt media after 2 days, all configurable).
- Default database, logs, and scratch space follow platform conventions
  (XDG on Linux) instead of living in the config directory.
