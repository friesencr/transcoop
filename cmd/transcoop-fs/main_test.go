//go:build fuse

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/friesencr/transcoop/internal/fsproto"
	"github.com/friesencr/transcoop/internal/remotefs"
	"github.com/winfsp/cgofuse/fuse"
)

type memoryCoordinator struct {
	mu    sync.Mutex
	files map[string][]byte
}

func (m *memoryCoordinator) RoundTrip(r *http.Request) (*http.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := httptest.NewRecorder()
	switch r.URL.Path {
	case "/api/jobs/1/fs/manifest":
		json.NewEncoder(w).Encode(fsproto.Manifest{Version: 1, Source: "source.mkv", Output: "output.mp4", Nodes: []fsproto.Node{{ID: "1", Path: "source.mkv", Size: 6}}})
	case "/api/jobs/1/fs/batch":
		var batch struct{ Operations []fsproto.Operation }
		json.NewDecoder(r.Body).Decode(&batch)
		for _, op := range batch.Operations {
			b := m.files[op.Name]
			switch op.Kind {
			case "create":
				m.files[op.Name] = []byte{}
			case "write":
				end := int(op.Offset) + len(op.Data)
				if len(b) < end {
					b = append(b, make([]byte, end-len(b))...)
				}
				copy(b[op.Offset:], op.Data)
				m.files[op.Name] = b
			case "truncate":
				if int64(len(b)) > op.Offset {
					b = b[:op.Offset]
				} else {
					b = append(b, make([]byte, int(op.Offset)-len(b))...)
				}
				m.files[op.Name] = b
			case "rename":
				m.files[op.Target] = b
				delete(m.files, op.Name)
			case "unlink":
				delete(m.files, op.Name)
			}
		}
		w.WriteHeader(204)
	case "/api/jobs/1/fs/read":
		b := m.files[r.URL.Query().Get("output")]
		if r.URL.Query().Get("node") != "" {
			b = []byte("source")
		}
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		size, _ := strconv.Atoi(r.URL.Query().Get("size"))
		if off >= len(b) {
			b = nil
		} else {
			b = b[off:min(off+size, len(b))]
		}
		w.Header().Set("X-Content-SHA256", fsproto.Digest(b))
		w.Write(b)
	default:
		w.WriteHeader(404)
	}
	return w.Result(), nil
}
func TestAdapterSeekRenameUnlinkReopen(t *testing.T) {
	settings := fsproto.Settings{}.Defaults(t.TempDir())
	if err := remotefs.EnsureDirectories(settings); err != nil {
		t.Fatal(err)
	}
	engine := remotefs.New(settings, "http://test", "secret")
	engine.HTTP.Transport = &memoryCoordinator{files: map[string][]byte{}}
	a, err := engine.Add(fsproto.Control{JobID: 1, Namespace: "attempt", Attempt: "token", Deadline: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	fs := &filesystem{attempts: map[string]*mountedAttempt{"attempt": {remote: a, files: map[string]*file{}}}, handles: map[uint64]handle{}}
	name := "/attempt/output/output.mp4"
	code, handle := fs.Create(name, fuse.O_RDWR, 0600)
	if code != 0 {
		t.Fatal(code)
	}
	if n := fs.Write(name, []byte("tail"), 4096, handle); n != 4 {
		t.Fatal(n)
	}
	if n := fs.Write(name, []byte("head"), 0, handle); n != 4 {
		t.Fatal(n)
	}
	b := make([]byte, 4100)
	if n := fs.Read(name, b, 0, handle); n != 4100 || !bytes.Equal(b[:4], []byte("head")) || !bytes.Equal(b[4096:], []byte("tail")) {
		t.Fatal("sparse/header read", n)
	}
	if code = fs.Rename(name, "/attempt/output/renamed.mp4"); code != 0 {
		t.Fatal(code)
	}
	if code = fs.Unlink("/attempt/output/renamed.mp4"); code != 0 {
		t.Fatal(code)
	}
	if n := fs.Read(name, b[:4], 0, handle); n != 4 || string(b[:4]) != "head" {
		t.Fatal("unlinked open handle lost", n)
	}
	if code = fs.Release(name, handle); code != 0 {
		t.Fatal(code)
	}
	code, handle = fs.Create(name, fuse.O_RDWR, 0600)
	if code != 0 {
		t.Fatal(code)
	}
	fs.Write(name, []byte("abcdef"), 0, handle)
	if code = fs.Truncate(name, 2, handle); code != 0 {
		t.Fatal(code)
	}
	fs.Release(name, handle)
	code, handle = fs.Open(name, fuse.O_RDWR)
	if code != 0 {
		t.Fatal(code)
	}
	if code = fs.Truncate(name, 8, handle); code != 0 {
		t.Fatal(code)
	}
	b = make([]byte, 8)
	if n := fs.Read(name, b, 0, handle); n != 8 || !bytes.Equal(b, []byte{'a', 'b', 0, 0, 0, 0, 0, 0}) {
		t.Fatal(n, b)
	}
	if code = fs.Fsync(name, false, handle); code != 0 {
		t.Fatal(code)
	}
	fs.Release(name, handle)
	if code, _ = fs.Open("/attempt/inputs/source.mkv", fuse.O_RDWR); code != -fuse.EROFS {
		t.Fatal("source writable")
	}
	if code = fs.Rename(name, "/other/output/x"); code != -fuse.EXDEV {
		t.Fatal("cross-attempt rename accepted")
	}
}

func TestDirectoryEnumerationResumes(t *testing.T) {
	fs := &filesystem{attempts: map[string]*mountedAttempt{"z": {}, "a": {}, "b": {}}}
	var names []string
	var next int64
	fill := func(name string, s *fuse.Stat_t, off int64) bool {
		if len(names) == 3 {
			return false
		}
		names = append(names, name)
		next = off
		return true
	}
	if code := fs.Readdir("/", fill, 0, 0); code != 0 {
		t.Fatal(code)
	}
	if code := fs.Readdir("/", func(name string, s *fuse.Stat_t, off int64) bool { names = append(names, name); return true }, next, 0); code != 0 {
		t.Fatal(code)
	}
	if len(names) != 5 || names[2] != "a" || names[3] != "b" || names[4] != "z" {
		t.Fatal(names)
	}
}
