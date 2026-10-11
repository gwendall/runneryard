package pool

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/gwendall/runneryard/provider"
)

type fakeCompute struct {
	mu           sync.Mutex
	launchErr    error
	inventory    []provider.Worker
	inventoryErr error
	launched     []string
	destroyed    []string
}

func (c *fakeCompute) Launch(_ context.Context, lease provider.Lease) (provider.Worker, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.launched = append(c.launched, lease.ID)
	if c.launchErr != nil {
		return provider.Worker{}, c.launchErr
	}
	return provider.Worker{ID: "w-" + lease.ID, LeaseID: lease.ID, RunnerName: lease.RunnerName}, nil
}

func (c *fakeCompute) Inventory(context.Context) ([]provider.Worker, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]provider.Worker(nil), c.inventory...), c.inventoryErr
}

func (c *fakeCompute) Destroy(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.destroyed = append(c.destroyed, id)
	return nil
}

type maintainedCompute struct {
	fakeCompute
	started chan struct{}
}

func (c *maintainedCompute) Run(ctx context.Context) error {
	close(c.started)
	<-ctx.Done()
	return ctx.Err()
}

func (c *maintainedCompute) Status() Status { return Status{Name: "warm", Billing: "owned"} }

func full() error {
	return &provider.CapacityError{Reason: CapacityReasonFull, Err: errors.New("no free slot")}
}

func TestNewPlacementValidatesTiers(t *testing.T) {
	compute := &fakeCompute{}
	cases := map[string][]Tier{
		"none":      nil,
		"bad name":  {{Name: "Warm", Compute: compute}},
		"no engine": {{Name: "warm"}},
		"duplicate": {{Name: "warm", Compute: compute}, {Name: "warm", Compute: compute}},
		"all drain": {{Name: "warm", Compute: compute, Drain: true}},
	}
	for name, tiers := range cases {
		if _, err := NewPlacement(tiers...); err == nil {
			t.Errorf("%s: NewPlacement accepted invalid tiers", name)
		}
	}
}

func TestPlacementOverflowsOnlyWhenATierIsFull(t *testing.T) {
	warm, ephemeral := &fakeCompute{launchErr: full()}, &fakeCompute{}
	placement, err := NewPlacement(Tier{Name: "warm", Compute: warm}, Tier{Name: "ephemeral", Compute: ephemeral})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := placement.Launch(context.Background(), provider.Lease{ID: "l1", RunnerName: "r1"})
	if err != nil || worker.ID != "ephemeral:w-l1" || worker.LeaseID != "l1" {
		t.Fatalf("overflow launch = %+v, %v", worker, err)
	}

	warm.launchErr = errors.New("ssh refused")
	if _, err := placement.Launch(context.Background(), provider.Lease{ID: "l2"}); err == nil || len(ephemeral.launched) != 1 {
		t.Fatalf("a non-capacity failure overflowed: err=%v ephemeral=%v", err, ephemeral.launched)
	}

	warm.launchErr = nil
	worker, err = placement.Launch(context.Background(), provider.Lease{ID: "l3"})
	if err != nil || worker.ID != "warm:w-l3" {
		t.Fatalf("launch with room = %+v, %v", worker, err)
	}
}

func TestPlacementReturnsTheLastTiersCapacityError(t *testing.T) {
	warm := &fakeCompute{launchErr: full()}
	placement, err := NewPlacement(Tier{Name: "warm", Compute: warm})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := placement.Launch(context.Background(), provider.Lease{ID: "l1"}); !provider.IsCapacity(err) {
		t.Fatalf("launch on a full only tier = %v, want the capacity error so the core backs off", err)
	}
}

func TestPlacementReturnsAPartialLaunchWithTheTierPrefix(t *testing.T) {
	partial := &provider.PartialLaunchError{Worker: provider.Worker{ID: "a/0/l1", LeaseID: "l1"}, Err: errors.New("no reply")}
	warm, ephemeral := &fakeCompute{launchErr: partial}, &fakeCompute{}
	placement, err := NewPlacement(Tier{Name: "warm", Compute: warm}, Tier{Name: "ephemeral", Compute: ephemeral})
	if err != nil {
		t.Fatal(err)
	}
	_, err = placement.Launch(context.Background(), provider.Lease{ID: "l1"})
	var got *provider.PartialLaunchError
	if !errors.As(err, &got) || got.Worker.ID != "warm:a/0/l1" {
		t.Fatalf("partial launch = %v, want the worker ID prefixed with its tier", err)
	}
	if len(ephemeral.launched) != 0 {
		t.Fatal("a partial launch must not start the lease on another tier")
	}
	if err := placement.Destroy(context.Background(), got.Worker.ID); err != nil || len(warm.destroyed) != 1 || warm.destroyed[0] != "a/0/l1" {
		t.Fatalf("destroy of the partial worker: err=%v destroyed=%v", err, warm.destroyed)
	}
}

func TestDrainingTierIsInventoriedAndDestroyedButNeverLaunched(t *testing.T) {
	old := &fakeCompute{inventory: []provider.Worker{{ID: "w-old", LeaseID: "old"}}}
	warm := &fakeCompute{inventory: []provider.Worker{{ID: "w-new", LeaseID: "new"}}}
	placement, err := NewPlacement(Tier{Name: "warm", Compute: warm}, Tier{Name: "ephemeral", Compute: old, Drain: true})
	if err != nil {
		t.Fatal(err)
	}
	warm.launchErr = full()
	if _, err := placement.Launch(context.Background(), provider.Lease{ID: "l1"}); !provider.IsCapacity(err) || len(old.launched) != 0 {
		t.Fatalf("launch overflowed into a draining tier: err=%v launched=%v", err, old.launched)
	}
	workers, err := placement.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, worker := range workers {
		ids[worker.ID] = true
	}
	if len(workers) != 2 || !ids["warm:w-new"] || !ids["ephemeral:w-old"] {
		t.Fatalf("inventory = %+v, want both tiers with their prefixes", workers)
	}
	if err := placement.Destroy(context.Background(), "ephemeral:w-old"); err != nil || len(old.destroyed) != 1 {
		t.Fatalf("destroy on a draining tier: err=%v destroyed=%v", err, old.destroyed)
	}
}

func TestPlacementInventoryFailsWhenAnyTierFails(t *testing.T) {
	warm := &fakeCompute{inventory: []provider.Worker{{ID: "w1"}}}
	ephemeral := &fakeCompute{inventoryErr: &provider.TransientError{Err: errors.New("api timeout")}}
	placement, err := NewPlacement(Tier{Name: "warm", Compute: warm}, Tier{Name: "ephemeral", Compute: ephemeral})
	if err != nil {
		t.Fatal(err)
	}
	workers, err := placement.Inventory(context.Background())
	if err == nil || workers != nil || !provider.IsTransient(err) {
		t.Fatalf("inventory = %+v, %v; want a transient failure and no partial list", workers, err)
	}
	ephemeral.inventoryErr = errors.New("unauthorized")
	if _, err := placement.Inventory(context.Background()); err == nil || provider.IsTransient(err) {
		t.Fatalf("inventory = %v, want a permanent failure kept permanent", err)
	}
}

func TestPlacementDestroyRoutesByPrefix(t *testing.T) {
	warm, ephemeral := &fakeCompute{}, &fakeCompute{}
	placement, err := NewPlacement(Tier{Name: "warm", Compute: warm}, Tier{Name: "ephemeral", Compute: ephemeral})
	if err != nil {
		t.Fatal(err)
	}
	if err := placement.Destroy(context.Background(), "ephemeral:fly-123"); err != nil || len(ephemeral.destroyed) != 1 || ephemeral.destroyed[0] != "fly-123" {
		t.Fatalf("destroy routed wrong: err=%v destroyed=%v", err, ephemeral.destroyed)
	}
	if err := placement.Destroy(context.Background(), ""); err != nil {
		t.Fatalf("destroy of an empty ID = %v, want nil", err)
	}
	for _, bad := range []string{"fly-123", "cold:fly-123", "warm:"} {
		if err := placement.Destroy(context.Background(), bad); err == nil {
			t.Errorf("destroy(%q) accepted", bad)
		}
	}
	if len(warm.destroyed) != 0 {
		t.Fatalf("warm destroyed %v", warm.destroyed)
	}
}

func TestPlacementRunsMaintainersAndReportsStatus(t *testing.T) {
	warm := &maintainedCompute{started: make(chan struct{})}
	old := &fakeCompute{}
	placement, err := NewPlacement(Tier{Name: "warm", Compute: warm}, Tier{Name: "ephemeral", Compute: &fakeCompute{}}, Tier{Name: "cold", Compute: old, Drain: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- placement.Run(ctx) }()
	<-warm.started
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}

	encoded, err := json.Marshal(placement.ComputeStatus())
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Tiers    []string `json:"tiers"`
		Draining []string `json:"draining"`
		Pools    []Status `json:"pools"`
	}
	if err := json.Unmarshal(encoded, &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Tiers) != 2 || status.Tiers[0] != "warm" || status.Tiers[1] != "ephemeral" || len(status.Draining) != 1 || status.Draining[0] != "cold" {
		t.Fatalf("status tiers = %s", encoded)
	}
	if len(status.Pools) != 1 || status.Pools[0].Name != "warm" {
		t.Fatalf("status pools = %s", encoded)
	}
}
