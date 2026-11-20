# Scheduling comparison — 2026-09-23

This measures the CPU-driven scheduler that replaces point estimates. On this
synthetic workload, fixed concurrency of four completed the batch fastest.
Automatic admission was conservative and did not improve throughput; it remains
opt-in. These measurements are not a universal recommendation for concurrency.

Each mode ran three times, with eight 120-second generated clips per run.
All **96 outputs** completed successfully and FFprobe confirmed their durations.

| Mode | Median batch time (range) | Average CPU | Samples ≥85% CPU | Peak sampled worker-tree RSS | Maximum active jobs |
| --- | ---: | ---: | ---: | ---: | ---: |
| fixed-1 | 134.5 s (134.5–135.5) | 27.6% | 0/403 | 1.84 GiB | 1 |
| fixed-2 | 89.5 s (89.5–89.5) | 46.2% | 0/267 | 2.86 GiB | 2 |
| fixed-4 | 71.6 s (71.6–72.7) | 67.7% | 135/214 | 4.68 GiB | 4 |
| auto-4 | 161.5 s (160.5–162.5) | 23.5% | 0/483 | 1.85 GiB | 2 |

Average CPU pools all one-second whole-machine samples across each mode's three
runs. RSS is the largest one-second sample across those runs, summed over the
worker and every descendant across all worker threads. Shared memory can be
counted more than once, and brief peaks between samples can be missed. Coordinator
memory is excluded. Maximum active jobs is reconstructed from the execution log.

## Interpretation

Automatic mode uses an 85% CPU target, a four-job cap, and a twenty-second
observation window below 75% CPU while all active jobs are in video encoding.
It also waits for five low-CPU samples when idle. Job/phase changes reset observation.
Many jobs in this batch finish quickly enough that these conditions prevent much
overlap. Lower CPU usage here includes waiting; it does not establish more
efficient encoding or a hard resource ceiling.

Fixed concurrency provides a useful baseline. Raising the cap can increase
throughput and peak CPU/memory use. The automatic policy needs evaluation on
longer real media, mixed phases, and different hardware before becoming a default.
No point calibration or machine-specific point budget is required by either mode.

## Workload and reproduction

- CPU: AMD Ryzen 9 9950X 16-Core Processor (32 logical CPUs).
- Platform: `Linux-7.2.7-1-cachyos-x86_64-with-glibc2.44`.
- HandBrake: `HandBrake 20260607133641-9eb6c9368-makepkg`.
- Inputs: FFmpeg testsrc2, H.264 ultrafast CRF 18, warm input caches.
- Encodes: quality 22, MKV, single pass, no audio/subtitles/additional filters.
- Storage: shared local files; separate databases and output directories per run.

| Source/output | Encoder / preset |
| --- | --- |
| 1280×720, 30 fps | x265 / medium |
| 1920×1080, 30 fps | x264 / slow |
| 1280×720, 60 fps | x264 / medium |
| 720×480, 30 fps | x265 / slow |

The four settings occur twice in each eight-job batch. Order was seeded/shuffled:
`1:auto-4, 1:fixed-2, 1:fixed-1, 1:fixed-4, 2:auto-4, 2:fixed-2, 2:fixed-1, 2:fixed-4, 3:fixed-2, 3:fixed-4, 3:fixed-1, 3:auto-4`.

Other development/verification processes ran on the host during this comparison;
CPU samples include them. Elapsed time includes worker startup and polling with
roughly one-second resolution. This is a repeated synthetic regression workload,
not an isolated production-media benchmark or a measure of desktop responsiveness.
GPU and memory-aware scheduling are not implemented.

```sh
go build -o /tmp/transcoop ./cmd/transcoop
python3 scripts/compare-scheduling.py \
  --binary /tmp/transcoop \
  --output /tmp/transcoop-comparison-new \
  --seconds 120 --repeats 3
```

The output directory must not exist. The script uses temporary paired credentials,
never contacts an existing farm, checks every output duration, and retains media,
logs, incremental results, and final results. Requires Linux, FFmpeg, FFprobe, and
HandBrakeCLI. CPU and process-tree memory sampling are Linux-specific.

[Raw results, CPU samples, and admission timelines](scheduling-comparison-results.json)
include a hash of the measured binary and scheduler source. The binary used the
same admission policy; subsequent changes covered configuration validation,
administrator credential rotation, and browser layout rather than scheduling.
