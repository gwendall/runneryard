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
	scaleSetClient *scaleset.Client
	scaleSetID     int
	minWorkers     int
	maxWorkers     int
	maxLifetime    time.Duration
	budget         *usageBudget
	logger         *slog.Logger
}

func (s *scaler) HandleDesiredRunnerCount(ctx context.Context, assignedJobs int) (int, error) {
	if err := s.reconcile(ctx); err != nil {
		return s.state.count(), err
	}
	target := min(s.maxWorkers, s.minWorkers+assignedJobs)
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
		return fmt.Errorf("inventory compute workers: %w", err)
	}
	local := s.state.all()
	localByWorker := make(map[string]struct{}, len(local))
	for _, record := range local {
		localByWorker[record.Worker.ID] = struct{}{}
	}
	present := make(map[string]struct{}, len(workers))
	for _, worker := range workers {
		if !worker.CreatedAt.IsZero() && time.Since(worker.CreatedAt) > s.maxLifetime {
			if err := s.destroyWorker(ctx, worker); err != nil {
				return fmt.Errorf("delete worker %s after maximum lifetime: %w", worker.RunnerName, err)
			}
			if err := s.budget.settle(worker.LeaseID, time.Now()); err != nil {
				return fmt.Errorf("settle runner usage budget for %s: %w", worker.RunnerName, err)
			}
			s.state.remove(worker.RunnerName)
			s.logger.Warn("deleted worker after maximum lifetime", "runner", worker.RunnerName, "worker_id", worker.ID, "maximum_lifetime", s.maxLifetime)
			continue
		}
		present[worker.ID] = struct{}{}
		if _, ok := localByWorker[worker.ID]; ok {
			continue
		}
		if err := s.budget.adopt(worker.LeaseID, worker.CreatedAt); err != nil {
			return fmt.Errorf("adopt runner usage budget for %s: %w", worker.RunnerName, err)
		}
		s.state.add(worker, true)
		s.logger.Warn("adopted managed worker missing from local state", "runner", worker.RunnerName, "worker_id", worker.ID)
	}
	for name, record := range local {
		if _, ok := present[record.Worker.ID]; ok {
			continue
		}
		s.state.remove(name)
		if err := s.budget.settle(record.Worker.LeaseID, time.Now()); err != nil {
			return fmt.Errorf("settle runner usage budget for disappeared worker %s: %w", name, err)
		}
		s.logger.Warn("worker disappeared before completion", "runner", name, "worker_id", record.Worker.ID)
	}
	return nil
}

func (s *scaler) HandleJobStarted(_ context.Context, job *scaleset.JobStarted) error {
	if !s.state.markBusy(job.RunnerName) {
		s.logger.Warn("job started on worker not present in local state", "runner", job.RunnerName, "job_id", job.JobID)
		return nil
	}
	s.logger.Info("job started", "runner", job.RunnerName, "job_id", job.JobID, "repository", job.RepositoryName)
	return nil
}

func (s *scaler) HandleJobCompleted(ctx context.Context, job *scaleset.JobCompleted) error {
	record, ok := s.state.get(job.RunnerName)
	if !ok {
		s.logger.Warn("job completed on worker not present in local state", "runner", job.RunnerName, "job_id", job.JobID)
		return nil
	}
	if err := s.destroyWorker(ctx, record.Worker); err != nil {
		return fmt.Errorf("delete completed worker %s: %w", job.RunnerName, err)
	}
	if err := s.budget.settle(record.Worker.LeaseID, time.Now()); err != nil {
		return fmt.Errorf("settle runner usage budget for %s: %w", job.RunnerName, err)
	}
	s.state.remove(job.RunnerName)
	s.logger.Info("job completed", "runner", job.RunnerName, "job_id", job.JobID, "result", job.Result)
	return nil
}

func (s *scaler) startWorker(ctx context.Context) (bool, error) {
	leaseID := uuid.NewString()
	name := "runner-" + leaseID[:8]
	allowed, next, err := s.budget.reserve(leaseID, time.Now(), s.maxLifetime)
	if err != nil {
		return false, fmt.Errorf("reserve runner usage budget: %w", err)
	}
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
			return false, errors.Join(fmt.Errorf("generate JIT config: %w", err), fmt.Errorf("release unused runner usage reservation: %w", budgetErr))
		}
		return false, fmt.Errorf("generate JIT config: %w", err)
	}
	worker, err := s.compute.Launch(ctx, provider.Lease{
		ID:         leaseID,
		RunnerName: name,
		JITConfig:  jit.EncodedJITConfig,
		Deadline:   time.Now().Add(s.maxLifetime - 30*time.Second),
	})
	if err != nil {
		cleanupErr := s.cleanupAmbiguousLaunch(context.WithoutCancel(ctx), leaseID, err)
		if cleanupErr != nil {
			return false, cleanupErr
		}
		if budgetErr := s.budget.forfeit(leaseID, time.Now()); budgetErr != nil {
			return false, errors.Join(err, fmt.Errorf("record failed launch against runner usage budget: %w", budgetErr))
		}
		return false, err
	}
	if err := s.budget.adopt(leaseID, worker.CreatedAt); err != nil {
		cleanupErr := s.destroyWorker(context.WithoutCancel(ctx), worker)
		budgetErr := s.budget.forfeit(leaseID, time.Now())
		return false, errors.Join(
			fmt.Errorf("confirm launched worker in runner usage budget: %w", err),
			cleanupErr,
			budgetErr,
		)
	}
	s.state.add(worker, false)
	s.logger.Info("worker created", "runner", name, "worker_id", worker.ID)
	return true, nil
}

func (s *scaler) cleanupAmbiguousLaunch(ctx context.Context, leaseID string, launchErr error) error {
	var partial *provider.PartialLaunchError
	if errors.As(launchErr, &partial) && partial.Worker.ID != "" {
		if err := s.destroyWorker(ctx, partial.Worker); err != nil {
			return errors.Join(launchErr, fmt.Errorf("clean up partial worker %s: %w", partial.Worker.ID, err))
		}
	}
	workers, err := s.compute.Inventory(ctx)
	if err != nil {
		return errors.Join(launchErr, fmt.Errorf("inventory after ambiguous launch: %w", err))
	}
	for _, worker := range workers {
		if worker.LeaseID != leaseID || (partial != nil && worker.ID == partial.Worker.ID) {
			continue
		}
		if err := s.destroyWorker(ctx, worker); err != nil {
			return errors.Join(launchErr, fmt.Errorf("clean up ambiguous worker %s: %w", worker.ID, err))
		}
	}
	return nil
}

func (s *scaler) recover(ctx context.Context) error {
	workers, err := s.compute.Inventory(ctx)
	if err != nil {
		return err
	}
	activeLeases := make(map[string]struct{}, len(workers))
	for _, worker := range workers {
		if err := s.budget.adopt(worker.LeaseID, worker.CreatedAt); err != nil {
			return err
		}
		activeLeases[worker.LeaseID] = struct{}{}
		s.state.add(worker, true)
	}
	if err := s.budget.reconcile(activeLeases, time.Now()); err != nil {
		return fmt.Errorf("reconcile runner usage budget: %w", err)
	}
	if len(workers) > 0 {
		s.logger.Info("recovered existing workers", "count", len(workers))
	}
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

var _ listener.Scaler = (*scaler)(nil)
