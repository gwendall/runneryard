package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/gwendall/runneryard/provider"
)

// speculatingCompute stands for a placement of one pool and one tier that
// boots a machine per worker. Launch takes a free slot ("pool:") and
// overflows to the booting tier ("boot:") when there is none; LaunchSpare
// takes free slots only. Destroying a pool worker frees its slot.
type speculatingCompute struct {
	mu        sync.Mutex
	room      int
	seq       int
	workers   []provider.Worker
	spares    int
	launchErr error
	spareErr  error
	destroyed []string
	roomReady chan struct{}
	spared    chan struct{}
}

func (c *speculatingCompute) add(tier string, lease provider.Lease) provider.Worker {
	c.seq++
	worker := provider.Worker{
		ID: fmt.Sprintf("%s:%d", tier, c.seq), LeaseID: lease.ID, RunnerName: lease.RunnerName,
		RunnerID: lease.RunnerID, RunnerScaleSetID: lease.RunnerScaleSetID, CreatedAt: time.Now(),
	}
	c.workers = append(c.workers, worker)
	return worker
}

func (c *speculatingCompute) Launch(_ context.Context, lease provider.Lease) (provider.Worker, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.launchErr != nil {
		return provider.Worker{}, c.launchErr
	}
	if c.room > 0 {
		c.room--
		return c.add("pool", lease), nil
	}
	return c.add("boot", lease), nil
}

func (c *speculatingCompute) LaunchSpare(_ context.Context, lease provider.Lease) (provider.Worker, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.spared != nil {
		defer func() { c.spared <- struct{}{} }()
	}
	if c.spareErr != nil {
		return provider.Worker{}, c.spareErr
	}
	if c.room < 1 {
		return provider.Worker{}, &provider.CapacityError{Reason: "pool_full", Err: errors.New("no free slot")}
	}
	c.room--
	c.spares++
	return c.add("pool", lease), nil
}

func (c *speculatingCompute) SpareRoom() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.room
}

func (*speculatingCompute) Fast(workerID string) bool { return strings.HasPrefix(workerID, "pool:") }

func (c *speculatingCompute) RoomReady() <-chan struct{} { return c.roomReady }

func (c *speculatingCompute) Inventory(context.Context) ([]provider.Worker, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]provider.Worker(nil), c.workers...), nil
}

func (c *speculatingCompute) Destroy(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	remaining := c.workers[:0]
	for _, worker := range c.workers {
		if worker.ID != id {
			remaining = append(remaining, worker)
		}
	}
	c.workers = remaining
	c.destroyed = append(c.destroyed, id)
	if strings.HasPrefix(id, "pool:") {
		c.room++
	}
	return nil
}

func (c *speculatingCompute) counts() (spares int, destroyed []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spares, append([]string(nil), c.destroyed...)
}

// seed records workers as this controller's, launched earlier: busy, idle,
// or adopted after a restart and never observed.
func (c *speculatingCompute) seed(state *workerState, tier string, count int, mark func(provider.Worker)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for range count {
		name := fmt.Sprintf("runner-%s-%d", tier, c.seq+1)
		mark(c.add(tier, provider.Lease{ID: "lease-" + name, RunnerName: name, RunnerID: 42, RunnerScaleSetID: 1}))
	}
}

func speculationScaler(t *testing.T, compute *speculatingCompute, maxWorkers int) *scaler {
	t.Helper()
	scaler := testScaler(t, newWorkerState(), compute)
	scaler.maxWorkers = maxWorkers
	scaler.launchConcurrency = 1
	client := scaler.scaleSetClient.(*fakeRunnerScaleSetClient)
	client.generateJIT = &scaleset.RunnerScaleSetJitRunnerConfig{Runner: &scaleset.RunnerReference{ID: 42}, EncodedJITConfig: "jit"}
	return scaler
}

func runnerOn(t *testing.T, state *workerState, prefix string) string {
	t.Helper()
	for name, record := range state.all() {
		if strings.HasPrefix(record.Worker.ID, prefix) {
			return name
		}
	}
	t.Fatalf("no worker on %s in %v", prefix, state.all())
	return ""
}

func TestSpeculationMovesAWaitingJobToAFreedSlotWithoutDestroyingItsWorker(t *testing.T) {
	compute := &speculatingCompute{room: 1}
	scaler := speculationScaler(t, compute, 10)
	ctx := context.Background()

	// Two jobs: one takes the pool's only slot, the other overflows to a
	// worker that boots a machine.
	if count, err := scaler.HandleDesiredRunnerCount(ctx, 2); err != nil || count != 2 {
		t.Fatalf("desired count = %d, %v", count, err)
	}
	if spares, _ := compute.counts(); spares != 0 {
		t.Fatalf("a full pool must start no speculative runner, got %d", spares)
	}
	pooled := runnerOn(t, scaler.state, "pool:")
	booting := runnerOn(t, scaler.state, "boot:")
	if err := scaler.HandleJobStarted(ctx, &scaleset.JobStarted{RunnerName: pooled}); err != nil {
		t.Fatal(err)
	}

	// The pool job completes and frees its slot while the other job still
	// waits on the booting worker: the same message's desired count moves it.
	if err := scaler.HandleJobCompleted(ctx, &scaleset.JobCompleted{RunnerName: pooled}); err != nil {
		t.Fatal(err)
	}
	if count, err := scaler.HandleDesiredRunnerCount(ctx, 1); err != nil || count != 2 {
		t.Fatalf("desired count after the slot freed = %d, %v", count, err)
	}
	if spares, _ := compute.counts(); spares != 1 {
		t.Fatalf("speculative runners = %d, want 1 in the freed slot", spares)
	}

	// Later passes start nothing more for the same job: its runner in the
	// slot covers it.
	compute.mu.Lock()
	compute.room = 3
	compute.mu.Unlock()
	for range 3 {
		if _, err := scaler.HandleDesiredRunnerCount(ctx, 1); err != nil {
			t.Fatal(err)
		}
	}
	if spares, _ := compute.counts(); spares != 1 {
		t.Fatalf("speculative runners = %d after repeated passes, want still 1", spares)
	}

	// The speculative runner takes the job. The booting worker is left to
	// take the next job or release itself: it may already hold an assignment.
	spare := ""
	for name, record := range scaler.state.all() {
		if strings.HasPrefix(record.Worker.ID, "pool:") && !record.Busy {
			spare = name
		}
	}
	if err := scaler.HandleJobStarted(ctx, &scaleset.JobStarted{RunnerName: spare}); err != nil {
		t.Fatal(err)
	}
	if _, err := scaler.HandleDesiredRunnerCount(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, destroyed := compute.counts(); len(destroyed) != 1 || destroyed[0] == "" || strings.HasPrefix(destroyed[0], "boot:") {
		t.Fatalf("destroyed = %v, want only the completed pool worker", destroyed)
	}
	if _, ok := scaler.state.get(booting); !ok {
		t.Fatal("the booting worker must stay until its idle timeout or the next job")
	}
	if spares, _ := compute.counts(); spares != 1 {
		t.Fatalf("speculative runners = %d once the job started, want 1", spares)
	}
}

func TestSpeculationBounds(t *testing.T) {
	idle := func(state *workerState) func(provider.Worker) {
		return func(worker provider.Worker) { state.add(worker, false) }
	}
	busy := func(state *workerState) func(provider.Worker) {
		return func(worker provider.Worker) { state.add(worker, true) }
	}
	adopted := func(state *workerState) func(provider.Worker) {
		return func(worker provider.Worker) { state.adopt(worker) }
	}
	// A failed regular launch leaves jobs with no worker at all; those are
	// the regular path's to retry, never speculation's.
	failed := &provider.TransientError{Err: errors.New("provider throttled")}
	for name, test := range map[string]struct {
		assigned, maxWorkers, room          int
		booting, inSlots, busySlow, adopted int
		launchErr                           error
		want                                int
	}{
		"one per free slot":                {assigned: 3, maxWorkers: 10, room: 1, booting: 3, want: 1},
		"one per waiting job":              {assigned: 1, maxWorkers: 10, room: 4, booting: 3, want: 1},
		"one per booting worker":           {assigned: 3, maxWorkers: 10, room: 4, booting: 1, launchErr: failed, want: 1},
		"no booting worker to wait on":     {assigned: 2, maxWorkers: 10, room: 4, launchErr: failed, want: 0},
		"started jobs are not waiting":     {assigned: 3, maxWorkers: 10, room: 4, booting: 2, busySlow: 2, want: 1},
		"idle runners in slots cover jobs": {assigned: 2, maxWorkers: 10, room: 4, booting: 2, inSlots: 1, want: 1},
		"never past MAX_RUNNERS":           {assigned: 2, maxWorkers: 3, room: 4, booting: 2, want: 1},
		"MAX_RUNNERS reached":              {assigned: 2, maxWorkers: 2, room: 4, booting: 2, want: 0},
		"adopted workers may run the jobs": {assigned: 1, maxWorkers: 10, room: 4, booting: 1, adopted: 1, want: 0},
		"no free slot":                     {assigned: 2, maxWorkers: 10, room: 0, booting: 2, want: 0},
		"no job waiting":                   {assigned: 0, maxWorkers: 10, room: 4, booting: 2, want: 0},
	} {
		t.Run(name, func(t *testing.T) {
			compute := &speculatingCompute{room: test.room, launchErr: test.launchErr}
			scaler := speculationScaler(t, compute, test.maxWorkers)
			compute.seed(scaler.state, "boot", test.booting, idle(scaler.state))
			compute.seed(scaler.state, "pool", test.inSlots, idle(scaler.state))
			compute.seed(scaler.state, "boot", test.busySlow, busy(scaler.state))
			compute.seed(scaler.state, "boot", test.adopted, adopted(scaler.state))
			before := scaler.state.count()
			if _, err := scaler.HandleDesiredRunnerCount(context.Background(), test.assigned); err != nil {
				t.Fatal(err)
			}
			spares, destroyed := compute.counts()
			if spares != test.want {
				t.Fatalf("speculative runners = %d, want %d", spares, test.want)
			}
			if len(destroyed) != 0 || scaler.state.count() != before+spares {
				t.Fatalf("speculation must only add runners: destroyed %v, %d workers from %d", destroyed, scaler.state.count(), before)
			}
			if scaler.state.count() > test.maxWorkers && spares > 0 {
				t.Fatalf("%d workers exceed MAX_RUNNERS %d", scaler.state.count(), test.maxWorkers)
			}
		})
	}
}

func TestSpeculationKeepsTheLaunchGate(t *testing.T) {
	t.Run("a lost slot leaves the gate open and undoes the launch", func(t *testing.T) {
		compute := &speculatingCompute{room: 1, spareErr: &provider.CapacityError{Reason: "pool_full", Err: errors.New("slot taken")}}
		scaler := speculationScaler(t, compute, 10)
		compute.seed(scaler.state, "boot", 1, func(worker provider.Worker) { scaler.state.add(worker, false) })
		if _, err := scaler.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		if !scaler.launchGateOpen() {
			t.Fatal("a speculative launch that found no slot must not close the launch gate")
		}
		client := scaler.scaleSetClient.(*fakeRunnerScaleSetClient)
		if len(client.removed) != 1 || client.removed[0] != 42 {
			t.Fatalf("the unused registration must be removed, got %v", client.removed)
		}
		if budget := scaler.budget.snapshot(time.Now()); budget.ReservedSeconds != 0 {
			t.Fatalf("the refused launch must refund its reservation: %#v", budget)
		}
		if scaler.retirements.count() != 0 || scaler.state.count() != 1 {
			t.Fatalf("retirements = %d, workers = %d", scaler.retirements.count(), scaler.state.count())
		}
	})
	t.Run("a closed gate stops speculation", func(t *testing.T) {
		compute := &speculatingCompute{room: 4}
		scaler := speculationScaler(t, compute, 10)
		compute.seed(scaler.state, "boot", 2, func(worker provider.Worker) { scaler.state.add(worker, false) })
		scaler.recordLaunchGate(gateCapacity, "fly_machine_limit", &provider.CapacityError{Reason: "fly_machine_limit", Err: errors.New("limit")})
		if _, err := scaler.HandleDesiredRunnerCount(context.Background(), 2); err != nil {
			t.Fatal(err)
		}
		if spares, _ := compute.counts(); spares != 0 {
			t.Fatalf("speculative runners = %d behind a closed gate, want 0", spares)
		}
	})
	t.Run("an exhausted budget stops speculation", func(t *testing.T) {
		compute := &speculatingCompute{room: 4}
		scaler := speculationScaler(t, compute, 10)
		compute.seed(scaler.state, "boot", 2, func(worker provider.Worker) { scaler.state.add(worker, false) })
		budget, err := initializedUsageBudget(t.TempDir()+"/budget.json", 2*time.Hour, 30*24*time.Hour, 2*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if allowed, _, err := budget.reserve("taken", time.Now(), 2*time.Hour); err != nil || !allowed {
			t.Fatalf("reserve = %v, %v", allowed, err)
		}
		scaler.budget = budget
		if _, err := scaler.HandleDesiredRunnerCount(context.Background(), 2); err != nil {
			t.Fatal(err)
		}
		if spares, _ := compute.counts(); spares != 0 {
			t.Fatalf("speculative runners = %d past the budget, want 0", spares)
		}
	})
}

func TestReadyRoomWakesTheListenerForSpeculation(t *testing.T) {
	compute := &speculatingCompute{roomReady: make(chan struct{}, 1), spared: make(chan struct{}, 4)}
	scaler, _ := supervisedScaler(t, compute)
	client := scaler.scaleSetClient.(*fakeRunnerScaleSetClient)
	client.generateJIT = &scaleset.RunnerScaleSetJitRunnerConfig{Runner: &scaleset.RunnerReference{ID: 42}, EncodedJITConfig: "jit"}
	scaler.probeWake = make(chan struct{}, 1)
	session := &blockingSession{reply: make(chan *scaleset.RunnerScaleSetMessage)}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-finished
	})
	go func() {
		defer close(finished)
		done <- runSession(ctx, session, scaler, Config{MaxWorkers: 2, Logger: slog.New(slog.DiscardHandler)}, 1, false)
	}()
	// The session's one assigned job overflows to a booting worker.
	for deadline := time.Now().Add(5 * time.Second); scaler.state.count() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the assigned job got no worker")
		}
		time.Sleep(time.Millisecond)
	}

	// A pool host becomes ready. GitHub sends nothing more; the pool's signal
	// alone moves the waiting job to the new slot.
	compute.mu.Lock()
	compute.room = 2
	compute.mu.Unlock()
	compute.roomReady <- struct{}{}
	select {
	case <-compute.spared:
	case <-time.After(5 * time.Second):
		t.Fatal("a ready pool host did not wake the listener")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("session: %v", err)
	}
	if spares, _ := compute.counts(); spares != 1 {
		t.Fatalf("speculative runners = %d, want 1 for the one waiting job", spares)
	}
}

func TestWakingSessionEndsAPollWhenRoomIsReady(t *testing.T) {
	room := make(chan struct{}, 1)
	session := wakingSession{messageSession: &blockingSession{reply: make(chan *scaleset.RunnerScaleSetMessage)}, room: room}
	room <- struct{}{}
	done := make(chan error, 1)
	go func() {
		message, err := session.GetMessage(context.Background(), 0, 1)
		if message != nil {
			err = errors.New("unexpected message")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a room signal must read as an empty poll: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a room signal did not end the poll")
	}
}
