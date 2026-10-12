package main

import (
	"bytes"
	"encoding/json"
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
		Capacity:   controller.CapacityStatus{Configured: 4, Effective: 3, Rejections: 1, Rejection: "fly_machine_limit", RetryAt: now.Add(time.Minute)},
		Budget:     controller.BudgetStatus{LimitSeconds: 7200, UsedSeconds: 1800, ReservedSeconds: 3600, RemainingSeconds: 1800, WindowSeconds: 86400},
	}
	var human bytes.Buffer
	if err := writeFleetStatus(&human, status, false); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"RunnerYard degraded", "4 actual", "2 busy", "1 unknown", "2 retirement(s) pending", "3 effective / 4 configured", "fly_machine_limit", "30m0s used", "desired_count"} {
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

func TestWriteFleetStatusPrintsPoolUsage(t *testing.T) {
	since := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	status := controller.FleetStatus{
		SchemaVersion: 2,
		Compute: json.RawMessage(`{"tiers":["warm","ephemeral"],"pools":[{"name":"warm","currency":"USD",
			"measured_since":"2026-10-12T09:00:00Z","occupancy":0.413,"ready_occupancy":0.45,"measured_cost":6.6,
			"ephemeral_equivalent":{"tier":"ephemeral","hourly_price":0.4,"currency":"USD","cost":5.28},
			"hosts":[{"id":"host-1","phase":"ready","slots":4,"running_seconds":36000,"occupancy":0.5,"ready_occupancy":0.52},
				{"id":"host-2","phase":"stopped","slots":4,"running_seconds":14400,"occupancy":0.2,"ready_occupancy":0.25}]}]}`),
	}
	var output bytes.Buffer
	if err := writeFleetStatus(&output, status, false); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"Pool        warm  2 host(s)  occupancy 41.3% (ready 45.0%)  cost USD 6.60  as ephemeral USD 5.28  since " + since.Format(time.RFC3339),
		"  host-1  ready  4 slot(s)  running 10h0m0s  occupancy 50.0% (ready 52.0%)",
		"  host-2  stopped  4 slot(s)  running 4h0m0s  occupancy 20.0% (ready 25.0%)",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("output missing %q:\n%s", expected, output.String())
		}
	}

	status.Compute = json.RawMessage(`{"tiers":["ephemeral"],"pools":[]}`)
	output.Reset()
	if err := writeFleetStatus(&output, status, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "Pool ") {
		t.Fatalf("output without pools = %s", output.String())
	}
}

func TestWriteFleetStatusFallsBackForPreCapacitySchemaV2Files(t *testing.T) {
	status := controller.FleetStatus{
		SchemaVersion: 2,
		Workers:       controller.WorkerStatus{Maximum: 7},
	}
	var output bytes.Buffer
	if err := writeFleetStatus(&output, status, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "7 effective / 7 configured") {
		t.Fatalf("legacy status output = %q", output.String())
	}
}
