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

type fakeCompute struct {
	workers    []provider.Worker
	destroyErr error
	destroyed  []string
}

func (f *fakeCompute) Launch(_ context.Context, _ provider.Lease) (provider.Worker, error) {
	return provider.Worker{}, errors.New("not implemented in this test")
}

func (f *fakeCompute) Inventory(_ context.Context) ([]provider.Worker, error) {
	return append([]provider.Worker(nil), f.workers...), nil
}

func (f *fakeCompute) Destroy(_ context.Context, id string) error {
	if f.destroyErr != nil {
		return f.destroyErr
	}
	f.destroyed = append(f.destroyed, id)
	return nil
}

func TestReconcileRemovesWorkerThatNoLongerExists(t *testing.T) {
	compute := &fakeCompute{}
	state := newWorkerState()
	state.add(provider.Worker{ID: "missing", RunnerName: "runner-one"}, true)
	scaler := testScaler(t, state, compute)
	if err := scaler.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state.count() != 0 {
		t.Fatal("missing worker was not removed")
	}
}

func TestReconcileAdoptsManagedWorkerMissingFromLocalState(t *testing.T) {
	compute := &fakeCompute{workers: []provider.Worker{{
		ID: "orphan", LeaseID: "lease-orphan", RunnerName: "runner-orphan", CreatedAt: time.Now(),
	}}}
	state := newWorkerState()
	scaler := testScaler(t, state, compute)
	if err := scaler.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	record, ok := state.get("runner-orphan")
	if !ok || record.Worker.ID != "orphan" || !record.Busy {
		t.Fatalf("worker was not conservatively adopted: %#v, %v", record, ok)
	}
}

func TestReconcileDestroysExpiredWorker(t *testing.T) {
	compute := &fakeCompute{workers: []provider.Worker{{
		ID: "expired", LeaseID: "lease-expired", RunnerName: "runner-expired", CreatedAt: time.Now().Add(-3 * time.Hour),
	}}}
	state := newWorkerState()
	scaler := testScaler(t, state, compute)
	if err := scaler.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(compute.destroyed) != 1 || compute.destroyed[0] != "expired" {
		t.Fatalf("expired worker was not destroyed: %#v", compute.destroyed)
	}
}

func TestCompletionKeepsStateWhenDeletionFails(t *testing.T) {
	compute := &fakeCompute{destroyErr: errors.New("temporary provider failure")}
	state := newWorkerState()
	state.add(provider.Worker{ID: "worker-one", RunnerName: "runner-one"}, true)
	scaler := testScaler(t, state, compute)
	if err := scaler.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "runner-one"}); err == nil {
		t.Fatal("expected deletion failure")
	}
	if _, ok := state.get("runner-one"); !ok {
		t.Fatal("state was forgotten before deletion succeeded")
	}
}

func TestAmbiguousLaunchInventoriesAndDestroysMatchingLease(t *testing.T) {
	compute := &fakeCompute{workers: []provider.Worker{
		{ID: "matching", LeaseID: "lease-one", RunnerName: "runner-one"},
		{ID: "other", LeaseID: "lease-other", RunnerName: "runner-other"},
	}}
	scaler := testScaler(t, newWorkerState(), compute)
	launchErr := errors.New("connection reset after Fly accepted create")
	if err := scaler.cleanupAmbiguousLaunch(context.Background(), "lease-one", launchErr); err != nil {
		t.Fatal(err)
	}
	if len(compute.destroyed) != 1 || compute.destroyed[0] != "matching" {
		t.Fatalf("ambiguous worker cleanup = %#v", compute.destroyed)
	}
}

func testScaler(t *testing.T, state *workerState, compute provider.Compute) *scaler {
	t.Helper()
	budget, err := initializedUsageBudget(filepath.Join(t.TempDir(), "budget.json"), 100*time.Hour, 30*24*time.Hour, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &scaler{
		state: state, compute: compute, maxLifetime: 2 * time.Hour,
		budget: budget,
		logger: slog.New(slog.DiscardHandler),
	}
}
