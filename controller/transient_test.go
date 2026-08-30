package controller

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

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

func TestCapacityRejectionBacksOffWhileCompletionsAndReplacementsContinue(t *testing.T) {
	clock := time.Now().UTC()
	worker := provider.Worker{
		ID: "worker-one", LeaseID: "lease-one", RunnerName: "runner-00000000-0000-0000-0000-000000000001",
		RunnerID: 7, RunnerScaleSetID: 1, CreatedAt: clock.Add(-time.Minute),
	}
	capacityErr := &provider.CapacityError{Reason: "fly_machine_limit", Err: errors.New("machine quota reached")}
	compute := &fakeCompute{workers: []provider.Worker{worker}, launchErr: capacityErr, removeBeforeDestroy: true}
	state := newWorkerState()
	state.add(worker, true)
	scaler := testScaler(t, state, compute)
	scaler.maxWorkers = 3
	scaler.capacityNow = func() time.Time { return clock }
	statusFile := filepath.Join(t.TempDir(), "status.json")
	reporter, err := newStatusReporter(Config{
		ControllerID: "test", ScaleSetName: "test", Provider: "fly", MaxWorkers: 3,
		StatusFile: statusFile, Logger: slog.New(slog.DiscardHandler),
	}, BudgetStatus{})
	if err != nil {
		t.Fatal(err)
	}
	reporter.githubActivity("session_created")
	scaler.reporter = reporter
	client := scaler.scaleSetClient.(*fakeRunnerScaleSetClient)
	client.runners[worker.RunnerName] = &scaleset.RunnerReference{ID: 7, Name: worker.RunnerName, RunnerScaleSetID: 1}
	client.generateJIT = &scaleset.RunnerScaleSetJitRunnerConfig{
		Runner: &scaleset.RunnerReference{ID: 42}, EncodedJITConfig: "jit",
	}

	count, err := scaler.HandleDesiredRunnerCount(context.Background(), 3)
	if err != nil || count != 1 {
		t.Fatalf("a capacity rejection must preserve the listener and existing worker: count=%d err=%v", count, err)
	}
	status := loadFleetStatus(t, statusFile)
	if status.Health != "degraded" || status.Reason != "provider_capacity_exhausted" ||
		status.Capacity.Configured != 3 || status.Capacity.Effective != 1 ||
		status.Capacity.Rejections != 1 || status.Capacity.Rejection != "fly_machine_limit" ||
		!status.Capacity.RetryAt.Equal(clock.Add(capacityInitialBackoff)) {
		t.Fatalf("capacity status = %#v", status)
	}
	if budget := scaler.budget.snapshot(time.Now()); budget.ReservedSeconds != 0 {
		t.Fatalf("a definite quota rejection must refund its launch reservation: %#v", budget)
	}

	if _, err := scaler.HandleDesiredRunnerCount(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if compute.launchCalls != 1 {
		t.Fatalf("desired-count messages inside the backoff must not hot-loop launches: %d", compute.launchCalls)
	}
	clock = clock.Add(capacityInitialBackoff + time.Second)
	if _, err := scaler.HandleDesiredRunnerCount(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	status = loadFleetStatus(t, statusFile)
	if compute.launchCalls != 2 || status.Capacity.Rejections != 2 ||
		!status.Capacity.RetryAt.Equal(clock.Add(2*capacityInitialBackoff)) {
		t.Fatalf("a rejected probe must double the bounded backoff: calls=%d status=%#v", compute.launchCalls, status)
	}

	if err := scaler.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: worker.RunnerName}); err != nil {
		t.Fatalf("normal completion must continue during a capacity incident: %v", err)
	}
	if status := loadFleetStatus(t, statusFile); status.Reason != "provider_capacity_exhausted" {
		t.Fatalf("a completion must not manufacture capacity recovery: %#v", status)
	}

	compute.launchErr = nil
	compute.launchWorker = provider.Worker{ID: "replacement-one", CreatedAt: clock}
	if count, err = scaler.HandleDesiredRunnerCount(context.Background(), 1); err != nil || count != 1 {
		t.Fatalf("capacity below the proven ceiling must remain serviceable: count=%d err=%v", count, err)
	}
	compute.launchWorker = provider.Worker{ID: "replacement-two", CreatedAt: clock}
	clock = clock.Add(2*capacityInitialBackoff + time.Second)
	if count, err = scaler.HandleDesiredRunnerCount(context.Background(), 2); err != nil || count != 2 {
		t.Fatalf("the bounded capacity probe must recover after quota changes: count=%d err=%v", count, err)
	}
	status = loadFleetStatus(t, statusFile)
	if status.Health != "ready" || status.Capacity.Effective != 3 || status.Capacity.Rejection != "" || status.Capacity.Rejections != 2 {
		t.Fatalf("capacity recovery status = %#v", status)
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
