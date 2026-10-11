package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gwendall/runneryard/provider"
)

const testImage = "ghcr.io/acme/runner@sha256:1111"

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type fakeSource struct {
	mu        sync.Mutex
	facts     Facts
	hosts     map[string]Host
	order     []string
	listErr   error
	createErr error
	deleteErr error
	created   int
	deleted   []string
	clock     *testClock
}

func (s *fakeSource) Facts() Facts { return s.facts }

func (s *fakeSource) List(context.Context) ([]Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	hosts := make([]Host, 0, len(s.order))
	for _, id := range s.order {
		if host, ok := s.hosts[id]; ok {
			hosts = append(hosts, host)
		}
	}
	return hosts, nil
}

func (s *fakeSource) add(host Host) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hosts == nil {
		s.hosts = map[string]Host{}
	}
	if _, ok := s.hosts[host.ID]; !ok {
		s.order = append(s.order, host.ID)
	}
	s.hosts[host.ID] = host
}

func (s *fakeSource) Create(context.Context) (Host, error) {
	s.mu.Lock()
	if s.createErr != nil {
		s.mu.Unlock()
		return Host{}, s.createErr
	}
	s.created++
	host := Host{ID: fmt.Sprintf("new-%d", s.created), Address: fmt.Sprintf("10.0.1.%d:22", s.created), HostKey: "key", CreatedAt: s.clock.Now()}
	s.mu.Unlock()
	s.add(host)
	return host, nil
}

func (s *fakeSource) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = append(s.deleted, id)
	delete(s.hosts, id)
	return nil
}

func (s *fakeSource) deletedHosts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...)
}

type startCall struct {
	host    string
	slot    int
	request StartRequest
}

type fakeAgent struct {
	mu          sync.Mutex
	info        map[string]HostInfo
	slots       map[string]map[int]Slot
	unreachable map[string]bool
	startErr    map[string]error
	removeErr   error
	starts      []startCall
	removes     []string
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{
		info: map[string]HostInfo{}, slots: map[string]map[int]Slot{},
		unreachable: map[string]bool{}, startErr: map[string]error{},
	}
}

func (a *fakeAgent) ready(host string, slots int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.info[host] = HostInfo{Version: "test", Slots: slots, Image: testImage, Ready: true}
	if a.slots[host] == nil {
		a.slots[host] = map[int]Slot{}
	}
}

func (a *fakeAgent) setUnreachable(host string, value bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.unreachable[host] = value
}

func (a *fakeAgent) occupy(host string, slot Slot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.slots[host] == nil {
		a.slots[host] = map[int]Slot{}
	}
	a.slots[host][slot.Index] = slot
}

func (a *fakeAgent) vacate(host string, slot int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.slots[host], slot)
}

func (a *fakeAgent) Info(_ context.Context, host Host) (HostInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.unreachable[host.ID] {
		return HostInfo{}, fmt.Errorf("%w: %s", ErrUnreachable, host.ID)
	}
	info, ok := a.info[host.ID]
	if !ok {
		return HostInfo{}, fmt.Errorf("%w: %s still booting", ErrUnreachable, host.ID)
	}
	return info, nil
}

func (a *fakeAgent) List(_ context.Context, host Host) ([]Slot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.unreachable[host.ID] {
		return nil, fmt.Errorf("%w: %s", ErrUnreachable, host.ID)
	}
	if _, ok := a.info[host.ID]; !ok {
		return nil, fmt.Errorf("%w: %s still booting", ErrUnreachable, host.ID)
	}
	slots := make([]Slot, 0, len(a.slots[host.ID]))
	for _, index := range sortedSlots(a.slots[host.ID]) {
		slots = append(slots, a.slots[host.ID][index])
	}
	return slots, nil
}

func (a *fakeAgent) Start(_ context.Context, host Host, slot int, request StartRequest) (Slot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.starts = append(a.starts, startCall{host: host.ID, slot: slot, request: request})
	if a.unreachable[host.ID] {
		return Slot{}, fmt.Errorf("%w: %s", ErrUnreachable, host.ID)
	}
	if err := a.startErr[host.ID]; err != nil {
		return Slot{}, err
	}
	if _, busy := a.slots[host.ID][slot]; busy {
		return Slot{}, &AgentError{Code: CodeSlotBusy}
	}
	started := Slot{
		Index: slot, ControllerID: request.ControllerID, LeaseID: request.LeaseID, RunnerName: request.RunnerName,
		RunnerID: request.RunnerID, RunnerScaleSetID: request.RunnerScaleSetID, State: "started",
	}
	a.slots[host.ID][slot] = started
	return started, nil
}

func (a *fakeAgent) Remove(_ context.Context, host Host, slot int, leaseID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.removes = append(a.removes, fmt.Sprintf("%s/%d/%s", host.ID, slot, leaseID))
	if a.unreachable[host.ID] {
		return fmt.Errorf("%w: %s", ErrUnreachable, host.ID)
	}
	if a.removeErr != nil {
		return a.removeErr
	}
	current, ok := a.slots[host.ID][slot]
	if !ok {
		return nil
	}
	if current.LeaseID != leaseID {
		return &AgentError{Code: CodeLeaseMismatch}
	}
	delete(a.slots[host.ID], slot)
	return nil
}

func (a *fakeAgent) startCalls() []startCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]startCall(nil), a.starts...)
}

type harness struct {
	pool   *Pool
	source *fakeSource
	agent  *fakeAgent
	clock  *testClock
}

func newHarness(t *testing.T, facts Facts, minHosts, maxHosts int) *harness {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)}
	source := &fakeSource{facts: facts, clock: clock}
	agent := newFakeAgent()
	created, err := New(Config{
		Name: "main", ControllerID: "fleet", Image: testImage, MinHosts: minHosts, MaxHosts: maxHosts,
		Source: source, Agent: agent, MaxLifetime: 2 * time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: clock.Now,
		Tick: 10 * time.Second, HostListInterval: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{pool: created, source: source, agent: agent, clock: clock}
}

// step advances the clock and runs one maintenance pass, waiting for the
// host creations it started.
func (h *harness) step(t *testing.T, d time.Duration) {
	t.Helper()
	h.clock.advance(d)
	h.pool.maintain(context.Background())
	h.pool.creations.Wait()
}

func (h *harness) readyHost(id string, slots int, createdAt time.Time) {
	h.source.add(Host{ID: id, Address: id + ":22", HostKey: "key", CreatedAt: createdAt})
	h.agent.ready(id, slots)
}

func lease(id string) provider.Lease {
	return provider.Lease{
		ID: id, RunnerName: "runner-" + id, RunnerID: 7, RunnerScaleSetID: 3, JITConfig: "aml0",
		Deadline: time.Date(2026, 10, 11, 11, 0, 0, 0, time.UTC), IdleTimeout: 10 * time.Minute,
	}
}

var ownedFacts = Facts{Billing: BillingOwned}

func TestLaunchPacksTheBusiestHostThatHasRoom(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	start := h.clock.Now()
	h.readyHost("a", 2, start.Add(-time.Hour))
	h.readyHost("b", 2, start.Add(-2*time.Hour))
	h.agent.occupy("a", Slot{Index: 0, ControllerID: "fleet", LeaseID: "old", RunnerName: "runner-old", State: "started"})
	h.step(t, time.Second)

	worker, err := h.pool.Launch(context.Background(), lease("l1"))
	if err != nil {
		t.Fatal(err)
	}
	if worker.ID != "a/1/l1" || worker.LeaseID != "l1" || worker.RunnerName != "runner-l1" || worker.RunnerID != 7 {
		t.Fatalf("worker = %+v, want slot 1 of the busier host a", worker)
	}
	calls := h.agent.startCalls()
	if len(calls) != 1 || calls[0].request.JITConfig != "aml0" || calls[0].request.Image != testImage || calls[0].request.ControllerID != "fleet" {
		t.Fatalf("start calls = %+v", calls)
	}
	if calls[0].request.IdleTimeoutSeconds != 600 || !calls[0].request.Deadline.Equal(lease("l1").Deadline) {
		t.Fatalf("start request lost the lease limits: %+v", calls[0].request)
	}

	// a is now full; the next lease goes to b.
	worker, err = h.pool.Launch(context.Background(), lease("l2"))
	if err != nil || !strings.HasPrefix(worker.ID, "b/") {
		t.Fatalf("second launch = %+v, %v; want host b", worker, err)
	}
}

func TestLaunchOnAFullPoolReportsCapacityAndGrowsOneHost(t *testing.T) {
	facts := Facts{Billing: BillingPerSecond, HourlyPrice: 0.1, Currency: "EUR", CanCreate: true}
	h := newHarness(t, facts, 0, 2)
	h.step(t, time.Second)

	_, err := h.pool.Launch(context.Background(), lease("l1"))
	var capacity *provider.CapacityError
	if !errors.As(err, &capacity) || capacity.Reason != CapacityReasonFull || !provider.IsCapacity(err) {
		t.Fatalf("launch on an empty pool = %v, want a pool_full capacity error", err)
	}
	if len(h.agent.startCalls()) != 0 {
		t.Fatal("a full pool must not call any host")
	}
	select {
	case <-h.pool.wake:
	default:
		t.Fatal("an overflow must wake the maintenance loop")
	}
	h.step(t, time.Second)
	if h.source.created != 1 {
		t.Fatalf("created %d hosts after one overflow, want 1", h.source.created)
	}
	// A second overflow while the first host is still booting creates nothing.
	_, _ = h.pool.Launch(context.Background(), lease("l2"))
	h.step(t, time.Second)
	if h.source.created != 1 {
		t.Fatalf("created %d hosts while one was booting, want 1", h.source.created)
	}
	// Once it is ready, the lease starts there.
	h.agent.ready("new-1", 2)
	h.step(t, time.Second)
	worker, err := h.pool.Launch(context.Background(), lease("l3"))
	if err != nil || worker.ID != "new-1/0/l3" {
		t.Fatalf("launch after growth = %+v, %v", worker, err)
	}
}

func TestGrowthStopsAtMaxHostsAndBacksOffOnCreateFailure(t *testing.T) {
	facts := Facts{Billing: BillingPerSecond, CanCreate: true}
	h := newHarness(t, facts, 2, 2)
	h.source.createErr = errors.New("quota exceeded")
	h.step(t, time.Second)
	if h.source.created != 0 || h.pool.createBackoff != initialCreateBackoff {
		t.Fatalf("create failure: created=%d backoff=%s", h.source.created, h.pool.createBackoff)
	}
	h.source.mu.Lock()
	h.source.createErr = nil
	h.source.mu.Unlock()
	h.step(t, 30*time.Second)
	if h.source.created != 0 {
		t.Fatal("created a host inside the create backoff")
	}
	h.step(t, time.Minute)
	if h.source.created != 2 {
		t.Fatalf("created %d hosts for min_hosts 2 after the backoff", h.source.created)
	}
	_, _ = h.pool.Launch(context.Background(), lease("l1"))
	h.step(t, time.Second)
	if h.source.created != 2 {
		t.Fatalf("created %d hosts, above max_hosts 2", h.source.created)
	}
}

func TestLaunchSkipsAnUnreachableHost(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	start := h.clock.Now()
	h.readyHost("a", 1, start.Add(-time.Hour))
	h.readyHost("b", 2, start.Add(-time.Hour))
	h.step(t, time.Second)
	h.agent.setUnreachable("a", true)

	worker, err := h.pool.Launch(context.Background(), lease("l1"))
	if err != nil || !strings.HasPrefix(worker.ID, "b/") {
		t.Fatalf("launch = %+v, %v; want host b after a was unreachable", worker, err)
	}
	if calls := h.agent.startCalls(); len(calls) != 2 || calls[0].host != "a" {
		t.Fatalf("start calls = %+v, want a then b", calls)
	}
	// The refused host is skipped for a while, so the next lease does not
	// pay for the failed attempt again.
	if _, err := h.pool.Launch(context.Background(), lease("l2")); err != nil {
		t.Fatal(err)
	}
	if calls := h.agent.startCalls(); len(calls) != 3 || calls[2].host != "b" {
		t.Fatalf("start calls = %+v, want the refused host skipped", calls)
	}
}

func TestAmbiguousStartIsAPartialLaunchAndKeepsTheSlot(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	h.readyHost("a", 2, h.clock.Now().Add(-time.Hour))
	h.step(t, time.Second)
	h.agent.startErr["a"] = errors.New("session closed before the reply")

	_, err := h.pool.Launch(context.Background(), lease("l1"))
	var partial *provider.PartialLaunchError
	if !errors.As(err, &partial) || partial.Worker.ID != "a/0/l1" || partial.Worker.LeaseID != "l1" {
		t.Fatalf("ambiguous start = %v, want a partial launch for a/0/l1", err)
	}
	h.agent.startErr["a"] = nil
	status := h.pool.Status()
	if status.Hosts[0].Busy != 1 {
		t.Fatalf("the ambiguous slot must stay taken: %+v", status.Hosts[0])
	}
	// The core destroys the partial worker; the slot is free again.
	if err := h.pool.Destroy(context.Background(), partial.Worker.ID); err != nil {
		t.Fatal(err)
	}
	if status := h.pool.Status(); status.Hosts[0].Busy != 0 {
		t.Fatalf("slot still busy after destroy: %+v", status.Hosts[0])
	}
}

func TestStartRefusalsFallThroughWithoutPartialLaunch(t *testing.T) {
	for _, code := range []string{CodeNotReady, CodeDiskFull, CodeImageMismatch} {
		t.Run(code, func(t *testing.T) {
			h := newHarness(t, ownedFacts, 0, 0)
			h.readyHost("a", 1, h.clock.Now().Add(-time.Hour))
			h.step(t, time.Second)
			h.agent.startErr["a"] = &AgentError{Code: code}
			_, err := h.pool.Launch(context.Background(), lease("l1"))
			if !provider.IsCapacity(err) {
				t.Fatalf("refusal %s = %v, want the pool reported full", code, err)
			}
			if code == CodeImageMismatch && h.pool.Status().Hosts[0].Phase != string(phaseDraining) {
				t.Fatal("a host running another image must drain")
			}
		})
	}
	h := newHarness(t, ownedFacts, 0, 0)
	h.readyHost("a", 1, h.clock.Now().Add(-time.Hour))
	h.step(t, time.Second)
	h.agent.startErr["a"] = &AgentError{Code: CodeInvalid, Message: "bad runner name"}
	_, err := h.pool.Launch(context.Background(), lease("l1"))
	var partial *provider.PartialLaunchError
	if err == nil || provider.IsCapacity(err) || errors.As(err, &partial) {
		t.Fatalf("invalid lease = %v, want a plain rejection", err)
	}
}

func TestSlotBusyMarksTheSlotAndTriesAnother(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	h.readyHost("a", 2, h.clock.Now().Add(-time.Hour))
	h.step(t, time.Second)
	// A container appears in slot 0 after the last listing.
	h.agent.occupy("a", Slot{Index: 0, ControllerID: "other", LeaseID: "x", RunnerName: "x"})
	worker, err := h.pool.Launch(context.Background(), lease("l1"))
	if err != nil || worker.ID != "a/1/l1" {
		t.Fatalf("launch = %+v, %v; want slot 1 after slot 0 was busy", worker, err)
	}
}

func TestDestroyIsIdempotentAndLeaseScoped(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	h.readyHost("a", 2, h.clock.Now().Add(-time.Hour))
	h.step(t, time.Second)
	worker, err := h.pool.Launch(context.Background(), lease("l1"))
	if err != nil {
		t.Fatal(err)
	}
	// A late destroy for an older lease of the same slot leaves the job alone.
	if err := h.pool.Destroy(context.Background(), "a/0/older"); err != nil {
		t.Fatalf("destroy of a replaced lease = %v, want nil", err)
	}
	if slots, _ := h.agent.List(context.Background(), Host{ID: "a"}); len(slots) != 1 {
		t.Fatal("a stale destroy removed the current job")
	}
	if err := h.pool.Destroy(context.Background(), worker.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.pool.Destroy(context.Background(), worker.ID); err != nil {
		t.Fatalf("second destroy = %v, want nil", err)
	}
	if err := h.pool.Destroy(context.Background(), "gone/0/l1"); err != nil {
		t.Fatalf("destroy on a host that no longer exists = %v, want nil", err)
	}
	if err := h.pool.Destroy(context.Background(), "not-a-worker"); err == nil {
		t.Fatal("malformed worker ID accepted")
	}
	h.agent.setUnreachable("a", true)
	err = h.pool.Destroy(context.Background(), "a/1/l9")
	if !provider.IsTransient(err) {
		t.Fatalf("destroy on an unreachable host = %v, want transient", err)
	}
	h.agent.setUnreachable("a", false)
	h.agent.removeErr = &AgentError{Code: CodeDockerFailed}
	err = h.pool.Destroy(context.Background(), "a/1/l9")
	if !provider.IsTransient(err) {
		t.Fatalf("destroy while the host's Docker fails = %v, want transient", err)
	}
	h.agent.removeErr = &AgentError{Code: CodeInvalid}
	err = h.pool.Destroy(context.Background(), "a/1/l9")
	if err == nil || provider.IsTransient(err) {
		t.Fatalf("destroy refused as invalid = %v, want a plain error", err)
	}
}

func TestDestroyOnAnUnknownHostFailsTransientlyWhenTheSourceIsDown(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	h.readyHost("a", 1, h.clock.Now().Add(-time.Hour))
	h.step(t, time.Second)
	h.source.mu.Lock()
	h.source.listErr = errors.New("api down")
	h.source.mu.Unlock()
	if err := h.pool.Destroy(context.Background(), "b/0/l1"); !provider.IsTransient(err) {
		t.Fatalf("destroy = %v, want transient while the host cannot be confirmed gone", err)
	}
}

func TestInventoryListsOnlyThisControllersSlotsAndToleratesShortOutages(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	h.readyHost("a", 3, h.clock.Now().Add(-time.Hour))
	h.agent.occupy("a", Slot{Index: 0, ControllerID: "fleet", LeaseID: "l0", RunnerName: "runner-l0", State: "started"})
	h.agent.occupy("a", Slot{Index: 1, ControllerID: "other-fleet", LeaseID: "x", RunnerName: "x", State: "started"})
	h.agent.occupy("a", Slot{Index: 2, State: "stopped"})
	h.step(t, time.Second)

	workers, err := h.pool.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 || workers[0].ID != "a/0/l0" || workers[0].State != "started" {
		t.Fatalf("inventory = %+v, want only this controller's slot", workers)
	}

	h.agent.setUnreachable("a", true)
	h.clock.advance(time.Minute)
	if workers, _ := h.pool.Inventory(context.Background()); len(workers) != 1 {
		t.Fatalf("inventory inside the unreachable grace = %+v, want the last listing", workers)
	}
	h.clock.advance(2 * time.Minute)
	if workers, _ := h.pool.Inventory(context.Background()); len(workers) != 0 {
		t.Fatalf("inventory after the unreachable grace = %+v, want nothing", workers)
	}
}

func TestInventoryFailsWhenNoHostWasEverListed(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	h.source.listErr = errors.New("api down")
	if _, err := h.pool.Inventory(context.Background()); err == nil {
		t.Fatal("inventory without any host listing must fail rather than report no workers")
	}
}

func TestPerStartedHourHostIsReturnedAtTheEndOfItsPaidHour(t *testing.T) {
	facts := Facts{Billing: BillingPerStartedHour, HourlyPrice: 0.1114, Currency: "EUR", CanCreate: true}
	h := newHarness(t, facts, 0, 3)
	created := h.clock.Now().Add(-10 * time.Minute)
	h.readyHost("a", 2, created)
	h.step(t, time.Second)
	if _, err := h.pool.Launch(context.Background(), lease("l1")); err != nil {
		t.Fatal(err)
	}
	h.agent.vacate("a", 0)
	h.step(t, 20*time.Minute)
	if len(h.source.deletedHosts()) != 0 {
		t.Fatal("returned a host in the middle of its paid hour")
	}
	h.step(t, created.Add(57*time.Minute+30*time.Second).Sub(h.clock.Now()))
	if len(h.source.deletedHosts()) != 0 {
		t.Fatal("returned a host with more than the margin left in its paid hour")
	}
	// 58m30s after creation: inside the two-minute margin.
	h.step(t, created.Add(58*time.Minute+30*time.Second).Sub(h.clock.Now()))
	if deleted := h.source.deletedHosts(); len(deleted) != 1 || deleted[0] != "a" {
		t.Fatalf("deleted = %v, want host a returned before its next hour", deleted)
	}
	status := h.pool.Status()
	if status.ReturnedHosts != 1 || status.BilledHours != 1 || status.Cost != 0.11 {
		t.Fatalf("status = %+v, want one returned host billed one hour", status)
	}
}

func TestBaseHostsAndBusyHostsAreKept(t *testing.T) {
	facts := Facts{Billing: BillingPerStartedHour, CanCreate: true}
	h := newHarness(t, facts, 1, 3)
	now := h.clock.Now()
	h.readyHost("old", 2, now.Add(-3*time.Hour-10*time.Minute))
	h.readyHost("busy", 2, now.Add(-time.Hour-10*time.Minute))
	h.agent.occupy("busy", Slot{Index: 0, ControllerID: "fleet", LeaseID: "l0", RunnerName: "r"})
	h.step(t, 47*time.Minute+30*time.Second)
	if deleted := h.source.deletedHosts(); len(deleted) != 0 {
		t.Fatalf("deleted = %v, want the base host and the busy host kept", deleted)
	}
}

func TestPerSecondHostIsReturnedAfterIdling(t *testing.T) {
	facts := Facts{Billing: BillingPerSecond, CanCreate: true}
	h := newHarness(t, facts, 0, 2)
	h.readyHost("a", 2, h.clock.Now().Add(-5*time.Minute))
	h.step(t, time.Second)
	h.step(t, time.Minute)
	if len(h.source.deletedHosts()) != 0 {
		t.Fatal("returned a per-second host before its idle period")
	}
	h.step(t, time.Minute+5*time.Second)
	if deleted := h.source.deletedHosts(); len(deleted) != 1 {
		t.Fatalf("deleted = %v, want the idle host returned", deleted)
	}
}

func TestReturnIsAbortedWhenTheFinalListingShowsAJob(t *testing.T) {
	facts := Facts{Billing: BillingPerSecond, CanCreate: true}
	h := newHarness(t, facts, 0, 2)
	h.readyHost("a", 2, h.clock.Now().Add(-5*time.Minute))
	h.step(t, time.Second)
	h.step(t, 3*time.Minute)
	if len(h.source.deletedHosts()) != 1 {
		t.Fatal("setup: the idle host should have been returned")
	}

	h = newHarness(t, facts, 0, 2)
	h.readyHost("a", 2, h.clock.Now().Add(-5*time.Minute))
	h.step(t, time.Second)
	h.clock.advance(3 * time.Minute)
	// The host looks empty from the last listing, but a job lands between it
	// and the return.
	h.pool.listSlots(context.Background(), []Host{{ID: "a", Address: "a:22"}})
	h.agent.occupy("a", Slot{Index: 1, ControllerID: "fleet", LeaseID: "l1", RunnerName: "r"})
	h.pool.returnHosts(context.Background())
	if len(h.source.deletedHosts()) != 0 {
		t.Fatal("returned a host whose final listing showed a job")
	}
	if phase := h.pool.Status().Hosts[0].Phase; phase != string(phaseReady) {
		t.Fatalf("phase after an aborted return = %s, want ready", phase)
	}
}

func TestOwnedHostsAreNeverReturned(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	h.readyHost("a", 2, h.clock.Now().Add(-50*time.Hour))
	h.step(t, time.Second)
	h.step(t, 3*time.Hour)
	if len(h.source.deletedHosts()) != 0 {
		t.Fatal("an owned host was returned")
	}
}

func TestHostThatNeverBootsIsReturnedAfterTheBootTimeout(t *testing.T) {
	facts := Facts{Billing: BillingPerStartedHour, CanCreate: true}
	h := newHarness(t, facts, 1, 2)
	h.step(t, time.Second)
	if h.source.created != 1 {
		t.Fatalf("created %d hosts for min_hosts 1", h.source.created)
	}
	h.step(t, 5*time.Minute)
	if len(h.source.deletedHosts()) != 0 {
		t.Fatal("returned a booting host inside its boot window")
	}
	h.step(t, 6*time.Minute)
	if deleted := h.source.deletedHosts(); len(deleted) != 1 || deleted[0] != "new-1" {
		t.Fatalf("deleted = %v, want the host that never booted", deleted)
	}
}

func TestAdoptedHostThatStaysSilentIsReturnedAfterMaxLifetime(t *testing.T) {
	facts := Facts{Billing: BillingPerStartedHour, CanCreate: true}
	h := newHarness(t, facts, 0, 2)
	// Created long before this controller started: not ours to time out on
	// boot, but nothing silent for longer than a job's lifetime runs a job.
	h.source.add(Host{ID: "old", Address: "old:22", HostKey: "key", CreatedAt: h.clock.Now().Add(-5 * time.Hour)})
	h.step(t, time.Second)
	h.step(t, 2*time.Hour)
	if len(h.source.deletedHosts()) != 0 {
		t.Fatal("returned a silent adopted host before the maximum lifetime and grace")
	}
	h.step(t, 3*time.Minute)
	if deleted := h.source.deletedHosts(); len(deleted) != 1 || deleted[0] != "old" {
		t.Fatalf("deleted = %v, want the silent adopted host", deleted)
	}
}

func TestHostWithAnotherImageDrainsAndIsReplaced(t *testing.T) {
	facts := Facts{Billing: BillingPerSecond, CanCreate: true}
	h := newHarness(t, facts, 1, 2)
	h.source.add(Host{ID: "stale", Address: "stale:22", HostKey: "key", CreatedAt: h.clock.Now().Add(-time.Hour)})
	h.agent.mu.Lock()
	h.agent.info["stale"] = HostInfo{Slots: 2, Image: "ghcr.io/acme/runner@sha256:0000", Ready: true}
	h.agent.slots["stale"] = map[int]Slot{}
	h.agent.mu.Unlock()
	h.step(t, time.Second)
	if _, err := h.pool.Launch(context.Background(), lease("l1")); !provider.IsCapacity(err) {
		t.Fatalf("launch on a draining host = %v, want capacity", err)
	}
	h.step(t, time.Second)
	if deleted := h.source.deletedHosts(); len(deleted) != 1 || deleted[0] != "stale" {
		t.Fatalf("deleted = %v, want the stale host returned once empty", deleted)
	}
	if h.source.created < 1 {
		t.Fatal("min_hosts must be restored with a host on the current image")
	}
}

func TestHostDisappearingFromItsSourceDropsItsSlots(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	h.readyHost("a", 1, h.clock.Now().Add(-time.Hour))
	h.agent.occupy("a", Slot{Index: 0, ControllerID: "fleet", LeaseID: "l0", RunnerName: "r"})
	h.step(t, time.Second)
	h.source.mu.Lock()
	delete(h.source.hosts, "a")
	h.source.mu.Unlock()
	h.clock.advance(2 * time.Second)
	workers, err := h.pool.Inventory(context.Background())
	if err != nil || len(workers) != 0 {
		t.Fatalf("inventory = %+v, %v; want the vanished host's slots gone", workers, err)
	}
}

func TestNewValidatesBoundsAndBilling(t *testing.T) {
	clock := &testClock{now: time.Now()}
	base := func(facts Facts) Config {
		return Config{
			Name: "main", ControllerID: "fleet", Image: testImage, Source: &fakeSource{facts: facts, clock: clock},
			Agent: newFakeAgent(), MaxLifetime: time.Hour, Logger: slog.New(slog.DiscardHandler),
		}
	}
	cases := map[string]func() Config{
		"bad name":                  func() Config { c := base(ownedFacts); c.Name = "Main"; return c },
		"missing image":             func() Config { c := base(ownedFacts); c.Image = ""; return c },
		"unknown billing":           func() Config { return base(Facts{Billing: "monthly"}) },
		"static with bounds":        func() Config { c := base(ownedFacts); c.MaxHosts = 2; return c },
		"creatable without maximum": func() Config { return base(Facts{Billing: BillingPerSecond, CanCreate: true}) },
		"min above max": func() Config {
			c := base(Facts{Billing: BillingPerSecond, CanCreate: true})
			c.MinHosts, c.MaxHosts = 3, 2
			return c
		},
		"no lifetime": func() Config { c := base(ownedFacts); c.MaxLifetime = 0; return c },
		"margin not above tick": func() Config {
			c := base(Facts{Billing: BillingPerStartedHour, CanCreate: true})
			c.MaxHosts, c.Tick, c.HourMargin = 1, time.Minute, time.Minute
			return c
		},
	}
	for name, build := range cases {
		if _, err := New(build()); err == nil {
			t.Errorf("%s: New accepted an invalid configuration", name)
		}
	}
	if _, err := New(base(ownedFacts)); err != nil {
		t.Fatalf("valid static pool refused: %v", err)
	}
}

func TestLaunchRejectsLeasesThatCannotNameASlotWorker(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	bad := lease("a/b")
	if _, err := h.pool.Launch(context.Background(), bad); err == nil || provider.IsCapacity(err) {
		t.Fatalf("launch with a slash in the lease ID = %v, want a rejection", err)
	}
}

func TestWorkerIDRoundTrip(t *testing.T) {
	id := WorkerID("hetzner-42", 3, "lease-1")
	host, slot, leaseID, err := parseWorkerID(id)
	if err != nil || host != "hetzner-42" || slot != 3 || leaseID != "lease-1" {
		t.Fatalf("parse(%q) = %q %d %q %v", id, host, slot, leaseID, err)
	}
	for _, bad := range []string{"", "a/b", "a/-1/l", "a/x/l", "/0/l", "a/0/", "a/0/l/extra"} {
		if _, _, _, err := parseWorkerID(bad); err == nil {
			t.Errorf("parseWorkerID(%q) accepted", bad)
		}
	}
}

func TestRunStopsAndWaitsForCreations(t *testing.T) {
	facts := Facts{Billing: BillingPerSecond, CanCreate: true}
	h := newHarness(t, facts, 1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.pool.Run(ctx) }()
	deadline := time.After(5 * time.Second)
	for {
		h.source.mu.Lock()
		created := h.source.created
		h.source.mu.Unlock()
		if created == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Run did not create the minimum host")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}
