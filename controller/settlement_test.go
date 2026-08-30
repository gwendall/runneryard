package controller

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/gwendall/runneryard/provider"
)

func TestSettleAtChargesTheWorkerLifetimeNotTheMessageDelay(t *testing.T) {
	budget, err := initializedUsageBudget(filepath.Join(t.TempDir(), "budget.json"), 100*time.Hour, 30*24*time.Hour, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-10 * time.Minute)
	if allowed, _, err := budget.reserve("lease", start, 2*time.Hour); err != nil || !allowed {
		t.Fatalf("reservation failed: %v %v", allowed, err)
	}
	stopped := start.Add(90 * time.Second)
	settled := start.Add(10 * time.Minute)
	if err := budget.settleAt("lease", stopped, settled); err != nil {
		t.Fatal(err)
	}
	if got := budget.entries[0].Seconds; got != 90 {
		t.Fatalf("charged %d seconds, want the 90s worker lifetime", got)
	}
	if !budget.entries[0].ChargedAt.Equal(settled.UTC()) {
		t.Fatal("the charge must be recorded at settlement time for window expiry")
	}
}

func TestSettleAtFallsBackToNowForUnknownOrImplausibleStops(t *testing.T) {
	budget, err := initializedUsageBudget(filepath.Join(t.TempDir(), "budget.json"), 100*time.Hour, 30*24*time.Hour, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-5 * time.Minute)
	now := start.Add(5 * time.Minute)
	for name, stopped := range map[string]time.Time{"zero": {}, "before start": start.Add(-time.Minute), "in the future": now.Add(time.Hour)} {
		leaseID := "lease-" + name
		if allowed, _, err := budget.reserve(leaseID, start, 2*time.Hour); err != nil || !allowed {
			t.Fatalf("%s: reservation failed: %v %v", name, allowed, err)
		}
		if err := budget.settleAt(leaseID, stopped, now); err != nil {
			t.Fatal(err)
		}
		for _, entry := range budget.entries {
			if entry.LeaseID == leaseID && entry.Seconds != 300 {
				t.Fatalf("%s: charged %d seconds, want the 300s until now", name, entry.Seconds)
			}
		}
	}
}

func TestCompletionSettlesAtGitHubFinishTimePlusTeardown(t *testing.T) {
	created := time.Now().Add(-20 * time.Minute)
	worker := provider.Worker{ID: "worker-one", LeaseID: "lease-one", RunnerName: "runner-00000001", RunnerScaleSetID: 1, CreatedAt: created}
	compute := &fakeCompute{workers: []provider.Worker{worker}, removeBeforeDestroy: true}
	state := newWorkerState()
	state.add(worker, true)
	scaler := testScaler(t, state, compute)
	if err := scaler.budget.adopt("lease-one", created); err != nil {
		t.Fatal(err)
	}
	finished := created.Add(2 * time.Minute)
	job := &scaleset.JobCompleted{RunnerName: "runner-00000001"}
	job.FinishTime = finished
	if err := scaler.HandleJobCompleted(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var charged int64
	for _, entry := range scaler.budget.entries {
		if entry.LeaseID == "lease-one" {
			charged = entry.Seconds
		}
	}
	want := int64((2*time.Minute + workerTeardownGrace) / time.Second)
	if charged != want {
		t.Fatalf("charged %d seconds, want %d (finish time plus teardown grace, not the 20 minutes until the message)", charged, want)
	}
}

func TestSnapshotReportsBurnAndHorizon(t *testing.T) {
	budget, err := initializedUsageBudget(filepath.Join(t.TempDir(), "budget.json"), 100*time.Hour, 30*24*time.Hour, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// Three settled hours, the oldest charged five hours ago: 3h over 5h,
	// extrapolated to 14.4 hours per day.
	for i, leaseID := range []string{"a", "b", "c"} {
		start := now.Add(-time.Duration(6-i) * time.Hour)
		if allowed, _, err := budget.reserve(leaseID, start, 2*time.Hour); err != nil || !allowed {
			t.Fatalf("reservation %s failed: %v %v", leaseID, allowed, err)
		}
		if err := budget.settleAt(leaseID, start.Add(time.Hour), start.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	status := budget.snapshot(now)
	if status.BurnSecondsPerDay < 14*3600 || status.BurnSecondsPerDay > 15*3600 {
		t.Fatalf("burn per day = %ds, want about 14.4h", status.BurnSecondsPerDay)
	}
	// 97 hours remain at ~14.4 hours per day: about 6.7 days of horizon.
	if status.HorizonSeconds < 6*24*3600 || status.HorizonSeconds > 7*24*3600 {
		t.Fatalf("horizon = %ds, want about 6.7 days", status.HorizonSeconds)
	}
	empty, err := initializedUsageBudget(filepath.Join(t.TempDir(), "empty.json"), 100*time.Hour, 30*24*time.Hour, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := empty.snapshot(now); snapshot.BurnSecondsPerDay != 0 || snapshot.HorizonSeconds != 0 {
		t.Fatalf("an unused ledger must report no burn and no horizon, got %#v", snapshot)
	}
}
