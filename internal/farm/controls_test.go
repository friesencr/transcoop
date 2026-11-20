package farm

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestExecutionControls(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a, b := enroll(t, s, "worker"), enroll(t, s, "worker")
	for _, name := range []string{"one", "two", "three"} {
		if _, err := s.Import(ctx, []json.RawMessage{fixture(t, t.TempDir(), name)}); err != nil {
			t.Fatal(err)
		}
	}
	set := func(id, action string) {
		t.Helper()
		if err := s.SetControl(ctx, id, action); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(id string, want bool) *Job {
		t.Helper()
		j, err := s.claimMatching(ctx, "same-name", []string{"x265_10bit"}, time.Minute, id)
		if err != nil || (j != nil) != want {
			t.Fatalf("claim: %v %v want=%v", j, err, want)
		}
		return j
	}
	first := claim(a.Device.ID, true)
	set(a.Device.ID, "pause")
	claim(a.Device.ID, false)
	if err := s.Heartbeat(ctx, first.ID, first.Token, time.Minute); err != nil {
		t.Fatal("pause canceled active job", err)
	}
	second := claim(b.Device.ID, true)
	set("", "pause")
	claim(b.Device.ID, false)
	set("", "play")
	claim(a.Device.ID, false) // Queue play does not override a worker pause.
	set(a.Device.ID, "stop")
	if err := s.Heartbeat(ctx, first.ID, first.Token, time.Minute); !errors.Is(err, ErrLease) {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, first.ID, first.Token, "", func(Job) error { t.Fatal("published stopped attempt"); return nil }); !errors.Is(err, ErrLease) {
		t.Fatal(err)
	}
	if err := s.Heartbeat(ctx, second.ID, second.Token, time.Minute); err != nil {
		t.Fatal("other worker stopped", err)
	}
	detail, err := s.Detail(ctx, first.ID)
	if err != nil || detail.Job.Status != "interrupted" || detail.Attempts[0].Status != "interrupted" {
		t.Fatal(detail, err)
	}
	set("", "stop")
	if err := s.Heartbeat(ctx, second.ID, second.Token, time.Minute); !errors.Is(err, ErrLease) {
		t.Fatal(err)
	}
	set(a.Device.ID, "play")
	claim(a.Device.ID, false) // Worker play does not override queue stop.
	set("", "play")
	claim(a.Device.ID, true)
	if err := s.SetControl(ctx, "missing", "pause"); err == nil {
		t.Fatal("accepted missing device")
	}
	if err := s.SetControl(ctx, "", "bad"); err == nil {
		t.Fatal("accepted bad action")
	}
}

func TestControlsPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "farm.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	worker := enroll(t, s, "worker")
	if err := s.SetControl(ctx, worker.Device.ID, "pause"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetControl(ctx, "", "stop"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	states, err := s.Controls(ctx)
	if err != nil || states[""] != "stopped" || states[worker.Device.ID] != "paused" {
		t.Fatal(states, err)
	}
}
