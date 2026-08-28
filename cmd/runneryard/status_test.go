package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gwendall/runneryard/controller"
)

func TestWriteFleetStatusHumanAndJSON(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	status := controller.FleetStatus{
		SchemaVersion: 2, UpdatedAt: now, StartedAt: now, Health: "degraded", Reason: "usage_budget_exhausted",
		Controller: controller.ControllerStatus{ID: "acme", Provider: "fly", Version: "1.2.3", CommitSHA: "abcdef"},
		GitHub:     controller.GitHubStatus{ScaleSet: "acme-linux", AssignedJobs: 5, DesiredWorkers: 4, LastActivityAt: now, LastEvent: "desired_count"},
		Workers:    controller.WorkerStatus{Actual: 4, Busy: 2, Idle: 1, Unknown: 1, PendingRetirements: 2, Maximum: 4, Saturated: true},
		Budget:     controller.BudgetStatus{LimitSeconds: 7200, UsedSeconds: 1800, ReservedSeconds: 3600, RemainingSeconds: 1800, WindowSeconds: 86400},
	}
	var human bytes.Buffer
	if err := writeFleetStatus(&human, status, false); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"RunnerYard degraded", "4 actual", "2 busy", "1 unknown", "2 retirement(s) pending", "30m0s used", "desired_count"} {
		if !strings.Contains(human.String(), expected) {
			t.Fatalf("human output missing %q:\n%s", expected, human.String())
		}
	}
	var machine bytes.Buffer
	if err := writeFleetStatus(&machine, status, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(machine.String(), `"schema_version":2`) || !strings.Contains(machine.String(), `"unknown":1`) || !strings.Contains(machine.String(), `"saturated":true`) {
		t.Fatalf("JSON output = %s", machine.String())
	}
}
