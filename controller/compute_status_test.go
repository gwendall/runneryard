package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gwendall/runneryard/provider"
)

type reportingCompute struct {
	fakeCompute
	mu      sync.Mutex
	state   any
	started bool
	stopped bool
}

func (c *reportingCompute) ComputeStatus() any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *reportingCompute) Run(ctx context.Context) error {
	c.mu.Lock()
	c.started = true
	c.mu.Unlock()
	<-ctx.Done()
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	return ctx.Err()
}

var (
	_ provider.StatusReporter = (*reportingCompute)(nil)
	_ provider.Maintainer     = (*reportingCompute)(nil)
)

func TestFleetStatusPublishesTheComputeState(t *testing.T) {
	statusFile := filepath.Join(t.TempDir(), "status.json")
	reporter, err := newStatusReporter(Config{
		GitHubURL: "https://github.com/acme/widgets", ScaleSetName: "acme-linux",
		ControllerID: "acme-linux", Provider: "pool", MaxWorkers: 4,
		StatusFile: statusFile, Version: "1.2.3", CommitSHA: "abcdef", Logger: slog.New(slog.DiscardHandler),
	}, BudgetStatus{LimitSeconds: 3600, RemainingSeconds: 3600, WindowSeconds: 86400})
	if err != nil {
		t.Fatal(err)
	}
	if status := loadFleetStatus(t, statusFile); status.Compute != nil {
		t.Fatalf("compute without its own state published %s", status.Compute)
	}

	compute := &reportingCompute{state: map[string]any{"tiers": []string{"warm", "ephemeral"}}}
	reporter.watchCompute(compute)
	if got := compactJSON(t, loadFleetStatus(t, statusFile).Compute); got != `{"tiers":["warm","ephemeral"]}` {
		t.Fatalf("compute status = %s", got)
	}

	compute.mu.Lock()
	compute.state = map[string]any{"tiers": []string{"ephemeral"}}
	compute.mu.Unlock()
	reporter.sampleCompute()
	if got := compactJSON(t, loadFleetStatus(t, statusFile).Compute); got != `{"tiers":["ephemeral"]}` {
		t.Fatalf("resampled compute status = %s", got)
	}

	compute.mu.Lock()
	compute.state = make(chan int)
	compute.mu.Unlock()
	reporter.sampleCompute()
	if got := compactJSON(t, loadFleetStatus(t, statusFile).Compute); got != `{"tiers":["ephemeral"]}` {
		t.Fatalf("an unencodable state replaced the last good one: %s", got)
	}
}

func TestRunStopsComputeMaintenanceBeforeReturning(t *testing.T) {
	dir := t.TempDir()
	budgetFile := filepath.Join(dir, "budget.json")
	if err := InitializeUsageBudget(budgetFile); err != nil {
		t.Fatal(err)
	}
	retirementFile := filepath.Join(dir, "retirements.json")
	if err := os.WriteFile(retirementFile, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	compute := &reportingCompute{state: map[string]any{}}
	fleet := &Controller{
		config: Config{
			GitHubURL: "https://github.com/acme/widgets", ScaleSetName: "acme-linux",
			ControllerID: "acme-linux", Provider: "pool", MaxWorkers: 4,
			MaxLifetime: time.Hour, UsageBudget: 10 * time.Hour, BudgetWindow: 24 * time.Hour, BudgetFile: budgetFile,
			RetirementFile: retirementFile, StatusFile: filepath.Join(dir, "status.json"), Logger: slog.New(slog.DiscardHandler),
		},
		compute: compute,
	}
	// The unreadable retirement ledger stops Run right after maintenance began.
	if err := fleet.Run(context.Background()); err == nil {
		t.Fatal("Run accepted an unreadable retirement ledger")
	}
	compute.mu.Lock()
	defer compute.mu.Unlock()
	if !compute.started || !compute.stopped {
		t.Fatalf("maintenance started=%v stopped=%v, want it run and stopped before Run returned", compute.started, compute.stopped)
	}
	var status FleetStatus
	data, err := os.ReadFile(filepath.Join(dir, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &status); err != nil || compactJSON(t, status.Compute) != "{}" {
		t.Fatalf("status compute = %s, %v", status.Compute, err)
	}
}

func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatalf("compute status %q: %v", raw, err)
	}
	return compact.String()
}
