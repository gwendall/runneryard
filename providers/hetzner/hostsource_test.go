package hetzner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/gwendall/runneryard/pool"
	"github.com/gwendall/runneryard/provider/retry"
)

const poolHostSelector = "runneryard-pool-host=true,runneryard-controller=test-controller,runneryard-pool=fast"

// testControllerKey returns a fresh controller key and its OpenSSH PEM.
func testControllerKey(t *testing.T) (*pool.ControllerKey, []byte) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "controller")
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(block)
	key, err := pool.ParseControllerKey(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return key, encoded
}

func testHostSourceConfig(server *httptest.Server, key *pool.ControllerKey) HostSourceConfig {
	return HostSourceConfig{
		APIToken: "hcloud-token", APIBaseURL: server.URL, Location: "fsn1", ServerType: "cx33",
		ServerImage: "docker-ce", ControllerID: "test-controller", Pool: "fast", FirewallID: 77, NetworkID: 84,
		RunnerImage: "ghcr.io/gwendall/runneryard:0.5.0", Slots: 3, MinFreeDiskGB: 20,
		Caches: []string{"/home/runner/.cache"}, HostKey: key.HostKey, AuthorizedKey: key.AuthorizedKey(),
		HTTPClient: server.Client(), ActionPollInterval: time.Millisecond, ActionTimeout: time.Second,
		Retry: retry.Policy{Attempts: 1, InitialBackoff: time.Millisecond},
	}
}

func testHostSource(t *testing.T, server *httptest.Server, key *pool.ControllerKey) *HostSource {
	t.Helper()
	source, err := NewHostSource(testHostSourceConfig(server, key))
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func poolHostServer(id int64, name string, labels map[string]string) server {
	item := server{ID: id, Name: name, Status: "running", Labels: labels}
	item.PublicNet.Firewalls = []serverFirewall{{ID: 77, Status: "applied"}}
	return item
}

func poolHostLabels() map[string]string {
	return map[string]string{poolHostKey: "true", controllerIDKey: "test-controller", poolNameKey: "fast"}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func TestNewHostSourceRejectsUnsafeConfig(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	key, _ := testControllerKey(t)
	for name, mutate := range map[string]func(*HostSourceConfig){
		"bad pool name":           func(cfg *HostSourceConfig) { cfg.Pool = "Fast_Pool" },
		"empty pool name":         func(cfg *HostSourceConfig) { cfg.Pool = "" },
		"no host key function":    func(cfg *HostSourceConfig) { cfg.HostKey = nil },
		"no controller key":       func(cfg *HostSourceConfig) { cfg.AuthorizedKey = "  " },
		"no slot":                 func(cfg *HostSourceConfig) { cfg.Slots = 0 },
		"negative slot memory":    func(cfg *HostSourceConfig) { cfg.SlotMemoryMB = -1 },
		"cache path with a quote": func(cfg *HostSourceConfig) { cfg.Caches = []string{"/home/runner/'x"} },
		"negative price":          func(cfg *HostSourceConfig) { cfg.HourlyPrice = -0.01 },
		"unknown type, no price":  func(cfg *HostSourceConfig) { cfg.ServerType = "cx99" },
		"no firewall":             func(cfg *HostSourceConfig) { cfg.FirewallID = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testHostSourceConfig(server, key)
			mutate(&cfg)
			if _, err := NewHostSource(cfg); err == nil {
				t.Fatal("expected the configuration to be refused")
			}
		})
	}
}

func TestNewHostSourcePricesFromListOrOverride(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	key, _ := testControllerKey(t)
	for name, test := range map[string]struct {
		serverType string
		override   float64
		want       float64
	}{
		"listed type":              {serverType: "cx33", want: 0.0136},
		"listed type, overridden":  {serverType: "cx33", override: 0.02, want: 0.02},
		"unlisted type with price": {serverType: "cx99", override: 0.5, want: 0.5},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testHostSourceConfig(server, key)
			cfg.ServerType = test.serverType
			cfg.HourlyPrice = test.override
			source, err := NewHostSource(cfg)
			if err != nil {
				t.Fatal(err)
			}
			facts := source.Facts()
			if facts.HourlyPrice != test.want || facts.Billing != pool.BillingPerStartedHour || facts.Currency != "EUR" || !facts.CanCreate {
				t.Fatalf("facts = %#v", facts)
			}
		})
	}
}

func TestHourlyPriceEUR(t *testing.T) {
	for serverType, want := range map[string]float64{
		"cx33": 0.0136, "cx53": 0.0473, "cpx22": 0.0312, "cpx32": 0.0569, "cpx42": 0.1114, "ccx33": 0.2219,
	} {
		if price, ok := HourlyPriceEUR(serverType); !ok || price != want {
			t.Fatalf("HourlyPriceEUR(%q) = %v, %v; want %v", serverType, price, ok, want)
		}
	}
	for _, serverType := range []string{"", "cx11", "CX33"} {
		if price, ok := HourlyPriceEUR(serverType); ok || price != 0 {
			t.Fatalf("HourlyPriceEUR(%q) = %v, %v; want no listed price", serverType, price, ok)
		}
	}
}

func TestHostSourceListReturnsOnlyThisPoolsHosts(t *testing.T) {
	key, _ := testControllerKey(t)
	created := time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)
	owned := poolHostServer(11, "ry-fast-0a1b2c3d", poolHostLabels())
	owned.CreatedAt = created
	owned.PublicNet.IPv4.IP = "203.0.113.10"
	booting := poolHostServer(12, "ry-fast-4e5f6a7b", poolHostLabels())
	booting.Status = "initializing"

	foreign := func(id int64, change func(map[string]string)) server {
		labels := poolHostLabels()
		change(labels)
		item := poolHostServer(id, "ry-other", labels)
		item.PublicNet.IPv4.IP = "203.0.113.99"
		return item
	}
	pages := map[string]serverListResponse{}
	first := serverListResponse{Servers: []server{
		owned,
		foreign(21, func(l map[string]string) { l[poolNameKey] = "slow" }),
		foreign(22, func(l map[string]string) { l[controllerIDKey] = "other-controller" }),
		foreign(23, func(l map[string]string) { l[managedByKey] = "true" }),
	}}
	next := 2
	first.Meta.Pagination.NextPage = &next
	pages["1"] = first
	pages["2"] = serverListResponse{Servers: []server{
		booting,
		foreign(24, func(l map[string]string) { l[poolHostKey] = "false" }),
		foreign(25, func(l map[string]string) { delete(l, poolHostKey); l[managedByKey] = "true" }),
	}}

	var mu sync.Mutex
	var selectors, pageRequests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/servers" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		selectors = append(selectors, r.URL.Query().Get("label_selector"))
		pageRequests = append(pageRequests, r.URL.Query().Get("page"))
		mu.Unlock()
		writeJSON(w, http.StatusOK, pages[r.URL.Query().Get("page")])
	}))
	defer server.Close()

	hosts, err := testHostSource(t, server, key).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pageRequests, []string{"1", "2"}) {
		t.Fatalf("pages = %v", pageRequests)
	}
	for _, selector := range selectors {
		if selector != poolHostSelector {
			t.Fatalf("label_selector = %q", selector)
		}
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts = %#v", hosts)
	}
	_, ownedKey, err := key.HostKey(owned.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got := hosts[0]; got.ID != "hetzner-11" || got.Address != "203.0.113.10:22" || got.HostKey != ownedKey || got.State != "running" || !got.CreatedAt.Equal(created) {
		t.Fatalf("owned host = %#v", got)
	}
	_, bootingKey, err := key.HostKey(booting.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got := hosts[1]; got.ID != "hetzner-12" || got.Address != "" || got.HostKey != bootingKey || got.State != "initializing" {
		t.Fatalf("booting host = %#v", got)
	}
	if ownedKey == bootingKey {
		t.Fatal("two hosts derived the same host key")
	}
}

// bootstrapUserData is the decoded cloud-config of a pool host.
type bootstrapUserData struct {
	SSHPasswordAuth *bool             `json:"ssh_pwauth"`
	DisableRoot     *bool             `json:"disable_root"`
	SSHDeleteKeys   *bool             `json:"ssh_deletekeys"`
	SSHGenKeyTypes  []string          `json:"ssh_genkeytypes"`
	SSHKeys         map[string]string `json:"ssh_keys"`
	RunCmd          [][]string        `json:"runcmd"`
}

func samePrivateKey(t *testing.T, got, want []byte) bool {
	t.Helper()
	parse := func(encoded []byte) ed25519.PrivateKey {
		raw, err := ssh.ParseRawPrivateKey(encoded)
		if err != nil {
			t.Fatal(err)
		}
		switch key := raw.(type) {
		case ed25519.PrivateKey:
			return key
		case *ed25519.PrivateKey:
			return *key
		}
		t.Fatalf("private key is a %T, want ed25519", raw)
		return nil
	}
	return parse(got).Equal(parse(want))
}

func flagValue(args []string, name string) (string, bool) {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func TestHostSourceCreateBootsALockedDownHost(t *testing.T) {
	key, controllerPEM := testControllerKey(t)
	var (
		mu        sync.Mutex
		calls     []string
		body      []byte
		request   createServerRequest
		poweredOn bool
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer hcloud-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/servers":
			var rawBody json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&rawBody); err != nil {
				t.Error(err)
			}
			body = rawBody
			if err := json.Unmarshal(rawBody, &request); err != nil {
				t.Error(err)
			}
			writeJSON(w, http.StatusCreated, createServerResponse{
				Server:      server{ID: 501, Name: request.Name, Status: "initializing"},
				Action:      action{ID: 1, Status: "running"},
				NextActions: []action{{ID: 2, Status: "success"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/actions/1":
			writeJSON(w, http.StatusOK, actionResponse{Action: action{ID: 1, Status: "success"}})
		case r.Method == http.MethodGet && r.URL.Path == "/servers/501":
			item := poolHostServer(501, request.Name, poolHostLabels())
			if poweredOn {
				item.PublicNet.IPv4.IP = "198.51.100.7"
			} else {
				item.Status = "off"
			}
			writeJSON(w, http.StatusOK, serverResponse{Server: item})
		case r.Method == http.MethodPost && r.URL.Path == "/servers/501/actions/poweron":
			poweredOn = true
			writeJSON(w, http.StatusCreated, actionResponse{Action: action{ID: 3, Status: "running"}})
		case r.Method == http.MethodGet && r.URL.Path == "/actions/3":
			writeJSON(w, http.StatusOK, actionResponse{Action: action{ID: 3, Status: "success"}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	host, err := testHostSource(t, server, key).Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()

	wantCalls := []string{
		"POST /servers", "GET /actions/1", "GET /servers/501",
		"POST /servers/501/actions/poweron", "GET /actions/3", "GET /servers/501",
	}
	if !slices.Equal(calls, wantCalls) {
		t.Fatalf("calls = %v\nwant    %v", calls, wantCalls)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["start_after_create"]) != "false" {
		t.Fatalf("start_after_create = %s", fields["start_after_create"])
	}
	if !regexp.MustCompile(`^ry-fast-[0-9a-f]{8}$`).MatchString(request.Name) {
		t.Fatalf("name = %q", request.Name)
	}
	if request.ServerType != "cx33" || request.Image != "docker-ce" || request.Location != "fsn1" {
		t.Fatalf("server shape = %#v", request)
	}
	if len(request.Labels) != 3 || request.Labels[poolHostKey] != "true" ||
		request.Labels[controllerIDKey] != "test-controller" || request.Labels[poolNameKey] != "fast" {
		t.Fatalf("labels = %#v", request.Labels)
	}
	if _, ok := request.Labels[managedByKey]; ok {
		t.Fatalf("a pool host must never carry %s: %#v", managedByKey, request.Labels)
	}
	if !slices.Equal(request.Firewalls, []createServerFirewall{{Firewall: 77}}) || !slices.Equal(request.Networks, []int64{84}) {
		t.Fatalf("firewalls = %#v, networks = %#v", request.Firewalls, request.Networks)
	}

	userData, ok := strings.CutPrefix(request.UserData, "#cloud-config\n")
	if !ok {
		t.Fatalf("user data is not cloud-config: %q", request.UserData)
	}
	if !strings.Contains(userData, `"ssh_genkeytypes":[]`) {
		t.Fatalf("user data must generate no host key: %s", userData)
	}
	var config bootstrapUserData
	if err := json.Unmarshal([]byte(userData), &config); err != nil {
		t.Fatal(err)
	}
	if config.SSHPasswordAuth == nil || *config.SSHPasswordAuth || config.SSHDeleteKeys == nil || !*config.SSHDeleteKeys ||
		config.DisableRoot == nil || !*config.DisableRoot || config.SSHGenKeyTypes == nil || len(config.SSHGenKeyTypes) != 0 {
		t.Fatalf("SSH settings = %#v", config)
	}
	hostPrivate, hostPublic, err := key.HostKey(request.Name)
	if err != nil {
		t.Fatal(err)
	}
	// The OpenSSH encoding carries a random check word, so compare the keys.
	if len(config.SSHKeys) != 2 || config.SSHKeys["ed25519_public"] != hostPublic ||
		!samePrivateKey(t, []byte(config.SSHKeys["ed25519_private"]), hostPrivate) {
		t.Fatalf("ssh_keys = %#v", config.SSHKeys)
	}

	if len(config.RunCmd) != 1 {
		t.Fatalf("runcmd = %#v", config.RunCmd)
	}
	args := config.RunCmd[0]
	if len(args) < 5 || args[0] != "bash" || args[1] != "-c" || args[4] != "ghcr.io/gwendall/runneryard:0.5.0" {
		t.Fatalf("bootstrap command = %#v", args)
	}
	for _, flag := range []string{"--harden-sshd", "--block-metadata"} {
		if !slices.Contains(args, flag) {
			t.Fatalf("bootstrap misses %s: %#v", flag, args)
		}
	}
	for flag, want := range map[string]string{
		"--authorized-key": key.AuthorizedKey(), "--slots": "3", "--min-free-disk-gb": "20", "--cache": "/home/runner/.cache",
	} {
		if got, ok := flagValue(args, flag); !ok || got != want {
			t.Fatalf("%s = %q, want %q", flag, got, want)
		}
	}

	// Only the host's own key reaches the machine, never the controller key.
	escapedPEM, err := json.Marshal(string(controllerPEM))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(request.UserData, string(controllerPEM)) || strings.Contains(request.UserData, strings.Trim(string(escapedPEM), `"`)) {
		t.Fatal("user data contains the controller private key")
	}
	if count := strings.Count(request.UserData, "BEGIN OPENSSH PRIVATE KEY"); count != 1 {
		t.Fatalf("user data holds %d private keys, want only the host key", count)
	}

	if host.ID != "hetzner-501" || host.Address != "198.51.100.7:22" || host.HostKey != hostPublic || host.State != "running" {
		t.Fatalf("host = %#v", host)
	}
}

func TestHostSourceCreateDeletesHostWhoseFirewallIsNotApplied(t *testing.T) {
	key, _ := testControllerKey(t)
	for name, firewalls := range map[string][]serverFirewall{
		"pending":        {{ID: 77, Status: "pending"}},
		"other firewall": {{ID: 42, Status: "applied"}},
		"no firewall":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				calls = append(calls, r.Method+" "+r.URL.Path)
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/servers":
					writeJSON(w, http.StatusCreated, createServerResponse{
						Server: server{ID: 502, Status: "initializing"},
						Action: action{ID: 1, Status: "success"},
					})
				case r.Method == http.MethodGet && r.URL.Path == "/servers/502":
					item := poolHostServer(502, "ry-fast-00000000", poolHostLabels())
					item.Status = "off"
					item.PublicNet.Firewalls = firewalls
					writeJSON(w, http.StatusOK, serverResponse{Server: item})
				case r.Method == http.MethodDelete && r.URL.Path == "/servers/502":
					writeJSON(w, http.StatusOK, actionResponse{Action: action{ID: 9, Status: "success"}})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			host, err := testHostSource(t, server, key).Create(context.Background())
			if err == nil || !strings.Contains(err.Error(), "firewall 77 is not applied") {
				t.Fatalf("err = %v", err)
			}
			if host != (pool.Host{}) {
				t.Fatalf("host = %#v", host)
			}
			mu.Lock()
			defer mu.Unlock()
			want := []string{"POST /servers", "GET /servers/502", "DELETE /servers/502"}
			if !slices.Equal(calls, want) {
				t.Fatalf("calls = %v, want %v", calls, want)
			}
		})
	}
}

func TestHostSourceDeleteOnlyDeletesThisPoolsHosts(t *testing.T) {
	key, _ := testControllerKey(t)
	labelled := func(change func(map[string]string)) map[string]string {
		labels := poolHostLabels()
		change(labels)
		return labels
	}
	for name, test := range map[string]struct {
		labels  map[string]string
		missing bool
		deleted bool
		wantErr bool
	}{
		"own host":            {labels: poolHostLabels(), deleted: true},
		"already gone":        {missing: true},
		"ephemeral worker":    {labels: map[string]string{managedByKey: "true", controllerIDKey: "test-controller"}, wantErr: true},
		"pool host + worker":  {labels: labelled(func(l map[string]string) { l[managedByKey] = "true" }), wantErr: true},
		"another pool":        {labels: labelled(func(l map[string]string) { l[poolNameKey] = "slow" }), wantErr: true},
		"another controller":  {labels: labelled(func(l map[string]string) { l[controllerIDKey] = "other" }), wantErr: true},
		"unlabelled server":   {labels: map[string]string{}, wantErr: true},
		"pool host flag gone": {labels: labelled(func(l map[string]string) { delete(l, poolHostKey) }), wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			deletes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/servers/601":
					if test.missing {
						writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "not_found", "message": "server not found"}})
						return
					}
					writeJSON(w, http.StatusOK, serverResponse{Server: poolHostServer(601, "ry-fast-11111111", test.labels)})
				case r.Method == http.MethodDelete && r.URL.Path == "/servers/601":
					deletes++
					writeJSON(w, http.StatusOK, actionResponse{Action: action{ID: 9, Status: "success"}})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			err := testHostSource(t, server, key).Delete(context.Background(), "hetzner-601")
			if (err != nil) != test.wantErr {
				t.Fatalf("err = %v, want error %v", err, test.wantErr)
			}
			mu.Lock()
			defer mu.Unlock()
			if want := map[bool]int{true: 1, false: 0}[test.deleted]; deletes != want {
				t.Fatalf("DELETE calls = %d, want %d", deletes, want)
			}
		})
	}
}

func TestHostSourceDeleteRejectsMalformedIDsWithoutCalls(t *testing.T) {
	key, _ := testControllerKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	source := testHostSource(t, server, key)
	for _, id := range []string{"", "601", "hetzner-", "hetzner-0", "hetzner--1", "hetzner-abc", "hetzner-601/../602", "static-601"} {
		if err := source.Delete(context.Background(), id); err == nil {
			t.Fatalf("Delete(%q) succeeded", id)
		}
	}
}

func TestEphemeralInventorySkipsPoolHosts(t *testing.T) {
	worker := ownedServer(1, "lease-one")
	disguised := poolHostServer(2, "ry-fast-22222222", poolHostLabels())
	disguised.Labels[managedByKey] = "true"
	disguised.Labels[leaseIDKey] = "lease-two"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/servers" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, serverListResponse{Servers: []server{worker, disguised}})
	}))
	defer server.Close()

	workers, err := testAdapter(t, server).Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 || workers[0].ID != "1" {
		t.Fatalf("workers = %#v", workers)
	}
}
