package fly

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gwendall/runneryard/pool"
	"github.com/gwendall/runneryard/provider"
	"github.com/gwendall/runneryard/provider/retry"
	"golang.org/x/crypto/ssh"
)

const (
	testPoolApp    = "ci-pool"
	testController = "test-controller"
	testPoolImage  = "ghcr.io/example/runner:1"
	testToken      = "fly-pool-token"
)

type fakeMachine struct {
	ID, Name, State, PrivateIP string
	CreatedAt, UpdatedAt       time.Time
	Metadata                   map[string]string
	Mounts                     []machineMount
	// pending is the state the next read reports, as Fly reports stopping
	// before stopped.
	pending string
}

type fakeVolume struct {
	ID, Name, State string
	Attached        string
	CreatedAt       time.Time
	// detachAfter is how many reads still report the volume attached after
	// its Machine is destroyed.
	detachAfter int
}

// fakeFly is an in-memory Fly Machines API for one app.
type fakeFly struct {
	t   *testing.T
	mu  sync.Mutex
	now time.Time
	seq int

	machines map[string]*fakeMachine
	volumes  map[string]*fakeVolume
	requests []string
	bodies   map[string][]byte

	// failMachineCreate, when set, answers the Machine creation; with
	// createAnyway the Machine exists all the same.
	failMachineCreate int
	createAnyway      bool
	failStart         string
	volumeDetachReads int
}

func newFakeFly(t *testing.T) *fakeFly {
	return &fakeFly{
		t: t, now: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC),
		machines: map[string]*fakeMachine{}, volumes: map[string]*fakeVolume{}, bodies: map[string][]byte{},
	}
}

func (f *fakeFly) machineJSON(m *fakeMachine) map[string]any {
	return map[string]any{
		"id": m.ID, "name": m.Name, "state": m.State, "region": "cdg", "private_ip": m.PrivateIP,
		"created_at": m.CreatedAt, "updated_at": m.UpdatedAt,
		"config": map[string]any{
			"metadata": m.Metadata, "mounts": m.Mounts,
			// The real API returns the files; the source must not need them.
			"files": []map[string]any{{"guest_path": "/etc/runneryard/ssh_host_ed25519_key", "raw_value": "c2VjcmV0"}},
		},
	}
}

func (f *fakeFly) volumeJSON(v *fakeVolume) map[string]any {
	var attached any
	if v.Attached != "" {
		attached = v.Attached
	}
	return map[string]any{"id": v.ID, "name": v.Name, "state": v.State, "attached_machine_id": attached, "created_at": v.CreatedAt}
}

func (f *fakeFly) addMachine(id, state string, metadata map[string]string, volumes ...string) *fakeMachine {
	f.mu.Lock()
	defer f.mu.Unlock()
	machine := &fakeMachine{
		ID: id, Name: "ry-pool-" + id, State: state, PrivateIP: "fdaa:0:1::" + fmt.Sprint(len(f.machines)+2),
		CreatedAt: f.now.Add(-time.Hour), UpdatedAt: f.now.Add(-10 * time.Minute), Metadata: metadata,
	}
	for _, volume := range volumes {
		machine.Mounts = append(machine.Mounts, machineMount{Volume: volume, Path: dataPath})
	}
	f.machines[id] = machine
	return machine
}

func (f *fakeFly) addVolume(id, name, attached string, age time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumes[id] = &fakeVolume{ID: id, Name: name, State: "created", Attached: attached, CreatedAt: f.now.Add(-age)}
}

func (f *fakeFly) calls(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, request := range f.requests {
		if strings.HasPrefix(request, prefix) {
			out = append(out, request)
		}
	}
	return out
}

func (f *fakeFly) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(f.requests), func(request string) bool { return strings.HasPrefix(request, "GET ") })
}

func (f *fakeFly) handler() http.Handler {
	mux := http.NewServeMux()
	base := "/v1/apps/" + testPoolApp
	reply := func(w http.ResponseWriter, status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}
	mux.HandleFunc("GET "+base+"/machines", func(w http.ResponseWriter, _ *http.Request) {
		list := []map[string]any{}
		for _, id := range sortedKeys(f.machines) {
			list = append(list, f.machineJSON(f.machines[id]))
		}
		reply(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST "+base+"/machines", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Name   string `json:"name"`
			Config struct {
				Metadata map[string]string `json:"metadata"`
				Mounts   []machineMount    `json:"mounts"`
			} `json:"config"`
		}
		if err := json.Unmarshal(f.bodies[r.Method+" "+r.URL.Path], &request); err != nil {
			reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if f.failMachineCreate != 0 && !f.createAnyway {
			reply(w, f.failMachineCreate, map[string]string{"error": "rejected"})
			return
		}
		f.seq++
		machine := &fakeMachine{
			ID: fmt.Sprintf("e%04d", f.seq), Name: request.Name, State: "created", PrivateIP: "fdaa:0:1::99",
			CreatedAt: f.now, UpdatedAt: f.now, Metadata: request.Config.Metadata, Mounts: request.Config.Mounts, pending: startedMachine,
		}
		f.machines[machine.ID] = machine
		for _, mount := range request.Config.Mounts {
			if volume := f.volumes[mount.Volume]; volume != nil {
				volume.Attached = machine.ID
			}
		}
		if f.failMachineCreate != 0 {
			reply(w, f.failMachineCreate, map[string]string{"error": "upstream timeout"})
			return
		}
		reply(w, http.StatusOK, f.machineJSON(machine))
	})
	mux.HandleFunc("GET "+base+"/machines/{id}", func(w http.ResponseWriter, r *http.Request) {
		machine := f.machines[r.PathValue("id")]
		if machine == nil {
			reply(w, http.StatusNotFound, map[string]string{"error": "machine not found"})
			return
		}
		body := f.machineJSON(machine)
		if machine.pending != "" {
			machine.State, machine.pending, machine.UpdatedAt = machine.pending, "", f.now
		}
		reply(w, http.StatusOK, body)
	})
	mux.HandleFunc("POST "+base+"/machines/{id}/{verb}", func(w http.ResponseWriter, r *http.Request) {
		machine := f.machines[r.PathValue("id")]
		if machine == nil {
			reply(w, http.StatusNotFound, map[string]string{"error": "machine not found"})
			return
		}
		switch r.PathValue("verb") {
		case "stop":
			machine.State, machine.pending = "stopping", stoppedMachine
		case "start":
			if f.failStart != "" {
				reply(w, http.StatusPreconditionFailed, map[string]string{"error": f.failStart})
				return
			}
			machine.State, machine.pending = "starting", startedMachine
		default:
			reply(w, http.StatusNotFound, nil)
			return
		}
		reply(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("DELETE "+base+"/machines/{id}", func(w http.ResponseWriter, r *http.Request) {
		machine := f.machines[r.PathValue("id")]
		if machine == nil {
			reply(w, http.StatusNotFound, map[string]string{"error": "machine not found"})
			return
		}
		if r.URL.Query().Get("force") != "true" {
			reply(w, http.StatusPreconditionFailed, map[string]string{"error": "machine is running"})
			return
		}
		machine.State, machine.pending = destroyingMachine, destroyedMachine
		for _, mount := range machine.Mounts {
			if volume := f.volumes[mount.Volume]; volume != nil {
				volume.detachAfter = f.volumeDetachReads
			}
		}
		reply(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET "+base+"/volumes", func(w http.ResponseWriter, _ *http.Request) {
		list := []map[string]any{}
		for _, id := range sortedKeys(f.volumes) {
			list = append(list, f.volumeJSON(f.volumes[id]))
		}
		reply(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST "+base+"/volumes", func(w http.ResponseWriter, r *http.Request) {
		var request createVolumeRequest
		if err := json.Unmarshal(f.bodies[r.Method+" "+r.URL.Path], &request); err != nil {
			reply(w, http.StatusBadRequest, nil)
			return
		}
		f.seq++
		volume := &fakeVolume{ID: fmt.Sprintf("vol_%04d", f.seq), Name: request.Name, State: "created", CreatedAt: f.now}
		f.volumes[volume.ID] = volume
		reply(w, http.StatusOK, f.volumeJSON(volume))
	})
	mux.HandleFunc("GET "+base+"/volumes/{id}", func(w http.ResponseWriter, r *http.Request) {
		volume := f.volumes[r.PathValue("id")]
		if volume == nil {
			reply(w, http.StatusNotFound, nil)
			return
		}
		if machine := f.machines[volume.Attached]; machine != nil && machine.State == destroyedMachine {
			if volume.detachAfter > 0 {
				volume.detachAfter--
			} else {
				volume.Attached = ""
			}
		}
		reply(w, http.StatusOK, f.volumeJSON(volume))
	})
	mux.HandleFunc("DELETE "+base+"/volumes/{id}", func(w http.ResponseWriter, r *http.Request) {
		volume := f.volumes[r.PathValue("id")]
		if volume == nil {
			reply(w, http.StatusNotFound, nil)
			return
		}
		if volume.Attached != "" {
			reply(w, http.StatusBadRequest, map[string]string{"error": "volume is attached to a machine"})
			return
		}
		delete(f.volumes, volume.ID)
		reply(w, http.StatusOK, f.volumeJSON(volume))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		key := r.Method + " " + r.URL.Path
		f.requests = append(f.requests, key)
		if len(body) > 0 {
			f.bodies[key] = body
		}
		mux.ServeHTTP(w, r)
	})
}

func sortedKeys[V any](items map[string]V) []string {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

type hostSourceFixture struct {
	fly        *fakeFly
	source     *HostSource
	controller *pool.ControllerKey
	cfg        HostSourceConfig
}

func newTestControllerKey(t *testing.T) *pool.ControllerKey {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "controller")
	if err != nil {
		t.Fatal(err)
	}
	key, err := pool.ParseControllerKey(pem.EncodeToMemory(block))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func testHostSourceConfig(controller *pool.ControllerKey, baseURL string, client *http.Client) HostSourceConfig {
	return HostSourceConfig{
		APIToken: testToken, APIBaseURL: baseURL, App: testPoolApp, Region: "cdg",
		CPUKind: "performance", CPUs: 8, MemoryMB: 16384, VolumeGB: 50,
		ControllerID: testController, Pool: "pool", RunnerImage: testPoolImage,
		Slots: 4, SlotCPUs: 2, SlotMemoryMB: 4096, Caches: []string{"/home/runner/.cache"}, MinFreeDiskGB: 5,
		HostKey: controller.HostKey, AuthorizedKey: controller.AuthorizedKey(),
		HTTPClient: client, Retry: retry.Policy{Attempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond, Rate: 1000, Burst: 1000},
		PollInterval: time.Millisecond, StateTimeout: 2 * time.Second,
	}
}

func newHostSourceFixture(t *testing.T) hostSourceFixture {
	t.Helper()
	fly := newFakeFly(t)
	server := httptest.NewServer(fly.handler())
	t.Cleanup(server.Close)
	controller := newTestControllerKey(t)
	cfg := testHostSourceConfig(controller, server.URL, server.Client())
	cfg.Now = func() time.Time { return fly.now }
	source, err := NewHostSource(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return hostSourceFixture{fly: fly, source: source, controller: controller, cfg: cfg}
}

func (x hostSourceFixture) owned() map[string]string {
	return map[string]string{poolHostKey: "true", controllerIDKey: testController, poolNameKey: "pool", poolImageKey: imageTag(testPoolImage)}
}

func with(metadata map[string]string, key, value string) map[string]string {
	out := make(map[string]string, len(metadata))
	for k, v := range metadata {
		out[k] = v
	}
	if value == "" {
		delete(out, key)
	} else {
		out[key] = value
	}
	return out
}

// foreignMachines seeds one Machine for each way a Machine can fail to be a
// host of this pool.
func (x hostSourceFixture) foreignMachines() []string {
	owned := x.owned()
	foreign := map[string]map[string]string{
		"f1nopoolhost":   with(owned, poolHostKey, ""),
		"f2nocontroller": with(owned, controllerIDKey, ""),
		"f3nopool":       with(owned, poolNameKey, ""),
		"f4othercontrol": with(owned, controllerIDKey, "other-controller"),
		"f5otherpool":    with(owned, poolNameKey, "other"),
		"f6worker":       with(owned, managedByKey, "runneryard"),
		"f7nometadata":   nil,
	}
	ids := sortedKeys(foreign)
	for _, id := range ids {
		x.fly.addMachine(id, startedMachine, foreign[id])
	}
	return ids
}

func TestHostSourceListsOnlyThisPoolsMachines(t *testing.T) {
	x := newHostSourceFixture(t)
	x.foreignMachines()
	x.fly.addMachine("a1running", startedMachine, x.owned())
	x.fly.addMachine("a2stopped", stoppedMachine, x.owned())
	x.fly.addMachine("a3oldimage", stoppedMachine, with(x.owned(), poolImageKey, "0123456789abcdef"))
	x.fly.addMachine("a4destroyed", destroyedMachine, x.owned())
	x.fly.addMachine("a5creating", "created", x.owned())
	hosts, err := x.source.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]pool.Host{}
	for _, host := range hosts {
		got[host.ID] = host
	}
	if len(got) != 4 || got["fly-a1running"].ID == "" || got["fly-a2stopped"].ID == "" || got["fly-a3oldimage"].ID == "" || got["fly-a5creating"].ID == "" {
		t.Fatalf("hosts = %+v, want only this pool's four live Machines", hosts)
	}
	running := got["fly-a1running"]
	_, public, _ := x.controller.HostKey("ry-pool-a1running")
	if running.State != "running" || running.HostKey != public || running.Image != testPoolImage ||
		!strings.HasPrefix(running.Address, "[fdaa:0:1::") || !strings.HasSuffix(running.Address, "]:22") ||
		!running.StateSince.Equal(x.fly.now.Add(-10*time.Minute)) {
		t.Fatalf("running host = %+v", running)
	}
	if state := got["fly-a2stopped"].State; state != pool.HostStopped {
		t.Fatalf("stopped Machine has state %q, want %q", state, pool.HostStopped)
	}
	if image := got["fly-a3oldimage"].Image; image == testPoolImage || image == "" {
		t.Fatalf("a Machine of another image reported image %q", image)
	}
	if state := got["fly-a5creating"].State; state != "created" {
		t.Fatalf("a Machine being created reported state %q", state)
	}
	if writes := x.fly.writes(); len(writes) != 0 {
		t.Fatalf("listing wrote %v", writes)
	}
}

func TestHostSourceNeverTouchesAForeignMachine(t *testing.T) {
	x := newHostSourceFixture(t)
	ids := x.foreignMachines()
	ctx := context.Background()
	for _, id := range ids {
		for verb, call := range map[string]func(context.Context, string) error{
			"stop": x.source.Stop, "start": x.source.Start, "delete": x.source.Delete,
		} {
			if err := call(ctx, hostIDPrefix+id); err == nil {
				t.Errorf("%s of foreign Machine %s succeeded", verb, id)
			}
		}
	}
	for _, id := range []string{"e0001", "fly-", "fly-../other-app", "fly-E0001", "fly-a/b"} {
		if err := x.source.Delete(ctx, id); err == nil {
			t.Errorf("delete of host id %q succeeded", id)
		}
	}
	if writes := x.fly.writes(); len(writes) != 0 {
		t.Fatalf("foreign Machines received %v", writes)
	}
}

func TestHostSourceCreatesAMachineWithItsVolumeAndKeys(t *testing.T) {
	x := newHostSourceFixture(t)
	host, err := x.source.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if host.ID != "fly-e0002" || host.Image != testPoolImage {
		t.Fatalf("host = %+v", host)
	}
	var volume createVolumeRequest
	if err := json.Unmarshal(x.fly.bodies["POST /v1/apps/"+testPoolApp+"/volumes"], &volume); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(volume.Name, x.source.volumePrefix) || len(volume.Name) > 30 || volume.SizeGB != 50 ||
		!volume.Encrypted || volume.AutoBackupEnabled || volume.Region != "cdg" || volume.Compute.CPUKind != "performance" {
		t.Fatalf("volume request = %+v", volume)
	}
	body := x.fly.bodies["POST /v1/apps/"+testPoolApp+"/machines"]
	if strings.Contains(string(body), testToken) {
		t.Fatal("the Machine configuration contains the API token")
	}
	var request createPoolMachineRequest
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	config := request.Config
	if !strings.HasPrefix(request.Name, "ry-pool-") || request.Region != "cdg" || config.Image != testPoolImage ||
		config.Guest != (guestConfig{CPUs: 8, MemoryMB: 16384, CPUKind: "performance"}) ||
		config.AutoDestroy || config.Restart.Policy != "no" {
		t.Fatalf("machine request = %+v", request)
	}
	if len(config.Mounts) != 1 || config.Mounts[0].Volume != "vol_0001" || config.Mounts[0].Path != "/data" {
		t.Fatalf("mounts = %+v", config.Mounts)
	}
	if len(config.Processes) != 1 {
		t.Fatalf("processes = %+v", config.Processes)
	}
	process := config.Processes[0]
	if process.User != "root" || !slices.Equal(process.Entrypoint, []string{"/usr/local/bin/runneryard", "host", "boot"}) ||
		!process.IgnoreAppSecrets || len(process.Env) != 1 || process.Env["RUNNERYARD_DOCKER_DNS"] == "" {
		t.Fatalf("process = %+v", process)
	}
	if len(config.Metadata) != 4 || config.Metadata[poolHostKey] != "true" || config.Metadata[controllerIDKey] != testController ||
		config.Metadata[poolNameKey] != "pool" || config.Metadata[managedByKey] != "" {
		t.Fatalf("metadata = %v", config.Metadata)
	}
	files := map[string]machineFile{}
	for _, file := range config.Files {
		files[file.GuestPath] = file
	}
	decode := func(path string) string {
		raw, err := base64.StdEncoding.DecodeString(files[path].RawValue)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return string(raw)
	}
	// The OpenSSH encoding carries a random check value: compare the keys.
	_, public, _ := x.controller.HostKey(request.Name)
	signer, err := ssh.ParsePrivateKey([]byte(decode("/etc/runneryard/ssh_host_ed25519_key")))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) != public || files["/etc/runneryard/ssh_host_ed25519_key"].Mode != 0o600 ||
		strings.TrimSpace(decode("/etc/runneryard/controller.pub")) != x.controller.AuthorizedKey() {
		t.Fatalf("files = %v", sortedKeys(files))
	}
	var hostConfig map[string]any
	if err := json.Unmarshal([]byte(decode("/etc/runneryard/host.json")), &hostConfig); err != nil {
		t.Fatal(err)
	}
	if hostConfig["docker_root"] != "/data/docker" || hostConfig["slots"] != float64(4) || hostConfig["image"] != testPoolImage {
		t.Fatalf("host.json = %v", hostConfig)
	}
	if strings.Contains(decode("/etc/runneryard/host.json"), "BEGIN") {
		t.Fatal("host.json carries a key")
	}
}

func TestHostSourceCreateRejectedDeletesTheVolume(t *testing.T) {
	x := newHostSourceFixture(t)
	x.fly.failMachineCreate = http.StatusUnprocessableEntity
	if _, err := x.source.Create(context.Background()); err == nil {
		t.Fatal("create succeeded")
	}
	if len(x.fly.volumes) != 0 || len(x.fly.machines) != 0 {
		t.Fatalf("left behind volumes %v and machines %v", sortedKeys(x.fly.volumes), sortedKeys(x.fly.machines))
	}
}

func TestHostSourceCreateFindsTheMachineAfterATransientFailure(t *testing.T) {
	x := newHostSourceFixture(t)
	x.fly.failMachineCreate, x.fly.createAnyway = http.StatusBadGateway, true
	host, err := x.source.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if host.ID != "fly-e0002" || len(x.fly.calls("POST /v1/apps/"+testPoolApp+"/machines")) != 1 || len(x.fly.volumes) != 1 {
		t.Fatalf("host = %+v, requests = %v", host, x.fly.writes())
	}
}

func TestHostSourceDeleteRemovesTheMachineThenItsVolume(t *testing.T) {
	x := newHostSourceFixture(t)
	x.fly.volumeDetachReads = 2
	host, err := x.source.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := x.source.Delete(context.Background(), host.ID); err != nil {
		t.Fatal(err)
	}
	if machine := x.fly.machines["e0002"]; machine.State != destroyedMachine || len(x.fly.volumes) != 0 {
		t.Fatalf("machine %+v, volumes %v", machine, sortedKeys(x.fly.volumes))
	}
	if deletes := x.fly.calls("DELETE /v1/apps/" + testPoolApp + "/volumes/"); len(deletes) != 1 {
		t.Fatalf("volume deletes = %v", deletes)
	}
	// A Machine already gone is already deleted.
	if err := x.source.Delete(context.Background(), "fly-e9999"); err != nil {
		t.Fatalf("delete of a missing Machine = %v", err)
	}
}

func TestHostSourceStopAndStartWaitForTheMachine(t *testing.T) {
	x := newHostSourceFixture(t)
	x.fly.addMachine("a1", startedMachine, x.owned())
	ctx := context.Background()
	if err := x.source.Stop(ctx, "fly-a1"); err != nil {
		t.Fatal(err)
	}
	if state := x.fly.machines["a1"].State; state != stoppedMachine {
		t.Fatalf("after stop the Machine is %q", state)
	}
	// Stopping a stopped Machine sends nothing.
	if err := x.source.Stop(ctx, "fly-a1"); err != nil || len(x.fly.calls("POST /v1/apps/"+testPoolApp+"/machines/a1/stop")) != 1 {
		t.Fatalf("second stop = %v, requests = %v", err, x.fly.writes())
	}
	if err := x.source.Start(ctx, "fly-a1"); err != nil {
		t.Fatal(err)
	}
	if state := x.fly.machines["a1"].State; state != startedMachine {
		t.Fatalf("after start the Machine is %q", state)
	}
}

func TestHostSourceStartWithoutCapacityIsACapacityError(t *testing.T) {
	x := newHostSourceFixture(t)
	x.fly.addMachine("a1", stoppedMachine, x.owned())
	x.fly.failStart = "could not reserve resource for machine: insufficient memory available"
	err := x.source.Start(context.Background(), "fly-a1")
	var capacity *provider.CapacityError
	if !errors.As(err, &capacity) {
		t.Fatalf("start = %v, want a capacity error", err)
	}
	if starts := x.fly.calls("POST /v1/apps/" + testPoolApp + "/machines/a1/start"); len(starts) != 1 {
		t.Fatalf("a capacity refusal was retried: %v", starts)
	}
}

func TestHostSourceSweepsOnlyOrphanedVolumesOfThisPool(t *testing.T) {
	x := newHostSourceFixture(t)
	prefix := x.source.volumePrefix
	x.fly.addMachine("a1", stoppedMachine, x.owned(), "vol_mounted")
	x.fly.addVolume("vol_orphan", prefix+"00000001", "", time.Hour)
	x.fly.addVolume("vol_mounted", prefix+"00000002", "", time.Hour)
	x.fly.addVolume("vol_attached", prefix+"00000003", "a9", time.Hour)
	x.fly.addVolume("vol_young", prefix+"00000004", "", time.Minute)
	x.fly.addVolume("vol_foreign", "data", "", time.Hour)
	x.fly.addVolume("vol_otherpool", "ry_00000000_00000005", "", time.Hour)
	ctx := context.Background()
	if _, err := x.source.List(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := sortedKeys(x.fly.volumes), []string{"vol_attached", "vol_foreign", "vol_mounted", "vol_otherpool", "vol_young"}; !slices.Equal(got, want) {
		t.Fatalf("volumes after the sweep = %v, want %v", got, want)
	}
	// The next listing within the sweep interval does not list volumes.
	x.fly.addVolume("vol_orphan2", prefix+"00000006", "", time.Hour)
	if _, err := x.source.List(ctx); err != nil {
		t.Fatal(err)
	}
	listings := slices.DeleteFunc(x.fly.calls("GET /v1/apps/"+testPoolApp+"/volumes"), func(request string) bool {
		return strings.HasPrefix(request, "GET /v1/apps/"+testPoolApp+"/volumes/")
	})
	if x.fly.volumes["vol_orphan2"] == nil || len(listings) != 1 {
		t.Fatalf("the sweep ran again within %s", volumeSweepEvery)
	}
	x.fly.now = x.fly.now.Add(volumeSweepEvery)
	if _, err := x.source.List(ctx); err != nil {
		t.Fatal(err)
	}
	if x.fly.volumes["vol_orphan2"] != nil {
		t.Fatal("the next sweep kept an orphaned volume")
	}
}

func TestNewHostSourceValidatesTheConfiguration(t *testing.T) {
	controller := newTestControllerKey(t)
	valid := testHostSourceConfig(controller, "http://fly.invalid", http.DefaultClient)
	source, err := NewHostSource(valid)
	if err != nil {
		t.Fatal(err)
	}
	facts := source.Facts()
	if want, _ := HourlyPriceUSD("performance", 8, 16384, "cdg"); facts.Billing != pool.BillingPerSecond || facts.HourlyPrice != want || facts.Currency != "USD" || !facts.CanCreate {
		t.Fatalf("facts = %+v", facts)
	}
	for name, change := range map[string]func(*HostSourceConfig){
		"pool name":      func(c *HostSourceConfig) { c.Pool = "Pool_1" },
		"token":          func(c *HostSourceConfig) { c.APIToken = "" },
		"app":            func(c *HostSourceConfig) { c.App = "" },
		"controller key": func(c *HostSourceConfig) { c.AuthorizedKey = " " },
		"cpu kind":       func(c *HostSourceConfig) { c.CPUKind = "gpu" },
		"memory step":    func(c *HostSourceConfig) { c.MemoryMB = 16000 },
		"small volume":   func(c *HostSourceConfig) { c.VolumeGB = 5 },
		"large volume":   func(c *HostSourceConfig) { c.VolumeGB = 1000 },
		"slots":          func(c *HostSourceConfig) { c.Slots = 0 },
		"unpriced":       func(c *HostSourceConfig) { c.Region = "syd" },
		"negative price": func(c *HostSourceConfig) { c.HourlyPrice = -1 },
		"docker dns":     func(c *HostSourceConfig) { c.DockerDNS = "resolver.example" },
	} {
		cfg := valid
		change(&cfg)
		if _, err := NewHostSource(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	priced := valid
	priced.Region, priced.HourlyPrice = "syd", 0.5
	if source, err := NewHostSource(priced); err != nil || source.Facts().HourlyPrice != 0.5 {
		t.Fatalf("explicit price: %v", err)
	}
}

func TestHourlyPriceMatchesTheListedRates(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		cpus   int
		memory int
		region string
		want   float64
	}{
		// 8 performance vCPUs include 16 GB: 8 x 0.000012732 x 3600.
		{"performance", 8, 16384, "iad", 0.36668},
		{"performance", 8, 16384, "cdg", 0.41604},
		// 1 shared vCPU and 1 GB: 0.0000008465 + 0.75 x 0.000002316, per hour.
		{"shared", 1, 1024, "ewr", 0.00930},
	} {
		got, ok := HourlyPriceUSD(tc.kind, tc.cpus, tc.memory, tc.region)
		if !ok || math.Abs(got-tc.want) > 0.00001 {
			t.Errorf("%s %d CPUs %d MB in %s = %.5f, %v; want %.5f", tc.kind, tc.cpus, tc.memory, tc.region, got, ok, tc.want)
		}
	}
	if _, ok := HourlyPriceUSD("performance", 8, 16384, "nrt"); ok {
		t.Error("a region without a listed multiplier was priced")
	}
}
