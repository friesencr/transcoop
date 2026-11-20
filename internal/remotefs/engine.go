// Package remotefs implements the driver-independent filesystem data plane.
package remotefs

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/friesencr/transcoop/internal/fsproto"
)

type diskEntry struct {
	key, path, sum string
	size           int64
}
type cacheEntry struct {
	key  string
	data []byte
}
type flight struct {
	done chan struct{}
	data []byte
	err  error
}
type Engine struct {
	disk                                           map[string]*list.Element
	diskLRU                                        *list.List
	diskSize                                       int64
	demanded                                       atomic.Int64
	Settings                                       fsproto.Settings
	URL, Credential                                string
	HTTP                                           *http.Client
	mu                                             sync.Mutex
	cache                                          map[string]*list.Element
	lru                                            *list.List
	flights                                        map[string]*flight
	cacheSize                                      int64
	reads, writes                                  *scheduler
	dirty, readBytes, writeBytes, retries, waiting atomic.Int64
}
type Output struct{ Size int64 }
type Attempt struct {
	ioMu             sync.Mutex
	forwardMu        sync.Mutex
	lastNode         string
	lastEnd          int64
	forward          int
	prefetchCancel   context.CancelFunc
	Engine           *Engine
	ID               int64
	Token, Namespace string
	Manifest         fsproto.Manifest
	Context          context.Context
	cancel           context.CancelFunc
	deadline         atomic.Int64
	mu               sync.Mutex
	Outputs          map[string]Output
	sequence         int64
	queue            []journal
	wake             chan struct{}
	done             chan struct{}
	err              error
	sealed           bool
}
type journal struct {
	dataBytes int64
	path      string
	bytes     int64
}

func New(settings fsproto.Settings, url, credential string) *Engine {
	return &Engine{Settings: settings, URL: strings.TrimRight(url, "/"), Credential: credential, HTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, cache: map[string]*list.Element{}, disk: map[string]*list.Element{}, diskLRU: list.New(), lru: list.New(), flights: map[string]*flight{}, reads: newScheduler(settings.Reads), writes: newScheduler(settings.Writes)}
}
func (e *Engine) Stats() fsproto.Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	return fsproto.Stats{Mounted: true, ReadBytes: e.readBytes.Load(), WriteBytes: e.writeBytes.Load(), CacheBytes: e.cacheSize + e.diskSize, DirtyBytes: e.dirty.Load(), Retries: e.retries.Load(), Waiting: e.waiting.Load()}
}
func (e *Engine) Add(c fsproto.Control) (*Attempt, error) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &Attempt{Engine: e, ID: c.JobID, Token: c.Attempt, Namespace: c.Namespace, Context: ctx, cancel: cancel, Outputs: map[string]Output{}, wake: make(chan struct{}, 1), done: make(chan struct{})}
	a.deadline.Store(c.Deadline)
	b, err := a.request("GET", "manifest", nil, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	if err = json.Unmarshal(b, &a.Manifest); err != nil {
		cancel()
		return nil, err
	}
	if a.Manifest.Version != 1 {
		cancel()
		return nil, errors.New("unsupported filesystem manifest version")
	}
	go a.stream()
	return a, nil
}
func (a *Attempt) Renew(deadline int64) { a.deadline.Store(deadline) }
func (a *Attempt) Stop() {
	a.cancel()
	<-a.done
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, j := range a.queue {
		os.Remove(j.path)
		a.Engine.dirty.Add(-j.bytes)
	}
	a.queue = nil
}
func (a *Attempt) valid() error {
	if a.Context.Err() != nil {
		return a.Context.Err()
	}
	if time.Now().Unix() >= a.deadline.Load() {
		a.cancel()
		return errors.New("filesystem lease expired")
	}
	return nil
}
func (a *Attempt) request(method, action string, body []byte, sem *scheduler) ([]byte, error) {
	return a.requestContext(a.Context, method, action, body, sem, false)
}
func (a *Attempt) requestContext(parent context.Context, method, action string, body []byte, sem *scheduler, speculative bool) ([]byte, error) {
	var waitMu sync.Mutex
	finished, reported := false, false
	waitTimer := time.AfterFunc(2*time.Second, func() {
		waitMu.Lock()
		defer waitMu.Unlock()
		if !finished {
			reported = true
			a.Engine.waiting.Add(1)
		}
	})
	defer func() {
		waitTimer.Stop()
		waitMu.Lock()
		finished = true
		if reported {
			a.Engine.waiting.Add(-1)
		}
		waitMu.Unlock()
	}()

	if sem != nil {
		release, err := sem.acquire(parent, a.Namespace, speculative)
		if err != nil {
			return nil, err
		}
		defer release()
	}

	started := time.Now()
	delay := 100 * time.Millisecond
	for {
		if err := a.valid(); err != nil {
			return nil, err
		}
		if err := parent.Err(); err != nil {
			return nil, err
		}
		deadline := time.Unix(a.deadline.Load(), 0)
		limit := started.Add(45 * time.Second)
		if deadline.Before(limit) {
			limit = deadline
		}
		ctx, cancel := context.WithDeadline(parent, limit)
		req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("%s/api/jobs/%d/fs/%s", a.Engine.URL, a.ID, action), strings.NewReader(string(body)))
		if err != nil {
			cancel()
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+a.Engine.Credential)
		req.Header.Set("X-Attempt-Token", a.Token)
		res, err := a.Engine.HTTP.Do(req)
		if err == nil {
			data, e := io.ReadAll(io.LimitReader(res.Body, 8<<20))
			res.Body.Close()
			if e == nil && res.StatusCode >= 200 && res.StatusCode < 300 {
				if sum := res.Header.Get("X-Content-SHA256"); strings.HasPrefix(action, "read?") && sum != fsproto.Digest(data) {
					cancel()
					return nil, errors.New("filesystem block checksum mismatch")
				}
				cancel()
				return data, nil
			}
			err = e
			if err == nil {
				err = fmt.Errorf("filesystem HTTP %d: %s", res.StatusCode, string(data))
			}
			if res.StatusCode < 500 && res.StatusCode != 429 {
				cancel()
				return nil, err
			}
		}
		cancel()
		if time.Now().Add(delay).After(limit) {
			return nil, err
		}
		a.Engine.retries.Add(1)
		a.Engine.waiting.Add(1)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-a.Context.Done():
			timer.Stop()
			a.Engine.waiting.Add(-1)
			return nil, a.Context.Err()
		}
		a.Engine.waiting.Add(-1)
		delay = min(delay*2, 2*time.Second)
	}
}
func (a *Attempt) ReadInput(n fsproto.Node, p []byte, offset int64) (int, error) {
	a.Engine.demanded.Add(1)
	defer a.Engine.demanded.Add(-1)
	initial := offset
	defer func() {
		a.forwardMu.Lock()
		defer a.forwardMu.Unlock()
		if a.lastNode == n.ID && initial == a.lastEnd {
			a.forward++
		} else {
			a.forward = 0
			if a.prefetchCancel != nil {
				a.prefetchCancel()
			}
		}
		a.lastNode = n.ID
		a.lastEnd = offset
		if a.forward == 2 {
			ctx, cancel := context.WithCancel(a.Context)
			a.prefetchCancel = cancel
			go func(start int64) {
				for i := int64(0); i < 8; i++ {
					if ctx.Err() != nil {
						return
					}
					block := start + i
					if block*fsproto.BlockSize >= n.Size {
						return
					}
					for a.Engine.demanded.Load() > 0 {
						select {
						case <-ctx.Done():
							return
						case <-time.After(5 * time.Millisecond):
						}
					}
					if _, err := a.blockContext(ctx, n, block, true); err != nil {
						return
					}
				}
			}(offset/fsproto.BlockSize + 1)
			a.forward = 0
		}
	}()

	if err := a.valid(); err != nil {
		return 0, err
	}
	if offset < 0 {
		return 0, errors.New("negative read offset")
	}
	total := 0
	for len(p) > 0 && offset < n.Size {
		block := offset / fsproto.BlockSize
		data, err := a.block(n, block)
		if err != nil {
			a.cancel()
			return total, err
		}
		start := offset % fsproto.BlockSize
		if start >= int64(len(data)) {
			return total, io.ErrUnexpectedEOF
		}
		count := copy(p, data[start:])
		p = p[count:]
		offset += int64(count)
		total += count
	}
	return total, nil
}
func (a *Attempt) block(n fsproto.Node, block int64) ([]byte, error) {
	return a.blockContext(a.Context, n, block, false)
}
func (a *Attempt) blockContext(ctx context.Context, n fsproto.Node, block int64, speculative bool) ([]byte, error) {
	e := a.Engine
	key := fmt.Sprintf("%s/%s/%d", a.Namespace, n.ID, block)
	e.mu.Lock()
	if el := e.cache[key]; el != nil {
		e.lru.MoveToFront(el)
		b := el.Value.(cacheEntry).data
		e.mu.Unlock()
		return b, nil
	}
	if f := e.flights[key]; f != nil {
		e.mu.Unlock()
		select {
		case <-f.done:
			if errors.Is(f.err, context.Canceled) && !speculative && a.Context.Err() == nil {
				return a.block(n, block)
			}
			return f.data, f.err
		case <-a.Context.Done():
			return nil, a.Context.Err()
		}
	}
	if el := e.disk[key]; el != nil {
		entry := el.Value.(diskEntry)
		b, err := os.ReadFile(entry.path)
		if err == nil && fsproto.Digest(b) == entry.sum {
			e.diskLRU.MoveToFront(el)
			e.mu.Unlock()
			return b, nil
		}
		os.Remove(entry.path)
		delete(e.disk, key)
		e.diskLRU.Remove(el)
		e.diskSize -= entry.size
	}
	f := &flight{done: make(chan struct{})}
	e.flights[key] = f
	e.mu.Unlock()
	b, err := a.requestContext(ctx, "GET", fmt.Sprintf("read?node=%s&offset=%d&size=%d", n.ID, block*fsproto.BlockSize, fsproto.BlockSize), nil, e.reads, speculative)
	if err == nil && len(b) != int(min(int64(fsproto.BlockSize), n.Size-block*fsproto.BlockSize)) {
		err = io.ErrUnexpectedEOF
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err == nil {
		e.readBytes.Add(int64(len(b)))
		budget := e.Settings.DiskBytes - e.Settings.DirtyBytes
		if int64(len(b)) <= budget {
			for e.diskSize+int64(len(b)) > budget && e.diskLRU.Len() > 0 {
				last := e.diskLRU.Back()
				entry := last.Value.(diskEntry)
				os.Remove(entry.path)
				delete(e.disk, entry.key)
				e.diskSize -= entry.size
				e.diskLRU.Remove(last)
			}
			file, writeErr := os.CreateTemp(e.Settings.CacheDir, "clean-*")
			if writeErr == nil {
				_, writeErr = file.Write(b)
				ce := file.Close()
				if writeErr == nil {
					writeErr = ce
				}
				if writeErr == nil {
					entry := diskEntry{key: key, path: file.Name(), sum: fsproto.Digest(b), size: int64(len(b))}
					e.disk[key] = e.diskLRU.PushFront(entry)
					e.diskSize += entry.size
				} else {
					os.Remove(file.Name())
				}
			}
			if writeErr != nil {
				err = fmt.Errorf("filesystem clean cache: %w", writeErr)
			}
		}

		for e.cacheSize+int64(len(b)) > e.Settings.RAMBytes && e.lru.Len() > 0 {
			last := e.lru.Back()
			entry := last.Value.(cacheEntry)
			delete(e.cache, entry.key)
			e.cacheSize -= int64(len(entry.data))
			e.lru.Remove(last)
		}
		if err == nil {
			e.cache[key] = e.lru.PushFront(cacheEntry{key, b})
			e.cacheSize += int64(len(b))
		}
	}
	f.data, f.err = b, err
	delete(e.flights, key)
	close(f.done)
	return b, err
}

// Mutate persists a bounded journal record before acknowledging to the kernel.
// Output metadata is maintained by the adapter while holding the attempt lock.
func (a *Attempt) Mutate(kind, name, target string, offset int64, data []byte) error {
	if len(data) > fsproto.MaxWrite {
		return errors.New("write exceeds maximum operation size")
	}
	cost := int64((len(data)*4+2)/3 + 4096)
	for {
		if err := a.valid(); err != nil {
			return err
		}
		current := a.Engine.dirty.Load()
		if current+cost <= a.Engine.Settings.DirtyBytes && a.Engine.dirty.CompareAndSwap(current, current+cost) {
			break
		}
		a.Engine.waiting.Add(1)
		select {
		case <-time.After(10 * time.Millisecond):
		case <-a.Context.Done():
			a.Engine.waiting.Add(-1)
			return a.Context.Err()
		}
		a.Engine.waiting.Add(-1)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil || a.sealed {
		a.Engine.dirty.Add(-cost)
		if a.err != nil {
			return a.err
		}
		return errors.New("output sealed")
	}
	op := fsproto.Operation{Sequence: a.sequence + 1, Kind: kind, Name: name, Target: target, Offset: offset, Data: data}
	op.Digest = op.Hash()
	if err := op.Validate(); err != nil {
		a.Engine.dirty.Add(-cost)
		return err
	}
	b, _ := json.Marshal(op)
	f, err := os.CreateTemp(a.Engine.Settings.CacheDir, "dirty-*")
	if err == nil {
		_, err = f.Write(b)
		if err == nil {
			err = f.Sync()
		}
		ce := f.Close()
		if err == nil {
			err = ce
		}
		if err == nil {
			err = syncJournalDirectory(a.Engine.Settings.CacheDir)
		}
		if err != nil {
			os.Remove(f.Name())
		}
	}
	if err != nil {
		a.Engine.dirty.Add(-cost)
		a.err = fmt.Errorf("filesystem journal: %w", err)
		return a.err
	}
	a.sequence++
	a.queue = append(a.queue, journal{path: f.Name(), bytes: cost, dataBytes: int64(len(data))})
	if kind == "seal" {
		a.sealed = true
	}
	select {
	case a.wake <- struct{}{}:
	default:
	}
	return nil
}
func (a *Attempt) stream() {
	defer close(a.done)
	for {
		select {
		case <-a.Context.Done():
			return
		case <-a.wake:
		}
		deadline := time.NewTimer(250 * time.Millisecond)
		collecting := true
		for collecting {
			a.mu.Lock()
			size := int64(0)
			for _, j := range a.queue {
				size += j.dataBytes
			}
			a.mu.Unlock()
			if size >= fsproto.MaxWrite {
				break
			}
			select {
			case <-a.Context.Done():
				deadline.Stop()
				return
			case <-deadline.C:
				collecting = false
			case <-a.wake:
			}
		}
		deadline.Stop()
		for {
			a.mu.Lock()
			pending := append([]journal(nil), a.queue...)
			a.mu.Unlock()
			if len(pending) == 0 {
				break
			}
			operations := []fsproto.Operation{}
			size := 0
			encoded := 0
			count := 0
			var err error
			for _, j := range pending {
				var b []byte
				b, err = os.ReadFile(j.path)
				if err != nil {
					break
				}
				var op fsproto.Operation
				err = json.Unmarshal(b, &op)
				if err != nil {
					break
				}
				if size+len(op.Data) > fsproto.MaxWrite || count >= 4096 || encoded+len(b) > 6<<20-1024 {
					break
				}
				size += len(op.Data)
				encoded += len(b)
				operations = append(operations, op)
				count++
			}
			a.ioMu.Lock()
			if err == nil {
				body, e := json.Marshal(struct {
					Operations []fsproto.Operation `json:"operations"`
				}{operations})
				err = e
				if err == nil {
					_, err = a.request("POST", "batch", body, a.Engine.writes)
				}
			}
			a.mu.Lock()
			if err != nil {
				a.err = err
				a.mu.Unlock()
				a.ioMu.Unlock()
				a.cancel()
				return
			}
			a.queue = a.queue[count:]
			a.mu.Unlock()
			for _, j := range pending[:count] {
				os.Remove(j.path)
				a.Engine.dirty.Add(-j.bytes)
			}
			a.Engine.writeBytes.Add(int64(size))
			a.ioMu.Unlock()
		}
	}
}
func (a *Attempt) Flush() error {
	for {
		a.mu.Lock()
		err := a.err
		empty := len(a.queue) == 0
		a.mu.Unlock()
		if err != nil {
			return err
		}
		if empty {
			return a.valid()
		}
		select {
		case <-a.Context.Done():
			return a.Context.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}
func (a *Attempt) ReadOutput(name string, p []byte, offset int64) (int, error) {
	if err := a.Flush(); err != nil {
		return 0, err
	}
	total := 0
	for len(p) > 0 {
		count := min(len(p), fsproto.BlockSize)
		b, err := a.request("GET", fmt.Sprintf("read?output=%s&offset=%d&size=%d", url.QueryEscape(name), offset, count), nil, a.Engine.reads)
		if err != nil {
			return total, err
		}
		n := copy(p, b)
		total += n
		offset += int64(n)
		p = p[n:]
		if n < count {
			break
		}
	}
	return total, nil
}
func (a *Attempt) Seal() error {
	if err := a.Mutate("seal", a.Manifest.Output, "", 0, nil); err != nil {
		return err
	}
	return a.Flush()
}
func EnsureDirectories(s fsproto.Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if !filepath.IsAbs(s.MountDir) || !filepath.IsAbs(s.CacheDir) || s.MountDir == s.CacheDir {
		return errors.New("mount and cache directories must be distinct absolute paths")
	}
	for _, p := range []string{s.MountDir, s.CacheDir} {
		if p == s.MountDir && fsproto.MountPointCreatedByDriver {
			p = filepath.Dir(p)
		}
		if err := os.MkdirAll(p, 0700); err != nil {
			return err
		}
	}
	return nil
}

func (a *Attempt) Cancel() { a.cancel() }
func (a *Attempt) Check() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	return a.valid()
}

// ReadOutputSize overlays journal records on a stable acknowledged remote
// prefix. The adapter supplies its current file size while holding its inode
// lock. Only one journal payload is decoded at a time, keeping reads bounded.
func (a *Attempt) ReadOutputSize(name string, p []byte, offset, size int64) (int, error) {
	if err := a.valid(); err != nil {
		return 0, err
	}
	if offset < 0 {
		return 0, errors.New("negative read offset")
	}
	if offset >= size {
		return 0, nil
	}
	p = p[:min(int64(len(p)), size-offset)]
	a.ioMu.Lock()
	defer a.ioMu.Unlock()
	a.mu.Lock()
	pending := append([]journal(nil), a.queue...)
	failure := a.err
	a.mu.Unlock()
	if failure != nil {
		return 0, failure
	}
	remoteName := name
	created := false
	relevant := []string{}
	load := func(path string) (fsproto.Operation, error) {
		var op fsproto.Operation
		b, err := os.ReadFile(path)
		if err == nil {
			err = json.Unmarshal(b, &op)
		}
		return op, err
	}
	for i := len(pending) - 1; i >= 0; i-- {
		op, err := load(pending[i].path)
		if err != nil {
			return 0, err
		}
		if op.Kind == "rename" && op.Target == remoteName {
			remoteName = op.Name
			continue
		}
		if op.Name != remoteName {
			continue
		}
		if op.Kind == "create" {
			created = true
			break
		}
		if op.Kind == "write" || op.Kind == "truncate" {
			relevant = append(relevant, pending[i].path)
		}
	}
	clear(p)
	if !created {
		for start := 0; start < len(p); {
			count := min(len(p)-start, fsproto.BlockSize)
			b, err := a.request("GET", fmt.Sprintf("read?output=%s&offset=%d&size=%d", url.QueryEscape(remoteName), offset+int64(start), count), nil, a.Engine.reads)
			if err != nil {
				return 0, err
			}
			copy(p[start:], b)
			start += count
			if len(b) < count {
				break
			}
		}
	}
	for i := len(relevant) - 1; i >= 0; i-- {
		op, err := load(relevant[i])
		if err != nil {
			return 0, err
		}
		if op.Kind == "truncate" {
			start := max(int64(0), op.Offset-offset)
			if start < int64(len(p)) {
				clear(p[start:])
			}
		} else {
			start := max(offset, op.Offset)
			end := min(offset+int64(len(p)), op.Offset+int64(len(op.Data)))
			if end > start {
				copy(p[start-offset:end-offset], op.Data[start-op.Offset:end-op.Offset])
			}
		}
	}
	return len(p), nil
}
