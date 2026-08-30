package controller

import (
	"context"
	"testing"
	"time"

	"github.com/gwendall/runneryard/provider"
)

func TestReconcileRetiresStoppedWorkersImmediately(t *testing.T) {
	stopped := provider.Worker{ID: "stopped", LeaseID: "lease-stopped", RunnerName: "runner-00000aaa", State: "off", CreatedAt: time.Now().Add(-3 * time.Minute)}
	running := provider.Worker{ID: "running", LeaseID: "lease-running", RunnerName: "runner-00000bbb", State: "running", CreatedAt: time.Now().Add(-3 * time.Minute)}
	compute := &fakeCompute{workers: []provider.Worker{stopped, running}, removeBeforeDestroy: true}
	state := newWorkerState()
	state.add(stopped, true)
	state.add(running, true)
	scaler := testScaler(t, state, compute)
	if _, err := scaler.HandleDesiredRunnerCount(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if len(compute.destroyed) != 1 || compute.destroyed[0] != "stopped" {
		t.Fatalf("only the stopped worker must be retired, destroyed=%v", compute.destroyed)
	}
	if _, ok := state.get("runner-00000bbb"); !ok {
		t.Fatal("the running worker must be kept")
	}
}

func TestWorkerStoppedRecognizesProviderStates(t *testing.T) {
	for _, state := range []string{"stopped", "off", "Failed", " destroyed "} {
		if !workerStopped(state) {
			t.Fatalf("%q must count as stopped", state)
		}
	}
	for _, state := range []string{"", "created", "starting", "started", "running", "stopping"} {
		if workerStopped(state) {
			t.Fatalf("%q must not count as stopped", state)
		}
	}
}
