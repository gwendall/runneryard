package controller

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeLedger(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "budget.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoweringMaximumLifetimeKeepsLargerActiveReservations(t *testing.T) {
	started := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
	path := writeLedger(t, `{"version":1,"entries":[{"lease_id":"old","charged_at":"`+started+`","started_at":"`+started+`","seconds":7200,"active":true,"confirmed":true}]}`)
	budget, err := newUsageBudget(100*time.Hour, 30*24*time.Hour, path, time.Hour)
	if err != nil {
		t.Fatalf("a ledger with an older, larger reservation must load after the lifetime is lowered: %v", err)
	}
	if budget.entries[0].Seconds != 7200 {
		t.Fatalf("the older reservation must stay charged at its own value, got %d", budget.entries[0].Seconds)
	}
	if err := budget.adopt("old", time.Now().Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if budget.entries[0].Seconds != 7200 {
		t.Fatalf("adoption must not lower an active reservation, got %d", budget.entries[0].Seconds)
	}
	if err := budget.settle("old", time.Now()); err != nil {
		t.Fatal(err)
	}
	if budget.entries[0].Seconds < 590 || budget.entries[0].Seconds > 610 {
		t.Fatalf("settlement must charge the elapsed lifetime, got %d", budget.entries[0].Seconds)
	}
}

func TestUnderchargedActiveReservationStillFailsClosed(t *testing.T) {
	started := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
	path := writeLedger(t, `{"version":1,"entries":[{"lease_id":"short","charged_at":"`+started+`","started_at":"`+started+`","seconds":1800,"active":true,"confirmed":true}]}`)
	if _, err := newUsageBudget(100*time.Hour, 30*24*time.Hour, path, time.Hour); err == nil {
		t.Fatal("an active reservation smaller than the configured lifetime must be rejected")
	}
}

func TestSettledChargesLargerThanTheLifetimeAreAccepted(t *testing.T) {
	started := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339Nano)
	charged := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	path := writeLedger(t, `{"version":1,"entries":[{"lease_id":"done","charged_at":"`+charged+`","started_at":"`+started+`","seconds":7200,"active":false,"confirmed":true}]}`)
	budget, err := newUsageBudget(100*time.Hour, 30*24*time.Hour, path, time.Hour)
	if err != nil {
		t.Fatalf("settled history from a longer lifetime must load: %v", err)
	}
	if budget.snapshot(time.Now()).UsedSeconds != 7200 {
		t.Fatal("settled history must keep counting against the window")
	}
}
