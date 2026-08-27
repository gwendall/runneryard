// Package controller owns GitHub assignment, policy, and worker reconciliation.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
	GitHubURL    string
	ScaleSetName string
	RunnerGroup  string
	ControllerID string
	MinWorkers   int
	MaxWorkers   int
	MaxLifetime  time.Duration
	UsageBudget  time.Duration
	BudgetWindow time.Duration
	BudgetFile   string
	Version      string
	CommitSHA    string
	Logger       *slog.Logger
}

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
	return &Controller{config: cfg, github: github, compute: compute}, nil
}

func (c *Controller) Run(ctx context.Context) error {
	cfg := c.config
	budget, err := newUsageBudget(cfg.UsageBudget, cfg.BudgetWindow, cfg.BudgetFile, cfg.MaxLifetime)
	if err != nil {
		return err
	}
	runnerGroupID := 1
	if cfg.RunnerGroup != scaleset.DefaultRunnerGroup {
		group, err := c.github.GetRunnerGroupByName(ctx, cfg.RunnerGroup)
		if err != nil {
			return fmt.Errorf("resolve runner group: %w", err)
		}
		runnerGroupID = group.ID
	}

	scaleSet, err := c.github.GetRunnerScaleSet(ctx, runnerGroupID, cfg.ScaleSetName)
	if err != nil {
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
			return fmt.Errorf("create runner scale set: %w", err)
		}
		cfg.Logger.Info("created runner scale set", "name", scaleSet.Name, "id", scaleSet.ID)
	} else {
		scaleSet, err = c.github.UpdateRunnerScaleSet(ctx, scaleSet.ID, desired)
		if err != nil {
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
		return fmt.Errorf("create message session: %w", err)
	}

	scaler := &scaler{
		state:          newWorkerState(),
		compute:        c.compute,
		scaleSetClient: c.github,
		scaleSetID:     scaleSet.ID,
		minWorkers:     cfg.MinWorkers,
		maxWorkers:     cfg.MaxWorkers,
		maxLifetime:    cfg.MaxLifetime,
		budget:         budget,
		logger:         cfg.Logger.WithGroup("scaler"),
	}
	return runControllerSession(ctx, session, scaler, cfg, scaleSet.ID)
}

func runControllerSession(ctx context.Context, session messageSession, scaler *scaler, cfg Config, scaleSetID int) error {
	defer shutdownController(session, scaler, cfg.Logger)
	if err := scaler.recover(ctx); err != nil {
		return fmt.Errorf("recover workers: %w", err)
	}

	queue, err := listener.New(session, listener.Config{
		ScaleSetID: scaleSetID,
		MaxRunners: cfg.MaxWorkers,
		Logger:     cfg.Logger.WithGroup("listener"),
	})
	if err != nil {
		return fmt.Errorf("create scale set listener: %w", err)
	}
	cfg.Logger.Info("controller ready", "github", cfg.GitHubURL, "scale_set", cfg.ScaleSetName, "min_workers", cfg.MinWorkers, "max_workers", cfg.MaxWorkers)
	if err := queue.Run(ctx, scaler); err != nil && !errors.Is(err, context.Canceled) {
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
