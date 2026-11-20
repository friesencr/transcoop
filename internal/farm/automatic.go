package farm

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/process"
)

type cpuReading struct {
	at    time.Time
	value float64
}
type cpuWindow struct {
	readings []cpuReading
	last     time.Time
	average  float64
	valid    bool
}

func (c *cpuWindow) sample(now time.Time, value float64, err error) bool {
	continuous := !c.last.IsZero() && now.Sub(c.last) >= 500*time.Millisecond && now.Sub(c.last) <= 1500*time.Millisecond
	if !continuous {
		c.readings = nil
	}
	c.last = now
	c.valid = err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 100
	if !c.valid {
		c.readings = nil
		return false
	}
	c.readings = append(c.readings, cpuReading{now, value})
	for len(c.readings) > 0 && now.Sub(c.readings[0].at) >= 10*time.Second {
		c.readings = c.readings[1:]
	}
	c.average = 0
	for _, v := range c.readings {
		c.average += v.value
	}
	c.average /= float64(len(c.readings))
	return continuous
}
func (c *cpuWindow) fresh(now time.Time) bool {
	return c.valid && !c.last.IsZero() && now.Sub(c.last) <= 1500*time.Millisecond
}

type executionHooks struct {
	report    *attemptReport
	phase     func(string)
	exited    func()
	started   func(int)
	succeeded func()
}

func (h executionHooks) setPhase(p string) {
	if h.phase != nil {
		h.phase(p)
	}
}
func (h executionHooks) processExited() {
	if h.exited != nil {
		h.exited()
	}
}

type attemptKey struct {
	id    int64
	token string
}
type attemptReservation struct {
	phase, workload        string
	pid                    int
	previousCPU            float64
	previousAt, encodingAt time.Time
	observation            workloadObservation
	succeeded              bool
}
type admissionState struct {
	cap           int
	target        float64
	cpu           cpuWindow
	active        map[attemptKey]*attemptReservation
	headroom      int
	profiles      map[string]workloadProfile
	refills       map[string][]time.Time
	candidate     *ClaimCandidate
	metricsFailed bool
}

const headroomSamples = 20

func (a *admissionState) observe(now time.Time, value float64, err error) {
	if !a.cpu.sample(now, value, err) {
		a.headroom = 0
	}
	if !a.cpu.fresh(now) || value >= a.target-10 || a.cpu.average >= a.target-10 || len(a.active) >= a.cap {
		a.headroom = 0
		return
	}
	for _, r := range a.active {
		if r.phase != "video encoding" {
			a.headroom = 0
			return
		}
	}
	// Preserve earned observation while a candidate refresh is in flight.
	a.headroom = min(max(a.headroom, headroomSamples, a.observationSamples(now)), a.headroom+1)
}
func (a *admissionState) reason(now time.Time) string {
	if !a.cpu.fresh(now) {
		return "CPU measurements unavailable"
	}
	if len(a.active) >= a.cap {
		return "Concurrency limit reached"
	}
	if a.cpu.average >= a.target {
		return "CPU target reached"
	}
	for _, r := range a.active {
		if r.phase != "video encoding" {
			return "Waiting for video encoding"
		}
	}
	for _, r := range a.active {
		p := a.profiles[r.workload]
		settling := headroomSamples
		if p.trusted(now) {
			settling = p.settle
		}
		if !r.encodingAt.IsZero() && now.Sub(r.encodingAt) < time.Duration(settling)*time.Second {
			return fmt.Sprintf("Waiting for workload settling (%ds)", settling)
		}
	}
	if a.candidate != nil && len(a.active) > 0 {
		if p := a.profiles[a.candidate.Workload]; p.trusted(now) && math.Max(a.cpu.average, a.latestCPU())+p.demand*1.25+5 > a.target {
			return "Waiting for learned workload CPU budget"
		}
	}
	need := a.observationSamples(now)
	if a.headroom < need {
		if a.candidate != nil && a.profiles[a.candidate.Workload].trusted(now) && len(a.active) > 0 && !a.metricsFailed {
			return fmt.Sprintf("Observing learned workload headroom (%d/%ds)", a.headroom, need)
		}
		return fmt.Sprintf("Observing CPU headroom (%d/%ds)", a.headroom, need)
	}
	return ""
}

// Only the exact next workload can use a recent completion's learned refill.
func (a *admissionState) observationSamples(now time.Time) int {
	if len(a.active) == 0 {
		return 5
	}
	if a.candidate == nil || a.metricsFailed {
		return headroomSamples
	}
	key := a.candidate.Workload
	p := a.profiles[key]
	if !p.trusted(now) {
		return headroomSamples
	}
	for _, until := range a.refills[key] {
		if now.Before(until) {
			return max(5, p.settle)
		}
	}
	return max(headroomSamples, p.settle)
}
func (a *admissionState) latestCPU() float64 {
	if len(a.cpu.readings) == 0 {
		return 100
	}
	return a.cpu.readings[len(a.cpu.readings)-1].value
}

type executionEvent struct {
	id           int64
	token, phase string
	exited, done bool
	pid          int
	succeeded    bool
}

func (a *admissionState) event(e executionEvent) {
	r := a.active[attemptKey{e.id, e.token}]
	if r == nil {
		return
	}
	if e.phase != "" && e.phase != r.phase || e.exited || e.done {
		a.headroom = 0
	}
	if e.pid > 0 {
		r.pid = e.pid
	}
	if e.succeeded {
		r.succeeded = true
	}
	if e.phase != "" {
		if e.phase == "video encoding" && r.phase != e.phase {
			r.encodingAt = a.cpu.last
			// A later encoding pass gets its own settling window. Keep peak demand
			// from the previous pass but require a fresh plateau for this one.
			r.observation.started = a.cpu.last
			r.observation.window = nil
			r.observation.settle = 0
			r.observation.plateau = 0
			r.previousAt = time.Time{}
		}
		r.phase = e.phase
	}
	if e.exited {
		r.phase = "post-processing"
	}
	if e.done {
		a.learnCompletion(r)
		delete(a.active, attemptKey{e.id, e.token})
	}
}

type admissionDeps struct {
	ticks      <-chan time.Time
	sample     func(context.Context) (float64, error)
	claim      func(context.Context, ClaimRequest) (*Job, error)
	peek       func(context.Context, ClaimRequest) (*ClaimCandidate, error)
	processCPU func(context.Context, int) (float64, error)
	cpuCount   int
	execute    func(context.Context, Job, executionHooks)
}

func (w *Worker) automatic(ctx context.Context) error {
	_, _ = cpu.PercentWithContext(ctx, 0, false)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	count, err := cpu.CountsWithContext(ctx, true)
	if err != nil || count < 1 {
		count = 0
	}
	return w.runAdmission(ctx, admissionDeps{cpuCount: count, processCPU: func(ctx context.Context, pid int) (float64, error) {
		p := process.Process{Pid: int32(pid)}
		v, err := p.TimesWithContext(ctx)
		if err != nil {
			return 0, err
		}
		return v.User + v.System, nil
	}, peek: func(ctx context.Context, r ClaimRequest) (*ClaimCandidate, error) {
		var candidate ClaimCandidate
		status, err := w.Client.JSON(ctx, "POST", "/api/claim/preview"+presetJobQuery, "", r, &candidate)
		if err != nil || status == http.StatusNoContent {
			return nil, err
		}
		return &candidate, nil
	}, ticks: ticker.C, sample: func(ctx context.Context) (float64, error) {
		v, err := cpu.PercentWithContext(ctx, 0, false)
		if err != nil {
			return 0, err
		}
		if len(v) != 1 {
			return 0, errors.New("missing total CPU sample")
		}
		return v[0], nil
	}, claim: func(ctx context.Context, r ClaimRequest) (*Job, error) {
		var j Job
		status, err := w.Client.JSON(ctx, "POST", "/api/claim"+presetJobQuery, "", r, &j)
		if err != nil || status == http.StatusNoContent {
			return nil, err
		}
		return &j, nil
	}, execute: w.executeObserved})
}

type claimResult struct {
	job *Job
	err error
}

func (w *Worker) runAdmission(ctx context.Context, d admissionDeps) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a := admissionState{cap: w.MaxJobs, target: w.CPUTarget, active: map[attemptKey]*attemptReservation{}}
	a.profiles = map[string]workloadProfile{}
	a.refills = map[string][]time.Time{}
	events := make(chan executionEvent, 64)
	previews := make(chan candidateResult, 1)
	var nextPreview time.Time
	peeking := false
	results := make(chan claimResult, 1)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	pending := false
	reason := "Waiting for CPU measurements"
	var now, nextPull, lastStatus time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case t, ok := <-d.ticks:
			if !ok {
				return nil
			}
			now = t
			v, err := d.sample(ctx)
			a.observe(now, v, err)
			if w.fuseBlocked() {
				a.headroom = 0
				for _, r := range a.active {
					r.observation.invalid = true
				}
			} else {
				a.sampleProcesses(ctx, now, d)
			}
		case result := <-previews:
			peeking = false
			a.candidate = result.candidate
			if result.candidate == nil {
				a.refills = map[string][]time.Time{}
			}
			nextPreview = now.Add(5 * time.Second)
			if result.err != nil {
				a.candidate = nil
				reason = "Queue preview failed; retrying"
			}
		case e := <-events:
			a.event(e)
		case r := <-results:
			pending = false
			a.candidate = nil
			nextPreview = now.Add(5 * time.Second)
			if r.err != nil || r.job == nil {
				a.refills = map[string][]time.Time{}
				reason = "No compatible queued job"
				if r.err != nil {
					reason = "Claim failed; retrying"
					log.Printf("claim: %v", r.err)
					w.networkHint(r.err)
				}
				nextPull = now.Add(5 * time.Second)
			} else {
				j := *r.job
				key := workloadKey(j.Payload)
				a.consumeRefill(key, now)
				a.active[attemptKey{j.ID, j.Token}] = &attemptReservation{phase: "preparation", workload: key}
				a.candidate = nil
				nextPreview = time.Time{}

				a.headroom = 0
				nextPull = time.Time{}
				wg.Add(1)
				go func() {
					defer wg.Done()
					send := func(e executionEvent) {
						select {
						case events <- e:
						case <-ctx.Done():
						}
					}
					d.execute(ctx, j, executionHooks{phase: func(p string) { send(executionEvent{id: j.ID, token: j.Token, phase: p}) }, exited: func() { send(executionEvent{id: j.ID, token: j.Token, exited: true}) }, started: func(pid int) { send(executionEvent{id: j.ID, token: j.Token, pid: pid}) }, succeeded: func() { send(executionEvent{id: j.ID, token: j.Token, succeeded: true}) }})
					send(executionEvent{id: j.ID, token: j.Token, done: true})
				}()
			}
		}
		if !pending && !peeking && d.peek != nil && !now.Before(nextPreview) {
			peeking = true
			a.candidate = nil
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, err := d.peek(ctx, ClaimRequest{Worker: w.Name, Encoders: w.Encoders})
				select {
				case previews <- candidateResult{c, err}:
				case <-ctx.Done():
				}
			}()
		}
		if !pending && !peeking {
			if w.fuseBlocked() {
				reason = "Filesystem I/O wait or write backpressure"
			} else if pause := a.reason(now); pause != "" {
				reason = pause
			} else if d.peek != nil && a.candidate == nil {
				reason = "No compatible job available or queue paused"
			} else if !now.Before(nextPull) {
				pending = true
				reason = "Claim in flight"
				expected := a.candidate
				wg.Add(1)
				go func() {
					defer wg.Done()
					j, err := d.claim(ctx, ClaimRequest{Worker: w.Name, Encoders: w.Encoders, Expected: expected})
					select {
					case results <- claimResult{j, err}:
					case <-ctx.Done():
					}
				}()
			}
		}
		w.admissionReport(&a, reason)
		if !now.IsZero() && (lastStatus.IsZero() || now.Sub(lastStatus) >= 10*time.Second) {
			log.Printf("admission: CPU=%.1f%% target=%.0f%% jobs=%d/%d %s", a.cpu.average, a.target, len(a.active), a.cap, reason)
			lastStatus = now
		}
	}
}

type candidateResult struct {
	candidate *ClaimCandidate
	err       error
}
