package controller

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRetirementQueuePersistsIdempotently(t *testing.T) {
	file := filepath.Join(t.TempDir(), "retirements.json")
	queue, err := newRetirementQueue(file)
	if err != nil {
		t.Fatal(err)
	}
	entry := retirementEntry{RunnerName: "runner-abcdef01", RunnerID: 42, RunnerScaleSetID: 1, LeaseID: "lease-one", BudgetDisposition: settleActualUsage}
	if err := queue.put(entry); err != nil {
		t.Fatal(err)
	}
	if err := queue.put(entry); err != nil {
		t.Fatal(err)
	}
	reloaded, err := newRetirementQueue(file)
	if err != nil {
		t.Fatal(err)
	}
	if entries := reloaded.all(); len(entries) != 1 || entries[0] != entry {
		t.Fatalf("reloaded entries = %#v", entries)
	}
	if err := reloaded.remove("runner-abcdef01"); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.remove("runner-abcdef01"); err != nil {
		t.Fatal(err)
	}
	if reloaded.count() != 0 {
		t.Fatalf("pending retirements = %d, want 0", reloaded.count())
	}
}

func TestRetirementQueueRejectsUnmanagedNamesAndUnsafeFiles(t *testing.T) {
	directory := t.TempDir()
	queue, err := newRetirementQueue(filepath.Join(directory, "retirements.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.put(retirementEntry{RunnerName: "production-runner", RunnerScaleSetID: 1, LeaseID: "lease-one", BudgetDisposition: settleActualUsage}); err == nil {
		t.Fatal("expected unmanaged runner name rejection")
	}

	outside := filepath.Join(directory, "outside.json")
	if err := os.WriteFile(outside, []byte(`{"version":1,"entries":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(directory, "linked.json")
	if err := os.Symlink(outside, linked); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := newRetirementQueue(linked); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func TestRetirementQueueRefusesIdentityMutation(t *testing.T) {
	queue, err := newRetirementQueue(filepath.Join(t.TempDir(), "retirements.json"))
	if err != nil {
		t.Fatal(err)
	}
	entry := retirementEntry{
		RunnerName: "runner-abcdef01", RunnerID: 42, RunnerScaleSetID: 1,
		LeaseID: "lease-one", BudgetDisposition: forfeitReservation,
	}
	if err := queue.put(entry); err != nil {
		t.Fatal(err)
	}
	mutated := entry
	mutated.RunnerID = 43
	if err := queue.put(mutated); err == nil {
		t.Fatal("expected registration identity mutation to be rejected")
	}
	mutated = entry
	mutated.LeaseID = "lease-two"
	if err := queue.put(mutated); err == nil {
		t.Fatal("expected lease identity mutation to be rejected")
	}
}

func TestRetirementQueueMigratesEmptyLegacyLedgerAndRejectsNonEmptyOne(t *testing.T) {
	directory := t.TempDir()
	empty := filepath.Join(directory, "empty.json")
	if err := os.WriteFile(empty, []byte("{\"version\":1,\"runners\":[]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	queue, err := newRetirementQueue(empty)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.persist(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(empty)
	if err != nil {
		t.Fatal(err)
	}
	var migrated retirementLedger
	if err := json.Unmarshal(data, &migrated); err != nil {
		t.Fatal(err)
	}
	if migrated.Version != 2 || len(migrated.Entries) != 0 {
		t.Fatalf("legacy ledger was not migrated: %s", data)
	}

	nonEmpty := filepath.Join(directory, "non-empty.json")
	if err := os.WriteFile(nonEmpty, []byte("{\"version\":1,\"runners\":[\"runner-abcdef01\"]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newRetirementQueue(nonEmpty); err == nil {
		t.Fatal("expected non-empty legacy ledger to fail closed")
	}
}
