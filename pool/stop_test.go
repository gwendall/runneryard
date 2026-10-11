package pool

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gwendall/runneryard/provider"
)

// stopSource is a fakeSource whose hosts stop and start: a stopped host's
// agent does not answer, and its slot containers are gone.
type stopSource struct {
	fakeSource
	agent    *fakeAgent
	stopErr  error
	startErr error
	stops    []string
	starts   []string
}

func (s *stopSource) Stop(_ context.Context, id string) error {
	s.mu.Lock()
	s.stops = append(s.stops, id)
	if s.stopErr != nil {
		s.mu.Unlock()
		return s.stopErr
	}
	s.setState(id, HostStopped)
	s.mu.Unlock()
	s.agent.mu.Lock()
	s.agent.unreachable[id] = true
	s.agent.slots[id] = map[int]Slot{}
	s.agent.mu.Unlock()
	return nil
}

func (s *stopSource) Start(_ context.Context, id string) error {
	s.mu.Lock()
	s.starts = append(s.starts, id)
	if s.startErr != nil {
		s.mu.Unlock()
		return s.startErr
	}
	s.setState(id, "running")
	s.mu.Unlock()
	s.agent.setUnreachable(id, false)
	return nil
}

// setState changes a host's state as its provider would; callers hold s.mu.
func (s *stopSource) setState(id, state string) {
	host := s.hosts[id]
	host.State = state
	host.StateSince = s.clock.Now()
	s.hosts[id] = host
}

func (s *stopSource) outside(id, state string) {
	s.mu.Lock()
	s.setState(id, state)
	s.mu.Unlock()
	s.agent.setUnreachable(id, state == HostStopped)
}

func (s *stopSource) calls() (stops, starts []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.stops...), append([]string(nil), s.starts...)
}

func newStopHarness(t *testing.T, minHosts, maxHosts, keepStopped int) (*harness, *stopSource) {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
	agent := newFakeAgent()
	source := &stopSource{
		fakeSource: fakeSource{facts: Facts{Billing: BillingPerSecond, HourlyPrice: 0.5, Currency: "USD", CanCreate: true}, clock: clock},
		agent:      agent,
	}
	created, err := New(Config{
		Name: "main", ControllerID: "fleet", Image: testImage, MinHosts: minHosts, MaxHosts: maxHosts,
		Source: source, Agent: agent, MaxLifetime: 2 * time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: clock.Now,
		Tick: 10 * time.Second, HostListInterval: time.Second,
		IdleStop: 5 * time.Minute, KeepStopped: keepStopped, DeleteStoppedAfter: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{pool: created, source: &source.fakeSource, agent: agent, clock: clock}, source
}

func sortedDeletes(source *fakeSource) string {
	deleted := source.deletedHosts()
	slices.Sort(deleted)
	return strings.Join(deleted, ",")
}

func hostStatus(t *testing.T, p *Pool, id string) HostStatus {
	t.Helper()
	for _, host := range p.Status().Hosts {
		if host.ID == id {
			return host
		}
	}
	t.Fatalf("host %s is not in the status", id)
	return HostStatus{}
}

func TestIdleHostIsStoppedInsteadOfDeleted(t *testing.T) {
	h, source := newStopHarness(t, 0, 2, 1)
	h.readyHost("a", 2, h.clock.Now().Add(-time.Hour))
	h.step(t, time.Second)
	h.step(t, 4*time.Minute)
	if stops, _ := source.calls(); len(stops) != 0 {
		t.Fatalf("stopped %v before idle_stop_after", stops)
	}
	h.step(t, time.Minute+5*time.Second)
	stops, _ := source.calls()
	if len(stops) != 1 || stops[0] != "a" || len(h.source.deletedHosts()) != 0 {
		t.Fatalf("stops = %v, deleted = %v; want the idle host stopped and kept", stops, h.source.deletedHosts())
	}
	host := hostStatus(t, h.pool, "a")
	if host.Phase != string(phaseStopped) || host.Reachable || host.StoppedAt.IsZero() {
		t.Fatalf("stopped host status = %+v", host)
	}
	if status := h.pool.Status(); status.HostStops != 1 || status.SlotsTotal != 0 {
		t.Fatalf("status = %+v, want one stop and no slots", status)
	}
	if workers, err := h.pool.Inventory(context.Background()); err != nil || len(workers) != 0 {
		t.Fatalf("inventory = %v, %v; a stopped host runs nothing", workers, err)
	}
	// Staying stopped is not a reason to stop again or delete.
	h.step(t, 30*time.Minute)
	if stops, _ := source.calls(); len(stops) != 1 || len(h.source.deletedHosts()) != 0 {
		t.Fatalf("stops = %v, deleted = %v after staying stopped", stops, h.source.deletedHosts())
	}
}

func TestDemandStartsAStoppedHostBeforeCreatingOne(t *testing.T) {
	h, source := newStopHarness(t, 0, 2, 1)
	h.readyHost("a", 2, h.clock.Now().Add(-time.Hour))
	h.step(t, time.Second)
	h.step(t, 6*time.Minute)
	if hostStatus(t, h.pool, "a").Phase != string(phaseStopped) {
		t.Fatal("setup: the idle host should be stopped")
	}
	billedWhenStopped := h.pool.Status().BilledHours
	h.clock.advance(2 * time.Hour)
	if _, err := h.pool.Launch(context.Background(), lease("l1")); !provider.IsCapacity(err) {
		t.Fatalf("launch with only a stopped host = %v, want capacity", err)
	}
	if billed := h.pool.Status().BilledHours; billed != billedWhenStopped {
		t.Fatalf("billed hours grew from %f to %f while stopped", billedWhenStopped, billed)
	}
	h.step(t, time.Second)
	if _, starts := source.calls(); len(starts) != 1 || starts[0] != "a" || h.source.created != 0 {
		t.Fatalf("starts = %v, created = %d; want the stopped host started and nothing created", starts, h.source.created)
	}
	if phase := hostStatus(t, h.pool, "a").Phase; phase != string(phaseBooting) {
		t.Fatalf("phase after start = %s, want booting", phase)
	}
	h.step(t, time.Second)
	worker, err := h.pool.Launch(context.Background(), lease("l1"))
	if err != nil || !strings.HasPrefix(worker.ID, "a/") {
		t.Fatalf("launch after the start = %+v, %v; want a slot on the started host", worker, err)
	}
	if status := h.pool.Status(); status.HostStarts != 1 || status.HostStops != 1 {
		t.Fatalf("status = %+v, want one stop and one start", status)
	}
}

func TestMinHostsStayRunningAndOnlySpareHostsStop(t *testing.T) {
	h, source := newStopHarness(t, 1, 3, 1)
	h.readyHost("old", 2, h.clock.Now().Add(-2*time.Hour))
	h.readyHost("new", 2, h.clock.Now().Add(-time.Hour))
	h.step(t, time.Second)
	h.step(t, 10*time.Minute)
	if stops, _ := source.calls(); len(stops) != 1 || stops[0] != "new" {
		t.Fatalf("stops = %v, want only the spare host stopped", stops)
	}
	h.step(t, 3*time.Hour)
	if phase := hostStatus(t, h.pool, "old").Phase; phase != string(phaseReady) {
		t.Fatalf("base host phase = %s, want ready", phase)
	}
}

func TestMaxHostsCountsStoppedHosts(t *testing.T) {
	h, source := newStopHarness(t, 0, 2, 2)
	h.readyHost("busy", 1, h.clock.Now().Add(-time.Hour))
	h.agent.occupy("busy", Slot{Index: 0, ControllerID: "fleet", LeaseID: "l0", RunnerName: "r"})
	h.source.add(Host{ID: "off", Address: "off:22", HostKey: "key", State: HostStopped, StateSince: h.clock.Now().Add(-time.Hour)})
	h.agent.ready("off", 1)
	h.agent.setUnreachable("off", true)
	source.startErr = errors.New("machine failed to start")
	h.step(t, time.Second)
	if _, err := h.pool.Launch(context.Background(), lease("l1")); !provider.IsCapacity(err) {
		t.Fatalf("launch on a full pool = %v, want capacity", err)
	}
	h.step(t, time.Second)
	h.step(t, time.Second)
	if _, starts := source.calls(); len(starts) != 1 || h.source.created != 0 {
		t.Fatalf("starts = %v, created = %d; a stopped host counts toward max_hosts", starts, h.source.created)
	}
}

func TestFailedStartsBackOffThenTheHostIsReplaced(t *testing.T) {
	h, source := newStopHarness(t, 0, 2, 1)
	h.source.add(Host{ID: "off", Address: "off:22", HostKey: "key", State: HostStopped, StateSince: h.clock.Now().Add(-time.Hour)})
	h.agent.ready("off", 2)
	h.agent.setUnreachable("off", true)
	source.startErr = errors.New("machine failed to start")
	h.step(t, time.Second)
	if _, err := h.pool.Launch(context.Background(), lease("l1")); !provider.IsCapacity(err) {
		t.Fatalf("launch = %v, want capacity", err)
	}
	h.step(t, time.Second)
	// The failed start keeps the demand, and the next pass creates a host
	// while the stopped one backs off.
	h.step(t, time.Second)
	if _, starts := source.calls(); len(starts) != 1 || h.source.created != 1 {
		t.Fatalf("starts = %v, created = %d; want one failed start, then a creation", starts, h.source.created)
	}
	if phase := hostStatus(t, h.pool, "off").Phase; phase != string(phaseStopped) {
		t.Fatalf("phase after a failed start = %s, want stopped", phase)
	}
	h.agent.ready("new-1", 2)
	h.step(t, time.Second)
	for i := range 2 {
		if _, err := h.pool.Launch(context.Background(), lease("fill"+string(rune('a'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.pool.Launch(context.Background(), lease("l2")); !provider.IsCapacity(err) {
		t.Fatalf("launch on a full pool = %v, want capacity", err)
	}
	h.step(t, time.Second)
	if _, starts := source.calls(); len(starts) != 1 {
		t.Fatalf("starts = %v, want the failed host left alone during its backoff", starts)
	}
	h.clock.advance(time.Minute)
	if _, err := h.pool.Launch(context.Background(), lease("l3")); !provider.IsCapacity(err) {
		t.Fatalf("launch on a full pool = %v, want capacity", err)
	}
	h.step(t, time.Second)
	h.step(t, time.Second)
	if _, starts := source.calls(); len(starts) != 2 {
		t.Fatalf("starts = %v, want a second attempt after the backoff", starts)
	}
	if deleted := h.source.deletedHosts(); len(deleted) != 1 || deleted[0] != "off" {
		t.Fatalf("deleted = %v, want the host that failed to start twice", deleted)
	}
}

func TestStartRefusedForCapacityDeletesTheHostAtOnce(t *testing.T) {
	h, source := newStopHarness(t, 0, 2, 1)
	h.source.add(Host{ID: "off", Address: "off:22", HostKey: "key", State: HostStopped, StateSince: h.clock.Now().Add(-time.Hour)})
	source.startErr = &provider.CapacityError{Reason: "host_full", Err: errors.New("insufficient resources on the host")}
	h.step(t, time.Second)
	if _, err := h.pool.Launch(context.Background(), lease("l1")); !provider.IsCapacity(err) {
		t.Fatalf("launch = %v, want capacity", err)
	}
	h.step(t, time.Second)
	h.step(t, time.Second)
	if deleted := h.source.deletedHosts(); len(deleted) != 1 || deleted[0] != "off" {
		t.Fatalf("deleted = %v, want the host whose machine is full", deleted)
	}
	if h.source.created != 1 {
		t.Fatalf("created = %d, want a replacement for the waiting demand", h.source.created)
	}
}

func TestStoppedHostsBeyondKeepStoppedAreDeletedAfterAWhile(t *testing.T) {
	h, _ := newStopHarness(t, 0, 4, 1)
	now := h.clock.Now()
	for id, stoppedFor := range map[string]time.Duration{"a": 3 * time.Hour, "b": 2 * time.Hour, "c": 10 * time.Minute, "d": 20 * time.Minute} {
		h.source.add(Host{ID: id, Address: id + ":22", HostKey: "key", CreatedAt: now.Add(-5 * time.Hour), State: HostStopped, StateSince: now.Add(-stoppedFor)})
	}
	h.step(t, time.Second)
	if deleted := sortedDeletes(h.source); deleted != "a,b" {
		t.Fatalf("deleted = %s, want the hosts stopped for longer than delete_stopped_after", deleted)
	}
	h.step(t, 45*time.Minute)
	if deleted := sortedDeletes(h.source); deleted != "a,b,d" {
		t.Fatalf("deleted = %s, want d deleted and the most recently stopped host kept", deleted)
	}
	h.step(t, 10*time.Hour)
	if deleted := sortedDeletes(h.source); deleted != "a,b,d" {
		t.Fatalf("deleted = %s, the kept stopped host must stay", deleted)
	}
	if phase := hostStatus(t, h.pool, "c").Phase; phase != string(phaseStopped) {
		t.Fatalf("kept host phase = %s", phase)
	}
}

func TestRestartAdoptsStoppedHostsAndStartsOneForMinHosts(t *testing.T) {
	h, source := newStopHarness(t, 1, 3, 1)
	now := h.clock.Now()
	h.source.add(Host{ID: "off-old", Address: "off-old:22", HostKey: "key", CreatedAt: now.Add(-5 * time.Hour), State: HostStopped, StateSince: now.Add(-30 * time.Minute)})
	h.source.add(Host{ID: "off-new", Address: "off-new:22", HostKey: "key", CreatedAt: now.Add(-5 * time.Hour), State: HostStopped, StateSince: now.Add(-5 * time.Minute)})
	h.agent.ready("off-old", 2)
	h.agent.ready("off-new", 2)
	h.agent.setUnreachable("off-old", true)
	h.agent.setUnreachable("off-new", true)
	h.step(t, time.Second)
	if _, starts := source.calls(); len(starts) != 1 || starts[0] != "off-new" || h.source.created != 0 {
		t.Fatalf("starts = %v, created = %d; want the most recently stopped host started for min_hosts", starts, h.source.created)
	}
	if len(h.source.deletedHosts()) != 0 {
		t.Fatalf("deleted = %v on adoption", h.source.deletedHosts())
	}
	h.step(t, time.Second)
	if phase := hostStatus(t, h.pool, "off-new").Phase; phase != string(phaseReady) {
		t.Fatalf("started host phase = %s, want ready", phase)
	}
	if phase := hostStatus(t, h.pool, "off-old").Phase; phase != string(phaseStopped) {
		t.Fatalf("other stopped host phase = %s, want stopped", phase)
	}
}

func TestStopsAndStartsOutsideThePoolAreFollowed(t *testing.T) {
	h, source := newStopHarness(t, 0, 2, 1)
	h.readyHost("a", 2, h.clock.Now().Add(-time.Hour))
	h.agent.occupy("a", Slot{Index: 0, ControllerID: "fleet", LeaseID: "l0", RunnerName: "r"})
	h.step(t, time.Second)
	source.outside("a", HostStopped)
	h.step(t, 2*time.Second)
	if phase := hostStatus(t, h.pool, "a").Phase; phase != string(phaseStopped) {
		t.Fatalf("phase after an outside stop = %s, want stopped", phase)
	}
	workers, err := h.pool.Inventory(context.Background())
	if err != nil || len(workers) != 0 {
		t.Fatalf("inventory = %v, %v; the stopped host's slots died with it", workers, err)
	}
	// The core removes the departed worker; on a stopped host that needs
	// no agent call.
	if err := h.pool.Destroy(context.Background(), WorkerID("a", 0, "l0")); err != nil {
		t.Fatal(err)
	}
	h.agent.mu.Lock()
	removes := len(h.agent.removes)
	h.agent.mu.Unlock()
	if removes != 0 {
		t.Fatalf("removes = %d, a stopped host is not called", removes)
	}
	source.outside("a", "running")
	h.agent.vacate("a", 0)
	h.step(t, 2*time.Second)
	h.step(t, time.Second)
	if phase := hostStatus(t, h.pool, "a").Phase; phase != string(phaseReady) {
		t.Fatalf("phase after an outside start = %s, want ready", phase)
	}
}

func TestStoppedHostForAnotherImageIsDeletedNotStarted(t *testing.T) {
	h, source := newStopHarness(t, 0, 2, 2)
	h.source.add(Host{ID: "stale", Address: "stale:22", HostKey: "key", State: HostStopped, StateSince: h.clock.Now(), Image: "ghcr.io/acme/runner@sha256:0000"})
	h.source.add(Host{ID: "current", Address: "current:22", HostKey: "key", State: HostStopped, StateSince: h.clock.Now().Add(-time.Minute), Image: testImage})
	h.step(t, time.Second)
	if deleted := h.source.deletedHosts(); len(deleted) != 1 || deleted[0] != "stale" {
		t.Fatalf("deleted = %v, want the stopped host prepared for another image", deleted)
	}
	if _, err := h.pool.Launch(context.Background(), lease("l1")); !provider.IsCapacity(err) {
		t.Fatalf("launch = %v, want capacity", err)
	}
	h.step(t, time.Second)
	if _, starts := source.calls(); len(starts) != 1 || starts[0] != "current" {
		t.Fatalf("starts = %v, want the host on the current image", starts)
	}
}

func TestFailedStopKeepsTheHostAndRetries(t *testing.T) {
	h, source := newStopHarness(t, 0, 2, 1)
	h.readyHost("a", 2, h.clock.Now().Add(-time.Hour))
	source.stopErr = errors.New("machines api unavailable")
	h.step(t, time.Second)
	h.step(t, 6*time.Minute)
	if phase := hostStatus(t, h.pool, "a").Phase; phase != string(phaseReady) {
		t.Fatalf("phase after a failed stop = %s, want ready", phase)
	}
	source.mu.Lock()
	source.stopErr = nil
	source.mu.Unlock()
	h.step(t, 10*time.Second)
	if phase := hostStatus(t, h.pool, "a").Phase; phase != string(phaseStopped) {
		t.Fatalf("phase after the retry = %s, want stopped", phase)
	}
	if len(h.source.deletedHosts()) != 0 {
		t.Fatal("a failed stop must not delete the host")
	}
}

func TestStartedHostThatNeverBootsIsDeleted(t *testing.T) {
	h, source := newStopHarness(t, 0, 2, 1)
	h.source.add(Host{ID: "off", Address: "off:22", HostKey: "key", CreatedAt: h.clock.Now().Add(-5 * time.Hour), State: HostStopped, StateSince: h.clock.Now().Add(-time.Minute)})
	h.step(t, time.Second)
	if _, err := h.pool.Launch(context.Background(), lease("l1")); !provider.IsCapacity(err) {
		t.Fatalf("launch = %v, want capacity", err)
	}
	h.step(t, time.Second)
	if _, starts := source.calls(); len(starts) != 1 {
		t.Fatalf("starts = %v", starts)
	}
	h.step(t, 5*time.Minute)
	if len(h.source.deletedHosts()) != 0 {
		t.Fatal("deleted a started host inside its boot window")
	}
	h.step(t, 6*time.Minute)
	if deleted := h.source.deletedHosts(); len(deleted) != 1 || deleted[0] != "off" {
		t.Fatalf("deleted = %v, want the started host that never answered", deleted)
	}
}

func TestStopperSourceSettingsAreValidated(t *testing.T) {
	base := func() Config {
		return Config{
			Name: "main", ControllerID: "fleet", Image: testImage, MaxHosts: 2, MaxLifetime: time.Hour, Agent: newFakeAgent(),
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
	}
	plain := base()
	plain.Source = &fakeSource{facts: Facts{Billing: BillingPerSecond, CanCreate: true}}
	plain.KeepStopped = 1
	if _, err := New(plain); err == nil {
		t.Fatal("keep_stopped on a source that cannot stop hosts must be rejected")
	}
	tooMany := base()
	tooMany.Source = &stopSource{fakeSource: fakeSource{facts: Facts{Billing: BillingPerSecond, CanCreate: true}}}
	tooMany.KeepStopped = 3
	if _, err := New(tooMany); err == nil {
		t.Fatal("keep_stopped above max_hosts must be rejected")
	}
	valid := base()
	valid.Source = &stopSource{fakeSource: fakeSource{facts: Facts{Billing: BillingPerSecond, CanCreate: true}}}
	valid.KeepStopped = 1
	created, err := New(valid)
	if err != nil {
		t.Fatal(err)
	}
	if created.cfg.IdleStop != defaultIdleReturn || created.cfg.DeleteStoppedAfter != defaultDeleteStopped {
		t.Fatalf("defaults = %s, %s", created.cfg.IdleStop, created.cfg.DeleteStoppedAfter)
	}
}
