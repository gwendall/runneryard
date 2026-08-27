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
	workers             []provider.Worker
	inventoryErr        error
	destroyErr          error
	removeBeforeDestroy bool
	destroyed           []string
	events              *[]string
}

func (f *fakeCompute) Launch(_ context.Context, _ provider.Lease) (provider.Worker, error) {
	return provider.Worker{}, errors.New("not implemented in this test")
}

func (f *fakeCompute) Inventory(_ context.Context) ([]provider.Worker, error) {
	if f.inventoryErr != nil {
		return nil, f.inventoryErr
	}
	return append([]provider.Worker(nil), f.workers...), nil
}

func (f *fakeCompute) Destroy(_ context.Context, id string) error {
	if f.removeBeforeDestroy {
		remaining := f.workers[:0]
		for _, worker := range f.workers {
			if worker.ID != id {
				remaining = append(remaining, worker)
			}
		}
		f.workers = remaining
	}
	if f.destroyErr != nil {
		return f.destroyErr
	}
	f.destroyed = append(f.destroyed, id)
	if f.events != nil {
		*f.events = append(*f.events, "worker")
	}
	return nil
}

type fakeSessionCloser struct {
	events         *[]string
	closeErr       error
	hadDeadline    bool
	deadlineWindow time.Duration
}

func (f *fakeSessionCloser) Close(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	f.hadDeadline = ok
	if ok {
		f.deadlineWindow = time.Until(deadline)
	}
	*f.events = append(*f.events, "session")
	return f.closeErr
}

type fakeMessageSession struct{ fakeSessionCloser }

func (*fakeMessageSession) GetMessage(context.Context, int, int) (*scaleset.RunnerScaleSetMessage, error) {
	return nil, errors.New("unexpected GetMessage call")
}

func (*fakeMessageSession) DeleteMessage(context.Context, int) error {
	return errors.New("unexpected DeleteMessage call")
}

func (*fakeMessageSession) AcquireJobs(context.Context, []int64) ([]int64, error) {
	return nil, errors.New("unexpected AcquireJobs call")
}

func (*fakeMessageSession) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{}
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
	worker := provider.Worker{ID: "worker-one", RunnerName: "runner-one"}
	compute := &fakeCompute{
		workers:    []provider.Worker{worker},
		destroyErr: errors.New("temporary provider failure"),
	}
	state := newWorkerState()
	state.add(worker, true)
	scaler := testScaler(t, state, compute)
	if err := scaler.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "runner-one"}); err == nil {
		t.Fatal("expected deletion failure")
	}
	if _, ok := state.get("runner-one"); !ok {
		t.Fatal("state was forgotten before deletion succeeded")
	}
}

func TestCompletionAcceptsAmbiguousDeletionWhenInventoryConfirmsAbsence(t *testing.T) {
	worker := provider.Worker{ID: "worker-one", LeaseID: "lease-one", RunnerName: "runner-one"}
	compute := &fakeCompute{
		workers:             []provider.Worker{worker},
		destroyErr:          errors.New("provider timeout after accepting delete"),
		removeBeforeDestroy: true,
	}
	state := newWorkerState()
	state.add(worker, true)
	scaler := testScaler(t, state, compute)
	if err := scaler.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "runner-one"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := state.get("runner-one"); ok {
		t.Fatal("confirmed deleted worker remained in local state")
	}
}

func TestShutdownReleasesSessionBeforeWorkerCleanup(t *testing.T) {
	events := make([]string, 0, 2)
	compute := &fakeCompute{events: &events}
	state := newWorkerState()
	state.add(provider.Worker{ID: "worker-one", LeaseID: "lease-one", RunnerName: "runner-one"}, false)
	scaler := testScaler(t, state, compute)

	session := &fakeSessionCloser{events: &events, closeErr: errors.New("session API unavailable")}
	shutdownController(session, scaler, slog.New(slog.DiscardHandler))
	if len(events) != 2 || events[0] != "session" || events[1] != "worker" {
		t.Fatalf("shutdown order = %#v, want session then worker", events)
	}
	if !session.hadDeadline || session.deadlineWindow <= 0 || session.deadlineWindow > sessionCloseTimeout {
		t.Fatalf("session close deadline = %s, present=%t", session.deadlineWindow, session.hadDeadline)
	}
}

func TestRecoveryFailureStillReleasesSession(t *testing.T) {
	events := make([]string, 0, 1)
	compute := &fakeCompute{inventoryErr: errors.New("provider inventory unavailable")}
	scaler := testScaler(t, newWorkerState(), compute)
	session := &fakeMessageSession{fakeSessionCloser: fakeSessionCloser{events: &events}}
	cfg := Config{MaxWorkers: 4, Logger: slog.New(slog.DiscardHandler)}

	if err := runControllerSession(context.Background(), session, scaler, cfg, 1); err == nil {
		t.Fatal("expected recovery failure")
	}
	if len(events) != 1 || events[0] != "session" {
		t.Fatalf("recovery failure did not release session: %#v", events)
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
