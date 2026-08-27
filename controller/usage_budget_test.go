package controller

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestUsageBudgetReservesWorstCaseAndRefundsOnSettlement(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "budget.json")
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	budget, err := initializedUsageBudget(stateFile, 65*time.Minute, 30*24*time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	allowed, _, err := budget.reserve("one", now, time.Hour)
	if err != nil || !allowed {
		t.Fatalf("first reservation = %v, %v", allowed, err)
	}
	if allowed, _, err = budget.reserve("two", now, time.Hour); err != nil || allowed {
		t.Fatalf("budget should be exhausted, got %v, %v", allowed, err)
	}
	if err := budget.settle("one", now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if allowed, _, err = budget.reserve("two", now.Add(5*time.Minute), time.Hour); err != nil || !allowed {
		t.Fatalf("settlement should refund unused reservation, got %v, %v", allowed, err)
	}
}

func TestUsageBudgetSurvivesRestartAndFailsClosedOnCorruption(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "budget.json")
	now := time.Now()
	budget, err := initializedUsageBudget(stateFile, time.Hour, 24*time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, _, err := budget.reserve("one", now, time.Hour); err != nil || !allowed {
		t.Fatalf("reserve = %v, %v", allowed, err)
	}
	reloaded, err := newUsageBudget(time.Hour, 24*time.Hour, stateFile, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, _, err := reloaded.reserve("two", now, time.Hour); err != nil || allowed {
		t.Fatalf("persistent budget should be exhausted, got %v, %v", allowed, err)
	}
	if err := os.WriteFile(stateFile, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newUsageBudget(time.Hour, 24*time.Hour, stateFile, time.Hour); err == nil {
		t.Fatal("corrupt budget must fail closed")
	}
}

func TestUsageBudgetPrunesOnlyCompletedUsage(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "budget.json")
	window := time.Hour
	now := time.Now()
	budget, err := initializedUsageBudget(stateFile, 2*time.Hour, window, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, _, err := budget.reserve("active", now.Add(-2*window), time.Hour); err != nil || !allowed {
		t.Fatal("active reservation failed")
	}
	if allowed, _, err := budget.reserve("done", now.Add(-2*window), time.Hour); err != nil || !allowed {
		t.Fatal("completed reservation failed")
	}
	if err := budget.settle("done", now.Add(-2*window+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if allowed, _, err := budget.reserve("new", now, time.Hour); err != nil || !allowed {
		t.Fatalf("expired completed usage should be pruned, got %v, %v", allowed, err)
	}
}

func TestUsageBudgetKeepsWorstCaseChargeAfterAmbiguousLaunch(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "budget.json")
	now := time.Now()
	budget, err := initializedUsageBudget(stateFile, time.Hour, 24*time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, _, err := budget.reserve("ambiguous", now, time.Hour); err != nil || !allowed {
		t.Fatalf("reserve = %v, %v", allowed, err)
	}
	if err := budget.forfeit("ambiguous", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if allowed, _, err := budget.reserve("next", now.Add(time.Second), time.Hour); err != nil || allowed {
		t.Fatalf("ambiguous create must retain its worst-case charge, got %v, %v", allowed, err)
	}
}

func TestUsageBudgetRefusesMissingStateDuringRecovery(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "budget.json")
	if _, err := newUsageBudget(time.Hour, 24*time.Hour, stateFile, time.Hour); err == nil {
		t.Fatal("missing ledger must fail closed")
	}
}

func TestUsageBudgetInitializationIsAtomicallyExclusive(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "budget.json")
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			results <- InitializeUsageBudget(stateFile)
		}()
	}
	close(start)
	group.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("exclusive initialization successes = %d, want 1", successes)
	}
	if _, err := newUsageBudget(time.Hour, 24*time.Hour, stateFile, time.Hour); err != nil {
		t.Fatalf("winning initialization did not leave a valid ledger: %v", err)
	}
}

func TestUsageBudgetRejectsSemanticallyInvalidLedger(t *testing.T) {
	for name, contents := range map[string]string{
		"negative usage":      `{"version":1,"entries":[{"lease_id":"one","charged_at":"2026-08-27T12:00:00Z","started_at":"2026-08-27T12:00:00Z","seconds":-1,"active":false}]}`,
		"empty lease":         `{"version":1,"entries":[{"lease_id":"","charged_at":"2026-08-27T12:00:00Z","started_at":"2026-08-27T12:00:00Z","seconds":1,"active":false}]}`,
		"duplicate lease":     `{"version":1,"entries":[{"lease_id":"one","charged_at":"2026-08-27T12:00:00Z","started_at":"2026-08-27T12:00:00Z","seconds":1,"active":false},{"lease_id":"one","charged_at":"2026-08-27T12:00:00Z","started_at":"2026-08-27T12:00:00Z","seconds":1,"active":false}]}`,
		"undercharged active": `{"version":1,"entries":[{"lease_id":"one","charged_at":"2026-08-27T12:00:00Z","started_at":"2026-08-27T12:00:00Z","seconds":1,"active":true}]}`,
		"integer overflow":    `{"version":1,"entries":[{"lease_id":"one","charged_at":"2026-08-27T12:00:00Z","started_at":"2026-08-27T12:00:00Z","seconds":9223372036854775807,"active":false}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			stateFile := filepath.Join(t.TempDir(), "budget.json")
			if err := os.WriteFile(stateFile, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := newUsageBudget(time.Hour, 24*time.Hour, stateFile, time.Hour); err == nil {
				t.Fatal("invalid ledger must fail closed")
			}
		})
	}
}

func TestUsageBudgetSettlesMissingAndExpiresOrphanReservations(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "budget.json")
	now := time.Now()
	budget, err := initializedUsageBudget(stateFile, time.Hour, 24*time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, _, err := budget.reserve("missing", now.Add(-time.Minute), time.Hour); err != nil || !allowed {
		t.Fatal("reservation failed")
	}
	if err := budget.adopt("missing", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := budget.reconcile(map[string]struct{}{}, now); err != nil {
		t.Fatal(err)
	}
	if budget.entries[0].Active || budget.entries[0].Seconds > 61 {
		t.Fatalf("missing reservation was not settled: %#v", budget.entries[0])
	}

	expiredFile := filepath.Join(t.TempDir(), "budget.json")
	expired, err := initializedUsageBudget(expiredFile, time.Hour, 24*time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, _, err := expired.reserve("orphan", now.Add(-2*time.Hour), time.Hour); err != nil || !allowed {
		t.Fatal("reservation failed")
	}
	reloaded, err := newUsageBudget(time.Hour, 24*time.Hour, expiredFile, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.entries[0].Active {
		t.Fatal("reservation older than its maximum lifetime must not stay active")
	}
}

func TestUsageBudgetDoesNotRefundUnconfirmedAmbiguousCreate(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "budget.json")
	now := time.Now()
	budget, err := initializedUsageBudget(stateFile, time.Hour, 24*time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, _, err := budget.reserve("ambiguous", now.Add(-time.Minute), time.Hour); err != nil || !allowed {
		t.Fatal("reservation failed")
	}
	if err := budget.reconcile(map[string]struct{}{}, now); err != nil {
		t.Fatal(err)
	}
	if budget.entries[0].Active || budget.entries[0].Seconds != 3600 {
		t.Fatalf("unknown launch outcome must retain worst-case charge: %#v", budget.entries[0])
	}
}

func TestUsageBudgetReactivatesExpiredLeaseStillPresentAtProvider(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "budget.json")
	now := time.Now()
	budget, err := initializedUsageBudget(stateFile, time.Hour, 24*time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	started := now.Add(-2 * time.Hour)
	if allowed, _, err := budget.reserve("still-present", started, time.Hour); err != nil || !allowed {
		t.Fatal("reservation failed")
	}
	reloaded, err := newUsageBudget(time.Hour, 24*time.Hour, stateFile, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.adopt("still-present", started); err != nil {
		t.Fatal(err)
	}
	if len(reloaded.entries) != 1 || !reloaded.entries[0].Active {
		t.Fatalf("expired lease was duplicated instead of reactivated: %#v", reloaded.entries)
	}
	if _, err := newUsageBudget(time.Hour, 24*time.Hour, stateFile, time.Hour); err != nil {
		t.Fatalf("reactivated ledger must survive restart: %v", err)
	}
}

func TestUsageBudgetCapsSettlementAtReservedWorstCase(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "budget.json")
	now := time.Now()
	budget, err := initializedUsageBudget(stateFile, time.Hour, 24*time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, _, err := budget.reserve("slow-delete", now.Add(-2*time.Hour), time.Hour); err != nil || !allowed {
		t.Fatal("reservation failed")
	}
	if err := budget.settle("slow-delete", now); err != nil {
		t.Fatal(err)
	}
	reloaded, err := newUsageBudget(time.Hour, 24*time.Hour, stateFile, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, _, err := reloaded.reserve("next", now, time.Hour); err != nil || allowed {
		t.Fatalf("worst-case charge should exhaust admission, got %v, %v", allowed, err)
	}
}

func initializedUsageBudget(stateFile string, limit, window, reservation time.Duration) (*usageBudget, error) {
	if err := InitializeUsageBudget(stateFile); err != nil {
		return nil, err
	}
	return newUsageBudget(limit, window, stateFile, reservation)
}
