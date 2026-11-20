package farm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/friesencr/transcoop/internal/fsproto"
	"github.com/friesencr/transcoop/internal/remotefs"
)

type fsTransport struct{ handler http.Handler }

func (t fsTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	t.handler.ServeHTTP(w, r)
	return w.Result(), nil
}
func fsFixture(t *testing.T) (*Store, Job, Enrollment, http.Handler) {
	t.Helper()
	s := newStore(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(source, bytes.Repeat([]byte("abcdef"), 400000), 0600); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"Job": map[string]any{"Source": map[string]any{"Path": source}, "Destination": map[string]any{"File": filepath.Join(dir, "out.mp4")}, "Video": map[string]any{"Encoder": "x264"}}})
	if _, err := s.Import(context.Background(), []json.RawMessage{payload}); err != nil {
		t.Fatal(err)
	}
	e := enroll(t, s, "worker")
	j, err := s.claimMatching(context.Background(), "test", []string{"x264"}, LeaseDuration, e.Device.ID)
	if err != nil || j == nil {
		t.Fatal(err)
	}
	return s, *j, e, (&Server{Store: s}).Handler()
}
func fsRequest(h http.Handler, j Job, e Enrollment, method, action string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/jobs/"+fmtID(j.ID)+"/fs/"+action, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+e.Token)
	r.Header.Set("X-Attempt-Token", j.Token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func fmtID(id int64) string { return fmt.Sprintf("%d", id) }
func TestFilesystemOrderedDurabilityAndRecovery(t *testing.T) {
	s, j, e, h := fsFixture(t)
	ctx := context.Background()
	if r := fsRequest(h, j, e, "GET", "manifest", nil); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	v, err := s.fsSession(ctx, j)
	if err != nil {
		t.Fatal(err)
	}
	apply := func(op fsproto.Operation) fsproto.Operation {
		t.Helper()
		op.Digest = op.Hash()
		b, _ := json.Marshal(op)
		if r := fsRequest(h, j, e, "POST", "operations", b); r.Code != 204 {
			t.Fatal(r.Code, r.Body.String())
		}
		return op
	}
	apply(fsproto.Operation{Sequence: 1, Kind: "create", Name: v.Manifest.Output})
	op := apply(fsproto.Operation{Sequence: 2, Kind: "write", Name: v.Manifest.Output, Offset: 4096, Data: []byte("tail")})
	apply(op) // Lost acknowledgement.
	op.Data = []byte("evil")
	op.Digest = op.Hash()
	b, _ := json.Marshal(op)
	if r := fsRequest(h, j, e, "POST", "operations", b); r.Code == 204 {
		t.Fatal("conflicting replay accepted")
	}
	op.Sequence = 4
	op.Digest = op.Hash()
	b, _ = json.Marshal(op)
	if r := fsRequest(h, j, e, "POST", "operations", b); r.Code == 204 {
		t.Fatal("out-of-order operation accepted")
	}
	if err = s.Finish(ctx, j.ID, j.Token, "", publish); err == nil {
		t.Fatal("published unsealed output")
	}
	apply(fsproto.Operation{Sequence: 3, Kind: "write", Name: v.Manifest.Output, Data: []byte("head")})
	// Reopen the SQLite database to exercise persisted manifest and sequence state.
	var seq int
	var dbName, dbPath string
	if err = s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &dbName, &dbPath); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, reopenErr := OpenStore(dbPath)
	if reopenErr != nil {
		t.Fatal(reopenErr)
	}
	s.db = reopened.db
	s.filesystemCancel = reopened.filesystemCancel
	s.filesystemDone = reopened.filesystemDone
	h = (&Server{Store: s}).Handler()
	apply(fsproto.Operation{Sequence: 4, Kind: "truncate", Name: v.Manifest.Output, Offset: 4100})
	apply(fsproto.Operation{Sequence: 5, Kind: "seal", Name: v.Manifest.Output})
	if _, err = os.Stat(j.Destination); !os.IsNotExist(err) {
		t.Fatal("output visible before completion")
	}
	if err = s.Finish(ctx, j.ID, j.Token, "", publish); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(j.Destination)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 4100 || string(data[:4]) != "head" || string(data[4096:]) != "tail" {
		t.Fatal("incorrect sparse/header output")
	}
	if err = s.CleanupFilesystem(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestFilesystemIsolationAndSourceChanges(t *testing.T) {
	s, j, e, h := fsFixture(t)
	r := fsRequest(h, j, e, "GET", "manifest", nil)
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	other := enroll(t, s, "worker")
	if r = fsRequest(h, j, other, "GET", "manifest", nil); r.Code != 403 {
		t.Fatal("wrong worker accepted", r.Code)
	}
	wrong := j
	wrong.Token = "wrong"
	if r = fsRequest(h, wrong, e, "GET", "read?node=1&offset=0&size=4", nil); r.Code != 403 {
		t.Fatal("wrong attempt accepted")
	}
	if r = fsRequest(h, j, e, "GET", "read?node=../../etc/passwd&offset=0&size=4", nil); r.Code == 200 {
		t.Fatal("path accepted as node")
	}
	if r = fsRequest(h, j, e, "GET", "read?node=1&offset=1&size=4", nil); r.Code != 200 || r.Body.String() != "bcde" {
		t.Fatal(r.Code, r.Body.String())
	}
	info, _ := os.Stat(j.Source)
	replacement := j.Source + ".new"
	data, _ := os.ReadFile(j.Source)
	os.WriteFile(replacement, data, 0600)
	os.Chtimes(replacement, info.ModTime(), info.ModTime())
	os.Rename(replacement, j.Source)
	if r = fsRequest(h, j, e, "GET", "read?node=1&offset=0&size=4", nil); r.Code == 200 {
		t.Fatal("replacement with preserved size/mtime accepted")
	}
	s.db.Exec(`UPDATE jobs SET lease_until=0 WHERE id=?`, j.ID)
	if r = fsRequest(h, j, e, "GET", "manifest", nil); r.Code != 403 {
		t.Fatal("expired lease accepted")
	}
}
func TestFilesystemRandomizedEngine(t *testing.T) {
	_, j, enrollment, h := fsFixture(t)
	settings := fsproto.Settings{RAMBytes: fsproto.BlockSize, DirtyBytes: 8 << 20, DiskBytes: 8 << 20}.Defaults(t.TempDir())
	if err := remotefs.EnsureDirectories(settings); err != nil {
		t.Fatal(err)
	}
	engine := remotefs.New(settings, "http://coordinator", enrollment.Token)
	engine.HTTP.Transport = fsTransport{h}
	a, err := engine.Add(fsproto.Control{JobID: j.ID, Attempt: j.Token, Namespace: "attempt-1", Deadline: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	source, _ := os.ReadFile(j.Source)
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 30; i++ {
		off := rng.Intn(len(source) - 2000)
		b := make([]byte, 2000)
		n, err := a.ReadInput(a.Manifest.Nodes[0], b, int64(off))
		if err != nil || n != len(b) || !bytes.Equal(b, source[off:off+n]) {
			t.Fatal("random input read", err)
		}
		if engine.Stats().CacheBytes > settings.RAMBytes {
			t.Fatal("cache exceeded limit")
		}
	}
	name := a.Manifest.Output
	if err = a.Mutate("create", name, "", 0, nil); err != nil {
		t.Fatal(err)
	}
	model := []byte{}
	for i := 0; i < 80; i++ {
		if rng.Intn(4) == 0 {
			size := rng.Intn(100000)
			if err = a.Mutate("truncate", name, "", int64(size), nil); err != nil {
				t.Fatal(err)
			}
			if size < len(model) {
				model = model[:size]
			} else {
				model = append(model, make([]byte, size-len(model))...)
			}
		} else {
			off := rng.Intn(100000)
			data := make([]byte, rng.Intn(4096)+1)
			rng.Read(data)
			if err = a.Mutate("write", name, "", int64(off), data); err != nil {
				t.Fatal(err)
			}
			if end := off + len(data); end > len(model) {
				model = append(model, make([]byte, end-len(model))...)
			}
			copy(model[off:], data)
		}
		if engine.Stats().DirtyBytes > settings.DirtyBytes {
			t.Fatal("dirty cache exceeded limit")
		}
		if i == 39 {
			if err = a.Flush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	overlay := make([]byte, len(model))
	if n, e := a.ReadOutputSize(name, overlay, 0, int64(len(model))); e != nil || n != len(model) || !bytes.Equal(overlay, model) {
		t.Fatal("dirty overlay differs from local file", e)
	}
	if err = a.Flush(); err != nil {
		t.Fatal(err)
	}
	actual := make([]byte, len(model))
	n, err := a.ReadOutput(name, actual, 0)
	if err != nil || n != len(model) || !bytes.Equal(actual, model) {
		t.Fatal("random writes/truncate/sparse mismatch", err, n, len(model))
	}
	if err = a.Mutate("rename", name, "temporary.mp4", 0, nil); err != nil {
		t.Fatal(err)
	}
	if err = a.Mutate("rename", "temporary.mp4", name, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err = a.Seal(); err != nil {
		t.Fatal(err)
	}
}
func TestFilesystemManifestDiscAndSidecars(t *testing.T) {
	dir := t.TempDir()
	disc := filepath.Join(dir, "disc")
	os.MkdirAll(filepath.Join(disc, "VIDEO_TS"), 0700)
	os.WriteFile(filepath.Join(disc, "VIDEO_TS", "VTS_01_1.VOB"), []byte("video"), 0600)
	odd := "bad:name.srt" // the mount renames sidecars to safe names
	if runtime.GOOS == "windows" {
		odd = "bad name.srt" // ":" is not allowed in Windows file names
	}
	subtitle := filepath.Join(dir, odd)
	os.WriteFile(subtitle, []byte("subtitle"), 0600)
	raw, _ := json.Marshal(map[string]any{"Job": map[string]any{"Subtitle": map[string]any{"SubtitleList": []any{map[string]any{"Import": map[string]any{"Filename": subtitle}}}}}})
	j := Job{Source: disc, Destination: filepath.Join(dir, "out.mkv"), Payload: raw}
	v, err := buildFSManifest(j)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Manifest.Nodes) != 4 || v.Manifest.Sidecars[subtitle] != "subtitle-0.srt" {
		t.Fatal(v.Manifest)
	}
	os.Symlink(subtitle, filepath.Join(disc, "escape"))
	if _, err = buildFSManifest(j); err == nil {
		t.Fatal("symlink tree accepted")
	}
	os.Remove(filepath.Join(disc, "escape"))
	// Only a case-sensitive filesystem can hold both VIDEO_TS and video_ts.
	if err := os.WriteFile(filepath.Join(disc, "video_ts"), nil, 0600); err == nil {
		if entries, _ := os.ReadDir(disc); len(entries) == 2 {
			if _, err = buildFSManifest(j); err == nil {
				t.Fatal("case collision accepted")
			}
		}
	}
}
func TestFilesystemChecksumFailure(t *testing.T) {
	_, j, e, h := fsFixture(t)
	settings := fsproto.Settings{}.Defaults(t.TempDir())
	remotefs.EnsureDirectories(settings)
	engine := remotefs.New(settings, "http://coordinator", e.Token)
	engine.HTTP.Transport = fsTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("node") != "" {
			w.Header().Set("X-Content-SHA256", "wrong")
			io.WriteString(w, "bad")
			return
		}
		h.ServeHTTP(w, r)
	})}
	a, err := engine.Add(fsproto.Control{JobID: j.ID, Attempt: j.Token, Namespace: "checksum", Deadline: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	if _, err = a.ReadInput(a.Manifest.Nodes[0], make([]byte, 1), 0); err == nil {
		t.Fatal("corrupt block accepted")
	}
}

func TestFilesystemLostBatchAcknowledgement(t *testing.T) {
	_, j, e, h := fsFixture(t)
	settings := fsproto.Settings{}.Defaults(t.TempDir())
	remotefs.EnsureDirectories(settings)
	engine := remotefs.New(settings, "http://coordinator", e.Token)
	var lost bool
	engine.HTTP.Transport = fsTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/jobs/1/fs/batch" && !lost {
			lost = true
			record := httptest.NewRecorder()
			h.ServeHTTP(record, r)
			if record.Code != 204 {
				t.Error(record.Body.String())
			}
			http.Error(w, "lost acknowledgement", http.StatusServiceUnavailable)
			return
		}
		h.ServeHTTP(w, r)
	})}
	a, err := engine.Add(fsproto.Control{JobID: j.ID, Attempt: j.Token, Namespace: "lost-ack", Deadline: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	for _, op := range []fsproto.Operation{{Kind: "create"}, {Kind: "write", Data: []byte("abcdef")}, {Kind: "write", Offset: 1, Data: []byte("XY")}} {
		if err = a.Mutate(op.Kind, a.Manifest.Output, "", op.Offset, op.Data); err != nil {
			t.Fatal(err)
		}
	}
	if err = a.Flush(); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	if _, err = a.ReadOutput(a.Manifest.Output, b, 0); err != nil || string(b) != "aXYdef" {
		t.Fatal(string(b), err)
	}
	if engine.Stats().Retries != 1 {
		t.Fatal("retry not recorded")
	}
}
func TestFilesystemDestinationConflictAndCancellation(t *testing.T) {
	s, j, e, h := fsFixture(t)
	settings := fsproto.Settings{}.Defaults(t.TempDir())
	remotefs.EnsureDirectories(settings)
	engine := remotefs.New(settings, "http://coordinator", e.Token)
	engine.HTTP.Transport = fsTransport{h}
	a, err := engine.Add(fsproto.Control{JobID: j.ID, Attempt: j.Token, Namespace: "conflict", Deadline: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	name := a.Manifest.Output
	if err = a.Mutate("create", name, "", 0, nil); err != nil {
		t.Fatal(err)
	}
	if err = a.Mutate("write", name, "", 0, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if err = a.Seal(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(j.Destination, []byte("existing"), 0600)
	if err = s.Finish(context.Background(), j.ID, j.Token, "", publish); err == nil {
		t.Fatal("overwrote destination")
	}
	b, _ := os.ReadFile(j.Destination)
	if string(b) != "existing" {
		t.Fatal("destination changed")
	}
	a.Cancel()
	if err = a.Mutate("write", name, "", 0, []byte("late")); err == nil {
		t.Fatal("write after cancellation accepted")
	}
}
func TestFilesystemJournalFailure(t *testing.T) {
	_, j, e, h := fsFixture(t)
	settings := fsproto.Settings{}.Defaults(t.TempDir())
	remotefs.EnsureDirectories(settings)
	engine := remotefs.New(settings, "http://coordinator", e.Token)
	engine.HTTP.Transport = fsTransport{h}
	a, err := engine.Add(fsproto.Control{JobID: j.ID, Attempt: j.Token, Namespace: "disk-failure", Deadline: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	os.RemoveAll(settings.CacheDir)
	os.WriteFile(settings.CacheDir, []byte("not a directory"), 0600)
	if err = a.Mutate("create", a.Manifest.Output, "", 0, nil); err == nil {
		t.Fatal("journal failure acknowledged")
	}
	if engine.Stats().DirtyBytes != 0 {
		t.Fatal("failed journal leaked quota")
	}
}
