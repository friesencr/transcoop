package farm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTelemetryFencingRetentionAndEvents(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	_, err := s.Import(ctx, []json.RawMessage{fixture(t, t.TempDir(), "one")})
	if err != nil {
		t.Fatal(err)
	}
	var old string
	for i := 1; i <= 4; i++ {
		j, e := s.Claim(ctx, "same name", []string{"x265_10bit"}, time.Minute)
		if e != nil {
			t.Fatal(e)
		}
		if old != "" {
			if e = s.PutTelemetry(ctx, j.ID, old, JobTelemetry{Tail: "old attempt"}); !errors.Is(e, ErrLease) {
				t.Fatal("old attempt accepted", e)
			}
		}
		if e = s.PutTelemetry(ctx, j.ID, j.Token, JobTelemetry{Phase: "encoding", Tail: "diagnostic " + j.Token}); e != nil {
			t.Fatal(e)
		}
		if e = s.Finish(ctx, j.ID, j.Token, "failure "+j.Token, nil); e != nil {
			t.Fatal(e)
		}
		if e = s.PutTelemetry(ctx, j.ID, j.Token, JobTelemetry{}); !errors.Is(e, ErrLease) {
			t.Fatal("finished lease accepted")
		}
		old = j.Token
		if i < 4 {
			if e = s.Retry(ctx, j.ID); e != nil {
				t.Fatal(e)
			}
		}
	}
	d, err := s.Detail(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Attempts) != 4 || d.Attempts[3].Telemetry != nil {
		t.Fatal("retention", d.Attempts)
	}
	for _, a := range d.Attempts[:3] {
		if a.Telemetry == nil || strings.Contains(a.Telemetry.Tail, old) || strings.Contains(a.Error, old) {
			t.Fatal("missing/redaction", a)
		}
	}
	if len(d.Events) != 12 {
		t.Fatalf("events: %+v", d.Events)
	}

}
func TestConcurrentWorkerPresence(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for _, instance := range []string{"instance-111111111", "instance-222222222"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if err := s.PutWorker(ctx, id, WorkerStatus{Name: "duplicate"}); err != nil {
					t.Error(err)
				}
			}
		}(instance)
	}
	wg.Wait()
	v, err := s.Workers(ctx)
	if err != nil || len(v) != 2 {
		t.Fatal(v, err)
	}
	for _, x := range []struct {
		age  int64
		want string
	}{{0, "online"}, {15, "stale"}, {30, "offline"}} {
		w := v[0]
		w.Updated = time.Now().Unix() - x.age
		if w.Presence() != x.want {
			t.Fatal(w.Presence())
		}
	}
}
func TestTailsAndUnknownMetrics(t *testing.T) {
	a := &attemptReport{secrets: []string{"authentication-secret", "attempt-secret"}}
	_, _ = a.Write([]byte(strings.Repeat("x", TailLimit+100)))
	_, _ = a.Write([]byte("authentication-secret attempt-secret"))
	v := a.snapshot()
	if len(v.Tail) > TailLimit || !v.Truncated || strings.Contains(v.Tail, "secret") {
		t.Fatal("tail or redaction")
	}
	p := &progressWriter{log: io.Discard, phase: a.phase, observed: a.observe}
	for _, record := range []string{`{"State":"WORKING","Working":{"PassID":-1,"Pass":1,"PassCount":3,"Progress":0.2}}`, `{"State":"WORKING","Working":{"PassID":1,"Pass":2,"PassCount":3,"Progress":0.3}}`, `{"State":"WORKING","Working":{"PassID":2,"Pass":3,"PassCount":3,"Progress":0.4}}`} {
		_, _ = p.Write([]byte("Progress: " + record))
		v = a.snapshot()
		if v.Fraction == nil || v.ETA != nil || v.FPS != nil {
			t.Fatal("invented metric", v)
		}
	}
	for _, phase := range []string{"MUXING", "WORKDONE"} {
		_, _ = p.Write([]byte(`Progress: {"State":"` + phase + `"}`))
		if a.snapshot().Fraction != nil {
			t.Fatal("previous-pass progress leaked")
		}
	}
}
func TestTelemetryMigrationRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "farm.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Import(ctx, []json.RawMessage{fixture(t, t.TempDir(), "old")})
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.Claim(ctx, "old-worker", []string{"x265_10bit"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`DROP TRIGGER job_import_event; DROP TRIGGER job_state_event; DROP TABLE attempts; DROP TABLE events; DROP TABLE worker_status;`)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d, err := s.Detail(ctx, j.ID)
	if err != nil || len(d.Attempts) != 1 || d.Attempts[0].Telemetry != nil {
		t.Fatal(d, err)
	}
	if err = s.PutTelemetry(ctx, j.ID, j.Token, JobTelemetry{Tail: "survives restart"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d, err = s.Detail(ctx, j.ID)
	if err != nil || d.Attempts[0].Telemetry.Tail != "survives restart" {
		t.Fatal(d, err)
	}
}
func TestReportingFinalAndFailureIsolation(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "delivery", true: "timeout"}[fail], func(t *testing.T) {
			t.Setenv("TRANSCOOP_FARM_TEST_ENCODER", "1")
			ctx := context.Background()
			s := newStore(t)
			dir := t.TempDir()
			q := fixture(t, dir, "report")
			if _, err := s.Import(ctx, []json.RawMessage{q}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "report.source.mkv"), []byte("input"), 0600); err != nil {
				t.Fatal(err)
			}
			j, err := s.Claim(ctx, "w", []string{"x265_10bit"}, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			j.Staging = StagePath(*j)
			api := testServer(s, "farm-secret").Handler()
			var reports atomic.Int32
			var finalBeforeCompletion atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/telemetry") {
					reports.Add(1)
					if fail {
						select {
						case <-r.Context().Done():
						case <-time.After(3 * time.Second):
						}
						return
					}
				}
				if strings.HasSuffix(r.URL.Path, "/complete") {
					finalBeforeCompletion.Store(reports.Load() > 0)
				}
				api.ServeHTTP(rw, r)
			}))
			defer server.Close()
			exe, _ := os.Executable()
			w := &Worker{Client: NewClient(server.URL, "farm-secret"), Name: "w", Mode: "shared", HandBrake: exe, WorkDir: dir, MaxJobs: 1, reporter: &workerReporter{status: WorkerStatus{Instance: "test-instance-1111"}, active: map[attemptKey]bool{}}}
			start := time.Now()
			w.execute(ctx, *j)
			if time.Since(start) > 6*time.Second {
				t.Fatal("unbounded final report")
			}
			d, err := s.Detail(ctx, j.ID)
			if err != nil || d.Job.Status != "succeeded" || !finalBeforeCompletion.Load() {
				t.Fatal("report interfered with completion", d, err)
			}
			if !fail && (d.Attempts[0].Telemetry == nil || !strings.Contains(d.Attempts[0].Telemetry.Tail, "stderr fixture")) {
				t.Fatal("final output missing")
			}
		})
	}
}
func TestTelemetryExpiredLeaseAndRetryRace(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	s.Import(ctx, []json.RawMessage{fixture(t, t.TempDir(), "expired")})
	j, err := s.Claim(ctx, "w", []string{"x265_10bit"}, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PutTelemetry(ctx, j.ID, j.Token, JobTelemetry{}); !errors.Is(err, ErrLease) {
		t.Fatal("expired accepted")
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Retry(ctx, j.ID) == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal("retry race", successes.Load())
	}
	d, err := s.Detail(ctx, j.ID)
	if err != nil || d.Attempts[0].Status != "interrupted" {
		t.Fatal(d, err)
	}
}

func TestMissingProgressInNewPassClearsMetrics(t *testing.T) {
	a := &attemptReport{}
	p := &progressWriter{log: io.Discard, observed: a.observe}
	p.Write([]byte(`Progress: {"State":"WORKING","Working":{"PassID":2,"Pass":1,"PassCount":2,"Progress":0.9,"ETASeconds":10,"Rate":5}}`))
	p.Write([]byte(`Progress: {"State":"WORKING","Working":{"PassID":2,"Pass":2,"PassCount":2}}`))
	v := a.snapshot()
	if v.Pass != 2 || v.Fraction != nil || v.ETA != nil || v.FPS != nil {
		t.Fatalf("previous pass metrics leaked: %+v", v)
	}
}
func TestIdleReportsWithoutCapabilityFallback(t *testing.T) {
	var reports atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/status") {
			t.Error("unexpected request", r.URL.Path)
		}
		reports.Add(1)
		w.WriteHeader(204)
	}))
	defer srv.Close()
	worker := &Worker{Client: NewClient(srv.URL, "test"), Name: "idle", Mode: "shared", Scheduler: "fixed", MaxJobs: 1}
	stop := worker.startReporting(context.Background())
	defer stop()
	deadline := time.Now().Add(time.Second)
	for reports.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if reports.Load() == 0 {
		t.Fatal("idle worker did not report")
	}
}
func TestSlowTelemetryDoesNotStopHeartbeat(t *testing.T) {
	t.Setenv("TRANSCOOP_FARM_TEST_ENCODER", "1")
	s := newStore(t)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 17*time.Second)
	defer cancel()
	if _, err := s.Import(ctx, []json.RawMessage{fixture(t, dir, "slow")}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "slow.source.mkv"), []byte("wait"), 0600); err != nil {
		t.Fatal(err)
	}
	j, err := s.Claim(ctx, "worker", []string{"x265_10bit"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	j.Staging = StagePath(*j)
	api := testServer(s, "test").Handler()
	var heartbeats, reports atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/telemetry") {
			reports.Add(1)
			io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
			return
		}
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			heartbeats.Add(1)
		}
		api.ServeHTTP(w, r)
	}))
	defer srv.Close()
	exe, _ := os.Executable()
	worker := &Worker{Client: NewClient(srv.URL, "test"), Name: "worker", Mode: "shared", HandBrake: exe, WorkDir: dir, reporter: &workerReporter{status: WorkerStatus{Instance: "test-instance-1111"}, active: map[attemptKey]bool{}}}
	worker.execute(ctx, *j)
	if reports.Load() < 3 || heartbeats.Load() < 2 {
		t.Fatalf("telemetry blocked progress/heartbeat: reports=%d heartbeats=%d", reports.Load(), heartbeats.Load())
	}
}
