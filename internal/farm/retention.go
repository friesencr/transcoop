package farm

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// Retention limits how much a worker keeps from finished attempts in its
// work directory. Zero values select the defaults.
type Retention struct {
	Days             int `toml:"days,omitempty"`               // delete attempt folders older than this
	Attempts         int `toml:"attempts,omitempty"`           // keep at most this many attempt folders
	FailedOutputDays int `toml:"failed_output_days,omitempty"` // delete leftover media sooner, keeping logs
}

const (
	DefaultRetentionDays     = 14
	DefaultRetentionAttempts = 100
	DefaultFailedOutputDays  = 2
)

func (r Retention) Defaults() Retention {
	if r.Days == 0 {
		r.Days = DefaultRetentionDays
	}
	if r.Attempts == 0 {
		r.Attempts = DefaultRetentionAttempts
	}
	if r.FailedOutputDays == 0 {
		r.FailedOutputDays = DefaultFailedOutputDays
	}
	return r
}

func (r Retention) Validate() error {
	if r.Days < 0 || r.Attempts < 0 || r.FailedOutputDays < 0 {
		return fmt.Errorf("worker.retention values must be positive")
	}
	if d := r.Defaults(); d.FailedOutputDays > d.Days {
		return fmt.Errorf("worker.retention.failed_output_days must not exceed days")
	}
	return nil
}

// attemptFolder matches only folders the worker creates for attempts:
// job-<id>-<attempt token> (shared/transfer) or job-<id>-attempt-<n> (fuse).
var attemptFolder = regexp.MustCompile(`^job-[0-9]+-(?:[0-9a-f]{48}|attempt-[0-9]+)$`)

// keptFiles survive the failed-output cleanup until the whole folder expires.
var keptFiles = map[string]bool{"handbrake.log": true, "queue.json": true}

type attemptInfo struct {
	path     string
	modified time.Time
}

// pruneAttempts applies the retention policy to dir. Folders in active and
// anything not created by the worker are never touched.
func pruneAttempts(dir string, r Retention, active map[string]bool, now time.Time) (removed int, freed int64, err error) {
	r = r.Defaults()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, err
	}
	var attempts []attemptInfo
	for _, e := range entries {
		if !e.IsDir() || !attemptFolder.MatchString(e.Name()) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if active[p] {
			continue
		}
		attempts = append(attempts, attemptInfo{p, lastActivity(p)})
	}
	// Newest first, so everything past the count limit is the oldest.
	sort.Slice(attempts, func(i, j int) bool { return attempts[i].modified.After(attempts[j].modified) })
	maxAge := time.Duration(r.Days) * 24 * time.Hour
	outputAge := time.Duration(r.FailedOutputDays) * 24 * time.Hour
	for i, a := range attempts {
		age := now.Sub(a.modified)
		if i >= r.Attempts || age > maxAge {
			size := folderSize(a.path)
			if e := os.RemoveAll(a.path); e != nil {
				err = e
				continue
			}
			removed++
			freed += size
			continue
		}
		if age > outputAge {
			freed += removeLeftovers(a.path)
		}
	}
	return removed, freed, err
}

// lastActivity is the newest modification time of a folder or its files.
func lastActivity(p string) time.Time {
	newest := time.Time{}
	if st, err := os.Stat(p); err == nil {
		newest = st.ModTime()
	}
	entries, _ := os.ReadDir(p)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}

func folderSize(p string) int64 {
	var total int64
	_ = filepath.WalkDir(p, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, e := d.Info(); e == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// removeLeftovers deletes everything in an attempt folder except its logs.
func removeLeftovers(p string) int64 {
	var freed int64
	entries, _ := os.ReadDir(p)
	for _, e := range entries {
		if keptFiles[e.Name()] {
			continue
		}
		child := filepath.Join(p, e.Name())
		size := folderSize(child)
		if info, err := e.Info(); err == nil && !e.IsDir() {
			size = info.Size()
		}
		if os.RemoveAll(child) == nil {
			freed += size
		}
	}
	return freed
}

// pruneWorkDir runs the retention policy and logs what it reclaimed.
func (w *Worker) pruneWorkDir() {
	w.activeMu.Lock()
	active := make(map[string]bool, len(w.activeDirs))
	for p := range w.activeDirs {
		active[p] = true
	}
	w.activeMu.Unlock()
	removed, freed, err := pruneAttempts(w.WorkDir, w.Retention, active, time.Now())
	if err != nil {
		log.Printf("retention: %v", err)
	}
	if removed > 0 || freed > 0 {
		log.Printf("retention: removed %d attempt folders, freed %.1f MB", removed, float64(freed)/(1<<20))
	}
}

func (w *Worker) markActive(dir string, active bool) {
	w.activeMu.Lock()
	defer w.activeMu.Unlock()
	if w.activeDirs == nil {
		w.activeDirs = map[string]bool{}
	}
	if active {
		w.activeDirs[dir] = true
	} else {
		delete(w.activeDirs, dir)
	}
}
