package hetzner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gwendall/runneryard/provider"
)

func TestLaunchCreatesIsolatedDisposableServer(t *testing.T) {
	polls := make(map[string]int)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer hcloud-token" {
			t.Fatalf("unexpected authorization header %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/actions/") {
			polls[r.URL.Path]++
			_ = json.NewEncoder(w).Encode(actionResponse{Action: action{ID: actionID(r.URL.Path), Status: "success"}})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/servers/123" {
			qualified := server{ID: 123, Name: "runner-one", Status: "running", Labels: map[string]string{
				leaseIDKey: "lease-one", runnerNameKey: "runner-one",
			}}
			qualified.PublicNet.Firewalls = []serverFirewall{{ID: 42, Status: "applied"}}
			_ = json.NewEncoder(w).Encode(serverResponse{Server: qualified})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/servers" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var request createServerRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Image != "docker-ce" || request.ServerType != "cpx32" || request.Location != "fsn1" || !request.StartAfterCreate {
			t.Fatalf("unexpected server shape: %#v", request)
		}
		if len(request.Firewalls) != 1 || request.Firewalls[0].Firewall != 42 {
			t.Fatalf("worker firewall missing: %#v", request.Firewalls)
		}
		if len(request.Networks) != 1 || request.Networks[0] != 84 {
			t.Fatalf("worker network missing: %#v", request.Networks)
		}
		if request.Labels[controllerIDKey] != "test-controller" || request.Labels[leaseIDKey] != "lease-one" {
			t.Fatalf("ownership labels missing: %#v", request.Labels)
		}
		if strings.Contains(request.UserData, "hcloud-token") || strings.Contains(request.UserData, "jit-secret") {
			t.Fatal("cloud-init must not contain plaintext credentials")
		}
		assertCloudInit(t, request.UserData)
		_ = json.NewEncoder(w).Encode(createServerResponse{Server: server{
			ID: 123, Name: request.Name, Status: "initializing", CreatedAt: time.Now(), Labels: request.Labels,
		}, Action: action{ID: 10, Status: "running"}, NextActions: []action{{ID: 11, Status: "running"}}})
	}))
	defer api.Close()

	worker, err := testAdapter(t, api).Launch(context.Background(), provider.Lease{
		ID: "lease-one", RunnerName: "runner-one", JITConfig: "jit-secret", Deadline: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if worker.ID != "123" || worker.LeaseID != "lease-one" || worker.RunnerName != "runner-one" {
		t.Fatalf("unexpected worker %#v", worker)
	}
	if polls["/actions/10"] != 1 || polls["/actions/11"] != 1 {
		t.Fatalf("create actions were not qualified: %#v", polls)
	}
}

func TestLaunchReturnsRecoverableWorkerWhenActionFails(t *testing.T) {
	deleted := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/servers":
			_ = json.NewEncoder(w).Encode(createServerResponse{
				Server: server{ID: 123, Name: "runner-one", Labels: map[string]string{leaseIDKey: "lease-one"}},
				Action: action{ID: 10, Status: "running"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/actions/10":
			_, _ = w.Write([]byte(`{"action":{"id":10,"status":"error","error":{"code":"resource_limit_exceeded","message":"capacity unavailable"}}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/servers/123":
			deleted = true
			_ = json.NewEncoder(w).Encode(actionResponse{Action: action{ID: 12, Status: "running"}})
		case r.Method == http.MethodGet && r.URL.Path == "/actions/12":
			_ = json.NewEncoder(w).Encode(actionResponse{Action: action{ID: 12, Status: "success"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	adapter := testAdapter(t, api)
	_, err := adapter.Launch(context.Background(), provider.Lease{
		ID: "lease-one", RunnerName: "runner-one", JITConfig: "jit-secret", Deadline: time.Now().Add(time.Hour),
	})
	var partial *provider.PartialLaunchError
	if !errors.As(err, &partial) || partial.Worker.ID != "123" || partial.Worker.LeaseID != "lease-one" {
		t.Fatalf("expected recoverable partial launch, got %#v", err)
	}
	// This is the cleanup contract used by the controller for ambiguous creates.
	if err := adapter.Destroy(context.Background(), partial.Worker.ID); err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("partially created server was not recoverable through Destroy")
	}
}

func TestLaunchRejectsWorkerWithoutAppliedFirewall(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/servers":
			_ = json.NewEncoder(w).Encode(createServerResponse{
				Server: server{ID: 123, Name: "runner-one", Labels: map[string]string{leaseIDKey: "lease-one"}},
				Action: action{ID: 10, Status: "success"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/servers/123":
			item := server{ID: 123, Name: "runner-one", Labels: map[string]string{leaseIDKey: "lease-one"}}
			item.PublicNet.Firewalls = []serverFirewall{{ID: 42, Status: "pending"}}
			_ = json.NewEncoder(w).Encode(serverResponse{Server: item})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	_, err := testAdapter(t, api).Launch(context.Background(), provider.Lease{
		ID: "lease-one", RunnerName: "runner-one", JITConfig: "jit-secret", Deadline: time.Now().Add(time.Hour),
	})
	var partial *provider.PartialLaunchError
	if !errors.As(err, &partial) || !strings.Contains(err.Error(), "firewall 42 is not applied") {
		t.Fatalf("expected firewall qualification failure, got %v", err)
	}
}

func TestInventoryPaginatesAndExcludesForeignServers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query, _ := url.QueryUnescape(r.URL.Query().Get("label_selector"))
		if query != managedByKey+"=true,"+controllerIDKey+"=test-controller" {
			t.Fatalf("unexpected selector %q", query)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			next := 2
			response := serverListResponse{Servers: []server{
				{ID: 1, Status: "running", Labels: map[string]string{managedByKey: "true", controllerIDKey: "test-controller", leaseIDKey: "lease-one"}},
				{ID: 2, Status: "running", Labels: map[string]string{managedByKey: "true", controllerIDKey: "foreign"}},
			}}
			response.Meta.Pagination.NextPage = &next
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		_ = json.NewEncoder(w).Encode(serverListResponse{Servers: []server{
			{ID: 3, Status: "off", Labels: map[string]string{managedByKey: "true", controllerIDKey: "test-controller", leaseIDKey: "lease-three"}},
		}})
	}))
	defer server.Close()

	workers, err := testAdapter(t, server).Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 2 || workers[0].ID != "1" || workers[1].ID != "3" {
		t.Fatalf("unexpected inventory %#v", workers)
	}
}

func TestDestroyTreatsAlreadyDeletedAsSuccess(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	if err := testAdapter(t, server).Destroy(context.Background(), "123"); err != nil {
		t.Fatal(err)
	}
}

func TestDestroyWaitsForDeleteAction(t *testing.T) {
	polls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/servers/123":
			_ = json.NewEncoder(w).Encode(actionResponse{Action: action{ID: 12, Status: "running"}})
		case r.Method == http.MethodGet && r.URL.Path == "/actions/12":
			polls++
			_ = json.NewEncoder(w).Encode(actionResponse{Action: action{ID: 12, Status: "success"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	if err := testAdapter(t, api).Destroy(context.Background(), "123"); err != nil {
		t.Fatal(err)
	}
	if polls != 1 {
		t.Fatalf("delete action was polled %d times", polls)
	}
}

func TestDestroyRejectsForeignIdentifier(t *testing.T) {
	adapter, err := New(Config{
		APIToken: "token", Location: "fsn1", ServerType: "cpx32", ServerImage: "docker-ce",
		RunnerImage: "ghcr.io/gwendall/runneryard:0.1.1", ControllerID: "controller", FirewallID: 42,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Destroy(context.Background(), "not-an-id"); err == nil {
		t.Fatal("expected invalid worker id to fail")
	}
}

func assertCloudInit(t *testing.T, userData string) {
	t.Helper()
	if !strings.HasPrefix(userData, "#cloud-config\n") {
		t.Fatal("missing cloud-config header")
	}
	var config struct {
		WriteFiles []struct {
			Permissions string `json:"permissions"`
			Encoding    string `json:"encoding"`
			Content     string `json:"content"`
		} `json:"write_files"`
		RunCmd [][]string `json:"runcmd"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(userData, "#cloud-config\n")), &config); err != nil {
		t.Fatal(err)
	}
	if len(config.WriteFiles) != 1 || config.WriteFiles[0].Permissions != "0600" || config.WriteFiles[0].Encoding != "b64" {
		t.Fatalf("unsafe lease file: %#v", config.WriteFiles)
	}
	decoded, err := base64.StdEncoding.DecodeString(config.WriteFiles[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(decoded), "ACTIONS_RUNNER_INPUT_JITCONFIG=jit-secret") || strings.Contains(string(decoded), "hcloud-token") {
		t.Fatalf("unexpected lease contents %q", decoded)
	}
	if len(config.RunCmd) != 1 || len(config.RunCmd[0]) < 6 {
		t.Fatalf("missing bootstrap command: %#v", config.RunCmd)
	}
	bootstrap := config.RunCmd[0][2]
	if !strings.Contains(bootstrap, "docker create") || !strings.Contains(bootstrap, "shred -u") || !strings.Contains(bootstrap, "shutdown -h now") {
		t.Fatalf("bootstrap does not create, erase, and stop: %s", bootstrap)
	}
	if config.RunCmd[0][4] != "ghcr.io/gwendall/runneryard:0.1.1" {
		t.Fatalf("unexpected runner image %q", config.RunCmd[0][4])
	}
}

func testAdapter(t *testing.T, server *httptest.Server) *Adapter {
	t.Helper()
	adapter, err := New(Config{
		APIBaseURL: server.URL, APIToken: "hcloud-token", Location: "fsn1", ServerType: "cpx32",
		ServerImage: "docker-ce", RunnerImage: "ghcr.io/gwendall/runneryard:0.1.1",
		ControllerID: "test-controller", FirewallID: 42, NetworkID: 84, HTTPClient: server.Client(),
		ActionPollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func actionID(path string) int64 {
	id, _ := strconv.ParseInt(strings.TrimPrefix(path, "/actions/"), 10, 64)
	return id
}
