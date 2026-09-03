package fly

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gwendall/runneryard/provider"
)

func TestCapacityReasonMapsFlyShortagesAndNothingElse(t *testing.T) {
	cases := []struct {
		status  int
		message string
		want    string
	}{
		{422, `{"error":"Your organization has reached its machine limit. Please contact billing@fly.io"}`, "fly_machine_limit"},
		{422, `{"error":"could not reserve resource for machine: insufficient memory available to fulfill request"}`, "fly_insufficient_resources"},
		{422, `{"error":"insufficient resources to create new machine with existing volume"}`, "fly_insufficient_resources"},
		{412, `{"error":"not enough capacity in region cdg"}`, "fly_insufficient_resources"},
		{422, `{"error":"unable to find a host to place this machine"}`, "fly_placement_unavailable"},
		{409, `{"error":"failed to place machine on a host"}`, "fly_placement_unavailable"},
		{422, `{"error":"unsupported guest cpu kind"}`, ""},
		{422, `{"error":"resources must include a valid image"}`, ""},
		{400, `{"error":"invalid machine config"}`, ""},
		{503, `{"error":"no capacity"}`, ""},
		{429, `{"error":"no capacity"}`, ""},
		{200, `{"error":"machine limit"}`, ""},
	}
	for _, tc := range cases {
		if got := capacityReason(tc.status, tc.message); got != tc.want {
			t.Errorf("capacityReason(%d, %q) = %q, want %q", tc.status, tc.message, got, tc.want)
		}
	}
}

func TestLaunchRegionShortageIsBoundedCapacityNotPermanent(t *testing.T) {
	stub := &flyStub{
		postPlan: []int{422},
		postBody: `{"error":"could not reserve resource for machine: insufficient memory available to fulfill request"}`,
	}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	_, err := retryAdapter(t, server, 3).Launch(context.Background(), testLease())
	if !provider.IsCapacity(err) || provider.IsTransient(err) {
		t.Fatalf("a region shortage needs the bounded-capacity classification: %v", err)
	}
	if provider.CapacityReason(err) != "fly_insufficient_resources" {
		t.Fatalf("unexpected rejection code %q", provider.CapacityReason(err))
	}
	if stub.posts != 1 || stub.gets != 0 {
		t.Fatalf("a shortage must not be retried in the adapter, got posts=%d gets=%d", stub.posts, stub.gets)
	}
}

func TestLaunchValidationFailureStaysPermanentWithoutRawPayload(t *testing.T) {
	stub := &flyStub{postPlan: []int{422}, postBody: `{"error":"unsupported guest cpu kind"}`}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	_, err := retryAdapter(t, server, 3).Launch(context.Background(), testLease())
	if err == nil || provider.IsCapacity(err) || provider.IsTransient(err) {
		t.Fatalf("a validation error must stay permanent: %v", err)
	}
}
