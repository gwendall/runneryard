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
	launchWorker        provider.Worker
	launchErr           error
	destroyErr          error
	removeBeforeDestroy bool
	destroyed           []string
	events              *[]string
}

type fakeRunnerScaleSetClient struct {
	runners     map[string]*scaleset.RunnerReference
	generateJIT *scaleset.RunnerScaleSetJitRunnerConfig
	generateErr error
	getErr      error
	removeErr   error
	removed     []int64
}

func (f *fakeRunnerScaleSetClient) GenerateJitRunnerConfig(_ context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	if f.generateErr != nil {
		return nil, f.generateErr
	}
	if f.generateJIT == nil {
		return nil, errors.New("unexpected GenerateJitRunnerConfig call")
	}
	if f.generateJIT.Runner != nil {
		registration := *f.generateJIT.Runner
		registration.Name = setting.Name
		registration.RunnerScaleSetID = scaleSetID
		f.runners[setting.Name] = &registration
		return &scaleset.RunnerScaleSetJitRunnerConfig{Runner: &registration, EncodedJITConfig: f.generateJIT.EncodedJITConfig}, nil
	}
	return f.generateJIT, nil
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

func (f *fakeCompute) Launch(_ context.Context, lease provider.Lease) (provider.Worker, error) {
	worker := f.launchWorker
	if worker.LeaseID == "" {
		worker.LeaseID = lease.ID
	}
	if worker.RunnerName == "" {
		worker.RunnerName = lease.RunnerName
	}
	if worker.RunnerID == 0 {
		worker.RunnerID = lease.RunnerID
	}
	if worker.RunnerScaleSetID == 0 {
		worker.RunnerScaleSetID = lease.RunnerScaleSetID
	}
	if worker.CreatedAt.IsZero() {
		worker.CreatedAt = time.Now()
	}
	if worker.ID != "" {
		f.workers = append(f.workers, worker)
	}
	return worker, f.launchErr
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
	state.add(provider.Worker{ID: "missing", LeaseID: "lease-one", RunnerName: "runner-00000001"}, true)
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
	if !ok || record.Worker.ID != "orphan" || record.Busy || record.Observed {
		t.Fatalf("worker was not conservatively adopted: %#v, %v", record, ok)
	}
}

func TestRecoveryDoesNotClaimAnUnobservedWorkerIsBusy(t *testing.T) {
	worker := provider.Worker{
		ID: "recovered", LeaseID: "lease-recovered", RunnerName: "runner-recovered",
		RunnerID: 42, RunnerScaleSetID: 1, CreatedAt: time.Now(),
	}
	state := newWorkerState()
	scaler := testScaler(t, state, &fakeCompute{workers: []provider.Worker{worker}})

	if err := scaler.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	record, ok := state.get(worker.RunnerName)
	if !ok {
		t.Fatal("recovered worker missing from local state")
	}
	if record.Busy {
		t.Fatal("recovered worker was reported busy without a JobStarted event")
	}
	actual, busy, idle, unknown := state.summary()
	if actual != 1 || busy != 0 || idle != 0 || unknown != 1 {
		t.Fatalf("recovered worker summary = actual %d, busy %d, idle %d, unknown %d", actual, busy, idle, unknown)
	}
	if err := scaler.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: worker.RunnerName}); err != nil {
		t.Fatal(err)
	}
	_, busy, idle, unknown = state.summary()
	if busy != 1 || idle != 0 || unknown != 0 {
		t.Fatalf("observed worker summary = busy %d, idle %d, unknown %d", busy, idle, unknown)
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

func TestCompletionDefersBusyGitHubRegistrationWithoutStoppingListener(t *testing.T) {
	worker := provider.Worker{
		ID: "worker-one", LeaseID: "lease-one", RunnerName: "runner-00000001",
		RunnerID: 42, RunnerScaleSetID: 1,
	}
	compute := &fakeCompute{workers: []provider.Worker{worker}, removeBeforeDestroy: true}
	state := newWorkerState()
	state.add(worker, true)
	scaler := testScaler(t, state, compute)
	github := &fakeRunnerScaleSetClient{
		runners: map[string]*scaleset.RunnerReference{
			worker.RunnerName: {ID: 42, Name: worker.RunnerName, RunnerScaleSetID: 1},
		},
		removeErr: scaleset.JobStillRunningError,
	}
	scaler.scaleSetClient = github

	if err := scaler.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: worker.RunnerName}); err != nil {
		t.Fatalf("busy registration stopped the listener: %v", err)
	}
	if scaler.retirements.count() != 1 || state.count() != 1 || len(compute.workers) != 0 {
		t.Fatalf("deferred completion state: pending=%d state=%d workers=%#v", scaler.retirements.count(), state.count(), compute.workers)
	}

	github.removeErr = nil
	if err := scaler.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if scaler.retirements.count() != 0 || state.count() != 0 || len(github.removed) != 1 {
		t.Fatalf("deferred completion did not converge: pending=%d state=%d removed=%#v", scaler.retirements.count(), state.count(), github.removed)
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
	if err := queue.put(retirementEntry{
		RunnerName: "runner-00000001", RunnerID: 42, RunnerScaleSetID: 1,
		LeaseID: "lease-one", BudgetDisposition: settleActualUsage,
	}); err != nil {
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

func TestRecoveryDefersBusyGitHubRegistrationWithoutBlockingController(t *testing.T) {
	directory := t.TempDir()
	queue, err := newRetirementQueue(filepath.Join(directory, "retirements.json"))
	if err != nil {
		t.Fatal(err)
	}
	entry := retirementEntry{
		RunnerName: "runner-00000001", RunnerID: 42, RunnerScaleSetID: 1,
		LeaseID: "lease-one", BudgetDisposition: settleActualUsage,
	}
	if err := queue.put(entry); err != nil {
		t.Fatal(err)
	}
	scaler := testScaler(t, newWorkerState(), &fakeCompute{})
	scaler.retirements = queue
	github := &fakeRunnerScaleSetClient{
		runners: map[string]*scaleset.RunnerReference{
			entry.RunnerName: {ID: 42, Name: entry.RunnerName, RunnerScaleSetID: 1},
		},
		removeErr: scaleset.JobStillRunningError,
	}
	scaler.scaleSetClient = github

	if err := scaler.recover(context.Background()); err != nil {
		t.Fatalf("busy registration blocked controller recovery: %v", err)
	}
	if queue.count() != 1 || len(github.removed) != 0 {
		t.Fatalf("busy retirement was not retained safely: pending=%d removed=%#v", queue.count(), github.removed)
	}

	github.removeErr = nil
	if err := scaler.reconcile(context.Background()); err != nil {
		t.Fatalf("deferred retirement did not converge: %v", err)
	}
	if queue.count() != 0 || len(github.removed) != 1 || github.removed[0] != entry.RunnerID {
		t.Fatalf("deferred retirement result: pending=%d removed=%#v", queue.count(), github.removed)
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
	if err := scaler.retirements.put(retirementEntry{
		RunnerName: "runner-00000001", RunnerID: 42, RunnerScaleSetID: 1,
		LeaseID: "lease-one", BudgetDisposition: forfeitReservation,
	}); err != nil {
		t.Fatal(err)
	}
	if err := scaler.cleanupAmbiguousLaunch(context.Background(), retirementEntry{
		RunnerName: "runner-00000001", RunnerID: 42, RunnerScaleSetID: 1,
		LeaseID: "lease-one", BudgetDisposition: forfeitReservation,
	}, launchErr); err != nil {
		t.Fatal(err)
	}
	if len(compute.destroyed) != 1 || compute.destroyed[0] != "matching" {
		t.Fatalf("ambiguous worker cleanup = %#v", compute.destroyed)
	}
}

func TestAmbiguousLaunchInventoryFailureSurvivesRestart(t *testing.T) {
	directory := t.TempDir()
	queueFile := filepath.Join(directory, "retirements.json")
	queue, err := newRetirementQueue(queueFile)
	if err != nil {
		t.Fatal(err)
	}
	compute := &fakeCompute{
		launchErr:    errors.New("launch response lost"),
		inventoryErr: errors.New("provider inventory unavailable"),
	}
	scaler := testScaler(t, newWorkerState(), compute)
	scaler.retirements = queue
	scaler.scaleSetClient = &fakeRunnerScaleSetClient{
		runners: make(map[string]*scaleset.RunnerReference),
		generateJIT: &scaleset.RunnerScaleSetJitRunnerConfig{
			Runner: &scaleset.RunnerReference{ID: 42}, EncodedJITConfig: "jit-secret",
		},
	}
	if started, err := scaler.startWorker(context.Background()); err == nil || started {
		t.Fatal("expected ambiguous launch cleanup to fail closed")
	}
	entries := queue.all()
	if len(entries) != 1 || entries[0].RunnerID != 42 || entries[0].BudgetDisposition != forfeitReservation {
		t.Fatalf("durable ambiguous launch proof = %#v", entries)
	}
	entry := entries[0]

	reloaded, err := newRetirementQueue(queueFile)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.count() != 1 {
		t.Fatalf("retirement intent count after restart = %d, want 1", reloaded.count())
	}
	recovered := testScaler(t, newWorkerState(), &fakeCompute{})
	recovered.retirements = reloaded
	recovered.scaleSetClient = &fakeRunnerScaleSetClient{runners: map[string]*scaleset.RunnerReference{
		entry.RunnerName: {ID: 42, Name: entry.RunnerName, RunnerScaleSetID: 1},
	}}
	if err := recovered.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reloaded.count() != 0 {
		t.Fatalf("retirement intent count after recovery = %d, want 0", reloaded.count())
	}
}

func TestAmbiguousLaunchForfeitsReservationInsteadOfUndercharging(t *testing.T) {
	for _, test := range []struct {
		name    string
		partial bool
	}{
		{name: "unknown provider outcome"},
		{name: "partial provider outcome", partial: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			compute := &fakeCompute{}
			scaler := testScaler(t, newWorkerState(), compute)
			entry := retirementEntry{
				RunnerName: "runner-00000001", RunnerID: 42, RunnerScaleSetID: 1,
				LeaseID: "lease-one", BudgetDisposition: forfeitReservation,
			}
			if allowed, _, err := scaler.budget.reserve(entry.LeaseID, time.Now(), scaler.maxLifetime); err != nil || !allowed {
				t.Fatalf("reserve = %t, %v", allowed, err)
			}
			if err := scaler.retirements.put(entry); err != nil {
				t.Fatal(err)
			}
			launchErr := error(errors.New("launch response lost"))
			if test.partial {
				partial := provider.Worker{
					ID: "partial", LeaseID: entry.LeaseID, RunnerName: entry.RunnerName,
					RunnerID: entry.RunnerID, RunnerScaleSetID: entry.RunnerScaleSetID,
				}
				compute.workers = []provider.Worker{partial}
				compute.removeBeforeDestroy = true
				launchErr = &provider.PartialLaunchError{Worker: partial, Err: launchErr}
			}
			if err := scaler.cleanupAmbiguousLaunch(context.Background(), entry, launchErr); err != nil {
				t.Fatal(err)
			}
			snapshot := scaler.budget.snapshot(time.Now())
			if snapshot.UsedSeconds != int64((2*time.Hour)/time.Second) || snapshot.ReservedSeconds != 0 {
				t.Fatalf("budget after ambiguous launch = %#v", snapshot)
			}
		})
	}
}

func TestCanceledCompletionRetiresWorkerAndLateEventsAreIdempotent(t *testing.T) {
	worker := provider.Worker{
		ID: "worker-one", LeaseID: "lease-one", RunnerName: "runner-00000001",
		RunnerID: 42, RunnerScaleSetID: 1,
	}
	compute := &fakeCompute{workers: []provider.Worker{worker}, removeBeforeDestroy: true}
	state := newWorkerState()
	state.add(worker, true)
	scaler := testScaler(t, state, compute)
	scaler.scaleSetClient = &fakeRunnerScaleSetClient{runners: map[string]*scaleset.RunnerReference{
		worker.RunnerName: {ID: 42, Name: worker.RunnerName, RunnerScaleSetID: 1},
	}}
	completed := &scaleset.JobCompleted{RunnerName: worker.RunnerName, Result: "canceled"}
	if err := scaler.HandleJobCompleted(context.Background(), completed); err != nil {
		t.Fatal(err)
	}
	if state.count() != 0 || len(compute.destroyed) != 1 {
		t.Fatalf("canceled job cleanup: state=%d destroyed=%#v", state.count(), compute.destroyed)
	}
	if err := scaler.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: worker.RunnerName}); err != nil {
		t.Fatal(err)
	}
	if err := scaler.HandleJobCompleted(context.Background(), completed); err != nil {
		t.Fatal(err)
	}
	if state.count() != 0 || len(compute.destroyed) != 1 {
		t.Fatalf("late events changed retired worker: state=%d destroyed=%#v", state.count(), compute.destroyed)
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
