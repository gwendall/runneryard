package controller

import (
	"context"
	"testing"
	"time"

	"github.com/gwendall/runneryard/provider"
)

func TestReconcileReleasesIdleWorkerAfterDanglingTimeout(t *testing.T) {
	old := time.Now().Add(-30 * time.Minute)
	idle := provider.Worker{ID: "idle", LeaseID: "lease-idle", RunnerName: "runner-0000aaaa", CreatedAt: old}
	busy := provider.Worker{ID: "busy", LeaseID: "lease-busy", RunnerName: "runner-0000bbbb", CreatedAt: old}
	fresh := provider.Worker{ID: "fresh", LeaseID: "lease-fresh", RunnerName: "runner-0000cccc", CreatedAt: time.Now().Add(-2 * time.Minute)}
	compute := &fakeCompute{workers: []provider.Worker{idle, busy, fresh}, removeBeforeDestroy: true}
	state := newWorkerState()
	state.add(idle, false)
	state.add(busy, true)
	state.add(fresh, false)
	scaler := testScaler(t, state, compute)
	scaler.danglingTimeout = 25 * time.Minute
	if _, err := scaler.HandleDesiredRunnerCount(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if len(compute.destroyed) != 1 || compute.destroyed[0] != "idle" {
		t.Fatalf("only the old idle worker must be released, destroyed=%v", compute.destroyed)
	}
	if _, ok := state.get("runner-0000aaaa"); ok {
		t.Fatal("released worker must leave local state")
	}
	for _, name := range []string{"runner-0000bbbb", "runner-0000cccc"} {
		if _, ok := state.get(name); !ok {
			t.Fatalf("%s must be kept", name)
		}
	}
}

func TestReconcileKeepsAdoptedWorkersUntilObserved(t *testing.T) {
	adopted := provider.Worker{ID: "adopted", LeaseID: "lease-adopted", RunnerName: "runner-0000dddd", CreatedAt: time.Now().Add(-40 * time.Minute)}
	compute := &fakeCompute{workers: []provider.Worker{adopted}}
	state := newWorkerState()
	state.adopt(adopted)
	scaler := testScaler(t, state, compute)
	scaler.danglingTimeout = 25 * time.Minute
	if _, err := scaler.HandleDesiredRunnerCount(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if len(compute.destroyed) != 0 {
		t.Fatalf("a worker adopted after restart may be mid-job; it must not be released, destroyed=%v", compute.destroyed)
	}
}

func TestLaunchPassesIdleTimeoutToTheWorker(t *testing.T) {
	compute := &slowCompute{delay: time.Millisecond}
	scaler := concurrentScaler(t, compute, 1, 2)
	scaler.idleTimeout = 7 * time.Minute
	if _, err := scaler.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if compute.lastLease.IdleTimeout != 7*time.Minute {
		t.Fatalf("lease idle timeout = %s, want 7m", compute.lastLease.IdleTimeout)
	}
}
