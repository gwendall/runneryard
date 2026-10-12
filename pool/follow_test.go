package pool

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	followedTag = "registry.example/acme/runner:current"
	digestOne   = "registry.example/acme/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	digestTwo   = "registry.example/acme/runner@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	otherRepo   = "registry.example/acme/other@sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

// fakeFollower resolves the followed tag to whatever the test sets.
type fakeFollower struct {
	mu    sync.Mutex
	image string
	err   error
	calls int
}

func (f *fakeFollower) Resolve(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.image, f.err
}

func (f *fakeFollower) Related(image string) bool {
	return image == followedTag || strings.HasPrefix(image, "registry.example/acme/runner@sha256:")
}

func (f *fakeFollower) set(image string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.image, f.err = image, err
}

func (f *fakeFollower) resolutions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newFollowHarness returns a per-second pool that follows followedTag, over
// a source that stops hosts when stops is set.
func newFollowHarness(t *testing.T, minHosts, maxHosts int, stops bool) (*harness, *stopSource, *fakeFollower) {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
	agent := newFakeAgent()
	source := &stopSource{
		fakeSource: fakeSource{facts: Facts{Billing: BillingPerSecond, HourlyPrice: 0.5, Currency: "USD", CanCreate: true}, clock: clock},
		agent:      agent,
	}
	follower := &fakeFollower{image: digestOne}
	cfg := Config{
		Name: "main", ControllerID: "fleet", Image: followedTag, Follow: follower, MinHosts: minHosts, MaxHosts: maxHosts,
		Source: &source.fakeSource, Agent: agent, MaxLifetime: 2 * time.Hour,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: clock.Now,
		Tick: 10 * time.Second, HostListInterval: time.Second,
	}
	if stops {
		cfg.Source, cfg.IdleStop, cfg.KeepStopped = source, 5*time.Minute, 1
	}
	created, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{pool: created, source: &source.fakeSource, agent: agent, clock: clock}, source, follower
}

// readyWith makes a host answer as ready on image.
func (a *fakeAgent) readyWith(host string, slots int, image string) {
	a.ready(host, slots)
	a.mu.Lock()
	defer a.mu.Unlock()
	info := a.info[host]
	info.Image = image
	a.info[host] = info
}

// imageHost adds a running host prepared for image that answers as ready.
func (h *harness) imageHost(id string, slots int, image string, createdAt time.Time) {
	h.source.add(Host{ID: id, Address: id + ":22", HostKey: "key", CreatedAt: createdAt, Image: image})
	h.agent.readyWith(id, slots, image)
}

func (s *fakeSource) createdImages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.images)
}

func (s *fakeSource) failCreate(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createErr = err
}

func (a *fakeAgent) removeCalls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.removes)
}

func TestFollowedTagCreatesHostsFromTheDigestItNames(t *testing.T) {
	h, _, follower := newFollowHarness(t, 1, 2, false)
	h.step(t, time.Second)
	resolvedAt := h.clock.Now()
	if images := h.source.createdImages(); !slices.Equal(images, []string{digestOne}) {
		t.Fatalf("created from %v, want the resolved digest", images)
	}
	h.agent.readyWith("new-1", 2, digestOne)
	h.step(t, time.Second)
	worker, err := h.pool.Launch(context.Background(), lease("l1"))
	if err != nil || worker.ID != "new-1/0/l1" {
		t.Fatalf("launch = %+v, %v", worker, err)
	}
	if calls := h.agent.startCalls(); len(calls) != 1 || calls[0].request.Image != digestOne {
		t.Fatalf("start calls = %+v, want the host's digest", calls)
	}
	status := h.pool.Status()
	if status.Image != digestOne || status.Follows != followedTag || !status.ImageCheckedAt.Equal(resolvedAt) ||
		status.ImageError != "" || status.ImageReplacements != 0 {
		t.Fatalf("status = %+v", status)
	}
	if host := hostStatus(t, h.pool, "new-1"); host.Image != digestOne || host.Stale {
		t.Fatalf("host status = %+v", host)
	}
	// The tag is resolved again every ImageRefresh, not on every pass.
	h.step(t, 4*time.Minute)
	if got := follower.resolutions(); got != 1 {
		t.Fatalf("resolved the tag %d times within the refresh interval, want 1", got)
	}
	h.step(t, time.Minute)
	if got := follower.resolutions(); got != 2 {
		t.Fatalf("resolved the tag %d times after the refresh interval, want 2", got)
	}
}

func TestFollowedTagServesFromTheTagUntilItResolves(t *testing.T) {
	h, _, follower := newFollowHarness(t, 2, 3, false)
	follower.set("", errors.New("registry unavailable"))
	h.imageHost("old", 2, digestOne, h.clock.Now().Add(-time.Hour))
	h.step(t, time.Second)
	if images := h.source.createdImages(); !slices.Equal(images, []string{followedTag}) {
		t.Fatalf("created from %v before the first resolution, want the tag", images)
	}
	worker, err := h.pool.Launch(context.Background(), lease("l1"))
	if err != nil || worker.ID != "old/0/l1" {
		t.Fatalf("launch = %+v, %v; a host on a digest of the tag must serve", worker, err)
	}
	if calls := h.agent.startCalls(); len(calls) != 1 || calls[0].request.Image != digestOne {
		t.Fatalf("start calls = %+v, want the host's own image", calls)
	}
	status := h.pool.Status()
	if status.Image != followedTag || !strings.Contains(status.ImageError, "registry unavailable") || !status.ImageCheckedAt.IsZero() {
		t.Fatalf("status = %+v", status)
	}
	if host := hostStatus(t, h.pool, "old"); host.Stale {
		t.Fatal("a host is stale before the tag was ever resolved")
	}
	// A failed resolution is retried after a minute.
	h.step(t, 30*time.Second)
	if got := follower.resolutions(); got != 1 {
		t.Fatalf("resolved %d times inside the retry delay, want 1", got)
	}
	h.step(t, 31*time.Second)
	if got := follower.resolutions(); got != 2 {
		t.Fatalf("resolved %d times after the retry delay, want 2", got)
	}
	follower.set(digestTwo, nil)
	h.step(t, time.Minute)
	status = h.pool.Status()
	if status.Image != digestTwo || status.ImageError != "" {
		t.Fatalf("status after the tag resolved = %+v", status)
	}
	// The host on the earlier digest is stale but waits: a host is booting.
	if host := hostStatus(t, h.pool, "old"); !host.Stale || host.Phase != string(phaseReady) {
		t.Fatalf("old host = %+v, want ready and stale", host)
	}
}

func TestFollowedTagIgnoresAResolutionOutsideItsRepository(t *testing.T) {
	for _, resolved := range []string{followedTag, otherRepo} {
		h, _, follower := newFollowHarness(t, 1, 1, false)
		follower.set(resolved, nil)
		h.step(t, time.Second)
		status := h.pool.Status()
		if status.Image != followedTag || status.ImageError == "" {
			t.Fatalf("resolved %s: status = %+v, want the tag kept and an error", resolved, status)
		}
		if images := h.source.createdImages(); !slices.Equal(images, []string{followedTag}) {
			t.Fatalf("resolved %s: created from %v", resolved, images)
		}
	}
}

func TestTagMoveReplacesOneHostAtATimeWithoutStoppingJobs(t *testing.T) {
	h, _, follower := newFollowHarness(t, 2, 3, false)
	now := h.clock.Now()
	h.imageHost("a", 4, digestOne, now.Add(-2*time.Hour))
	h.imageHost("b", 2, digestOne, now.Add(-time.Hour))
	h.agent.occupy("a", Slot{Index: 0, ControllerID: "fleet", LeaseID: "l0", RunnerName: "r"})
	h.step(t, time.Second)
	if deleted := h.source.deletedHosts(); len(deleted) != 0 || h.source.created != 0 {
		t.Fatalf("deleted %v, created %d before the tag moved", deleted, h.source.created)
	}

	// The tag moves: the idle host goes first and is replaced from the new
	// digest; the busy one keeps its job and keeps serving.
	follower.set(digestTwo, nil)
	h.step(t, 5*time.Minute)
	if deleted := h.source.deletedHosts(); !slices.Equal(deleted, []string{"b"}) {
		t.Fatalf("deleted = %v, want the idle stale host", deleted)
	}
	if images := h.source.createdImages(); !slices.Equal(images, []string{digestTwo}) {
		t.Fatalf("created from %v, want the new digest", images)
	}
	if host := hostStatus(t, h.pool, "a"); host.Phase != string(phaseReady) || !host.Stale || host.Image != digestOne {
		t.Fatalf("busy stale host = %+v, want ready and stale", host)
	}
	worker, err := h.pool.Launch(context.Background(), lease("l1"))
	if err != nil || worker.ID != "a/1/l1" {
		t.Fatalf("launch while the replacement boots = %+v, %v", worker, err)
	}

	// With a host on the new digest ready, it takes jobs first; the stale
	// host waits for the replacement interval.
	h.agent.readyWith("new-1", 2, digestTwo)
	h.step(t, time.Second)
	if host := hostStatus(t, h.pool, "a"); host.Phase != string(phaseReady) {
		t.Fatalf("stale host = %+v, replaced inside the replacement interval", host)
	}
	worker, err = h.pool.Launch(context.Background(), lease("l2"))
	if err != nil || worker.ID != "new-1/0/l2" {
		t.Fatalf("launch = %+v, %v; want the host on the new digest", worker, err)
	}
	calls := h.agent.startCalls()
	if len(calls) != 2 || calls[0].request.Image != digestOne || calls[1].request.Image != digestTwo {
		t.Fatalf("start calls = %+v, want each host's own image", calls)
	}

	// After the interval the busy host drains: no new job, no removal.
	h.step(t, 5*time.Minute)
	if host := hostStatus(t, h.pool, "a"); host.Phase != string(phaseDraining) || host.Busy != 2 {
		t.Fatalf("busy stale host = %+v, want draining with its jobs", host)
	}
	worker, err = h.pool.Launch(context.Background(), lease("l3"))
	if err != nil || !strings.HasPrefix(worker.ID, "new-1/") {
		t.Fatalf("launch = %+v, %v; a draining host takes no job", worker, err)
	}
	h.agent.vacate("a", 0)
	h.agent.vacate("a", 1)
	h.step(t, time.Second)
	if deleted := h.source.deletedHosts(); !slices.Equal(deleted, []string{"b", "a"}) {
		t.Fatalf("deleted = %v, want the drained host once empty", deleted)
	}
	if removes := h.agent.removeCalls(); len(removes) != 0 {
		t.Fatalf("removed slots %v: a replacement must not interrupt a job", removes)
	}
	if status := h.pool.Status(); status.ImageReplacements != 2 {
		t.Fatalf("image replacements = %d, want 2", status.ImageReplacements)
	}
	for _, image := range h.source.createdImages() {
		if image != digestTwo {
			t.Fatalf("created from %v, want only the new digest", h.source.createdImages())
		}
	}
}

func TestReplacementWaitsForTheDrainingHost(t *testing.T) {
	h, _, follower := newFollowHarness(t, 2, 3, false)
	now := h.clock.Now()
	h.imageHost("a", 2, digestOne, now.Add(-2*time.Hour))
	h.imageHost("b", 2, digestOne, now.Add(-time.Hour))
	h.agent.occupy("a", Slot{Index: 0, ControllerID: "fleet", LeaseID: "la", RunnerName: "r"})
	h.agent.occupy("b", Slot{Index: 0, ControllerID: "fleet", LeaseID: "lb", RunnerName: "r"})
	h.step(t, time.Second)

	// The tag moves: the older busy host drains and its replacement comes up.
	follower.set(digestTwo, nil)
	h.step(t, 5*time.Minute)
	if host := hostStatus(t, h.pool, "a"); host.Phase != string(phaseDraining) {
		t.Fatalf("oldest stale host = %+v, want draining", host)
	}
	h.agent.readyWith("new-1", 2, digestTwo)
	h.step(t, time.Second)

	// The interval passes, but the drained host still runs its job: the next
	// stale host keeps serving.
	h.step(t, 5*time.Minute)
	if host := hostStatus(t, h.pool, "b"); host.Phase != string(phaseReady) {
		t.Fatalf("second stale host = %+v, replaced while another drains", host)
	}
	if status := h.pool.Status(); status.ImageReplacements != 1 {
		t.Fatalf("image replacements = %d, want 1 while a host drains", status.ImageReplacements)
	}

	// Once the drained host is deleted, the next one goes.
	h.agent.vacate("a", 0)
	h.step(t, time.Second)
	if deleted := h.source.deletedHosts(); !slices.Equal(deleted, []string{"a"}) {
		t.Fatalf("deleted = %v, want the drained host", deleted)
	}
	h.step(t, time.Second)
	if host := hostStatus(t, h.pool, "b"); host.Phase != string(phaseDraining) {
		t.Fatalf("second stale host = %+v, want draining once the first is gone", host)
	}
	if removes := h.agent.removeCalls(); len(removes) != 0 {
		t.Fatalf("removed slots %v: a replacement must not interrupt a job", removes)
	}
}

func TestStaleHostsAreDeletedRatherThanStopped(t *testing.T) {
	h, source, follower := newFollowHarness(t, 0, 3, true)
	now := h.clock.Now()
	h.imageHost("a", 2, digestOne, now.Add(-2*time.Hour))
	h.imageHost("b", 2, digestOne, now.Add(-time.Hour))
	h.source.add(Host{ID: "s", Address: "s:22", HostKey: "key", CreatedAt: now.Add(-3 * time.Hour), State: HostStopped, StateSince: now.Add(-10 * time.Minute), Image: digestOne})
	h.agent.setUnreachable("s", true)
	h.step(t, time.Second)
	if deleted := h.source.deletedHosts(); len(deleted) != 0 {
		t.Fatalf("deleted %v while every host is on the current digest", deleted)
	}

	// After the move the stopped host is deleted, not kept or started, and
	// the idle hosts are deleted, not stopped: one replaced, the other
	// returned for idling.
	follower.set(digestTwo, nil)
	h.step(t, 5*time.Minute)
	if deleted := sortedDeletes(h.source); deleted != "a,b,s" {
		t.Fatalf("deleted = %s, want every stale host", deleted)
	}
	if stops, starts := source.calls(); len(stops) != 0 || len(starts) != 0 {
		t.Fatalf("stops = %v, starts = %v; a stale host is never kept", stops, starts)
	}
}

func TestHostOfAnotherRepositoryDrainsAtOnce(t *testing.T) {
	h, _, _ := newFollowHarness(t, 0, 3, false)
	now := h.clock.Now()
	h.imageHost("x", 2, otherRepo, now.Add(-time.Hour))
	h.imageHost("y", 2, otherRepo, now.Add(-time.Hour))
	h.imageHost("z", 2, digestOne, now.Add(-time.Hour))
	h.agent.occupy("x", Slot{Index: 0, ControllerID: "fleet", LeaseID: "l0", RunnerName: "r"})
	h.step(t, time.Second)
	if host := hostStatus(t, h.pool, "x"); host.Phase != string(phaseDraining) || host.Stale {
		t.Fatalf("busy host of another repository = %+v, want draining", host)
	}
	if deleted := h.source.deletedHosts(); !slices.Equal(deleted, []string{"y"}) {
		t.Fatalf("deleted = %v, want the empty host of another repository", deleted)
	}
	worker, err := h.pool.Launch(context.Background(), lease("l1"))
	if err != nil || !strings.HasPrefix(worker.ID, "z/") {
		t.Fatalf("launch = %+v, %v; want the host on the followed tag", worker, err)
	}
	if status := h.pool.Status(); status.ImageReplacements != 0 {
		t.Fatalf("image replacements = %d; another repository is a mismatch", status.ImageReplacements)
	}
}

func TestFailedCreationFromTheNewDigestHoldsFurtherReplacements(t *testing.T) {
	h, _, follower := newFollowHarness(t, 2, 2, false)
	now := h.clock.Now()
	h.imageHost("a", 2, digestOne, now.Add(-2*time.Hour))
	h.imageHost("b", 2, digestOne, now.Add(-time.Hour))
	h.step(t, time.Second)

	follower.set(digestTwo, nil)
	h.source.failCreate(errors.New("image not found"))
	h.step(t, 5*time.Minute)
	h.step(t, time.Second)
	if deleted := h.source.deletedHosts(); !slices.Equal(deleted, []string{"a"}) {
		t.Fatalf("deleted = %v, want the older idle stale host", deleted)
	}
	if images := h.source.createdImages(); len(images) == 0 || images[0] != digestTwo {
		t.Fatalf("create attempts = %v, want the new digest", images)
	}
	// Past the replacement interval, with no host on the new digest ready,
	// the last host on the earlier digest keeps serving.
	h.step(t, 5*time.Minute)
	h.step(t, 5*time.Minute)
	if host := hostStatus(t, h.pool, "b"); host.Phase != string(phaseReady) {
		t.Fatalf("stale host = %+v, replaced although the new digest never served", host)
	}
	if worker, err := h.pool.Launch(context.Background(), lease("l1")); err != nil || !strings.HasPrefix(worker.ID, "b/") {
		t.Fatalf("launch = %+v, %v", worker, err)
	}
	h.agent.vacate("b", 0)

	// Once a host on the new digest is ready, replacement resumes.
	h.source.failCreate(nil)
	h.step(t, 15*time.Minute)
	created := h.source.created
	if created != 1 {
		t.Fatalf("created %d hosts after the failures, want 1", created)
	}
	h.agent.readyWith("new-1", 2, digestTwo)
	h.step(t, time.Second)
	if deleted := h.source.deletedHosts(); !slices.Equal(deleted, []string{"a", "b"}) {
		t.Fatalf("deleted = %v, want the last stale host replaced", deleted)
	}
}

func TestPinnedPoolNeitherFollowsNorMarksHostsStale(t *testing.T) {
	h := newHarness(t, Facts{Billing: BillingPerSecond, CanCreate: true}, 1, 2)
	h.step(t, time.Second)
	if images := h.source.createdImages(); !slices.Equal(images, []string{testImage}) {
		t.Fatalf("created from %v, want the pinned image", images)
	}
	h.agent.ready("new-1", 1)
	h.step(t, time.Second)
	status := h.pool.Status()
	if status.Image != testImage || status.Follows != "" || !status.ImageCheckedAt.IsZero() || status.ImageError != "" {
		t.Fatalf("status = %+v", status)
	}
	if host := hostStatus(t, h.pool, "new-1"); host.Image != testImage || host.Stale {
		t.Fatalf("host status = %+v", host)
	}
}

func TestNewRefusesToFollowATagOnASourceThatCannotCreateHosts(t *testing.T) {
	_, err := New(Config{
		Name: "main", ControllerID: "fleet", Image: followedTag, Follow: &fakeFollower{},
		Source: &fakeSource{facts: ownedFacts, clock: &testClock{}}, Agent: newFakeAgent(),
		MaxLifetime: time.Hour, Logger: slog.New(slog.DiscardHandler),
	})
	if err == nil || !strings.Contains(err.Error(), "cannot follow an image tag") {
		t.Fatalf("New = %v, want a refusal", err)
	}
}
