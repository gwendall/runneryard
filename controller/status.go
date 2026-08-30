package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const (
	statusSchemaVersion = 2
	maximumStatusSize   = 1 << 20
)

type FleetStatus struct {
	SchemaVersion int              `json:"schema_version"`
	UpdatedAt     time.Time        `json:"updated_at"`
	StartedAt     time.Time        `json:"started_at"`
	Health        string           `json:"health"`
	Reason        string           `json:"reason,omitempty"`
	Controller    ControllerStatus `json:"controller"`
	GitHub        GitHubStatus     `json:"github"`
	Workers       WorkerStatus     `json:"workers"`
	Capacity      CapacityStatus   `json:"capacity"`
	Latency       LatencyStatus    `json:"latency"`
	Budget        BudgetStatus     `json:"budget"`
}

type ControllerStatus struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Version   string `json:"version"`
	CommitSHA string `json:"commit_sha"`
}

type GitHubStatus struct {
	ConfigURL      string    `json:"config_url"`
	ScaleSet       string    `json:"scale_set"`
	AssignedJobs   int       `json:"assigned_jobs"`
	DesiredWorkers int       `json:"desired_workers"`
	LastActivityAt time.Time `json:"last_activity_at,omitempty"`
	LastEvent      string    `json:"last_event,omitempty"`
}

type WorkerStatus struct {
	Actual             int `json:"actual"`
	Starting           int `json:"starting"`
	Busy               int `json:"busy"`
	Idle               int `json:"idle"`
	Unknown            int `json:"unknown"`
	OrphanCandidates   int `json:"orphan_candidates"`
	PendingRetirements int `json:"pending_retirements"`
	// OverdueRetirements counts pending retirements older than the
	// retirement grace. GitHub can keep a runner registered for minutes after
	// its job completed, so a fresh deferral is routine and only an overdue
	// one degrades the fleet.
	OverdueRetirements int  `json:"overdue_retirements"`
	Maximum            int  `json:"maximum"`
	Saturated          bool `json:"saturated"`
}

// CapacityStatus distinguishes the operator-configured ceiling from the
// capacity a provider has most recently proven available to this fleet.
// Rejection is a stable adapter-supplied code, never a raw provider payload.
type CapacityStatus struct {
	Configured int       `json:"configured"`
	Effective  int       `json:"effective"`
	Rejections uint64    `json:"rejections"`
	Rejection  string    `json:"provider_rejection,omitempty"`
	RetryAt    time.Time `json:"retry_at,omitempty"`
}

type LatencyStatus struct {
	ProviderCreate LatencyStats `json:"provider_create"`
	Assignment     LatencyStats `json:"assignment"`
}

type LatencyStats struct {
	Samples   uint64  `json:"samples"`
	Failures  uint64  `json:"failures"`
	LastMS    int64   `json:"last_ms"`
	AverageMS float64 `json:"average_ms"`
	MaximumMS int64   `json:"maximum_ms"`
}

type BudgetStatus struct {
	LimitSeconds     int64     `json:"limit_seconds"`
	UsedSeconds      int64     `json:"used_seconds"`
	ReservedSeconds  int64     `json:"reserved_seconds"`
	RemainingSeconds int64     `json:"remaining_seconds"`
	WindowSeconds    int64     `json:"window_seconds"`
	RefusalReason    string    `json:"refusal_reason,omitempty"`
	NextAvailableAt  time.Time `json:"next_available_at,omitempty"`
	// BurnSecondsPerDay is the settled usage of the trailing day extrapolated
	// to a daily rate; HorizonSeconds is how long the remaining budget lasts at
	// that rate. Both are zero when there is no recent usage.
	BurnSecondsPerDay int64 `json:"burn_seconds_per_day"`
	HorizonSeconds    int64 `json:"horizon_seconds,omitempty"`
}

type statusReporter struct {
	mu       sync.Mutex
	status   FleetStatus
	file     string
	now      func() time.Time
	logger   *slog.Logger
	failure  string
	dirty    bool
	revision uint64
	started  bool
	cancel   context.CancelFunc
	done     chan struct{}
	alerter  *alerter
}

func newStatusReporter(cfg Config, budget BudgetStatus) (*statusReporter, error) {
	now := time.Now().UTC()
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	reporter := &statusReporter{
		file: cfg.StatusFile, now: time.Now, logger: logger.WithGroup("status"),
		status: FleetStatus{
			SchemaVersion: statusSchemaVersion,
			UpdatedAt:     now,
			StartedAt:     now,
			Health:        "starting",
			Controller: ControllerStatus{
				ID: cfg.ControllerID, Provider: cfg.Provider, Version: cfg.Version, CommitSHA: cfg.CommitSHA,
			},
			GitHub:   GitHubStatus{ConfigURL: cfg.GitHubURL, ScaleSet: cfg.ScaleSetName},
			Workers:  WorkerStatus{Maximum: cfg.MaxWorkers},
			Capacity: CapacityStatus{Configured: cfg.MaxWorkers, Effective: cfg.MaxWorkers},
			Budget:   budget,
		},
	}
	alerts, err := newAlerter(cfg)
	if err != nil {
		return nil, err
	}
	reporter.alerter = alerts
	if err := writeStatusFile(reporter.file, reporter.status); err != nil {
		return nil, fmt.Errorf("initialize fleet status: %w", err)
	}
	return reporter, nil
}

func (reporter *statusReporter) update(change func(*FleetStatus)) {
	if reporter == nil {
		return
	}
	reporter.mu.Lock()
	change(&reporter.status)
	reporter.status.UpdatedAt = reporter.now().UTC()
	reporter.derive()
	reporter.dirty = true
	if reporter.revision < math.MaxUint64 {
		reporter.revision++
	}
	synchronous := !reporter.started
	snapshot := reporter.status
	reporter.mu.Unlock()
	reporter.alerter.observe(snapshot)
	if synchronous {
		reporter.flush()
	}
}

func (reporter *statusReporter) derive() {
	status := &reporter.status
	effective := status.Capacity.Effective
	if status.Capacity.Configured == 0 {
		effective = status.Workers.Maximum
	}
	status.Workers.Saturated = status.Workers.Actual+status.Workers.Starting >= effective
	switch {
	case reporter.failure != "":
		status.Health, status.Reason = "degraded", reporter.failure
	case status.Capacity.Rejection != "":
		status.Health, status.Reason = "degraded", "provider_capacity_exhausted"
	case status.Workers.OverdueRetirements > 0:
		status.Health, status.Reason = "degraded", "runner_retirements_pending"
	case status.Workers.OrphanCandidates > 0:
		status.Health, status.Reason = "degraded", "orphan_candidates"
	case status.Budget.RefusalReason != "":
		status.Health, status.Reason = "degraded", status.Budget.RefusalReason
	case status.GitHub.LastActivityAt.IsZero():
		status.Health, status.Reason = "starting", ""
	default:
		status.Health, status.Reason = "ready", ""
	}
}

func (reporter *statusReporter) githubActivity(event string) {
	reporter.update(func(status *FleetStatus) {
		status.GitHub.LastActivityAt = reporter.now().UTC()
		status.GitHub.LastEvent = event
	})
}

func (reporter *statusReporter) desired(assigned, desired int) {
	reporter.update(func(status *FleetStatus) {
		status.GitHub.AssignedJobs = max(0, assigned)
		status.GitHub.DesiredWorkers = max(0, desired)
		status.GitHub.LastActivityAt = reporter.now().UTC()
		status.GitHub.LastEvent = "desired_count"
	})
}

func (reporter *statusReporter) workers(actual, busy, idle, unknown int) {
	reporter.update(func(status *FleetStatus) {
		status.Workers.Actual = max(0, actual)
		status.Workers.Busy = max(0, busy)
		status.Workers.Idle = max(0, idle)
		status.Workers.Unknown = max(0, unknown)
	})
}

func (reporter *statusReporter) starting(delta int) {
	reporter.update(func(status *FleetStatus) {
		status.Workers.Starting = max(0, status.Workers.Starting+delta)
	})
}

func (reporter *statusReporter) orphans(count int) {
	reporter.update(func(status *FleetStatus) { status.Workers.OrphanCandidates = max(0, count) })
}

func (reporter *statusReporter) retirements(count, overdue int) {
	reporter.update(func(status *FleetStatus) {
		status.Workers.PendingRetirements = max(0, count)
		status.Workers.OverdueRetirements = min(max(0, overdue), max(0, count))
	})
}

func (reporter *statusReporter) budget(snapshot BudgetStatus) {
	reporter.update(func(status *FleetStatus) { status.Budget = snapshot })
}

func (reporter *statusReporter) latency(providerCreate bool, elapsed time.Duration, failed bool) {
	reporter.update(func(status *FleetStatus) {
		stats := &status.Latency.Assignment
		if providerCreate {
			stats = &status.Latency.ProviderCreate
		}
		addLatency(stats, elapsed, failed)
	})
}

func (reporter *statusReporter) capacityRejected(effective int, reason string, retryAt time.Time) {
	reporter.update(func(status *FleetStatus) {
		status.Capacity.Effective = min(max(0, effective), status.Capacity.Configured)
		if status.Capacity.Rejections < math.MaxUint64 {
			status.Capacity.Rejections++
		}
		status.Capacity.Rejection = reason
		status.Capacity.RetryAt = retryAt.UTC()
	})
}

func (reporter *statusReporter) capacityRecovered() {
	reporter.update(func(status *FleetStatus) {
		status.Capacity.Effective = status.Capacity.Configured
		status.Capacity.Rejection = ""
		status.Capacity.RetryAt = time.Time{}
	})
}

func (reporter *statusReporter) degraded(reason string) {
	reporter.update(func(_ *FleetStatus) { reporter.failure = reason })
}

func (reporter *statusReporter) recovered() {
	reporter.update(func(_ *FleetStatus) { reporter.failure = "" })
}

func (reporter *statusReporter) start(ctx context.Context, budget *usageBudget) {
	if reporter == nil {
		return
	}
	reporter.mu.Lock()
	if reporter.started {
		reporter.mu.Unlock()
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	reporter.started = true
	reporter.cancel = cancel
	reporter.done = make(chan struct{})
	done := reporter.done
	reporter.mu.Unlock()
	reporter.alerter.run(loopCtx)
	go reporter.loop(loopCtx, budget, done)
}

func (reporter *statusReporter) loop(ctx context.Context, budget *usageBudget, done chan<- struct{}) {
	defer close(done)
	flushTicker := time.NewTicker(time.Second)
	heartbeatTicker := time.NewTicker(30 * time.Second)
	defer flushTicker.Stop()
	defer heartbeatTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-flushTicker.C:
			reporter.flush()
		case now := <-heartbeatTicker.C:
			reporter.budget(budget.snapshot(now))
		}
	}
}

func (reporter *statusReporter) close() {
	if reporter == nil {
		return
	}
	reporter.mu.Lock()
	cancel, done := reporter.cancel, reporter.done
	reporter.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	reporter.alerter.close()
	reporter.flush()
}

func (reporter *statusReporter) flush() {
	if reporter == nil {
		return
	}
	reporter.mu.Lock()
	if !reporter.dirty {
		reporter.mu.Unlock()
		return
	}
	snapshot, revision := reporter.status, reporter.revision
	reporter.mu.Unlock()
	if err := writeStatusFile(reporter.file, snapshot); err != nil {
		reporter.logger.Error("failed to persist fleet status", "error", err)
		return
	}
	reporter.mu.Lock()
	if reporter.revision == revision {
		reporter.dirty = false
	}
	reporter.mu.Unlock()
}

func addLatency(stats *LatencyStats, elapsed time.Duration, failed bool) {
	milliseconds := max(int64(0), elapsed.Milliseconds())
	if stats.Samples < math.MaxUint64 {
		stats.Samples++
		stats.AverageMS += (float64(milliseconds) - stats.AverageMS) / float64(stats.Samples)
	}
	if failed && stats.Failures < math.MaxUint64 {
		stats.Failures++
	}
	stats.LastMS = milliseconds
	stats.MaximumMS = max(stats.MaximumMS, milliseconds)
}

func writeStatusFile(path string, status FleetStatus) error {
	if path == "" {
		return errors.New("fleet status file is required")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create fleet status directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("refusing to replace a non-regular or symlinked fleet status file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect fleet status file: %w", err)
	}
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return fmt.Errorf("encode fleet status: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".runneryard-status-*")
	if err != nil {
		return fmt.Errorf("create temporary fleet status: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary fleet status: %w", err)
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write fleet status: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync fleet status: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close fleet status: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("commit fleet status: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure fleet status: %w", err)
	}
	directoryHandle, err := os.Open(directory) // #nosec G304 -- operator-selected durable status directory
	if err != nil {
		return fmt.Errorf("open fleet status directory: %w", err)
	}
	defer directoryHandle.Close()
	if err := directoryHandle.Sync(); err != nil {
		return fmt.Errorf("sync fleet status directory: %w", err)
	}
	return nil
}

func LoadStatus(path string) (FleetStatus, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return FleetStatus{}, errors.New("resolve fleet status path")
	}
	root, err := os.OpenRoot(filepath.Dir(absolute))
	if err != nil {
		return FleetStatus{}, errors.New("open fleet status directory")
	}
	defer root.Close()
	name := filepath.Base(absolute)
	info, err := root.Lstat(name)
	if err != nil {
		return FleetStatus{}, fmt.Errorf("inspect fleet status: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return FleetStatus{}, errors.New("fleet status must be a regular file, not a symlink")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return FleetStatus{}, errors.New("fleet status is readable by group or others")
	}
	if info.Size() < 1 || info.Size() > maximumStatusSize {
		return FleetStatus{}, errors.New("fleet status size is outside the safe range")
	}
	file, err := root.Open(name)
	if err != nil {
		return FleetStatus{}, errors.New("open fleet status")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return FleetStatus{}, errors.New("fleet status changed while it was being inspected")
	}
	var status FleetStatus
	decoder := json.NewDecoder(io.LimitReader(file, maximumStatusSize))
	if err := decoder.Decode(&status); err != nil {
		return FleetStatus{}, fmt.Errorf("decode fleet status: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return FleetStatus{}, errors.New("fleet status contains trailing data")
	}
	if err := status.validate(); err != nil {
		return FleetStatus{}, err
	}
	return status, nil
}

func (status FleetStatus) validate() error {
	if status.SchemaVersion != statusSchemaVersion || status.UpdatedAt.IsZero() || status.StartedAt.IsZero() {
		return errors.New("fleet status has an unsupported schema or missing timestamps")
	}
	if status.Health != "starting" && status.Health != "ready" && status.Health != "degraded" {
		return errors.New("fleet status contains an invalid health value")
	}
	workerValues := []int{status.Workers.Actual, status.Workers.Starting, status.Workers.Busy, status.Workers.Idle, status.Workers.Unknown, status.Workers.OrphanCandidates, status.Workers.PendingRetirements, status.Workers.Maximum}
	for _, value := range workerValues {
		if value < 0 {
			return errors.New("fleet status contains a negative worker count")
		}
	}
	if status.Capacity.Configured < 0 || status.Capacity.Effective < 0 ||
		(status.Capacity.Configured > 0 && status.Capacity.Effective > status.Capacity.Configured) {
		return errors.New("fleet status contains invalid capacity metrics")
	}
	if status.Capacity.Rejection != "" && (status.Capacity.Rejections == 0 || status.Capacity.RetryAt.IsZero()) {
		return errors.New("fleet status contains an incomplete provider capacity rejection")
	}
	if status.Workers.Busy+status.Workers.Idle+status.Workers.Unknown != status.Workers.Actual {
		return errors.New("fleet status worker counts are inconsistent")
	}
	for _, latency := range []LatencyStats{status.Latency.ProviderCreate, status.Latency.Assignment} {
		if latency.Failures > latency.Samples || latency.LastMS < 0 || latency.MaximumMS < 0 || latency.AverageMS < 0 || math.IsNaN(latency.AverageMS) || math.IsInf(latency.AverageMS, 0) {
			return errors.New("fleet status contains invalid latency metrics")
		}
	}
	budgetValues := []int64{status.Budget.LimitSeconds, status.Budget.UsedSeconds, status.Budget.ReservedSeconds, status.Budget.RemainingSeconds, status.Budget.WindowSeconds, status.Budget.BurnSecondsPerDay, status.Budget.HorizonSeconds}
	for _, value := range budgetValues {
		if value < 0 {
			return errors.New("fleet status contains a negative budget value")
		}
	}
	return nil
}
