package webui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/friesencr/transcoop/internal/farm"
	"github.com/friesencr/transcoop/internal/fsproto"
)

type versionKey struct{}

// versionFrom returns the coordinator version that render placed in the context.
func versionFrom(ctx context.Context) string {
	if v, _ := ctx.Value(versionKey{}).(string); v != "" {
		return v
	}
	return "dev"
}

var navItems = []struct{ Href, Label string }{
	{"/", "Dashboard"}, {"/jobs", "Queue"}, {"/workers", "Workers"},
	{"/add", "Add jobs"}, {"/watch", "Watch"}, {"/import", "Import"}, {"/devices", "Devices"},
}

// navSection maps a page title to the navigation entry it belongs to.
func navSection(title string) string {
	switch {
	case title == "Dashboard":
		return "/"
	case title == "Queue" || strings.HasPrefix(title, "Job "):
		return "/jobs"
	case title == "Workers":
		return "/workers"
	case title == "Add jobs" || title == "Preview jobs":
		return "/add"
	case title == "Watch folders":
		return "/watch"
	case title == "Import":
		return "/import"
	case title == "Devices":
		return "/devices"
	}
	return ""
}

// baseName and dirName split coordinator paths from any platform.
func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 && i < len(p)-1 {
		return p[i+1:]
	}
	return p
}

func dirName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i > 0 {
		return p[:i]
	}
	return ""
}

func ago(t int64) string {
	if t == 0 {
		return "—"
	}
	d := time.Since(time.Unix(t, 0))
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%d d ago", int(d.Hours()/24))
	}
	return time.Unix(t, 0).Format("Jan 2, 2006")
}

func isoTime(t int64) string { return time.Unix(t, 0).UTC().Format(time.RFC3339) }

// exactTime is replaced with the browser's local time by ui.js.
func exactTime(t int64) string { return time.Unix(t, 0).Format("2006-01-02 15:04:05 MST") }

func duration(seconds int64) string {
	if seconds < 0 {
		return "—"
	}
	d := time.Duration(seconds) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", seconds)
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%dh %02dm", seconds/3600, seconds%3600/60)
}

func took(started, finished int64) string {
	if started == 0 || finished < started {
		return ""
	}
	return duration(finished - started)
}

func etaText(n *int) string {
	if n == nil || *n <= 0 {
		return "—" // HandBrake reports 0 until it has an estimate
	}
	return duration(int64(*n))
}

func fps(n *float64) string {
	if n == nil || *n <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f", *n)
}

func percent(n *float64) string {
	if n == nil {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", *n*100)
}

func fractionValue(n *float64) string {
	if n == nil {
		return "0"
	}
	return fmt.Sprintf("%.4f", min(max(*n, 0), 1))
}

func byteSize(n int64) string {
	switch {
	case n < 0:
		return "—"
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.0f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
}

// sizeChange describes the output size relative to the source, e.g. "−66%".
func sizeChange(src, out int64) string {
	if src <= 0 || out < 0 {
		return ""
	}
	return strings.Replace(fmt.Sprintf("%+.0f%%", (float64(out)/float64(src)-1)*100), "-", "−", 1)
}

// fileSize stats a coordinator path; -1 when it is unavailable.
func fileSize(p string) int64 {
	if p == "" {
		return -1
	}
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return -1
	}
	return st.Size()
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 160 {
		s = s[:157] + "…"
	}
	return s
}

// jobRow is a job with its latest attempt and media sizes for list views.
type jobRow struct {
	farm.Job
	Latest   *farm.AttemptSummary
	Src, Out int64
}

func newJobRow(j farm.Job, summaries map[int64]farm.AttemptSummary) jobRow {
	row := jobRow{Job: j, Src: fileSize(j.Source), Out: -1}
	if a, ok := summaries[j.ID]; ok {
		row.Latest = &a
	}
	if j.Status == "succeeded" {
		row.Out = fileSize(j.Destination)
	}
	return row
}

func (r jobRow) Took() string {
	if r.Latest == nil {
		return ""
	}
	return took(r.Latest.Started, r.Latest.Finished)
}

// hardwareEncoder recognises HandBrake's GPU and media-engine encoders.
func hardwareEncoder(name string) bool {
	for _, marker := range []string{"qsv", "nvenc", "vce", "amf", "vaapi", "toolbox"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	// VideoToolbox (vt_h265) and Media Foundation (mf_h264); not svt_av1.
	return strings.HasPrefix(name, "vt_") || strings.HasPrefix(name, "mf_")
}

func splitEncoders(encoders []string) (hardware, software []string) {
	for _, e := range encoders {
		if hardwareEncoder(e) {
			hardware = append(hardware, e)
		} else {
			software = append(software, e)
		}
	}
	return hardware, software
}

// slots lists the worker's job slots: an active job ID or 0 for a free slot.
func slots(v farm.WorkerStatus) []int64 {
	out := make([]int64, max(v.MaxJobs, len(v.Active)))
	copy(out, v.Active)
	return out
}

func cpuValue(v farm.WorkerStatus) string {
	if v.CPU == nil {
		return "0"
	}
	return fmt.Sprintf("%.1f", min(max(*v.CPU, 0), 100))
}

func filesystemSummary(s *fsproto.Stats) string {
	if s == nil {
		return ""
	}
	mounted := "not mounted"
	if s.Mounted {
		mounted = "mounted"
	}
	return fmt.Sprintf("%s · read %.1f MiB/s · write %.1f MiB/s · cache %.0f MiB · dirty %.0f MiB · retries %d · waits %d",
		mounted, s.ReadRate/(1<<20), s.WriteRate/(1<<20), float64(s.CacheBytes)/(1<<20), float64(s.DirtyBytes)/(1<<20), s.Retries, s.Waiting)
}

func versionSummary(v farm.WorkerStatus) string {
	parts := []string{}
	if v.Version != "" {
		parts = append(parts, "transcoop "+v.Version)
	}
	if v.HandBrake != "" {
		parts = append(parts, v.HandBrake)
	}
	return strings.Join(parts, " · ")
}

func staleTelemetry(t *farm.JobTelemetry) bool { return time.Now().Unix()-t.Updated >= 15 }

func retryable(status string) bool { return status == "failed" || status == "interrupted" }

// crumb is one clickable segment of a browsed folder path.
type crumb struct{ Name, URL string }

func watchToggle(enabled bool) string {
	if enabled {
		return "pause"
	}
	return "resume"
}
