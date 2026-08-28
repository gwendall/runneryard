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
			if err := s.retireWorker(ctx, worker, true, settleActualUsage); err != nil {
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
		s.state.adopt(worker)
		s.logger.Warn("adopted managed worker missing from local state", "runner", worker.RunnerName, "worker_id", worker.ID)
	}
	for name, record := range local {
		if _, retiredNow := retired[name]; retiredNow {
			continue
		}
		if _, ok := present[record.Worker.ID]; ok {
			continue
		}
		if err := s.retireWorker(ctx, record.Worker, false, settleActualUsage); err != nil {
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
	if err := s.retireWorker(ctx, record.Worker, true, settleActualUsage); err != nil {
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
	name := "runner-" + leaseID
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
	existing, err := s.scaleSetClient.GetRunnerByName(ctx, name)
	if err != nil {
		if budgetErr := s.budget.release(leaseID); budgetErr != nil {
			return false, errors.Join(fmt.Errorf("preflight runner registration: %w", err), budgetErr)
		}
		return false, fmt.Errorf("preflight runner registration: %w", err)
	}
	if existing != nil {
		if budgetErr := s.budget.release(leaseID); budgetErr != nil {
			return false, errors.Join(fmt.Errorf("generated runner name %q already exists", name), budgetErr)
		}
		return false, fmt.Errorf("generated runner name %q already exists", name)
	}
	retirement := retirementEntry{
		RunnerName: name, RunnerScaleSetID: s.scaleSetID, LeaseID: leaseID, BudgetDisposition: forfeitReservation,
	}
	if err := s.retirements.put(retirement); err != nil {
		s.reporter.degraded("runner_retirement_state_failed")
		budgetErr := s.budget.forfeit(leaseID, time.Now())
		return false, errors.Join(fmt.Errorf("journal runner registration intent: %w", err), budgetErr)
	}
	s.reporter.retirements(s.retirements.count())
	jit, err := s.scaleSetClient.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{
		Name:       name,
		WorkFolder: "_work",
	}, s.scaleSetID)
	if err != nil {
		return false, s.cleanupJITGenerationFailure(context.WithoutCancel(ctx), retirement, err)
	}
	if jit == nil {
		return false, s.cleanupJITGenerationFailure(context.WithoutCancel(ctx), retirement, errors.New("GitHub returned an empty JIT runner response"))
	}
	registration := jit.Runner
	if registration == nil || registration.ID < 1 {
		registration, err = s.scaleSetClient.GetRunnerByName(ctx, name)
		if err != nil {
			return false, fmt.Errorf("resolve generated runner registration: %w", err)
		}
	}
	if registration == nil || registration.ID < 1 || registration.Name != name || (registration.RunnerScaleSetID != 0 && registration.RunnerScaleSetID != s.scaleSetID) {
		return false, fmt.Errorf("GitHub returned invalid identity for generated runner %q", name)
	}
	retirement.RunnerID = int64(registration.ID)
	if err := s.retirements.put(retirement); err != nil {
		s.reporter.degraded("runner_retirement_state_failed")
		return false, fmt.Errorf("bind generated runner registration: %w", err)
	}
	s.reporter.starting(1)
	launchStarted := time.Now()
	worker, err := s.compute.Launch(ctx, provider.Lease{
		ID: leaseID, RunnerName: name, RunnerID: retirement.RunnerID, RunnerScaleSetID: s.scaleSetID,
		JITConfig: jit.EncodedJITConfig, Deadline: time.Now().Add(s.maxLifetime - 30*time.Second),
	})
	s.reporter.starting(-1)
	s.reporter.latency(true, time.Since(launchStarted), err != nil)
	if err != nil {
		s.reporter.degraded("provider_launch_failed")
		cleanupErr := s.cleanupAmbiguousLaunch(context.WithoutCancel(ctx), retirement, err)
		if cleanupErr != nil {
			return false, cleanupErr
		}
		s.reporter.budget(s.budget.snapshot(time.Now()))
		return false, err
	}
	if worker.RunnerID == 0 {
		worker.RunnerID = retirement.RunnerID
	}
	if worker.RunnerScaleSetID == 0 {
		worker.RunnerScaleSetID = retirement.RunnerScaleSetID
	}
	if err := s.budget.adopt(leaseID, worker.CreatedAt); err != nil {
		s.reporter.degraded("usage_budget_write_failed")
		cleanupErr := s.retireWorker(context.WithoutCancel(ctx), worker, true, forfeitReservation)
		s.reporter.budget(s.budget.snapshot(time.Now()))
		return false, errors.Join(
			fmt.Errorf("confirm launched worker in runner usage budget: %w", err),
			cleanupErr,
		)
	}
	s.state.add(worker, false)
	if err := s.retirements.remove(name); err != nil {
		s.reporter.degraded("runner_retirement_state_failed")
		return false, fmt.Errorf("commit launched runner state: %w", err)
	}
	s.reporter.retirements(s.retirements.count())
	s.reportState()
	s.reporter.budget(s.budget.snapshot(time.Now()))
	s.reporter.recovered()
	s.logger.Info("worker created", "runner", name, "worker_id", worker.ID)
	return true, nil
}

func (s *scaler) cleanupJITGenerationFailure(ctx context.Context, retirement retirementEntry, generationErr error) error {
	runner, err := s.scaleSetClient.GetRunnerByName(ctx, retirement.RunnerName)
	if err != nil {
		return errors.Join(generationErr, fmt.Errorf("resolve ambiguous JIT generation: %w", err))
	}
	if runner == nil {
		if err := s.retirements.remove(retirement.RunnerName); err != nil {
			return errors.Join(generationErr, err)
		}
		s.reporter.retirements(s.retirements.count())
		if err := s.budget.release(retirement.LeaseID); err != nil {
			return errors.Join(generationErr, err)
		}
		return generationErr
	}
	if runner.Name != retirement.RunnerName || (runner.RunnerScaleSetID != 0 && runner.RunnerScaleSetID != retirement.RunnerScaleSetID) {
		return errors.Join(generationErr, fmt.Errorf("ambiguous JIT generation returned an unexpected runner identity"))
	}
	retirement.RunnerID = int64(runner.ID)
	if err := s.retirements.put(retirement); err != nil {
		return errors.Join(generationErr, err)
	}
	if err := s.finishRetirement(ctx, provider.Worker{LeaseID: retirement.LeaseID, RunnerName: retirement.RunnerName}, false, retirement); err != nil {
		return errors.Join(generationErr, err)
	}
	return generationErr
}

func (s *scaler) cleanupAmbiguousLaunch(ctx context.Context, retirement retirementEntry, launchErr error) error {
	var partial *provider.PartialLaunchError
	if errors.As(launchErr, &partial) && partial.Worker.ID != "" {
		if err := s.retireWorker(ctx, partial.Worker, true, forfeitReservation); err != nil {
			return errors.Join(launchErr, fmt.Errorf("clean up partial worker %s: %w", partial.Worker.ID, err))
		}
	}
	workers, err := s.compute.Inventory(ctx)
	if err != nil {
		return errors.Join(launchErr, fmt.Errorf("inventory after ambiguous launch: %w", err))
	}
	cleanedWorker := false
	for _, worker := range workers {
		if worker.LeaseID != retirement.LeaseID || (partial != nil && worker.ID == partial.Worker.ID) {
			continue
		}
		if err := s.retireWorker(ctx, worker, true, forfeitReservation); err != nil {
			return errors.Join(launchErr, fmt.Errorf("clean up ambiguous worker %s: %w", worker.ID, err))
		}
		cleanedWorker = true
	}
	if cleanedWorker {
		return nil
	}
	if err := s.finishRetirement(ctx, provider.Worker{LeaseID: retirement.LeaseID, RunnerName: retirement.RunnerName}, false, retirement); err != nil {
		return errors.Join(launchErr, fmt.Errorf("clean up ambiguous runner registration %s: %w", retirement.RunnerName, err))
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
		s.state.adopt(worker)
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
	for _, retirement := range s.retirements.all() {
		worker, providerPresent := byName[retirement.RunnerName]
		if !providerPresent {
			if record, ok := s.state.get(retirement.RunnerName); ok {
				worker = record.Worker
			} else {
				worker = provider.Worker{
					LeaseID: retirement.LeaseID, RunnerName: retirement.RunnerName,
					RunnerID: retirement.RunnerID, RunnerScaleSetID: retirement.RunnerScaleSetID,
				}
			}
		}
		if err := validateWorkerRetirementProof(worker, retirement); err != nil {
			return err
		}
		if err := s.finishRetirement(ctx, worker, providerPresent, retirement); err != nil {
			return fmt.Errorf("finish pending retirement for %s: %w", retirement.RunnerName, err)
		}
	}
	return nil
}

func (s *scaler) retireWorker(ctx context.Context, worker provider.Worker, providerPresent bool, disposition budgetDisposition) error {
	retirement := retirementEntry{
		RunnerName: worker.RunnerName, RunnerID: worker.RunnerID, RunnerScaleSetID: worker.RunnerScaleSetID,
		LeaseID: worker.LeaseID, BudgetDisposition: disposition,
	}
	if retirement.RunnerScaleSetID == 0 {
		retirement.RunnerScaleSetID = s.scaleSetID
	}
	if err := s.retirements.put(retirement); err != nil {
		s.reporter.degraded("runner_retirement_state_failed")
		return err
	}
	s.reporter.retirements(s.retirements.count())
	return s.finishRetirement(ctx, worker, providerPresent, retirement)
}

func validateWorkerRetirementProof(worker provider.Worker, retirement retirementEntry) error {
	if worker.RunnerName != retirement.RunnerName || worker.LeaseID != retirement.LeaseID {
		return fmt.Errorf("provider worker identity does not match retirement proof for %q", retirement.RunnerName)
	}
	if worker.RunnerID != 0 && retirement.RunnerID != 0 && worker.RunnerID != retirement.RunnerID {
		return fmt.Errorf("provider worker registration id does not match retirement proof for %q", retirement.RunnerName)
	}
	if worker.RunnerScaleSetID != 0 && worker.RunnerScaleSetID != retirement.RunnerScaleSetID {
		return fmt.Errorf("provider worker scale set does not match retirement proof for %q", retirement.RunnerName)
	}
	return nil
}

func (s *scaler) finishRetirement(ctx context.Context, worker provider.Worker, providerPresent bool, retirement retirementEntry) error {
	if err := validateWorkerRetirementProof(worker, retirement); err != nil {
		return err
	}
	if providerPresent {
		if err := s.destroyWorker(ctx, worker); err != nil {
			s.reporter.degraded("provider_delete_failed")
			return err
		}
	}
	if err := s.removeRunnerRegistration(ctx, retirement); err != nil {
		s.reporter.degraded("github_runner_cleanup_failed")
		return err
	}
	var budgetErr error
	if retirement.BudgetDisposition == forfeitReservation {
		budgetErr = s.budget.forfeit(retirement.LeaseID, time.Now())
	} else {
		budgetErr = s.budget.settle(retirement.LeaseID, time.Now())
	}
	if budgetErr != nil {
		s.reporter.degraded("usage_budget_write_failed")
		return budgetErr
	}
	s.state.remove(retirement.RunnerName)
	if err := s.retirements.remove(retirement.RunnerName); err != nil {
		s.reporter.degraded("runner_retirement_state_failed")
		return err
	}
	s.reporter.retirements(s.retirements.count())
	return nil
}

func (s *scaler) removeRunnerRegistration(ctx context.Context, retirement retirementEntry) error {
	if retirement.RunnerScaleSetID != s.scaleSetID {
		return fmt.Errorf("refusing to remove runner from unexpected scale set %d", retirement.RunnerScaleSetID)
	}
	runner, err := s.scaleSetClient.GetRunnerByName(ctx, retirement.RunnerName)
	if err != nil {
		return fmt.Errorf("find GitHub runner registration: %w", err)
	}
	if runner == nil {
		return nil
	}
	if runner.Name != retirement.RunnerName || (runner.RunnerScaleSetID != 0 && runner.RunnerScaleSetID != retirement.RunnerScaleSetID) {
		return fmt.Errorf("refusing to remove runner %q from unexpected scale set %d", runner.Name, runner.RunnerScaleSetID)
	}
	if retirement.RunnerID == 0 {
		retirement.RunnerID = int64(runner.ID)
		if err := s.retirements.put(retirement); err != nil {
			return fmt.Errorf("bind GitHub runner registration before removal: %w", err)
		}
	}
	if int64(runner.ID) != retirement.RunnerID {
		return fmt.Errorf("refusing to remove runner %q with unexpected registration id %d", runner.Name, runner.ID)
	}
	if err := s.scaleSetClient.RemoveRunner(ctx, retirement.RunnerID); err != nil {
		return fmt.Errorf("remove GitHub runner registration %d: %w", runner.ID, err)
	}
	s.logger.Info("removed GitHub runner registration", "runner", retirement.RunnerName, "runner_id", runner.ID)
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
	actual, busy, idle, unknown := s.state.summary()
	s.reporter.workers(actual, busy, idle, unknown)
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
