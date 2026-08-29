package fly

import (
	"context"
	"encoding/json"
	"io"
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
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err := json.Unmarshal(body, &wire); err != nil {
			t.Fatal(err)
		}
		config, ok := wire["config"].(map[string]any)
		if !ok {
			t.Fatalf("machine request has no config object: %#v", wire)
		}
		rootFS, ok := config["rootfs"].(map[string]any)
		if !ok || rootFS["size_gb"] != float64(30) {
			t.Fatalf("worker rootfs must use Fly's config.rootfs.size_gb schema: %#v", config)
		}
		if _, legacy := config["rootfs_size_gb"]; legacy {
			t.Fatalf("worker request contains Fly's ignored legacy rootfs_size_gb field: %#v", config)
		}
		var request createMachineRequest
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		if !request.Config.AutoDestroy || request.Config.Restart.Policy != "no" {
			t.Fatalf("worker must auto-destroy without restart: %#v", request.Config)
		}
		if len(request.Config.Processes) != 1 || !request.Config.Processes[0].IgnoreAppSecrets {
			t.Fatalf("worker process must ignore app secrets: %#v", request.Config.Processes)
		}
		if len(request.Config.Processes[0].Env) != 3 || request.Config.Processes[0].Env["ACTIONS_RUNNER_INPUT_JITCONFIG"] != "jit-secret" || request.Config.Processes[0].Env["RUNNERYARD_DEADLINE"] == "" || request.Config.Processes[0].Env["RUNNERYARD_DOCKER_DNS"] != defaultDockerDNS {
			t.Fatalf("worker received unexpected process environment: %#v", request.Config.Processes[0].Env)
		}
		if request.Config.Guest.CPUKind != "shared" || request.Config.Guest.CPUs != 4 || request.Config.Guest.MemoryMB != 8192 {
			t.Fatalf("worker did not preserve the configured shape: %#v", request.Config.Guest)
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

func TestNewNormalizesExplicitDockerDNS(t *testing.T) {
	adapter, err := New(Config{
		APIBaseURL: "https://api.example.test", APIToken: "fly-token", App: "ci-runners", Region: "cdg",
		Image: "registry.fly.io/ci-runners:test", ControllerID: "test-controller",
		CPUKind: "performance", CPUs: 2, MemoryMB: 8192, RootFSGB: 30,
		DockerDNS: " 2606:4700:4700::1111 , 1.0.0.1 ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if adapter.dockerDNS != "2606:4700:4700::1111,1.0.0.1" {
		t.Fatalf("unexpected normalized Docker DNS %q", adapter.dockerDNS)
	}
}

func TestNewRejectsUnsafeDockerDNS(t *testing.T) {
	for _, value := range []string{
		"resolver.internal",
		"1.1.1.1,",
		"1.1.1.1,1.1.1.1",
		"1.1.1.1,8.8.8.8,9.9.9.9,1.0.0.1",
	} {
		t.Run(value, func(t *testing.T) {
			_, err := New(Config{
				APIBaseURL: "https://api.example.test", APIToken: "fly-token", App: "ci-runners", Region: "cdg",
				Image: "registry.fly.io/ci-runners:test", ControllerID: "test-controller",
				CPUKind: "performance", CPUs: 2, MemoryMB: 8192, RootFSGB: 30, DockerDNS: value,
			})
			if err == nil {
				t.Fatalf("expected Docker DNS %q to fail", value)
			}
		})
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
