package farm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/friesencr/transcoop/internal/fsproto"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Mapping struct{ From, To string }
type Worker struct {
	Fuse                           fsproto.Settings
	fuse                           *fuseHelper
	reporter                       *workerReporter
	handbrakeVersion               string
	lastIdleLog                    atomic.Int64
	Client                         *Client
	Name, Mode, HandBrake, WorkDir string
	Version                        string // Transcoop version, reported to the coordinator
	Retention                      Retention
	Priority                       string // HandBrake process priority: normal, low or idle
	hintOnce                       sync.Once
	activeMu                       sync.Mutex
	activeDirs                     map[string]bool
	Encoders                       []string
	Mappings                       []Mapping
	MaxJobs                        int
	Scheduler                      string
	CPUTarget                      float64
}

func normalize(path string) string { return strings.ReplaceAll(path, `\`, "/") }

// MapPath uses the longest matching directory prefix, with a component boundary.
func MapPath(path string, mappings []Mapping) string {
	p := normalize(path)
	best := -1
	result := path
	for _, m := range mappings {
		from := strings.TrimRight(normalize(m.From), "/")
		if len(from) > best && (p == from || strings.HasPrefix(p, from+"/")) {
			best = len(from)
			result = filepath.Join(m.To, filepath.FromSlash(strings.TrimPrefix(p[len(from):], "/")))
		}
	}
	return result
}

func RewriteQueue(j Job, source, destination string, mappings []Mapping, transfer bool) ([]byte, error) {
	var root map[string]any
	d := json.NewDecoder(strings.NewReader(string(j.Payload)))
	d.UseNumber()
	if err := d.Decode(&root); err != nil {
		return nil, err
	}
	job, ok := root["Job"].(map[string]any)
	if !ok {
		return nil, errors.New("missing Job object")
	}
	if transfer {
		// External subtitles require another file; this first transfer implementation is single-file.
		var external func(any) bool
		external = func(v any) bool {
			switch v := v.(type) {
			case map[string]any:
				for k, item := range v {
					if text, ok := item.(string); ok && text != "" && (strings.EqualFold(k, "filename") || strings.EqualFold(k, "file") || strings.EqualFold(k, "path")) {
						return true
					}
					if external(item) {
						return true
					}
				}
			case []any:
				for _, item := range v {
					if external(item) {
						return true
					}
				}
			}
			return false
		}
		if external(job["Subtitle"]) {
			return nil, errors.New("transfer mode does not support external subtitle files; use shared mode")
		}
	}
	var walk func(any) any
	walk = func(v any) any {
		switch v := v.(type) {
		case string:
			if normalize(v) == normalize(j.Source) {
				return source
			}
			if normalize(v) == normalize(j.Destination) {
				return destination
			}
			return MapPath(v, mappings)
		case map[string]any:
			for k, item := range v {
				v[k] = walk(item)
			}
			return v
		case []any:
			for i, item := range v {
				v[i] = walk(item)
			}
			return v
		default:
			return v
		}
	}
	walk(root)
	// Explicitly set execution paths, even if the imported paths needed cleaning.
	job["Source"].(map[string]any)["Path"] = source
	job["Destination"].(map[string]any)["File"] = destination
	if ui, ok := root["uiSettings"].(map[string]any); ok {
		ui["source"] = source
		ui["destination"] = destination
		ui["dest_dir"] = filepath.Dir(destination)
		ui["dest_file"] = filepath.Base(destination)
	}
	if title, ok := root["Title"].(map[string]any); ok {
		title["Path"] = source
	}
	return json.Marshal([]any{root})
}

func (w *Worker) Run(ctx context.Context) error {
	if w.MaxJobs < 0 || w.Name == "" || len(w.Encoders) == 0 {
		return errors.New("worker name, encoders and a nonnegative job limit are required")
	}
	if w.Mode != "shared" && w.Mode != "transfer" && w.Mode != "fuse" {
		return errors.New("mode must be shared, transfer, or fuse")
	}
	if _, err := exec.LookPath(w.HandBrake); err != nil {
		return err
	}
	if err := os.MkdirAll(w.WorkDir, 0700); err != nil {
		return err
	}
	if err := w.Retention.Validate(); err != nil {
		return err
	}
	if err := ValidatePriority(w.Priority); err != nil {
		return err
	}
	if w.Priority == "" {
		w.Priority = DefaultPriority
	}
	w.pruneWorkDir()

	if w.Scheduler == "" {
		w.Scheduler = "fixed"
	}
	if w.Scheduler != "fixed" && w.Scheduler != "auto" {
		return errors.New("scheduler must be fixed or auto")
	}
	if w.CPUTarget == 0 {
		w.CPUTarget = 85
	}
	if w.CPUTarget <= 10 || w.CPUTarget > 100 || math.IsNaN(w.CPUTarget) || math.IsInf(w.CPUTarget, 0) {
		return errors.New("CPU target must be greater than 10 and at most 100")
	}
	if w.MaxJobs == 0 {
		w.MaxJobs = 1
		if w.Scheduler == "auto" {
			w.MaxJobs = 4
		}
	}
	var caps map[string]bool
	if _, err := w.Client.JSON(ctx, "GET", "/api/capabilities", "", nil, &caps); err != nil {
		return fmt.Errorf("coordinator connection failed; check pairing and address: %w", err)
	}
	if !caps["pairing"] {
		return errors.New("coordinator does not support this protocol; upgrade all devices together")
	}
	if w.Mode == "fuse" {
		if !caps[fsproto.Capability] {
			return errors.New("coordinator does not support filesystem storage; upgrade coordinator and worker together")
		}
		var err error
		w.fuse, err = startFuse(ctx, w.Fuse.Defaults(w.WorkDir), w.Client)
		if err != nil {
			return err
		}
		defer w.fuse.close()
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		go func() {
			select {
			case <-w.fuse.done:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	handbrake, err := HandBrakeVersion(ctx, w.HandBrake)
	if err != nil {
		log.Printf("warning: %v", err)
	}
	w.handbrakeVersion = handbrake
	log.Printf("worker %q starting: transcoop=%s handbrake=%q storage=%s encoders=%s work_dir=%q scheduler=%s max_jobs=%d priority=%s", w.Name, w.Version, handbrake, w.Mode, strings.Join(w.Encoders, ","), w.WorkDir, w.Scheduler, w.MaxJobs, w.Priority)
	stopReporting := w.startReporting(ctx)
	defer stopReporting()
	if w.Scheduler == "auto" {
		return w.automatic(ctx)
	}
	var wg sync.WaitGroup
	for i := 0; i < w.MaxJobs; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); w.loop(ctx) }()
	}
	wg.Wait()
	return nil
}

// logIdle reports an empty queue at most once a minute per worker, however
// many job slots are polling.
func (w *Worker) logIdle() {
	now := time.Now().UnixNano()
	last := w.lastIdleLog.Load()
	if now-last >= int64(time.Minute) && w.lastIdleLog.CompareAndSwap(last, now) {
		log.Print("waiting: coordinator has no compatible queued job available")
	}
}
func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func (w *Worker) loop(ctx context.Context) {
	for ctx.Err() == nil {
		w.fixedReason("claim in flight")
		var j Job
		status, err := w.Client.JSON(ctx, "POST", "/api/claim"+presetJobQuery, "", ClaimRequest{Worker: w.Name, Encoders: w.Encoders}, &j)
		if err != nil {
			log.Printf("claim: %v", err)
			w.networkHint(err)
		}
		if err != nil || status == http.StatusNoContent {
			if err != nil {
				w.fixedReason("claim failed")
			} else {
				w.fixedReason("no compatible queued job available")
			}
			if err == nil {
				w.logIdle()
			}
			if !wait(ctx, 5*time.Second) {
				return
			}
			continue
		}
		w.lastIdleLog.Store(0)
		w.execute(ctx, j)
	}
}
func (w *Worker) execute(parent context.Context, j Job) {
	w.executeObserved(parent, j, executionHooks{})
}
func (w *Worker) executeObserved(parent context.Context, j Job, hooks executionHooks) {
	started := time.Now()
	originalPhase, lastPhase := hooks.phase, ""
	hooks.phase = func(phase string) {
		if phase != lastPhase {
			log.Printf("job %d: phase=%s", j.ID, phase)
			lastPhase = phase
		}
		if originalPhase != nil {
			originalPhase(phase)
		}
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	attempt := w.attemptDir(j)
	w.markActive(attempt, true)
	defer w.pruneWorkDir()
	defer w.markActive(attempt, false)
	if w.Mode == "fuse" {
		defer func() { w.fuse.call(fsproto.Control{Kind: "remove", Namespace: w.fuseNamespace(j)}) }()
	}
	base := fmt.Sprintf("/api/jobs/%d", j.ID)
	// Renew immediately: time spent receiving the claim must not consume the encode lease.
	confirmed, err := w.heartbeat(ctx, j, j.LeaseUntil)
	if err != nil {
		log.Printf("job %d: initial heartbeat: %v", j.ID, err)
		return
	}
	j.LeaseUntil = confirmed
	done := make(chan struct{})
	go func() {
		defer close(done)
		for wait(ctx, 15*time.Second) {
			next, err := w.heartbeat(ctx, j, confirmed)
			if err != nil {
				log.Printf("job %d: heartbeat lost, stopping: %v", j.ID, err)
				cancel()
				return
			}
			confirmed = next
			if w.Mode == "fuse" {
				if _, err := w.fuse.call(fsproto.Control{Kind: "renew", Namespace: w.fuseNamespace(j), Deadline: confirmed}); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	log.Printf("job %d: encoding on %s (%s)", j.ID, w.Name, w.Mode)
	log.Printf("job %d: source=%q destination=%q", j.ID, j.Source, j.Destination)
	finishReport := w.reportAttempt(ctx, j, &hooks)
	defer w.forgetAttempt(j)
	err = w.encodeObserved(ctx, j, hooks)
	finishReport()
	hooks.setPhase("completion reporting")
	// The heartbeat remains alive through upload and coordinator publication.
	if err == nil {
		_, err = w.Client.JSON(ctx, "POST", base+"/complete", j.Token, map[string]string{"error": ""}, nil)
	}
	if err != nil && parent.Err() != nil {
		// Shutting down (service stop, restart, upgrade): hand the job back
		// so it runs again without an operator retry.
		log.Printf("job %d: worker shutting down after %s; returning the job to the queue", j.ID, time.Since(started).Round(time.Second))
		reportCtx, reportCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, reportErr := w.Client.JSON(reportCtx, "POST", base+"/complete", j.Token, map[string]any{"error": "worker " + w.Name + " shut down", "release": true}, nil)
		reportCancel()
		if reportErr != nil {
			log.Printf("job %d: could not return the job; it is marked interrupted when its lease expires: %v", j.ID, reportErr)
		}
	} else if err != nil {
		log.Printf("job %d: failed after %s: %v", j.ID, time.Since(started).Round(time.Second), err)
		reportCtx, reportCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, reportErr := w.Client.JSON(reportCtx, "POST", base+"/complete", j.Token, map[string]string{"error": err.Error()}, nil)
		reportCancel()
		if reportErr != nil {
			log.Printf("job %d: could not report failure: %v", j.ID, reportErr)
		}
	} else {
		if hooks.succeeded != nil {
			hooks.succeeded()
		}
		log.Printf("job %d: succeeded in %s", j.ID, time.Since(started).Round(time.Second))
		if w.Mode == "transfer" {
			_ = os.Remove(filepath.Join(w.WorkDir, fmt.Sprintf("job-%d-%s", j.ID, j.Token), "output"+filepath.Ext(normalize(j.Destination))))
		}
	}
	cancel()
	<-done
}

// attemptDir is the attempt's folder in the work directory.
func (w *Worker) attemptDir(j Job) string {
	if w.Mode == "fuse" {
		return filepath.Join(w.WorkDir, w.fuseNamespace(j))
	}
	return filepath.Join(w.WorkDir, fmt.Sprintf("job-%d-%s", j.ID, j.Token))
}
func (w *Worker) encodeObserved(ctx context.Context, j Job, hooks executionHooks) error {
	hooks.setPhase("preparation")
	dir := w.attemptDir(j)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	source, destination := MapPath(j.Source, w.Mappings), MapPath(j.Staging, w.Mappings)
	mappings := w.Mappings
	if w.Mode == "fuse" {
		var err error
		source, destination, mappings, err = w.prepareFuse(j)
		if err != nil {
			return err
		}
	}
	if w.Mode == "transfer" {
		source = filepath.Join(dir, "source"+filepath.Ext(normalize(j.Source)))
		destination = filepath.Join(dir, "output"+filepath.Ext(normalize(j.Destination)))
	}
	if !filepath.IsAbs(source) || !filepath.IsAbs(destination) {
		return errors.New("worker paths must be absolute; configure path mappings or an absolute work directory")
	}
	queue, err := RewriteQueue(j, source, destination, mappings, w.Mode == "transfer")
	if err != nil {
		return err
	}
	if w.Mode == "transfer" {
		hooks.setPhase("source transfer")
		if err = w.download(ctx, j, source); err != nil {
			return err
		}
		defer os.Remove(source)
	} else if w.Mode == "shared" {
		if _, err = os.Stat(source); err != nil {
			return fmt.Errorf("source: %w", err)
		}
		if _, err = os.Lstat(MapPath(j.Destination, w.Mappings)); err == nil {
			return errors.New("destination already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err = os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return err
	}
	if _, err = os.Lstat(destination); err == nil {
		return errors.New("staging output already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	queuePath := filepath.Join(dir, "queue.json")
	if err = os.WriteFile(queuePath, queue, 0600); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(dir, "handbrake.log"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	log.Printf("job %d: HandBrake log: %s", j.ID, logFile.Name())
	hooks.setPhase("preparation")
	payload := j.Payload
	if w.Mode == "fuse" {
		var rewritten []json.RawMessage
		if err = json.Unmarshal(queue, &rewritten); err != nil {
			return err
		}
		payload = rewritten[0]
	}
	args, err := PresetArguments(payload, source, destination, dir)
	if err != nil {
		return err
	}
	if args == nil {
		args = []string{"--queue-import-file", queuePath, "--json"}
	}
	cmd := exec.CommandContext(ctx, w.HandBrake, args...)
	var logOutput io.Writer = logFile
	if hooks.report != nil {
		logOutput = io.MultiWriter(logFile, hooks.report)
	}
	output := &lockedWriter{w: logOutput}
	progress := &jobProgressLogger{id: j.ID, now: time.Now}
	cmd.Stdout = &progressWriter{log: output, phase: hooks.setPhase, progress: progress.report, observed: func(t JobTelemetry) {
		if hooks.report != nil {
			hooks.report.observe(t)
		}
	}}
	cmd.Stderr = output
	err = startWithPriority(cmd, w.Priority)
	if err == nil {
		if hooks.started != nil {
			hooks.started(cmd.Process.Pid)
		}
		err = cmd.Wait()
	}
	// Wait has reaped the child and drained both streams, even on cancellation.
	hooks.processExited()
	if err != nil {
		if w.Mode == "fuse" {
			if _, ioErr := w.fuse.call(fsproto.Control{Kind: "check", Namespace: w.fuseNamespace(j)}); ioErr != nil {
				return fmt.Errorf("filesystem: %s (HandBrake log: %s)", Redact(ioErr.Error(), j.Token, w.Client.Token), logFile.Name())
			}
		}
		return fmt.Errorf("HandBrake: %w (log: %s)", err, logFile.Name())
	}
	info, err := os.Stat(destination)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("HandBrake did not produce a nonempty regular output")
	}
	if w.Mode == "transfer" {
		hooks.setPhase("output transfer")
		f, err := os.Open(destination)
		if err != nil {
			return err
		}
		defer f.Close()
		res, err := w.Client.Request(ctx, "PUT", fmt.Sprintf("/api/jobs/%d/output", j.ID), j.Token, f)
		if err != nil {
			return err
		}
		res.Body.Close()
		// Retain the local output until the whole attempt has been acknowledged.
	}
	if w.Mode == "fuse" {
		hooks.setPhase("output durability")
		_, err = w.fuse.call(fsproto.Control{Kind: "seal", Namespace: w.fuseNamespace(j)})
		return err
	}
	return nil
}
func (w *Worker) download(ctx context.Context, j Job, path string) error {
	res, err := w.Client.Request(ctx, "GET", fmt.Sprintf("/api/jobs/%d/source", j.ID), j.Token, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, res.Body)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}
