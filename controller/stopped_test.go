package controller

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
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

// stoppedScaler retires one stopped worker that this controller launched.
func stoppedScaler(t *testing.T, state string, busy bool, age time.Duration) (*scaler, *workerState, *bytes.Buffer) {
	t.Helper()
	worker := provider.Worker{ID: "slot", LeaseID: "lease-slot", RunnerName: "runner-00000ccc", RunnerScaleSetID: 1, State: state, CreatedAt: time.Now().Add(-age)}
	workers := newWorkerState()
	workers.add(worker, busy)
	scaler, logs := capturingScaler(t, workers, &fakeCompute{workers: []provider.Worker{worker}, removeBeforeDestroy: true})
	if err := scaler.budget.adopt(worker.LeaseID, worker.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if err := scaler.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if workers.count() != 0 {
		t.Fatal("the stopped worker must be retired")
	}
	return scaler, workers, logs
}

func completeJob(t *testing.T, scaler *scaler, runner string) {
	t.Helper()
	job := &scaleset.JobCompleted{RunnerName: runner}
	job.Result = "succeeded"
	if err := scaler.HandleJobCompleted(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerStoppedAfterItsJobIsNotAWarning(t *testing.T) {
	for _, state := range []string{"stopped", "off"} {
		t.Run(state, func(t *testing.T) {
			scaler, _, logs := stoppedScaler(t, state, true, 3*time.Minute)
			if !strings.Contains(logs.String(), `level=INFO msg="retired stopped worker; awaiting its job completion"`) {
				t.Fatalf("unexpected stop log:\n%s", logs.String())
			}
			completeJob(t, scaler, "runner-00000ccc")
			if !strings.Contains(logs.String(), `level=INFO msg="job completed after its worker stopped"`) || strings.Contains(logs.String(), "level=WARN") {
				t.Fatalf("a job completed after its slot stopped must not warn:\n%s", logs.String())
			}
		})
	}
}

func TestJobMessagesAfterAStoppedRetirementAreMatched(t *testing.T) {
	// A short job can finish before GitHub's start message is read: the
	// worker is retired as stopped before it was ever marked busy.
	scaler, workers, logs := stoppedScaler(t, "stopped", false, 20*time.Second)
	if err := scaler.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: "runner-00000ccc"}); err != nil {
		t.Fatal(err)
	}
	completeJob(t, scaler, "runner-00000ccc")
	if !strings.Contains(logs.String(), `level=INFO msg="job started after its worker was retired"`) ||
		!strings.Contains(logs.String(), `level=INFO msg="job completed after its worker stopped"`) || strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("job messages after a stop must be matched:\n%s", logs.String())
	}
	if overdue := workers.takeOverdue(time.Now().Add(time.Hour)); len(overdue) != 0 {
		t.Fatalf("a completed job must not be reported overdue: %+v", overdue)
	}
}

func TestStoppedWorkerWithoutCompletionWarnsOnce(t *testing.T) {
	scaler, workers, logs := stoppedScaler(t, "stopped", true, 3*time.Minute)
	workers.mu.Lock()
	stopped := workers.departed["runner-00000ccc"]
	stopped.At = time.Now().Add(-stoppedCompletionGrace - time.Second)
	workers.departed["runner-00000ccc"] = stopped
	workers.mu.Unlock()

	for range 2 {
		if err := scaler.reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if count := strings.Count(logs.String(), `level=WARN msg="stopped worker never reported its job completion"`); count != 1 {
		t.Fatalf("a stop without completion must warn once, got %d:\n%s", count, logs.String())
	}
	logs.Reset()
	completeJob(t, scaler, "runner-00000ccc")
	if !strings.Contains(logs.String(), `level=INFO msg="job completed after its worker stopped"`) {
		t.Fatalf("a late completion must still be matched:\n%s", logs.String())
	}
}

func TestAbnormalOrUnknownStopsStayWarnings(t *testing.T) {
	t.Run("failed", func(t *testing.T) {
		_, workers, logs := stoppedScaler(t, "failed", true, 3*time.Minute)
		if !strings.Contains(logs.String(), `level=WARN msg="retired stopped worker"`) {
			t.Fatalf("a failed worker must warn:\n%s", logs.String())
		}
		if _, remembered := workers.takeDeparted("runner-00000ccc"); remembered {
			t.Fatal("a failed worker must not be awaited as a routine stop")
		}
	})
	t.Run("unknown", func(t *testing.T) {
		worker := provider.Worker{ID: "slot", LeaseID: "lease-slot", RunnerName: "runner-00000ddd", RunnerScaleSetID: 1, State: "stopped", CreatedAt: time.Now().Add(-time.Minute)}
		scaler, logs := capturingScaler(t, newWorkerState(), &fakeCompute{workers: []provider.Worker{worker}, removeBeforeDestroy: true})
		if err := scaler.reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(logs.String(), `level=WARN msg="retired stopped worker"`) {
			t.Fatalf("a stopped worker this controller does not know must warn:\n%s", logs.String())
		}
	})
}

func TestIdleWorkerThatStoppedItselfIsNotAwaited(t *testing.T) {
	_, workers, logs := stoppedScaler(t, "stopped", false, 12*time.Minute)
	if !strings.Contains(logs.String(), `level=INFO msg="idle worker stopped itself"`) || strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("unexpected idle stop log:\n%s", logs.String())
	}
	if overdue := workers.takeOverdue(time.Now().Add(time.Hour)); len(overdue) != 0 {
		t.Fatalf("an idle stop has no job to await: %+v", overdue)
	}
}
