package fly

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gwendall/runneryard/provider"
)

func TestLaunchIsEphemeralAndReceivesOnlyJITCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer fly-token" {
			t.Fatalf("unexpected authorization header %q", got)
		}
		var request createMachineRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if !request.Config.AutoDestroy || request.Config.Restart.Policy != "no" {
			t.Fatalf("worker must auto-destroy without restart: %#v", request.Config)
		}
		if len(request.Config.Processes) != 1 || !request.Config.Processes[0].IgnoreAppSecrets {
			t.Fatalf("worker process must ignore app secrets: %#v", request.Config.Processes)
		}
		if len(request.Config.Processes[0].Env) != 2 || request.Config.Processes[0].Env["ACTIONS_RUNNER_INPUT_JITCONFIG"] != "jit-secret" || request.Config.Processes[0].Env["RUNNERYARD_DEADLINE"] == "" {
			t.Fatalf("worker received unexpected process environment: %#v", request.Config.Processes[0].Env)
		}
		if request.Config.Metadata[controllerIDKey] != "test-controller" || request.Config.Metadata[leaseIDKey] != "lease-one" || request.Config.Metadata[runnerIDKey] != "42" || request.Config.Metadata[runnerScaleSetIDKey] != "7" {
			t.Fatal("ownership metadata missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(machine{
			ID: "machine-id", Name: request.Name, Region: request.Region, CreatedAt: time.Now(), Config: request.Config,
		})
	}))
	defer server.Close()

	adapter := testAdapter(t, server)
	worker, err := adapter.Launch(context.Background(), provider.Lease{
		ID: "lease-one", RunnerName: "runner-one", RunnerID: 42, RunnerScaleSetID: 7,
		JITConfig: "jit-secret", Deadline: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if worker.ID != "machine-id" || worker.LeaseID != "lease-one" || worker.RunnerID != 42 || worker.RunnerScaleSetID != 7 {
		t.Fatalf("unexpected worker %#v", worker)
	}
}

func TestInventoryExcludesForeignMachines(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]machine{
			{ID: "owned", Config: machineConfig{Metadata: map[string]string{managedByKey: "true", controllerIDKey: "test-controller"}}},
			{ID: "foreign", Config: machineConfig{Metadata: map[string]string{managedByKey: "true", controllerIDKey: "someone-else"}}},
		})
	}))
	defer server.Close()

	workers, err := testAdapter(t, server).Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 || workers[0].ID != "owned" {
		t.Fatalf("unexpected inventory %#v", workers)
	}
}

func TestDestroyTreatsAlreadyDestroyedAsSuccess(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	if err := testAdapter(t, server).Destroy(context.Background(), "gone"); err != nil {
		t.Fatal(err)
	}
}

func TestFlyDurationAcceptsAPIStringAndMarshalsNanoseconds(t *testing.T) {
	var duration flyDuration
	if err := json.Unmarshal([]byte(`"30s"`), &duration); err != nil {
		t.Fatal(err)
	}
	if time.Duration(duration) != 30*time.Second {
		t.Fatalf("unexpected duration %s", time.Duration(duration))
	}
	encoded, err := json.Marshal(duration)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "30000000000" {
		t.Fatalf("unexpected encoded duration %s", encoded)
	}
}

func testAdapter(t *testing.T, server *httptest.Server) *Adapter {
	t.Helper()
	adapter, err := New(Config{
		APIBaseURL: server.URL, APIToken: "fly-token", App: "ci-runners", Region: "cdg",
		Image: "registry.fly.io/ci-runners:test", ControllerID: "test-controller",
		CPUKind: "shared", CPUs: 4, MemoryMB: 8192, RootFSGB: 30, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}
