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
}

func (c *slowCompute) Launch(_ context.Context, lease provider.Lease) (provider.Worker, error) {
	c.mu.Lock()
	c.launched++
	c.lastLease = lease
	n := c.launched
	c.inFlight++
	if c.inFlight > c.peak {
		c.peak = c.inFlight
	}
	c.mu.Unlock()
	time.Sleep(c.delay)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight--
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
	compute := &slowCompute{delay: 40 * time.Millisecond}
	scaler := concurrentScaler(t, compute, 3, 12)
	count, err := scaler.HandleDesiredRunnerCount(context.Background(), 6)
	if err != nil {
		t.Fatal(err)
	}
	if count != 6 {
		t.Fatalf("expected 6 workers, got %d", count)
	}
	// Journal and ledger writes stay serialized (they fsync), so wall-clock
	// time is not a reliable signal; the observed peak proves both the overlap
	// and the bound.
	if compute.peak < 2 || compute.peak > 3 {
		t.Fatalf("expected launches to overlap within the bound of 3, observed peak %d", compute.peak)
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

func TestPermanentLaunchFailureStopsFurtherLaunchesAndSurfaces(t *testing.T) {
	compute := &slowCompute{delay: 5 * time.Millisecond, failAfter: 2, failWith: errors.New("image rejected")}
	scaler := concurrentScaler(t, compute, 2, 8)
	_, err := scaler.HandleDesiredRunnerCount(context.Background(), 8)
	if err == nil || !errors.Is(err, compute.failWith) {
		t.Fatalf("expected the permanent launch error to surface, got %v", err)
	}
	if compute.launched >= 8 {
		t.Fatalf("launches must stop after a permanent failure, got %d attempts", compute.launched)
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
