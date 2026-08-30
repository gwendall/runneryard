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
