package farm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/friesencr/transcoop/internal/fsproto"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
)

type workerReporter struct {
	mu     sync.Mutex
	status WorkerStatus
	active map[attemptKey]bool
}

func (w *Worker) startReporting(ctx context.Context) func() {
	if ctx.Err() != nil || w.Client == nil {
		return func() {}
	}
	var id [24]byte
	if _, err := rand.Read(id[:]); err != nil {
		return func() {}
	}
	w.reporter = &workerReporter{status: WorkerStatus{Instance: hex.EncodeToString(id[:]), Name: w.Name, Version: w.Version, HandBrake: w.handbrakeVersion, Priority: w.Priority, Mode: w.Mode, Encoders: w.Encoders, Scheduler: w.Scheduler, MaxJobs: w.MaxJobs, CPUTarget: w.CPUTarget, Reason: "starting"}, active: map[attemptKey]bool{}}
	ctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if w.Scheduler != "auto" {
			_, _ = cpu.PercentWithContext(ctx, 0, false)
		}
		var previous fsproto.Stats
		previousAt := time.Now()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			w.reporter.mu.Lock()
			v := w.reporter.status
			v.Active = append([]int64{}, v.Active...)
			if w.Scheduler != "auto" {
				v.Active = nil
				for id := range w.reporter.active {
					v.Active = append(v.Active, id.id)
				}
				v.MaxJobs = max(1, w.MaxJobs)
				if len(v.Active) >= v.MaxJobs {
					v.Reason = "Concurrency limit reached"
				}
			}
			w.reporter.mu.Unlock()
			if w.Scheduler != "auto" {
				if values, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(values) == 1 {
					v.CPU = &values[0]
				}
			}
			if w.fuse != nil {
				reply, err := w.fuse.call(fsproto.Control{Kind: "stats"})
				if err == nil {
					s := reply.Stats
					elapsed := time.Since(previousAt).Seconds()
					s.ReadRate = float64(s.ReadBytes-previous.ReadBytes) / elapsed
					s.WriteRate = float64(s.WriteBytes-previous.WriteBytes) / elapsed
					previous = s
					previousAt = time.Now()
					v.Filesystem = &s
				}
			}
			send, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, _ = w.Client.JSON(send, "POST", "/api/workers/"+v.Instance+"/status", "", v, nil)
			cancel()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { stop(); <-done }
}
func (w *Worker) admissionReport(a *admissionState, reason string) {
	if w.reporter == nil {
		return
	}
	r := w.reporter
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.Reason = reason
	r.status.Headroom = a.headroom
	r.status.Active = nil
	for k := range a.active {
		r.status.Active = append(r.status.Active, k.id)
	}
	r.status.CPU = nil
	if a.cpu.valid && len(a.cpu.readings) > 0 {
		v := a.cpu.average
		r.status.CPU = &v
	}
}
func (w *Worker) fixedReason(reason string) {
	if w.reporter != nil {
		w.reporter.mu.Lock()
		w.reporter.status.Reason = reason
		w.reporter.mu.Unlock()
	}
}

type attemptReport struct {
	mu      sync.Mutex
	data    JobTelemetry
	tail    []byte
	secrets []string
}

func (a *attemptReport) Write(b []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := len(b)
	if len(b) >= TailLimit {
		a.tail = append(a.tail[:0], b[len(b)-TailLimit:]...)
		a.data.Truncated = true
	} else {
		a.tail = append(a.tail, b...)
		if len(a.tail) > TailLimit {
			a.tail = append([]byte(nil), a.tail[len(a.tail)-TailLimit:]...)
			a.data.Truncated = true
		}
	}
	return n, nil
}
func (a *attemptReport) phase(p string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.data.Phase != p {
		a.data.Phase = p
		a.data.Fraction = nil
		a.data.FPS = nil
		a.data.AverageFPS = nil
		a.data.ETA = nil
		a.data.Pass = 0
		a.data.PassCount = 0
	}
}
func (a *attemptReport) observe(t JobTelemetry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.data.Phase = t.Phase
	a.data.Pass = t.Pass
	a.data.PassCount = t.PassCount
	a.data.Fraction = t.Fraction
	a.data.FPS = t.FPS
	a.data.AverageFPS = t.AverageFPS
	a.data.ETA = t.ETA
}
func (a *attemptReport) snapshot() JobTelemetry {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := a.data
	v.Tail, v.Truncated = cleanTail(string(a.tail), v.Truncated, a.secrets...)
	return v
}
func (w *Worker) reportAttempt(ctx context.Context, j Job, h *executionHooks) func() {
	if w.reporter == nil {
		return func() {}
	}
	r := w.reporter
	r.mu.Lock()
	r.active[attemptKey{j.ID, j.Token}] = true
	instance := r.status.Instance
	r.mu.Unlock()
	a := &attemptReport{data: JobTelemetry{Instance: instance, Phase: "preparation"}, secrets: []string{w.Client.Token, j.Token}}
	old := h.phase
	h.phase = func(p string) {
		a.phase(p)
		if old != nil {
			old(p)
		}
	}
	h.report = a
	reportCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	send := func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_, _ = w.Client.JSON(ctx, "POST", fmt.Sprintf("/api/jobs/%d/telemetry", j.ID), j.Token, a.snapshot(), nil)
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-reportCtx.Done():
				return
			case <-ticker.C:
				send(reportCtx)
			}
		}
	}()
	return func() {
		cancel()
		<-done
		send(context.Background())
	}
}

func (w *Worker) forgetAttempt(j Job) {
	if w.reporter != nil {
		w.reporter.mu.Lock()
		delete(w.reporter.active, attemptKey{j.ID, j.Token})
		w.reporter.mu.Unlock()
	}
}
