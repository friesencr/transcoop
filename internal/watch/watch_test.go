package watch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/friesencr/transcoop/internal/farm"
	"github.com/friesencr/transcoop/internal/review"
)

type harness struct {
	t      *testing.T
	store  *farm.Store
	w      *Watcher
	clock  time.Time
	src    string
	out    string
	opts   []review.Options
	failOn map[string]error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := farm.OpenStore(filepath.Join(t.TempDir(), "farm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h := &harness{t: t, store: store, clock: time.Unix(1_800_000_000, 0), src: t.TempDir(), out: t.TempDir(), failOn: map[string]error{}}
	h.w = &Watcher{Store: store, Settle: time.Minute}
	h.w.now = func() time.Time { return h.clock }
	h.w.prepare = func(_ context.Context, o review.Options) ([]json.RawMessage, error) {
		h.opts = append(h.opts, o)
		if err := h.failOn[filepath.Base(o.Queue)]; err != nil {
			return nil, err
		}
		dest := filepath.Join(o.OutputDir, strings.TrimSuffix(filepath.Base(o.Queue), filepath.Ext(o.Queue))+".mp4")
		raw, _ := json.Marshal(map[string]any{"Job": map[string]any{"Source": map[string]string{"Path": o.Queue}, "Destination": map[string]string{"File": dest}, "Video": map[string]string{"Encoder": "x264"}}})
		return []json.RawMessage{raw}, nil
	}
	return h
}

func (h *harness) write(rel, body string) {
	h.t.Helper()
	p := filepath.Join(h.src, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) add(recursive bool) farm.Watch {
	h.t.Helper()
	wt := farm.Watch{Source: h.src, Output: h.out, Preset: "Fast 1080p30", Recursive: recursive, Enabled: true}
	id, err := h.store.AddWatch(context.Background(), wt)
	if err != nil {
		h.t.Fatal(err)
	}
	wt.ID = id
	return wt
}

func (h *harness) scanAt(offset time.Duration) Status {
	h.t.Helper()
	h.clock = time.Unix(1_800_000_000, 0).Add(offset)
	h.w.ScanAll(context.Background())
	for _, st := range h.w.Status() {
		return st
	}
	return Status{}
}

func (h *harness) jobs() []string {
	h.t.Helper()
	jobs, err := h.store.List(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, j := range jobs {
		out = append(out, filepath.Base(j.Source))
	}
	return out
}

func TestWatchQueuesSettledFilesOnce(t *testing.T) {
	h := newHarness(t)
	h.write("a.mkv", "one")
	h.write(".partial.mkv", "hidden")
	h.write("notes.txt", "not video")
	h.write("season/b.mkv", "nested")
	wt := h.add(false)

	if st := h.scanAt(0); st.Waiting != 1 || len(h.jobs()) != 0 {
		t.Fatalf("queued before settling: %+v %v", st, h.jobs())
	}
	h.scanAt(30 * time.Second)
	if len(h.jobs()) != 0 {
		t.Fatal("queued before settling")
	}
	if st := h.scanAt(61 * time.Second); st.Waiting != 0 || strings.Join(h.jobs(), ",") != "a.mkv" {
		t.Fatalf("%+v %v", st, h.jobs())
	}
	if h.opts[0].OutputDir != h.out || h.opts[0].Preset != "Fast 1080p30" {
		t.Fatalf("%+v", h.opts[0])
	}
	h.scanAt(5 * time.Minute)
	if len(h.opts) != 1 {
		t.Fatal("recorded file prepared again")
	}

	// Clearing the queue must not bring the file back.
	if _, err := h.store.ClearQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.scanAt(10 * time.Minute)
	if len(h.jobs()) != 0 || len(h.opts) != 1 {
		t.Fatal("cleared file re-queued")
	}

	// A changed file is a new version; it settles again.
	h.write("a.mkv", "two!")
	h.scanAt(11 * time.Minute)
	h.scanAt(12*time.Minute + time.Second)
	if strings.Join(h.jobs(), ",") != "a.mkv" {
		t.Fatal("changed file not queued", h.jobs())
	}
	s, _ := h.store.WatchSummaries(context.Background(), 5)
	if s[wt.ID].Counts[farm.WatchQueued] != 1 {
		t.Fatalf("%+v", s[wt.ID])
	}
}

func TestWatchWaitsForGrowingFiles(t *testing.T) {
	h := newHarness(t)
	h.add(false)
	body := "x"
	for i := 0; i < 5; i++ {
		body += "more"
		h.write("copying.mkv", body)
		h.scanAt(time.Duration(i) * 50 * time.Second)
	}
	if len(h.jobs()) != 0 {
		t.Fatal("queued a file that was still growing")
	}
	h.scanAt(10 * time.Minute)
	if len(h.jobs()) != 1 {
		t.Fatal("stable file not queued")
	}
}

func TestWatchRecursiveBaselineAndFailures(t *testing.T) {
	h := newHarness(t)
	h.write("old.mkv", "already here")
	wt := h.add(true)
	if n, err := Baseline(context.Background(), h.store, wt); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	h.write("Show/S01/e1.mkv", "new")
	h.write("exists.mkv", "new")
	h.write("broken.mkv", "new")
	h.failOn["exists.mkv"] = errors.New("output already exists: /x.mp4")
	h.failOn["broken.mkv"] = errors.New("HandBrake: no title found")
	h.scanAt(0)
	h.scanAt(2 * time.Minute)
	if strings.Join(h.jobs(), ",") != "e1.mkv" {
		t.Fatal(h.jobs())
	}
	for _, o := range h.opts {
		if filepath.Base(o.Queue) == "e1.mkv" && o.OutputDir != filepath.Join(h.out, "Show", "S01") {
			t.Fatal("subfolder layout not mirrored:", o.OutputDir)
		}
		if filepath.Base(o.Queue) == "old.mkv" {
			t.Fatal("baseline file prepared")
		}
	}
	files, _ := h.store.WatchFiles(context.Background(), wt.ID)
	got := map[string]string{}
	for p, f := range files {
		got[filepath.Base(p)] = f.Result
	}
	want := map[string]string{"old.mkv": farm.WatchExisting, "e1.mkv": farm.WatchQueued, "exists.mkv": farm.WatchSkipped, "broken.mkv": farm.WatchFailed}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %s, want %s", k, got[k], v)
		}
	}
	// A failed file is not retried until it changes.
	before := len(h.opts)
	h.scanAt(10 * time.Minute)
	if len(h.opts) != before {
		t.Fatal("failed file retried without changing")
	}
}

func TestWatchDisabledAndMissingFolder(t *testing.T) {
	h := newHarness(t)
	h.write("a.mkv", "x")
	wt := h.add(false)
	if err := h.store.SetWatchEnabled(context.Background(), wt.ID, false); err != nil {
		t.Fatal(err)
	}
	h.scanAt(0)
	h.scanAt(5 * time.Minute)
	if len(h.opts) != 0 {
		t.Fatal("disabled watch queued files")
	}
	if err := h.store.SetWatchEnabled(context.Background(), wt.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(h.src); err != nil {
		t.Fatal(err)
	}
	if st := h.scanAt(6 * time.Minute); !strings.Contains(st.Error, "not found") {
		t.Fatalf("%+v", st)
	}
}

func TestValidate(t *testing.T) {
	src := t.TempDir()
	ok := []farm.Watch{
		{Source: src, Output: filepath.Join(t.TempDir(), "out"), Preset: "p"},
		{Source: src, Output: filepath.Join(src, "done"), Preset: "p"},
	}
	for _, wt := range ok {
		if err := Validate(wt); err != nil {
			t.Errorf("%+v: %v", wt, err)
		}
	}
	bad := []farm.Watch{
		{Source: "relative", Output: "/out", Preset: "p"},
		{Source: filepath.Join(src, "missing"), Output: "/out", Preset: "p"},
		{Source: src, Output: src, Preset: "p"},
		{Source: src, Output: filepath.Dir(src), Preset: "p"},
		{Source: src, Output: filepath.Join(src, "done"), Preset: "p", Recursive: true},
		{Source: src, Output: "/out"},
	}
	for _, wt := range bad {
		if Validate(wt) == nil {
			t.Errorf("accepted %+v", wt)
		}
	}
}
