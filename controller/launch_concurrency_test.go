package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/gwendall/runneryard/provider"
)

// slowCompute records how many launches overlap so the test can prove that
// launches run concurrently but never beyond the configured bound.
type slowCompute struct {
	mu        sync.Mutex
	delay     time.Duration
	inFlight  int
	peak      int
	launched  int
	failAfter int
	failWith  error
	workers   []provider.Worker
	lastLease provider.Lease
	releaseAt int
	release   chan struct{}
	releaseMu sync.Once
}

func (c *slowCompute) Launch(ctx context.Context, lease provider.Lease) (provider.Worker, error) {
	c.mu.Lock()
	c.launched++
	c.lastLease = lease
	n := c.launched
	c.inFlight++
	if c.inFlight > c.peak {
		c.peak = c.inFlight
	}
	release := c.release
	shouldRelease := release != nil && c.inFlight >= c.releaseAt
	c.mu.Unlock()
	if shouldRelease {
		c.releaseMu.Do(func() { close(release) })
	}
	var waitErr error
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			waitErr = ctx.Err()
		}
	} else if c.delay > 0 {
		timer := time.NewTimer(c.delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			waitErr = ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight--
	if waitErr != nil {
		return provider.Worker{}, waitErr
	}
	if c.failAfter > 0 && n > c.failAfter {
		return provider.Worker{}, c.failWith
	}
	worker := provider.Worker{ID: fmt.Sprintf("worker-%d", n), LeaseID: lease.ID, RunnerName: lease.RunnerName, RunnerID: lease.RunnerID, RunnerScaleSetID: lease.RunnerScaleSetID, CreatedAt: time.Now()}
	c.workers = append(c.workers, worker)
	return worker, nil
}

func (c *slowCompute) Inventory(context.Context) ([]provider.Worker, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]provider.Worker(nil), c.workers...), nil
}

func (c *slowCompute) Destroy(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	remaining := c.workers[:0]
	for _, worker := range c.workers {
		if worker.ID != id {
			remaining = append(remaining, worker)
		}
	}
	c.workers = remaining
	return nil
}

func concurrentScaler(t *testing.T, compute provider.Compute, concurrency, maxWorkers int) *scaler {
	t.Helper()
	scaler := testScaler(t, newWorkerState(), compute)
	scaler.launchConcurrency = concurrency
	scaler.maxWorkers = maxWorkers
	scaler.scaleSetClient.(*fakeRunnerScaleSetClient).generateJIT = &scaleset.RunnerScaleSetJitRunnerConfig{
		Runner: &scaleset.RunnerReference{ID: 7}, EncodedJITConfig: "jit",
	}
	return scaler
}

func TestLaunchesOverlapUpToTheConfiguredBound(t *testing.T) {
	compute := &slowCompute{releaseAt: 3, release: make(chan struct{})}
	scaler := concurrentScaler(t, compute, 3, 12)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	count, err := scaler.HandleDesiredRunnerCount(ctx, 6)
	if err != nil {
		t.Fatal(err)
	}
	if count != 6 {
		t.Fatalf("expected 6 workers, got %d", count)
	}
	// The first wave is held until all three slots reach the provider. This
	// proves exact overlap without depending on scheduler or filesystem timing;
	// the context fails promptly if launches ever become serial again.
	if compute.peak != 3 {
		t.Fatalf("expected launches to reach the bound of 3, observed peak %d", compute.peak)
	}
}

func TestLaunchConcurrencyDefaultsToSerialWhenUnset(t *testing.T) {
	compute := &slowCompute{delay: 5 * time.Millisecond}
	scaler := concurrentScaler(t, compute, 0, 4)
	if _, err := scaler.HandleDesiredRunnerCount(context.Background(), 4); err != nil {
		t.Fatal(err)
	}
	if compute.peak != 1 {
		t.Fatalf("an unset bound must keep launches serial, observed peak %d", compute.peak)
	}
}

func TestPermanentLaunchFailureStopsFurtherLaunchesAndGatesTheNextMessage(t *testing.T) {
	// Before 0.4.4 the permanent error surfaced and stopped the controller.
	// It is now a bounded launch rejection: the burst stops, the listener
	// stays, and the next desired-count message inside the backoff launches
	// nothing.
	compute := &slowCompute{delay: 5 * time.Millisecond, failAfter: 2, failWith: errors.New("image rejected")}
	scaler := concurrentScaler(t, compute, 2, 8)
	if _, err := scaler.HandleDesiredRunnerCount(context.Background(), 8); err != nil {
		t.Fatalf("a permanent launch rejection must not stop the listener, got %v", err)
	}
	if compute.launched >= 8 {
		t.Fatalf("launches must stop after a permanent failure, got %d attempts", compute.launched)
	}
	launched := compute.launched
	if _, err := scaler.HandleDesiredRunnerCount(context.Background(), 8); err != nil {
		t.Fatal(err)
	}
	if compute.launched != launched {
		t.Fatalf("launches inside the rejection backoff must not run: %d then %d attempts", launched, compute.launched)
	}
}

func TestTransientLaunchFailureStopsQuietly(t *testing.T) {
	compute := &slowCompute{delay: 5 * time.Millisecond, failAfter: 1, failWith: &provider.TransientError{Err: errors.New("no capacity")}}
	scaler := concurrentScaler(t, compute, 2, 8)
	count, err := scaler.HandleDesiredRunnerCount(context.Background(), 8)
	if err != nil {
		t.Fatalf("a transient launch failure must not stop the listener: %v", err)
	}
	if count < 1 || compute.launched >= 8 {
		t.Fatalf("expected the successful launch to be kept and the burst to stop, count=%d attempts=%d", count, compute.launched)
	}
}
