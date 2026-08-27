package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
	"github.com/gwendall/runneryard/provider"
)

type scaler struct {
	state          *workerState
	compute        provider.Compute
	scaleSetClient runnerScaleSetClient
	scaleSetID     int
	minWorkers     int
	maxWorkers     int
	maxLifetime    time.Duration
	budget         *usageBudget
	retirements    *retirementQueue
	reporter       *statusReporter
	logger         *slog.Logger
}

type runnerScaleSetClient interface {
	GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error)
	RemoveRunner(context.Context, int64) error
}

func (s *scaler) HandleDesiredRunnerCount(ctx context.Context, assignedJobs int) (int, error) {
	target := min(s.maxWorkers, s.minWorkers+assignedJobs)
	s.reporter.desired(assignedJobs, target)
	if err := s.reconcile(ctx); err != nil {
		return s.state.count(), err
	}
	current := s.state.count()
	if target == current {
		return current, nil
	}

	if target > current {
		needed := target - current
		s.logger.Info("scaling up", "current", current, "target", target, "count", needed)
		for range needed {
			started, err := s.startWorker(ctx)
			if err != nil {
				return s.state.count(), err
			}
			if !started {
				break
			}
		}
		return s.state.count(), nil
	}

	// Do not scale down from a desired-count update. GitHub can assign a job to
	// an apparently idle JIT runner immediately before the JobStarted message is
	// delivered. Destroying that runner here strands the assigned job until
	// GitHub's timeout. JobCompleted removes one-job runners synchronously; a
	// runner that never receives a job is bounded by its provider lease.
	return current, nil
}

func (s *scaler) reconcile(ctx context.Context) error {
	workers, err := s.compute.Inventory(ctx)
	if err != nil {
		s.reporter.degraded("provider_inventory_failed")
		return fmt.Errorf("inventory compute workers: %w", err)
	}
	if s.retirements.count() > 0 {
		if err := s.finishPendingRetirements(ctx, workers); err != nil {
			s.reporter.degraded("runner_retirement_failed")
			return err
		}
		workers, err = s.compute.Inventory(ctx)
		if err != nil {
			s.reporter.degraded("provider_inventory_failed")
			return fmt.Errorf("refresh inventory after runner retirement: %w", err)
		}
	}
	local := s.state.all()
	s.reporter.orphans(orphanCandidateCount(workers, local, s.maxLifetime, time.Now()))
	localByWorker := make(map[string]struct{}, len(local))
	for _, record := range local {
		localByWorker[record.Worker.ID] = struct{}{}
	}
	present := make(map[string]struct{}, len(workers))
	retired := make(map[string]struct{})
	for _, worker := range workers {
		if !worker.CreatedAt.IsZero() && time.Since(worker.CreatedAt) > s.maxLifetime {
			if err := s.retireWorker(ctx, worker, true); err != nil {
				return fmt.Errorf("retire worker %s after maximum lifetime: %w", worker.RunnerName, err)
			}
			retired[worker.RunnerName] = struct{}{}
			s.logger.Warn("deleted worker after maximum lifetime", "runner", worker.RunnerName, "worker_id", worker.ID, "maximum_lifetime", s.maxLifetime)
			continue
		}
		present[worker.ID] = struct{}{}
		if _, ok := localByWorker[worker.ID]; ok {
			continue
		}
		if err := s.budget.adopt(worker.LeaseID, worker.CreatedAt); err != nil {
			s.reporter.degraded("usage_budget_write_failed")
			return fmt.Errorf("adopt runner usage budget for %s: %w", worker.RunnerName, err)
		}
		s.state.add(worker, true)
		s.logger.Warn("adopted managed worker missing from local state", "runner", worker.RunnerName, "worker_id", worker.ID)
	}
	for name, record := range local {
		if _, retiredNow := retired[name]; retiredNow {
			continue
		}
		if _, ok := present[record.Worker.ID]; ok {
			continue
		}
		if err := s.retireWorker(ctx, record.Worker, false); err != nil {
			return fmt.Errorf("retire disappeared worker %s: %w", name, err)
		}
		s.logger.Warn("worker disappeared before completion", "runner", name, "worker_id", record.Worker.ID)
	}
	s.reporter.orphans(0)
	s.reportState()
	s.reporter.budget(s.budget.snapshot(time.Now()))
	s.reporter.recovered()
	return nil
}

func (s *scaler) HandleJobStarted(_ context.Context, job *scaleset.JobStarted) error {
	s.reporter.githubActivity("job_started")
	if !s.state.markBusy(job.RunnerName) {
		s.logger.Warn("job started on worker not present in local state", "runner", job.RunnerName, "job_id", job.JobID)
		return nil
	}
	if record, ok := s.state.get(job.RunnerName); ok && !record.Worker.CreatedAt.IsZero() {
		s.reporter.latency(false, time.Since(record.Worker.CreatedAt), false)
	}
	s.reportState()
	s.logger.Info("job started", "runner", job.RunnerName, "job_id", job.JobID, "repository", job.RepositoryName)
	return nil
}

func (s *scaler) HandleJobCompleted(ctx context.Context, job *scaleset.JobCompleted) error {
	s.reporter.githubActivity("job_completed")
	record, ok := s.state.get(job.RunnerName)
	if !ok {
		s.logger.Warn("job completed on worker not present in local state", "runner", job.RunnerName, "job_id", job.JobID)
		return nil
	}
	if err := s.retireWorker(ctx, record.Worker, true); err != nil {
		return fmt.Errorf("retire completed worker %s: %w", job.RunnerName, err)
	}
	s.reportState()
	s.reporter.budget(s.budget.snapshot(time.Now()))
	s.reporter.recovered()
	s.logger.Info("job completed", "runner", job.RunnerName, "job_id", job.JobID, "result", job.Result)
	return nil
}

func (s *scaler) startWorker(ctx context.Context) (bool, error) {
	leaseID := uuid.NewString()
	name := "runner-" + leaseID[:8]
	allowed, next, err := s.budget.reserve(leaseID, time.Now(), s.maxLifetime)
	if err != nil {
		s.reporter.degraded("usage_budget_write_failed")
		return false, fmt.Errorf("reserve runner usage budget: %w", err)
	}
	s.reporter.budget(s.budget.snapshot(time.Now()))
	if !allowed {
		s.logger.Warn("runner usage budget exhausted; leaving jobs queued", "next_release", next, "budget_window", s.budget.window, "budget", s.budget.limit)
		return false, nil
	}
	jit, err := s.scaleSetClient.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{
		Name:       name,
		WorkFolder: "_work",
	}, s.scaleSetID)
	if err != nil {
		if budgetErr := s.budget.release(leaseID); budgetErr != nil {
			s.reporter.degraded("usage_budget_write_failed")
			return false, errors.Join(fmt.Errorf("generate JIT config: %w", err), fmt.Errorf("release unused runner usage reservation: %w", budgetErr))
		}
		s.reporter.budget(s.budget.snapshot(time.Now()))
		return false, fmt.Errorf("generate JIT config: %w", err)
	}
	s.reporter.starting(1)
	launchStarted := time.Now()
	worker, err := s.compute.Launch(ctx, provider.Lease{
		ID:         leaseID,
		RunnerName: name,
		JITConfig:  jit.EncodedJITConfig,
		Deadline:   time.Now().Add(s.maxLifetime - 30*time.Second),
	})
	s.reporter.starting(-1)
	s.reporter.latency(true, time.Since(launchStarted), err != nil)
	if err != nil {
		s.reporter.degraded("provider_launch_failed")
		cleanupErr := s.cleanupAmbiguousLaunch(context.WithoutCancel(ctx), leaseID, name, err)
		if cleanupErr != nil {
			return false, cleanupErr
		}
		if budgetErr := s.budget.forfeit(leaseID, time.Now()); budgetErr != nil {
			s.reporter.degraded("usage_budget_write_failed")
			return false, errors.Join(err, fmt.Errorf("record failed launch against runner usage budget: %w", budgetErr))
		}
		s.reporter.budget(s.budget.snapshot(time.Now()))
		return false, err
	}
	if err := s.budget.adopt(leaseID, worker.CreatedAt); err != nil {
		s.reporter.degraded("usage_budget_write_failed")
		cleanupErr := s.retireWorker(context.WithoutCancel(ctx), worker, true)
		budgetErr := s.budget.forfeit(leaseID, time.Now())
		s.reporter.budget(s.budget.snapshot(time.Now()))
		return false, errors.Join(
			fmt.Errorf("confirm launched worker in runner usage budget: %w", err),
			cleanupErr,
			budgetErr,
		)
	}
	s.state.add(worker, false)
	s.reportState()
	s.reporter.budget(s.budget.snapshot(time.Now()))
	s.reporter.recovered()
	s.logger.Info("worker created", "runner", name, "worker_id", worker.ID)
	return true, nil
}

func (s *scaler) cleanupAmbiguousLaunch(ctx context.Context, leaseID, runnerName string, launchErr error) error {
	var partial *provider.PartialLaunchError
	if errors.As(launchErr, &partial) && partial.Worker.ID != "" {
		if err := s.retireWorker(ctx, partial.Worker, true); err != nil {
			return errors.Join(launchErr, fmt.Errorf("clean up partial worker %s: %w", partial.Worker.ID, err))
		}
	}
	workers, err := s.compute.Inventory(ctx)
	if err != nil {
		return errors.Join(launchErr, fmt.Errorf("inventory after ambiguous launch: %w", err))
	}
	cleanedWorker := false
	for _, worker := range workers {
		if worker.LeaseID != leaseID || (partial != nil && worker.ID == partial.Worker.ID) {
			continue
		}
		if err := s.retireWorker(ctx, worker, true); err != nil {
			return errors.Join(launchErr, fmt.Errorf("clean up ambiguous worker %s: %w", worker.ID, err))
		}
		cleanedWorker = true
	}
	if cleanedWorker {
		return nil
	}
	if err := s.retireWorker(ctx, provider.Worker{LeaseID: leaseID, RunnerName: runnerName}, false); err != nil {
		return errors.Join(launchErr, fmt.Errorf("clean up ambiguous runner registration %s: %w", runnerName, err))
	}
	return nil
}

func (s *scaler) recover(ctx context.Context) error {
	workers, err := s.compute.Inventory(ctx)
	if err != nil {
		s.reporter.degraded("provider_inventory_failed")
		return err
	}
	if err := s.finishPendingRetirements(ctx, workers); err != nil {
		s.reporter.degraded("runner_retirement_failed")
		return err
	}
	workers, err = s.compute.Inventory(ctx)
	if err != nil {
		s.reporter.degraded("provider_inventory_failed")
		return fmt.Errorf("refresh inventory after recovering runner retirements: %w", err)
	}
	s.reporter.orphans(len(workers))
	activeLeases := make(map[string]struct{}, len(workers))
	for _, worker := range workers {
		if err := s.budget.adopt(worker.LeaseID, worker.CreatedAt); err != nil {
			s.reporter.degraded("usage_budget_write_failed")
			return err
		}
		activeLeases[worker.LeaseID] = struct{}{}
		s.state.add(worker, true)
	}
	if err := s.budget.reconcile(activeLeases, time.Now()); err != nil {
		s.reporter.degraded("usage_budget_write_failed")
		return fmt.Errorf("reconcile runner usage budget: %w", err)
	}
	if len(workers) > 0 {
		s.logger.Info("recovered existing workers", "count", len(workers))
	}
	s.reporter.orphans(0)
	s.reportState()
	s.reporter.budget(s.budget.snapshot(time.Now()))
	return nil
}

func (s *scaler) finishPendingRetirements(ctx context.Context, workers []provider.Worker) error {
	byName := make(map[string]provider.Worker, len(workers))
	for _, worker := range workers {
		if _, duplicate := byName[worker.RunnerName]; duplicate {
			return fmt.Errorf("provider inventory contains duplicate runner name %q", worker.RunnerName)
		}
		byName[worker.RunnerName] = worker
	}
	for _, name := range s.retirements.all() {
		worker, providerPresent := byName[name]
		if !providerPresent {
			if record, ok := s.state.get(name); ok {
				worker = record.Worker
			} else {
				worker.RunnerName = name
			}
		}
		if err := s.finishRetirement(ctx, worker, providerPresent); err != nil {
			return fmt.Errorf("finish pending retirement for %s: %w", name, err)
		}
	}
	return nil
}

func (s *scaler) retireWorker(ctx context.Context, worker provider.Worker, providerPresent bool) error {
	if err := s.retirements.add(worker.RunnerName); err != nil {
		s.reporter.degraded("runner_retirement_state_failed")
		return err
	}
	s.reporter.retirements(s.retirements.count())
	return s.finishRetirement(ctx, worker, providerPresent)
}

func (s *scaler) finishRetirement(ctx context.Context, worker provider.Worker, providerPresent bool) error {
	if providerPresent {
		if err := s.destroyWorker(ctx, worker); err != nil {
			s.reporter.degraded("provider_delete_failed")
			return err
		}
	}
	if err := s.removeRunnerRegistration(ctx, worker.RunnerName); err != nil {
		s.reporter.degraded("github_runner_cleanup_failed")
		return err
	}
	if worker.LeaseID != "" {
		if err := s.budget.settle(worker.LeaseID, time.Now()); err != nil {
			s.reporter.degraded("usage_budget_write_failed")
			return err
		}
	}
	s.state.remove(worker.RunnerName)
	if err := s.retirements.remove(worker.RunnerName); err != nil {
		s.reporter.degraded("runner_retirement_state_failed")
		return err
	}
	s.reporter.retirements(s.retirements.count())
	return nil
}

func (s *scaler) removeRunnerRegistration(ctx context.Context, name string) error {
	runner, err := s.scaleSetClient.GetRunnerByName(ctx, name)
	if err != nil {
		return fmt.Errorf("find GitHub runner registration: %w", err)
	}
	if runner == nil {
		return nil
	}
	// The Actions service omits runnerScaleSetId on some responses. A non-zero
	// value must match; otherwise the durable, controller-generated random name
	// is the ownership proof.
	if runner.Name != name || (runner.RunnerScaleSetID != 0 && runner.RunnerScaleSetID != s.scaleSetID) {
		return fmt.Errorf("refusing to remove runner %q from unexpected scale set %d", runner.Name, runner.RunnerScaleSetID)
	}
	if err := s.scaleSetClient.RemoveRunner(ctx, int64(runner.ID)); err != nil {
		return fmt.Errorf("remove GitHub runner registration %d: %w", runner.ID, err)
	}
	s.logger.Info("removed GitHub runner registration", "runner", name, "runner_id", runner.ID)
	return nil
}

func (s *scaler) shutdown(_ context.Context) {
	// Preserve workers across controller replacement. "Idle" only means that a
	// JobStarted event has not arrived yet, so deleting those workers during a
	// deploy has the same assignment race as desired-count scale-down. Workers
	// remain bounded by their one-job JIT configuration and provider deadline;
	// the successor adopts them from inventory.
	if count := s.state.count(); count > 0 {
		s.logger.Info("preserving workers for controller successor", "count", count)
	}
}

// destroyWorker resolves an ambiguous provider error against authoritative
// inventory. A delete request can time out after the provider accepted it; in
// that case restarting the scale-set listener would interrupt unrelated jobs
// even though the worker is already gone. A worker still present in inventory
// remains a hard error so reconciliation continues to fail closed.
func (s *scaler) destroyWorker(ctx context.Context, worker provider.Worker) error {
	deleteErr := s.compute.Destroy(ctx, worker.ID)
	if deleteErr == nil {
		return nil
	}
	workers, inventoryErr := s.compute.Inventory(ctx)
	if inventoryErr != nil {
		return errors.Join(deleteErr, fmt.Errorf("confirm deletion of worker %s: %w", worker.ID, inventoryErr))
	}
	for _, present := range workers {
		if present.ID == worker.ID {
			return deleteErr
		}
	}
	s.logger.Warn("provider reported a deletion error after worker disappeared", "runner", worker.RunnerName, "worker_id", worker.ID, "error", deleteErr)
	return nil
}

func (s *scaler) reportState() {
	actual, busy, idle := s.state.summary()
	s.reporter.workers(actual, busy, idle)
}

func orphanCandidateCount(workers []provider.Worker, local map[string]workerRecord, maximumLifetime time.Duration, now time.Time) int {
	known := make(map[string]struct{}, len(local))
	for _, record := range local {
		known[record.Worker.ID] = struct{}{}
	}
	candidates := 0
	for _, worker := range workers {
		_, present := known[worker.ID]
		expired := !worker.CreatedAt.IsZero() && now.Sub(worker.CreatedAt) > maximumLifetime
		invalid := worker.ID == "" || worker.LeaseID == "" || worker.RunnerName == "" || worker.CreatedAt.IsZero()
		if !present || expired || invalid {
			candidates++
		}
	}
	return candidates
}

var _ listener.Scaler = (*scaler)(nil)
