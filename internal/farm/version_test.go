package farm

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseHandBrakeVersionIgnoresDiagnostics(t *testing.T) {
	out := `[18:19:04] Compile-time hardening features are enabled
Cannot load libnvidia-encode.so.1
[18:19:04] vcn: not available on this system
HandBrake has exited.
HandBrake 1.11.2
`
	if got := ParseHandBrakeVersion(out); got != "HandBrake 1.11.2" {
		t.Fatalf("got %q", got)
	}
	if got := ParseHandBrakeVersion("HandBrake has exited.\n"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestVersionWarnings(t *testing.T) {
	now := time.Now().Unix()
	workers := []WorkerStatus{
		{Name: "a", Version: "v1", HandBrake: "HandBrake 1.11.2", Updated: now},
		{Name: "b", Version: "v1", HandBrake: "HandBrake 1.10.0", Updated: now},
		{Name: "c", Version: "v0", HandBrake: "HandBrake 1.9.0", Updated: now - 3600}, // offline
	}
	got := VersionWarnings(workers, "v1")
	if len(got) != 1 || !strings.Contains(got[0], "1.10.0 (b)") || !strings.Contains(got[0], "1.11.2 (a)") || strings.Contains(got[0], "1.9.0") {
		t.Fatalf("unexpected warnings: %q", got)
	}
	workers[1].HandBrake = workers[0].HandBrake
	workers[1].Version = "v2"
	got = VersionWarnings(workers, "v1")
	if len(got) != 1 || !strings.Contains(got[0], "Worker b runs Transcoop v2") {
		t.Fatalf("unexpected warnings: %q", got)
	}
	if got := VersionWarnings(workers[:1], "v1"); len(got) != 0 {
		t.Fatalf("matching farm warned: %q", got)
	}
}

func TestHealthIsUnauthenticated(t *testing.T) {
	s := testServer(newStore(t), "0123456789abcdef0123456789abcdef")
	s.Version = "v9.9.9"
	r := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	var v map[string]string
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &v) != nil || v["status"] != "ok" || v["version"] != "v9.9.9" {
		t.Fatalf("health: %d %s", w.Code, w.Body)
	}
	r = httptest.NewRequest("GET", "/api/jobs", nil)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("jobs without credentials: %d", w.Code)
	}
}

func TestIdleLoggedOncePerWorker(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(previous)
	w := &Worker{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); w.logIdle() }()
	}
	wg.Wait()
	if n := strings.Count(buf.String(), "waiting:"); n != 1 {
		t.Fatalf("logged %d times", n)
	}
}

func TestVisibleWorkersHideSupersededInstances(t *testing.T) {
	now := int64(1_000_000)
	all := []WorkerStatus{
		{Instance: "old", DeviceID: "d1", Updated: now - 600},       // offline, superseded by "new"
		{Instance: "new", DeviceID: "d1", Updated: now - 2},         // live
		{Instance: "twin", DeviceID: "d1", Updated: now - 3},        // second live process on the same credential
		{Instance: "solo", DeviceID: "d2", Updated: now - 600},      // offline, but the device's only instance
		{Instance: "ancient", DeviceID: "d3", Updated: now - 90000}, // silent for over a day
		{Instance: "legacy", Updated: now - 600},                    // no device ID: never superseded
	}
	got := map[string]bool{}
	for _, w := range visibleWorkers(all, now) {
		got[w.Instance] = true
	}
	want := map[string]bool{"new": true, "twin": true, "solo": true, "legacy": true}
	if len(got) != len(want) {
		t.Fatalf("visible %v, want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("visible %v, want %v", got, want)
		}
	}
}

func TestWorkerStatusPostsDoNotEvictEachOther(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	var wg sync.WaitGroup
	for _, id := range []string{"instance-aaaaaaaaaaaaaaaa", "instance-bbbbbbbbbbbbbbbb"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if err := s.PutWorker(ctx, id, WorkerStatus{Name: "gizmo", DeviceID: "dev-1"}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	workers, err := s.Workers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 2 {
		t.Fatalf("live instances listed: %d", len(workers))
	}
}
