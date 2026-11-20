package remotefs

import (
	"context"
	"sync"
)

// scheduler grants slots round-robin across attempts; demanded reads always
// precede speculative ones. No goroutine is needed when the queue is empty.
type scheduler struct {
	mu            sync.Mutex
	limit, active int
	last          string
	queue         []*ticket
}
type ticket struct {
	key                  string
	speculative, granted bool
	ready                chan struct{}
}

func newScheduler(limit int) *scheduler { return &scheduler{limit: limit} }
func (s *scheduler) dispatch() {
	for s.active < s.limit && len(s.queue) > 0 {
		pick := -1
		// Choose the next namespace after the previous winner, wrapping once.
		// Scan demand first; prefetch only competes when no demand is queued.
		for pass := 0; pass < 2 && pick < 0; pass++ {
			after, first := -1, -1
			for i, t := range s.queue {
				if pass == 0 && t.speculative {
					continue
				}
				if first < 0 || t.key < s.queue[first].key {
					first = i
				}
				if t.key > s.last && (after < 0 || t.key < s.queue[after].key) {
					after = i
				}
			}
			pick = after
			if pick < 0 {
				pick = first
			}
		}
		t := s.queue[pick]
		s.queue = append(s.queue[:pick], s.queue[pick+1:]...)
		s.active++
		s.last = t.key
		t.granted = true
		close(t.ready)
	}
}
func (s *scheduler) acquire(ctx context.Context, key string, speculative bool) (func(), error) {
	t := &ticket{key: key, speculative: speculative, ready: make(chan struct{})}
	s.mu.Lock()
	s.queue = append(s.queue, t)
	s.dispatch()
	s.mu.Unlock()
	release := func() { s.mu.Lock(); s.active--; s.dispatch(); s.mu.Unlock() }
	select {
	case <-t.ready:
		return release, nil
	case <-ctx.Done():
		s.mu.Lock()
		if t.granted {
			s.active--
		} else {
			for i, item := range s.queue {
				if item == t {
					s.queue = append(s.queue[:i], s.queue[i+1:]...)
					break
				}
			}
		}
		s.dispatch()
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}
