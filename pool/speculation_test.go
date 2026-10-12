package pool

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gwendall/runneryard/provider"
)

var growingFacts = Facts{Billing: BillingPerSecond, HourlyPrice: 0.1, Currency: "EUR", CanCreate: true}

func TestSpareLaunchTakesAFreeSlotAndNeverAsksForAHost(t *testing.T) {
	h := newHarness(t, growingFacts, 0, 2)
	h.readyHost("a", 1, h.clock.Now().Add(-time.Minute))
	h.step(t, time.Second)
	if free := h.pool.freeSlots(); free != 1 {
		t.Fatalf("free slots = %d, want 1", free)
	}

	worker, err := h.pool.launchSpare(context.Background(), lease("l1"))
	if err != nil || worker.ID != "a/0/l1" {
		t.Fatalf("spare launch = %+v, %v", worker, err)
	}
	if free := h.pool.freeSlots(); free != 0 {
		t.Fatalf("free slots after the spare launch = %d, want 0", free)
	}

	_, err = h.pool.launchSpare(context.Background(), lease("l2"))
	var capacity *provider.CapacityError
	if !errors.As(err, &capacity) || capacity.Reason != CapacityReasonFull || capacity.RetryAfter != 0 {
		t.Fatalf("spare launch on a full pool = %v, want pool_full without a retry hint", err)
	}
	select {
	case <-h.pool.wake:
		t.Fatal("a refused spare launch must not wake the pool to grow")
	default:
	}
	h.step(t, time.Second)
	status := h.pool.Status()
	if h.source.created != 0 || status.Overflowed != 0 {
		t.Fatalf("a refused spare launch created %d hosts and counted %d overflows, want none", h.source.created, status.Overflowed)
	}
	if status.Launched != 1 || status.Speculated != 1 {
		t.Fatalf("launched %d, speculated %d, want 1 and 1", status.Launched, status.Speculated)
	}

	// A regular launch on the same full pool still grows it.
	if _, err := h.pool.Launch(context.Background(), lease("l3")); !provider.IsCapacity(err) {
		t.Fatalf("regular launch on a full pool = %v", err)
	}
	h.step(t, time.Second)
	if h.source.created != 1 {
		t.Fatalf("created %d hosts after a regular overflow, want 1", h.source.created)
	}
}

func TestFreeSlotsCountsOnlyHostsThatTakeLeases(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	start := h.clock.Now()
	h.readyHost("a", 3, start.Add(-time.Hour))
	h.agent.occupy("a", Slot{Index: 0, ControllerID: "fleet", LeaseID: "old", RunnerName: "runner-old", State: "started"})
	h.source.add(Host{ID: "b", Address: "b:22", HostKey: "key", CreatedAt: start}) // still booting
	h.step(t, time.Second)
	if free := h.pool.freeSlots(); free != 2 {
		t.Fatalf("free slots = %d, want the two free slots of the ready host", free)
	}
	h.pool.refuseHost("a", CodeNotReady, errors.New("not ready"))
	if free := h.pool.freeSlots(); free != 0 {
		t.Fatalf("free slots of a host backing off = %d, want 0", free)
	}
}

func TestAHostBecomingReadySignalsRoomOnce(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	signals := 0
	h.pool.onRoomReady(func() { signals++ })
	h.source.add(Host{ID: "a", Address: "a:22", HostKey: "key", CreatedAt: h.clock.Now()})
	h.step(t, time.Second)
	if signals != 0 {
		t.Fatalf("a booting host signalled room %d times", signals)
	}
	h.agent.ready("a", 2)
	h.step(t, time.Second)
	h.step(t, time.Second)
	if signals != 1 {
		t.Fatalf("room signals = %d, want 1 when the host became ready", signals)
	}
}

// speculativeHarness is a harness whose pool takes speculative leases.
func speculativeHarness(t *testing.T, speculative bool) *harness {
	t.Helper()
	h := newHarness(t, ownedFacts, 0, 0)
	h.pool.cfg.Speculative = speculative
	return h
}

func TestPlacementSpeculatesOnlyOnPoolsAheadOfABootingTier(t *testing.T) {
	warm, cold, burst, drained := speculativeHarness(t, true), speculativeHarness(t, false), speculativeHarness(t, true), speculativeHarness(t, true)
	for _, h := range []*harness{warm, cold, burst, drained} {
		h.readyHost("a", 2, h.clock.Now().Add(-time.Hour))
		h.step(t, time.Second)
	}
	ephemeral := &fakeCompute{}
	placement, err := NewPlacement(
		Tier{Name: "warm", Compute: warm.pool},
		Tier{Name: "drained", Compute: drained.pool, Drain: true},
		Tier{Name: "cold", Compute: cold.pool},
		Tier{Name: "ephemeral", Compute: ephemeral},
		Tier{Name: "burst", Compute: burst.pool},
	)
	if err != nil {
		t.Fatal(err)
	}
	status := placement.ComputeStatus().(PlacementStatus)
	if !slices.Equal(status.Speculative, []string{"warm"}) {
		t.Fatalf("speculative pools = %v, want [warm]: cold opted out, burst is the overflow after ephemeral, drained starts nothing", status.Speculative)
	}
	if room := placement.SpareRoom(); room != 2 {
		t.Fatalf("spare room = %d, want warm's 2 free slots only", room)
	}
	for _, id := range []string{"l1", "l2"} {
		worker, err := placement.LaunchSpare(context.Background(), lease(id))
		if err != nil || !strings.HasPrefix(worker.ID, "warm:a/") {
			t.Fatalf("spare launch = %+v, %v; want a warm slot", worker, err)
		}
		if !placement.Fast(worker.ID) {
			t.Fatalf("a pool worker %s must count as fast", worker.ID)
		}
	}
	if _, err := placement.LaunchSpare(context.Background(), lease("l3")); !provider.IsCapacity(err) {
		t.Fatalf("spare launch with warm full = %v, want a capacity error", err)
	}
	if len(ephemeral.launched) != 0 || cold.pool.Status().Launched != 0 || burst.pool.Status().Launched != 0 || drained.pool.Status().Launched != 0 {
		t.Fatalf("a spare launch reached another tier: ephemeral %v, cold %d, burst %d, drained %d", ephemeral.launched, cold.pool.Status().Launched, burst.pool.Status().Launched, drained.pool.Status().Launched)
	}
	if warm.pool.Status().Overflowed != 0 {
		t.Fatal("a refused spare launch must not count as an overflow")
	}
	for id, fast := range map[string]bool{"burst:a/0/l9": true, "drained:a/0/l9": true, "ephemeral:fly-1": false, "unknown:x": false, "no-tier": false} {
		if placement.Fast(id) != fast {
			t.Errorf("Fast(%q) = %v, want %v", id, !fast, fast)
		}
	}
}

func TestPlacementSignalsRoomWhenASpeculativePoolGetsAReadyHost(t *testing.T) {
	warm, burst := speculativeHarness(t, true), speculativeHarness(t, true)
	placement, err := NewPlacement(
		Tier{Name: "warm", Compute: warm.pool},
		Tier{Name: "ephemeral", Compute: &fakeCompute{}},
		Tier{Name: "burst", Compute: burst.pool},
	)
	if err != nil {
		t.Fatal(err)
	}
	burst.readyHost("b", 2, burst.clock.Now())
	burst.step(t, time.Second)
	select {
	case <-placement.RoomReady():
		t.Fatal("a pool that takes no speculative runner must not wake the controller")
	default:
	}
	warm.readyHost("a", 2, warm.clock.Now())
	warm.readyHost("c", 2, warm.clock.Now())
	warm.step(t, time.Second)
	select {
	case <-placement.RoomReady():
	default:
		t.Fatal("a speculative pool's ready host must signal room")
	}
	select {
	case <-placement.RoomReady():
		t.Fatal("signals must coalesce into one pending token")
	default:
	}
}

func TestPlacementWithoutSpeculativePoolsHasNoSpareRoom(t *testing.T) {
	cold := speculativeHarness(t, false)
	cold.readyHost("a", 2, cold.clock.Now())
	cold.step(t, time.Second)
	placement, err := NewPlacement(Tier{Name: "cold", Compute: cold.pool}, Tier{Name: "ephemeral", Compute: &fakeCompute{}})
	if err != nil {
		t.Fatal(err)
	}
	if room := placement.SpareRoom(); room != 0 {
		t.Fatalf("spare room = %d, want 0", room)
	}
	if _, err := placement.LaunchSpare(context.Background(), lease("l1")); !provider.IsCapacity(err) {
		t.Fatalf("spare launch without speculative pools = %v, want a capacity error", err)
	}
	if cold.pool.Status().Launched != 0 {
		t.Fatal("a pool that opted out took a spare launch")
	}
}
