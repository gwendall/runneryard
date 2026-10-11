package pool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/gwendall/runneryard/provider"
)

// Tier is one placement choice: a pool, or the ephemeral provider.
type Tier struct {
	Name    string
	Compute provider.Compute
	// Drain keeps the tier's workers inventoried and destroyed but starts no
	// new ones there, so a tier taken out of the placement order never leaks
	// the workers it still runs.
	Drain bool
}

// Placement tries its tiers in order and moves to the next one only when a
// tier is full, so a job never waits for a pool host while another tier can
// start it now. Worker IDs carry the tier name ("tier:id"), which routes a
// destroy back to the tier that owns the worker without any stored state.
type Placement struct {
	tiers  []Tier
	byName map[string]provider.Compute
}

// NewPlacement validates the tiers.
func NewPlacement(tiers ...Tier) (*Placement, error) {
	if len(tiers) == 0 {
		return nil, errors.New("placement requires at least one tier")
	}
	byName := make(map[string]provider.Compute, len(tiers))
	launching := 0
	for _, tier := range tiers {
		if !tier.Drain {
			launching++
		}
		if !ValidName(tier.Name) || tier.Compute == nil {
			return nil, fmt.Errorf("invalid placement tier %q", tier.Name)
		}
		if _, duplicate := byName[tier.Name]; duplicate {
			return nil, fmt.Errorf("placement tier %q appears twice", tier.Name)
		}
		byName[tier.Name] = tier.Compute
	}
	if launching == 0 {
		return nil, errors.New("placement requires at least one tier that starts workers")
	}
	return &Placement{tiers: append([]Tier(nil), tiers...), byName: byName}, nil
}

// Launch starts the lease on the first tier with room. A partial launch is
// returned at once: the core must clean that worker up before anything else.
func (p *Placement) Launch(ctx context.Context, lease provider.Lease) (provider.Worker, error) {
	launching := make([]Tier, 0, len(p.tiers))
	for _, tier := range p.tiers {
		if !tier.Drain {
			launching = append(launching, tier)
		}
	}
	var lastErr error
	for i, tier := range launching {
		worker, err := tier.Compute.Launch(ctx, lease)
		if err == nil {
			worker.ID = tier.Name + ":" + worker.ID
			return worker, nil
		}
		var partial *provider.PartialLaunchError
		if errors.As(err, &partial) {
			if partial.Worker.ID != "" {
				partial.Worker.ID = tier.Name + ":" + partial.Worker.ID
			}
			return provider.Worker{}, err
		}
		lastErr = err
		if provider.IsCapacity(err) && i < len(launching)-1 {
			continue
		}
		return provider.Worker{}, err
	}
	return provider.Worker{}, lastErr
}

// Inventory joins every tier's workers. One failing tier fails the whole
// inventory: a partial list would make the other tier's running workers look
// departed.
func (p *Placement) Inventory(ctx context.Context) ([]provider.Worker, error) {
	results := make([][]provider.Worker, len(p.tiers))
	failures := make([]error, len(p.tiers))
	var wait sync.WaitGroup
	for i, tier := range p.tiers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results[i], failures[i] = tier.Compute.Inventory(ctx)
		}()
	}
	wait.Wait()
	workers := make([]provider.Worker, 0)
	for i, tier := range p.tiers {
		if failures[i] != nil {
			err := fmt.Errorf("placement tier %s: %w", tier.Name, failures[i])
			if provider.IsTransient(failures[i]) {
				return nil, &provider.TransientError{Err: err}
			}
			return nil, err
		}
		for _, worker := range results[i] {
			worker.ID = tier.Name + ":" + worker.ID
			workers = append(workers, worker)
		}
	}
	return workers, nil
}

// Destroy routes the worker to the tier named in its ID.
func (p *Placement) Destroy(ctx context.Context, workerID string) error {
	if workerID == "" {
		return nil
	}
	name, id, ok := strings.Cut(workerID, ":")
	compute, known := p.byName[name]
	if !ok || !known || id == "" {
		return fmt.Errorf("worker id %q names no placement tier", workerID)
	}
	return compute.Destroy(ctx, id)
}

// Run runs every tier that maintains itself, such as a pool, until ctx ends.
func (p *Placement) Run(ctx context.Context) error {
	var wait sync.WaitGroup
	for _, tier := range p.tiers {
		runner, ok := tier.Compute.(provider.Maintainer)
		if !ok {
			continue
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			_ = runner.Run(ctx)
		}()
	}
	wait.Wait()
	return nil
}

// PlacementStatus is the placement's share of the fleet status.
type PlacementStatus struct {
	// Tiers is the launch order; Draining lists tiers that start nothing.
	Tiers    []string `json:"tiers"`
	Draining []string `json:"draining,omitempty"`
	Pools    []Status `json:"pools"`
}

// ComputeStatus reports the tier order and every pool tier.
func (p *Placement) ComputeStatus() any {
	status := PlacementStatus{Tiers: make([]string, 0, len(p.tiers)), Pools: make([]Status, 0)}
	for _, tier := range p.tiers {
		if tier.Drain {
			status.Draining = append(status.Draining, tier.Name)
		} else {
			status.Tiers = append(status.Tiers, tier.Name)
		}
		if pool, ok := tier.Compute.(interface{ Status() Status }); ok {
			status.Pools = append(status.Pools, pool.Status())
		}
	}
	return status
}

var (
	_ provider.Compute        = (*Placement)(nil)
	_ provider.Maintainer     = (*Placement)(nil)
	_ provider.StatusReporter = (*Placement)(nil)
	_ provider.Maintainer     = (*Pool)(nil)
)
