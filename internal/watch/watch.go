// Package watch queues new videos that appear in watched folders.
//
// Folders are polled rather than observed with inotify so that SMB and NFS
// shares work. A file is queued once its size and modification time have not
// changed for Settle, which skips files that are still being copied. Each file
// version is recorded, so clearing the queue never re-adds it and a file that
// cannot be prepared is not retried until it changes. Sources are never moved
// or deleted.
package watch

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/friesencr/transcoop/internal/farm"
	"github.com/friesencr/transcoop/internal/presets"
	"github.com/friesencr/transcoop/internal/review"
)

// Status is the latest scan of one watch.
type Status struct {
	LastScan time.Time
	Waiting  int    // files seen but not yet settled
	Error    string // why the last scan failed, if it did
}

type candidate struct {
	size, mtime int64
	first       time.Time
}

type file struct {
	path        string
	size, mtime int64
}

type Watcher struct {
	Store      *farm.Store
	HandBrake  string
	PresetDir  string
	Interval   time.Duration // between scans; default 30s
	Settle     time.Duration // unchanged this long before queueing; default 1 min
	MaxPerScan int           // files prepared per watch per scan; default 50

	now     func() time.Time
	prepare func(context.Context, review.Options) ([]json.RawMessage, error)

	mu      sync.Mutex
	status  map[int64]Status
	pending map[int64]map[string]candidate
	trigger chan struct{}
}

func (w *Watcher) init() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status != nil {
		return
	}
	w.status = map[int64]Status{}
	w.pending = map[int64]map[string]candidate{}
	w.trigger = make(chan struct{}, 1)
	if w.Interval <= 0 {
		w.Interval = 30 * time.Second
	}
	if w.Settle <= 0 {
		w.Settle = time.Minute
	}
	if w.MaxPerScan <= 0 {
		w.MaxPerScan = 50
	}
	if w.now == nil {
		w.now = time.Now
	}
	if w.prepare == nil {
		w.prepare = func(ctx context.Context, o review.Options) ([]json.RawMessage, error) {
			return review.PrepareMedia(ctx, o, 1)
		}
	}
}

// Run scans every Interval, or sooner after Trigger, until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	w.init()
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()
	for {
		w.ScanAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-w.trigger:
		}
	}
}

// Trigger requests a scan soon; files still need to settle.
func (w *Watcher) Trigger() {
	w.init()
	select {
	case w.trigger <- struct{}{}:
	default:
	}
}

// Status returns the latest scan state of each watch.
func (w *Watcher) Status() map[int64]Status {
	w.init()
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[int64]Status, len(w.status))
	for k, v := range w.status {
		out[k] = v
	}
	return out
}

// ScanAll scans every enabled watch once.
func (w *Watcher) ScanAll(ctx context.Context) {
	w.init()
	watches, err := w.Store.Watches(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("watch: %v", err)
		}
		return
	}
	live := map[int64]bool{}
	for _, wt := range watches {
		live[wt.ID] = true
		if !wt.Enabled {
			w.setStatus(wt.ID, Status{LastScan: w.now()})
			w.mu.Lock()
			delete(w.pending, wt.ID)
			w.mu.Unlock()
			continue
		}
		waiting, err := w.scan(ctx, wt)
		if ctx.Err() != nil {
			return
		}
		st := Status{LastScan: w.now(), Waiting: waiting}
		if err != nil {
			st.Error = err.Error()
		}
		w.setStatus(wt.ID, st)
	}
	w.mu.Lock()
	for id := range w.status {
		if !live[id] {
			delete(w.status, id)
			delete(w.pending, id)
		}
	}
	w.mu.Unlock()
}

func (w *Watcher) setStatus(id int64, st Status) {
	w.mu.Lock()
	w.status[id] = st
	w.mu.Unlock()
}

// list returns the watch's video files: no hidden files or folders, no
// subfolders unless recursive, and nothing under the output folder.
func list(wt farm.Watch) ([]file, error) {
	var out []file
	err := filepath.WalkDir(wt.Source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == wt.Source {
				return err
			}
			return nil // unreadable subfolder; keep scanning the rest
		}
		if path == wt.Source {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if !wt.Recursive || Within(wt.Output, path) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !review.IsVideoFile(path) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, file{path, info.Size(), info.ModTime().UnixNano()})
		return nil
	})
	return out, err
}

// Within reports whether path is dir or inside it.
func Within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Baseline records the files already in a new watch so only later arrivals are queued.
func Baseline(ctx context.Context, store *farm.Store, wt farm.Watch) (int, error) {
	files, err := list(wt)
	if err != nil {
		return 0, err
	}
	records := make([]farm.WatchFile, len(files))
	for i, f := range files {
		records[i] = farm.WatchFile{Path: f.path, Size: f.size, MTime: f.mtime, Result: farm.WatchExisting, Detail: "present when the watch was created"}
	}
	return len(files), store.RecordWatchFiles(ctx, wt.ID, records)
}

func (w *Watcher) scan(ctx context.Context, wt farm.Watch) (int, error) {
	files, err := list(wt)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, errors.New("folder not found; is the share mounted?")
		}
		return 0, err
	}
	done, err := w.Store.WatchFiles(ctx, wt.ID)
	if err != nil {
		return 0, err
	}
	now := w.now()
	w.mu.Lock()
	seen := w.pending[wt.ID]
	next := map[string]candidate{}
	var ready []file
	for _, f := range files {
		if d, ok := done[f.path]; ok && d.Size == f.size && d.MTime == f.mtime {
			continue
		}
		c, ok := seen[f.path]
		if !ok || c.size != f.size || c.mtime != f.mtime {
			c = candidate{f.size, f.mtime, now}
		}
		if now.Sub(c.first) >= w.Settle && len(ready) < w.MaxPerScan {
			ready = append(ready, f)
			continue
		}
		next[f.path] = c
	}
	w.pending[wt.ID] = next
	w.mu.Unlock()
	for _, f := range ready {
		if ctx.Err() != nil {
			return len(next), ctx.Err()
		}
		record := w.queue(ctx, wt, f)
		if ctx.Err() != nil {
			return len(next), ctx.Err()
		}
		if err := w.Store.RecordWatchFiles(ctx, wt.ID, []farm.WatchFile{record}); err != nil {
			return len(next), err
		}
	}
	return len(next), nil
}

// queue prepares one settled file with the watch's preset and imports it.
func (w *Watcher) queue(ctx context.Context, wt farm.Watch, f file) farm.WatchFile {
	record := farm.WatchFile{Path: f.path, Size: f.size, MTime: f.mtime}
	rel, err := filepath.Rel(wt.Source, filepath.Dir(f.path))
	if err != nil {
		rel = "."
	}
	opts := review.Options{Queue: f.path, OutputDir: filepath.Join(wt.Output, rel), Preset: wt.Preset, HandBrake: w.HandBrake}
	if p := presets.Find(w.PresetDir, wt.Preset); p != nil {
		opts.PresetFile = p.File
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	queue, err := w.prepare(ctx, opts)
	n := 0
	if err == nil {
		n, err = w.Store.Import(ctx, queue)
	}
	switch {
	case err != nil && strings.Contains(err.Error(), "output already exists"):
		record.Result, record.Detail = farm.WatchSkipped, "output already exists"
	case err != nil:
		record.Result, record.Detail = farm.WatchFailed, err.Error()
	case n == 0:
		record.Result, record.Detail = farm.WatchSkipped, "already in the queue"
	default:
		record.Result = farm.WatchQueued
	}
	if record.Result == farm.WatchQueued {
		log.Printf("watch %s: queued %s", wt.Source, f.path)
	} else {
		log.Printf("watch %s: %s %s: %s", wt.Source, record.Result, f.path, record.Detail)
	}
	return record
}

// Validate checks a new watch's folders.
func Validate(wt farm.Watch) error {
	if !filepath.IsAbs(wt.Source) || !filepath.IsAbs(wt.Output) {
		return errors.New("use absolute paths for the watched and output folders")
	}
	info, err := os.Stat(wt.Source)
	if err != nil || !info.IsDir() {
		return errors.New("the watched folder must exist on the coordinator")
	}
	if filepath.Clean(wt.Source) == filepath.Clean(wt.Output) {
		return errors.New("the output folder must differ from the watched folder")
	}
	if Within(wt.Output, wt.Source) {
		return errors.New("the watched folder cannot be inside the output folder")
	}
	if wt.Recursive && Within(wt.Source, wt.Output) {
		return errors.New("with subfolders included, the output folder must be outside the watched folder; encoded files would be picked up again")
	}
	if wt.Preset == "" {
		return errors.New("choose a preset")
	}
	return nil
}
