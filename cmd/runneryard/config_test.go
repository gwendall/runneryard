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

func setValidEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GITHUB_CONFIG_URL", "https://github.com/acme/repo")
	t.Setenv("GITHUB_TOKEN", "token")
	t.Setenv("FLY_API_TOKEN", "fly-token")
	t.Setenv("RUNNER_FLY_APP", "ci-runners")
	t.Setenv("RUNNER_FLY_REGION", "cdg")
	t.Setenv("RUNNER_IMAGE", "registry.fly.io/ci-runners:test")
	t.Setenv("RUNNER_BUDGET_FILE", t.TempDir()+"/budget.json")
}
