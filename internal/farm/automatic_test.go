package farm

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"
)

func TestAutomaticObservationAndPhases(t *testing.T) {
	a := admissionState{cap: 4, target: 85, active: map[attemptKey]*attemptReservation{}}
	now := time.Unix(100, 0)
	for i := 1; i <= 5; i++ {
		now = now.Add(time.Second)
		a.observe(now, 20, nil)
		if (a.reason(now) == "") != (i == 5) {
			t.Fatalf("idle sample %d: %s", i, a.reason(now))
		}
	}
	key := attemptKey{1, "a"}
	a.active[key] = &attemptReservation{phase: "preparation"}
	a.headroom = 0
	for _, phase := range []string{"source transfer", "preparation", "subtitle search", "video analysis", "muxing", "output transfer", "unknown", "post-processing"} {
		a.active[key].phase = phase
		for i := 0; i < 25; i++ {
			now = now.Add(time.Second)
			a.observe(now, 20, nil)
		}
		if a.headroom != 0 || a.reason(now) == "" {
			t.Fatal("admitted during", phase)
		}
	}
	a.event(executionEvent{id: 1, token: "a", phase: "video encoding"})
	for i := 1; i <= 20; i++ {
		now = now.Add(time.Second)
		a.observe(now, 20, nil)
		if (a.reason(now) == "") != (i == 20) {
			t.Fatalf("encoding sample %d: %s", i, a.reason(now))
		}
	}
	a.event(executionEvent{id: 1, token: "a", exited: true})
	if a.headroom != 0 || len(a.active) != 1 {
		t.Fatal("released cap before upload/completion")
	}
	a.event(executionEvent{id: 1, token: "a", done: true})
	if len(a.active) != 0 {
		t.Fatal("completed attempt retained")
	}
}
func TestAutomaticCPUFailuresRecoveryAndCap(t *testing.T) {
	for _, value := range []float64{85, 100, math.NaN(), math.Inf(1), -1} {
		a := admissionState{cap: 1, target: 85, active: map[attemptKey]*attemptReservation{}}
		now := time.Unix(100, 0)
		for i := 0; i < 5; i++ {
			now = now.Add(time.Second)
			a.observe(now, 10, nil)
		}
		now = now.Add(time.Second)
		a.observe(now, value, nil)
		if a.reason(now) == "" || a.headroom != 0 {
			t.Fatal("admitted bad/high sample", value)
		}
		for i := 0; i < 15; i++ {
			now = now.Add(time.Second)
			a.observe(now, 10, nil)
		}
		if a.reason(now) != "" {
			t.Fatal("did not recover", a.reason(now))
		}
		now = now.Add(3 * time.Second)
		a.observe(now, 10, nil)
		if a.reason(now) == "" {
			t.Fatal("gap retained observation")
		}
		a.observe(now.Add(time.Second), 10, errors.New("sampler"))
		if a.reason(now.Add(time.Second)) == "" {
			t.Fatal("sampler failure admitted")
		}
		for i := 0; i < 20; i++ {
			now = now.Add(time.Second)
			a.observe(now, 10, nil)
		}
		a.active[attemptKey{1, "a"}] = &attemptReservation{phase: "video encoding"}
		if a.reason(now) != "Concurrency limit reached" {
			t.Fatal(a.reason(now))
		}
	}
}
func TestAutomaticSerialClaimsCapAndInFlightSpike(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	claims := make(chan struct{}, 4)
	release := make(chan struct{})
	executing := make(chan struct{}, 4)
	var load atomic.Int32
	load.Store(10)
	w := Worker{Name: "worker", MaxJobs: 1, CPUTarget: 85, Encoders: []string{"x264"}}
	done := make(chan error, 1)
	go func() {
		done <- w.runAdmission(ctx, admissionDeps{ticks: ticks, sample: func(context.Context) (float64, error) { return float64(load.Load()), nil }, claim: func(ctx context.Context, _ ClaimRequest) (*Job, error) {
			claims <- struct{}{}
			select {
			case <-release:
				return &Job{ID: 1, Token: "a"}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}, execute: func(ctx context.Context, j Job, h executionHooks) {
			h.setPhase("video encoding")
			executing <- struct{}{}
			<-ctx.Done()
			h.processExited()
		}})
	}()
	now := time.Unix(100, 0)
	tick := func() {
		now = now.Add(time.Second)
		select {
		case ticks <- now:
		case <-time.After(time.Second):
			t.Fatal("scheduler blocked")
		}
	}
	for i := 0; i < 5; i++ {
		tick()
	}
	select {
	case <-claims:
	case <-time.After(time.Second):
		t.Fatal("no first claim")
	}
	for i := 0; i < 25; i++ {
		tick()
	}
	select {
	case <-claims:
		t.Fatal("concurrent claim")
	default:
	}
	load.Store(100)
	tick()
	close(release)
	select {
	case <-executing:
	case <-time.After(time.Second):
		t.Fatal("lost in-flight claim")
	}
	load.Store(10)
	for i := 0; i < 35; i++ {
		tick()
	}
	select {
	case <-claims:
		t.Fatal("exceeded cap")
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown failed")
	}
}

func TestAutomaticEmptyQueueAndClaimFailureBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	claims := make(chan time.Time, 32)
	var sampled atomic.Int64
	w := Worker{Name: "worker", MaxJobs: 4, CPUTarget: 85, Encoders: []string{"x264"}, reporter: &workerReporter{active: map[attemptKey]bool{}}}
	done := make(chan error, 1)
	count := 0
	go func() {
		done <- w.runAdmission(ctx, admissionDeps{ticks: ticks, sample: func(context.Context) (float64, error) { return 10, nil }, claim: func(context.Context, ClaimRequest) (*Job, error) {
			count++
			claims <- time.Unix(sampled.Load(), 0)
			if count == 1 {
				return nil, errors.New("temporary outage")
			}
			return nil, nil
		}, execute: func(context.Context, Job, executionHooks) { t.Error("executed empty claim") }})
	}()
	now := time.Unix(100, 0)
	for i := 0; i < 25; i++ {
		now = now.Add(time.Second)
		sampled.Store(now.Unix())
		ticks <- now
		// Give asynchronous claim results time to enter the event loop; assertions
		// concern virtual sample times rather than this scheduling delay.
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	close(claims)
	var previous time.Time
	n := 0
	for at := range claims {
		if !previous.IsZero() && at.Sub(previous) < 5*time.Second {
			t.Fatal("retried without backoff")
		}
		previous = at
		n++
	}
	if n < 2 {
		t.Fatal("did not recover and retry", n)
	}
}

func TestAutomaticLearnedRefill(t *testing.T) {
	now := time.Unix(100, 0)
	a := admissionState{cap: 4, target: 85, active: map[attemptKey]*attemptReservation{{1, "a"}: {phase: "video encoding"}}, profiles: map[string]workloadProfile{}, refills: map[string][]time.Time{}, candidate: &ClaimCandidate{Workload: "dvd"}}
	a.profiles["dvd"] = workloadProfile{successes: 2, demand: 20, settle: 12, updated: now}
	a.refills["dvd"] = []time.Time{now.Add(time.Minute)}
	for i := 1; i <= 12; i++ {
		now = now.Add(time.Second)
		a.observe(now, 30, nil)
		if (a.reason(now) == "") != (i == 12) {
			t.Fatal(i, a.reason(now))
		}
	}
	a.candidate.Workload = "hd"
	if a.observationSamples(now) != 20 || a.reason(now) == "" {
		t.Fatal("accelerated an unfamiliar resolution")
	}
	a.candidate.Workload = "dvd"
	a.profiles["dvd"] = workloadProfile{successes: 1, demand: 20, settle: 12, updated: now}
	if a.observationSamples(now) != 20 {
		t.Fatal("one run earned confidence")
	}
	a.profiles["dvd"] = workloadProfile{successes: 2, demand: 50, settle: 12, updated: now}
	if a.reason(now) != "Waiting for learned workload CPU budget" {
		t.Fatal(a.reason(now))
	}
	a.profiles["dvd"] = workloadProfile{successes: 2, demand: 20, settle: 12, updated: now}
	a.metricsFailed = true
	if a.observationSamples(now) != 20 {
		t.Fatal("accelerated without process measurements")
	}
	a.metricsFailed = false
	if a.observationSamples(now.Add(profileLifetime)) != 20 {
		t.Fatal("used expired profile")
	}
	a.consumeRefill("dvd", now)
	if a.observationSamples(now) != 20 {
		t.Fatal("reused freed capacity")
	}
	a.refills["dvd"] = []time.Time{now.Add(time.Minute)}
	a.profiles["dvd"] = workloadProfile{successes: 2, demand: 20, settle: 30, updated: now}
	for i := 0; i < 30; i++ {
		if i%5 == 0 {
			a.candidate = nil
		} else {
			a.candidate = &ClaimCandidate{Workload: "dvd"}
		}
		now = now.Add(time.Second)
		a.observe(now, 30, nil)
	}
	a.candidate = &ClaimCandidate{Workload: "dvd"}
	if a.reason(now) != "" {
		t.Fatal("long learned delay never completed", a.reason(now))
	}
	a.event(executionEvent{id: 1, token: "a", phase: "source transfer"})
	if a.reason(now) != "Waiting for video encoding" {
		t.Fatal(a.reason(now))
	}
}
