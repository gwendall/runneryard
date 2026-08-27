package controller

import (
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
	if err := queue.add("runner-abcdef01"); err != nil {
		t.Fatal(err)
	}
	if err := queue.add("runner-abcdef01"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := newRetirementQueue(file)
	if err != nil {
		t.Fatal(err)
	}
	if names := reloaded.all(); len(names) != 1 || names[0] != "runner-abcdef01" {
		t.Fatalf("reloaded names = %#v", names)
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
	if err := queue.add("production-runner"); err == nil {
		t.Fatal("expected unmanaged runner name rejection")
	}

	outside := filepath.Join(directory, "outside.json")
	if err := os.WriteFile(outside, []byte(`{"version":1,"runners":[]}`), 0o600); err != nil {
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
