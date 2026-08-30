package controller

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/gwendall/runneryard/provider"
)

func TestDeferredRetirementDegradesOnlyAfterTheGrace(t *testing.T) {
	created := time.Now().Add(-5 * time.Minute)
	worker := provider.Worker{ID: "worker-one", LeaseID: "lease-one", RunnerName: "runner-00000001", RunnerID: 5837, RunnerScaleSetID: 1, CreatedAt: created}
	compute := &fakeCompute{workers: []provider.Worker{worker}, removeBeforeDestroy: true}
	state := newWorkerState()
	state.add(worker, true)
	scaler := testScaler(t, state, compute)
	statusFile := filepath.Join(t.TempDir(), "status.json")
	reporter, err := newStatusReporter(Config{
		GitHubURL: "https://github.com/acme/widgets", ScaleSetName: "acme-linux",
		ControllerID: "acme-linux", Provider: "fly", MaxWorkers: 4,
		StatusFile: statusFile, Logger: slog.New(slog.DiscardHandler),
	}, BudgetStatus{})
	if err != nil {
		t.Fatal(err)
	}
	scaler.reporter = reporter
	client := scaler.scaleSetClient.(*fakeRunnerScaleSetClient)
	client.runners["runner-00000001"] = &scaleset.RunnerReference{ID: 5837, Name: "runner-00000001", RunnerScaleSetID: 1}
	client.removeErr = scaleset.JobStillRunningError
	if err := scaler.budget.adopt("lease-one", created); err != nil {
		t.Fatal(err)
	}
	if err := scaler.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "runner-00000001"}); err != nil {
		t.Fatalf("a deferred GitHub cleanup must not fail the completion: %v", err)
	}
	fresh := loadFleetStatus(t, statusFile)
	if fresh.Workers.PendingRetirements != 1 || fresh.Workers.OverdueRetirements != 0 {
		t.Fatalf("a fresh deferral must be pending but not overdue: %#v", fresh.Workers)
	}
	if fresh.Reason == "runner_retirements_pending" {
		t.Fatalf("a fresh deferral must not degrade the fleet: %#v", fresh)
	}
	scaler.retirements.mu.Lock()
	for name, entry := range scaler.retirements.entries {
		entry.RequestedAt = time.Now().Add(-retirementGrace - time.Minute)
		scaler.retirements.entries[name] = entry
	}
	scaler.retirements.mu.Unlock()
	scaler.reportRetirements()
	aged := loadFleetStatus(t, statusFile)
	if aged.Health != "degraded" || aged.Reason != "runner_retirements_pending" || aged.Workers.OverdueRetirements != 1 {
		t.Fatalf("a retirement older than the grace must degrade the fleet: %#v", aged)
	}
	client.removeErr = nil
	if err := scaler.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	cleared := loadFleetStatus(t, statusFile)
	if cleared.Workers.PendingRetirements != 0 || cleared.Workers.OverdueRetirements != 0 || cleared.Reason == "runner_retirements_pending" {
		t.Fatalf("a finished retirement must clear the degraded state: %#v", cleared)
	}
}
