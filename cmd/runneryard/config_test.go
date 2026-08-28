package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gwendall/runneryard/provider"
)

func TestConfigRejectsSharedControllerAndWorkerFlyApp(t *testing.T) {
	setValidEnv(t)
	t.Setenv("FLY_APP_NAME", "ci-runners")
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected shared controller and worker app to fail")
	}
}

func TestConfigUsesBoundedDefaults(t *testing.T) {
	setValidEnv(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinWorkers != 0 || cfg.MaxWorkers != 8 {
		t.Fatalf("unexpected worker defaults: min=%d max=%d", cfg.MinWorkers, cfg.MaxWorkers)
	}
	if cfg.RunnerCPUKind != "performance" || cfg.RunnerCPUs != 2 || cfg.RunnerMemoryMB != 8192 {
		t.Fatalf("unexpected Fly worker defaults: kind=%s cpus=%d memory=%d", cfg.RunnerCPUKind, cfg.RunnerCPUs, cfg.RunnerMemoryMB)
	}
}

func TestConfigPreservesExplicitSharedFlyShape(t *testing.T) {
	setValidEnv(t)
	t.Setenv("RUNNER_CPU_KIND", "shared")
	t.Setenv("RUNNER_CPUS", "4")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RunnerCPUKind != "shared" || cfg.RunnerCPUs != 4 {
		t.Fatalf("unexpected explicit Fly worker shape: kind=%s cpus=%d", cfg.RunnerCPUKind, cfg.RunnerCPUs)
	}
	if _, err := cfg.compute(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigPreservesLegacySharedFlyShape(t *testing.T) {
	setValidEnv(t)
	t.Setenv("RUNNER_CPUS", "4")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RunnerCPUKind != "shared" || cfg.RunnerCPUs != 4 {
		t.Fatalf("unexpected legacy Fly worker shape: kind=%s cpus=%d", cfg.RunnerCPUKind, cfg.RunnerCPUs)
	}
}

func TestConfigWiresDefaultFlyShapeToLaunch(t *testing.T) {
	var launched struct {
		CPUKind string
		CPUs    int
		Memory  int
		DNS     string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Config struct {
				Guest struct {
					CPUKind string `json:"cpu_kind"`
					CPUs    int    `json:"cpus"`
					Memory  int    `json:"memory_mb"`
				} `json:"guest"`
				Processes []struct {
					Env map[string]string `json:"env"`
				} `json:"processes"`
			} `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		launched.CPUKind = request.Config.Guest.CPUKind
		launched.CPUs = request.Config.Guest.CPUs
		launched.Memory = request.Config.Guest.Memory
		launched.DNS = request.Config.Processes[0].Env["RUNNERYARD_DOCKER_DNS"]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "machine-id", "name": "runner-one"})
	}))
	defer server.Close()

	setValidEnv(t)
	t.Setenv("FLY_API_BASE_URL", server.URL)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	compute, err := cfg.compute()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compute.Launch(context.Background(), provider.Lease{
		ID: "lease-one", RunnerName: "runner-one", RunnerID: 42, RunnerScaleSetID: 7,
		JITConfig: "jit", Deadline: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if launched.CPUKind != "performance" || launched.CPUs != 2 || launched.Memory != 8192 || launched.DNS != "1.1.1.1,8.8.8.8" {
		t.Fatalf("unexpected launched Fly worker shape: %#v", launched)
	}
}

func TestConfigRejectsNonGitHubConfigURL(t *testing.T) {
	setValidEnv(t)
	t.Setenv("GITHUB_CONFIG_URL", "https://example.com/acme/repo")
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected non-GitHub URL to fail")
	}
}

func TestConfigLoadsHetznerAdapter(t *testing.T) {
	setValidEnv(t)
	t.Setenv("COMPUTE_PROVIDER", "hetzner")
	t.Setenv("FLY_API_TOKEN", "")
	t.Setenv("RUNNER_FLY_APP", "")
	t.Setenv("RUNNER_FLY_REGION", "")
	t.Setenv("HCLOUD_TOKEN", "hcloud-token")
	t.Setenv("RUNNER_HETZNER_FIREWALL_ID", "42")
	t.Setenv("RUNNER_HETZNER_NETWORK_ID", "84")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HetznerLocation != "fsn1" || cfg.HetznerServerType != "cpx32" || cfg.HetznerServerImage != "docker-ce" {
		t.Fatalf("unexpected Hetzner defaults: %#v", cfg)
	}
	if cfg.HetznerFirewallID != 42 || cfg.HetznerNetworkID != 84 {
		t.Fatalf("unexpected Hetzner resource IDs: %#v", cfg)
	}
	if _, err := cfg.compute(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigRequiresHetznerFirewall(t *testing.T) {
	setValidEnv(t)
	t.Setenv("COMPUTE_PROVIDER", "hetzner")
	t.Setenv("HCLOUD_TOKEN", "hcloud-token")
	t.Setenv("RUNNER_HETZNER_FIREWALL_ID", "")
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected missing worker firewall to fail")
	}
}

func setValidEnv(t *testing.T) {
	t.Helper()
	t.Setenv("COMPUTE_PROVIDER", "fly")
	t.Setenv("GITHUB_CONFIG_URL", "https://github.com/acme/repo")
	t.Setenv("GITHUB_TOKEN", "token")
	t.Setenv("FLY_API_TOKEN", "fly-token")
	t.Setenv("RUNNER_FLY_APP", "ci-runners")
	t.Setenv("RUNNER_FLY_REGION", "cdg")
	t.Setenv("RUNNER_IMAGE", "registry.fly.io/ci-runners:test")
	t.Setenv("RUNNER_CPU_KIND", "")
	t.Setenv("RUNNER_CPUS", "")
	t.Setenv("RUNNER_BUDGET_FILE", t.TempDir()+"/budget.json")
}
