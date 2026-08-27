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

type fakeRunnerScaleSetClient struct {
	runners   map[string]*scaleset.RunnerReference
	getErr    error
	removeErr error
	removed   []int64
}

func (f *fakeRunnerScaleSetClient) GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	return nil, errors.New("unexpected GenerateJitRunnerConfig call")
}

func (f *fakeRunnerScaleSetClient) GetRunnerByName(_ context.Context, name string) (*scaleset.RunnerReference, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.runners[name], nil
}

func (f *fakeRunnerScaleSetClient) RemoveRunner(_ context.Context, id int64) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, id)
	for name, runner := range f.runners {
		if int64(runner.ID) == id {
			delete(f.runners, name)
		}
	}
	return nil
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
	state.add(provider.Worker{ID: "missing", RunnerName: "runner-00000001"}, true)
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
		ID: "orphan", LeaseID: "lease-orphan", RunnerName: "runner-00000003", CreatedAt: time.Now(),
	}}}
	state := newWorkerState()
	scaler := testScaler(t, state, compute)
	if err := scaler.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	record, ok := state.get("runner-00000003")
	if !ok || record.Worker.ID != "orphan" || !record.Busy {
		t.Fatalf("worker was not conservatively adopted: %#v, %v", record, ok)
	}
}

func TestReconcileDestroysExpiredWorker(t *testing.T) {
	compute := &fakeCompute{workers: []provider.Worker{{
		ID: "expired", LeaseID: "lease-expired", RunnerName: "runner-00000004", CreatedAt: time.Now().Add(-3 * time.Hour),
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
	worker := provider.Worker{ID: "worker-one", RunnerName: "runner-00000001"}
	compute := &fakeCompute{
		workers:    []provider.Worker{worker},
		destroyErr: errors.New("temporary provider failure"),
	}
	state := newWorkerState()
	state.add(worker, true)
	scaler := testScaler(t, state, compute)
	if err := scaler.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "runner-00000001"}); err == nil {
		t.Fatal("expected deletion failure")
	}
	if _, ok := state.get("runner-00000001"); !ok {
		t.Fatal("state was forgotten before deletion succeeded")
	}
}

func TestCompletionAcceptsAmbiguousDeletionWhenInventoryConfirmsAbsence(t *testing.T) {
	worker := provider.Worker{ID: "worker-one", LeaseID: "lease-one", RunnerName: "runner-00000001"}
	compute := &fakeCompute{
		workers:             []provider.Worker{worker},
		destroyErr:          errors.New("provider timeout after accepting delete"),
		removeBeforeDestroy: true,
	}
	state := newWorkerState()
	state.add(worker, true)
	scaler := testScaler(t, state, compute)
	if err := scaler.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "runner-00000001"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := state.get("runner-00000001"); ok {
		t.Fatal("confirmed deleted worker remained in local state")
	}
}

func TestCompletionRemovesGitHubRegistrationAfterProviderWorker(t *testing.T) {
	worker := provider.Worker{ID: "worker-one", LeaseID: "lease-one", RunnerName: "runner-00000001"}
	compute := &fakeCompute{workers: []provider.Worker{worker}, removeBeforeDestroy: true}
	state := newWorkerState()
	state.add(worker, true)
	scaler := testScaler(t, state, compute)
	github := &fakeRunnerScaleSetClient{runners: map[string]*scaleset.RunnerReference{
		// The real Actions service may omit runnerScaleSetId and decode it as zero.
		"runner-00000001": {ID: 42, Name: "runner-00000001"},
	}}
	scaler.scaleSetClient = github

	if err := scaler.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "runner-00000001"}); err != nil {
		t.Fatal(err)
	}
	if len(github.removed) != 1 || github.removed[0] != 42 {
		t.Fatalf("removed GitHub runners = %#v, want [42]", github.removed)
	}
	if scaler.retirements.count() != 0 || state.count() != 0 {
		t.Fatalf("retirement did not converge: pending=%d state=%d", scaler.retirements.count(), state.count())
	}
}

func TestRegistrationCleanupFailureRemainsRetryable(t *testing.T) {
	worker := provider.Worker{ID: "worker-one", LeaseID: "lease-one", RunnerName: "runner-00000001"}
	compute := &fakeCompute{workers: []provider.Worker{worker}, removeBeforeDestroy: true}
	state := newWorkerState()
	state.add(worker, true)
	scaler := testScaler(t, state, compute)
	github := &fakeRunnerScaleSetClient{
		runners: map[string]*scaleset.RunnerReference{
			"runner-00000001": {ID: 42, Name: "runner-00000001", RunnerScaleSetID: 1},
		},
		removeErr: errors.New("temporary GitHub API failure"),
	}
	scaler.scaleSetClient = github

	if err := scaler.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "runner-00000001"}); err == nil {
		t.Fatal("expected GitHub cleanup failure")
	}
	if scaler.retirements.count() != 1 || state.count() != 1 {
		t.Fatalf("cleanup intent was lost: pending=%d state=%d", scaler.retirements.count(), state.count())
	}
	if len(compute.workers) != 0 {
		t.Fatal("provider worker still exists after accepted deletion")
	}

	github.removeErr = nil
	if err := scaler.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if scaler.retirements.count() != 0 || state.count() != 0 || len(github.removed) != 1 {
		t.Fatalf("retry did not converge: pending=%d state=%d removed=%#v", scaler.retirements.count(), state.count(), github.removed)
	}
}

func TestRegistrationCleanupRefusesUnexpectedScaleSet(t *testing.T) {
	worker := provider.Worker{ID: "worker-one", LeaseID: "lease-one", RunnerName: "runner-00000001"}
	compute := &fakeCompute{workers: []provider.Worker{worker}, removeBeforeDestroy: true}
	state := newWorkerState()
	state.add(worker, true)
	scaler := testScaler(t, state, compute)
	github := &fakeRunnerScaleSetClient{runners: map[string]*scaleset.RunnerReference{
		"runner-00000001": {ID: 42, Name: "runner-00000001", RunnerScaleSetID: 999},
	}}
	scaler.scaleSetClient = github

	if err := scaler.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "runner-00000001"}); err == nil {
		t.Fatal("expected scale-set identity failure")
	}
	if len(github.removed) != 0 || scaler.retirements.count() != 1 {
		t.Fatalf("unsafe cleanup was not blocked: removed=%#v pending=%d", github.removed, scaler.retirements.count())
	}
}

func TestRecoveryResumesDurableRegistrationCleanup(t *testing.T) {
	directory := t.TempDir()
	queueFile := filepath.Join(directory, "retirements.json")
	queue, err := newRetirementQueue(queueFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.add("runner-00000001"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := newRetirementQueue(queueFile)
	if err != nil {
		t.Fatal(err)
	}
	scaler := testScaler(t, newWorkerState(), &fakeCompute{})
	scaler.retirements = reloaded
	github := &fakeRunnerScaleSetClient{runners: map[string]*scaleset.RunnerReference{
		"runner-00000001": {ID: 42, Name: "runner-00000001", RunnerScaleSetID: 1},
	}}
	scaler.scaleSetClient = github

	if err := scaler.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reloaded.count() != 0 || len(github.removed) != 1 {
		t.Fatalf("restart cleanup did not converge: pending=%d removed=%#v", reloaded.count(), github.removed)
	}
}

func TestShutdownReleasesSessionBeforePreservingWorkers(t *testing.T) {
	events := make([]string, 0, 1)
	compute := &fakeCompute{events: &events}
	state := newWorkerState()
	state.add(provider.Worker{ID: "worker-one", LeaseID: "lease-one", RunnerName: "runner-00000001"}, false)
	scaler := testScaler(t, state, compute)

	session := &fakeSessionCloser{events: &events, closeErr: errors.New("session API unavailable")}
	shutdownController(session, scaler, slog.New(slog.DiscardHandler))
	if len(events) != 1 || events[0] != "session" {
		t.Fatalf("shutdown events = %#v, want session only", events)
	}
	if !session.hadDeadline || session.deadlineWindow <= 0 || session.deadlineWindow > sessionCloseTimeout {
		t.Fatalf("session close deadline = %s, present=%t", session.deadlineWindow, session.hadDeadline)
	}
	if len(compute.destroyed) != 0 {
		t.Fatalf("shutdown destroyed runners: %#v", compute.destroyed)
	}
	if state.count() != 1 {
		t.Fatalf("runner state count = %d, want 1", state.count())
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

func TestDesiredCountDoesNotDestroyApparentlyIdleWorkers(t *testing.T) {
	workers := []provider.Worker{
		{ID: "worker-one", LeaseID: "lease-one", RunnerName: "runner-00000001", CreatedAt: time.Now()},
		{ID: "worker-two", LeaseID: "lease-two", RunnerName: "runner-00000002", CreatedAt: time.Now()},
	}
	compute := &fakeCompute{workers: workers}
	state := newWorkerState()
	for _, worker := range workers {
		state.add(worker, false)
	}
	scaler := testScaler(t, state, compute)
	scaler.maxWorkers = 4

	count, err := scaler.HandleDesiredRunnerCount(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if count != len(workers) {
		t.Fatalf("runner count = %d, want %d", count, len(workers))
	}
	if len(compute.destroyed) != 0 {
		t.Fatalf("desired-count update destroyed runners: %#v", compute.destroyed)
	}
}

func TestAmbiguousLaunchInventoriesAndDestroysMatchingLease(t *testing.T) {
	compute := &fakeCompute{workers: []provider.Worker{
		{ID: "matching", LeaseID: "lease-one", RunnerName: "runner-00000001"},
		{ID: "other", LeaseID: "lease-other", RunnerName: "runner-00000005"},
	}}
	scaler := testScaler(t, newWorkerState(), compute)
	launchErr := errors.New("connection reset after Fly accepted create")
	if err := scaler.cleanupAmbiguousLaunch(context.Background(), "lease-one", "runner-00000001", launchErr); err != nil {
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
	retirements, err := newRetirementQueue(filepath.Join(t.TempDir(), "retirements.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &scaler{
		state: state, compute: compute, maxLifetime: 2 * time.Hour,
		budget: budget, retirements: retirements,
		scaleSetClient: &fakeRunnerScaleSetClient{runners: make(map[string]*scaleset.RunnerReference)}, scaleSetID: 1,
		logger: slog.New(slog.DiscardHandler),
	}
}
