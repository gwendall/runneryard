package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/actions/scaleset"
	"github.com/gwendall/runneryard/controller"
	"github.com/gwendall/runneryard/provider"
	flyprovider "github.com/gwendall/runneryard/providers/fly"
	hetznerprovider "github.com/gwendall/runneryard/providers/hetzner"
)

type appConfig struct {
	GitHubURL          string
	ScaleSetName       string
	RunnerGroup        string
	GitHubApp          scaleset.GitHubAppAuth
	GitHubToken        string
	ComputeProvider    string
	ControllerID       string
	ControllerFlyApp   string
	FlyAPIToken        string
	FlyAPIBaseURL      string
	FlyWorkerApp       string
	FlyRegion          string
	HetznerAPIToken    string
	HetznerAPIBaseURL  string
	HetznerLocation    string
	HetznerServerType  string
	HetznerServerImage string
	HetznerFirewallID  int64
	HetznerNetworkID   int64
	RunnerImage        string
	MinWorkers         int
	MaxWorkers         int
	RunnerCPUs         int
	RunnerMemoryMB     int
	RunnerRootFSGB     int
	RunnerDockerDNS    string
	RunnerCPUKind      string
	RunnerMaxLifetime  time.Duration
	RunnerUsageBudget  time.Duration
	RunnerBudgetWindow time.Duration
	RunnerBudgetFile   string
	RunnerStatusFile   string
	LogLevel           slog.Level
}

func loadConfig() (appConfig, error) {
	runnerCPUKind := strings.TrimSpace(os.Getenv("RUNNER_CPU_KIND"))
	if runnerCPUKind == "" {
		// Scaffolds before 0.3.8 set RUNNER_CPUS without setting a CPU kind.
		// Preserve their shared shape instead of silently upgrading their cost.
		if strings.TrimSpace(os.Getenv("RUNNER_CPUS")) != "" {
			runnerCPUKind = "shared"
		} else {
			runnerCPUKind = "performance"
		}
	}
	cfg := appConfig{
		GitHubURL:          strings.TrimSpace(os.Getenv("GITHUB_CONFIG_URL")),
		ScaleSetName:       envOr("SCALE_SET_NAME", "runneryard-linux-x64"),
		RunnerGroup:        envOr("RUNNER_GROUP", scaleset.DefaultRunnerGroup),
		GitHubToken:        strings.TrimSpace(os.Getenv("GITHUB_TOKEN")),
		ComputeProvider:    envOr("COMPUTE_PROVIDER", "fly"),
		ControllerID:       envOr("CONTROLLER_ID", envOr("SCALE_SET_NAME", "runneryard-linux-x64")),
		ControllerFlyApp:   strings.TrimSpace(os.Getenv("FLY_APP_NAME")),
		FlyAPIToken:        strings.TrimSpace(os.Getenv("FLY_API_TOKEN")),
		FlyAPIBaseURL:      envOr("FLY_API_BASE_URL", "https://api.machines.dev"),
		FlyWorkerApp:       strings.TrimSpace(os.Getenv("RUNNER_FLY_APP")),
		FlyRegion:          envOr("RUNNER_FLY_REGION", os.Getenv("FLY_REGION")),
		HetznerAPIToken:    strings.TrimSpace(os.Getenv("HCLOUD_TOKEN")),
		HetznerAPIBaseURL:  envOr("HCLOUD_API_BASE_URL", "https://api.hetzner.cloud/v1"),
		HetznerLocation:    envOr("RUNNER_HETZNER_LOCATION", "fsn1"),
		HetznerServerType:  envOr("RUNNER_HETZNER_SERVER_TYPE", "cpx32"),
		HetznerServerImage: envOr("RUNNER_HETZNER_IMAGE", "docker-ce"),
		RunnerImage:        envOr("RUNNER_IMAGE", os.Getenv("FLY_IMAGE_REF")),
		RunnerDockerDNS:    strings.TrimSpace(os.Getenv("RUNNER_DOCKER_DNS")),
		RunnerCPUKind:      runnerCPUKind,
		RunnerBudgetFile:   strings.TrimSpace(os.Getenv("RUNNER_BUDGET_FILE")),
		RunnerStatusFile:   strings.TrimSpace(os.Getenv("RUNNER_STATUS_FILE")),
		LogLevel:           parseLogLevel(envOr("LOG_LEVEL", "info")),
	}

	var err error
	if cfg.MinWorkers, err = envInt("MIN_RUNNERS", 0); err != nil {
		return appConfig{}, err
	}
	if cfg.MaxWorkers, err = envInt("MAX_RUNNERS", 8); err != nil {
		return appConfig{}, err
	}
	if cfg.RunnerCPUs, err = envInt("RUNNER_CPUS", 2); err != nil {
		return appConfig{}, err
	}
	if cfg.RunnerMemoryMB, err = envInt("RUNNER_MEMORY_MB", 8192); err != nil {
		return appConfig{}, err
	}
	if cfg.RunnerRootFSGB, err = envInt("RUNNER_ROOTFS_GB", 30); err != nil {
		return appConfig{}, err
	}
	if cfg.HetznerFirewallID, err = envInt64("RUNNER_HETZNER_FIREWALL_ID", 0); err != nil {
		return appConfig{}, err
	}
	if cfg.HetznerNetworkID, err = envInt64("RUNNER_HETZNER_NETWORK_ID", 0); err != nil {
		return appConfig{}, err
	}
	if cfg.RunnerMaxLifetime, err = envDuration("RUNNER_MAX_LIFETIME", 2*time.Hour); err != nil {
		return appConfig{}, err
	}
	if cfg.RunnerUsageBudget, err = envDuration("RUNNER_USAGE_BUDGET", 10_000*time.Minute); err != nil {
		return appConfig{}, err
	}
	if cfg.RunnerBudgetWindow, err = envDuration("RUNNER_BUDGET_WINDOW", 30*24*time.Hour); err != nil {
		return appConfig{}, err
	}
	if cfg.RunnerStatusFile == "" && cfg.RunnerBudgetFile != "" {
		cfg.RunnerStatusFile = filepath.Join(filepath.Dir(cfg.RunnerBudgetFile), "status.json")
	}

	installationID, err := envInt64("GITHUB_APP_INSTALLATION_ID", 0)
	if err != nil {
		return appConfig{}, err
	}
	privateKey := os.Getenv("GITHUB_APP_PRIVATE_KEY")
	if keyPath := strings.TrimSpace(os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE")); privateKey == "" && keyPath != "" {
		contents, readErr := readPrivateKeyFile(keyPath)
		if readErr != nil {
			return appConfig{}, fmt.Errorf("read GITHUB_APP_PRIVATE_KEY_FILE: %w", readErr)
		}
		privateKey = contents
	}
	cfg.GitHubApp = scaleset.GitHubAppAuth{
		ClientID:       strings.TrimSpace(os.Getenv("GITHUB_APP_CLIENT_ID")),
		InstallationID: installationID,
		PrivateKey:     privateKey,
	}

	if err := cfg.validate(); err != nil {
		return appConfig{}, err
	}
	return cfg, nil
}

func (c appConfig) validate() error {
	if _, _, err := normalizeGitHubConfigURL(c.GitHubURL); err != nil {
		return fmt.Errorf("GITHUB_CONFIG_URL %w", err)
	}
	if c.ScaleSetName == "" || c.ControllerID == "" {
		return fmt.Errorf("scale set name and controller ID are required")
	}
	if c.GitHubToken == "" {
		if err := c.GitHubApp.Validate(); err != nil {
			return fmt.Errorf("provide GITHUB_TOKEN or complete GitHub App credentials: %w", err)
		}
	}
	if c.GitHubToken != "" && c.GitHubApp.Validate() == nil {
		return fmt.Errorf("provide either GITHUB_TOKEN or GitHub App credentials, not both")
	}
	switch c.ComputeProvider {
	case "fly":
		if c.FlyAPIToken == "" || c.FlyWorkerApp == "" || c.FlyRegion == "" || c.RunnerImage == "" {
			return fmt.Errorf("fly adapter requires FLY_API_TOKEN, RUNNER_FLY_APP, RUNNER_FLY_REGION, and RUNNER_IMAGE")
		}
		if c.ControllerFlyApp != "" && c.ControllerFlyApp == c.FlyWorkerApp {
			return fmt.Errorf("controller and worker Fly apps must be different so app secrets cannot reach jobs")
		}
	case "hetzner":
		if c.HetznerAPIToken == "" || c.HetznerLocation == "" || c.HetznerServerType == "" || c.HetznerServerImage == "" || c.HetznerFirewallID < 1 || c.RunnerImage == "" {
			return fmt.Errorf("hetzner adapter requires HCLOUD_TOKEN, RUNNER_HETZNER_LOCATION, RUNNER_HETZNER_SERVER_TYPE, RUNNER_HETZNER_IMAGE, RUNNER_HETZNER_FIREWALL_ID, and RUNNER_IMAGE")
		}
		if c.HetznerNetworkID < 0 {
			return fmt.Errorf("RUNNER_HETZNER_NETWORK_ID cannot be negative")
		}
	default:
		return fmt.Errorf("unsupported COMPUTE_PROVIDER %q", c.ComputeProvider)
	}
	if c.MinWorkers < 0 || c.MaxWorkers < 1 || c.MinWorkers > c.MaxWorkers {
		return fmt.Errorf("worker bounds must satisfy 0 <= MIN_RUNNERS <= MAX_RUNNERS and MAX_RUNNERS >= 1")
	}
	if c.RunnerMaxLifetime < time.Minute || c.RunnerMaxLifetime > 24*time.Hour {
		return fmt.Errorf("RUNNER_MAX_LIFETIME must be between 1m and 24h")
	}
	if c.RunnerUsageBudget < c.RunnerMaxLifetime {
		return fmt.Errorf("RUNNER_USAGE_BUDGET must cover at least one RUNNER_MAX_LIFETIME")
	}
	if c.RunnerBudgetWindow < time.Hour {
		return fmt.Errorf("RUNNER_BUDGET_WINDOW must be at least 1h")
	}
	if c.RunnerBudgetFile == "" {
		return fmt.Errorf("RUNNER_BUDGET_FILE must point to durable controller storage")
	}
	if c.RunnerStatusFile == "" || c.RunnerStatusFile == c.RunnerBudgetFile {
		return fmt.Errorf("RUNNER_STATUS_FILE must be set and differ from RUNNER_BUDGET_FILE")
	}
	return nil
}

func (c appConfig) githubClient() (*scaleset.Client, error) {
	info := scaleset.SystemInfo{System: "runneryard", Subsystem: "controller", Version: version, CommitSHA: commitSHA}
	if c.GitHubToken != "" {
		return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL:     c.GitHubURL,
			PersonalAccessToken: c.GitHubToken,
			SystemInfo:          info,
		})
	}
	return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: c.GitHubURL,
		GitHubAppAuth:   c.GitHubApp,
		SystemInfo:      info,
	})
}

func (c appConfig) compute() (provider.Compute, error) {
	switch c.ComputeProvider {
	case "fly":
		return flyprovider.New(flyprovider.Config{
			APIToken:     c.FlyAPIToken,
			APIBaseURL:   c.FlyAPIBaseURL,
			App:          c.FlyWorkerApp,
			Region:       c.FlyRegion,
			Image:        c.RunnerImage,
			ControllerID: c.ControllerID,
			CPUKind:      c.RunnerCPUKind,
			CPUs:         c.RunnerCPUs,
			MemoryMB:     c.RunnerMemoryMB,
			RootFSGB:     c.RunnerRootFSGB,
			DockerDNS:    c.RunnerDockerDNS,
		})
	case "hetzner":
		return hetznerprovider.New(hetznerprovider.Config{
			APIToken:     c.HetznerAPIToken,
			APIBaseURL:   c.HetznerAPIBaseURL,
			Location:     c.HetznerLocation,
			ServerType:   c.HetznerServerType,
			ServerImage:  c.HetznerServerImage,
			RunnerImage:  c.RunnerImage,
			ControllerID: c.ControllerID,
			FirewallID:   c.HetznerFirewallID,
			NetworkID:    c.HetznerNetworkID,
		})
	default:
		return nil, fmt.Errorf("unsupported COMPUTE_PROVIDER %q", c.ComputeProvider)
	}
}

func (c appConfig) controllerConfig(logger *slog.Logger) controller.Config {
	return controller.Config{
		GitHubURL:    c.GitHubURL,
		ScaleSetName: c.ScaleSetName,
		RunnerGroup:  c.RunnerGroup,
		ControllerID: c.ControllerID,
		MinWorkers:   c.MinWorkers,
		MaxWorkers:   c.MaxWorkers,
		MaxLifetime:  c.RunnerMaxLifetime,
		UsageBudget:  c.RunnerUsageBudget,
		BudgetWindow: c.RunnerBudgetWindow,
		BudgetFile:   c.RunnerBudgetFile,
		StatusFile:   c.RunnerStatusFile,
		Provider:     c.ComputeProvider,
		Version:      version,
		CommitSHA:    commitSHA,
		Logger:       logger,
	}
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return strings.TrimSpace(fallback)
}

func envInt(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return parsed, nil
}

func envInt64(name string, fallback int64) (int64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return parsed, nil
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", name, err)
	}
	return parsed, nil
}

func parseLogLevel(value string) slog.Level {
	switch strings.ToLower(value) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
