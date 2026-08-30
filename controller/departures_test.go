package controller

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/gwendall/runneryard/provider"
)

func capturingScaler(t *testing.T, state *workerState, compute provider.Compute) (*scaler, *bytes.Buffer) {
	t.Helper()
	scaler := testScaler(t, state, compute)
	logs := &bytes.Buffer{}
	scaler.logger = slog.New(slog.NewTextHandler(logs, nil))
	scaler.idleTimeout = 10 * time.Minute
	return scaler, logs
}

func TestBusyWorkerDepartureAndItsCompletionAreNotWarnings(t *testing.T) {
	worker := provider.Worker{ID: "worker-one", LeaseID: "lease-one", RunnerName: "runner-00000001", RunnerScaleSetID: 1, CreatedAt: time.Now().Add(-3 * time.Minute)}
	state := newWorkerState()
	state.add(worker, true)
	state.markMissing(worker.RunnerName, time.Now().Add(-inventoryAbsenceGrace-time.Second))
	scaler, logs := capturingScaler(t, state, &fakeCompute{})
	if err := scaler.budget.adopt("lease-one", worker.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if err := scaler.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state.count() != 0 {
		t.Fatal("the departed worker must leave local state")
	}
	if !strings.Contains(logs.String(), "level=INFO msg=\"busy worker left inventory; awaiting its job completion\"") || strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("unexpected departure log:\n%s", logs.String())
	}
	logs.Reset()
	job := &scaleset.JobCompleted{RunnerName: "runner-00000001"}
	job.Result = "succeeded"
	if err := scaler.HandleJobCompleted(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "level=INFO msg=\"job completed after its worker left inventory\"") || !strings.Contains(logs.String(), "result=succeeded") || strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("unexpected completion log:\n%s", logs.String())
	}
	if _, still := state.takeDeparted("runner-00000001"); still {
		t.Fatal("a matched completion must forget the departure")
	}
	logs.Reset()
	if err := scaler.HandleJobCompleted(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "level=WARN msg=\"job completed on worker not present in local state\"") {
		t.Fatalf("a second completion for a forgotten runner must still warn:\n%s", logs.String())
	}
}

func TestIdleWorkerDepartureIsExplainedByItsAge(t *testing.T) {
	for name, age := range map[string]time.Duration{"released itself": 12 * time.Minute, "vanished early": 30 * time.Second} {
		t.Run(name, func(t *testing.T) {
			worker := provider.Worker{ID: "worker-one", LeaseID: "lease-one", RunnerName: "runner-00000001", RunnerScaleSetID: 1, CreatedAt: time.Now().Add(-age)}
			state := newWorkerState()
			state.add(worker, false)
			state.markMissing(worker.RunnerName, time.Now().Add(-inventoryAbsenceGrace-time.Second))
			scaler, logs := capturingScaler(t, state, &fakeCompute{})
			if err := scaler.budget.adopt("lease-one", worker.CreatedAt); err != nil {
				t.Fatal(err)
			}
			if err := scaler.reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			expected := "level=WARN msg=\"worker disappeared before starting a job\""
			if age >= scaler.idleTimeout {
				expected = "level=INFO msg=\"idle worker released itself\""
			}
			if !strings.Contains(logs.String(), expected) {
				t.Fatalf("departure log lacks %q:\n%s", expected, logs.String())
			}
		})
	}
}

func TestReconcileToleratesOneTransientInventoryOmissionBeforeJobStarts(t *testing.T) {
	worker := provider.Worker{
		ID:               "worker-one",
		LeaseID:          "lease-one",
		RunnerName:       "runner-00000001",
		RunnerScaleSetID: 1,
		CreatedAt:        time.Now().Add(-time.Minute),
	}
	state := newWorkerState()
	state.add(worker, false)
	compute := &fakeCompute{workers: []provider.Worker{worker}}
	scaler, logs := capturingScaler(t, state, compute)

	// Fly inventory can omit a recently created Machine for one snapshot while
	// GitHub is already assigning it. That observation must not erase the
	// runner before the corresponding JobStarted message arrives.
	compute.workers = nil
	if err := scaler.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := state.get(worker.RunnerName); !ok {
		t.Fatal("one missing inventory snapshot removed a runner still awaiting assignment")
	}

	if err := scaler.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: worker.RunnerName}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "job started on worker not present in local state") {
		t.Fatalf("transient inventory omission lost assignment correlation:\n%s", logs.String())
	}
}

func TestReappearanceClearsInventoryAbsenceGrace(t *testing.T) {
	worker := provider.Worker{
		ID:         "worker-one",
		LeaseID:    "lease-one",
		RunnerName: "runner-00000001",
		CreatedAt:  time.Now().Add(-time.Minute),
	}
	state := newWorkerState()
	state.add(worker, false)
	compute := &fakeCompute{}
	scaler, _ := capturingScaler(t, state, compute)

	if err := scaler.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	compute.workers = []provider.Worker{worker}
	if err := scaler.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	compute.workers = nil
	if err := scaler.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	record, ok := state.get(worker.RunnerName)
	if !ok {
		t.Fatal("a new isolated omission reused the previous grace window")
	}
	if record.MissingSince.IsZero() || time.Since(record.MissingSince) >= inventoryAbsenceGrace {
		t.Fatalf("new omission did not start a fresh grace window: %#v", record)
	}
}

func TestAdoptedWorkerDepartureIsInformational(t *testing.T) {
	worker := provider.Worker{ID: "worker-one", LeaseID: "lease-one", RunnerName: "runner-00000001", RunnerScaleSetID: 1, CreatedAt: time.Now().Add(-time.Minute)}
	state := newWorkerState()
	state.adopt(worker)
	state.markMissing(worker.RunnerName, time.Now().Add(-inventoryAbsenceGrace-time.Second))
	scaler, logs := capturingScaler(t, state, &fakeCompute{})
	if err := scaler.budget.adopt("lease-one", worker.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if err := scaler.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "level=INFO msg=\"adopted worker left inventory\"") || strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("unexpected departure log:\n%s", logs.String())
	}
}

func TestDeparturesAreForgottenAfterTheMemoryWindow(t *testing.T) {
	state := newWorkerState()
	worker := provider.Worker{ID: "worker-old", RunnerName: "runner-00000009"}
	state.markDeparted(workerRecord{Worker: worker, Busy: true}, time.Now().Add(-departureMemory-time.Minute))
	state.markDeparted(workerRecord{Worker: provider.Worker{ID: "worker-new", RunnerName: "runner-00000010"}, Busy: true}, time.Now())
	scaler, logs := capturingScaler(t, state, &fakeCompute{})
	if err := scaler.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, known := state.takeDeparted("runner-00000009"); known {
		t.Fatal("a departure older than the memory window must be pruned")
	}
	if _, known := state.takeDeparted("runner-00000010"); !known {
		t.Fatal("a recent departure must survive reconciliation")
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("pruning must not warn:\n%s", logs.String())
	}
}
