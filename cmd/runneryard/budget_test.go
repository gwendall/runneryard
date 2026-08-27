package main

import (
	"path/filepath"
	"testing"
)

func TestBudgetInitIsExplicitAndCannotResetLedger(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "budget.json")
	args := []string{"init", "--file", stateFile}
	if err := runBudget(args); err != nil {
		t.Fatal(err)
	}
	if err := runBudget(args); err == nil {
		t.Fatal("budget init must refuse to reset existing usage")
	}
}
