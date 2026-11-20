# Configuring Transcoop

Run `transcoop setup` to configure and pair a coordinator or worker. Setup prints
the path to the TOML file it saved. Edit the sections for the roles that run on
that machine. A coordinator that also encodes can use both sections.

**Find a setting** · [Connection](#connection) · [Coordinator](#coordinator) · [Worker](#worker) · [Scheduling](#scheduling) · [Retention](#retention) · [FUSE](#fuse-storage) · [Terminal review](#terminal-review) · [File locations](#file-locations)

## HandBrakeCLI location

Each role has its own `handbrake` setting. A worker runs the encoder. The
coordinator scans media for **Add jobs** and **Watch**. The terminal UI scans
folders and rescans sources on the machine where you run `transcoop tui`.
All three look for `HandBrakeCLI` on that machine’s `PATH` by default.

For example, if you extracted the CLI to a tools folder on a Windows worker,
edit the existing `[worker]` section. TOML single quotes preserve backslashes.

```toml
[worker]
handbrake = 'C:\Tools\HandBrakeCLI\HandBrakeCLI.exe'
```

On a Mac that runs the coordinator and TUI, point each role at the path
printed by `command -v HandBrakeCLI`. An Apple Silicon installation might use
the following settings.

```toml
[coordinator]
handbrake = "/opt/homebrew/bin/HandBrakeCLI"

[tui]
handbrake = "/opt/homebrew/bin/HandBrakeCLI"
```

A full path is useful when a service starts with a different `PATH` from your
terminal. For a single run, `serve`, `worker`, and `tui` also accept
`-handbrake PATH`.

## Editing and checking settings

To create a starter file manually, run `transcoop config init`. It requires
a new file path. Guided setup also creates the credentials needed for pairing.
Use `transcoop config check` after edits. Use the same file for validation and startup.

```sh
transcoop config check -config ./worker.toml
```

The check validates TOML, key names and setting values. Guided setup checks
that HandBrakeCLI runs, that the scratch directory is writable, and that
the worker can reach the coordinator. Restart the affected process to apply
changes.

The tables give defaults for **omitted settings**. Setup writes choices such
as the LAN address and the encoders available on this machine. Explicit CLI
flags take precedence. Lists supplied with flags replace configured lists.
Paths in the file resolve relative to the TOML file. Paths on the command line
resolve relative to the working directory. A bare executable name uses `PATH`.

## Connection

Place this setting before the first `[section]` heading.

| Key | Default | What it controls |
| --- | --- | --- |
| `server` | `"http://127.0.0.1:8765"` | Coordinator address used by clients. On another machine, use its reachable hostname or LAN address, for example `"http://encoding-box:8765"`. HTTP and HTTPS are supported. Include the listening port when it differs from the scheme’s standard port. |

## Coordinator

These keys belong in `[coordinator]`.

| Key | Default | What it controls |
| --- | --- | --- |
| `listen` | `"127.0.0.1:8765"` | Address and port for the API and console. Loopback serves this machine. Select a LAN address in setup for remote workers, or use `"0.0.0.0:8765"` for all IPv4 interfaces. |
| `db` | Data directory, `farm.db` | SQLite queue, device records and job history. Use local disk and one coordinator per database. |
| `ui` | `true` | Enable the browser console. Set to `false` to serve only the API. |
| `handbrake` | `"HandBrakeCLI"` | Executable for listing presets and scanning media on this machine. |
| `log_file` | Log directory, `serve.log` | Append coordinator logs here as well as writing to stderr. Use `"-"` for stderr only. |
| `secure_cookies` | `false` | Set to `true` when browsers reach the console over HTTPS through a trusted TLS proxy. Preserve the original Host at the proxy. Leave false for plain HTTP on a trusted LAN. |

## Worker

These keys belong in `[worker]`.

| Key | Default | What it controls |
| --- | --- | --- |
| `name` | Hostname | Display name in the console, up to 200 bytes. |
| `mode` | `"transfer"` | `"transfer"` downloads each input file and uploads the encoded result. `"shared"` uses storage visible to both machines. Experimental `"fuse"` streams through a native mount. |
| `encoders` | `["x265_10bit"]` | HandBrake encoder IDs this worker accepts. Guided setup detects software encoders and probes hardware encoders. An explicit list needs 1–100 nonempty entries. |
| `handbrake` | `"HandBrakeCLI"` | Executable used for worker probes and encodes. |
| `work_dir` | Cache directory, `worker-data` | Local scratch space for attempts, media transfers and HandBrake logs. Choose a disk with room for concurrent jobs. |
| `mappings` | Empty list | Shared-mode translations from coordinator paths to worker paths. Use `"FROM=TO"` per entry. The longest matching directory prefix wins. |
| `priority` | `"low"` | Scheduling priority of the HandBrake process. Choose `"normal"`, `"low"` or `"idle"`. Set worker job order separately on the Workers page. |
| `log_file` | Log directory, `worker.log` | Append worker service logs here as well as writing to stderr. Use `"-"` for stderr only. Per-attempt HandBrake logs live in `work_dir`. |

For example, this worker reads the coordinator’s `/srv/media` share at
`/mnt/media` and accepts two x264 jobs at a time. Pair the worker with
`transcoop setup` and edit its saved settings.

```toml
server = "http://encoding-box:8765"

[worker]
name = "living-room"
mode = "shared"
mappings = ["/srv/media=/mnt/media"]
encoders = ["x264"]

[worker.scheduling]
mode = "fixed"
max_jobs = 2
```

### Scheduling

These keys belong in `[worker.scheduling]`.

| Key | Default | What it controls |
| --- | --- | --- |
| `mode` | `"fixed"` | Fixed scheduling fills available job slots. Experimental `"auto"` admits new jobs according to measured CPU headroom. |
| `max_jobs` | `1` in fixed mode, `4` in auto mode | Maximum concurrent jobs, including preparation and transfer. An explicit value must be a positive integer. |
| `cpu_target` | `85` | Whole-machine CPU target, as a percentage, for admitting work in auto mode. Values must be greater than 10 and at most 100. Running jobs can exceed it. |

### Retention

These keys belong in `[worker.retention]`. The worker prunes finished attempt
folders at startup and after each attempt. Zero selects the default for that key.

| Key | Default | What it controls |
| --- | --- | --- |
| `days` | `14` | Remove finished attempt folders after this many days without activity. |
| `attempts` | `100` | Keep up to this many finished attempt folders, newest first. Both the count and age limits apply. |
| `failed_output_days` | `2` | Remove leftover media earlier while retaining the attempt log and queue JSON. Must be at most `days`. |

### FUSE storage

These keys belong in `[worker.fuse]`. Set `worker.mode = "fuse"` to use them.
Budget values are integer bytes. Zero selects the default for numeric settings.
Driver installation and platform test results are in the [FUSE guide](filesystem.md).

| Key | Default | What it controls |
| --- | --- | --- |
| `mount_dir` | `mount` inside `work_dir` | Mount point reserved for this worker. Keep it separate from `cache_dir`, with neither directory inside the other. |
| `cache_dir` | `cache` inside `work_dir` | Directory for cached input blocks and journaled writes. Give each worker its own cache. |
| `ram_bytes` | `268435456` (256 MiB) | In-memory block cache budget. Minimum 1 MiB. |
| `disk_bytes` | `8589934592` (8 GiB) | Disk cache budget, including the dirty-write reservation. Must be at least `dirty_bytes`. |
| `dirty_bytes` | `536870912` (512 MiB) | Space reserved for unacknowledged writes. Writers wait when this fills. Minimum 8 MiB. |
| `read_concurrency` | `4` | Concurrent remote reads, from 1 to 64. |
| `write_concurrency` | `2` | Concurrent remote writes, from 1 to 64. |

## Terminal review

These keys belong in `[tui]`. The folder-only settings are `preset`,
`preset_file`, `output_dir`, and `recursive`. If you also open exported queue
JSON files, pass folder-only options as flags for individual folder scans or
use a separate config file.

| Key | Default | What it controls |
| --- | --- | --- |
| `handbrake` | `"HandBrakeCLI"` | Local executable for folder scans and queue rescans. |
| `preset` | HandBrake’s default preset | Name of the preset to apply when preparing a folder. |
| `preset_file` | Installed HandBrake presets | Path to an exported preset JSON file. Also set `preset` to the name to use from the file. |
| `output_dir` | Unset | Destination directory, required for folder input. Choose a different folder from the source. |
| `keep_languages` | No initial language filter | Comma-separated three-letter audio language codes, for example `"eng,jpn,und"`. |
| `first_audio` | `false` | Initially keep the first included source audio track per language. |
| `recursive` | `false` | Include video files in subfolders. |
| `mappings` | Empty list | Translate coordinator path prefixes to local paths for folder scans and rescans. Use the same `"FROM=TO"` syntax as worker mappings. |

## File locations

| File or directory | Linux | macOS | Windows |
| --- | --- | --- | --- |
| Settings | `~/.config/transcoop/config.toml` | `~/Library/Application Support/transcoop/config.toml` | `%AppData%\transcoop\config.toml` |
| Database | `~/.local/share/transcoop/farm.db` | `~/Library/Application Support/transcoop/farm.db` | `%LocalAppData%\transcoop\farm.db` |
| Service logs | `~/.local/state/transcoop/logs/` | `~/Library/Logs/transcoop/logs/` | `%LocalAppData%\transcoop\logs\` |
| Worker scratch | `~/.cache/transcoop/worker-data/` | `~/Library/Caches/transcoop/worker-data/` | `%LocalAppData%\transcoop\cache\transcoop\worker-data\` |

Linux honors `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, and
`XDG_CACHE_HOME`. With a non-default config file, the default database is
`data/farm.db` beside that file. The default logs and scratch space are in
sibling `logs` and `worker-data` directories.

Setup and pairing store private credentials beside the settings file. For
`config.toml`, that file is `config.credentials.json`. Keep credentials private.
Clients select credentials by the coordinator URL, so pair again if its
address changes.
