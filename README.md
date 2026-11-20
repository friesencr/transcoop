# Transcoop

**A self-hosted HandBrake encoding farm.** Pool the machines you already own to
work through one shared encode queue.

> [!WARNING]
> **Alpha software.** Transcoop works end to end, but expect rough edges and
> breaking changes to configuration, the database schema, and the API between
> alpha releases. Keep originals of your media and back up the coordinator
> database before upgrading. Please [report problems](https://github.com/friesencr/transcoop/issues).

One machine runs the **coordinator**: it keeps the job queue in a local SQLite
database and serves a browser console. Other machines run **workers**: they pair
with the coordinator, claim jobs their HandBrakeCLI can encode, report progress,
and hand finished files back. A single machine can be both.

[Install](#install) · [First setup](#first-setup) · [Add your first job](#add-jobs-in-the-browser) · [Update](#update) · [Configuration reference](docs/configuration.md)

## Install

Download **v0.1.0-alpha.1** for your machine. The coordinator and worker use
the same executable. [Release notes and checksums](https://github.com/friesencr/transcoop/releases/tag/v0.1.0-alpha.1)

| Platform | Intel or AMD (x64) | ARM64 |
| --- | --- | --- |
| Linux | [Linux x64](https://github.com/friesencr/transcoop/releases/download/v0.1.0-alpha.1/transcoop_v0.1.0-alpha.1_linux_amd64.tar.gz) | [Linux ARM64](https://github.com/friesencr/transcoop/releases/download/v0.1.0-alpha.1/transcoop_v0.1.0-alpha.1_linux_arm64.tar.gz) |
| macOS | [Intel Mac](https://github.com/friesencr/transcoop/releases/download/v0.1.0-alpha.1/transcoop_v0.1.0-alpha.1_darwin_amd64.tar.gz) | [Apple Silicon Mac](https://github.com/friesencr/transcoop/releases/download/v0.1.0-alpha.1/transcoop_v0.1.0-alpha.1_darwin_arm64.tar.gz) |
| Windows | [Windows x64](https://github.com/friesencr/transcoop/releases/download/v0.1.0-alpha.1/transcoop_v0.1.0-alpha.1_windows_amd64.zip) | [Windows ARM64](https://github.com/friesencr/transcoop/releases/download/v0.1.0-alpha.1/transcoop_v0.1.0-alpha.1_windows_arm64.zip) |

Extract the archive. On Linux or macOS, open a terminal in the extracted
directory and run `./transcoop version`. On Windows, open PowerShell there
and run `.\transcoop.exe version`. It should print `transcoop` followed by the
tag of the release you downloaded.
Move the executable to a directory on your `PATH` to run `transcoop` from anywhere.

Install **HandBrakeCLI** on each encoding worker. Also install it on the
coordinator for **Add jobs** and **Watch**, or on a machine that scans media
with the TUI. Run `HandBrakeCLI --version` to check it. If it lives outside
your path, set its location for that role.
[HandBrakeCLI downloads](https://handbrake.fr/downloads2.php) · [Set the HandBrakeCLI path](docs/configuration.md#handbrakecli-location)

Continue to [First setup](#first-setup) to pair your machines.

### Build from source

To try the current source, clone this repository or extract **Code → Download ZIP**.
Open a terminal in the directory containing `go.mod`. You need Go 1.27.4 or later.
[Download Go](https://go.dev/dl/)

**Linux or macOS** (sh, Bash or Zsh)

```sh
mkdir -p "$HOME/.local/bin"
CGO_ENABLED=0 go build -trimpath -o "$HOME/.local/bin/transcoop" ./cmd/transcoop
export PATH="$HOME/.local/bin:$PATH"
transcoop version
```

**Windows** (PowerShell)

```powershell
$installDir = Join-Path $env:LOCALAPPDATA 'Programs\Transcoop'
New-Item -ItemType Directory -Force $installDir | Out-Null
go build -trimpath -o "$installDir\transcoop.exe" ./cmd/transcoop
$env:Path = "$installDir;$env:Path"
transcoop version
```

These commands put Transcoop on your path for the current terminal. For future
terminals, add the same directory to your shell profile or Windows user `Path`.
Use the same Transcoop version on each machine.

## Update

**Starting from v0.1.0-alpha.1?** That release predates the updater. When the
first release with `transcoop update` is published, download its archive and
replace your executable manually once. Keep the existing executable path so
your service uses the new version. [GitHub releases](https://github.com/friesencr/transcoop/releases)

With an updater-enabled release, run `transcoop update --check` on any machine
to see the installed and available versions. Stable builds follow stable
releases. Alpha, beta and other prerelease builds also see newer prereleases.

1. Pause the queue and let active jobs finish. Stop your workers and coordinator,
   then back up the coordinator's SQLite database and its accompanying files.
2. Run `transcoop update` on the coordinator and each worker, using an account
   that can write to the executable's directory. Transcoop verifies the signed
   checksums and replaces the local executable. On Windows, keep the terminal
   open until the helper prints **Installed** or reports an error.
3. Run `transcoop version` on each machine. Start the coordinator, start the
   workers, confirm their versions in the browser console, and resume the queue.
   Existing configuration and pairing credentials carry over.

For a source build, rebuild from the desired tag. For a FUSE worker, rebuild
both `transcoop` and `transcoop-fs` from that tag. The updater asks for a manual
upgrade when the worker uses FUSE or the helper is beside Transcoop or on
your `PATH`. [FUSE build instructions](docs/filesystem.md)

## First setup

On the coordinator, run guided setup in a terminal.

```sh
transcoop setup
```

Choose **coordinator**, select the displayed LAN address (or `0.0.0.0` for every
interface, such as LAN plus a VPN), choose the port (default 8765), and choose the
database path. If ufw or firewalld is active, setup prints the command to open the
port; other machines cannot connect until it is open. Rerunning setup preserves the existing port
unless you change it. Setup generates a private local administrator credential and remembers the
address. If setup prints a **Start** command, run it to start the coordinator
and leave that terminal open. Accepting the Linux systemd offer starts the
service for you. Open the printed URL, then issue a browser pairing code from
another terminal.

```sh
transcoop pair -role browser
```

Enter the code and a device name in the browser. The browser stays paired for
30 days, including across coordinator restarts. The **Devices** page issues codes
for additional browsers, workers, and operator CLIs, and revokes individual devices.
Logging out revokes that browser; another pairing is required to return.

On the encoding machine, start guided setup.

```sh
transcoop setup
```

Choose **worker** and enter the coordinator address. Setup checks that the
coordinator is reachable before continuing; if not, check the address and the
coordinator's firewall. Follow the HandBrake, storage, and scheduling prompts.
For a first encode, choose **transfer** and **fixed**. When prompted to pair,
generate a worker code on the **Devices** page and enter it in setup. Run the
printed **Start** command if one appears.

Setup checks the installed HandBrake version and includes advertised software
encoders automatically.
Hardware and unrecognized encoders must pass a short encode using a bundled test
clip; failed checks are skipped with an explanation. Each check has a 45-second
timeout and needs no additional tools. Setup shows the accepted encoders and checks
scratch-directory writability. To restrict the selection, edit `worker.encoders`
in the config. The test verifies basic operation, not every source format or
encoding setting. Setup does not install HandBrake. On Linux with systemd it
offers to run the coordinator or worker as a service (see
[Run at boot](#run-at-boot-linux-systemd)).

Trusted LAN HTTP is the normal connection model: pairing controls access but does
not encrypt traffic. HTTPS URLs and a reverse proxy remain usable. With TLS
termination at a proxy, set `coordinator.secure_cookies = true` and preserve Host.
Do not enable secure cookies when opening the console through plain HTTP.

## Add jobs in the browser

Open **Add jobs** to browse video files and folders accessible to the coordinator.
Select an input file or folder, choose an output folder and an installed HandBrake
preset, and optionally include subfolders. **Preview jobs** scans the media and
shows each source, output, and encoder. **Add jobs to queue** submits the reviewed
batch. For a first run, pick one short video and check **Queue** for
`succeeded`, then open the encoded file in the output folder. To submit an
existing HandBrake queue JSON file, use **Import**.

The coordinator needs HandBrakeCLI for this workflow; it scans media without
encoding it. Set `coordinator.handbrake` in the config (or `serve -handbrake PATH`)
if the executable is not on PATH. Built-in presets and presets saved by the
coordinator's HandBrake GUI user appear in the preset selector.

To use your own preset without installing it on the coordinator, export it from
HandBrake (**Presets → Export**) and upload the `.json` file under **Custom
presets** on the Add jobs page. Every preset in the file, including ones inside
folders, then appears under **Uploaded** in the preset selector for all browsers.
Uploads are kept in a `presets/` folder beside the database. HandBrake picks
presets by name, so an upload whose preset name matches an installed or
already-uploaded preset is rejected; rename it in HandBrake first. Removing an
upload does not change jobs already queued, since each job carries its own copy
of the preset's settings.

Batches support up to 500 files and use title 1, the preset's track-selection
rules, and a saved preset snapshot. Subfolder paths are preserved in the output
folder. Existing output files and conflicting destinations are rejected. Shared
media must be mounted on the coordinator; worker path mappings still apply when
jobs run. Media is not uploaded through the browser; to queue files as they
arrive, use a watch folder.

## Features

- **Browser console:** dashboard, queue, workers, devices, and folder-based job
  creation with any installed HandBrake preset. No CDN or external assets.
- **Import HandBrake GUI queues** exported as JSON, or scan folders with a preset.
- **Watch folders:** videos copied into a watched folder or share are queued
  automatically once the copy finishes.
- **Audio and subtitle track review** in a terminal UI: keep languages, drop
  commentary, and apply one file's choices across a whole season before queueing.
- **Device pairing:** short single-use codes for browsers, workers, and operator
  CLIs; individual revocation.
- **Safe by default:** leases with heartbeats, fenced completion, and atomic
  publication that never overwrites an existing output.
- **Flexible media access** per worker:

| Mode | Source | Encoded output |
| --- | --- | --- |
| `shared` | Worker reads a shared filesystem, with optional path mappings | Worker writes a staged file on that filesystem; coordinator publishes it |
| `transfer` | Worker downloads a single media file from the coordinator | Worker encodes in local scratch space and uploads the result for publication |
| `fuse` *(experimental)* | Worker mounts the source and reads it on demand | Output streams back to the coordinator while encoding |

All modes can run against the same coordinator. Media is streamed over HTTP and
is never stored in SQLite or buffered entirely in memory.

## Requirements

- Linux, macOS, or Windows (amd64 or arm64).
- [HandBrakeCLI](https://handbrake.fr/downloads2.php) on every worker, with the
  encoders its jobs use. Use the same HandBrake version across the farm. The
  coordinator needs it when scanning media for browser jobs or watch folders.
- A trusted LAN. Pairing controls access but plain HTTP is not encrypted; see
  [First setup](#first-setup) for running behind a TLS proxy.

### macOS workers

- Install HandBrakeCLI with `brew install handbrake`. Apple Silicon workers can
  run the VideoToolbox presets (`vt_h264`, `vt_h265`, `vt_h265_10bit`), which
  only Macs support; such jobs go only to Mac workers.
- macOS 15 and later restrict local network access. Start the worker from
  Terminal the first time and allow the "find devices on your local network"
  prompt, or allow `transcoop` in **System Settings → Privacy & Security → Local
  Network**. A worker started in the background without that permission (for
  example with `nohup` from an SSH session that then ends) fails with "no route to
  host"; the worker log then says so.

### Windows workers

- Extract the HandBrakeCLI zip from handbrake.fr and point the worker at it, for
  example `handbrake = 'C:\Tools\HandBrakeCLI\HandBrakeCLI.exe'` (single quotes
  keep backslashes literal in TOML), or add its folder to `PATH`.
- `priority = "low"` runs HandBrake in the Below Normal priority class; `idle`
  uses Idle.
- Ctrl+C in the worker's console returns running jobs to the queue before it
  exits. Ending the process from Task Manager cannot do that: its jobs show as
  interrupted once their lease runs out, and **Retry** queues them again.
- There is no built-in Windows service yet. To start a worker when you log in,
  run this once in PowerShell (adjust the path):

  ```powershell
  $action = New-ScheduledTaskAction -Execute 'C:\Tools\transcoop\transcoop.exe' -Argument 'worker'
  $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit 0 -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
  Register-ScheduledTask -TaskName 'Transcoop worker' -Action $action -Settings $settings -Trigger (New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME)
  ```

  `-ExecutionTimeLimit 0` matters: by default Task Scheduler stops tasks after
  three days. The worker log is in `%LocalAppData%\transcoop\logs\`.
- FUSE mode uses [WinFsp](https://winfsp.dev/); see
  [Streaming filesystem storage](#streaming-filesystem-storage-experimental).

## Watch folders

The **Watch** page queues new videos dropped into a folder on the coordinator,
for example an SMB share that any machine can copy to. Each watch has a folder,
an output folder, a preset (installed or uploaded), and optionally subfolders,
whose layout is kept in the output.

- Folders are polled every 30 seconds, so network shares work (inotify does not
  see changes made by other machines). A video is queued once its size and
  modification time have not changed for a minute, which skips copies in
  progress. Hidden files and folders are ignored.
- Videos already in the folder when the watch is created are left alone unless
  you tick **Also queue videos already in the folder**.
- Each file version (path, size, modification time) is handled once. Clearing
  the queue does not re-add it; replacing a file with a new version queues it
  again. A file that cannot be prepared is shown as failed and retried only after
  it changes. Existing outputs are never replaced; such files show as skipped.
- Sources are never moved or deleted.
- With subfolders off, the output folder may be inside the watched folder (for
  example `inbox/done`); with subfolders on it must be elsewhere.

Each watch shows recent files and counts. **Pause** stops scanning a folder,
**Remove** stops watching it (queued jobs and files stay), and **Scan now** checks
immediately. Watches persist across coordinator restarts; files waiting to settle
start their minute again after a restart. Disc folders (VIDEO_TS, BDMV) are not
watched; add them from **Add jobs**.

## Configuration

`transcoop setup` saves this machine’s settings in `config.toml`. After editing
the file, run `transcoop config check` and restart the affected worker or
coordinator. Use `-config FILE` to select a different file.

The [configuration reference](docs/configuration.md) lists every setting, its
default, and where files live on each platform. It includes examples for
HandBrakeCLI outside your path and a worker that uses shared storage.

## Pairing and daily commands

Codes are single-use, expire after ten minutes, and allow five failed attempts.
Creating another code for the same role replaces the previous one. Redemption is
limited to 30 attempts per minute across the coordinator.
`pair -json` prints the code as JSON for scripts. `GET /api/health` needs no
credentials and returns `{"status":"ok","version":"..."}`, which is useful for
checking connectivity (for example with `curl`) before pairing.

```sh
transcoop pair -role worker
transcoop pair -role operator
transcoop join -server encoding-box:8765 -role operator
transcoop devices list
transcoop devices revoke DEVICE_ID
transcoop import handbrake-queue.json
transcoop jobs
transcoop retry 12
```

`join` prompts for the code and saves the connection. Worker setup pairs a worker
credential; `join -role worker` is also available for manually configured workers.
Workers can claim jobs, report their own presence, and access only their own active
attempts. Operators and paired browsers can administer the farm. The local
administrator cannot be revoked through Devices and can recover access by issuing
a new code. Revoking every browser never opens unauthenticated access.

Queue imports are atomic and exact duplicates are ignored. Different jobs targeting
the same normalized destination are rejected. Source and destination paths must be
absolute as seen by the coordinator. Existing output files are never replaced.
Importing alone does not encode anything; a worker must be running.

## Browser operator console

The embedded console provides Dashboard, Queue, Workers, Add jobs, Watch, Import,
and Devices pages.
Use `serve -ui=false` for API-only operation. Assets are bundled; no CDN is used.

- Play, pause, and stop the whole queue from the Dashboard, Queue, or Workers
  page, or a single worker from its card.
- Dashboard and Workers show CPU, job slots versus the concurrency cap,
  scheduling mode, hardware encoders, and why the worker is waiting. Automatic
  workers show CPU target and headroom observation progress.
- Queue shows file names with progress, FPS, and ETA for running jobs, and time
  taken and size change for finished ones. Filter by filename, status, or worker;
  pagination keeps the filters in the URL.
- Job details show the source and output, attempt history, live progress, and a
  searchable recent log.
- Add jobs browses coordinator folders (starting at your home folder, then the
  last folder used) and previews the jobs before they are queued.
- Import previews a HandBrake queue before explicit submission; retry requires
  confirmation and preserves overwrite protection.
- Devices lists pairing state separately from worker process presence, with code
  creation and individual revocation.

Status panels refresh every five seconds. Reports become stale after 15 seconds;
worker presence becomes offline after 30 seconds. Job leases remain authoritative.
Missing progress is shown as a dash. CPU history is not retained. Times are
relative; hover for the exact time in the browser's time zone.

The console follows the system light or dark setting and includes ten
[Omarchy](https://omarchy.org)-inspired themes (Tokyo Night, Catppuccin, Gruvbox,
Nord, Everforest, Kanagawa, Matte Black, Rosé Pine, Catppuccin Latte, Flexoki
Light). Press <kbd>T</kbd> (or <kbd>Shift</kbd>+<kbd>T</kbd>) or use the theme
button in the header; the choice is remembered per browser.

Recent logs retain 64 KiB per attempt for the latest three attempts. Full logs
remain on workers. Credentials and attempt tokens are redacted from submitted
logs. Browser cookies are HttpOnly and SameSite=Strict, and mutations require CSRF
tokens. Browser cookies do not authorize bearer-token API requests.

## Review audio in the TUI

Open an exported GUI queue **before importing it into the farm**:

```sh
transcoop tui handbrake-queue.json
# Submit to a remote coordinator, and map paths for optional local rescans:
transcoop tui -server http://coordinator:8765 \
  -map /srv/media=/mnt/media handbrake-queue.json
```

The TUI starts with the queue's embedded track metadata and the audio outputs
already selected in the GUI. Each output shows its source track number, language,
source codec/channel count, name and output encoder. Multiple encodings of the
same source (for example FLAC plus AC3 passthrough) appear as separate outputs.
Excluded outputs are removed from the prepared job; source media is never edited.

| Key | Action |
| --- | --- |
| Arrow keys / `j`, `k` | Move within the focused pane |
| `Tab` | Cycle files, audio outputs, and subtitles |
| `t` | Open the subtitle pane |
| `Space` / `Enter` | Select a file, or include/exclude the highlighted audio/subtitle output |
| `d` / `o` / `i` | In subtitles: toggle default playback / forced-only filtering / burn-in |
| `a` | Select/deselect all visible files |
| `/` | Filter by path, filename or track metadata; empty input clears it |
| `l` | Keep languages, such as `eng,jpn,und` |
| `c` | Exclude commentary identified by flags or commentary labels |
| `f` | Keep the first currently included source track per language |
| `b` | Apply the highlighted file's audio or subtitle choices (current pane) to selected visible files |
| `x` | Exclude matches: `lang=spa`, `codec=ac3`, `channels=2`, `name=commentary` |
| `r` / `u` | Restore original selections for the current track pane / undo the last track change |
| `v` | Rescan selected sources with the local HandBrakeCLI |
| `p` | Preview each file's retained and excluded outputs |
| `s` | Open preview, then press again and confirm with `y` to submit |
| `w` | Save reviewed jobs to a new JSON file |
| `q` | Quit; confirm if edits were made |

Batch rules, rescans, saving and submission apply only to **selected visible
files**. Filtering can therefore scope an operation to a folder or metadata match.
An individual audio toggle affects just the highlighted file. Rules compose as
exclusions; use reset, undo, or individual toggles to restore tracks.

To use the same audio across the batch, choose the outputs on one file with
`Tab` and `Space`, then press `b`. All files start selected; clear any search
filter to apply to the whole batch. The reference is the highlighted file.
Matching uses language, commentary status and the source's position within that
group, rather than its absolute track number. Encoder and repeated-output
position distinguish multiple encodings of the same source. Encoding controls
(mixdown, sample rate, bitrate/quality, compression, gain, DRC, dithering and
normalization) are copied to matching outputs. Passthrough retains each file's
resolved encoder; track numbers and names stay specific to that file.

If a file lacks a required source or prepared output encoder, the entire file
keeps its previous settings and is flagged in `p` preview. Tracks and output
encoders absent from the prepared job are not added. Preview shows the retained
encoding settings; `u` undoes the bulk action and `r` restores the original
audio settings and selections. Choosing no audio on the reference also applies
no audio to the selected files, with the usual preview warnings.

The first-per-language rule retains multiple output encodings of the chosen
source track. It does not strip a preset's secondary audio encoding. Codec and
channel matching refer to the source, not the output encoding settings.

Files with missing track/language metadata are skipped by batch rules and marked
for review. Rescans update review metadata without changing audio selections.
Track commentary labels are imperfect, so inspect the preview. Files left with
no audio are explicitly marked in the preview and submission confirmation.
Exclusion rules preserve retained audio settings. Video and subtitle settings
are preserved by all audio actions.

Review and saving work offline, without pairing or installed HandBrake.
Rescanning requires HandBrakeCLI and access to the source on the TUI machine;
`-map` applies only to this scan access. Submitted source/destination paths remain
the coordinator's paths. Submission requires a paired operator and a reachable
coordinator. The original queue and existing save files are never overwritten.

The TUI reviews imported jobs or scans folders with a preset. It cannot add
audio outputs absent from the prepared job or change jobs already imported into
the farm. Available source subtitles can be added from the subtitle pane. Prepare the batch here before importing it;
submitting a different job to an already-used destination is rejected by the
coordinator. Saved queues are intended for this tool or HandBrakeCLI; the editor
updates executable job settings, not platform-specific GUI editing state.

## Review subtitles

Press `t`, or cycle with `Tab`, to review subtitle tracks inside the output file.
The pane starts with the imported job's selections or the folder preset's defaults,
and also lists unselected source tracks so you can include them with `Space`.
Use `v` to scan older queues that lack subtitle metadata. `d` marks one track as
preferred for playback; `o` retains only forced events in DVD/PGS subtitles;
`i` burns one track into the picture. Default playback and burn-in are mutually
exclusive. Burned subtitles cannot be turned off during playback.

Foreign audio search is a separate row: it selects a subtitle during encoding,
so the final track cannot be determined from source metadata alone. Toggle this
row off to use only the explicit tracks you select. To keep no subtitles, uncheck
all rows, including foreign audio search. HandBrake controls final container
compatibility and subtitle flags; its MKV default tracks also carry the forced
playback flag, which is different from forced-only filtering.

In the subtitle pane, `b` copies the choices to selected visible files by language,
format, source forced/commentary attributes, and position within that group.
Files missing a match retain their settings and are reported in the status line.
`r` restores original subtitle choices, and `u` undoes the last track edit.
Preview (`p`) includes both audio and subtitles before saving or submission.

Folder jobs whose subtitles you edited carry those explicit selections; untouched
jobs use the saved preset's subtitle behavior. This feature selects tracks inside
the output container; it does not extract separate subtitle files.

## Review a folder with a preset

Skip the GUI queue export and scan a folder directly:

```sh
transcoop tui -preset Mkv -output-dir /srv/encoded \
  -keep-languages eng,jpn -first-audio /srv/videos
```

This selects the first available English and Japanese **source** audio track in
each file, using the Mkv preset's audio encodings. For a preset like that, this can
mean both FLAC and passthrough outputs per language. To get exactly two output
tracks, toggle unwanted outputs on one file, then press `b` to apply its audio
choices across the selected batch. Omit `-keep-languages` and
`-first-audio` to start with the preset's full audio selection rules instead.
Files missing a requested language are marked for review, and no-audio outputs
are explicitly flagged. The rule selects the first matching track; it does not
infer which is the main soundtrack. Use `c` to exclude identified commentary,
then `f` again as needed (reset with `r` if a desired track was excluded).

`-preset` accepts an installed GUI or built-in preset name. Without it,
HandBrake's default after importing GUI presets is used. For an exported preset
file, pass both `-preset-file presets.json` and `-preset "Name"`. The selected
preset is normalized and saved with each job, so workers do not need your GUI
configuration and subsequent preset edits do not change queued work. Use
compatible HandBrake versions on the review machine and workers.

Only regular video files are scanned, in filename order. Add `-recursive` to
include subfolders; their relative directory layout is preserved in the output
folder. Extensions are matched without regard to case: mkv, mp4, m4v, avi, mov,
webm, wmv, mpg, mpeg, m2ts, mts, ts, vob, ogv and flv. Symlink entries are skipped.
Disc folders and playlists are not supported by this workflow. Each file uses
title 1; use a GUI queue for multi-title/disc selections.

`-output-dir` is required and must differ from the input folder. The destination
extension follows the preset's container (MKV, MP4, WebM or MOV). Existing outputs
and colliding output names stop preparation with an error; no file is replaced.
An output folder inside the source folder is excluded from recursive discovery.
The scan never creates encoded outputs or submits jobs. Failed scans are listed
as skipped in the preview while remaining files are reviewed normally. Ctrl+C
cancels scanning. Press `p` to preview, `s` to submit with confirmation, or `w` to
save a queue for later.

Source and output paths are coordinator paths. As with queue rescans, `-map`
provides local access when mounts differ:

```sh
transcoop tui -preset Mkv -output-dir /srv/encoded \
  -map /srv=/mnt/server -recursive /srv/videos
```

The review machine must have HandBrakeCLI and access to the source files. Video
and picture settings are applied by HandBrake from the saved preset at execution
time. Subtitles also follow the preset until edited in the subtitle pane; edited
subtitle selections and flags are supplied explicitly to HandBrake. Audio selections and per-output settings are fixed by the
review; HandBrake still sanitizes codec/container compatibility and mixdown
settings. Presets with audio-template fields this version cannot preserve
(such as per-track audio filter chains) are rejected with a message to use a GUI
queue export.

Folder-generated saved queues use a **Transcoop preset-job format**, not a
standalone HandBrake GUI queue. Reopen them with `transcoop tui` or submit them
with `transcoop import`; workers translate them to HandBrakeCLI arguments and
save the resolved `preset.json` with attempt logs. GUI queue imports remain supported.
Workers advertise preset support with `?preset_jobs=1` on claim and preview
requests. Workers that omit it receive only ordinary HandBrake queue jobs.

## Shared-storage worker

```sh
transcoop worker \
  -server http://coordinator:8765 \
  -name encoder-1 \
  -mode shared \
  -max-jobs 2 \
  -encoders x265_10bit \
  -map /srv/media=/mnt/media
```

The coordinator and this worker must see the **same underlying files** at the
mapped locations. Map both source and destination trees if their mount points
differ. Mappings use the longest matching directory prefix, not a substring.
Repeat `-map` for additional mounts. A Windows worker can use a mapping such as
`-map '/srv/media=Z:\Media'`. When mount paths are identical, omit mappings.

The coordinator publishes shared outputs itself, so it needs access to the output
share. Disc directories and external subtitle files require shared mode; map
their paths too.

## File-transfer worker

```sh
transcoop worker \
  -server http://coordinator:8765 \
  -name encoder-2 \
  -mode transfer \
  -max-jobs 1 \
  -encoders x265_10bit \
  -work-dir /fast-scratch/transcoop
```

No shared mount is required on this worker. The coordinator must be able to read
the source and write the destination specified in the imported queue. Scratch
space must hold the downloaded source and encoded output for each active slot.
Transfers currently restart from the beginning after a failed attempt; they do
not resume or use a content cache. Single regular media files are supported;
directory sources and external subtitle files fail with a message to use shared
mode. Storage compatibility is not yet a scheduling filter, so use shared workers
alone for queues that contain those jobs.

Downloaded sources are removed after encoding, and local encoded outputs are
removed after acknowledged completion. Every attempt saves `queue.json` and its
full `handbrake.log` in its own folder in the work directory; the coordinator
keeps only the last 64 KiB of each log.

Workers prune these folders at startup and after each attempt. Defaults:

```toml
[worker.retention]
days = 14               # delete attempt folders untouched for this long
attempts = 100          # keep at most this many attempt folders, newest first
failed_output_days = 2  # delete leftover media from failed attempts, keeping logs
```

Only folders the worker created (`job-ID-TOKEN` or `job-ID-attempt-N`) are
touched, never those of running attempts or other files in the directory.

## Run at boot (Linux, systemd)

`transcoop setup` offers this at the end, or run it any time:

```sh
transcoop service install     # role (serve or worker) is read from the config
transcoop service status
transcoop service uninstall   # keeps configuration, database and logs
```

Install writes a systemd **user** unit to
`~/.config/systemd/user/transcoop-serve.service` or `transcoop-worker.service`,
then enables and starts it. A user unit runs as you, so it sees the same
HandBrake, GPU access, config and paths as a manual start, and needs no root. It
also enables lingering (`loginctl enable-linger`) so the service starts at boot
without anyone logging in; if that needs administrator rights, the command to
run is printed. Use `-role serve|worker` when one config holds both, `-config`
for a non-default config, `-linger=false` to skip lingering, and `-print` to show
the unit without installing it (for example to adapt it as a system service).

Install the binary somewhere permanent first, such as `~/.local/bin`; the unit
records its absolute path. After upgrading the binary, run
`systemctl --user restart transcoop-worker` (or `transcoop-serve`). Reinstall
after moving the binary or config. Stop a manually started process before
installing; the command refuses while one is running.

Stopping or restarting a worker service hands its running jobs back to the queue,
so they start again (from the beginning) without a manual retry. Service output
goes to the journal (`journalctl --user -u transcoop-worker -f`) as well as the
log file below.

macOS (launchd) and Windows services are not built in yet; start Transcoop from a
login item, or on Windows from a scheduled task (see [Windows workers](#windows-workers)).

## Logs

`serve` and `worker` append service logs to `serve.log` and `worker.log` in the
logs directory (see [Configuration](#configuration)), as well as writing to
stderr. Each line identifies the service and process ID, and the first line
records the Transcoop and HandBrake versions. Follow both with:

```sh
tail -f ~/.local/state/transcoop/logs/*.log   # Linux, default config
```

The coordinator records imports, assignments, completions, failures, and retries.
Workers report scheduling configuration, job phases, and encoding percent, FPS,
and ETA when HandBrake supplies them. Progress is logged roughly every ten
seconds, and when the pass or phase changes. Automatic admission decisions are
also recorded. Each job reports its full `handbrake.log` location for diagnosis.

Use `-log-file /path/to/service.log` to choose another file, or `-log-file -`
for stderr only. Logs append across launches and are not automatically rotated.
Existing processes pick up logging changes on their next launch.

## Worker priority

Two independent settings decide how hard a worker works and when it gets jobs.

**Process priority** (`worker.priority` in the worker's config, or `-priority`)
sets how strongly HandBrake competes with other programs on that machine:

| Value | Linux / macOS | Windows | Use for |
| --- | --- | --- | --- |
| `normal` | nice 0 | normal class | dedicated encoding machines |
| `low` (default) | nice 10, lowest best-effort I/O | below normal | desktops and media servers that should stay responsive |
| `idle` | nice 19, idle I/O class | idle | machines where encoding must only use spare time |

On a machine that is otherwise idle, `low` and `idle` encode at full speed;
they only yield when something else needs the CPU or disk. The worker itself
keeps normal priority. The Workers page shows each worker's setting. I/O
priority applies on Linux only.

**Job order** is set per worker on the Workers page: **first**, **normal** or
**last**. When a worker asks for a job, the coordinator holds the job back if a
higher-ranked worker is online, playing, has a free slot and supports the job's
encoder. Use **first** for the fastest machine and **last** for one that should
only help when the others are busy. A held job goes to the asking worker after 30
seconds anyway, so a preferred worker that is slow to ask, or that its CPU
admission keeps waiting, cannot stall the queue. Workers with the same rank take
jobs in the order they ask, as before.

## CPU-driven concurrency (opt-in)

Use fixed concurrency for predictable job counts, or opt into measured CPU
admission without estimating points:

```sh
transcoop worker -scheduler fixed -max-jobs 2
transcoop worker -scheduler auto -cpu-target 85 -max-jobs 4
```

Use one worker process per machine. An omitted cap defaults to one for fixed
scheduling and four for automatic scheduling. Explicit caps must be positive.
The cap counts preparation, transfers, encoding, upload, and completion handling.

Automatic scheduling samples whole-machine CPU each second and maintains a
ten-second rolling average. When idle, it starts a job after five consecutive
samples with current and average CPU below `target - 10` (75% by default).
Unfamiliar workloads require twenty consecutive samples below that threshold
while **every active attempt is in video encoding**. Every dispatch, phase change,
exit, completion, sampling gap, or loss of headroom resets observation.

Automatic workers learn CPU demand and settling time from HandBrake's process CPU
usage, starting at its JSON video-encoding phase event. Workloads are grouped by
actual encoding settings, source geometry, and frame rate. Two successful runs,
each with at least twenty unsaturated measurements and a stable ten-sample CPU
window, are required before an estimate is trusted. Later CPU rises extend the
settling estimate; unexpectedly high demand invalidates a trusted profile.

A recently completed job allows one matching replacement to use its learned
settling interval (at least ten seconds) instead of the default twenty-second
check. Slow-settling presets can require longer than twenty seconds. Admission
also checks that estimated additional CPU demand fits under the target, with a
25% demand margin plus five percentage points. This is an estimate, not a CPU
limit. Growing concurrency retains at least the normal twenty-second check.
The worker previews the oldest compatible job without leasing it, then claims
only that exact job and payload; a changed queue triggers another preview.

Learning is local to the running worker: profiles expire after thirty minutes,
are capped at 128, and reset on restart. Replacement opportunities expire after
one minute and are discarded when the queue is empty or a request fails. Missing
source metadata or process measurements keeps the conservative timing. Existing
queue imports with source metadata can learn without a named preset snapshot.

Preparation, transfers, subtitle searches, analysis, muxing, and unknown phases
block additional admission. At or above the target, or with missing/invalid CPU
measurements, new admission stops. Running jobs continue; the target is not a hard
CPU ceiling. A claim already in flight can still start after CPU rises.

Only one claim is in flight at a time. Claims select the oldest compatible queued
job. Empty queues and claim failures retry after five seconds while sampling
continues. CPU measurements include other applications. Low CPU does not establish
GPU, memory, or storage headroom; the concurrency cap is the resource guard in
this version. Automatic mode remains experimental and opt-in.

See [the scheduling comparison](docs/scheduling-comparison.md) for the isolated
benchmark and its limitations.

## Status, concurrency and recovery

```sh
transcoop jobs -server http://coordinator:8765
transcoop retry -server http://coordinator:8765 12
```

The Queue and Workers pages provide **Play**, **Pause**, and **Stop** controls.
Pause prevents new assignments while active jobs finish. Stop also interrupts
active attempts; connected workers cancel on their next heartbeat (within about
15 seconds). Use Retry to restart interrupted jobs. Play resumes new assignments.
Queue controls and individual worker controls are independent: both must be
playing for a worker to receive jobs. Controls persist across restarts and follow
the paired worker, even if its name or process instance changes.

The state flow is `pending -> running -> succeeded | failed | interrupted`.
Claims are atomic. Workers only claim encoders listed in their `-encoders` flag;
setup selects advertised software encoders and verifies hardware encoders with a short encode. Fixed workers
default to one job; automatic workers use measured CPU headroom up to their
concurrency cap (four by default).

A running attempt has a 90-second lease, renewed every 15 seconds. A worker stops
its HandBrake process if renewal fails. Expired jobs become `interrupted` when
the queue is listed, claimed or retried. They require an explicit retry; there is
no automatic reassignment of an expired attempt. Retry creates a new token and
staging path, and stale attempts cannot complete a newer attempt.

The coordinator publishes a nonempty staged result only for the active lease,
using an atomic hard link that refuses to replace an existing destination. The
destination filesystem must support hard links, including in shared mode. A
failure to publish leaves the job failed and the staged output available for
inspection. Filesystems that cannot support this operation need a different
publication strategy before use.

SQLite transactions and filesystem operations are not a single transaction. A
coordinator crash after publication but before recording success can leave a
valid final output with an interrupted job. Inspect the output and job before
retrying; retries refuse to overwrite it. This foundation does not promise
exactly-once encoding or automatic reconciliation of that crash window. Confirm
an old worker has stopped before retrying an interrupted job to avoid wasted
encoding work. Old partial staging files may need manual cleanup.

Keep the database on the coordinator's local disk, **not an SMB/NFS share**, and
run one coordinator process per database. Workers never open SQLite. A stopped
coordinator can be backed up by copying its database files; use SQLite's backup
API or CLI `.backup` for a live backup. When a worker shuts down (Ctrl+C,
SIGTERM, a service stop or restart), it stops its HandBrake processes and returns
their jobs to the queue as `pending`; the attempt is recorded as interrupted. If
the worker cannot reach the coordinator to do this, the job becomes `interrupted`
when its lease expires and needs a retry.

## Tests and current scope

```sh
go test -race ./...
```

Tests use temporary media and a fake HandBrake subprocess, including full HTTP
worker runs in both storage modes. They cover persistence, idempotent/atomic
imports, concurrent claims, encoder matching, stale-attempt rejection, explicit
retries, authentication, failure handling and output overwrite protection.
Automatic-scheduler tests use fake clocks, CPU samples, and executors to verify
observation periods, phase resets, serialized claims, hard caps, sampling failures,
and cancellation. Pairing tests cover expiry, concurrent redemption, role and
attempt ownership, revocation, persistence, and database migration. Folder tests cover discovery, collisions, snapshots, audio selection, missing
languages, failed scans and cancellation. To additionally generate multilingual
media and encode the reviewed selections with real HandBrake in both modes:

```sh
TRANSCOOP_REVIEW_INTEGRATION=1 go test ./internal/review -run TestRealFolderPresetBothStorageModes -v
```

This optional test requires `ffmpeg`, `ffprobe` and `HandBrakeCLI` on PATH.

This version has a CLI, an audio/subtitle review TUI, a browser console, and an
authenticated HTTP API. Remote cancellation, priorities, transfer resumption,
coordinator high availability, and historical metrics remain outside this version. Output
validation checks HandBrake's exit code and a nonempty regular file; it does not
probe the finished media for decoding errors.

The current farm protocol lives in `internal/farm`; the CLI is in
`cmd/transcoop`. The database schema uses versioned migrations for paired device ownership.

## Streaming filesystem storage (experimental)

`worker.mode = "fuse"` reads assigned inputs on demand and streams seekable output
back to the coordinator through a separate `transcoop-fs` helper, so workers need
neither a shared mount nor a full local copy of the source. It requires a native
FUSE driver (libfuse, macFUSE, or WinFsp) and a helper built on the target
platform; release archives do not include the helper yet. Setup recommends this
mode only after a real mount preflight succeeds.

On Linux the helper uses the FUSE 2 library and `fusermount`, which many
distributions no longer install by default even when FUSE 3 is present. Install
them first: `sudo pacman -S fuse2` (Arch), `sudo apt install libfuse2 fuse`
(Debian/Ubuntu), or `sudo dnf install fuse fuse-libs` (Fedora).

Linux x86-64 has been validated with libfuse 2.9 against a LAN coordinator: real
HandBrake encodes, stopping a worker mid-encode (jobs return to the queue and the
mount is removed), and restarting after a helper crash (the stale mount is
cleared automatically). In that test, encoding through the mount was about three
times slower than transfer mode for the same clips, so prefer transfer mode
unless worker disk space is the constraint.

Windows has been validated with WinFsp 2.1 on Windows 11 24H2 (in a VM) against
the same coordinator: real encodes, Ctrl+C mid-encode (the job returns to the
queue), and ending the worker process (the helper unmounts itself and the next
start mounts cleanly). Install WinFsp with its default options; the worker
creates the mount directory's parent and WinFsp creates the mount point itself.

macOS has not been validated with a native driver: on macOS 26.5 with macFUSE
5.5, the FSKit module could not be enabled (System Settings switches it back
off), and the kernel extension requires lowering the Mac's startup security in
Recovery. Do not rely on this mode for important work. See
[filesystem setup, configuration, and validation gates](docs/filesystem.md).

## Development

Maintainers can find the one-time signing key setup and release procedure in
the [release guide](docs/releases.md).

Generated templ Go files are checked in. The generator is pinned:

```sh
go generate ./internal/webui
go test -race ./...
python scripts/test-webui.py --binary /path/to/transcoop --chromium /path/to/chrome
python scripts/test-setup.py --binary /path/to/transcoop
```

The browser smoke test requires Python Playwright, Chromium, and HandBrakeCLI. It
uses an isolated temporary farm to check pairing, restart persistence, queue and
worker controls, media browsing and preparation, imports, filters, polling, log
interaction, mobile layout, retry, and logout. The setup smoke test uses the
installed HandBrakeCLI to verify guided worker setup, saved settings, stable
device identity across restarts, operator pairing, and revocation.

Contributions and bug reports are welcome. Run `gofmt`, `go vet ./...` and
`go test -race ./...` before opening a pull request.

## License

[MIT](LICENSE). Transcoop is not affiliated with the HandBrake project; it runs
HandBrakeCLI as a separate program. The console bundles
[htmx](https://htmx.org) (BSD Zero-Clause, `internal/webui/assets/HTMX-LICENSE.txt`)
and [JetBrains Mono](https://www.jetbrains.com/lp/mono/) (SIL Open Font License
1.1, `internal/webui/assets/JETBRAINS-MONO-OFL.txt`).
