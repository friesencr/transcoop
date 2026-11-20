package farm

import (
	"context"
	"encoding/json"
	"fmt"
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

// The test executable doubles as a portable fake HandBrake process.
func TestMain(m *testing.M) {
	if os.Getenv("TRANSCOOP_FARM_TEST_ENCODER") == "1" {
		if len(os.Args) != 4 || os.Args[1] != "--queue-import-file" || os.Args[3] != "--json" {
			os.Exit(2)
		}
		data, err := os.ReadFile(os.Args[2])
		if err != nil {
			os.Exit(3)
		}
		var queue []json.RawMessage
		if json.Unmarshal(data, &queue) != nil || len(queue) != 1 {
			os.Exit(4)
		}
		j, err := parseJob(queue[0])
		if err != nil {
			os.Exit(5)
		}
		input, err := os.ReadFile(j.Source)
		if err != nil {
			os.Exit(6)
		}
		if string(input) == "wait" {
			fmt.Println("waiting for cancellation")
			time.Sleep(time.Hour)
		}
		fmt.Println(`Progress: {"State":"SCANNING"}`)
		fmt.Fprintln(os.Stderr, "stderr fixture")
		fmt.Println(`Progress: {"State":"WORKING","Working":{"PassID":-1}}`)
		fmt.Println(`Progress: {"State":"WORKING","Working":{"PassID":1}}`)
		fmt.Println(`Progress: {"State":"WORKING","Working":{"PassID":2}}`)
		fmt.Println(`Progress: {"State":"MUXING"}`)
		fmt.Println(`Progress: {"State":"WORKDONE"}`)
		if string(input) == "fail" {
			fmt.Fprintln(os.Stderr, "simulated encoder failure")
			os.Exit(7)
		}
		if os.WriteFile(j.Destination, append([]byte("encoded:"), input...), 0600) != nil {
			os.Exit(8)
		}
		fmt.Println("encoded fixture")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fixture(t *testing.T, dir, name string) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(map[string]any{"Job": map[string]any{
		"Source":      map[string]any{"Path": filepath.Join(dir, name+".source.mkv")},
		"Destination": map[string]any{"File": filepath.Join(dir, name+".mkv")},
		"Video":       map[string]any{"Encoder": "x265_10bit", "Quality": 20},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "farm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func TestImportAtomicAndPersistent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "farm.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	first := fixture(t, dir, "first")
	if n, err := s.Import(ctx, []json.RawMessage{first}); err != nil || n != 1 {
		t.Fatalf("import: %d %v", n, err)
	}
	if n, err := s.Import(ctx, []json.RawMessage{first}); err != nil || n != 0 {
		t.Fatalf("duplicate: %d %v", n, err)
	}
	conflict := json.RawMessage(strings.Replace(string(first), `"Quality":20`, `"Quality":21`, 1))
	if _, err := s.Import(ctx, []json.RawMessage{fixture(t, dir, "second"), conflict}); err == nil {
		t.Fatal("expected destination conflict")
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	jobs, err := s.List(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("atomic persistence: %v %v", jobs, err)
	}
}
func TestConcurrentClaimsAndLeaseFencing(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	dir := t.TempDir()
	queue := []json.RawMessage{}
	for i := 0; i < 20; i++ {
		queue = append(queue, fixture(t, dir, fmt.Sprint(i)))
	}
	if _, err := s.Import(ctx, queue); err != nil {
		t.Fatal(err)
	}
	if j, err := s.Claim(ctx, "wrong-encoder", []string{"nvenc_h265"}, time.Minute); err != nil || j != nil {
		t.Fatalf("encoder match: %v %v", j, err)
	}
	var wg sync.WaitGroup
	claims := make(chan *Job, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, err := s.Claim(ctx, "worker", []string{"x265_10bit"}, time.Minute)
			if err != nil {
				t.Error(err)
			}
			if j != nil {
				claims <- j
			}
		}()
	}
	wg.Wait()
	close(claims)
	seen := map[int64]bool{}
	var old *Job
	for j := range claims {
		if seen[j.ID] {
			t.Fatal("duplicate claim")
		}
		seen[j.ID] = true
		old = j
	}
	if len(seen) != 20 {
		t.Fatalf("claimed %d jobs", len(seen))
	}
	if _, err := s.db.Exec(`UPDATE jobs SET lease_until=0 WHERE id=?`, old.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Heartbeat(ctx, old.ID, old.Token, time.Minute); err != ErrLease {
		t.Fatalf("expired heartbeat: %v", err)
	}
	jobs, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.Token != "" || j.Payload != nil {
			t.Fatal("list exposes attempt credentials/payload")
		}
		if j.ID == old.ID && j.Status != "interrupted" {
			t.Fatalf("expired state: %s", j.Status)
		}
	}
	if j, err := s.Claim(ctx, "next", []string{"x265_10bit"}, time.Minute); err != nil || j != nil {
		t.Fatal("expired job must not be retried automatically")
	}
	if err := s.Retry(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	next, err := s.Claim(ctx, "next", []string{"x265_10bit"}, time.Minute)
	if err != nil || next == nil {
		t.Fatalf("retry claim: %v %v", next, err)
	}
	if next.Token == old.Token || next.Attempts != 2 {
		t.Fatal("retry must create a new attempt")
	}
	called := false
	if err := s.Finish(ctx, old.ID, old.Token, "", func(Job) error { called = true; return nil }); err != ErrLease || called {
		t.Fatalf("stale completion: %v publish=%v", err, called)
	}
	if err := s.Retry(ctx, next.ID); err == nil {
		t.Fatal("cannot retry a running job")
	}
}
func TestPublishNeverOverwrites(t *testing.T) {
	j := Job{ID: 1, Token: "attempt", Destination: filepath.Join(t.TempDir(), "final.mkv")}
	if err := os.WriteFile(j.Destination, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StagePath(j), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publish(j); err == nil {
		t.Fatal("publish overwrote existing destination")
	}
	data, _ := os.ReadFile(j.Destination)
	if string(data) != "original" {
		t.Fatal("destination changed")
	}
}
func TestMapPath(t *testing.T) {
	maps := []Mapping{{From: "/media", To: filepath.Join(t.TempDir(), "broad")}, {From: "/media/movies", To: filepath.Join(t.TempDir(), "specific")}}
	if got := MapPath("/media/movies/a.mkv", maps); got != filepath.Join(maps[1].To, "a.mkv") {
		t.Fatal(got)
	}
	if got := MapPath("/media-old/a.mkv", maps); got != "/media-old/a.mkv" {
		t.Fatal(got)
	}
	windows := []Mapping{{From: `Z:\Movies`, To: t.TempDir()}}
	if got := MapPath(`Z:\Movies\a.mkv`, windows); got != filepath.Join(windows[0].To, "a.mkv") {
		t.Fatal(got)
	}
}
func TestWorkerBothStorageModes(t *testing.T) {
	t.Setenv("TRANSCOOP_FARM_TEST_ENCODER", "1")
	for _, mode := range []string{"shared", "transfer"} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fail=%v", mode, fail), func(t *testing.T) {
				ctx := context.Background()
				s := newStore(t)
				server := httptest.NewServer(testServer(s, "secret").Handler())
				defer server.Close()
				c := NewClient(server.URL, "secret")
				raw := fixture(t, t.TempDir(), "test")
				parsed, _ := parseJob(raw)
				input := "source media"
				if fail {
					input = "fail"
				}
				if err := os.WriteFile(parsed.Source, []byte(input), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := c.JSON(ctx, "POST", "/api/jobs", "", []json.RawMessage{raw}, nil); err != nil {
					t.Fatal(err)
				}
				paired := enroll(t, s, "worker")
				workerClient := NewClient(server.URL, paired.Token)
				var j Job
				if _, err := workerClient.JSON(ctx, "POST", "/api/claim", "", ClaimRequest{Worker: "test-worker", Encoders: []string{"x265_10bit"}}, &j); err != nil {
					t.Fatal(err)
				}
				exe, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				worker := Worker{Client: workerClient, Name: "test-worker", Mode: mode, HandBrake: exe, WorkDir: t.TempDir()}
				exited := false
				var phases []string
				worker.executeObserved(ctx, j, executionHooks{phase: func(p string) {
					phases = append(phases, p)
					if (p == "output transfer" || p == "completion reporting") && !exited {
						t.Error("transfer/report before reaped")
					}
				}, exited: func() { exited = true }})
				if !exited {
					t.Fatal("missing process exit")
				}
				jobs, err := s.List(ctx)
				if err != nil {
					t.Fatal(err)
				}
				expected := "succeeded"
				if fail {
					expected = "failed"
				}
				if jobs[0].Status != expected {
					t.Fatalf("job: %+v", jobs[0])
				}
				output, err := os.ReadFile(j.Destination)
				if fail {
					if !os.IsNotExist(err) {
						t.Fatalf("failed encode published output: %v", err)
					}
				} else if err != nil || string(output) != "encoded:"+input {
					t.Fatalf("output: %q %v", output, err)
				}
				logs, _ := filepath.Glob(filepath.Join(worker.WorkDir, "*", "handbrake.log"))
				if len(logs) != 1 {
					t.Fatal("missing per-attempt log")
				}
				data, err := os.ReadFile(logs[0])
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(data), "stderr fixture") || !strings.Contains(string(data), `"PassID":1`) {
					t.Fatal("lost subprocess log")
				}
				if !strings.Contains(strings.Join(phases, ","), "subtitle search,video analysis,video encoding,muxing,awaiting process exit") {
					t.Fatal(phases)
				}
			})
		}
	}
}
func TestAuthorizationAndExpiredTransfer(t *testing.T) {
	s := newStore(t)
	server := httptest.NewServer(testServer(s, "secret").Handler())
	defer server.Close()
	res, err := http.Get(server.URL + "/api/jobs")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal(res.Status)
	}
	ctx := context.Background()
	raw := fixture(t, t.TempDir(), "test")
	if _, err := s.Import(ctx, []json.RawMessage{raw}); err != nil {
		t.Fatal(err)
	}
	j, err := s.Claim(ctx, "w", []string{"x265_10bit"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE jobs SET lease_until=0 WHERE id=?`, j.ID); err != nil {
		t.Fatal(err)
	}
	c := NewClient(server.URL, "secret")
	for _, method := range []string{"GET", "PUT"} {
		suffix := "source"
		if method == "PUT" {
			suffix = "output"
		}
		if res, err := c.Request(ctx, method, fmt.Sprintf("/api/jobs/%d/%s", j.ID, suffix), j.Token, strings.NewReader("stale")); err == nil {
			res.Body.Close()
			t.Fatal("stale attempt can transfer")
		}
	}
}

func TestWorkerCancellationAndLeaseLossReapProcess(t *testing.T) {
	t.Setenv("TRANSCOOP_FARM_TEST_ENCODER", "1")
	for _, leaseLoss := range []bool{false, true} {
		t.Run(fmt.Sprint("leaseLoss=", leaseLoss), func(t *testing.T) {
			s := newStore(t)
			handler := testServer(s, "secret").Handler()
			var beats atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/heartbeat") && beats.Add(1) > 1 && leaseLoss {
					http.Error(w, "lease lost", http.StatusConflict)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			raw := fixture(t, t.TempDir(), "wait")
			j0, _ := parseJob(raw)
			if err := os.WriteFile(j0.Source, []byte("wait"), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := s.Import(context.Background(), []json.RawMessage{raw})
			if err != nil {
				t.Fatal(err)
			}
			j, err := s.Claim(context.Background(), "w", []string{"x265_10bit"}, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			j.Staging = StagePath(*j)
			exe, _ := os.Executable()
			w := Worker{Client: NewClient(server.URL, "secret"), Name: "w", Mode: "shared", HandBrake: exe, WorkDir: t.TempDir()}
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			exited := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				w.executeObserved(ctx, *j, executionHooks{exited: func() { close(exited) }})
			}()
			logfile := filepath.Join(w.WorkDir, fmt.Sprintf("job-%d-%s", j.ID, j.Token), "handbrake.log")
			deadline := time.Now().Add(5 * time.Second)
			for {
				data, _ := os.ReadFile(logfile)
				if strings.Contains(string(data), "waiting for cancellation") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("process did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			select {
			case <-exited:
				t.Fatal("released live process")
			default:
			}
			if !leaseLoss {
				cancel()
			}
			select {
			case <-done:
			case <-ctx.Done():
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("process not reaped")
				}
			}
			select {
			case <-exited:
			default:
				t.Fatal("exit hook missing")
			}
			if _, err := os.Stat(j.Destination); !os.IsNotExist(err) {
				t.Fatal("canceled process published output")
			}
			// Worker shutdown hands the job back; a lost lease is a failure.
			want := "pending"
			if leaseLoss {
				want = "failed"
			}
			jobs, err := s.List(context.Background())
			if err != nil || jobs[0].Status != want {
				t.Fatalf("want %s: %v %v", want, jobs, err)
			}
		})
	}
}

func TestAutomaticBothStorageModes(t *testing.T) {
	t.Setenv("TRANSCOOP_FARM_TEST_ENCODER", "1")
	for _, mode := range []string{"shared", "transfer"} {
		t.Run(mode, func(t *testing.T) {
			s := newStore(t)
			server := httptest.NewServer(testServer(s, "secret").Handler())
			defer server.Close()
			dir := t.TempDir()
			var queue []json.RawMessage
			for i := 0; i < 3; i++ {
				raw := fixture(t, dir, fmt.Sprint(i))
				j, _ := parseJob(raw)
				if err := os.WriteFile(j.Source, []byte("media"), 0600); err != nil {
					t.Fatal(err)
				}
				queue = append(queue, raw)
			}
			if _, err := s.Import(context.Background(), queue); err != nil {
				t.Fatal(err)
			}
			exe, _ := os.Executable()
			paired := enroll(t, s, "worker")
			w := Worker{Client: NewClient(server.URL, paired.Token), Name: "w", Mode: mode, HandBrake: exe, WorkDir: t.TempDir(), MaxJobs: 3, CPUTarget: 85, Encoders: []string{"x265_10bit"}}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ticks := make(chan time.Time)
			go func() {
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				now := time.Unix(100, 0)
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						now = now.Add(time.Second)
						select {
						case ticks <- now:
						case <-ctx.Done():
							return
						}
					}
				}
			}()
			var completed atomic.Int32
			err := w.runAdmission(ctx, admissionDeps{ticks: ticks, sample: func(context.Context) (float64, error) { return 0, nil }, claim: func(ctx context.Context, r ClaimRequest) (*Job, error) {
				var j Job
				status, err := w.Client.JSON(ctx, "POST", "/api/claim", "", r, &j)
				if err != nil || status == http.StatusNoContent {
					return nil, err
				}
				return &j, nil
			}, execute: func(ctx context.Context, j Job, h executionHooks) {
				w.executeObserved(ctx, j, h)
				if completed.Add(1) == 3 {
					cancel()
				}
			}})
			if err != nil || completed.Load() != 3 {
				t.Fatalf("completed %d: %v", completed.Load(), err)
			}
			jobs, err := s.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, j := range jobs {
				if j.Status != "succeeded" {
					t.Fatal(j)
				}
			}
		})
	}
}

func testServer(s *Store, token string) *Server {
	_, err := s.db.Exec(`INSERT INTO devices(id,name,role,hash,created) VALUES(?,'test operator','operator',?,?) ON CONFLICT(hash) DO NOTHING`, randomToken(), credentialHash(token), time.Now().Unix())
	if err != nil {
		panic(err)
	}
	return &Server{Store: s}
}
