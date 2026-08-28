// Package provider defines the compute seam used by the runneryard core.
package provider

import (
	"context"
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
