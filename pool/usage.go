package pool

import (
	"math"
	"time"
)

// meter measures one host's time since this process first saw it. Each
// accrual adds the time since the previous one at the state the previous one
// recorded, then records the current state, so a change to a host's phase,
// billing or slots is followed by an accrual before the clock moves on.
type meter struct {
	at      time.Time
	running bool
	ready   bool
	busy    int
	used    usage
}

// usage is measured time: running is billed time, ready is time taking jobs,
// busy is slot time holding a runner or a reservation while running, and
// readyBusy its share while ready. The slot fields are in slot-seconds.
type usage struct {
	running, ready  time.Duration
	busy, readyBusy float64
	// slot and readySlot are the slot time the running and ready time
	// offered, filled when a host's usage is read.
	slot, readySlot float64
}

func (u usage) add(other usage) usage {
	return usage{
		running: u.running + other.running, ready: u.ready + other.ready,
		busy: u.busy + other.busy, readyBusy: u.readyBusy + other.readyBusy,
		slot: u.slot + other.slot, readySlot: u.readySlot + other.readySlot,
	}
}

func (u usage) report() Usage {
	return Usage{
		RunningSeconds: int64(u.running / time.Second), ReadySeconds: int64(u.ready / time.Second),
		SlotSeconds: int64(u.slot), BusySlotSeconds: int64(u.busy),
		Occupancy: ratio(u.busy, u.slot), ReadyOccupancy: ratio(u.readyBusy, u.readySlot),
	}
}

// Usage is what a host, or a pool's hosts together, did since the pool's
// MeasuredSince.
type Usage struct {
	// RunningSeconds is billed time: from a start or a creation to the stop
	// or the deletion. ReadySeconds is the part spent taking jobs.
	RunningSeconds int64 `json:"running_seconds"`
	ReadySeconds   int64 `json:"ready_seconds"`
	// SlotSeconds is RunningSeconds times the host's slots: the slot time
	// paid for. BusySlotSeconds is the slot time that held a runner, from
	// its start to its removal, as an ephemeral worker would have run.
	SlotSeconds     int64 `json:"slot_seconds"`
	BusySlotSeconds int64 `json:"busy_slot_seconds"`
	// Occupancy is BusySlotSeconds / SlotSeconds. ReadyOccupancy is the same
	// share of the slot time of ready hosts only, without boots and stops.
	Occupancy      float64 `json:"occupancy"`
	ReadyOccupancy float64 `json:"ready_occupancy"`
}

// EphemeralEquivalent prices a pool's busy slot time as ephemeral workers:
// each slot-hour that held a runner at the ephemeral tier's hourly price.
type EphemeralEquivalent struct {
	Tier        string  `json:"tier"`
	HourlyPrice float64 `json:"hourly_price"`
	Currency    string  `json:"currency"`
	Cost        float64 `json:"cost"`
}

// ephemeralEquivalent prices busySlotSeconds at an ephemeral tier's price.
func ephemeralEquivalent(busySlotSeconds int64, tier string, hourlyPrice float64, currency string) *EphemeralEquivalent {
	return &EphemeralEquivalent{
		Tier: tier, HourlyPrice: hourlyPrice, Currency: currency,
		Cost: priceHours(float64(busySlotSeconds)/3600, hourlyPrice),
	}
}

// priceHours is every cost in the pool status: hours at an hourly price,
// rounded to the cent.
func priceHours(hours, hourlyPrice float64) float64 {
	return math.Round(hours*hourlyPrice*100) / 100
}

func ratio(part, whole float64) float64 {
	if whole <= 0 {
		return 0
	}
	return math.Round(part/whole*1000) / 1000
}

// accrue brings every host's meter to now. Callers hold p.mu.
func (p *Pool) accrue(now time.Time) {
	for _, h := range p.hosts {
		m := &h.meter
		if elapsed := now.Sub(m.at); !m.at.IsZero() && elapsed > 0 {
			if m.running {
				m.used.running += elapsed
				m.used.busy += float64(m.busy) * elapsed.Seconds()
			}
			if m.ready {
				m.used.ready += elapsed
				m.used.readyBusy += float64(m.busy) * elapsed.Seconds()
			}
		}
		if now.After(m.at) {
			m.at = now
		}
		m.running = !h.runningSince.IsZero()
		m.ready = m.running && h.phase == phaseReady
		m.busy = 0
		if m.running {
			m.busy = h.occupied() + len(h.reserved)
			if slots := p.slotsOf(h); slots > 0 {
				m.busy = min(m.busy, slots)
			}
		}
	}
}

// slotsOf returns a host's slots, or before its first probe the most slots a
// host of the pool has reported. Callers hold p.mu.
func (p *Pool) slotsOf(h *hostState) int {
	if h.info.Slots > 0 {
		return h.info.Slots
	}
	return p.slotsSeen
}

// usageOf returns a host's measured usage; callers hold p.mu and accrued.
func (p *Pool) usageOf(h *hostState) usage {
	used := h.meter.used
	slots := float64(p.slotsOf(h))
	used.slot = slots * used.running.Seconds()
	used.readySlot = slots * used.ready.Seconds()
	return used
}

// forget removes a host and keeps its usage in the pool's. Callers hold p.mu.
func (p *Pool) forget(id string, now time.Time) {
	h, ok := p.hosts[id]
	if !ok {
		return
	}
	p.accrue(now)
	p.retired = p.retired.add(p.usageOf(h))
	delete(p.hosts, id)
}
