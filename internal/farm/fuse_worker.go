package farm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/friesencr/transcoop/internal/filelock"
	"github.com/friesencr/transcoop/internal/fsproto"
)

type fuseHelper struct {
	snapshotAt   atomic.Int64
	next         int64
	pending      map[int64]chan fsproto.Reply
	readerDone   chan struct{}
	cacheSession string
	snapshot     atomic.Pointer[fsproto.Stats]
	mu           sync.Mutex
	cmd          *exec.Cmd
	in           io.WriteCloser
	enc          *json.Encoder
	dec          *json.Decoder
	done         chan struct{}
	unlock       []func()
	settings     fsproto.Settings
}

func (h *fuseHelper) readReplies() {
	defer close(h.readerDone)
	for {
		var r fsproto.Reply
		if err := h.dec.Decode(&r); err != nil {
			return
		}
		h.mu.Lock()
		ch := h.pending[r.RequestID]
		delete(h.pending, r.RequestID)
		h.mu.Unlock()
		if ch != nil {
			ch <- r
		}
	}
}
func (h *fuseHelper) call(c fsproto.Control) (fsproto.Reply, error) {
	h.mu.Lock()
	h.next++
	c.RequestID = h.next
	ch := make(chan fsproto.Reply, 1)
	h.pending[c.RequestID] = ch
	err := h.enc.Encode(c)
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.pending, c.RequestID); h.mu.Unlock() }()
	if err != nil {
		return fsproto.Reply{}, fmt.Errorf("filesystem helper stopped: %w", err)
	}
	var r fsproto.Reply
	select {
	case r = <-ch:
	case <-h.readerDone:
		return r, errors.New("filesystem helper stopped")
	}
	copy := r.Stats
	h.snapshot.Store(&copy)
	h.snapshotAt.Store(time.Now().Unix())
	if r.Error != "" {
		return r, errors.New(r.Error)
	}
	return r, nil
}
func (h *fuseHelper) close() error {
	_, unmountErr := h.call(fsproto.Control{Kind: "unmount"})
	h.in.Close()
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		h.cmd.Process.Kill()
		<-h.done
		if unmountErr == nil {
			unmountErr = errors.New("filesystem helper did not stop after unmount")
		}
	}
	if h.cacheSession != "" {
		os.RemoveAll(h.cacheSession)
	}
	for i := len(h.unlock) - 1; i >= 0; i-- {
		h.unlock[i]()
	}
	return unmountErr
}
func FuseDriverInstructions() string {
	switch runtime.GOOS {
	case "windows":
		return "install WinFsp from https://winfsp.dev/ and the matching transcoop-fs.exe helper"
	case "darwin":
		return "install and enable macFUSE from https://macfuse.github.io/ and the matching transcoop-fs helper"
	default:
		return "install FUSE (libfuse2 and fusermount), enable /dev/fuse for this user, and install the matching transcoop-fs helper"
	}
}
func startFuse(ctx context.Context, settings fsproto.Settings, client *Client) (*fuseHelper, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if withinFilesystemPath(settings.MountDir, settings.CacheDir) || withinFilesystemPath(settings.CacheDir, settings.MountDir) {
		return nil, errors.New("FUSE mount/cache directories must not contain each other")
	}
	if !filepath.IsAbs(settings.MountDir) || !filepath.IsAbs(settings.CacheDir) || settings.MountDir == settings.CacheDir {
		return nil, errors.New("FUSE mount and cache directories must be distinct absolute paths")
	}
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/dev/fuse"); err != nil {
			return nil, fmt.Errorf("FUSE unavailable: %s", FuseDriverInstructions())
		}
	}
	h := &fuseHelper{done: make(chan struct{}), settings: settings, pending: map[int64]chan fsproto.Reply{}, readerDone: make(chan struct{})}
	success := false
	defer func() {
		if !success {
			for _, f := range h.unlock {
				f()
			}
		}
	}()
	for _, p := range []string{settings.MountDir, settings.CacheDir} {
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			return nil, err
		}
		unlock, err := filelock.Lock(p + ".transcoop.lock")
		if err != nil {
			return nil, fmt.Errorf("FUSE directory is in use: %w", err)
		}
		h.unlock = append(h.unlock, unlock)
		if p == settings.MountDir {
			// Holding the lock means no live worker owns a mount here, so a
			// Transcoop mount is one a crashed helper left behind.
			if cleared, err := clearStaleMount(p); err != nil {
				return nil, fmt.Errorf("FUSE mount directory has a stale Transcoop mount that could not be removed (%w); run fusermount -u %s", err, p)
			} else if cleared {
				log.Printf("filesystem: removed a stale Transcoop mount at %s", p)
			}
			if fsproto.MountPointCreatedByDriver {
				// os.Remove deletes only an empty directory, so nothing is lost.
				if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
					return nil, fmt.Errorf("FUSE mount directory %s must be empty or absent: %w", p, err)
				}
				continue
			}
		}
		if err := os.MkdirAll(p, 0700); err != nil {
			return nil, err
		}
	}
	if !fsproto.MountPointCreatedByDriver {
		entries, err := os.ReadDir(settings.MountDir)
		if err != nil || len(entries) != 0 {
			return nil, errors.New("FUSE mount directory must be empty and unmounted; refusing to remove an unverified mount")
		}
	}
	// Only caches marked by this program are removed while holding the cache lock.
	old, err := os.ReadDir(settings.CacheDir)
	if err != nil {
		return nil, err
	}
	for _, entry := range old {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "transcoop-cache-") {
			p := filepath.Join(settings.CacheDir, entry.Name())
			marker, e := os.ReadFile(filepath.Join(p, ".owner"))
			if e == nil && string(marker) == "transcoop filesystem cache v1\n" {
				if e = os.RemoveAll(p); e != nil {
					return nil, e
				}
			}
		}
	}
	session, err := os.MkdirTemp(settings.CacheDir, "transcoop-cache-")
	if err != nil {
		return nil, err
	}
	h.cacheSession = session
	if err = os.WriteFile(filepath.Join(session, ".owner"), []byte("transcoop filesystem cache v1\n"), 0600); err != nil {
		return nil, err
	}
	settings.CacheDir = session
	defer func() {
		if !success {
			os.RemoveAll(session)
		}
	}()
	probe, err := os.CreateTemp(settings.CacheDir, ".write-check-*")
	if err != nil {
		return nil, err
	}
	_, err = probe.Write([]byte("transcoop"))
	if err == nil {
		err = probe.Sync()
	}
	probe.Close()
	os.Remove(probe.Name())
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	name := "transcoop-fs"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	helper := filepath.Join(filepath.Dir(exe), name)
	if _, err = os.Stat(helper); err != nil {
		helper, err = exec.LookPath(name)
		if err != nil {
			return nil, fmt.Errorf("FUSE helper unavailable: %s", FuseDriverInstructions())
		}
	}
	// Not tied to ctx: cancelling would SIGKILL the helper and strand its
	// mount. close() unmounts first; the helper also unmounts on stdin EOF.
	h.cmd = exec.Command(helper)
	isolateHelper(h.cmd)
	h.cmd.Stderr = os.Stderr
	h.in, err = h.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := h.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	h.enc = json.NewEncoder(h.in)
	h.dec = json.NewDecoder(out)
	if err = h.cmd.Start(); err != nil {
		return nil, err
	}
	go func() { h.cmd.Wait(); close(h.done) }()
	go h.readReplies()
	c := fsproto.Control{Kind: "mount", Settings: settings}
	if client != nil {
		c.URL = client.URL
		c.Credential = client.Token
	}
	if _, err = h.call(c); err != nil {
		h.in.Close()
		h.cmd.Process.Kill()
		<-h.done
		return nil, fmt.Errorf("FUSE mount failed: %w; %s", err, FuseDriverInstructions())
	}
	if err = probeMountedFilesystem(settings.MountDir); err != nil {
		h.in.Close()
		h.cmd.Process.Kill()
		<-h.done
		return nil, fmt.Errorf("FUSE read/write/seek preflight: %w", err)
	}
	success = true
	return h, nil
}

// ProbeFuse performs an actual driver mount. Missing drivers never trigger a
// silent storage-mode change on a configured worker.
func ProbeFuse(ctx context.Context, work string) error {
	dir, err := os.MkdirTemp(work, ".fuse-check-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	h, err := startFuse(ctx, fsproto.Settings{}.Defaults(dir), nil)
	if err != nil {
		return err
	}
	return h.close()
}
func probeMountedFilesystem(mount string) error {
	p := filepath.Join(mount, ".transcoop-probe")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.WriteAt([]byte("seek"), 4096); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	b := make([]byte, 4)
	if _, err = f.ReadAt(b, 4096); err != nil {
		return err
	}
	if string(b) != "seek" {
		return errors.New("FUSE seek/read preflight failed")
	}
	return nil
}

func (w *Worker) fuseNamespace(j Job) string {
	return fmt.Sprintf("job-%d-attempt-%d", j.ID, j.Attempts)
}
func (w *Worker) prepareFuse(j Job) (string, string, []Mapping, error) {
	ns := w.fuseNamespace(j)
	r, err := w.fuse.call(fsproto.Control{Kind: "add", Namespace: ns, JobID: j.ID, Attempt: j.Token, Deadline: j.LeaseUntil})
	if err != nil {
		return "", "", nil, err
	}
	if r.Manifest == nil {
		return "", "", nil, errors.New("missing filesystem manifest")
	}
	root := filepath.Join(w.fuse.settings.MountDir, ns)
	m := []Mapping{}
	for original, alias := range r.Manifest.Sidecars {
		m = append(m, Mapping{original, filepath.Join(root, "inputs", filepath.FromSlash(alias))})
	}
	return filepath.Join(root, "inputs", filepath.FromSlash(r.Manifest.Source)), filepath.Join(root, "output", r.Manifest.Output), m, nil
}

func (w *Worker) heartbeat(ctx context.Context, j Job, last int64) (int64, error) {
	end := time.Now().Add(45 * time.Second)
	if t := time.Unix(last, 0); t.Before(end) {
		end = t
	}
	delay := 100 * time.Millisecond
	for {
		started := time.Now()
		requestCtx := ctx
		cancel := func() {}
		if w.Mode == "fuse" {
			requestCtx, cancel = context.WithDeadline(ctx, end)
		}
		_, err := w.Client.JSON(requestCtx, "POST", fmt.Sprintf("/api/jobs/%d/heartbeat", j.ID), j.Token, nil, nil)
		cancel()
		if err == nil {
			return started.Add(LeaseDuration - 5*time.Second).Unix(), nil
		}
		var status *HTTPError
		if w.Mode != "fuse" || ctx.Err() != nil || errors.As(err, &status) && status.Status < 500 && status.Status != 429 || time.Now().Add(delay).After(end) {
			return 0, err
		}
		if !wait(ctx, delay) {
			return 0, ctx.Err()
		}
		delay = min(delay*2, 2*time.Second)
	}
}

func (w *Worker) fuseBlocked() bool {
	if w.fuse == nil {
		return false
	}
	s := w.fuse.snapshot.Load()
	return s == nil || time.Now().Unix()-w.fuse.snapshotAt.Load() > 10 || !s.Mounted || s.Waiting > 0 || s.DirtyBytes > w.fuse.settings.DirtyBytes*4/5
}

func withinFilesystemPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
