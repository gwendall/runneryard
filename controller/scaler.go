package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
	"github.com/gwendall/runneryard/provider"
	"github.com/gwendall/runneryard/provider/retry"
)

var errRetirementDeferred = errors.New("runner retirement deferred")

// workerTeardownGrace approximates the time between GitHub's job finish
// timestamp and the one-job worker's exit and destruction.
const workerTeardownGrace = 15 * time.Second

const (
	capacityInitialBackoff = time.Minute
	capacityMaximumBackoff = 15 * time.Minute
)

// The launch gate has two kinds: a provider capacity ceiling, and the
// provider's permanent rejection of the launch request itself. Both keep the
// GitHub session, both back off and probe; they differ only in what the
// operator is told.
const (
	gateCapacity = "capacity"
	gateLaunch   = "launch"
)

// launchRejectedError reports a provider's permanent refusal of one launch
// request after the controller proved that no worker carries the lease. It
// is neither transient (the adapter did not retry it) nor an identity failure
// (401 and 403 stay fatal), so the core treats it like a capacity ceiling:
// degraded status, bounded backoff, one probe at a time, no controller exit.
// Before 0.4.4 any such response - an unusable image, a region without the
// requested shape, a 409 on a name - stopped the whole controller.
type launchRejectedError struct {
	// Reason is a stable, non-secret code such as "fly_status_422".
	Reason string
	Err    error
}

func (e *launchRejectedError) Error() string {
	return fmt.Sprintf("provider rejected worker launch (%s): %v", e.Reason, e.Err)
}

func (e *launchRejectedError) Unwrap() error { return e.Err }

// classifyLaunchFailure sorts a provider launch error into the classes the
// core handles: transient and capacity errors pass through, an authorization
// failure stays fatal so a bad credential fails closed, and every other
// permanent response becomes a bounded launch rejection.
func classifyLaunchFailure(err error) error {
	if err == nil || provider.IsTransient(err) || provider.IsCapacity(err) {
		return err
	}
	var status *retry.StatusError
	if errors.As(err, &status) {
		if status.Status == http.StatusUnauthorized || status.Status == http.StatusForbidden {
			return err
		}
		return &launchRejectedError{Reason: fmt.Sprintf("%s_status_%d", status.Provider, status.Status), Err: err}
	}
	return &launchRejectedError{Reason: "provider_launch_rejected", Err: err}
}

// handlerFailure marks an error the scaler returned from a listener handler.
// Those are the fail-closed cases - identity, state, or ledger corruption -
// and they end the controller. Every other error that reaches the session
// supervisor comes from the GitHub transport and is retried.
type handlerFailure struct {
	Err error
}

func (e *handlerFailure) Error() string { return e.Err.Error() }

func (e *handlerFailure) Unwrap() error { return e.Err }

func failClosed(err error) error {
	if err == nil {
		return nil
	}
	var already *handlerFailure
	if errors.As(err, &already) {
		return err
	}
	return &handlerFailure{Err: err}
}

// isHandlerFailure reports whether err carries a scaler handler failure.
func isHandlerFailure(err error) bool {
	var failure *handlerFailure
	return errors.As(err, &failure)
}

// predecessorWindow is how long after start-up a message about a runner this
// process never created is attributed to its predecessor. After a restart
// GitHub replays the completions of every worker the previous controller
// launched; forty warnings for routine successes hide the one that matters.
const predecessorWindow = departureMemory

// workerStoppedAt derives the worker's stop time from GitHub's job finish
// timestamp; a zero timestamp leaves the ledger to settle at message arrival.
func workerStoppedAt(finished time.Time) time.Time {
	if finished.IsZero() {
		return time.Time{}
	}
	return finished.Add(workerTeardownGrace)
}

type scaler struct {
	state             *workerState
	compute           provider.Compute
	scaleSetClient    runnerScaleSetClient
	scaleSetID        int
	minWorkers        int
	maxWorkers        int
	launchConcurrency int
	maxLifetime       time.Duration
	idleTimeout       time.Duration
	danglingTimeout   time.Duration
	budget            *usageBudget
	retirements       *retirementQueue
	reporter          *statusReporter
	logger            *slog.Logger
	// startedAt dates this controller process; zero disables predecessor
	// attribution (tests construct the scaler directly).
	startedAt         time.Time
	capacityMu        sync.Mutex
	capacityKind      string
	capacityEffective int
	capacityReason    string
	capacityRetryAt   time.Time
	capacityBackoff   time.Duration
	capacityNow       func() time.Time
}

type runnerScaleSetClient interface {
	GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error)
	RemoveRunner(context.Context, int64) error
}

// HandleDesiredRunnerCount, HandleJobStarted, and HandleJobCompleted are the
// listener's handlers. An error they return is a fail-closed condition -
// identity, state, or ledger corruption - and is marked as such so the session
// supervisor ends the controller instead of reopening the session.
func (s *scaler) HandleDesiredRunnerCount(ctx context.Context, assignedJobs int) (int, error) {
	count, err := s.handleDesiredRunnerCount(ctx, assignedJobs)
	return count, failClosed(err)
}

func (s *scaler) handleDesiredRunnerCount(ctx context.Context, assignedJobs int) (int, error) {
	target := min(s.maxWorkers, s.minWorkers+assignedJobs)
	s.reporter.desired(assignedJobs, target)
	if err := s.reconcile(ctx); err != nil {
		// A transient provider failure already exhausted the adapter's retry
		// policy. Keep the GitHub session and the degraded status, skip this
		// cycle, and let the next message reconcile again. Every other error
		// remains fatal so identity and state corruption still fail closed.
		if provider.IsTransient(err) {
			s.logger.Warn("provider unavailable during reconciliation; retrying on the next message", "error", err)
			return s.state.count(), nil
		}
		return s.state.count(), err
	}
	current := s.state.count()
	if target == current {
		return current, nil
	}

	if target > current {
		needed, probing := s.capacityAllowance(current, target)
		if needed == 0 {
			return current, nil
		}
		s.logger.Info("scaling up", "current", current, "target", target, "count", needed)
		if err := s.launchWorkers(ctx, needed); err != nil {
			return s.state.count(), err
		}
		if probing && s.state.count() > current {
			s.clearLaunchGate()
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

// launchWorkers starts up to needed workers with at most launchConcurrency
// provider calls in flight. Budget admission, the retirement journal, and
// local state stay serialized behind their own locks; only the slow provider
// and GitHub round trips overlap, so a burst of assigned jobs no longer
// delays JobCompleted handling behind a queue of sequential launches.
//
// The first refusal (exhausted budget), transient provider failure, capacity
// ceiling, or launch rejection stops further launches; workers already in
// flight finish normally. Only a fail-closed error is returned, after every
// in-flight launch has settled.
func (s *scaler) launchWorkers(ctx context.Context, needed int) error {
	limit := max(1, s.launchConcurrency)
	slots := make(chan struct{}, limit)
	var wait sync.WaitGroup
	var mu sync.Mutex
	var fatal error
	var capacityErr error
	var rejectedErr error
	stopped := false
	for range needed {
		slots <- struct{}{}
		// Re-check after acquiring a slot: a launch that just finished may have
		// refused admission or hit a transient failure, and its slot release is
		// what let this iteration proceed.
		mu.Lock()
		stop := stopped
		mu.Unlock()
		if stop {
			<-slots
			break
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			defer func() { <-slots }()
			started, err := s.startWorker(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				stopped = true
				if provider.IsCapacity(err) {
					if capacityErr == nil {
						capacityErr = err
					}
					return
				}
				if provider.IsTransient(err) {
					s.logger.Warn("provider unavailable during launch; leaving jobs queued until the next message", "error", err)
					return
				}
				var rejected *launchRejectedError
				if errors.As(err, &rejected) {
					if rejectedErr == nil {
						rejectedErr = err
					}
					return
				}
				if fatal == nil {
					fatal = err
				}
				return
			}
			if !started {
				stopped = true
			}
		}()
	}
	wait.Wait()
	switch {
	case capacityErr != nil:
		s.recordLaunchGate(gateCapacity, provider.CapacityReason(capacityErr), capacityErr)
	case rejectedErr != nil:
		s.recordLaunchGate(gateLaunch, launchRejectionReason(rejectedErr), rejectedErr)
	}
	return fatal
}

// launchRejectionReason returns the stable code carried by a launch
// rejection, never the provider's wording.
func launchRejectionReason(err error) string {
	var rejected *launchRejectedError
	if errors.As(err, &rejected) && rejected.Reason != "" {
		return rejected.Reason
	}
	return "provider_launch_rejected"
}

func (s *scaler) now() time.Time {
	if s.capacityNow != nil {
		return s.capacityNow()
	}
	return time.Now()
}

// capacityAllowance keeps replacing workers below the last proven provider
// ceiling, suppresses launches above it until the backoff expires, then admits
// exactly one probe. A successful probe clears the ceiling for later messages.
func (s *scaler) capacityAllowance(current, target int) (needed int, probing bool) {
	s.capacityMu.Lock()
	defer s.capacityMu.Unlock()
	requested := max(0, target-current)
	if requested == 0 || s.capacityReason == "" {
		return requested, false
	}
	if current < s.capacityEffective {
		return min(requested, s.capacityEffective-current), false
	}
	if s.now().Before(s.capacityRetryAt) {
		return 0, false
	}
	return min(1, requested), true
}

// recordLaunchGate closes the launch gate after a capacity ceiling or a
// launch rejection: the current fleet size becomes the proven ceiling, and
// the next probe waits for a backoff that doubles up to fifteen minutes. A
// gate of the other kind is released first so status never reports both.
func (s *scaler) recordLaunchGate(kind, reason string, err error) {
	now := s.now()
	effective := s.state.count()
	s.capacityMu.Lock()
	if s.capacityBackoff == 0 {
		s.capacityBackoff = capacityInitialBackoff
	} else {
		s.capacityBackoff = min(s.capacityBackoff*2, capacityMaximumBackoff)
	}
	previousKind := s.capacityKind
	s.capacityKind = kind
	s.capacityEffective = effective
	s.capacityReason = reason
	s.capacityRetryAt = now.Add(s.capacityBackoff)
	retryAt := s.capacityRetryAt
	s.capacityMu.Unlock()
	if previousKind != "" && previousKind != kind {
		s.releaseGateStatus(previousKind)
	}
	switch kind {
	case gateLaunch:
		s.reporter.launchRejected(reason, retryAt)
		s.logger.Warn("provider rejected worker launch; keeping the listener alive",
			"provider_rejection", reason,
			"retry_at", retryAt,
			"error", err,
		)
	default:
		s.reporter.capacityRejected(effective, reason, retryAt)
		s.logger.Warn("provider capacity rejected worker launch; keeping the listener alive",
			"configured_capacity", s.maxWorkers,
			"effective_capacity", effective,
			"provider_rejection", reason,
			"retry_at", retryAt,
			"error", err,
		)
	}
}

func (s *scaler) releaseGateStatus(kind string) {
	switch kind {
	case gateLaunch:
		s.reporter.launchRecovered()
		s.logger.Info("provider accepted a worker launch again")
	default:
		s.reporter.capacityRecovered()
		s.logger.Info("provider capacity probe succeeded", "configured_capacity", s.maxWorkers)
	}
}

func (s *scaler) clearLaunchGate() {
	s.capacityMu.Lock()
	hadRejection := s.capacityReason != ""
	kind := s.capacityKind
	s.capacityKind = ""
	s.capacityEffective = 0
	s.capacityReason = ""
	s.capacityRetryAt = time.Time{}
	s.capacityBackoff = 0
	s.capacityMu.Unlock()
	if hadRejection {
		s.releaseGateStatus(kind)
	}
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
	localByWorker := make(map[string]string, len(local))
	for name, record := range local {
		localByWorker[record.Worker.ID] = name
	}
	present := make(map[string]struct{}, len(workers))
	retired := make(map[string]struct{})
	for _, worker := range workers {
		if !worker.CreatedAt.IsZero() && time.Since(worker.CreatedAt) > s.maxLifetime {
			if err := s.retireWorker(ctx, worker, true, settleActualUsage, time.Time{}); err != nil {
				return fmt.Errorf("retire worker %s after maximum lifetime: %w", worker.RunnerName, err)
			}
			retired[worker.RunnerName] = struct{}{}
			s.logger.Warn("deleted worker after maximum lifetime", "runner", worker.RunnerName, "worker_id", worker.ID, "maximum_lifetime", s.maxLifetime)
			continue
		}
		if workerStopped(worker.State) {
			// A stopped worker cannot run its job any more and, on providers
			// that bill stopped machines, still costs money. Retire it now
			// instead of waiting for the maximum lifetime.
			if err := s.retireWorker(ctx, worker, true, settleActualUsage, time.Time{}); err != nil {
				return fmt.Errorf("retire stopped worker %s: %w", worker.RunnerName, err)
			}
			retired[worker.RunnerName] = struct{}{}
			s.logger.Warn("retired stopped worker", "runner", worker.RunnerName, "worker_id", worker.ID, "state", worker.State)
			continue
		}
		if record, known := local[worker.RunnerName]; known && s.danglingTimeout > 0 && record.Observed && !record.Busy &&
			!worker.CreatedAt.IsZero() && time.Since(worker.CreatedAt) > s.danglingTimeout {
			// This controller created the worker and never saw a JobStarted for
			// it. The assignment race lasts seconds; after the dangling timeout
			// the worker is either stuck or was never assigned, so release it.
			if err := s.retireWorker(ctx, worker, true, settleActualUsage, time.Time{}); err != nil {
				return fmt.Errorf("retire dangling worker %s: %w", worker.RunnerName, err)
			}
			retired[worker.RunnerName] = struct{}{}
			s.logger.Warn("released worker that never started a job", "runner", worker.RunnerName, "worker_id", worker.ID, "dangling_timeout", s.danglingTimeout)
			continue
		}
		present[worker.ID] = struct{}{}
		if name, ok := localByWorker[worker.ID]; ok {
			s.state.markPresent(name)
			continue
		}
		if err := s.budget.adopt(worker.LeaseID, worker.CreatedAt); err != nil {
			s.reporter.degraded("usage_budget_write_failed")
			return fmt.Errorf("adopt runner usage budget for %s: %w", worker.RunnerName, err)
		}
		s.state.adopt(worker)
		s.logger.Warn("adopted managed worker missing from local state", "runner", worker.RunnerName, "worker_id", worker.ID)
	}
	now := time.Now()
	s.state.pruneDeparted(now.Add(-departureMemory))
	for name, record := range local {
		if _, retiredNow := retired[name]; retiredNow {
			continue
		}
		if _, ok := present[record.Worker.ID]; ok {
			continue
		}
		age := time.Duration(0)
		if !record.Worker.CreatedAt.IsZero() {
			age = now.Sub(record.Worker.CreatedAt)
		}
		maxLifetimeExpired := s.maxLifetime > 0 && age > s.maxLifetime
		danglingExpired := s.danglingTimeout > 0 && record.Observed && !record.Busy && age > s.danglingTimeout
		if !maxLifetimeExpired && !danglingExpired {
			if record.MissingSince.IsZero() {
				if !s.state.markMissing(name, now) {
					continue
				}
				s.logger.Info("worker absent from provider inventory; waiting for confirmation", "runner", record.Worker.RunnerName, "worker_id", record.Worker.ID, "grace", inventoryAbsenceGrace)
				continue
			}
			if now.Sub(record.MissingSince) < inventoryAbsenceGrace {
				continue
			}
		}
		if err := s.retireWorker(ctx, record.Worker, false, settleActualUsage, time.Time{}); err != nil {
			return fmt.Errorf("retire disappeared worker %s: %w", name, err)
		}
		s.state.markDeparted(record, now)
		s.logDeparture(record, now)
	}
	s.reporter.orphans(0)
	s.reportState()
	s.reporter.budget(s.budget.snapshot(time.Now()))
	s.reporter.recovered()
	return nil
}

// unknownRunnerLevel picks the level for a message about a runner this
// process does not know. Within the predecessor window it is the previous
// controller's worker finishing its job, which is routine; later it is a
// genuinely unknown runner and worth a warning.
func (s *scaler) unknownRunnerLevel() slog.Level {
	if !s.startedAt.IsZero() && time.Since(s.startedAt) < predecessorWindow {
		return slog.LevelInfo
	}
	return slog.LevelWarn
}

func (s *scaler) HandleJobStarted(ctx context.Context, job *scaleset.JobStarted) error {
	return failClosed(s.handleJobStarted(ctx, job))
}

func (s *scaler) handleJobStarted(ctx context.Context, job *scaleset.JobStarted) error {
	s.reporter.githubActivity("job_started")
	if !s.state.markBusy(job.RunnerName) {
		s.logger.Log(ctx, s.unknownRunnerLevel(), "job started on worker not present in local state", "runner", job.RunnerName, "job_id", job.JobID)
		return nil
	}
	if record, ok := s.state.get(job.RunnerName); ok && !record.Worker.CreatedAt.IsZero() {
		s.reporter.latency(false, time.Since(record.Worker.CreatedAt), false)
	}
	s.reportState()
	s.logger.Info("job started", "runner", job.RunnerName, "job_id", job.JobID, "repository", job.RepositoryName)
	return nil
}

// retirementGrace is how long a journaled retirement may stay pending before
// the fleet reports degraded. GitHub keeps a runner registered, and refuses
// its removal as "job still running", for several minutes after the job
// completed and the worker exited; that deferral is routine, not a fault.
const retirementGrace = 15 * time.Minute

// reportRetirements publishes the journal size and how many entries have
// outlived the grace.
func (s *scaler) reportRetirements() {
	s.reporter.retirements(s.retirements.count(), s.retirements.overdue(time.Now(), retirementGrace))
}

// departureMemory bounds how long a departed worker is remembered for the
// completion message that GitHub sends after the job finished. Completion
// follows departure within minutes; anything older is a genuinely unknown
// runner again.
const departureMemory = time.Hour

// inventoryAbsenceGrace prevents an eventually consistent provider inventory
// snapshot from erasing a worker while GitHub's JobStarted message is already
// in flight. Explicit stopped states, maximum-lifetime expiry, and job
// completion still retire immediately; only an unexplained absence must remain
// continuous for this window before it is considered a departure.
const inventoryAbsenceGrace = 30 * time.Second

// logDeparture explains why a worker left inventory before its completion
// message. A busy worker finishing its job and self-destroying is the normal
// order of events on providers that destroy the Machine on exit, and an idle
// worker past the idle timeout released itself by design; only a worker that
// vanished before it could start a job is worth a warning.
func (s *scaler) logDeparture(record workerRecord, now time.Time) {
	age := time.Duration(0)
	if !record.Worker.CreatedAt.IsZero() {
		age = now.Sub(record.Worker.CreatedAt).Round(time.Second)
	}
	switch {
	case record.Busy:
		s.logger.Info("busy worker left inventory; awaiting its job completion", "runner", record.Worker.RunnerName, "worker_id", record.Worker.ID, "age", age)
	case !record.Observed:
		s.logger.Info("adopted worker left inventory", "runner", record.Worker.RunnerName, "worker_id", record.Worker.ID, "age", age)
	case s.idleTimeout > 0 && age >= s.idleTimeout:
		s.logger.Info("idle worker released itself", "runner", record.Worker.RunnerName, "worker_id", record.Worker.ID, "age", age, "idle_timeout", s.idleTimeout)
	default:
		s.logger.Warn("worker disappeared before starting a job", "runner", record.Worker.RunnerName, "worker_id", record.Worker.ID, "age", age)
	}
}

func (s *scaler) HandleJobCompleted(ctx context.Context, job *scaleset.JobCompleted) error {
	return failClosed(s.handleJobCompleted(ctx, job))
}

func (s *scaler) handleJobCompleted(ctx context.Context, job *scaleset.JobCompleted) error {
	s.reporter.githubActivity("job_completed")
	record, ok := s.state.get(job.RunnerName)
	if !ok {
		if departed, known := s.state.takeDeparted(job.RunnerName); known {
			s.logger.Info("job completed after its worker left inventory", "runner", job.RunnerName, "job_id", job.JobID, "result", job.Result, "worker_id", departed.Worker.ID)
			return nil
		}
		s.logger.Log(ctx, s.unknownRunnerLevel(), "job completed on worker not present in local state", "runner", job.RunnerName, "job_id", job.JobID, "result", job.Result)
		return nil
	}
	if err := s.retireWorker(ctx, record.Worker, true, settleActualUsage, workerStoppedAt(job.FinishTime)); err != nil {
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
		RequestedAt: time.Now().UTC(),
	}
	if err := s.retirements.put(retirement); err != nil {
		s.reporter.degraded("runner_retirement_state_failed")
		budgetErr := s.budget.forfeit(leaseID, time.Now())
		return false, errors.Join(fmt.Errorf("journal runner registration intent: %w", err), budgetErr)
	}
	s.reportRetirements()
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
		IdleTimeout: s.idleTimeout,
	})
	s.reporter.starting(-1)
	s.reporter.latency(true, time.Since(launchStarted), err != nil)
	if err != nil {
		if provider.IsCapacity(err) {
			if cleanupErr := s.cleanupRejectedLaunch(context.WithoutCancel(ctx), retirement); cleanupErr != nil {
				return false, fmt.Errorf("clean up capacity-rejected launch: %w", cleanupErr)
			}
			s.reporter.budget(s.budget.snapshot(time.Now()))
			return false, err
		}
		classified := classifyLaunchFailure(err)
		var rejected *launchRejectedError
		if !errors.As(classified, &rejected) {
			s.reporter.degraded("provider_launch_failed")
		}
		// A permanent rejection is proven only once inventory shows no worker
		// carrying the lease; the cleanup below checks exactly that.
		cleanupErr := s.cleanupAmbiguousLaunch(context.WithoutCancel(ctx), retirement, err)
		if cleanupErr != nil {
			return false, cleanupErr
		}
		s.reporter.budget(s.budget.snapshot(time.Now()))
		return false, classified
	}
	if worker.RunnerID == 0 {
		worker.RunnerID = retirement.RunnerID
	}
	if worker.RunnerScaleSetID == 0 {
		worker.RunnerScaleSetID = retirement.RunnerScaleSetID
	}
	if err := s.budget.adopt(leaseID, worker.CreatedAt); err != nil {
		s.reporter.degraded("usage_budget_write_failed")
		cleanupErr := s.retireWorker(context.WithoutCancel(ctx), worker, true, forfeitReservation, time.Time{})
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
	s.reportRetirements()
	s.reportState()
	s.reporter.budget(s.budget.snapshot(time.Now()))
	s.reporter.recovered()
	s.logger.Info("worker created", "runner", name, "worker_id", worker.ID)
	return true, nil
}

// cleanupRejectedLaunch handles a provider response that definitively proves
// no worker was created. It removes the unused JIT registration and refunds
// the reservation without an unnecessary provider inventory call.
func (s *scaler) cleanupRejectedLaunch(ctx context.Context, retirement retirementEntry) error {
	if err := s.removeRunnerRegistration(ctx, retirement); err != nil {
		s.reporter.degraded("github_runner_cleanup_failed")
		return err
	}
	if err := s.budget.release(retirement.LeaseID); err != nil {
		s.reporter.degraded("usage_budget_write_failed")
		return err
	}
	if err := s.retirements.remove(retirement.RunnerName); err != nil {
		s.reporter.degraded("runner_retirement_state_failed")
		return err
	}
	s.reportRetirements()
	return nil
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
		s.reportRetirements()
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
		if err := s.retireWorker(ctx, partial.Worker, true, forfeitReservation, time.Time{}); err != nil {
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
		if err := s.retireWorker(ctx, worker, true, forfeitReservation, time.Time{}); err != nil {
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
			if errors.Is(err, errRetirementDeferred) {
				continue
			}
			if provider.IsTransient(err) {
				// The journal keeps the proof; the next reconciliation retries.
				s.logger.Warn("provider unavailable while finishing retirement; keeping it pending", "runner", retirement.RunnerName, "error", err)
				continue
			}
			return fmt.Errorf("finish pending retirement for %s: %w", retirement.RunnerName, err)
		}
	}
	return nil
}

func (s *scaler) retireWorker(ctx context.Context, worker provider.Worker, providerPresent bool, disposition budgetDisposition, stoppedAt time.Time) error {
	retirement := retirementEntry{
		RunnerName: worker.RunnerName, RunnerID: worker.RunnerID, RunnerScaleSetID: worker.RunnerScaleSetID,
		LeaseID: worker.LeaseID, BudgetDisposition: disposition, StoppedAt: stoppedAt.UTC(),
		RequestedAt: time.Now().UTC(),
	}
	if stoppedAt.IsZero() {
		retirement.StoppedAt = time.Time{}
	}
	if retirement.RunnerScaleSetID == 0 {
		retirement.RunnerScaleSetID = s.scaleSetID
	}
	if err := s.retirements.put(retirement); err != nil {
		s.reporter.degraded("runner_retirement_state_failed")
		return err
	}
	s.reportRetirements()
	err := s.finishRetirement(ctx, worker, providerPresent, retirement)
	if errors.Is(err, errRetirementDeferred) {
		return nil
	}
	if provider.IsTransient(err) {
		// The retirement intent is journaled and the worker record is kept, so
		// reconciliation finishes the cleanup once the provider answers again.
		s.logger.Warn("provider unavailable during retirement; keeping it pending", "runner", worker.RunnerName, "worker_id", worker.ID, "error", err)
		return nil
	}
	return err
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
		if errors.Is(err, scaleset.JobStillRunningError) {
			s.logger.Warn(
				"deferred GitHub runner cleanup while job is still running",
				"runner", retirement.RunnerName,
				"runner_id", retirement.RunnerID,
			)
			return errors.Join(errRetirementDeferred, err)
		}
		return err
	}
	var budgetErr error
	if retirement.BudgetDisposition == forfeitReservation {
		budgetErr = s.budget.forfeit(retirement.LeaseID, time.Now())
	} else {
		budgetErr = s.budget.settleAt(retirement.LeaseID, retirement.StoppedAt, time.Now())
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
	s.reportRetirements()
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

// workerStopped reports provider states in which a worker can no longer run
// a job. "stopped" and "off" come from Fly and Hetzner respectively.
func workerStopped(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "stopped", "off", "failed", "destroyed", "suspended":
		return true
	default:
		return false
	}
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
