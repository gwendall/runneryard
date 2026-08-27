package main

import "testing"

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
	t.Setenv("RUNNER_BUDGET_FILE", t.TempDir()+"/budget.json")
}
