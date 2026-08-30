// Package controller owns GitHub assignment, policy, and worker reconciliation.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/gwendall/runneryard/provider"
)

const sessionCloseTimeout = 3 * time.Second

type sessionCloser interface {
	Close(context.Context) error
}

type messageSession interface {
	listener.Client
	sessionCloser
}

type Config struct {
	GitHubURL      string
	ScaleSetName   string
	RunnerGroup    string
	ControllerID   string
	MinWorkers     int
	MaxWorkers     int
	MaxLifetime    time.Duration
	UsageBudget    time.Duration
	BudgetWindow   time.Duration
	BudgetFile     string
	RetirementFile string
	StatusFile     string
	Provider       string
	Version        string
	CommitSHA      string
	// GitHubAPIRate paces the controller's own GitHub API calls in requests
	// per second; zero disables pacing. GitHubAPIBurst is the bucket depth.
	GitHubAPIRate  float64
	GitHubAPIBurst int
	// LaunchConcurrency bounds how many worker launches run at once; zero
	// selects the default of 8.
	LaunchConcurrency int
	// IdleTimeout is passed to workers so they release themselves when no job
	// arrives; DanglingTimeout is the controller-side backstop after which an
	// idle worker it created is retired. Zero disables either one.
	IdleTimeout     time.Duration
	DanglingTimeout time.Duration
	// AlertWebhookURL receives a JSON {"text": ...} POST on every health
	// transition and one reminder per hour while degraded. Empty disables it.
	AlertWebhookURL string
	Logger          *slog.Logger
}

const defaultLaunchConcurrency = 8

type Controller struct {
	config  Config
	github  *scaleset.Client
	compute provider.Compute
}

func New(cfg Config, github *scaleset.Client, compute provider.Compute) (*Controller, error) {
	if github == nil || compute == nil {
		return nil, fmt.Errorf("GitHub and compute adapters are required")
	}
	if cfg.ScaleSetName == "" || cfg.ControllerID == "" || cfg.MaxWorkers < 1 || cfg.MinWorkers < 0 || cfg.MinWorkers > cfg.MaxWorkers {
		return nil, fmt.Errorf("invalid controller identity or worker bounds")
	}
	if cfg.MaxLifetime < time.Minute || cfg.MaxLifetime > 24*time.Hour {
		return nil, fmt.Errorf("maximum worker lifetime must be between 1m and 24h")
	}
	if cfg.UsageBudget < cfg.MaxLifetime || cfg.BudgetWindow < time.Hour || cfg.BudgetFile == "" {
		return nil, fmt.Errorf("usage budget must cover at least one worker lifetime and use a durable state file")
	}
	if cfg.RunnerGroup == "" {
		cfg.RunnerGroup = scaleset.DefaultRunnerGroup
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	if cfg.StatusFile == "" {
		cfg.StatusFile = filepath.Join(filepath.Dir(cfg.BudgetFile), "status.json")
	}
	if cfg.RetirementFile == "" {
		cfg.RetirementFile = filepath.Join(filepath.Dir(cfg.BudgetFile), "retirements.json")
	}
	if cfg.StatusFile == cfg.BudgetFile {
		return nil, fmt.Errorf("fleet status and runner usage budget must use different files")
	}
	if cfg.RetirementFile == cfg.BudgetFile || cfg.RetirementFile == cfg.StatusFile {
		return nil, fmt.Errorf("runner retirement queue must use a dedicated file")
	}
	if cfg.Provider == "" {
		cfg.Provider = "unknown"
	}
	if cfg.LaunchConcurrency < 1 {
		cfg.LaunchConcurrency = defaultLaunchConcurrency
	}
	if cfg.IdleTimeout < 0 || cfg.IdleTimeout > cfg.MaxLifetime || cfg.DanglingTimeout < 0 || cfg.DanglingTimeout > cfg.MaxLifetime {
		return nil, fmt.Errorf("idle and dangling timeouts must be between 0 and the maximum worker lifetime")
	}
	return &Controller{config: cfg, github: github, compute: compute}, nil
}

func (c *Controller) Run(ctx context.Context) error {
	cfg := c.config
	budget, err := newUsageBudget(cfg.UsageBudget, cfg.BudgetWindow, cfg.BudgetFile, cfg.MaxLifetime)
	if err != nil {
		return err
	}
	reporter, err := newStatusReporter(cfg, budget.snapshot(time.Now()))
	if err != nil {
		return err
	}
	reporter.start(ctx, budget)
	defer reporter.close()
	retirements, err := newRetirementQueue(cfg.RetirementFile)
	if err != nil {
		reporter.degraded("runner_retirement_state_failed")
		return err
	}
	reporter.retirements(retirements.count(), retirements.overdue(time.Now(), retirementGrace))
	runnerGroupID := 1
	if cfg.RunnerGroup != scaleset.DefaultRunnerGroup {
		group, err := c.github.GetRunnerGroupByName(ctx, cfg.RunnerGroup)
		if err != nil {
			reporter.degraded("github_runner_group_failed")
			return fmt.Errorf("resolve runner group: %w", err)
		}
		runnerGroupID = group.ID
	}

	scaleSet, err := c.github.GetRunnerScaleSet(ctx, runnerGroupID, cfg.ScaleSetName)
	if err != nil {
		reporter.degraded("github_scale_set_failed")
		return fmt.Errorf("find runner scale set: %w", err)
	}
	desired := &scaleset.RunnerScaleSet{
		Name:          cfg.ScaleSetName,
		RunnerGroupID: runnerGroupID,
		Labels:        []scaleset.Label{{Name: cfg.ScaleSetName}},
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	}
	if scaleSet == nil {
		scaleSet, err = c.github.CreateRunnerScaleSet(ctx, desired)
		if err != nil {
			reporter.degraded("github_scale_set_failed")
			return fmt.Errorf("create runner scale set: %w", err)
		}
		cfg.Logger.Info("created runner scale set", "name", scaleSet.Name, "id", scaleSet.ID)
	} else {
		scaleSet, err = c.github.UpdateRunnerScaleSet(ctx, scaleSet.ID, desired)
		if err != nil {
			reporter.degraded("github_scale_set_failed")
			return fmt.Errorf("update runner scale set: %w", err)
		}
		cfg.Logger.Info("using existing runner scale set", "name", scaleSet.Name, "id", scaleSet.ID)
	}
	c.github.SetSystemInfo(c.systemInfo(scaleSet.ID))

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = cfg.ControllerID
	}
	session, err := c.github.MessageSessionClient(ctx, scaleSet.ID, hostname)
	if err != nil {
		reporter.degraded("github_session_failed")
		return fmt.Errorf("create message session: %w", err)
	}
	reporter.githubActivity("session_created")

	scaler := &scaler{
		state:             newWorkerState(),
		compute:           c.compute,
		scaleSetClient:    newPacedScaleSetClient(c.github, cfg.GitHubAPIRate, cfg.GitHubAPIBurst),
		scaleSetID:        scaleSet.ID,
		minWorkers:        cfg.MinWorkers,
		maxWorkers:        cfg.MaxWorkers,
		launchConcurrency: cfg.LaunchConcurrency,
		maxLifetime:       cfg.MaxLifetime,
		idleTimeout:       cfg.IdleTimeout,
		danglingTimeout:   cfg.DanglingTimeout,
		budget:            budget,
		retirements:       retirements,
		reporter:          reporter,
		logger:            cfg.Logger.WithGroup("scaler"),
	}
	return runControllerSession(ctx, session, scaler, cfg, scaleSet.ID)
}

func runControllerSession(ctx context.Context, session messageSession, scaler *scaler, cfg Config, scaleSetID int) error {
	defer shutdownController(session, scaler, cfg.Logger)
	if err := scaler.recover(ctx); err != nil {
		return fmt.Errorf("recover workers: %w", err)
	}
	scaler.reporter.recovered()

	queue, err := listener.New(session, listener.Config{
		ScaleSetID: scaleSetID,
		MaxRunners: cfg.MaxWorkers,
		Logger:     cfg.Logger.WithGroup("listener"),
	})
	if err != nil {
		scaler.reporter.degraded("github_listener_failed")
		return fmt.Errorf("create scale set listener: %w", err)
	}
	cfg.Logger.Info("controller ready", "github", cfg.GitHubURL, "scale_set", cfg.ScaleSetName, "min_workers", cfg.MinWorkers, "max_workers", cfg.MaxWorkers)
	if err := queue.Run(ctx, scaler); err != nil && !errors.Is(err, context.Canceled) {
		scaler.reporter.degraded("github_listener_failed")
		return fmt.Errorf("run scale set listener: %w", err)
	}
	return nil
}

func shutdownController(session sessionCloser, scaler *scaler, logger *slog.Logger) {
	// Release GitHub's single active scale-set session before handing existing
	// workers to the successor. A stale session would prevent the next
	// controller process from starting after a deployment.
	closeCtx, cancelClose := context.WithTimeout(context.Background(), sessionCloseTimeout)
	if err := session.Close(closeCtx); err != nil {
		logger.Error("failed to close message session", "error", err)
	}
	cancelClose()
	scaler.shutdown(context.Background())
}

func (c *Controller) systemInfo(scaleSetID int) scaleset.SystemInfo {
	return scaleset.SystemInfo{
		System:     "runneryard",
		Subsystem:  "controller",
		Version:    c.config.Version,
		CommitSHA:  c.config.CommitSHA,
		ScaleSetID: scaleSetID,
	}
}
