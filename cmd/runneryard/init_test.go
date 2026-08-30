package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInitCreatesSafeScaffold(t *testing.T) {
	previousVersion, previousCommit := version, commitSHA
	version, commitSHA = "9.8.7", "0123456789abcdef0123456789abcdef01234567"
	t.Cleanup(func() { version, commitSHA = previousVersion, previousCommit })
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
	for _, expected := range []string{"FLY_APP_NAME=acme-ci-controller", "FLY_API_TOKEN=", "runneryard auth github create --controller-app acme-ci-controller"} {
		if !strings.Contains(contents, expected) {
			t.Fatalf("generated env missing %q", expected)
		}
	}
	for _, policy := range []string{"MAX_RUNNERS", "RUNNER_IMAGE", "GITHUB_TOKEN="} {
		if strings.Contains(contents, policy) {
			t.Fatalf("secret template must not carry %s; policy belongs to the TOML only", policy)
		}
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
	for _, expected := range []string{
		`app = "acme-ci-controller"`,
		"[build]\n  image = \"ghcr.io/gwendall/runneryard:9.8.7\"",
		`GITHUB_CONFIG_URL = "https://github.com/acme/widgets"`,
		`SCALE_SET_NAME = "acme-linux"`,
		`RUNNER_FLY_APP = "acme-ci-runners"`,
		`RUNNER_IMAGE = "ghcr.io/gwendall/runneryard:9.8.7"`,
		`MAX_RUNNERS = "3"`,
		`RUNNER_CPU_KIND = "performance"`, `RUNNER_CPUS = "2"`, `RUNNER_ROOTFS_GB = "30"`,
		`RUNNER_DOCKER_DNS = "1.1.1.1,8.8.8.8"`, `RUNNER_USAGE_BUDGET = "166h40m"`,
		`RUNNER_BUDGET_FILE = "/var/lib/runneryard/budget.json"`,
		`RUNNER_STATUS_FILE = "/var/lib/runneryard/status.json"`,
		`cpu_kind = "shared"`, `cpus = 1`, `[[restart]]`, `policy = "always"`,
	} {
		if !strings.Contains(tomlContents, expected) {
			t.Fatalf("generated Fly TOML missing %q", expected)
		}
	}
	if strings.Contains(tomlContents, "FLY_API_TOKEN") || strings.Contains(tomlContents, "GITHUB_TOKEN") {
		t.Fatal("the deployable TOML must never carry a secret name")
	}
	canary, err := os.ReadFile(filepath.Join(directory, ".github", "workflows", "runneryard-canary.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`RUNNERYARD_EXPECTED_VERSION: "9.8.7"`,
		`RUNNERYARD_EXPECTED_COMMIT: "0123456789abcdef0123456789abcdef01234567"`,
		`RUNNERYARD_EXPECTED_NODE: "22.23.2"`,
		`RUNNERYARD_MIN_ROOTFS_GIB: "25"`,
		`runs-on: "acme-linux"`,
		`set -euo pipefail`,
		`actions/setup-node@820762786026740c76f36085b0efc47a31fe5020 # v7.0.0`,
		`actual_version="$(runneryard version)"`,
		`expected_version="runneryard ${RUNNERYARD_EXPECTED_VERSION} (${RUNNERYARD_EXPECTED_COMMIT})"`,
		`test "$RUNNER_ENVIRONMENT" = self-hosted`,
		`df --output=size --block-size=1 /`,
		`test "$rootfs_bytes" -ge "$minimum_rootfs_bytes"`,
		`docker buildx version`,
		`docker compose version`,
		`test "$storage_driver" != vfs`,
		`test "$storage_driver" = fuse-overlayfs`,
		`docker buildx build --load`,
		`docker compose -f /tmp/runneryard-compose-canary.yml run --rm canary`,
		`RUN nslookup registry.npmjs.org >/dev/null`,
	} {
		if !strings.Contains(string(canary), expected) {
			t.Fatalf("generated canary missing %q", expected)
		}
	}
}

func TestRunInitOmitsCommitBindingForUnreleasedBuilds(t *testing.T) {
	previousVersion, previousCommit := version, commitSHA
	version, commitSHA = "dev", "unknown"
	t.Cleanup(func() { version, commitSHA = previousVersion, previousCommit })
	directory := t.TempDir()
	if err := runInit([]string{"--directory", directory, "--github", "https://github.com/acme/widgets"}); err != nil {
		t.Fatal(err)
	}
	canary, err := os.ReadFile(filepath.Join(directory, ".github", "workflows", "runneryard-canary.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canary), `RUNNERYARD_EXPECTED_COMMIT: "`) {
		t.Fatal("a build without a release commit must not pin one in the canary")
	}
	if !strings.Contains(string(canary), `"runneryard ${RUNNERYARD_EXPECTED_VERSION} ("*) ;;`) {
		t.Fatal("the canary must still verify the release version by prefix")
	}
}

func TestRunInitDescribesAnExistingFleet(t *testing.T) {
	previousVersion := version
	version = "9.8.7"
	t.Cleanup(func() { version = previousVersion })
	directory := t.TempDir()
	err := runInit([]string{
		"--directory", directory, "--github", "https://github.com/acme/widgets", "--name", "acme-linux",
		"--controller-app", "acme-control", "--worker-app", "acme-workers", "--controller-id", "acme-fleet",
		"--max-runners", "60", "--rootfs-gb", "50", "--usage-budget", "6000h",
	})
	if err != nil {
		t.Fatal(err)
	}
	toml, err := os.ReadFile(filepath.Join(directory, ".runneryard", "fly.controller.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`app = "acme-control"`, `CONTROLLER_ID = "acme-fleet"`, `RUNNER_FLY_APP = "acme-workers"`,
		`MAX_RUNNERS = "60"`, `RUNNER_ROOTFS_GB = "50"`, `RUNNER_USAGE_BUDGET = "6000h"`,
	} {
		if !strings.Contains(string(toml), expected) {
			t.Fatalf("generated Fly TOML missing %q:\n%s", expected, toml)
		}
	}
	canary, err := os.ReadFile(filepath.Join(directory, ".github", "workflows", "runneryard-canary.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canary), `RUNNERYARD_MIN_ROOTFS_GIB: "43"`) {
		t.Fatalf("canary must derive its root filesystem floor from --rootfs-gb:\n%s", canary)
	}
	env, err := os.ReadFile(filepath.Join(directory, ".runneryard", "controller.env.example"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "FLY_APP_NAME=acme-control") {
		t.Fatalf("secret template must name the controller app:\n%s", env)
	}
}

func TestRunInitRejectsUnsafeFleetDescriptions(t *testing.T) {
	base := []string{"--github", "https://github.com/acme/widgets"}
	for name, extra := range map[string][]string{
		"shared app":          {"--controller-app", "same", "--worker-app", "same"},
		"uppercase app":       {"--controller-app", "Acme-Control"},
		"unsafe controller":   {"--controller-id", "fleet;rm"},
		"zero rootfs":         {"--rootfs-gb", "0"},
		"negative budget":     {"--usage-budget", "-1h"},
		"malformed budget":    {"--usage-budget", "lots"},
		"hetzner fly-only":    {"--provider", "hetzner", "--worker-app", "acme-workers"},
		"hetzner rootfs flag": {"--provider", "hetzner", "--rootfs-gb", "30"},
	} {
		t.Run(name, func(t *testing.T) {
			args := append([]string{"--directory", t.TempDir()}, base...)
			if err := runInit(append(args, extra...)); err == nil {
				t.Fatalf("expected %v to be rejected", extra)
			}
		})
	}
}

func TestMinimumRootfsGiB(t *testing.T) {
	for rootfsGB, expected := range map[int]int{30: 25, 50: 43, 8: 6, 1: 0} {
		if actual := minimumRootfsGiB(rootfsGB); actual != expected {
			t.Fatalf("minimumRootfsGiB(%d) = %d, want %d", rootfsGB, actual, expected)
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
	contents, err := os.ReadFile(filepath.Join(directory, ".runneryard", "fly.controller.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "GITHUB_CONFIG_URL = \"https://github.com/Acme/widgets\"\n") {
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
