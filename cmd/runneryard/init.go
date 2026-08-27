package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type initOptions struct {
	directory  string
	githubURL  string
	provider   string
	scaleSet   string
	region     string
	maxRunners int
	force      bool
}

var (
	safeName        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	safeGitHubOwner = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	safeGitHubRepo  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	safeRegion      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
)

func runInit(args []string) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(os.Stdout)
	options := initOptions{}
	flags.StringVar(&options.directory, "directory", ".", "repository directory")
	flags.StringVar(&options.githubURL, "github", "", "GitHub repository or organization URL")
	flags.StringVar(&options.provider, "provider", "fly", "compute provider")
	flags.StringVar(&options.scaleSet, "name", "runneryard-linux-x64", "scale set and runs-on label")
	flags.StringVar(&options.region, "region", "", "provider region")
	flags.IntVar(&options.maxRunners, "max-runners", 4, "hard concurrency ceiling")
	flags.BoolVar(&options.force, "force", false, "overwrite generated files")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if options.githubURL == "" {
		options.githubURL = inferGitHubURL(options.directory)
	}
	if options.githubURL == "" {
		return errors.New("--github is required outside a GitHub checkout")
	}
	normalizedGitHubURL, owner, err := normalizeGitHubConfigURL(options.githubURL)
	if err != nil {
		return fmt.Errorf("--github: %w", err)
	}
	options.githubURL = normalizedGitHubURL
	switch options.provider {
	case "fly":
		if options.region == "" {
			options.region = "cdg"
		}
	case "hetzner":
		if options.region == "" {
			options.region = "fsn1"
		}
	default:
		return fmt.Errorf("provider %q is not bundled; see docs/adapter-contract.md", options.provider)
	}
	if !safeName.MatchString(options.scaleSet) {
		return errors.New("--name must contain only letters, numbers, dots, underscores, or hyphens")
	}
	if !safeRegion.MatchString(options.region) {
		return errors.New("--region must contain only lowercase letters, numbers, or hyphens")
	}
	if options.maxRunners < 1 || options.maxRunners > 100 {
		return errors.New("--max-runners must be between 1 and 100")
	}

	projectDir, err := filepath.Abs(options.directory)
	if err != nil {
		return err
	}
	controllerApp := strings.ToLower(owner) + "-ci-controller"
	workerApp := strings.ToLower(owner) + "-ci-runners"
	files := []generatedFile{{
		path: filepath.Join(projectDir, ".github", "workflows", "runneryard-canary.yml"), contents: renderCanary(options.scaleSet), mode: 0o644,
	}}
	if options.provider == "fly" {
		files = append(files,
			generatedFile{path: filepath.Join(projectDir, ".runneryard", "controller.env.example"), contents: renderFlyEnv(options, controllerApp, workerApp), mode: 0o600},
			generatedFile{path: filepath.Join(projectDir, ".runneryard", "fly.controller.toml"), contents: renderFly(options, controllerApp, workerApp), mode: 0o644},
		)
	} else {
		files = append(files,
			generatedFile{path: filepath.Join(projectDir, ".runneryard", "controller.env.example"), contents: renderHetznerEnv(options), mode: 0o600},
			generatedFile{path: filepath.Join(projectDir, ".runneryard", "hetzner.controller.compose.yml"), contents: renderHetznerCompose(), mode: 0o644},
			generatedFile{path: filepath.Join(projectDir, ".runneryard", ".gitignore"), contents: "controller.env\ngithub-app.pem\n", mode: 0o644},
		)
	}
	for _, file := range files {
		if _, err := os.Stat(file.path); err == nil && !options.force {
			return fmt.Errorf("refusing to overwrite %s; pass --force after reviewing it", file.path)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, file := range files {
		if err := writeGenerated(file.path, file.contents, file.mode); err != nil {
			return err
		}
	}
	fmt.Printf("Created RunnerYard configuration for %s\n\n", options.githubURL)
	if options.provider == "fly" {
		fmt.Printf("Next:\n  1. Review .runneryard/controller.env.example\n  2. Follow docs/providers/fly.md to create the isolated apps and durable volume\n  3. Run: runneryard doctor --provider fly --controller-app %s --worker-app %s\n  4. Deploy the controller, then trigger .github/workflows/runneryard-canary.yml\n", controllerApp, workerApp)
	} else {
		fmt.Print("Next:\n  1. Create a dedicated Hetzner project and a firewall with no inbound rules\n  2. Fill .runneryard/controller.env from the generated example\n  3. Run: runneryard doctor --provider hetzner --firewall-id <id>\n  4. Follow docs/providers/hetzner.md, then trigger .github/workflows/runneryard-canary.yml\n")
	}
	return nil
}

func inferGitHubURL(directory string) string {
	output, err := exec.Command("git", "-C", directory, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	remote := strings.TrimSuffix(strings.TrimSpace(string(output)), ".git")
	if strings.HasPrefix(remote, "git@github.com:") {
		return "https://github.com/" + strings.TrimPrefix(remote, "git@github.com:")
	}
	if strings.HasPrefix(remote, "https://github.com/") {
		return remote
	}
	return ""
}

func normalizeGitHubConfigURL(raw string) (string, string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", "", errors.New("must be an https://github.com repository or organization URL without surrounding whitespace")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") || parsed.Port() != "" {
		return "", "", errors.New("must be an https://github.com repository or organization URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return "", "", errors.New("must not contain credentials, query parameters, fragments, or encoded path characters")
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 1 || len(parts) > 2 || !safeGitHubOwner.MatchString(parts[0]) {
		return "", "", errors.New("must contain one GitHub owner and at most one repository name")
	}
	if len(parts) == 2 && !safeGitHubRepo.MatchString(parts[1]) {
		return "", "", errors.New("contains an invalid GitHub repository name")
	}
	return "https://github.com/" + strings.Join(parts, "/"), parts[0], nil
}

type generatedFile struct {
	path     string
	contents string
	mode     os.FileMode
}

func writeGenerated(path, contents string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(contents), mode)
}

func renderFlyEnv(options initOptions, controllerApp, workerApp string) string {
	return fmt.Sprintf(`# Copy to a secret store. Never commit the completed file.
GITHUB_CONFIG_URL=%s
SCALE_SET_NAME=%s
COMPUTE_PROVIDER=fly
CONTROLLER_ID=%s
FLY_APP_NAME=%s
RUNNER_FLY_APP=%s
RUNNER_FLY_REGION=%s
RUNNER_IMAGE=ghcr.io/gwendall/runneryard:%s
MIN_RUNNERS=0
MAX_RUNNERS=%d
RUNNER_CPUS=4
RUNNER_MEMORY_MB=8192
RUNNER_ROOTFS_GB=30
RUNNER_MAX_LIFETIME=2h
RUNNER_USAGE_BUDGET=166h40m
RUNNER_BUDGET_WINDOW=720h
RUNNER_BUDGET_FILE=/var/lib/runneryard/budget.json

# Set only on the controller:
FLY_API_TOKEN=
GITHUB_APP_CLIENT_ID=
GITHUB_APP_INSTALLATION_ID=
GITHUB_APP_PRIVATE_KEY=
`, options.githubURL, options.scaleSet, options.scaleSet, controllerApp, workerApp, options.region, version, options.maxRunners)
}

func renderHetznerEnv(options initOptions) string {
	return fmt.Sprintf(`# Copy to .runneryard/controller.env on the controller host. Never commit it.
GITHUB_CONFIG_URL=%s
SCALE_SET_NAME=%s
COMPUTE_PROVIDER=hetzner
CONTROLLER_ID=%s
RUNNER_HETZNER_LOCATION=%s
RUNNER_HETZNER_SERVER_TYPE=cpx32
RUNNER_HETZNER_IMAGE=docker-ce
RUNNER_HETZNER_FIREWALL_ID=
# Optional private network ID. Workers still need public egress for GitHub.
RUNNER_HETZNER_NETWORK_ID=
RUNNER_IMAGE=ghcr.io/gwendall/runneryard:%s
MIN_RUNNERS=0
MAX_RUNNERS=%d
RUNNER_MAX_LIFETIME=2h
RUNNER_USAGE_BUDGET=166h40m
RUNNER_BUDGET_WINDOW=720h
RUNNER_BUDGET_FILE=/var/lib/runneryard/budget.json

# Set only on the controller:
HCLOUD_TOKEN=
GITHUB_APP_CLIENT_ID=
GITHUB_APP_INSTALLATION_ID=
GITHUB_APP_PRIVATE_KEY_FILE=/run/secrets/github-app.pem
`, options.githubURL, options.scaleSet, options.scaleSet, options.region, version, options.maxRunners)
}

func renderHetznerCompose() string {
	return fmt.Sprintf(`services:
  controller:
    image: ghcr.io/gwendall/runneryard:%s
    restart: unless-stopped
    env_file:
      - controller.env
    volumes:
      - runneryard_state:/var/lib/runneryard
      - ./github-app.pem:/run/secrets/github-app.pem:ro

volumes:
  runneryard_state:
`, version)
}

func renderFly(options initOptions, controllerApp, workerApp string) string {
	return fmt.Sprintf(`app = %q
primary_region = %q

[env]
  GITHUB_CONFIG_URL = %q
  SCALE_SET_NAME = %q
  COMPUTE_PROVIDER = "fly"
  CONTROLLER_ID = %q
  RUNNER_FLY_APP = %q
  RUNNER_FLY_REGION = %q
  MIN_RUNNERS = "0"
  MAX_RUNNERS = %q
  RUNNER_CPUS = "4"
  RUNNER_MEMORY_MB = "8192"
  RUNNER_ROOTFS_GB = "30"
  RUNNER_MAX_LIFETIME = "2h"
  RUNNER_USAGE_BUDGET = "166h40m"
  RUNNER_BUDGET_WINDOW = "720h"
  RUNNER_BUDGET_FILE = "/var/lib/runneryard/budget.json"

[[mounts]]
  source = "runneryard_state"
  destination = "/var/lib/runneryard"

[[vm]]
  cpu_kind = "shared"
  cpus = 1
  memory = "512mb"
`, controllerApp, options.region, options.githubURL, options.scaleSet, options.scaleSet, workerApp, options.region, fmt.Sprint(options.maxRunners))
}

func renderCanary(scaleSet string) string {
	return fmt.Sprintf(`name: RunnerYard canary

on:
  workflow_dispatch:

permissions:
  contents: read

jobs:
  canary:
    runs-on: %q
    timeout-minutes: 10
    steps:
      - name: Verify isolated worker
        run: |
          test -n "$RUNNER_NAME"
          docker version
`, scaleSet)
}
