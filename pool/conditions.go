package pool

import (
	"fmt"
	"time"

	"github.com/gwendall/runneryard/provider"
)

// The pool reports conditions the controller alerts on. Each one is derived
// from the pool's state when it is read, so it clears on its own once that
// state is gone, and the normal life of a host (creation, boot, idle stop or
// return, an image change on a source that replaces hosts) raises none.
const (
	// sourceUnavailableAfter is how long the host source may fail to list
	// before it is reported. Listing is retried on every pass.
	sourceUnavailableAfter = 5 * time.Minute
	// imageUnresolvedAfter is how long a followed runner image tag may fail
	// to resolve before it is reported. It is retried every minute.
	imageUnresolvedAfter = 15 * time.Minute
	// launchFailuresReported counts consecutive failed host launches: a
	// creation round, a start, or a started host stopping before it is ready.
	launchFailuresReported = 3
	// slotFailuresReported counts consecutive refused or unconfirmed slot
	// starts on one ready host.
	slotFailuresReported = 3
	// foreignSlotAfter is how long a slot may hold a container this controller
	// did not start before it is reported.
	foreignSlotAfter = 5 * time.Minute
	// A pool reports overflow when, over the last overflowWindow, at least
	// overflowReported launches found it full, no fewer than it took, while it
	// could not add a host. The report clears when fewer than half as many
	// overflow, or under a quarter of the launches.
	overflowWindow   = time.Hour
	overflowBucket   = 5 * time.Minute
	overflowReported = 20
	// bootFailureQuiet ends a failed boot report once the pool has stopped
	// retrying: no host on its way, and no boot failure and no launch that
	// found the pool full for that long.
	bootFailureQuiet = 30 * time.Minute
)

// poolHealth is what the pool remembers to report its conditions; callers
// hold p.mu.
type poolHealth struct {
	sourceFailingSince time.Time
	imageFailingSince  time.Time
	launchFailures     int
	launchFailingSince time.Time
	launchFailure      string
	// bootFailure describes the last host that failed its boot since a host
	// last became ready, at bootFailedAt.
	bootFailingSince time.Time
	bootFailedAt     time.Time
	bootFailure      string
	window           []launchBucket
	overflowingSince time.Time
	// lastFull is the last launch that found the pool full.
	lastFull time.Time
}

// launchBucket counts the launches a pool took and refused as full in one
// overflowBucket.
type launchBucket struct {
	start      time.Time
	launched   int
	overflowed int
}

// hostHealth is what the pool remembers about one host's conditions.
type hostHealth struct {
	drainedAt        time.Time
	slotFailures     int
	slotFailingSince time.Time
	slotFailure      string
	foreignSince     time.Time
}

func (s *poolHealth) launchFailed(now time.Time, reason string) {
	if s.launchFailures == 0 {
		s.launchFailingSince = now
	}
	s.launchFailures++
	s.launchFailure = reason
}

func (s *poolHealth) launchSucceeded() {
	s.launchFailures = 0
	s.launchFailingSince = time.Time{}
	s.launchFailure = ""
}

func (s *poolHealth) bootFailed(now time.Time, failure string) {
	if s.bootFailingSince.IsZero() {
		s.bootFailingSince = now
	}
	s.bootFailedAt = now
	s.bootFailure = failure
}

func (s *poolHealth) bootRecovered() {
	s.bootFailingSince = time.Time{}
	s.bootFailedAt = time.Time{}
	s.bootFailure = ""
}

// hostReady clears what a host becoming ready proves wrong: hosts can be
// launched and booted, and this one starts slots again.
func (s *poolHealth) hostReady(h *hostState) {
	s.launchSucceeded()
	s.bootRecovered()
	h.health.slotFailures = 0
}

func (s *hostHealth) slotFailed(now time.Time, reason string) {
	if s.slotFailures == 0 {
		s.slotFailingSince = now
	}
	s.slotFailures++
	s.slotFailure = reason
}

// trackForeign records since when the host's slots hold a container this
// controller did not start; callers hold p.mu.
func (p *Pool) trackForeign(h *hostState, now time.Time) {
	switch {
	case !p.foreign(h):
		h.health.foreignSince = time.Time{}
	case h.health.foreignSince.IsZero():
		h.health.foreignSince = now
	}
}

// foreign reports whether a slot holds a container this controller did not
// start; callers hold p.mu.
func (p *Pool) foreign(h *hostState) bool {
	for _, slot := range h.slots {
		if slot.ControllerID != p.cfg.ControllerID {
			return true
		}
	}
	return false
}

// count records one launch the pool took, or one it refused as full.
func (s *poolHealth) count(now time.Time, overflowed bool) {
	start := now.Truncate(overflowBucket)
	if last := len(s.window) - 1; last < 0 || !s.window[last].start.Equal(start) {
		kept := s.window[:0]
		for _, bucket := range s.window {
			if now.Sub(bucket.start) < overflowWindow {
				kept = append(kept, bucket)
			}
		}
		s.window = append(kept, launchBucket{start: start})
	}
	bucket := &s.window[len(s.window)-1]
	if overflowed {
		s.lastFull = now
		bucket.overflowed++
	} else {
		bucket.launched++
	}
}

// recent sums the launches taken and refused over the last overflowWindow.
func (s *poolHealth) recent(now time.Time) (launched, overflowed int) {
	for _, bucket := range s.window {
		if now.Sub(bucket.start) < overflowWindow {
			launched += bucket.launched
			overflowed += bucket.overflowed
		}
	}
	return launched, overflowed
}

// checkHealth updates the reports that time alone changes. Overflow starts
// or ends: a burst that a new host absorbs is not reported, the pool must be
// unable to add one. A failed boot ends once the pool has stopped retrying,
// with no host on its way and no demand for one; the next failure reports it
// again.
func (p *Pool) checkHealth() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	if !p.health.bootFailingSince.IsZero() && !p.hostOnItsWay() &&
		now.Sub(latest(p.health.bootFailedAt, p.health.lastFull)) >= bootFailureQuiet {
		p.health.bootRecovered()
	}
	launched, overflowed := p.health.recent(now)
	switch {
	case p.health.overflowingSince.IsZero():
		if overflowed >= overflowReported && overflowed >= launched && p.atCeiling(now) {
			p.health.overflowingSince = now
		}
	case 2*overflowed < overflowReported || 3*overflowed < launched:
		p.health.overflowingSince = time.Time{}
	}
}

// hostOnItsWay reports whether a host is being created, started, or booted.
// Callers hold p.mu.
func (p *Pool) hostOnItsWay() bool {
	for _, h := range p.hosts {
		if h.phase == phaseCreating || h.phase == phaseStarting || h.phase == phaseBooting {
			return true
		}
	}
	return false
}

// atCeiling reports whether the pool can add no room: no host on its way,
// none stopped that it may start, and no creation left within max_hosts.
// Callers hold p.mu.
func (p *Pool) atCeiling(now time.Time) bool {
	if p.hostOnItsWay() {
		return false
	}
	if p.startable(now) != nil {
		return false
	}
	return !p.facts.CanCreate || len(p.hosts) >= p.cfg.MaxHosts
}

// Conditions reports the pool's problems present now. It implements
// provider.ConditionReporter.
func (p *Pool) Conditions() []provider.Condition {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conditions(p.cfg.Now())
}

// conditions derives the pool's conditions; callers hold p.mu.
func (p *Pool) conditions(now time.Time) []provider.Condition {
	name := p.cfg.Name
	conditions := make([]provider.Condition, 0)
	add := func(kind, host string, since time.Time, summary, action string) {
		key := "pool/" + name + "/" + kind
		if host != "" {
			key += "/" + host
		}
		conditions = append(conditions, provider.Condition{Key: key, Summary: summary, Action: action, Since: since})
	}
	health := &p.health
	if since := health.sourceFailingSince; !since.IsZero() && now.Sub(since) >= sourceUnavailableAfter {
		add("host_source_unavailable", "", since,
			fmt.Sprintf("pool %s cannot list its hosts", name),
			"The pool keeps using the hosts it already knows. Check the provider token and the provider's status; the controller log has each error.")
	}
	if since := health.imageFailingSince; !since.IsZero() && now.Sub(since) >= imageUnresolvedAfter {
		add("image_tag_unresolved", "", since,
			fmt.Sprintf("pool %s cannot resolve its runner image tag", name),
			"Hosts keep their image, and new hosts use the last digest resolved, or the tag itself before any. Check the registry token and that the tag exists; the controller log has each error.")
	}
	if health.launchFailures >= launchFailuresReported {
		add("host_launch_failing", "", health.launchFailingSince,
			fmt.Sprintf("pool %s failed to create or start a host %d times in a row (last: %s)", name, health.launchFailures, health.launchFailure),
			"Jobs the pool cannot take go to the next tier, or wait if it is the last one. Check the provider quota, capacity in the region, and the token; the controller log has each error.")
	}
	if !health.bootFailingSince.IsZero() {
		add("host_boot_failed", "", health.bootFailingSince,
			fmt.Sprintf("pool %s %s", name, health.bootFailure),
			"Check the host's boot log and the pool's runner_image. Until a host becomes ready, the pool keeps replacing hosts on demand, each billed while it boots.")
	}
	if !health.overflowingSince.IsZero() {
		launched, overflowed := health.recent(now)
		action := "Jobs went to the next tier, or waited if the pool is the last one. If the load lasts, raise max_hosts or slots."
		if !p.facts.CanCreate {
			action = "Jobs went to the next tier, or waited if the pool is the last one. If the load lasts, add hosts or raise slots."
		}
		add("pool_overflowing", "", health.overflowingSince,
			fmt.Sprintf("pool %s was full for %d of %d launches in the last hour", name, overflowed, launched+overflowed), action)
	}
	for _, id := range p.sortedIDs() {
		h := p.hosts[id]
		switch {
		case (h.phase == phaseReady || h.phase == phaseDraining) && !h.lastContact.IsZero() && now.Sub(h.lastContact) > p.cfg.UnreachableGrace:
			action := "The pool sends it no new job; jobs running there fail if it does not come back. Check the host and the network from the controller to it."
			if p.facts.CanCreate {
				action += " The pool deletes it once it has been silent for longer than the maximum worker lifetime."
			}
			add("host_unreachable", id, h.lastContact,
				fmt.Sprintf("pool %s host %s has not answered for more than %s", name, id, roughly(p.cfg.UnreachableGrace)), action)
		case h.phase == phaseBooting && !h.ownBoot && now.Sub(latest(h.firstSeen, h.bootSince)) > p.cfg.BootTimeout:
			// A host the pool did not see boot is never returned for a boot
			// timeout: it may have run slots before this process.
			add("host_not_serving", id, latest(h.firstSeen, h.bootSince),
				fmt.Sprintf("pool %s host %s has not become ready for more than %s", name, id, roughly(p.cfg.BootTimeout)),
				"The pool sends it no job. Check the host's agent and its boot log.")
		case h.phase == phaseDraining && !p.facts.CanCreate && !h.health.drainedAt.IsZero() && now.Sub(h.health.drainedAt) > p.cfg.BootTimeout:
			add("host_not_serving", id, h.health.drainedAt,
				fmt.Sprintf("pool %s static host %s takes no job (%s)", name, id, h.drainReason),
				"The pool never replaces a static host. Update it to the pool's runner image, then restart the controller to put it back in service.")
		case h.phase == phaseReady && h.health.slotFailures >= slotFailuresReported:
			add("host_refusing", id, h.health.slotFailingSince,
				fmt.Sprintf("pool %s host %s failed to start %d slots in a row (last: %s)", name, id, h.health.slotFailures, h.health.slotFailure),
				"The pool sends new jobs to its other hosts first. Check the host's agent; for disk_full, free the host's disk.")
		}
		if since := h.health.foreignSince; !since.IsZero() && p.foreign(h) && now.Sub(since) >= foreignSlotAfter {
			add("foreign_slot", id, since,
				fmt.Sprintf("pool %s host %s has a slot container this controller did not start", name, id),
				"The pool leaves that slot alone, so the host runs fewer jobs. Look for a second controller using the host, or a container started by hand or under an old CONTROLLER_ID, and remove it.")
		}
	}
	return conditions
}

// launchFailureReason names a failed host launch without the provider's
// error text, which may quote a request.
func launchFailureReason(err error) string {
	if provider.IsCapacity(err) {
		return provider.CapacityReason(err)
	}
	return "provider_error"
}

// roughly formats a duration for a message: "45s", "10m", "2h05m".
func roughly(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return fmt.Sprintf("%dm", d.Round(time.Minute)/time.Minute)
	default:
		d = d.Round(time.Minute)
		return fmt.Sprintf("%dh%02dm", d/time.Hour, d%time.Hour/time.Minute)
	}
}
