package pool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestUsageFollowsReadyStoppedAndBootingTime(t *testing.T) {
	h, _ := newStopHarness(t, 0, 2, 1)
	measuredSince := h.clock.Now()
	h.readyHost("a", 2, h.clock.Now().Add(-time.Hour))
	h.step(t, time.Second)
	if _, err := h.pool.Launch(context.Background(), lease("l1")); err != nil {
		t.Fatal(err)
	}
	h.step(t, 30*time.Minute)
	if err := h.pool.Destroy(context.Background(), "a/0/l1"); err != nil {
		t.Fatal(err)
	}
	h.step(t, 5*time.Minute)
	host := hostStatus(t, h.pool, "a")
	if host.Phase != string(phaseStopped) {
		t.Fatalf("setup: phase = %s, want the idle host stopped", host.Phase)
	}
	// 35 minutes ready on 2 slots, one of them busy for 30.
	want := Usage{RunningSeconds: 2100, ReadySeconds: 2100, SlotSeconds: 4200, BusySlotSeconds: 1800, Occupancy: 0.429, ReadyOccupancy: 0.429}
	if host.Usage != want {
		t.Fatalf("usage when stopped = %+v, want %+v", host.Usage, want)
	}

	// A stopped hour adds nothing; the boot after a start is running time
	// but not ready time.
	h.clock.advance(time.Hour)
	if _, err := h.pool.Launch(context.Background(), lease("l2")); err == nil {
		t.Fatal("setup: a launch on a stopped pool should overflow")
	}
	h.step(t, time.Second)
	h.step(t, 30*time.Second)
	if phase := hostStatus(t, h.pool, "a").Phase; phase != string(phaseReady) {
		t.Fatalf("setup: phase after the start = %s, want ready", phase)
	}
	h.step(t, 10*time.Minute)
	status := h.pool.Status()
	want = Usage{RunningSeconds: 2730, ReadySeconds: 2700, SlotSeconds: 5460, BusySlotSeconds: 1800, Occupancy: 0.33, ReadyOccupancy: 0.333}
	if status.Usage != want || status.Hosts[0].Usage != want {
		t.Fatalf("pool usage = %+v, host usage = %+v, want %+v", status.Usage, status.Hosts[0].Usage, want)
	}
	// The host had run an hour before this process saw it: Cost counts that
	// hour, MeasuredCost only the 2730 seconds measured.
	if !status.MeasuredSince.Equal(measuredSince) || status.Cost != 0.88 || status.MeasuredCost != 0.38 {
		t.Fatalf("measured since %s, cost %v, measured cost %v; want %s, 0.88, 0.38", status.MeasuredSince, status.Cost, status.MeasuredCost, measuredSince)
	}
}

func TestUsageOfAHostStoppedMidWindow(t *testing.T) {
	h, source := newStopHarness(t, 1, 2, 1)
	h.readyHost("a", 2, h.clock.Now().Add(-2*time.Hour))
	h.readyHost("b", 2, h.clock.Now().Add(-time.Hour))
	h.step(t, time.Second)
	if worker, err := h.pool.Launch(context.Background(), lease("l1")); err != nil || !strings.HasPrefix(worker.ID, "a/") {
		t.Fatalf("launch = %+v, %v; want a slot on the older host", worker, err)
	}
	h.clock.advance(10 * time.Minute)
	source.outside("a", HostStopped)
	h.step(t, 0)
	h.step(t, 20*time.Minute)
	status := h.pool.Status()
	if a := hostStatus(t, h.pool, "a"); a.Phase != string(phaseStopped) || a.Usage != (Usage{RunningSeconds: 600, ReadySeconds: 600, SlotSeconds: 1200, BusySlotSeconds: 600, Occupancy: 0.5, ReadyOccupancy: 0.5}) {
		t.Fatalf("stopped host = %+v", a)
	}
	if b := hostStatus(t, h.pool, "b"); b.Usage != (Usage{RunningSeconds: 1800, ReadySeconds: 1800, SlotSeconds: 3600}) {
		t.Fatalf("running host = %+v", b)
	}
	if want := (Usage{RunningSeconds: 2400, ReadySeconds: 2400, SlotSeconds: 4800, BusySlotSeconds: 600, Occupancy: 0.125, ReadyOccupancy: 0.125}); status.Usage != want {
		t.Fatalf("pool usage = %+v, want %+v", status.Usage, want)
	}
}

func TestUsageCountsAReusedSlotOnce(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	h.readyHost("a", 2, h.clock.Now().Add(-time.Hour))
	h.step(t, time.Second)
	for _, id := range []string{"l1", "l2"} {
		worker, err := h.pool.Launch(context.Background(), lease(id))
		if err != nil || worker.ID != "a/0/"+id {
			t.Fatalf("launch %s = %+v, %v; want slot 0", id, worker, err)
		}
		h.step(t, 10*time.Minute)
		if err := h.pool.Destroy(context.Background(), worker.ID); err != nil {
			t.Fatal(err)
		}
		h.step(t, 5*time.Minute)
	}
	status := h.pool.Status()
	want := Usage{RunningSeconds: 1800, ReadySeconds: 1800, SlotSeconds: 3600, BusySlotSeconds: 1200, Occupancy: 0.333, ReadyOccupancy: 0.333}
	if status.Usage != want || status.Cost != 0 || status.MeasuredCost != 0 || status.EphemeralEquivalent != nil {
		t.Fatalf("status = %+v, want usage %+v and no cost", status, want)
	}
}

func TestUsageKeepsAReturnedHost(t *testing.T) {
	h := newHarness(t, Facts{Billing: BillingPerSecond, HourlyPrice: 0.5, Currency: "USD", CanCreate: true}, 0, 2)
	h.readyHost("a", 2, h.clock.Now().Add(-5*time.Minute))
	h.step(t, time.Second)
	worker, err := h.pool.Launch(context.Background(), lease("l1"))
	if err != nil {
		t.Fatal(err)
	}
	h.step(t, 10*time.Minute)
	if err := h.pool.Destroy(context.Background(), worker.ID); err != nil {
		t.Fatal(err)
	}
	h.step(t, 2*time.Minute)
	if deleted := h.source.deletedHosts(); len(deleted) != 1 {
		t.Fatalf("setup: deleted = %v, want the idle host returned", deleted)
	}
	h.step(t, time.Hour)
	status := h.pool.Status()
	want := Usage{RunningSeconds: 720, ReadySeconds: 720, SlotSeconds: 1440, BusySlotSeconds: 600, Occupancy: 0.417, ReadyOccupancy: 0.417}
	if len(status.Hosts) != 0 || status.ReturnedHosts != 1 || status.Usage != want {
		t.Fatalf("status = %+v, want no host and usage %+v", status, want)
	}
	// 17 minutes billed, 12 of them measured.
	if status.Cost != 0.14 || status.MeasuredCost != 0.1 {
		t.Fatalf("cost %v, measured cost %v; want 0.14 and 0.10", status.Cost, status.MeasuredCost)
	}
}

type pricedCompute struct {
	fakeCompute
	price float64
	known bool
}

func (c *pricedCompute) WorkerHourlyPrice() (float64, string, bool) { return c.price, "USD", c.known }

func TestPlacementPricesBusySlotTimeAsEphemeralWorkers(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	h.readyHost("a", 2, h.clock.Now().Add(-time.Hour))
	h.step(t, time.Second)
	if _, err := h.pool.Launch(context.Background(), lease("l1")); err != nil {
		t.Fatal(err)
	}
	h.step(t, 30*time.Minute)

	for name, test := range map[string]struct {
		ephemeral *pricedCompute
		want      *EphemeralEquivalent
	}{
		"known price": {
			ephemeral: &pricedCompute{price: 0.4, known: true},
			want:      &EphemeralEquivalent{Tier: "ephemeral", HourlyPrice: 0.4, Currency: "USD", Cost: 0.2},
		},
		"unknown price": {ephemeral: &pricedCompute{price: 0.4}},
		"no price":      {},
	} {
		t.Run(name, func(t *testing.T) {
			var ephemeral Tier
			if test.ephemeral != nil {
				ephemeral = Tier{Name: "ephemeral", Compute: test.ephemeral}
			} else {
				ephemeral = Tier{Name: "ephemeral", Compute: &fakeCompute{}}
			}
			placement, err := NewPlacement(Tier{Name: "warm", Compute: h.pool}, ephemeral)
			if err != nil {
				t.Fatal(err)
			}
			status := placement.ComputeStatus().(PlacementStatus)
			got := status.Pools[0].EphemeralEquivalent
			if (got == nil) != (test.want == nil) || (got != nil && *got != *test.want) {
				t.Fatalf("ephemeral equivalent = %+v, want %+v", got, test.want)
			}
			encoded, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "ephemeral_equivalent") != (test.want != nil) || !strings.Contains(string(encoded), `"busy_slot_seconds":1800`) {
				t.Fatalf("status = %s", encoded)
			}
		})
	}
}
