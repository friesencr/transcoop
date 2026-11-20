//go:build fuse

// transcoop-fs is the only binary linked to the platform filesystem driver.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/friesencr/transcoop/internal/fsproto"
	"github.com/friesencr/transcoop/internal/remotefs"
	"github.com/winfsp/cgofuse/fuse"
)

type file struct {
	name    string
	size    int64
	refs    int
	removed bool
}
type mountedAttempt struct {
	remote *remotefs.Attempt
	mu     sync.Mutex
	files  map[string]*file
}
type handle struct {
	attempt  *mountedAttempt
	node     *fsproto.Node
	output   *file
	writable bool
	readable bool
}
type filesystem struct {
	probeMu   sync.Mutex
	probe     [8192]byte
	probeSize int64
	fuse.FileSystemBase
	mu       sync.Mutex
	attempts map[string]*mountedAttempt
	handles  map[uint64]handle
	next     uint64
	ready    chan struct{}
}

func (f *filesystem) Statfs(p string, s *fuse.Statfs_t) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	free := ^uint64(0)
	for _, a := range f.attempts {
		if v := a.remote.Manifest.AvailableBytes; v > 0 {
			free = min(free, v)
		}
	}
	if free == ^uint64(0) {
		return -fuse.ENOSYS
	}
	s.Bsize = 4096
	s.Frsize = 4096
	s.Blocks = free / 4096
	s.Bfree = s.Blocks
	s.Bavail = s.Blocks
	s.Files = 1000000
	s.Ffree = 1000000
	s.Namemax = 255
	return 0
}
func (f *filesystem) Init() { close(f.ready) }
func (f *filesystem) lookup(p string) (*mountedAttempt, string) {
	parts := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 2)
	f.mu.Lock()
	a := f.attempts[parts[0]]
	f.mu.Unlock()
	if len(parts) == 1 {
		return a, ""
	}
	return a, parts[1]
}
func (f *filesystem) node(a *mountedAttempt, p string) *fsproto.Node {
	if !strings.HasPrefix(p, "inputs/") {
		return nil
	}
	for i := range a.remote.Manifest.Nodes {
		n := &a.remote.Manifest.Nodes[i]
		if n.Path == strings.TrimPrefix(p, "inputs/") {
			return n
		}
	}
	return nil
}
func (f *filesystem) getHandle(id uint64) (handle, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.handles[id]
	return h, ok
}
func (f *filesystem) newHandle(h handle) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.handles[f.next] = h
	return f.next
}
func (f *filesystem) Getattr(p string, s *fuse.Stat_t, id uint64) int {
	s.Uid = uint32(os.Getuid())
	s.Gid = uint32(os.Getgid())
	if p == "/.transcoop-probe" {
		f.probeMu.Lock()
		s.Size = f.probeSize
		f.probeMu.Unlock()
		s.Mode = fuse.S_IFREG | 0600
		s.Nlink = 1
		return 0
	}
	s.Nlink = 1
	s.Uid = uint32(os.Getuid())
	s.Gid = uint32(os.Getgid())
	if p == "/" {
		s.Mode = fuse.S_IFDIR | 0700
		return 0
	}
	if h, ok := f.getHandle(id); ok && h.output != nil {
		h.attempt.mu.Lock()
		s.Size = h.output.size
		h.attempt.mu.Unlock()
		s.Mode = fuse.S_IFREG | 0600
		return 0
	}
	a, rel := f.lookup(p)
	if a == nil {
		return -fuse.ENOENT
	}
	if rel == "" || rel == "inputs" || rel == "output" {
		s.Mode = fuse.S_IFDIR | 0700
		return 0
	}
	if n := f.node(a, rel); n != nil {
		s.Mode = fuse.S_IFREG | 0400
		if n.Directory {
			s.Mode = fuse.S_IFDIR | 0500
		}
		s.Size = n.Size
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if o := a.files[strings.TrimPrefix(rel, "output/")]; strings.HasPrefix(rel, "output/") && o != nil {
		s.Mode = fuse.S_IFREG | 0600
		s.Size = o.size
		return 0
	}
	return -fuse.ENOENT
}
func (f *filesystem) Readdir(p string, fill func(string, *fuse.Stat_t, int64) bool, offset int64, id uint64) int {
	names := []string{".", ".."}
	if p == "/" {
		f.mu.Lock()
		for n := range f.attempts {
			names = append(names, n)
		}
		f.mu.Unlock()
	} else {
		a, rel := f.lookup(p)
		if a == nil {
			return -fuse.ENOENT
		}
		switch rel {
		case "":
			names = append(names, "inputs", "output")
		case "output":
			a.mu.Lock()
			for n := range a.files {
				names = append(names, n)
			}
			a.mu.Unlock()
		default:
			prefix := strings.TrimPrefix(rel, "inputs")
			prefix = strings.TrimPrefix(prefix, "/")
			for _, n := range a.remote.Manifest.Nodes {
				if path.Dir(n.Path) == prefix || prefix == "" && path.Dir(n.Path) == "." {
					names = append(names, path.Base(n.Path))
				}
			}
		}
	}
	if offset < 0 {
		return -fuse.EINVAL
	}
	sort.Strings(names[2:])
	for i := offset; i < int64(len(names)); i++ {
		if !fill(names[i], nil, i+1) {
			break
		}
	}
	return 0
}
func (f *filesystem) Opendir(p string) (int, uint64) {
	var s fuse.Stat_t
	e := f.Getattr(p, &s, ^uint64(0))
	if e != 0 {
		return e, 0
	}
	if s.Mode&fuse.S_IFDIR == 0 {
		return -fuse.ENOTDIR, 0
	}
	return 0, 0
}
func (f *filesystem) Releasedir(string, uint64) int { return 0 }
func (f *filesystem) Open(p string, flags int) (int, uint64) {
	if p == "/.transcoop-probe" {
		return 0, ^uint64(0) - 1
	}
	a, rel := f.lookup(p)
	if a == nil {
		return -fuse.ENOENT, 0
	}
	if n := f.node(a, rel); n != nil {
		if flags&(fuse.O_WRONLY|fuse.O_RDWR|fuse.O_TRUNC) != 0 {
			return -fuse.EROFS, 0
		}
		if n.Directory {
			return -fuse.EISDIR, 0
		}
		return 0, f.newHandle(handle{attempt: a, node: n, readable: true})
	}
	if !strings.HasPrefix(rel, "output/") {
		return -fuse.EROFS, 0
	}
	name := strings.TrimPrefix(rel, "output/")
	a.mu.Lock()
	defer a.mu.Unlock()
	o := a.files[name]
	if o == nil {
		return -fuse.ENOENT, 0
	}
	writable := flags&(fuse.O_WRONLY|fuse.O_RDWR) != 0
	if flags&fuse.O_TRUNC != 0 {
		if !writable {
			return -fuse.EACCES, 0
		}
		if err := a.remote.Mutate("truncate", name, "", 0, nil); err != nil {
			return -fuse.EIO, 0
		}
		o.size = 0
	}
	o.refs++
	return 0, f.newHandle(handle{attempt: a, output: o, writable: writable, readable: flags&fuse.O_WRONLY == 0})
}
func (f *filesystem) Create(p string, flags int, mode uint32) (int, uint64) {
	if p == "/.transcoop-probe" {
		return 0, ^uint64(0) - 1
	}
	a, rel := f.lookup(p)
	if a == nil || !strings.HasPrefix(rel, "output/") {
		return -fuse.EROFS, 0
	}
	name := strings.TrimPrefix(rel, "output/")
	if !fsproto.PortableName(name) {
		return -fuse.EINVAL, 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for existing, o := range a.files {
		if !strings.EqualFold(existing, name) {
			continue
		}
		if existing != name || flags&fuse.O_EXCL != 0 {
			return -fuse.EEXIST, 0
		}
		writable := flags&(fuse.O_WRONLY|fuse.O_RDWR) != 0
		if flags&fuse.O_TRUNC != 0 {
			if !writable {
				return -fuse.EACCES, 0
			}
			if err := a.remote.Mutate("truncate", name, "", 0, nil); err != nil {
				return -fuse.EIO, 0
			}
			o.size = 0
		}
		o.refs++
		return 0, f.newHandle(handle{attempt: a, output: o, writable: writable, readable: flags&fuse.O_WRONLY == 0})
	}
	if err := a.remote.Mutate("create", name, "", 0, nil); err != nil {
		return -fuse.EIO, 0
	}
	o := &file{name: name, refs: 1}
	a.files[name] = o
	return 0, f.newHandle(handle{attempt: a, output: o, writable: flags&(fuse.O_WRONLY|fuse.O_RDWR) != 0, readable: flags&fuse.O_WRONLY == 0})
}
func (f *filesystem) Read(p string, b []byte, off int64, id uint64) int {
	if p == "/.transcoop-probe" {
		f.probeMu.Lock()
		defer f.probeMu.Unlock()
		if off < 0 || off >= f.probeSize {
			return 0
		}
		return copy(b, f.probe[off:f.probeSize])
	}
	h, ok := f.getHandle(id)
	if !ok || !h.readable {
		return -fuse.EBADF
	}
	if h.node != nil {
		n, e := h.attempt.remote.ReadInput(*h.node, b, off)
		if e != nil {
			return -fuse.EIO
		}
		return n
	}
	h.attempt.mu.Lock()
	defer h.attempt.mu.Unlock()
	n, e := h.attempt.remote.ReadOutputSize(h.output.name, b, off, h.output.size)
	if e != nil {
		return -fuse.EIO
	}
	return n
}
func (f *filesystem) Write(p string, b []byte, off int64, id uint64) int {
	if p == "/.transcoop-probe" {
		f.probeMu.Lock()
		defer f.probeMu.Unlock()
		if off < 0 || off+int64(len(b)) > 8192 {
			return -fuse.ENOSPC
		}
		n := copy(f.probe[off:], b)
		f.probeSize = max(f.probeSize, off+int64(n))
		return n
	}
	h, ok := f.getHandle(id)
	if !ok || h.output == nil || !h.writable {
		return -fuse.EBADF
	}
	a := h.attempt
	a.mu.Lock()
	defer a.mu.Unlock()
	total := 0
	for len(b) > 0 {
		n := min(len(b), fsproto.MaxWrite)
		if err := a.remote.Mutate("write", h.output.name, "", off, b[:n]); err != nil {
			return -fuse.EIO
		}
		off += int64(n)
		total += n
		b = b[n:]
	}
	h.output.size = max(h.output.size, off)
	return total
}
func (f *filesystem) Truncate(p string, size int64, id uint64) int {
	h, ok := f.getHandle(id)
	if !ok {
		a, rel := f.lookup(p)
		if a == nil || !strings.HasPrefix(rel, "output/") {
			return -fuse.EROFS
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		o := a.files[strings.TrimPrefix(rel, "output/")]
		if o == nil {
			return -fuse.ENOENT
		}
		if err := a.remote.Mutate("truncate", o.name, "", size, nil); err != nil {
			return -fuse.EIO
		}
		o.size = size
		return 0
	}
	if h.output == nil || !h.writable {
		return -fuse.EROFS
	}
	h.attempt.mu.Lock()
	defer h.attempt.mu.Unlock()
	if err := h.attempt.remote.Mutate("truncate", h.output.name, "", size, nil); err != nil {
		return -fuse.EIO
	}
	h.output.size = size
	return 0
}
func (f *filesystem) Fsync(p string, datasync bool, id uint64) int {
	if p == "/.transcoop-probe" {
		return 0
	}
	h, ok := f.getHandle(id)
	if !ok {
		return -fuse.EBADF
	}
	if err := h.attempt.remote.Flush(); err != nil {
		return -fuse.EIO
	}
	return 0
}
func (f *filesystem) Flush(p string, id uint64) int { return f.Fsync(p, false, id) }
func (f *filesystem) Release(p string, id uint64) int {
	f.mu.Lock()
	h, ok := f.handles[id]
	delete(f.handles, id)
	f.mu.Unlock()
	if !ok {
		return 0
	}
	if h.output != nil {
		h.attempt.mu.Lock()
		defer h.attempt.mu.Unlock()
		h.output.refs--
		if h.output.removed && h.output.refs == 0 {
			if err := h.attempt.remote.Mutate("unlink", h.output.name, "", 0, nil); err != nil {
				return -fuse.EIO
			}
		}
	}
	return 0
}
func (f *filesystem) Unlink(p string) int {
	a, rel := f.lookup(p)
	if a == nil || !strings.HasPrefix(rel, "output/") {
		return -fuse.EROFS
	}
	name := strings.TrimPrefix(rel, "output/")
	a.mu.Lock()
	defer a.mu.Unlock()
	o := a.files[name]
	if o == nil {
		return -fuse.ENOENT
	}
	if o.refs > 0 {
		target := fmt.Sprintf("removed-%d", time.Now().UnixNano())
		if err := a.remote.Mutate("rename", name, target, 0, nil); err != nil {
			return -fuse.EIO
		}
		o.name = target
		o.removed = true
	} else {
		if err := a.remote.Mutate("unlink", name, "", 0, nil); err != nil {
			return -fuse.EIO
		}
	}
	delete(a.files, name)
	return 0
}
func (f *filesystem) Rename(old, new string) int {
	a, rel := f.lookup(old)
	b, target := f.lookup(new)
	if a == nil || a != b {
		return -fuse.EXDEV
	}
	if !strings.HasPrefix(rel, "output/") || !strings.HasPrefix(target, "output/") {
		return -fuse.EROFS
	}
	name := strings.TrimPrefix(rel, "output/")
	to := strings.TrimPrefix(target, "output/")
	if !fsproto.PortableName(to) {
		return -fuse.EINVAL
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	o := a.files[name]
	if o == nil {
		return -fuse.ENOENT
	}
	if name == to {
		return 0
	}
	for existing := range a.files {
		if existing != name && existing != to && strings.EqualFold(existing, to) {
			return -fuse.EEXIST
		}
	}
	if dest := a.files[to]; dest != nil {
		if dest.refs > 0 {
			hidden := fmt.Sprintf("removed-%d", time.Now().UnixNano())
			if err := a.remote.Mutate("rename", to, hidden, 0, nil); err != nil {
				return -fuse.EIO
			}
			dest.name = hidden
			dest.removed = true
		} else {
			if err := a.remote.Mutate("unlink", to, "", 0, nil); err != nil {
				return -fuse.EIO
			}
		}
		delete(a.files, to)
	}
	if err := a.remote.Mutate("rename", name, to, 0, nil); err != nil {
		return -fuse.EIO
	}
	delete(a.files, name)
	o.name = to
	a.files[to] = o
	return 0
}
func (f *filesystem) Chmod(p string, mode uint32) int {
	_, rel := f.lookup(p)
	if !strings.HasPrefix(rel, "output/") {
		return -fuse.EROFS
	}
	return 0
}
func (f *filesystem) Utimens(p string, times []fuse.Timespec) int {
	_, rel := f.lookup(p)
	if !strings.HasPrefix(rel, "output/") {
		return -fuse.EROFS
	}
	return 0
}

// waitMounted returns once the mount serves this filesystem. Init alone is not
// proof: WinFsp calls it before setting the mount point, and if that fails it
// can leave a plain directory where the mount should be.
func waitMounted(mountDir string, ready <-chan struct{}, finished <-chan bool) error {
	failed := errors.New("mount failed; install and enable the platform FUSE driver, and check that the mount directory is not in use")
	deadline := time.After(15 * time.Second)
	select {
	case <-ready:
	case <-finished:
		return failed
	case <-deadline:
		return errors.New("mount readiness timeout")
	}
	probe := filepath.Join(mountDir, ".transcoop-probe") // served only by this filesystem
	for {
		if _, err := os.Stat(probe); err == nil {
			return nil
		}
		select {
		case <-finished:
			return failed
		case <-deadline:
			return errors.New("mount readiness timeout: the mount point never served the Transcoop filesystem")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func run() error {
	dec := json.NewDecoder(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	var init fsproto.Control
	if err := dec.Decode(&init); err != nil {
		return err
	}
	if init.Kind != "mount" {
		return fmt.Errorf("expected mount configuration")
	}
	if err := remotefs.EnsureDirectories(init.Settings); err != nil {
		return err
	}
	engine := remotefs.New(init.Settings, init.URL, init.Credential)
	fs := &filesystem{attempts: map[string]*mountedAttempt{}, handles: map[uint64]handle{}, ready: make(chan struct{})}
	host := fuse.NewFileSystemHost(fs)
	finished := make(chan bool, 1)
	go func() {
		finished <- host.Mount(init.Settings.MountDir, []string{"-o", mountOptions})
	}()
	if err := waitMounted(init.Settings.MountDir, fs.ready, finished); err != nil {
		host.Unmount()
		return err
	}
	defer host.Unmount()
	if err := enc.Encode(fsproto.Reply{RequestID: init.RequestID, Stats: engine.Stats()}); err != nil {
		return err
	}
	defer func() {
		for _, a := range fs.attempts {
			a.remote.Stop()
		}
	}()
	var responseMu sync.Mutex
	var requests sync.WaitGroup
	defer requests.Wait()
	for {
		var c fsproto.Control
		if err := dec.Decode(&c); err != nil {
			fs.mu.Lock()
			active := make([]*mountedAttempt, 0, len(fs.attempts))
			for _, a := range fs.attempts {
				active = append(active, a)
			}
			fs.mu.Unlock()
			for _, a := range active {
				a.remote.Cancel()
			}
			return nil
		}
		if c.Kind == "unmount" {
			fs.mu.Lock()
			active := fs.attempts
			fs.attempts = map[string]*mountedAttempt{}
			fs.mu.Unlock()
			for _, a := range active {
				a.remote.Stop()
			}
			requests.Wait()
			reply := fsproto.Reply{RequestID: c.RequestID}
			if !host.Unmount() {
				reply.Error = "unmount failed"
			}
			enc.Encode(reply)
			return nil
		}
		requests.Add(1)
		go func(c fsproto.Control) {
			defer requests.Done()
			reply := fsproto.Reply{RequestID: c.RequestID}
			fs.mu.Lock()
			a := fs.attempts[c.Namespace]
			fs.mu.Unlock()
			var err error
			switch c.Kind {
			case "add":
				if !fsproto.PortableName(c.Namespace) || a != nil {
					err = fmt.Errorf("invalid or duplicate attempt namespace")
				} else {
					var remote *remotefs.Attempt
					remote, err = engine.Add(c)
					if err == nil {
						fs.mu.Lock()
						fs.attempts[c.Namespace] = &mountedAttempt{remote: remote, files: map[string]*file{}}
						fs.mu.Unlock()
						reply.Manifest = &remote.Manifest
					}
				}
			case "renew":
				if a != nil {
					a.remote.Renew(c.Deadline)
					err = a.remote.Check()
				}
			case "remove":
				if a != nil {
					fs.mu.Lock()
					delete(fs.attempts, c.Namespace)
					fs.mu.Unlock()
					a.remote.Stop()
				}
			case "seal":
				if a == nil {
					err = fmt.Errorf("unknown attempt")
				} else {
					err = a.remote.Seal()
				}
			case "check":
				if a == nil {
					err = fmt.Errorf("unknown attempt")
				} else {
					err = a.remote.Check()
				}
			case "stats":
			default:
				err = fmt.Errorf("unknown helper operation")
			}
			if err != nil {
				reply.Error = err.Error()
			}
			reply.Stats = engine.Stats()
			responseMu.Lock()
			enc.Encode(reply)
			responseMu.Unlock()
		}(c)
	}
}
func main() {
	if err := run(); err != nil {
		json.NewEncoder(os.Stdout).Encode(fsproto.Reply{Error: err.Error()})
		os.Exit(1)
	}
}
