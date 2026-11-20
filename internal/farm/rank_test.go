package farm

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestRankedClaims(t *testing.T) {
	ctx := context.Background()
	previous := ClaimGrace
	ClaimGrace = 200 * time.Millisecond
	defer func() { ClaimGrace = previous }()

	s := newStore(t)
	fast, slow := enroll(t, s, "worker"), enroll(t, s, "worker")
	dir := t.TempDir()
	var queue []json.RawMessage
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		queue = append(queue, fixture(t, dir, name))
	}
	if _, err := s.Import(ctx, queue); err != nil {
		t.Fatal(err)
	}
	report := func(e Enrollment, active int, encoders ...string) {
		t.Helper()
		v := WorkerStatus{Name: e.Device.Name, DeviceID: e.Device.ID, Encoders: encoders, MaxJobs: 1, Scheduler: "fixed", Mode: "shared"}
		for i := 0; i < active; i++ {
			v.Active = append(v.Active, int64(100+i))
		}
		if err := s.PutWorker(ctx, "instance-"+e.Device.ID, v); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(e Enrollment) *Job {
		t.Helper()
		j, err := s.claimMatching(ctx, e.Device.Name, []string{"x265_10bit"}, time.Minute, e.Device.ID)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}

	// Without ranks, any worker gets the oldest job.
	report(fast, 0, "x265_10bit")
	if j := claim(slow); j == nil || j.ID != 1 {
		t.Fatal("unranked claim", j)
	}
	if err := s.SetRank(ctx, fast.Device.ID, RankFirst); err != nil {
		t.Fatal(err)
	}
	if s.SetRank(ctx, "nope", RankFirst) == nil {
		t.Fatal("rank set for unknown device")
	}
	// The idle preferred worker is online: the slow worker waits.
	if j := claim(slow); j != nil {
		t.Fatal("job not held for the preferred worker", j.ID)
	}
	if j := claim(fast); j == nil || j.ID != 2 {
		t.Fatal("preferred worker did not get the job", j)
	}
	// Busy preferred worker: no hold.
	report(fast, 1, "x265_10bit")
	if j := claim(slow); j == nil || j.ID != 3 {
		t.Fatal("held although the preferred worker is busy", j)
	}
	// Preferred worker cannot run the encoder: no hold.
	report(fast, 0, "x264")
	if j := claim(slow); j == nil || j.ID != 4 {
		t.Fatal("held for a worker without the encoder", j)
	}
	// Paused preferred worker: no hold.
	report(fast, 0, "x265_10bit")
	if err := s.SetControl(ctx, fast.Device.ID, "pause"); err != nil {
		t.Fatal(err)
	}
	if j := claim(slow); j == nil || j.ID != 5 {
		t.Fatal("held for a paused worker", j)
	}
	if err := s.SetControl(ctx, fast.Device.ID, "play"); err != nil {
		t.Fatal(err)
	}
	// A preferred worker that never claims only delays a job by ClaimGrace.
	if _, err := s.Import(ctx, []json.RawMessage{fixture(t, dir, "f")}); err != nil {
		t.Fatal(err)
	}
	if j := claim(slow); j != nil {
		t.Fatal("not held")
	}
	time.Sleep(ClaimGrace + 50*time.Millisecond)
	if j := claim(slow); j == nil || j.ID != 6 {
		t.Fatal("job stalled behind an unresponsive preferred worker", j)
	}
	ranks, _ := s.Ranks(ctx)
	if ranks[fast.Device.ID] != RankFirst || len(ranks) != 1 {
		t.Fatal(ranks)
	}
	if err := s.SetRank(ctx, fast.Device.ID, RankNormal); err != nil {
		t.Fatal(err)
	}
	if ranks, _ = s.Ranks(ctx); len(ranks) != 0 {
		t.Fatal("normal rank stored", ranks)
	}
}

// A retried or requeued job waits for the preferred worker again instead of
// reusing the wait it spent on an earlier attempt.
func TestHoldRestartsForEachAttempt(t *testing.T) {
	previous := ClaimGrace
	ClaimGrace = time.Minute
	defer func() { ClaimGrace = previous }()
	s := newStore(t)
	start := time.Now()
	j := Job{ID: 7, Attempts: 0}
	if !s.holdJob(j, start) {
		t.Fatal("first attempt not held")
	}
	if s.holdJob(j, start.Add(2*time.Minute)) {
		t.Fatal("first attempt held past the grace period")
	}
	j.Attempts = 1 // released or retried, then claimable again
	if !s.holdJob(j, start.Add(3*time.Minute)) {
		t.Fatal("next attempt inherited the expired wait")
	}
}
