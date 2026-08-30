package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/actions/scaleset"
	"github.com/gwendall/runneryard/provider"
)

func transient(message string) error {
	return &provider.TransientError{Err: errors.New(message)}
}

func TestDesiredCountSkipsCycleWhenInventoryIsTransientlyUnavailable(t *testing.T) {
	worker := provider.Worker{ID: "worker-one", LeaseID: "lease-one", RunnerName: "runner-00000001"}
	compute := &fakeCompute{inventoryErr: transient("provider listing unavailable")}
	state := newWorkerState()
	state.add(worker, true)
	scaler := testScaler(t, state, compute)
	count, err := scaler.HandleDesiredRunnerCount(context.Background(), 3)
	if err != nil {
		t.Fatalf("a transient inventory failure must not stop the listener: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected the known worker to be preserved, got %d", count)
	}
}

func TestDesiredCountFailsClosedOnPermanentInventoryFailure(t *testing.T) {
	compute := &fakeCompute{inventoryErr: errors.New("token rejected")}
	scaler := testScaler(t, newWorkerState(), compute)
	if _, err := scaler.HandleDesiredRunnerCount(context.Background(), 1); err == nil {
		t.Fatal("a permanent inventory failure must remain fatal")
	}
}

func TestDesiredCountLeavesJobsQueuedWhenLaunchIsTransientlyUnavailable(t *testing.T) {
	compute := &fakeCompute{launchErr: transient("no capacity in region")}
	scaler := testScaler(t, newWorkerState(), compute)
	client := scaler.scaleSetClient.(*fakeRunnerScaleSetClient)
	client.generateJIT = &scaleset.RunnerScaleSetJitRunnerConfig{Runner: &scaleset.RunnerReference{ID: 42}, EncodedJITConfig: "jit"}
	scaler.maxWorkers = 4
	count, err := scaler.HandleDesiredRunnerCount(context.Background(), 2)
	if err != nil {
		t.Fatalf("a transient launch failure must not stop the listener: %v", err)
	}
	if count != 0 {
		t.Fatalf("no worker should be recorded after a failed launch, got %d", count)
	}
	if scaler.retirements.count() != 0 {
		t.Fatalf("the failed launch must leave no pending retirement, got %d", scaler.retirements.count())
	}
	if len(client.removed) != 1 || client.removed[0] != 42 {
		t.Fatalf("the unused GitHub registration must be removed, got %v", client.removed)
	}
}

func TestCompletionKeepsRetirementPendingWhenProviderIsTransientlyUnavailable(t *testing.T) {
	worker := provider.Worker{ID: "worker-one", LeaseID: "lease-one", RunnerName: "runner-00000001", RunnerScaleSetID: 1}
	compute := &fakeCompute{workers: []provider.Worker{worker}, destroyErr: transient("delete timed out")}
	state := newWorkerState()
	state.add(worker, true)
	scaler := testScaler(t, state, compute)
	if err := scaler.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "runner-00000001"}); err != nil {
		t.Fatalf("a transient delete failure must not stop the listener: %v", err)
	}
	if _, ok := state.get("runner-00000001"); !ok {
		t.Fatal("the worker record must be kept until the provider confirms deletion")
	}
	if scaler.retirements.count() != 1 {
		t.Fatalf("the retirement must stay journaled, got %d entries", scaler.retirements.count())
	}

	compute.destroyErr = nil
	compute.removeBeforeDestroy = true
	if _, err := scaler.HandleDesiredRunnerCount(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := state.get("runner-00000001"); ok {
		t.Fatal("reconciliation must finish the pending retirement once the provider recovers")
	}
	if scaler.retirements.count() != 0 || len(compute.destroyed) != 1 {
		t.Fatalf("expected the retirement to complete, pending=%d destroyed=%v", scaler.retirements.count(), compute.destroyed)
	}
}

func TestPacedScaleSetClientPassesThroughWhenDisabled(t *testing.T) {
	inner := &fakeRunnerScaleSetClient{runners: map[string]*scaleset.RunnerReference{}}
	if got := newPacedScaleSetClient(inner, 0, 0); got != inner {
		t.Fatal("a zero rate must return the inner client unchanged")
	}
	paced := newPacedScaleSetClient(inner, 1000, 1000)
	if _, err := paced.GetRunnerByName(context.Background(), "runner-x"); err != nil {
		t.Fatal(err)
	}
}
