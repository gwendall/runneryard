package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gwendall/runneryard/provider"
)

func conditionKeys(p *Pool) []string {
	keys := make([]string, 0)
	for _, condition := range p.Conditions() {
		keys = append(keys, condition.Key)
	}
	slices.Sort(keys)
	return keys
}

// expectConditions checks the keys of the pool's conditions, and that none
// carries an error text, an address, or a credential.
func expectConditions(t *testing.T, p *Pool, want ...string) []provider.Condition {
	t.Helper()
	slices.Sort(want)
	if got := conditionKeys(p); !slices.Equal(got, want) {
		t.Fatalf("conditions = %v, want %v", got, want)
	}
	conditions := p.Conditions()
	for _, condition := range conditions {
		for _, text := range []string{condition.Summary, condition.Action} {
			if strings.Contains(text, "secret") || strings.Contains(text, ":22") || strings.Contains(text, testImage) {
				t.Fatalf("condition %s leaks %q", condition.Key, text)
			}
		}
		if condition.Summary == "" || condition.Action == "" || condition.Since.IsZero() {
			t.Fatalf("incomplete condition %+v", condition)
		}
	}
	return conditions
}

func condition(t *testing.T, p *Pool, key string) provider.Condition {
	t.Helper()
	for _, condition := range p.Conditions() {
		if condition.Key == key {
			return condition
		}
	}
	t.Fatalf("no condition %s in %v", key, conditionKeys(p))
	return provider.Condition{}
}

func (a *fakeAgent) runs(host, image string, ready bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.info[host] = HostInfo{Version: "test", Slots: 2, Image: image, Ready: ready}
	if a.slots[host] == nil {
		a.slots[host] = map[int]Slot{}
	}
}

func TestNormalHostLifecycleRaisesNoCondition(t *testing.T) {
	h := newHarness(t, growingFacts, 0, 2)
	// A host from an earlier image is drained and returned, as on a rollout.
	h.source.add(Host{ID: "old", Address: "old:22", HostKey: "key", CreatedAt: h.clock.Now().Add(-3 * time.Hour)})
	h.agent.runs("old", "ghcr.io/acme/runner@sha256:0000", true)
	h.step(t, time.Second)
	expectConditions(t, h.pool)

	// A burst overflows while the first host is created and boots.
	for i := range 25 {
		if _, err := h.pool.Launch(context.Background(), lease(fmt.Sprintf("burst-%d", i))); !provider.IsCapacity(err) {
			t.Fatalf("launch on an empty pool = %v, want capacity", err)
		}
	}
	h.step(t, time.Second)
	if h.source.created != 1 {
		t.Fatalf("created = %d, want one host for the burst", h.source.created)
	}
	expectConditions(t, h.pool)
	h.step(t, 3*time.Minute)
	expectConditions(t, h.pool)
	h.agent.ready("new-1", 2)
	h.step(t, time.Second)
	worker, err := h.pool.Launch(context.Background(), lease("l1"))
	if err != nil {
		t.Fatal(err)
	}
	expectConditions(t, h.pool)

	// One slow removal is retried, not reported.
	h.agent.mu.Lock()
	h.agent.removeErr = errors.New("docker is busy")
	h.agent.mu.Unlock()
	if err := h.pool.Destroy(context.Background(), worker.ID); !provider.IsTransient(err) {
		t.Fatalf("destroy = %v, want a transient failure", err)
	}
	expectConditions(t, h.pool)
	h.agent.mu.Lock()
	h.agent.removeErr = nil
	h.agent.mu.Unlock()
	if err := h.pool.Destroy(context.Background(), worker.ID); err != nil {
		t.Fatal(err)
	}

	// The idle host is returned.
	h.step(t, time.Second)
	h.step(t, 3*time.Minute)
	if deleted := sortedDeletes(h.source); deleted != "new-1,old" {
		t.Fatalf("deleted = %s, want the drained and the idle host returned", deleted)
	}
	expectConditions(t, h.pool)
}

func TestStoppingAndStartingAHostRaisesNoCondition(t *testing.T) {
	h, source := newStopHarness(t, 0, 2, 1)
	h.readyHost("a", 2, h.clock.Now().Add(-time.Hour))
	h.step(t, time.Second)
	h.step(t, 6*time.Minute)
	if hostStatus(t, h.pool, "a").Phase != string(phaseStopped) {
		t.Fatal("setup: the idle host should be stopped")
	}
	expectConditions(t, h.pool)
	h.step(t, time.Hour)
	expectConditions(t, h.pool)
	if _, err := h.pool.Launch(context.Background(), lease("l1")); !provider.IsCapacity(err) {
		t.Fatalf("launch with only a stopped host = %v, want capacity", err)
	}
	h.step(t, time.Second)
	if _, starts := source.calls(); len(starts) != 1 {
		t.Fatalf("starts = %v, want the stopped host started", starts)
	}
	expectConditions(t, h.pool)
	h.step(t, time.Second)
	if _, err := h.pool.Launch(context.Background(), lease("l1")); err != nil {
		t.Fatal(err)
	}
	expectConditions(t, h.pool)
}

func TestAFailedStartThatTheNextHostReplacesRaisesNoCondition(t *testing.T) {
	h, source := newStopHarness(t, 0, 2, 1)
	h.source.add(Host{ID: "off", Address: "off:22", HostKey: "key", State: HostStopped, StateSince: h.clock.Now().Add(-time.Hour)})
	h.agent.ready("off", 2)
	h.agent.setUnreachable("off", true)
	source.startErr = &provider.CapacityError{Reason: "host_full", Err: errors.New("insufficient resources")}
	h.step(t, time.Second)
	if _, err := h.pool.Launch(context.Background(), lease("l1")); !provider.IsCapacity(err) {
		t.Fatalf("launch = %v, want capacity", err)
	}
	h.step(t, time.Second)
	h.step(t, time.Second)
	h.agent.ready("new-1", 2)
	h.step(t, time.Second)
	if phase := hostStatus(t, h.pool, "new-1").Phase; phase != string(phaseReady) {
		t.Fatalf("replacement phase = %s, want ready", phase)
	}
	expectConditions(t, h.pool)
}

func TestUnreachableHostIsReportedAfterTheGraceUntilItAnswers(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	h.readyHost("a", 2, time.Time{})
	h.step(t, time.Second)
	answered := h.clock.Now()
	h.agent.setUnreachable("a", true)
	h.step(t, time.Minute)
	expectConditions(t, h.pool)
	h.step(t, 70*time.Second)
	got := condition(t, h.pool, "pool/main/host_unreachable/a")
	if !got.Since.Equal(answered) || got.Summary != "pool main host a has not answered for more than 2m" || strings.Contains(got.Action, "deletes") {
		t.Fatalf("condition = %+v", got)
	}
	expectConditions(t, h.pool, "pool/main/host_unreachable/a")
	if status := h.pool.Status(); len(status.Conditions) != 1 || status.Conditions[0].Key != got.Key {
		t.Fatalf("status conditions = %+v", status.Conditions)
	}
	h.agent.setUnreachable("a", false)
	h.step(t, time.Second)
	expectConditions(t, h.pool)
	if status := h.pool.Status(); status.Conditions != nil {
		t.Fatalf("status conditions after recovery = %+v", status.Conditions)
	}
}

func TestHostSourceFailureIsReportedAfterFiveMinutes(t *testing.T) {
	h := newHarness(t, growingFacts, 1, 2)
	h.readyHost("a", 2, h.clock.Now().Add(-time.Hour))
	h.step(t, time.Second)
	h.source.mu.Lock()
	h.source.listErr = errors.New("401 Unauthorized: token secret-123 is invalid")
	h.source.mu.Unlock()
	h.step(t, time.Second)
	failing := h.clock.Now()
	h.step(t, 4*time.Minute)
	expectConditions(t, h.pool)
	h.step(t, time.Minute)
	if got := condition(t, h.pool, "pool/main/host_source_unavailable"); !got.Since.Equal(failing) {
		t.Fatalf("condition = %+v, want it since the first failure", got)
	}
	expectConditions(t, h.pool, "pool/main/host_source_unavailable")
	h.source.mu.Lock()
	h.source.listErr = nil
	h.source.mu.Unlock()
	h.step(t, time.Second)
	expectConditions(t, h.pool)
}

func TestHostLaunchFailuresAreReportedAfterThreeRounds(t *testing.T) {
	h := newHarness(t, growingFacts, 1, 2)
	h.source.mu.Lock()
	h.source.createErr = errors.New("server limit reached for token secret-123")
	h.source.mu.Unlock()
	h.step(t, time.Second)
	firstFailure := h.clock.Now()
	h.step(t, time.Minute)
	// Passes during the backoff try nothing and count nothing.
	h.step(t, time.Minute)
	expectConditions(t, h.pool)
	h.step(t, time.Minute)
	got := condition(t, h.pool, "pool/main/host_launch_failing")
	if !got.Since.Equal(firstFailure) || got.Summary != "pool main failed to create or start a host 3 times in a row (last: provider_error)" {
		t.Fatalf("condition = %+v", got)
	}
	h.source.mu.Lock()
	h.source.createErr = &provider.CapacityError{Reason: "server_limit", Err: errors.New("limit for token secret-123")}
	h.source.mu.Unlock()
	h.step(t, 4*time.Minute)
	if got := condition(t, h.pool, "pool/main/host_launch_failing"); !strings.HasSuffix(got.Summary, "4 times in a row (last: server_limit)") {
		t.Fatalf("condition = %+v", got)
	}
	expectConditions(t, h.pool, "pool/main/host_launch_failing")
	h.source.mu.Lock()
	h.source.createErr = nil
	h.source.mu.Unlock()
	h.step(t, 8*time.Minute)
	if h.source.created != 1 {
		t.Fatalf("created = %d, want the creation to succeed", h.source.created)
	}
	expectConditions(t, h.pool)
}

func TestHostThatNeverBootsIsReportedUntilAHostIsReady(t *testing.T) {
	h := newHarness(t, growingFacts, 1, 1)
	h.step(t, time.Second)
	h.step(t, 5*time.Minute)
	expectConditions(t, h.pool)
	// The host stays booting while its deletion fails; it is reported once,
	// as a failed boot.
	h.source.mu.Lock()
	h.source.deleteErr = errors.New("api unavailable")
	h.source.mu.Unlock()
	h.step(t, 6*time.Minute)
	got := condition(t, h.pool, "pool/main/host_boot_failed")
	if got.Summary != "pool main host new-1 did not become ready within 10m" {
		t.Fatalf("condition = %+v", got)
	}
	expectConditions(t, h.pool, "pool/main/host_boot_failed")
	h.source.mu.Lock()
	h.source.deleteErr = nil
	h.source.mu.Unlock()
	h.step(t, time.Second)
	if deleted := sortedDeletes(h.source); deleted != "new-1" {
		t.Fatalf("deleted = %s, want the host that never booted", deleted)
	}
	expectConditions(t, h.pool, "pool/main/host_boot_failed")
	h.agent.ready("new-2", 2)
	h.step(t, time.Second)
	expectConditions(t, h.pool)
}

func TestFailedBootClearsOnceThePoolStopsRetrying(t *testing.T) {
	h := newHarness(t, growingFacts, 0, 1)
	full := func(id string) {
		t.Helper()
		if _, err := h.pool.Launch(context.Background(), lease(id)); !provider.IsCapacity(err) {
			t.Fatalf("launch %s = %v, want capacity", id, err)
		}
	}
	bootFailed := func() bool {
		return slices.Contains(conditionKeys(h.pool), "pool/main/host_boot_failed")
	}
	setBootTimeout := func(d time.Duration) {
		h.pool.mu.Lock()
		h.pool.cfg.BootTimeout = d
		h.pool.mu.Unlock()
	}
	// A job finds the pool empty; the host created for it never boots.
	full("l1")
	h.step(t, time.Second)
	h.step(t, 11*time.Minute)
	expectConditions(t, h.pool, "pool/main/host_boot_failed")

	// A host still booting is a retry in progress, however long it takes.
	setBootTimeout(2 * time.Hour)
	full("l2")
	h.step(t, time.Second)
	for range 45 {
		h.step(t, time.Minute)
		if !bootFailed() {
			t.Fatal("the failed boot cleared while a host boots")
		}
	}
	setBootTimeout(10 * time.Minute)
	h.step(t, time.Second)
	if deleted := sortedDeletes(h.source); deleted != "new-1,new-2" {
		t.Fatalf("deleted = %s, want both hosts that never booted", deleted)
	}

	// Jobs that keep finding the pool full keep it reported, even with no
	// host on its way.
	h.source.failCreate(errors.New("quota"))
	for i := range 8 {
		h.step(t, 5*time.Minute)
		full(fmt.Sprintf("again-%d", i))
		if !bootFailed() {
			t.Fatalf("the failed boot cleared while jobs wait, at %d", i)
		}
	}

	// Once nothing is on its way and no job asks for a host for half an
	// hour, the pool has stopped retrying: the report ends.
	h.step(t, bootFailureQuiet-time.Minute)
	if !bootFailed() {
		t.Fatal("the failed boot cleared before the quiet period")
	}
	h.step(t, time.Minute)
	if bootFailed() {
		t.Fatal("the failed boot is still reported after the quiet period")
	}

	// The next host that fails its boot reports it again.
	h.source.failCreate(nil)
	created := h.source.created
	for i := 0; h.source.created == created; i++ {
		if i == 30 {
			t.Fatal("no host created after the creation backoff")
		}
		full(fmt.Sprintf("l3-%d", i))
		h.step(t, time.Minute)
	}
	if bootFailed() {
		t.Fatal("a new host booting reported a failed boot")
	}
	h.step(t, 11*time.Minute)
	if !bootFailed() {
		t.Fatal("the next failed boot is not reported")
	}
}

func TestCreatedHostWithAnotherRunnerImageIsReported(t *testing.T) {
	h := newHarness(t, growingFacts, 1, 1)
	h.step(t, time.Second)
	h.agent.runs("new-1", "ghcr.io/acme/runner@sha256:0000", true)
	h.step(t, time.Second)
	got := condition(t, h.pool, "pool/main/host_boot_failed")
	if got.Summary != "pool main host new-1, created for this pool, runs another runner image" {
		t.Fatalf("condition = %+v", got)
	}
	expectConditions(t, h.pool, "pool/main/host_boot_failed")
}

func TestHostThatKeepsRefusingSlotsIsReported(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	h.readyHost("a", 1, time.Time{})
	h.step(t, time.Second)
	failures := []error{&AgentError{Code: CodeDiskFull}, &AgentError{Code: CodeNotReady}, errors.New("session closed")}
	for i, failure := range failures {
		if i > 0 {
			expectConditions(t, h.pool)
		}
		h.agent.mu.Lock()
		h.agent.startErr["a"] = failure
		h.agent.mu.Unlock()
		h.step(t, hostRefusalBackoff+time.Second)
		if _, err := h.pool.Launch(context.Background(), lease(fmt.Sprintf("l%d", i))); err == nil {
			t.Fatalf("launch %d succeeded on a refusing host", i)
		}
	}
	got := condition(t, h.pool, "pool/main/host_refusing/a")
	if got.Summary != "pool main host a failed to start 3 slots in a row (last: start_unconfirmed)" {
		t.Fatalf("condition = %+v", got)
	}
	expectConditions(t, h.pool, "pool/main/host_refusing/a")
	h.agent.mu.Lock()
	delete(h.agent.startErr, "a")
	h.agent.mu.Unlock()
	h.step(t, hostRefusalBackoff+time.Second)
	if _, err := h.pool.Launch(context.Background(), lease("ok")); err != nil {
		t.Fatal(err)
	}
	expectConditions(t, h.pool)
}

func TestASlotThatAnswersLateResetsTheHostsRefusals(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	h.readyHost("a", 2, time.Time{})
	h.step(t, time.Second)
	attempt := func(id string, failure error, late bool) error {
		h.agent.mu.Lock()
		h.agent.startErr["a"] = failure
		delete(h.agent.late, "a")
		if late {
			h.agent.late["a"] = runningSlot("fleet", id)
		}
		h.agent.mu.Unlock()
		h.step(t, hostRefusalBackoff+time.Second)
		_, err := h.pool.Launch(context.Background(), lease(id))
		return err
	}
	if err := attempt("l1", &AgentError{Code: CodeDiskFull}, false); err == nil {
		t.Fatal("launch on a full disk succeeded")
	}
	if err := attempt("l2", &AgentError{Code: CodeNotReady}, false); err == nil {
		t.Fatal("launch on a host not ready succeeded")
	}
	// A start whose answer was lost but whose runner runs is a launch.
	if err := attempt("l3", errLostAnswer, true); err != nil {
		t.Fatalf("late start = %v, want it kept", err)
	}
	if err := attempt("l4", &AgentError{Code: CodeDiskFull}, false); err == nil {
		t.Fatal("launch on a full disk succeeded")
	}
	expectConditions(t, h.pool)
}

func TestForeignSlotIsReportedWhenItLasts(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	h.readyHost("a", 2, time.Time{})
	h.agent.occupy("a", Slot{Index: 1, ControllerID: "other-fleet", LeaseID: "x", RunnerName: "runner-x", State: "started"})
	h.step(t, time.Second)
	h.step(t, 4*time.Minute)
	expectConditions(t, h.pool)
	h.step(t, time.Minute)
	expectConditions(t, h.pool, "pool/main/foreign_slot/a")
	h.agent.vacate("a", 1)
	h.step(t, time.Second)
	expectConditions(t, h.pool)
}

func TestSustainedOverflowIsReportedOnlyAtTheCeiling(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	h.readyHost("a", 1, time.Time{})
	h.step(t, time.Second)
	if _, err := h.pool.Launch(context.Background(), lease("taken")); err != nil {
		t.Fatal(err)
	}
	for i := range overflowReported - 1 {
		if _, err := h.pool.Launch(context.Background(), lease(fmt.Sprintf("o%d", i))); !provider.IsCapacity(err) {
			t.Fatalf("launch on a full pool = %v, want capacity", err)
		}
	}
	h.step(t, time.Second)
	expectConditions(t, h.pool)
	if _, err := h.pool.Launch(context.Background(), lease("last")); !provider.IsCapacity(err) {
		t.Fatalf("launch on a full pool = %v, want capacity", err)
	}
	h.step(t, time.Second)
	got := condition(t, h.pool, "pool/main/pool_overflowing")
	if got.Summary != "pool main was full for 20 of 21 launches in the last hour" || !strings.Contains(got.Action, "add hosts") {
		t.Fatalf("condition = %+v", got)
	}
	h.step(t, 30*time.Minute)
	expectConditions(t, h.pool, "pool/main/pool_overflowing")
	h.step(t, 31*time.Minute)
	expectConditions(t, h.pool)
}

func TestOverflowBelowHalfTheLaunchesIsNotReported(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	h.readyHost("a", 30, time.Time{})
	h.step(t, time.Second)
	for i := range 30 {
		if _, err := h.pool.Launch(context.Background(), lease(fmt.Sprintf("l%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 29 {
		if _, err := h.pool.Launch(context.Background(), lease(fmt.Sprintf("o%d", i))); !provider.IsCapacity(err) {
			t.Fatalf("launch on a full pool = %v, want capacity", err)
		}
	}
	h.step(t, time.Second)
	expectConditions(t, h.pool)
}

func TestStaticHostsThatNeverServeAreReported(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	h.source.add(Host{ID: "a", Address: "a:22", HostKey: "key"})
	h.agent.runs("a", testImage, false)
	h.source.add(Host{ID: "b", Address: "b:22", HostKey: "key"})
	h.agent.runs("b", "ghcr.io/acme/runner@sha256:0000", true)
	h.step(t, time.Second)
	h.step(t, 10*time.Minute)
	expectConditions(t, h.pool)
	h.step(t, time.Second)
	conditions := expectConditions(t, h.pool, "pool/main/host_not_serving/a", "pool/main/host_not_serving/b")
	if conditions[1].Summary != "pool main static host b takes no job (image_mismatch)" {
		t.Fatalf("conditions = %+v", conditions)
	}
}

func TestPlacementReportsItsPoolsConditions(t *testing.T) {
	h := newHarness(t, ownedFacts, 0, 0)
	h.readyHost("a", 2, time.Time{})
	h.step(t, time.Second)
	h.agent.setUnreachable("a", true)
	h.step(t, 3*time.Minute)
	placement, err := NewPlacement(Tier{Name: "main", Compute: h.pool}, Tier{Name: "ephemeral", Compute: &fakeCompute{}})
	if err != nil {
		t.Fatal(err)
	}
	conditions := placement.Conditions()
	if len(conditions) != 1 || conditions[0].Key != "pool/main/host_unreachable/a" {
		t.Fatalf("placement conditions = %+v", conditions)
	}
	encoded, err := json.Marshal(placement.ComputeStatus())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"conditions":[{"key":"pool/main/host_unreachable/a"`) {
		t.Fatalf("compute status = %s", encoded)
	}
}

func TestUnresolvedImageTagIsReportedAfterFifteenMinutes(t *testing.T) {
	h, _, follower := newFollowHarness(t, 1, 2, false)
	h.imageHost("a", 2, digestOne, h.clock.Now().Add(-time.Hour))
	h.step(t, time.Second)
	expectConditions(t, h.pool)

	// The registry rejects the token after the first resolution: the pool
	// retries every minute and reports it once it lasts.
	follower.set("", errors.New("registry token secret-123 rejected"))
	h.step(t, 5*time.Minute)
	for range 14 {
		h.step(t, time.Minute)
		expectConditions(t, h.pool)
	}
	h.step(t, time.Minute)
	reported := expectConditions(t, h.pool, "pool/main/image_tag_unresolved")
	if !strings.Contains(reported[0].Summary, "cannot resolve its runner image tag") {
		t.Fatalf("summary = %q", reported[0].Summary)
	}
	if host := hostStatus(t, h.pool, "a"); host.Phase != string(phaseReady) || host.Stale {
		t.Fatalf("host = %+v, want it to keep serving its image", host)
	}

	follower.set(digestOne, nil)
	h.step(t, time.Minute)
	expectConditions(t, h.pool)
}

func TestTagMoveRaisesNoCondition(t *testing.T) {
	h, _, follower := newFollowHarness(t, 2, 3, false)
	now := h.clock.Now()
	h.imageHost("a", 2, digestOne, now.Add(-2*time.Hour))
	h.imageHost("b", 2, digestOne, now.Add(-time.Hour))
	h.step(t, time.Second)

	// The tag moves: stale hosts are replaced one at a time, and the new
	// hosts boot on the new digest.
	follower.set(digestTwo, nil)
	for range 6 {
		h.step(t, 5*time.Minute)
		for i := range h.source.created {
			h.agent.readyWith(fmt.Sprintf("new-%d", i+1), 2, digestTwo)
		}
		h.step(t, time.Second)
		expectConditions(t, h.pool)
	}
	if deleted := h.source.deletedHosts(); len(deleted) != 2 || !slices.Contains(deleted, "a") || !slices.Contains(deleted, "b") {
		t.Fatalf("deleted = %v, want both stale hosts replaced", deleted)
	}
}
