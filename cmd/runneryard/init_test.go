package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInitCreatesSafeScaffold(t *testing.T) {
	previousVersion := version
	version = "9.8.7"
	t.Cleanup(func() { version = previousVersion })
	directory := t.TempDir()
	err := runInit([]string{"--directory", directory, "--github", "https://github.com/acme/widgets", "--name", "acme-linux", "--max-runners", "3"})
	if err != nil {
		t.Fatal(err)
	}
	env, err := os.ReadFile(filepath.Join(directory, ".runneryard", "controller.env.example"))
	if err != nil {
		t.Fatal(err)
	}
	contents := string(env)
	for _, expected := range []string{"GITHUB_CONFIG_URL=https://github.com/acme/widgets", "FLY_APP_NAME=acme-ci-controller", "RUNNER_FLY_APP=acme-ci-runners", "MAX_RUNNERS=3", "RUNNER_CPU_KIND=performance", "RUNNER_CPUS=2", "RUNNER_IMAGE=ghcr.io/gwendall/runneryard:9.8.7", "RUNNER_BUDGET_FILE=/var/lib/runneryard/budget.json", "RUNNER_STATUS_FILE=/var/lib/runneryard/status.json"} {
		if !strings.Contains(contents, expected) {
			t.Fatalf("generated env missing %q", expected)
		}
	}
	if strings.Contains(contents, "GITHUB_TOKEN=") {
		t.Fatal("scaffold should prefer GitHub App credentials over a PAT")
	}
	info, err := os.Stat(filepath.Join(directory, ".runneryard", "controller.env.example"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("secret template mode = %o, want 600", info.Mode().Perm())
	}
	toml, err := os.ReadFile(filepath.Join(directory, ".runneryard", "fly.controller.toml"))
	if err != nil {
		t.Fatal(err)
	}
	tomlContents := string(toml)
	for _, expected := range []string{`RUNNER_CPU_KIND = "performance"`, `RUNNER_CPUS = "2"`, `cpu_kind = "shared"`, `cpus = 1`} {
		if !strings.Contains(tomlContents, expected) {
			t.Fatalf("generated Fly TOML missing %q", expected)
		}
	}
	canary, err := os.ReadFile(filepath.Join(directory, ".github", "workflows", "runneryard-canary.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`RUNNERYARD_EXPECTED_VERSION: "9.8.7"`,
		`RUNNERYARD_EXPECTED_NODE: "22.23.2"`,
		`actions/setup-node@820762786026740c76f36085b0efc47a31fe5020`,
		`actual_version="$(runneryard version)"`,
		`docker buildx version`,
		`test "$storage_driver" != vfs`,
		`test "$storage_driver" = fuse-overlayfs`,
		`docker buildx build --load`,
	} {
		if !strings.Contains(string(canary), expected) {
			t.Fatalf("generated canary missing %q", expected)
		}
	}
}

func TestRunInitRefusesOverwrite(t *testing.T) {
	directory := t.TempDir()
	args := []string{"--directory", directory, "--github", "https://github.com/acme/widgets"}
	if err := runInit(args); err != nil {
		t.Fatal(err)
	}
	if err := runInit(args); err == nil {
		t.Fatal("expected overwrite refusal")
	}
}

func TestRunInitRejectsUnsafeGitHubURL(t *testing.T) {
	for _, githubURL := range []string{
		"https://github.com/acme/widgets?token=value",
		"https://github.com/acme/widgets%0aMAX_RUNNERS=100",
		"https://github.com/acme/widgets/extra",
		"https://github.example.com/acme/widgets",
		" https://github.com/acme/widgets",
	} {
		t.Run(githubURL, func(t *testing.T) {
			if err := runInit([]string{"--directory", t.TempDir(), "--github", githubURL}); err == nil {
				t.Fatalf("expected %q to be rejected", githubURL)
			}
		})
	}
}

func TestRunInitNormalizesGitHubURL(t *testing.T) {
	directory := t.TempDir()
	if err := runInit([]string{"--directory", directory, "--github", "https://github.com/Acme/widgets/"}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(directory, ".runneryard", "controller.env.example"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "GITHUB_CONFIG_URL=https://github.com/Acme/widgets\n") {
		t.Fatalf("GitHub URL was not normalized:\n%s", contents)
	}
}

func TestRunInitCreatesHetznerScaffold(t *testing.T) {
	previousVersion := version
	version = "9.8.7"
	t.Cleanup(func() { version = previousVersion })
	directory := t.TempDir()
	if err := runInit([]string{
		"--directory", directory,
		"--github", "https://github.com/acme/widgets",
		"--provider", "hetzner",
	}); err != nil {
		t.Fatal(err)
	}
	env, err := os.ReadFile(filepath.Join(directory, ".runneryard", "controller.env.example"))
	if err != nil {
		t.Fatal(err)
	}
	contents := string(env)
	for _, expected := range []string{
		"COMPUTE_PROVIDER=hetzner",
		"RUNNER_HETZNER_LOCATION=fsn1",
		"RUNNER_HETZNER_SERVER_TYPE=cpx32",
		"RUNNER_HETZNER_IMAGE=docker-ce",
		"RUNNER_HETZNER_FIREWALL_ID=",
		"HCLOUD_TOKEN=",
		"RUNNER_STATUS_FILE=/var/lib/runneryard/status.json",
		"RUNNER_IMAGE=ghcr.io/gwendall/runneryard:9.8.7",
	} {
		if !strings.Contains(contents, expected) {
			t.Fatalf("generated Hetzner env missing %q", expected)
		}
	}
	compose, err := os.ReadFile(filepath.Join(directory, ".runneryard", "hetzner.controller.compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), "ghcr.io/gwendall/runneryard:9.8.7") || !strings.Contains(string(compose), "runneryard_state") || !strings.Contains(string(compose), "github-app.env") || !strings.Contains(string(compose), "./github-app.pem:/run/secrets/github-app.pem:ro") {
		t.Fatalf("unexpected compose scaffold:\n%s", compose)
	}
	if _, err := os.Stat(filepath.Join(directory, ".runneryard", "fly.controller.toml")); !os.IsNotExist(err) {
		t.Fatal("Hetzner scaffold should not contain Fly configuration")
	}
	ignore, err := os.ReadFile(filepath.Join(directory, ".runneryard", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if string(ignore) != "controller.env\ngithub-app.env\ngithub-app.pem\n" {
		t.Fatalf("unexpected secret ignore file %q", ignore)
	}
}
