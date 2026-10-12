package controller

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/gwendall/runneryard/provider"
)

const ephemeralScaleSetID = 2

// tierCompute launches into one shared inventory and records its own leases,
// as the ephemeral-only placement shares the ephemeral provider and its
// worker IDs with the full placement.
type tierCompute struct {
	*slowCompute
	leases []provider.Lease
}

func (c *tierCompute) Launch(ctx context.Context, lease provider.Lease) (provider.Worker, error) {
	worker, err := c.slowCompute.Launch(ctx, lease)
	c.mu.Lock()
	c.leases = append(c.leases, lease)
	c.mu.Unlock()
	return worker, err
}

// scaleSetFleet is a scaler serving a main scale set (id 1), placed on
// pools, and an ephemeral one (id 2), placed on the ephemeral tier alone.
type scaleSetFleet struct {
	scaler    *scaler
	shared    *slowCompute
	pools     *tierCompute
	direct    *tierCompute
	main      scaleSetHandler
	ephemeral scaleSetHandler
	github    *fakeRunnerScaleSetClient
}

func newScaleSetFleet(t *testing.T, maxWorkers, minWorkers int) scaleSetFleet {
	t.Helper()
	shared := &slowCompute{}
	pools := &tierCompute{slowCompute: shared}
	direct := &tierCompute{slowCompute: shared}
	scaler := concurrentScaler(t, pools, 0, maxWorkers)
	scaler.minWorkers = minWorkers
	scaler.scaleSetName = "acme-linux"
	scaler.ephemeral = &scaleSetRoute{name: "acme-linux-ephemeral", id: ephemeralScaleSetID, launch: direct}
	return scaleSetFleet{
		scaler: scaler, shared: shared, pools: pools, direct: direct,
		main:      scaleSetHandler{scaler: scaler, route: scaler.mainRoute()},
		ephemeral: scaleSetHandler{scaler: scaler, route: *scaler.ephemeral},
		github:    scaler.scaleSetClient.(*fakeRunnerScaleSetClient),
	}
}

func desire(t *testing.T, handler scaleSetHandler, assigned, want int) {
	t.Helper()
	count, err := handler.HandleDesiredRunnerCount(context.Background(), assigned)
	if err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("%s: %d assigned job(s) left %d worker(s), want %d", handler.route.name, assigned, count, want)
	}
}

func TestEphemeralScaleSetLaunchesOnItsOwnTierAndScaleSet(t *testing.T) {
	fleet := newScaleSetFleet(t, 4, 0)

	desire(t, fleet.ephemeral, 1, 1)
	if len(fleet.direct.leases) != 1 || len(fleet.pools.leases) != 0 {
		t.Fatalf("an ephemeral scale set job must skip the pools: ephemeral=%d pools=%d", len(fleet.direct.leases), len(fleet.pools.leases))
	}
	lease := fleet.direct.leases[0]
	if lease.RunnerScaleSetID != ephemeralScaleSetID {
		t.Fatalf("lease scale set = %d, want %d", lease.RunnerScaleSetID, ephemeralScaleSetID)
	}
	if registration := fleet.github.runners[lease.RunnerName]; registration == nil || registration.RunnerScaleSetID != ephemeralScaleSetID {
		t.Fatalf("the runner must be generated in the ephemeral scale set: %#v", registration)
	}

	desire(t, fleet.main, 1, 1)
	if len(fleet.pools.leases) != 1 || fleet.pools.leases[0].RunnerScaleSetID != 1 {
		t.Fatalf("a main scale set job must follow the placement in its own scale set: %#v", fleet.pools.leases)
	}

	desire(t, fleet.ephemeral, 1, 1)
	if len(fleet.direct.leases) != 1 {
		t.Fatalf("a repeated desired count must not launch again, got %d launches", len(fleet.direct.leases))
	}
}

func TestScaleSetsShareMaxRunnersAndTheBudget(t *testing.T) {
	fleet := newScaleSetFleet(t, 3, 0)

	desire(t, fleet.main, 2, 2)
	desire(t, fleet.ephemeral, 3, 1)
	desire(t, fleet.main, 3, 2)
	if fleet.scaler.state.count() != 3 || len(fleet.pools.leases) != 2 || len(fleet.direct.leases) != 1 {
		t.Fatalf("MAX_RUNNERS must cap both scale sets together: fleet=%d pools=%d ephemeral=%d",
			fleet.scaler.state.count(), len(fleet.pools.leases), len(fleet.direct.leases))
	}
	if reserved := fleet.scaler.budget.snapshot(time.Now()).ReservedSeconds; reserved != 3*int64((2*time.Hour).Seconds()) {
		t.Fatalf("one ledger must hold every launch's reservation, reserved %d s", reserved)
	}
}

func TestMinRunnersWarmsOnlyTheMainScaleSet(t *testing.T) {
	fleet := newScaleSetFleet(t, 4, 1)

	desire(t, fleet.ephemeral, 0, 0)
	desire(t, fleet.main, 0, 1)
	if len(fleet.direct.leases) != 0 || len(fleet.pools.leases) != 1 {
		t.Fatalf("MIN_RUNNERS must warm the main scale set alone: ephemeral=%d pools=%d", len(fleet.direct.leases), len(fleet.pools.leases))
	}
}

// crossedScaleSetClient answers every JIT request with a runner of the main
// scale set, whatever scale set asked for it.
type crossedScaleSetClient struct{ *fakeRunnerScaleSetClient }

func (c crossedScaleSetClient) GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, _ int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	return c.fakeRunnerScaleSetClient.GenerateJitRunnerConfig(ctx, setting, 1)
}

func TestEphemeralRunnerFromAnotherScaleSetFailsClosed(t *testing.T) {
	fleet := newScaleSetFleet(t, 4, 0)
	fleet.scaler.scaleSetClient = crossedScaleSetClient{fleet.github}

	_, err := fleet.ephemeral.HandleDesiredRunnerCount(context.Background(), 1)
	if !isHandlerFailure(err) {
		t.Fatalf("a runner generated in the wrong scale set must fail closed, got %v", err)
	}
	if len(fleet.direct.leases) != 0 || len(fleet.pools.leases) != 0 {
		t.Fatalf("no worker may start for a misattributed runner: ephemeral=%d pools=%d", len(fleet.direct.leases), len(fleet.pools.leases))
	}
}

func TestRetirementRemovesRunnersOfBothScaleSetsAndNoOther(t *testing.T) {
	fleet := newScaleSetFleet(t, 4, 0)
	now := time.Now()
	ephemeral := provider.Worker{ID: "worker-e", LeaseID: "lease-e", RunnerName: "runner-0000000e", RunnerID: 41, RunnerScaleSetID: ephemeralScaleSetID, CreatedAt: now}
	main := provider.Worker{ID: "worker-m", LeaseID: "lease-m", RunnerName: "runner-0000000a", RunnerID: 42, RunnerScaleSetID: 1, CreatedAt: now}
	foreign := provider.Worker{ID: "worker-f", LeaseID: "lease-f", RunnerName: "runner-0000000f", RunnerID: 43, RunnerScaleSetID: 3, CreatedAt: now}
	for _, worker := range []provider.Worker{ephemeral, main, foreign} {
		fleet.shared.workers = append(fleet.shared.workers, worker)
		fleet.scaler.state.add(worker, true)
		fleet.github.runners[worker.RunnerName] = &scaleset.RunnerReference{ID: int(worker.RunnerID), Name: worker.RunnerName, RunnerScaleSetID: worker.RunnerScaleSetID}
	}
	ctx := context.Background()

	if err := fleet.ephemeral.HandleJobCompleted(ctx, &scaleset.JobCompleted{RunnerName: "runner-0000000e"}); err != nil {
		t.Fatal(err)
	}
	if err := fleet.main.HandleJobCompleted(ctx, &scaleset.JobCompleted{RunnerName: "runner-0000000a"}); err != nil {
		t.Fatal(err)
	}
	if len(fleet.github.removed) != 2 || fleet.github.removed[0] != 41 || fleet.github.removed[1] != 42 {
		t.Fatalf("removed GitHub runners = %#v, want [41 42]", fleet.github.removed)
	}

	err := fleet.main.HandleJobCompleted(ctx, &scaleset.JobCompleted{RunnerName: "runner-0000000f"})
	if !isHandlerFailure(err) {
		t.Fatalf("a runner of a scale set the controller does not serve must fail closed, got %v", err)
	}
	if len(fleet.github.removed) != 2 || fleet.scaler.retirements.count() != 1 {
		t.Fatalf("the foreign registration must stay, its retirement pending: removed=%#v pending=%d", fleet.github.removed, fleet.scaler.retirements.count())
	}
}

func TestRecoveryCountsEachWorkerTowardItsScaleSet(t *testing.T) {
	fleet := newScaleSetFleet(t, 4, 0)
	now := time.Now()
	fleet.shared.workers = []provider.Worker{
		{ID: "worker-m", LeaseID: "lease-m", RunnerName: "runner-m", RunnerID: 42, RunnerScaleSetID: 1, CreatedAt: now},
		{ID: "worker-e", LeaseID: "lease-e", RunnerName: "runner-e", RunnerID: 41, RunnerScaleSetID: ephemeralScaleSetID, CreatedAt: now},
	}
	if err := fleet.scaler.recover(context.Background()); err != nil {
		t.Fatal(err)
	}

	desire(t, fleet.ephemeral, 1, 1)
	desire(t, fleet.main, 1, 1)
	if len(fleet.direct.leases)+len(fleet.pools.leases) != 0 {
		t.Fatalf("recovered workers must cover their own scale set's jobs: ephemeral=%d pools=%d", len(fleet.direct.leases), len(fleet.pools.leases))
	}
	desire(t, fleet.main, 2, 2)
	if len(fleet.pools.leases) != 1 || len(fleet.direct.leases) != 0 {
		t.Fatalf("a recovered ephemeral worker must not count toward the main scale set: ephemeral=%d pools=%d", len(fleet.direct.leases), len(fleet.pools.leases))
	}
}

func TestSingleScaleSetCountsEveryWorkerAsBefore(t *testing.T) {
	// Without the option a worker of another scale set still fills the
	// fleet, as every worker did before there were two scale sets.
	compute := &slowCompute{workers: []provider.Worker{{
		ID: "worker-old", LeaseID: "lease-old", RunnerName: "runner-old", RunnerScaleSetID: 9, CreatedAt: time.Now(),
	}}}
	scaler := concurrentScaler(t, compute, 0, 4)
	if err := scaler.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	count, err := scaler.HandleDesiredRunnerCount(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || compute.launched != 0 {
		t.Fatalf("a single scale set must count every worker: count=%d launches=%d", count, compute.launched)
	}
	if scaler.servesScaleSet(ephemeralScaleSetID) {
		t.Fatal("a single scale set must serve no other")
	}
}

// failingScaler fails closed on its first desired count, as an identity or
// ledger failure does.
type failingScaler struct{}

func (failingScaler) HandleDesiredRunnerCount(context.Context, int) (int, error) {
	return 0, failClosed(errors.New("runner identity mismatch"))
}

func (failingScaler) HandleJobStarted(context.Context, *scaleset.JobStarted) error { return nil }

func (failingScaler) HandleJobCompleted(context.Context, *scaleset.JobCompleted) error { return nil }

func TestSecondScaleSetSessionOpensOnlyAfterRecovery(t *testing.T) {
	worker := provider.Worker{ID: "worker-e", LeaseID: "lease-e", RunnerName: "runner-e", RunnerScaleSetID: ephemeralScaleSetID, CreatedAt: time.Now()}
	compute := &fakeCompute{workers: []provider.Worker{worker}}
	scaler, _ := supervisedScaler(t, compute)
	scaler.ephemeral = &scaleSetRoute{name: "eph", id: ephemeralScaleSetID, launch: compute}
	cfg := Config{MaxWorkers: 2, Logger: slog.New(slog.DiscardHandler)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mainEvents, ephemeralEvents := make([]string, 0, 1), make([]string, 0, 1)
	recoveredAtOpen := -1
	sets := []sessionScaleSet{
		{name: "main", id: 1, handler: scaleSetHandler{scaler: scaler, route: scaler.mainRoute()}, open: func(context.Context) (messageSession, error) {
			return &supervisedSession{fakeSessionCloser: fakeSessionCloser{events: &mainEvents}}, nil
		}},
		{name: "eph", id: ephemeralScaleSetID, handler: scaleSetHandler{scaler: scaler, route: *scaler.ephemeral}, open: func(context.Context) (messageSession, error) {
			recoveredAtOpen = scaler.state.count()
			return &supervisedSession{fakeSessionCloser: fakeSessionCloser{events: &ephemeralEvents}, onPoll: cancel}, nil
		}},
	}
	if err := superviseScaleSets(ctx, scaler, cfg, sets, time.Millisecond, 4*time.Millisecond); err != nil {
		t.Fatalf("cancellation must end both sessions quietly, got %v", err)
	}
	if recoveredAtOpen != 1 {
		t.Fatalf("the second session must open after recovery adopted the fleet, saw %d worker(s)", recoveredAtOpen)
	}
	if len(mainEvents) != 1 || len(ephemeralEvents) != 1 {
		t.Fatalf("both sessions must be released: main=%#v ephemeral=%#v", mainEvents, ephemeralEvents)
	}
}

func TestHandlerFailureOnEitherScaleSetStopsBoth(t *testing.T) {
	compute := &fakeCompute{}
	scaler, _ := supervisedScaler(t, compute)
	scaler.ephemeral = &scaleSetRoute{name: "eph", id: ephemeralScaleSetID, launch: compute}
	cfg := Config{MaxWorkers: 2, Logger: slog.New(slog.DiscardHandler)}

	mainEvents, ephemeralEvents := make([]string, 0, 1), make([]string, 0, 1)
	opened := 0
	sets := []sessionScaleSet{
		{name: "main", id: 1, handler: scaleSetHandler{scaler: scaler, route: scaler.mainRoute()}, open: func(context.Context) (messageSession, error) {
			return &supervisedSession{fakeSessionCloser: fakeSessionCloser{events: &mainEvents}}, nil
		}},
		{name: "eph", id: ephemeralScaleSetID, handler: failingScaler{}, open: func(context.Context) (messageSession, error) {
			opened++
			return &supervisedSession{fakeSessionCloser: fakeSessionCloser{events: &ephemeralEvents}}, nil
		}},
	}
	err := superviseScaleSets(context.Background(), scaler, cfg, sets, time.Millisecond, time.Millisecond)
	if !isHandlerFailure(err) {
		t.Fatalf("a handler failure on the second scale set must end the controller, got %v", err)
	}
	if opened != 1 {
		t.Fatalf("a handler failure must not reopen the session, got %d sessions", opened)
	}
	if len(mainEvents) != 1 || len(ephemeralEvents) != 1 {
		t.Fatalf("both sessions must be released: main=%#v ephemeral=%#v", mainEvents, ephemeralEvents)
	}
}

func TestLaunchGateWakeReachesEveryScaleSet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := make(chan struct{}, 1)
	targets := []chan struct{}{make(chan struct{}, 1), make(chan struct{}, 1)}
	go fanOutWake(ctx, source, targets)
	source <- struct{}{}
	for i, target := range targets {
		select {
		case <-target:
		case <-time.After(2 * time.Second):
			t.Fatalf("session %d was not woken for the probe", i)
		}
	}
}

func TestFleetStatusListsBothScaleSets(t *testing.T) {
	statusFile := filepath.Join(t.TempDir(), "status.json")
	reporter, err := newStatusReporter(Config{
		ScaleSetName: "acme-linux", EphemeralScaleSetName: "acme-linux-ephemeral", ControllerID: "acme",
		Provider: "fly", MaxWorkers: 4, StatusFile: statusFile, Logger: slog.New(slog.DiscardHandler),
	}, BudgetStatus{})
	if err != nil {
		t.Fatal(err)
	}
	reporter.desired("acme-linux", 3, 3)
	reporter.desired("acme-linux-ephemeral", 1, 1)
	reporter.scaleSetWorkers("acme-linux", 2)
	reporter.scaleSetWorkers("acme-linux-ephemeral", 1)
	status := loadFleetStatus(t, statusFile)
	if status.GitHub.AssignedJobs != 4 || status.GitHub.DesiredWorkers != 4 {
		t.Fatalf("fleet figures must sum both scale sets: %#v", status.GitHub)
	}
	want := []ScaleSetStatus{
		{Name: "acme-linux", AssignedJobs: 3, DesiredWorkers: 3, Workers: 2},
		{Name: "acme-linux-ephemeral", AssignedJobs: 1, DesiredWorkers: 1, Workers: 1},
	}
	if len(status.GitHub.ScaleSets) != 2 || status.GitHub.ScaleSets[0] != want[0] || status.GitHub.ScaleSets[1] != want[1] {
		t.Fatalf("scale sets = %#v, want %#v", status.GitHub.ScaleSets, want)
	}
}

func TestFleetStatusWithOneScaleSetIsUnchanged(t *testing.T) {
	statusFile := filepath.Join(t.TempDir(), "status.json")
	reporter, err := newStatusReporter(Config{
		ScaleSetName: "acme-linux", ControllerID: "acme", Provider: "fly", MaxWorkers: 4,
		StatusFile: statusFile, Logger: slog.New(slog.DiscardHandler),
	}, BudgetStatus{})
	if err != nil {
		t.Fatal(err)
	}
	reporter.desired("acme-linux", 2, 2)
	reporter.scaleSetWorkers("acme-linux", 2)
	contents, err := os.ReadFile(statusFile) // #nosec G304 -- test temporary file
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		GitHub map[string]json.RawMessage `json:"github"`
	}
	if err := json.Unmarshal(contents, &raw); err != nil {
		t.Fatal(err)
	}
	if _, listed := raw.GitHub["scale_sets"]; listed {
		t.Fatalf("a single scale set must keep the status schema as it was: %s", contents)
	}
	if status := loadFleetStatus(t, statusFile); status.GitHub.AssignedJobs != 2 || status.GitHub.DesiredWorkers != 2 {
		t.Fatalf("status = %#v", status.GitHub)
	}
}

// bootingTier is the ephemeral scale set's compute in a speculation test: the
// same inventory as the placement, but every launch boots a machine.
type bootingTier struct{ *speculatingCompute }

func (c bootingTier) Launch(_ context.Context, lease provider.Lease) (provider.Worker, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.add("boot", lease), nil
}

func speculatingFleet(t *testing.T, compute *speculatingCompute) (*scaler, scaleSetHandler, scaleSetHandler) {
	t.Helper()
	scaler := speculationScaler(t, compute, 10)
	scaler.scaleSetName = "acme-linux"
	scaler.ephemeral = &scaleSetRoute{name: "acme-linux-ephemeral", id: ephemeralScaleSetID, launch: bootingTier{compute}}
	return scaler, scaleSetHandler{scaler: scaler, route: scaler.mainRoute()}, scaleSetHandler{scaler: scaler, route: *scaler.ephemeral}
}

func TestEphemeralScaleSetJobsGetNoSpeculativeRunner(t *testing.T) {
	compute := &speculatingCompute{room: 2}
	scaler, main, ephemeral := speculatingFleet(t, compute)

	// The job asked to skip the pools: it waits on its booting worker even
	// though slots are free, on every pass of either scale set.
	for range 2 {
		desire(t, ephemeral, 1, 1)
		desire(t, main, 0, 0)
	}
	if spares, _ := compute.counts(); spares != 0 {
		t.Fatalf("speculative runners = %d for an ephemeral scale set job, want 0", spares)
	}
	if runnerOn(t, scaler.state, "boot:") == "" {
		t.Fatal("the ephemeral scale set job must keep its booting worker")
	}
}

func TestSpeculationCountsOnlyTheMainScaleSet(t *testing.T) {
	compute := &speculatingCompute{}
	scaler, main, ephemeral := speculatingFleet(t, compute)
	ctx := context.Background()

	// An ephemeral scale set job runs on its own worker.
	desire(t, ephemeral, 1, 1)
	if err := ephemeral.HandleJobStarted(ctx, &scaleset.JobStarted{RunnerName: runnerOn(t, scaler.state, "boot:")}); err != nil {
		t.Fatal(err)
	}
	// A main scale set job overflows to a booting worker, then a slot frees.
	desire(t, main, 1, 1)
	compute.mu.Lock()
	compute.room = 1
	compute.mu.Unlock()
	desire(t, main, 1, 2)

	// The other scale set's busy worker does not cover the main job: it still
	// gets its runner in the slot, in the main scale set.
	if spares, _ := compute.counts(); spares != 1 {
		t.Fatalf("speculative runners = %d, want 1 for the main job", spares)
	}
	name := runnerOn(t, scaler.state, "pool:")
	record, _ := scaler.state.get(name)
	if record.Worker.RunnerScaleSetID != 1 {
		t.Fatalf("speculative worker scale set = %d, want the main one", record.Worker.RunnerScaleSetID)
	}
	client := scaler.scaleSetClient.(*fakeRunnerScaleSetClient)
	if registration := client.runners[name]; registration == nil || registration.RunnerScaleSetID != 1 {
		t.Fatalf("the speculative runner must be generated in the main scale set: %#v", registration)
	}
	desire(t, ephemeral, 1, 1)
	if spares, _ := compute.counts(); spares != 1 {
		t.Fatalf("speculative runners = %d after an ephemeral pass, want still 1", spares)
	}
}

func TestOnlyTheMainScaleSetSessionWakesForRoom(t *testing.T) {
	compute := &speculatingCompute{roomReady: make(chan struct{}, 1)}
	scaler, _, _ := speculatingFleet(t, compute)
	opener := func(int) sessionOpener { return nil }
	sets := scaler.scaleSetSessions(opener, []chan struct{}{make(chan struct{}, 1), make(chan struct{}, 1)})
	if len(sets) != 2 || sets[0].id != 1 || sets[1].id != ephemeralScaleSetID {
		t.Fatalf("sessions = %#v, want main then ephemeral", sets)
	}
	if sets[0].room != (<-chan struct{})(compute.roomReady) {
		t.Fatal("the main scale set's session must wake when a pool host becomes ready")
	}
	if sets[1].room != nil {
		t.Fatal("the ephemeral scale set's session must not wake for pool room: it never speculates")
	}
	if sets[0].wake == nil || sets[1].wake == nil || sets[0].wake == sets[1].wake {
		t.Fatal("each session must take its own copy of the launch gate's wake-ups")
	}
}
