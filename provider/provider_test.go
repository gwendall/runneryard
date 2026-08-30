package provider

import (
	"errors"
	"fmt"
	"testing"
)

func TestCapacityErrorKeepsAStableProviderNeutralClassification(t *testing.T) {
	err := fmt.Errorf("launch worker: %w", &CapacityError{
		Reason: "fly_machine_limit",
		Err:    errors.New("provider rejected create"),
	})
	if !IsCapacity(err) || IsTransient(err) {
		t.Fatalf("capacity classification = capacity:%t transient:%t", IsCapacity(err), IsTransient(err))
	}
	if reason := CapacityReason(err); reason != "fly_machine_limit" {
		t.Fatalf("capacity reason = %q", reason)
	}
	if reason := CapacityReason(errors.New("other")); reason != "provider_capacity_limit" {
		t.Fatalf("fallback capacity reason = %q", reason)
	}
	unsafe := &CapacityError{Reason: "tenant secret: abc", Err: errors.New("rejected")}
	if reason := CapacityReason(unsafe); reason != "provider_capacity_limit" {
		t.Fatalf("unsafe adapter reason escaped into status: %q", reason)
	}
}
