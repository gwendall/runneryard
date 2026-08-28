package controller

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFleetStatusCoversEmptySaturatedAndDegradedStates(t *testing.T) {
	statusFile := filepath.Join(t.TempDir(), "status.json")
	reporter, err := newStatusReporter(Config{
		GitHubURL: "https://github.com/acme/widgets", ScaleSetName: "acme-linux",
		ControllerID: "acme-linux", Provider: "fly", MaxWorkers: 4,
		StatusFile: statusFile, Version: "1.2.3", CommitSHA: "abcdef", Logger: slog.New(slog.DiscardHandler),
	}, BudgetStatus{LimitSeconds: 3600, RemainingSeconds: 3600, WindowSeconds: 86400})
	if err != nil {
		t.Fatal(err)
	}
	reporter.githubActivity("session_created")
	empty := loadFleetStatus(t, statusFile)
	if empty.Health != "ready" || empty.Workers.Actual != 0 {
		t.Fatalf("empty status = %#v", empty)
	}

	reporter.desired(8, 4)
	reporter.workers(4, 2, 1, 1)
	saturated := loadFleetStatus(t, statusFile)
	if !saturated.Workers.Saturated || saturated.GitHub.DesiredWorkers != 4 {
		t.Fatalf("saturated status = %#v", saturated)
	}

	reporter.orphans(1)
	degraded := loadFleetStatus(t, statusFile)
	if degraded.Health != "degraded" || degraded.Reason != "orphan_candidates" {
		t.Fatalf("degraded status = %#v", degraded)
	}

	reporter.retirements(2)
	retiring := loadFleetStatus(t, statusFile)
	if retiring.Health != "degraded" || retiring.Reason != "runner_retirements_pending" || retiring.Workers.PendingRetirements != 2 {
		t.Fatalf("retirement status = %#v", retiring)
	}
}

func TestFleetStatusSeparatesLatencyAndExhaustedBudget(t *testing.T) {
	statusFile := filepath.Join(t.TempDir(), "status.json")
	reporter, err := newStatusReporter(Config{
		GitHubURL: "https://github.com/acme/widgets", ScaleSetName: "acme-linux",
		ControllerID: "acme-linux", Provider: "hetzner", MaxWorkers: 2,
		StatusFile: statusFile, Logger: slog.New(slog.DiscardHandler),
	}, BudgetStatus{LimitSeconds: 3600, RemainingSeconds: 3600, WindowSeconds: 86400})
	if err != nil {
		t.Fatal(err)
	}
	reporter.githubActivity("session_created")
	reporter.latency(true, 4*time.Second, false)
	reporter.latency(true, 6*time.Second, true)
	reporter.latency(false, 2*time.Second, false)
	reporter.budget(BudgetStatus{
		LimitSeconds: 3600, ReservedSeconds: 3600, RemainingSeconds: 0, WindowSeconds: 86400,
		RefusalReason: "usage_budget_exhausted", NextAvailableAt: time.Now().Add(time.Hour),
	})
	status := loadFleetStatus(t, statusFile)
	if status.Latency.ProviderCreate.Samples != 2 || status.Latency.ProviderCreate.Failures != 1 || status.Latency.ProviderCreate.AverageMS != 5000 {
		t.Fatalf("provider latency = %#v", status.Latency.ProviderCreate)
	}
	if status.Latency.Assignment.Samples != 1 || status.Latency.Assignment.LastMS != 2000 {
		t.Fatalf("assignment latency = %#v", status.Latency.Assignment)
	}
	if status.Health != "degraded" || status.Budget.RefusalReason != "usage_budget_exhausted" {
		t.Fatalf("budget status = %#v", status)
	}
}

func TestUsageBudgetSnapshotSeparatesUsedReservedAndRemaining(t *testing.T) {
	now := time.Now()
	budget, err := initializedUsageBudget(filepath.Join(t.TempDir(), "budget.json"), 3*time.Hour, 24*time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, _, err := budget.reserve("used", now.Add(-10*time.Minute), time.Hour); err != nil || !allowed {
		t.Fatalf("reserve used = %t, %v", allowed, err)
	}
	if err := budget.settle("used", now.Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if allowed, _, err := budget.reserve("reserved", now, time.Hour); err != nil || !allowed {
		t.Fatalf("reserve active = %t, %v", allowed, err)
	}
	snapshot := budget.snapshot(now)
	if snapshot.UsedSeconds != 300 || snapshot.ReservedSeconds != 3600 || snapshot.RemainingSeconds != 6900 || snapshot.RefusalReason != "" {
		t.Fatalf("budget snapshot = %#v", snapshot)
	}
}

func TestFleetStatusFileIsPrivateAtomicAndRejectsSymlinks(t *testing.T) {
	directory := t.TempDir()
	statusFile := filepath.Join(directory, "status.json")
	now := time.Now().UTC()
	status := FleetStatus{
		SchemaVersion: statusSchemaVersion, UpdatedAt: now, StartedAt: now, Health: "ready",
		Controller: ControllerStatus{ID: "one", Provider: "fly"},
		Workers:    WorkerStatus{Maximum: 4},
		Budget:     BudgetStatus{LimitSeconds: 1, RemainingSeconds: 1, WindowSeconds: 1},
	}
	if err := writeStatusFile(statusFile, status); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(statusFile)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("status mode = %o", info.Mode().Perm())
	}
	if _, err := LoadStatus(statusFile); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(directory, "outside.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(directory, "linked.json")
	if err := os.Symlink(target, linked); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := LoadStatus(linked); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func TestFleetStatusRejectsNegativePendingRetirements(t *testing.T) {
	statusFile := filepath.Join(t.TempDir(), "status.json")
	now := time.Now().UTC()
	status := FleetStatus{
		SchemaVersion: statusSchemaVersion, UpdatedAt: now, StartedAt: now, Health: "ready",
		Controller: ControllerStatus{ID: "one", Provider: "fly"},
		Workers:    WorkerStatus{Maximum: 4, PendingRetirements: -1},
		Budget:     BudgetStatus{LimitSeconds: 1, RemainingSeconds: 1, WindowSeconds: 1},
	}
	if err := writeStatusFile(statusFile, status); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStatus(statusFile); err == nil {
		t.Fatal("expected negative pending retirements to be rejected")
	}
}

func TestFleetStatusSchemaContainsNoWorkloadOrCredentialFields(t *testing.T) {
	now := time.Now().UTC()
	status := FleetStatus{SchemaVersion: statusSchemaVersion, UpdatedAt: now, StartedAt: now, Health: "ready"}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"jit", "token", "secret", "job_id", "repository_payload", "runner_name"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("status schema exposed forbidden field %q: %s", forbidden, encoded)
		}
	}
}

func loadFleetStatus(t *testing.T, path string) FleetStatus {
	t.Helper()
	status, err := LoadStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	return status
}
