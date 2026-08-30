package hetzner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gwendall/runneryard/provider"
	"github.com/gwendall/runneryard/provider/retry"
)

type hetznerStub struct {
	mu       sync.Mutex
	creates  int
	lists    int
	deletes  int
	listPlan []int
	crtPlan  []int
	delPlan  []int
	servers  []server
}

func ownedServer(id int64, leaseID string) server {
	item := server{ID: id, Name: "runner-" + leaseID, Status: "running", CreatedAt: time.Now(), Labels: map[string]string{
		managedByKey: "true", controllerIDKey: "test-controller", leaseIDKey: leaseID, runnerNameKey: "runner-" + leaseID,
	}}
	item.PublicNet.Firewalls = []serverFirewall{{ID: 42, Status: "applied"}}
	return item
}

func (s *hetznerStub) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/servers":
			s.creates++
			if status := plannedStatus(s.crtPlan, s.creates); status != 0 {
				http.Error(w, `{"error":{"code":"rate_limit_exceeded"}}`, status)
				return
			}
			var request createServerRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			created := ownedServer(int64(100+s.creates), request.Labels[leaseIDKey])
			created.Name = request.Name
			created.Status = "off"
			s.servers = append(s.servers, created)
			_ = json.NewEncoder(w).Encode(createServerResponse{Server: created, Action: action{ID: 1, Status: "success"}})
		case r.Method == http.MethodGet && r.URL.Path == "/servers":
			s.lists++
			if status := plannedStatus(s.listPlan, s.lists); status != 0 {
				http.Error(w, `{"error":{"code":"unavailable"}}`, status)
				return
			}
			selector := r.URL.Query().Get("label_selector")
			var matched []server
			for _, item := range s.servers {
				if lease, ok := selectorLease(selector); ok && item.Labels[leaseIDKey] != lease {
					continue
				}
				matched = append(matched, item)
			}
			_ = json.NewEncoder(w).Encode(serverListResponse{Servers: matched})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/servers/"):
			for _, item := range s.servers {
				if strings.HasSuffix(r.URL.Path, "/"+strconv.FormatInt(item.ID, 10)) {
					item.Status = "running"
					_ = json.NewEncoder(w).Encode(serverResponse{Server: item})
					return
				}
			}
			http.NotFound(w, r)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions/poweron"):
			_ = json.NewEncoder(w).Encode(actionResponse{Action: action{ID: 2, Status: "success"}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/actions/"):
			_ = json.NewEncoder(w).Encode(actionResponse{Action: action{ID: 1, Status: "success"}})
		case r.Method == http.MethodDelete:
			s.deletes++
			if status := plannedStatus(s.delPlan, s.deletes); status != 0 {
				http.Error(w, `{"error":{"code":"unavailable"}}`, status)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
}

func selectorLease(selector string) (string, bool) {
	for _, part := range strings.Split(selector, ",") {
		if value, ok := strings.CutPrefix(part, leaseIDKey+"="); ok {
			return value, true
		}
	}
	return "", false
}

func plannedStatus(plan []int, attempt int) int {
	if attempt-1 < len(plan) {
		return plan[attempt-1]
	}
	return 0
}

func retryAdapter(t *testing.T, server *httptest.Server, attempts int) *Adapter {
	t.Helper()
	adapter, err := New(Config{
		APIBaseURL: server.URL, APIToken: "hcloud-token", Location: "fsn1", ServerType: "cpx32",
		ServerImage: "docker-ce", RunnerImage: "ghcr.io/gwendall/runneryard:test",
		ControllerID: "test-controller", FirewallID: 42, HTTPClient: server.Client(),
		ActionPollInterval: time.Millisecond, ActionTimeout: time.Second,
		Retry: retry.Policy{Attempts: attempts, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, Rate: 10000, Burst: 10000},
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func TestHetznerInventoryRetriesTransientFailures(t *testing.T) {
	stub := &hetznerStub{listPlan: []int{503, 429}, servers: []server{ownedServer(1, "lease-a")}}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	workers, err := retryAdapter(t, srv, 4).Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 || workers[0].ID != "1" || stub.lists != 3 {
		t.Fatalf("expected the server after two retries, got %#v after %d attempts", workers, stub.lists)
	}
}

func TestHetznerInventoryExhaustedRetriesStayTransient(t *testing.T) {
	stub := &hetznerStub{listPlan: []int{500, 500}}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	_, err := retryAdapter(t, srv, 2).Inventory(context.Background())
	if !provider.IsTransient(err) || stub.lists != 2 {
		t.Fatalf("expected a transient error after 2 attempts, got %v after %d", err, stub.lists)
	}
}

func TestHetznerDestroyRetriesTransientFailures(t *testing.T) {
	stub := &hetznerStub{delPlan: []int{503}}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	if err := retryAdapter(t, srv, 3).Destroy(context.Background(), "7"); err != nil {
		t.Fatal(err)
	}
	if stub.deletes != 2 {
		t.Fatalf("expected 2 delete attempts, got %d", stub.deletes)
	}
}

func TestHetznerLaunchRetriesCreateAfterLeaseLookup(t *testing.T) {
	stub := &hetznerStub{crtPlan: []int{503}}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	worker, err := retryAdapter(t, srv, 3).Launch(context.Background(), provider.Lease{
		ID: "lease-one", RunnerName: "runner-lease-one", JITConfig: "jit", Deadline: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if worker.LeaseID != "lease-one" || stub.creates != 2 || stub.lists != 1 {
		t.Fatalf("expected one lease lookup between two creates, got worker=%#v creates=%d lists=%d", worker, stub.creates, stub.lists)
	}
}

func TestHetznerLaunchReportsPartialWhenLeaseAlreadyHasServer(t *testing.T) {
	stub := &hetznerStub{crtPlan: []int{503}, servers: []server{ownedServer(9, "lease-one")}}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	_, err := retryAdapter(t, srv, 3).Launch(context.Background(), provider.Lease{
		ID: "lease-one", RunnerName: "runner-lease-one", JITConfig: "jit", Deadline: time.Now().Add(time.Hour),
	})
	var partial *provider.PartialLaunchError
	if err == nil || !errors.As(err, &partial) || partial.Worker.ID != "9" {
		t.Fatalf("expected a partial launch pointing at the existing server, got %v", err)
	}
	if stub.creates != 1 {
		t.Fatalf("a second create must never run for a lease that already has a server, got %d", stub.creates)
	}
}

func TestHetznerLaunchDoesNotRetryPermanentFailure(t *testing.T) {
	stub := &hetznerStub{crtPlan: []int{422}}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	_, err := retryAdapter(t, srv, 3).Launch(context.Background(), provider.Lease{
		ID: "lease-one", RunnerName: "runner-lease-one", JITConfig: "jit", Deadline: time.Now().Add(time.Hour),
	})
	if err == nil || provider.IsTransient(err) || stub.creates != 1 || stub.lists != 0 {
		t.Fatalf("expected a single permanent failure, got %v (creates=%d lists=%d)", err, stub.creates, stub.lists)
	}
}
