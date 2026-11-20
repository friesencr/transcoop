package remotefs

import (
	"context"
	"testing"
	"time"
)

func TestSchedulerFairDemandBeforePrefetch(t *testing.T) {
	s := newScheduler(1)
	release, err := s.acquire(context.Background(), "a", false)
	if err != nil {
		t.Fatal(err)
	}
	// Queue deterministically to exercise priority and the attempt rotation.
	prefetch := &ticket{key: "b", speculative: true, ready: make(chan struct{})}
	same := &ticket{key: "a", ready: make(chan struct{})}
	other := &ticket{key: "c", ready: make(chan struct{})}
	s.mu.Lock()
	s.queue = append(s.queue, prefetch, same, other)
	s.mu.Unlock()
	release()
	select {
	case <-other.ready:
	default:
		t.Fatal("another attempt's demand was not scheduled first")
	}
	select {
	case <-prefetch.ready:
		t.Fatal("prefetch outranked demand")
	default:
	}
	s.mu.Lock()
	s.active--
	s.dispatch()
	s.mu.Unlock()
	select {
	case <-same.ready:
	default:
		t.Fatal("demand did not precede prefetch")
	}
}
func TestSchedulerCancellationDoesNotLeakSlots(t *testing.T) {
	s := newScheduler(1)
	release, _ := s.acquire(context.Background(), "a", false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := s.acquire(ctx, "b", false); err == nil {
		t.Fatal("canceled request acquired slot")
	}
	release()
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	release, err := s.acquire(ctx2, "c", false)
	if err != nil {
		t.Fatal(err)
	}
	release()
}
