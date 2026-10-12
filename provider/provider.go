// Package provider defines the compute seam used by the runneryard core.
package provider

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Lease is the provider-neutral instruction for one ephemeral GitHub runner.
// JITConfig is the only credential that may be delivered to the worker.
type Lease struct {
	ID               string
	RunnerName       string
	RunnerID         int64
	RunnerScaleSetID int
	JITConfig        string
	Deadline         time.Time
	// IdleTimeout releases a worker that received no job; zero disables it.
	IdleTimeout time.Duration
}

// Worker is the provider-neutral inventory record used for reconciliation.
type Worker struct {
	ID               string
	LeaseID          string
	RunnerName       string
	RunnerID         int64
	RunnerScaleSetID int
	State            string
	CreatedAt        time.Time
}

// Compute launches, inventories, and destroys workers owned by one controller.
// Inventory must exclude foreign workers. Destroy must be idempotent.
type Compute interface {
	Launch(context.Context, Lease) (Worker, error)
	Inventory(context.Context) ([]Worker, error)
	Destroy(context.Context, string) error
}

// Maintainer is implemented by compute that keeps capacity on its own, such as
// a pool of shared hosts. The controller runs it for its whole lifetime.
type Maintainer interface {
	Run(context.Context) error
}

// StatusReporter is implemented by compute with state of its own to publish.
// The controller writes the value under "compute" in the fleet status.
type StatusReporter interface {
	ComputeStatus() any
}

// Pricer is implemented by compute that bills each worker per second of
// running time at a known hourly price. Status reports use it to price a
// pool's busy slot time as ephemeral workers; a provider that bills by any
// other unit does not implement it.
type Pricer interface {
	// WorkerHourlyPrice returns the price of one worker hour; ok is false
	// when the price is not known.
	WorkerHourlyPrice() (price float64, currency string, ok bool)
}

// Speculator is implemented by compute that holds spare room where a worker
// starts in seconds, such as the free slots of a pool's ready hosts, next to
// tiers that boot a machine per worker. The core starts a second runner there
// for a job still waiting on a booting worker: the first runner online takes
// the job, and the other one takes the next job or releases itself after its
// idle timeout. The core never destroys either one for it.
type Speculator interface {
	// SpareRoom returns how many workers LaunchSpare can start now.
	SpareRoom() int
	// LaunchSpare starts the lease in spare room only. Without room it returns
	// a CapacityError, creates nothing, and asks for no new machine.
	LaunchSpare(context.Context, Lease) (Worker, error)
	// Fast reports whether the worker runs where a runner comes online in
	// seconds, so a job waiting for it does not wait on a machine's boot.
	Fast(workerID string) bool
	// RoomReady receives a token when spare room appears on its own, such as
	// a pool host becoming ready, rather than through the core's own calls.
	RoomReady() <-chan struct{}
}

// PartialLaunchError reports that a provider created a worker but could not
// confirm the complete response. The core immediately schedules its cleanup.
type PartialLaunchError struct {
	Worker Worker
	Err    error
}

func (e *PartialLaunchError) Error() string {
	return fmt.Sprintf("worker %s may have been created: %v", e.Worker.ID, e.Err)
}

func (e *PartialLaunchError) Unwrap() error { return e.Err }

// TransientError reports a provider failure that the adapter already retried
// within its policy and that may succeed later: throttling, a provider-side
// outage, or a transport failure. The core keeps its GitHub session open,
// reports degraded status, and retries on the next reconciliation instead of
// exiting. Identity, authorization, and validation failures must never be
// wrapped as transient.
type TransientError struct {
	Err error
}

func (e *TransientError) Error() string {
	return fmt.Sprintf("transient provider failure: %v", e.Err)
}

func (e *TransientError) Unwrap() error { return e.Err }

// IsTransient reports whether err, or any error it wraps or joins, is a
// TransientError.
func IsTransient(err error) bool {
	var transient *TransientError
	return errors.As(err, &transient)
}

// CapacityError reports a provider-enforced compute ceiling. It is permanent
// for an individual launch attempt, so adapters must not retry it, but it is
// not a controller-fatal identity or validation failure. The core keeps the
// GitHub session alive and probes again with bounded backoff.
type CapacityError struct {
	// Reason is a stable, non-secret provider rejection code suitable for
	// aggregate status and alerts (for example, "fly_machine_limit").
	Reason string
	// RetryAfter, when positive, says room is on its way, such as a pool host
	// being created or started: the core probes again after it instead of
	// after its growing backoff.
	RetryAfter time.Duration
	Err        error
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("provider capacity exhausted (%s): %v", e.Reason, e.Err)
}

func (e *CapacityError) Unwrap() error { return e.Err }

// IsCapacity reports whether err, or any error it wraps or joins, is a
// CapacityError.
func IsCapacity(err error) bool {
	var capacity *CapacityError
	return errors.As(err, &capacity)
}

// CapacityRetryAfter returns the RetryAfter hint carried by a CapacityError,
// or zero.
func CapacityRetryAfter(err error) time.Duration {
	var capacity *CapacityError
	if errors.As(err, &capacity) && capacity.RetryAfter > 0 {
		return capacity.RetryAfter
	}
	return 0
}

// CapacityReason returns the stable reason carried by a CapacityError.
func CapacityReason(err error) string {
	var capacity *CapacityError
	if errors.As(err, &capacity) && validCapacityReason(capacity.Reason) {
		return capacity.Reason
	}
	return "provider_capacity_limit"
}

func validCapacityReason(reason string) bool {
	if len(reason) < 1 || len(reason) > 64 || reason[0] < 'a' || reason[0] > 'z' {
		return false
	}
	for _, character := range reason[1:] {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}
