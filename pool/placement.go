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
	// spare lists the pools that take speculative runners, in order; room
	// receives a token when one of them gets a ready host.
	spare []Tier
	room  chan struct{}
}

// spareTier is a pool's side of a speculative launch: free slots used as they
// are, without creating or starting a host.
type spareTier interface {
	freeSlots() int
	launchSpare(context.Context, provider.Lease) (provider.Worker, error)
	speculative() bool
	onRoomReady(func())
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
	placement := &Placement{tiers: append([]Tier(nil), tiers...), byName: byName, room: make(chan struct{}, 1)}
	// A pool takes speculative runners for jobs waiting on a later tier that
	// boots a machine per worker. A pool placed after such a tier is the
	// operator's overflow for it, and keeps only that role.
	bootsBefore := false
	for _, tier := range placement.tiers {
		if tier.Drain {
			continue
		}
		pool, ok := tier.Compute.(spareTier)
		if !ok {
			bootsBefore = true
			continue
		}
		if pool.speculative() && !bootsBefore {
			placement.spare = append(placement.spare, tier)
			pool.onRoomReady(placement.signalRoom)
		}
	}
	return placement, nil
}

func (p *Placement) signalRoom() {
	select {
	case p.room <- struct{}{}:
	default:
	}
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

// SpareRoom counts the free slots of the speculative pools' ready hosts.
func (p *Placement) SpareRoom() int {
	room := 0
	for _, tier := range p.spare {
		room += tier.Compute.(spareTier).freeSlots()
	}
	return room
}

// LaunchSpare starts the lease in a free slot of the first speculative pool
// with one. It never reaches a tier that boots a machine, and a full pool asks
// for no new host: the job it is for already has a worker on its way.
func (p *Placement) LaunchSpare(ctx context.Context, lease provider.Lease) (provider.Worker, error) {
	var lastErr error = &provider.CapacityError{Reason: CapacityReasonFull, Err: fmt.Errorf("no speculative pool: %w", ErrFull)}
	for _, tier := range p.spare {
		worker, err := tier.Compute.(spareTier).launchSpare(ctx, lease)
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
		if !provider.IsCapacity(err) {
			return provider.Worker{}, err
		}
		lastErr = err
	}
	return provider.Worker{}, lastErr
}

// Fast reports whether the worker runs in a pool slot, whose runner comes
// online seconds after its launch, draining or not.
func (p *Placement) Fast(workerID string) bool {
	name, _, ok := strings.Cut(workerID, ":")
	if !ok {
		return false
	}
	_, pool := p.byName[name].(spareTier)
	return pool
}

// RoomReady receives a token when a speculative pool gets a ready host.
func (p *Placement) RoomReady() <-chan struct{} { return p.room }

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
	// Speculative lists the pools that take speculative runners.
	Speculative []string `json:"speculative,omitempty"`
	Pools       []Status `json:"pools"`
}

// ComputeStatus reports the tier order and every pool tier. A pool's
// ephemeral equivalent uses the first tier that knows its worker price.
func (p *Placement) ComputeStatus() any {
	status := PlacementStatus{Tiers: make([]string, 0, len(p.tiers)), Pools: make([]Status, 0)}
	pricedTier, price, currency, priced := p.workerPrice()
	for _, tier := range p.tiers {
		if tier.Drain {
			status.Draining = append(status.Draining, tier.Name)
		} else {
			status.Tiers = append(status.Tiers, tier.Name)
		}
		if pool, ok := tier.Compute.(interface{ Status() Status }); ok {
			poolStatus := pool.Status()
			if priced {
				poolStatus.EphemeralEquivalent = ephemeralEquivalent(poolStatus.BusySlotSeconds, pricedTier, price, currency)
			}
			status.Pools = append(status.Pools, poolStatus)
		}
	}
	for _, tier := range p.spare {
		status.Speculative = append(status.Speculative, tier.Name)
	}
	return status
}

// workerPrice returns the first tier that knows its worker price, and the
// price.
func (p *Placement) workerPrice() (string, float64, string, bool) {
	for _, tier := range p.tiers {
		if pricer, ok := tier.Compute.(provider.Pricer); ok {
			if price, currency, known := pricer.WorkerHourlyPrice(); known {
				return tier.Name, price, currency, true
			}
		}
	}
	return "", 0, "", false
}

// Conditions reports the conditions of every tier that has some, in tier
// order.
func (p *Placement) Conditions() []provider.Condition {
	conditions := make([]provider.Condition, 0)
	for _, tier := range p.tiers {
		if reporter, ok := tier.Compute.(provider.ConditionReporter); ok {
			conditions = append(conditions, reporter.Conditions()...)
		}
	}
	return conditions
}

var (
	_ provider.Compute           = (*Placement)(nil)
	_ provider.Maintainer        = (*Placement)(nil)
	_ provider.StatusReporter    = (*Placement)(nil)
	_ provider.ConditionReporter = (*Placement)(nil)
	_ provider.Speculator        = (*Placement)(nil)
	_ spareTier                  = (*Pool)(nil)
	_ provider.Maintainer        = (*Pool)(nil)
	_ provider.ConditionReporter = (*Pool)(nil)
)
