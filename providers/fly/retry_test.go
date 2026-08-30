package fly

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gwendall/runneryard/provider"
	"github.com/gwendall/runneryard/provider/retry"
)

type flyStub struct {
	mu       sync.Mutex
	posts    int
	gets     int
	deletes  int
	postPlan []int // status per POST attempt; 0 means create succeeds
	postBody string
	getPlan  []int // status per GET attempt; 0 means success
	delPlan  []int // status per DELETE attempt; 0 means success
	machines []machine
	abortGet bool
}

func (s *flyStub) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost:
			s.posts++
			if status := planned(s.postPlan, s.posts); status != 0 {
				body := s.postBody
				if body == "" {
					body = `{"error":"try later"}`
				}
				http.Error(w, body, status)
				return
			}
			var request createMachineRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			created := machine{ID: "machine-new", Name: request.Name, State: "started", CreatedAt: time.Now(), Config: request.Config}
			s.machines = append(s.machines, created)
			_ = json.NewEncoder(w).Encode(created)
		case r.Method == http.MethodGet:
			s.gets++
			if s.abortGet && s.gets == 1 {
				panic(http.ErrAbortHandler)
			}
			if status := planned(s.getPlan, s.gets); status != 0 {
				http.Error(w, `{"error":"try later"}`, status)
				return
			}
			_ = json.NewEncoder(w).Encode(s.machines)
		case r.Method == http.MethodDelete:
			s.deletes++
			if status := planned(s.delPlan, s.deletes); status != 0 {
				http.Error(w, `{"error":"try later"}`, status)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	})
}

func planned(plan []int, attempt int) int {
	if attempt-1 < len(plan) {
		return plan[attempt-1]
	}
	return 0
}

func retryAdapter(t *testing.T, server *httptest.Server, attempts int) *Adapter {
	t.Helper()
	adapter, err := New(Config{
		APIBaseURL: server.URL, APIToken: "fly-token", App: "ci-runners", Region: "cdg",
		Image: "registry.fly.io/ci-runners:test", ControllerID: "test-controller",
		CPUKind: "shared", CPUs: 4, MemoryMB: 8192, RootFSGB: 30, HTTPClient: server.Client(),
		Retry: retry.Policy{Attempts: attempts, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, Rate: 10000, Burst: 10000},
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func ownedMachine(id, leaseID string) machine {
	return machine{ID: id, State: "started", CreatedAt: time.Now(), Config: machineConfig{Metadata: map[string]string{
		managedByKey: "true", controllerIDKey: "test-controller", leaseIDKey: leaseID, runnerNameKey: "runner-" + leaseID,
	}}}
}

func testLease() provider.Lease {
	return provider.Lease{ID: "lease-one", RunnerName: "runner-lease-one", JITConfig: "jit", Deadline: time.Now().Add(time.Hour)}
}

func TestInventoryRetriesThrottlingAndServerErrors(t *testing.T) {
	stub := &flyStub{getPlan: []int{503, 429}, machines: []machine{ownedMachine("machine-a", "lease-a")}}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	workers, err := retryAdapter(t, server, 4).Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 || workers[0].ID != "machine-a" {
		t.Fatalf("unexpected inventory %#v", workers)
	}
	if stub.gets != 3 {
		t.Fatalf("expected 3 list attempts, got %d", stub.gets)
	}
}

func TestInventoryRetriesAbortedConnection(t *testing.T) {
	stub := &flyStub{abortGet: true, machines: []machine{ownedMachine("machine-a", "lease-a")}}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	workers, err := retryAdapter(t, server, 3).Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 || stub.gets != 2 {
		t.Fatalf("expected a second attempt after the aborted connection, got %d attempts and %#v", stub.gets, workers)
	}
}

func TestInventoryReportsExhaustedRetriesAsTransient(t *testing.T) {
	stub := &flyStub{getPlan: []int{503, 503, 503}}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	_, err := retryAdapter(t, server, 3).Inventory(context.Background())
	if !provider.IsTransient(err) {
		t.Fatalf("expected a transient error after exhausting retries, got %v", err)
	}
	if stub.gets != 3 {
		t.Fatalf("expected exactly 3 attempts, got %d", stub.gets)
	}
}

func TestInventoryDoesNotRetryPermanentFailures(t *testing.T) {
	stub := &flyStub{getPlan: []int{401}}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	_, err := retryAdapter(t, server, 3).Inventory(context.Background())
	if err == nil || provider.IsTransient(err) || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected a permanent 401 failure, got %v", err)
	}
	if stub.gets != 1 {
		t.Fatalf("permanent failures must not be retried, got %d attempts", stub.gets)
	}
}

func TestDestroyRetriesThenSucceeds(t *testing.T) {
	stub := &flyStub{delPlan: []int{429, 502}}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	if err := retryAdapter(t, server, 4).Destroy(context.Background(), "machine-a"); err != nil {
		t.Fatal(err)
	}
	if stub.deletes != 3 {
		t.Fatalf("expected 3 delete attempts, got %d", stub.deletes)
	}
}

func TestLaunchRetriesCreateOnlyAfterInventoryProvesAbsence(t *testing.T) {
	stub := &flyStub{postPlan: []int{503}}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	worker, err := retryAdapter(t, server, 3).Launch(context.Background(), testLease())
	if err != nil {
		t.Fatal(err)
	}
	if worker.ID != "machine-new" || worker.LeaseID != "lease-one" {
		t.Fatalf("unexpected worker %#v", worker)
	}
	if stub.posts != 2 || stub.gets != 1 {
		t.Fatalf("expected one inventory check between two creates, got posts=%d gets=%d", stub.posts, stub.gets)
	}
}

func TestLaunchAdoptsMachineCreatedByTheFailedAttempt(t *testing.T) {
	stub := &flyStub{postPlan: []int{503}, machines: []machine{ownedMachine("machine-hidden", "lease-one")}}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	worker, err := retryAdapter(t, server, 3).Launch(context.Background(), testLease())
	if err != nil {
		t.Fatal(err)
	}
	if worker.ID != "machine-hidden" {
		t.Fatalf("expected the existing lease machine, got %#v", worker)
	}
	if stub.posts != 1 {
		t.Fatalf("a second create must never run for a lease that already has a machine, got %d creates", stub.posts)
	}
}

func TestLaunchDoesNotRetryPermanentCreateFailure(t *testing.T) {
	stub := &flyStub{postPlan: []int{422}}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	_, err := retryAdapter(t, server, 3).Launch(context.Background(), testLease())
	if err == nil || provider.IsTransient(err) {
		t.Fatalf("expected a permanent failure, got %v", err)
	}
	if stub.posts != 1 || stub.gets != 0 {
		t.Fatalf("permanent create failures must not be retried or inventoried, got posts=%d gets=%d", stub.posts, stub.gets)
	}
}

func TestLaunchMachineLimitDoesNotEscapeAsPermanent(t *testing.T) {
	stub := &flyStub{
		postPlan: []int{422},
		postBody: `{"error":"Your organization has reached its machine limit"}`,
	}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	_, err := retryAdapter(t, server, 3).Launch(context.Background(), testLease())
	if !provider.IsCapacity(err) || provider.IsTransient(err) {
		t.Fatalf("a provider machine limit needs its bounded-capacity classification: %v", err)
	}
	if stub.posts != 1 || stub.gets != 0 {
		t.Fatalf("a permanent quota must not be retried in the adapter, got posts=%d gets=%d", stub.posts, stub.gets)
	}
}

func TestLaunchExhaustedCreateRetriesStayTransient(t *testing.T) {
	stub := &flyStub{postPlan: []int{503, 503}}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	_, err := retryAdapter(t, server, 2).Launch(context.Background(), testLease())
	if !provider.IsTransient(err) {
		t.Fatalf("expected a transient error, got %v", err)
	}
	if stub.posts != 2 {
		t.Fatalf("expected 2 create attempts, got %d", stub.posts)
	}
}
