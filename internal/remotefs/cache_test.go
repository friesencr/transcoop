package remotefs

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/friesencr/transcoop/internal/fsproto"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func cacheFixture(t *testing.T) (*Engine, *Attempt, *atomic.Int64) {
	t.Helper()
	s := fsproto.Settings{RAMBytes: fsproto.BlockSize, DiskBytes: 10 << 20, DirtyBytes: 8 << 20}.Defaults(t.TempDir())
	if err := EnsureDirectories(s); err != nil {
		t.Fatal(err)
	}
	e := New(s, "http://test", "token")
	calls := &atomic.Int64{}
	e.HTTP.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		if strings.HasSuffix(r.URL.Path, "manifest") {
			json.NewEncoder(w).Encode(fsproto.Manifest{Version: 1, Output: "output.mp4", Nodes: []fsproto.Node{{ID: "1", Size: 5 << 20, Path: "source.mkv"}}})
		} else if strings.HasSuffix(r.URL.Path, "read") {
			calls.Add(1)
			time.Sleep(10 * time.Millisecond)
			off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			b := bytes.Repeat([]byte{byte(off / fsproto.BlockSize)}, fsproto.BlockSize)
			w.Header().Set("X-Content-SHA256", fsproto.Digest(b))
			w.Write(b)
		} else {
			w.WriteHeader(204)
		}
		return w.Result(), nil
	})
	a, err := e.Add(fsproto.Control{JobID: 1, Attempt: "attempt", Namespace: "job", Deadline: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	return e, a, calls
}
func TestCacheBudgetsCorruptionAndDeduplication(t *testing.T) {
	e, a, calls := cacheFixture(t)
	n := a.Manifest.Nodes[0]
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.ReadInput(n, make([]byte, 1), 0); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("in-flight requests not deduplicated", calls.Load())
	}
	for i := 1; i < 5; i++ {
		if _, err := a.ReadInput(n, make([]byte, 1), int64(i*fsproto.BlockSize)); err != nil {
			t.Fatal(err)
		}
		e.mu.Lock()
		ram, disk := e.cacheSize, e.diskSize
		e.mu.Unlock()
		if ram > e.Settings.RAMBytes || disk > e.Settings.DiskBytes-e.Settings.DirtyBytes {
			t.Fatal("cache exceeded budget", ram, disk)
		}
	}
	e.mu.Lock()
	entry := e.diskLRU.Back().Value.(diskEntry)
	e.mu.Unlock()
	if err := os.WriteFile(entry.path, []byte("corrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	b := make([]byte, 1)
	if _, err := a.ReadInput(n, b, 3*fsproto.BlockSize); err != nil {
		t.Fatal(err)
	}
	if b[0] != 3 || calls.Load() != before+1 {
		t.Fatal("corrupted disk block used", b, calls.Load())
	}
}
func TestDirtyBackpressurePreservesUnacknowledgedWrites(t *testing.T) {
	e, a, _ := cacheFixture(t)
	original := e.HTTP.Transport
	entered := make(chan struct{})
	var once sync.Once
	e.HTTP.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "batch") {
			once.Do(func() { close(entered) })
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return original.RoundTrip(r)
	})
	if err := a.Mutate("write", "output.mp4", "", 0, make([]byte, fsproto.MaxWrite)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("output did not start streaming")
	}
	done := make(chan error, 1)
	go func() { done <- a.Mutate("write", "output.mp4", "", fsproto.MaxWrite, make([]byte, fsproto.MaxWrite)) }()
	select {
	case err := <-done:
		t.Fatal("dirty limit did not backpressure", err)
	case <-time.After(30 * time.Millisecond):
	}
	if v := e.Stats(); v.DirtyBytes == 0 || v.DirtyBytes > e.Settings.DirtyBytes {
		t.Fatal("unacknowledged data evicted or budget exceeded", v)
	}
	a.Cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled write acknowledged")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked writer ignored cancellation")
	}
}
