package hostagent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gwendall/runneryard/pool"
)

const agentTestImage = "ghcr.io/acme/runner@sha256:2222"

var (
	agentTestNow = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	agentTestJIT = base64.StdEncoding.EncodeToString([]byte(`{".runner":"single-use-runner-secret"}`))
	// errUnscripted lets a fakeDocker script fall through to the model.
	errUnscripted = errors.New("unscripted")
)

type dockerCall struct {
	env  []string
	args []string
}

// commandKey is a docker command's first two words, such as "run --detach".
func commandKey(args []string) string { return strings.Join(args[:min(2, len(args))], " ") }

// fakeDocker models what a slot needs from the daemon: one image, the
// containers by name with their labels, and failures per command key.
type fakeDocker struct {
	mu         sync.Mutex
	calls      []dockerCall
	image      string
	containers map[string]map[string]string
	fail       map[string]error
	// failedRunLeaves is the lease label of the container a failed run leaves
	// behind; empty leaves nothing.
	failedRunLeaves string
	onVolumeRemove  func()
	// script answers before the model unless it returns errUnscripted.
	script func(args []string) ([]byte, error)
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{image: agentTestImage, containers: map[string]map[string]string{}, fail: map[string]error{}}
}

func (d *fakeDocker) Run(_ context.Context, env []string, args ...string) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, dockerCall{env: slices.Clone(env), args: slices.Clone(args)})
	if d.script != nil {
		if output, err := d.script(args); !errors.Is(err, errUnscripted) {
			return output, err
		}
	}
	key := commandKey(args)
	name := args[len(args)-1]
	if err := d.fail[key]; err != nil {
		if key == "run --detach" && d.failedRunLeaves != "" {
			labels := runLabels(args)
			labels[labelLease] = d.failedRunLeaves
			d.containers[flagValue(args, "--name")] = labels
		}
		return nil, err
	}
	switch {
	case key == "image inspect":
		if d.image == "" || name != d.image {
			return nil, errors.New("Error response from daemon: No such image: " + name)
		}
		return []byte("sha256:2222\n"), nil
	case key == "inspect --type", len(args) == 2 && args[0] == "inspect":
		labels, ok := d.containers[name]
		if !ok {
			return []byte("[]\n"), errors.New("Error: No such container: " + name)
		}
		return inspectJSON(inspectEntry{Name: name, Created: "2026-10-11T12:00:01Z", Status: "running", Labels: labels}), nil
	case key == "run --detach":
		d.containers[flagValue(args, "--name")] = runLabels(args)
		return []byte("4f9c0ffee\n"), nil
	case key == "rm --force":
		delete(d.containers, name)
		return []byte(name + "\n"), nil
	case key == "volume rm":
		if d.onVolumeRemove != nil {
			d.onVolumeRemove()
		}
		return []byte(strings.Join(args[3:], "\n") + "\n"), nil
	}
	return nil, fmt.Errorf("fake docker: unexpected command %q", args)
}

func (d *fakeDocker) snapshot() []dockerCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

func (d *fakeDocker) keys() []string {
	var keys []string
	for _, call := range d.snapshot() {
		keys = append(keys, commandKey(call.args))
	}
	return keys
}

func (d *fakeDocker) find(key string) []dockerCall {
	var found []dockerCall
	for _, call := range d.snapshot() {
		if commandKey(call.args) == key {
			found = append(found, call)
		}
	}
	return found
}

func runLabels(args []string) map[string]string {
	labels := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--label" {
			key, value, _ := strings.Cut(args[i+1], "=")
			labels[key] = value
		}
	}
	return labels
}

func flagValue(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

type inspectEntry struct {
	Name    string
	Created string
	Status  string
	Labels  map[string]string
}

// inspectJSON prints entries the way "docker inspect" does.
func inspectJSON(entries ...inspectEntry) []byte {
	out := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		out = append(out, map[string]any{
			"Id":      "4f9c0ffee",
			"Name":    "/" + entry.Name,
			"Created": entry.Created,
			"State":   map[string]any{"Status": entry.Status},
			"Config":  map[string]any{"Labels": entry.Labels},
		})
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		panic(err)
	}
	return encoded
}

type lockRecorder struct {
	mu       sync.Mutex
	locked   []int
	released int
}

func (l *lockRecorder) lock(slot int) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.locked = append(l.locked, slot)
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.released++
	}, nil
}

func newTestAgent(docker *fakeDocker) *Agent {
	return &Agent{
		Config: Config{
			Slots:         2,
			Image:         agentTestImage,
			Caches:        []string{"/home/runner/.cache", "/opt/hostedtoolcache"},
			MinFreeDiskGB: 20,
		},
		Docker:    docker,
		FreeBytes: func(string) (uint64, error) { return 100 << 30, nil },
		Lock:      func(int) (func(), error) { return func() {}, nil },
		Now:       func() time.Time { return agentTestNow },
		Version:   "1.2.3",
	}
}

func validStartRequest() pool.StartRequest {
	return pool.StartRequest{
		Image:              agentTestImage,
		ControllerID:       "ctl-main",
		LeaseID:            "lease-0001",
		RunnerName:         "ry-pool-0001",
		RunnerID:           4242,
		RunnerScaleSetID:   7,
		JITConfig:          agentTestJIT,
		Deadline:           agentTestNow.Add(2 * time.Hour),
		IdleTimeoutSeconds: 900,
	}
}

func requestBody(t *testing.T, request pool.StartRequest) io.Reader {
	t.Helper()
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(encoded)
}

func replyText(reply pool.Reply) string {
	encoded, _ := json.Marshal(reply)
	return string(encoded)
}

func decodeOK(t *testing.T, reply pool.Reply, into any) {
	t.Helper()
	if !reply.OK || reply.Error != nil {
		t.Fatalf("reply = %s, want ok", replyText(reply))
	}
	if into == nil {
		return
	}
	if err := json.Unmarshal(reply.Result, into); err != nil {
		t.Fatalf("decode result %s: %v", reply.Result, err)
	}
}

func requireRefusal(t *testing.T, reply pool.Reply, code string) {
	t.Helper()
	if reply.OK || reply.Error == nil || reply.Error.Code != code || len(reply.Result) != 0 {
		t.Fatalf("reply = %s, want refusal %q", replyText(reply), code)
	}
}

func sameSlots(got, want []pool.Slot) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		a, b := got[i], want[i]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return false
		}
		a.CreatedAt, b.CreatedAt = time.Time{}, time.Time{}
		if a != b {
			return false
		}
	}
	return true
}

func TestInfoReportsReadinessFromTheImage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		image     string
		freeBytes func(string) (uint64, error)
		want      pool.HostInfo
	}{
		{
			name:  "image present",
			image: agentTestImage,
			want:  pool.HostInfo{Version: "1.2.3", Slots: 2, Image: agentTestImage, Ready: true, DiskFreeBytes: 100 << 30},
		},
		{
			name: "image absent",
			want: pool.HostInfo{Version: "1.2.3", Slots: 2, Image: agentTestImage, DiskFreeBytes: 100 << 30},
		},
		{
			name:      "free space unknown",
			image:     agentTestImage,
			freeBytes: func(string) (uint64, error) { return 0, errors.New("statfs failed") },
			want:      pool.HostInfo{Version: "1.2.3", Slots: 2, Image: agentTestImage, Ready: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			docker := newFakeDocker()
			docker.image = tc.image
			agent := newTestAgent(docker)
			if tc.freeBytes != nil {
				agent.FreeBytes = tc.freeBytes
			}
			var info pool.HostInfo
			decodeOK(t, agent.Handle(context.Background(), "info", strings.NewReader("")), &info)
			if info != tc.want {
				t.Fatalf("info = %+v, want %+v", info, tc.want)
			}
			calls := docker.snapshot()
			if len(calls) != 1 || !slices.Equal(calls[0].args, []string{"image", "inspect", "--format", "{{.Id}}", agentTestImage}) {
				t.Fatalf("docker calls = %v, want one image inspect", calls)
			}
		})
	}
}

func TestHostWithoutConfigurationOnlyAnswersInfo(t *testing.T) {
	t.Parallel()
	docker := newFakeDocker()
	agent := &Agent{
		ConfigErr: errors.New("open /etc/runneryard/host.json: no such file or directory"),
		Docker:    docker,
		FreeBytes: func(string) (uint64, error) { return 100 << 30, nil },
		Now:       func() time.Time { return agentTestNow },
		Version:   "1.2.3",
	}
	var info pool.HostInfo
	decodeOK(t, agent.Handle(context.Background(), "info", strings.NewReader("")), &info)
	if want := (pool.HostInfo{Version: "1.2.3"}); info != want {
		t.Fatalf("info = %+v, want %+v", info, want)
	}
	for _, command := range []string{"list", "start 0", "remove 0 lease-0001"} {
		reply := agent.Handle(context.Background(), command, requestBody(t, validStartRequest()))
		requireRefusal(t, reply, pool.CodeNotReady)
	}
	if calls := docker.snapshot(); len(calls) != 0 {
		t.Fatalf("docker calls = %v, want none on an uninstalled host", calls)
	}
}

func TestListReportsSlotContainers(t *testing.T) {
	t.Parallel()
	own := map[string]string{
		labelSlot: "0", labelController: "ctl-main", labelLease: "lease-0001", labelRunnerName: "ry-pool-0001",
		labelRunnerID: "4242", labelScaleSetID: "7", labelImage: agentTestImage,
	}
	foreign := map[string]string{
		labelSlot: "1", labelController: "ctl-other", labelLease: "lease-9", labelRunnerName: "ry-other-9",
		labelRunnerID: "9", labelScaleSetID: "3",
	}
	docker := newFakeDocker()
	docker.script = func(args []string) ([]byte, error) {
		switch commandKey(args) {
		case "ps --all":
			return []byte("aaa\nbbb\nccc\nddd\neee\n"), nil
		case "inspect aaa":
			return inspectJSON(
				inspectEntry{Name: "runneryard-slot-0", Created: "2026-10-11T11:00:00.5Z", Status: "running", Labels: own},
				inspectEntry{Name: "runneryard-slot-1", Created: "2026-10-11T10:00:00Z", Status: "exited", Labels: foreign},
				inspectEntry{Name: "unlabelled", Created: "2026-10-11T10:00:00Z", Status: "running"},
				inspectEntry{Name: "bad-slot", Created: "2026-10-11T10:00:00Z", Status: "running", Labels: map[string]string{labelSlot: "one"}},
				inspectEntry{Name: "negative-slot", Created: "2026-10-11T10:00:00Z", Status: "running", Labels: map[string]string{labelSlot: "-1"}},
			), nil
		}
		return nil, errUnscripted
	}
	var slots []pool.Slot
	decodeOK(t, newTestAgent(docker).Handle(context.Background(), "list", strings.NewReader("")), &slots)
	want := []pool.Slot{
		{
			Index: 0, ControllerID: "ctl-main", LeaseID: "lease-0001", RunnerName: "ry-pool-0001", RunnerID: 4242,
			RunnerScaleSetID: 7, State: "started", CreatedAt: time.Date(2026, 10, 11, 11, 0, 0, 5e8, time.UTC),
		},
		// Another controller's container is reported, so the pool keeps off its slot.
		{
			Index: 1, ControllerID: "ctl-other", LeaseID: "lease-9", RunnerName: "ry-other-9", RunnerID: 9,
			RunnerScaleSetID: 3, State: "stopped", CreatedAt: time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC),
		},
	}
	if !sameSlots(slots, want) {
		t.Fatalf("slots = %+v, want %+v", slots, want)
	}
	calls := docker.snapshot()
	if len(calls) != 2 {
		t.Fatalf("docker calls = %v, want ps then inspect", calls)
	}
	if want := []string{"ps", "--all", "--no-trunc", "--filter", "label=runneryard.slot", "--format", "{{.ID}}"}; !slices.Equal(calls[0].args, want) {
		t.Fatalf("ps args = %q, want %q", calls[0].args, want)
	}
	if want := []string{"inspect", "aaa", "bbb", "ccc", "ddd", "eee"}; !slices.Equal(calls[1].args, want) {
		t.Fatalf("inspect args = %q, want %q", calls[1].args, want)
	}
}

func TestListToleratesAContainerRemovedDuringInspect(t *testing.T) {
	t.Parallel()
	docker := newFakeDocker()
	docker.script = func(args []string) ([]byte, error) {
		switch commandKey(args) {
		case "ps --all":
			return []byte("aaa\nbbb\n"), nil
		case "inspect aaa":
			labels := map[string]string{labelSlot: "1", labelController: "ctl-main", labelLease: "lease-0002", labelRunnerName: "ry-pool-0002"}
			return inspectJSON(inspectEntry{Name: "runneryard-slot-1", Created: "2026-10-11T11:00:00Z", Status: "created", Labels: labels}),
				errors.New("Error: No such object: bbb")
		}
		return nil, errUnscripted
	}
	var slots []pool.Slot
	decodeOK(t, newTestAgent(docker).Handle(context.Background(), "list", strings.NewReader("")), &slots)
	if len(slots) != 1 || slots[0].Index != 1 || slots[0].LeaseID != "lease-0002" || slots[0].State != "starting" {
		t.Fatalf("slots = %+v, want slot 1 starting", slots)
	}
}

func TestListFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		ps      []byte
		psErr   error
		inspect []byte
		err     error
		code    string
	}{
		{name: "no slot containers", ps: []byte("\n")},
		{name: "docker ps fails", psErr: errors.New("Cannot connect to the Docker daemon"), code: pool.CodeDockerFailed},
		{name: "inspect fails without output", ps: []byte("aaa\n"), err: errors.New("Cannot connect to the Docker daemon"), code: pool.CodeDockerFailed},
		{name: "inspect prints garbage", ps: []byte("aaa\n"), inspect: []byte("not json"), code: pool.CodeDockerFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			docker := newFakeDocker()
			docker.script = func(args []string) ([]byte, error) {
				switch commandKey(args) {
				case "ps --all":
					return tc.ps, tc.psErr
				case "inspect aaa":
					return tc.inspect, tc.err
				}
				return nil, errUnscripted
			}
			reply := newTestAgent(docker).Handle(context.Background(), "list", strings.NewReader(""))
			if tc.code != "" {
				requireRefusal(t, reply, tc.code)
				return
			}
			decodeOK(t, reply, nil)
			if string(reply.Result) != "[]" {
				t.Fatalf("result = %s, want an empty list, not null", reply.Result)
			}
			if keys := docker.keys(); !slices.Equal(keys, []string{"ps --all"}) {
				t.Fatalf("docker calls = %q, want ps only", keys)
			}
		})
	}
}

func TestWorkerStateMapsDockerStates(t *testing.T) {
	t.Parallel()
	for status, want := range map[string]string{
		"running": "started", "created": "starting", "restarting": "starting", "removing": "destroying",
		"exited": "stopped", "dead": "stopped", "paused": "stopped", "": "unknown", "frozen": "unknown",
	} {
		if got := workerState(status); got != want {
			t.Errorf("workerState(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestStartRunsTheSlotWithTheJITOnlyInItsEnvironment(t *testing.T) {
	t.Parallel()
	docker := newFakeDocker()
	agent := newTestAgent(docker)
	locks := &lockRecorder{}
	agent.Lock = locks.lock
	request := validStartRequest()
	reply := agent.Handle(context.Background(), "start 1", requestBody(t, request))

	var started pool.Slot
	decodeOK(t, reply, &started)
	want := pool.Slot{
		Index: 1, ControllerID: "ctl-main", LeaseID: "lease-0001", RunnerName: "ry-pool-0001", RunnerID: 4242,
		RunnerScaleSetID: 7, State: "started", CreatedAt: time.Date(2026, 10, 11, 12, 0, 1, 0, time.UTC),
	}
	if !sameSlots([]pool.Slot{started}, []pool.Slot{want}) {
		t.Fatalf("started = %+v, want %+v", started, want)
	}
	if strings.Contains(replyText(reply), agentTestJIT) {
		t.Fatal("the reply echoes the JIT configuration")
	}
	if keys, want := docker.keys(), []string{"inspect --type", "image inspect", "run --detach", "inspect runneryard-slot-1"}; !slices.Equal(keys, want) {
		t.Fatalf("docker calls = %q, want %q (no cache removal with free disk)", keys, want)
	}
	for _, call := range docker.snapshot() {
		for _, arg := range call.args {
			if strings.Contains(arg, agentTestJIT) {
				t.Fatalf("the JIT configuration is on a docker command line: %q", call.args)
			}
		}
		if commandKey(call.args) != "run --detach" && call.env != nil {
			t.Fatalf("docker %q got environment %q, want none", call.args, call.env)
		}
	}
	run := docker.find("run --detach")[0]
	if want := []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=" + agentTestJIT}; !slices.Equal(run.env, want) {
		t.Fatalf("run environment = %q, want only the JIT configuration", run.env)
	}
	wantArgs := []string{
		"run", "--detach",
		"--name", "runneryard-slot-1",
		"--hostname", "ry-pool-0001",
		"--privileged",
		"--label", "runneryard.slot=1",
		"--label", "runneryard.controller=ctl-main",
		"--label", "runneryard.lease=lease-0001",
		"--label", "runneryard.runner-name=ry-pool-0001",
		"--label", "runneryard.runner-id=4242",
		"--label", "runneryard.scale-set-id=7",
		"--label", "runneryard.image=" + agentTestImage,
		"--log-driver", "json-file", "--log-opt", "max-size=20m", "--log-opt", "max-file=2",
		"--env", "ACTIONS_RUNNER_INPUT_JITCONFIG",
		"--env", "RUNNERYARD_DEADLINE=2026-10-11T14:00:00Z",
		"--env", "RUNNERYARD_IDLE_TIMEOUT=900",
		"--env", "RUNNERYARD_REUSED_SLOT=1",
		"--mount", "type=volume,source=runneryard-slot-1-docker,target=/var/lib/docker",
		"--mount", "type=volume,source=runneryard-slot-1-cache-home-runner-.cache,target=/home/runner/.cache",
		"--mount", "type=volume,source=runneryard-slot-1-cache-opt-hostedtoolcache,target=/opt/hostedtoolcache",
		"--pull", "never",
		"--entrypoint", "/usr/bin/dumb-init",
		agentTestImage, "--", "/usr/local/bin/runner-entrypoint",
	}
	if !slices.Equal(run.args, wantArgs) {
		t.Fatalf("run args =\n%q\nwant\n%q", run.args, wantArgs)
	}
	if !slices.Equal(locks.locked, []int{1}) || locks.released != 1 {
		t.Fatalf("locks = %v released %d, want slot 1 locked and released once", locks.locked, locks.released)
	}
}

// A kept work directory gave a sparse checkout the previous job's full tree:
// every job must start in an empty workspace, as on a disposable worker.
func TestStartGivesEveryJobAnEmptyWorkspace(t *testing.T) {
	t.Parallel()
	docker := newFakeDocker()
	decodeOK(t, newTestAgent(docker).Handle(context.Background(), "start 0", requestBody(t, validStartRequest())), nil)
	run := docker.find("run --detach")[0]
	for i, arg := range run.args {
		if arg == "--mount" && strings.Contains(run.args[i+1], "/home/runner/_work") {
			t.Fatalf("slot mounts %q: the workspace would outlive the job", run.args[i+1])
		}
	}
	if keys := docker.keys(); slices.Contains(keys, "volume rm") {
		t.Fatalf("docker calls = %q: a start with free disk removes no volume", keys)
	}
}

func TestStartRunsTheLocalImageAndLabelsTheConfiguredOne(t *testing.T) {
	t.Parallel()
	docker := newFakeDocker()
	docker.image = LocalImageName(agentTestImage)
	agent := newTestAgent(docker)
	agent.LocalImage = docker.image
	reply := agent.Handle(context.Background(), "start 1", requestBody(t, validStartRequest()))

	var started pool.Slot
	decodeOK(t, reply, &started)
	run := docker.find("run --detach")[0]
	if image := run.args[len(run.args)-3]; image != "ghcr.io/acme/runner:sha256-2222" {
		t.Fatalf("slot runs %q, want the local name of the pinned image", image)
	}
	if label := runLabels(run.args)[labelImage]; label != agentTestImage {
		t.Fatalf("image label = %q, want the configured image %q", label, agentTestImage)
	}
}

func TestStartAppliesSlotLimitsAndAUTCDeadline(t *testing.T) {
	t.Parallel()
	docker := newFakeDocker()
	agent := newTestAgent(docker)
	agent.Config.SlotCPUs = 1.5
	agent.Config.SlotMemoryMB = 4096
	request := validStartRequest()
	request.Deadline = agentTestNow.Add(90*time.Minute + 500*time.Millisecond).In(time.FixedZone("CEST", 2*60*60))
	decodeOK(t, agent.Handle(context.Background(), "start 0", requestBody(t, request)), nil)
	args := docker.find("run --detach")[0].args
	for _, pair := range [][2]string{
		{"--cpus", "1.5"},
		{"--memory", "4096m"},
		{"--env", "RUNNERYARD_DEADLINE=2026-10-11T13:30:00Z"},
	} {
		if !hasPair(args, pair[0], pair[1]) {
			t.Fatalf("run args %q lack %s %s", args, pair[0], pair[1])
		}
	}
	if cpus, env := slices.Index(args, "--cpus"), slices.Index(args, "--env"); cpus > env {
		t.Fatalf("limits come after the environment in %q", args)
	}
}

func TestStartRefusesInvalidRequests(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*pool.StartRequest)
		body   *string
		code   string
	}{
		{name: "lease with a space", mutate: func(r *pool.StartRequest) { r.LeaseID = "lease 0001" }},
		{name: "lease with a slash", mutate: func(r *pool.StartRequest) { r.LeaseID = "../lease" }},
		{name: "lease with a shell character", mutate: func(r *pool.StartRequest) { r.LeaseID = "lease;id" }},
		{name: "empty lease", mutate: func(r *pool.StartRequest) { r.LeaseID = "" }},
		{name: "lease too long", mutate: func(r *pool.StartRequest) { r.LeaseID = strings.Repeat("a", 64) }},
		{name: "controller id with a quote", mutate: func(r *pool.StartRequest) { r.ControllerID = `ctl"main` }},
		{name: "runner name with an underscore", mutate: func(r *pool.StartRequest) { r.RunnerName = "ry_pool" }},
		{name: "runner name with a dot", mutate: func(r *pool.StartRequest) { r.RunnerName = "ry.pool" }},
		{name: "runner name starting with a hyphen", mutate: func(r *pool.StartRequest) { r.RunnerName = "-ry" }},
		{name: "runner name too long", mutate: func(r *pool.StartRequest) { r.RunnerName = strings.Repeat("r", 64) }},
		{name: "negative runner id", mutate: func(r *pool.StartRequest) { r.RunnerID = -1 }},
		{name: "negative scale set id", mutate: func(r *pool.StartRequest) { r.RunnerScaleSetID = -1 }},
		{name: "empty JIT", mutate: func(r *pool.StartRequest) { r.JITConfig = "" }},
		{name: "JIT with a space", mutate: func(r *pool.StartRequest) { r.JITConfig = "abc def" }},
		{name: "JIT with a newline", mutate: func(r *pool.StartRequest) { r.JITConfig = agentTestJIT + "\nX=1" }},
		{name: "JIT with shell characters", mutate: func(r *pool.StartRequest) { r.JITConfig = "abc$(id)" }},
		{name: "JIT in url-safe base64", mutate: func(r *pool.StartRequest) { r.JITConfig = "ab-c_d" }},
		{name: "oversized JIT", mutate: func(r *pool.StartRequest) { r.JITConfig = strings.Repeat("A", maxJITBytes+1) }},
		{name: "deadline in the past", mutate: func(r *pool.StartRequest) { r.Deadline = agentTestNow.Add(-time.Second) }},
		{name: "deadline now", mutate: func(r *pool.StartRequest) { r.Deadline = agentTestNow }},
		{name: "deadline missing", mutate: func(r *pool.StartRequest) { r.Deadline = time.Time{} }},
		{name: "deadline beyond 25h", mutate: func(r *pool.StartRequest) { r.Deadline = agentTestNow.Add(25*time.Hour + time.Second) }},
		{name: "negative idle timeout", mutate: func(r *pool.StartRequest) { r.IdleTimeoutSeconds = -1 }},
		{name: "idle timeout over a day", mutate: func(r *pool.StartRequest) { r.IdleTimeoutSeconds = 86_401 }},
		{name: "other image", mutate: func(r *pool.StartRequest) { r.Image = "ghcr.io/acme/runner@sha256:3333" }, code: pool.CodeImageMismatch},
		{name: "body not JSON", body: ptr("{")},
		{name: "empty body", body: ptr("")},
		{name: "oversized body", body: ptr(`{"pad":"` + strings.Repeat("A", maxRequestBytes) + `"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			docker := newFakeDocker()
			request := validStartRequest()
			if tc.mutate != nil {
				tc.mutate(&request)
			}
			body := requestBody(t, request)
			if tc.body != nil {
				body = strings.NewReader(*tc.body)
			}
			code := tc.code
			if code == "" {
				code = pool.CodeInvalid
			}
			reply := newTestAgent(docker).Handle(context.Background(), "start 0", body)
			requireRefusal(t, reply, code)
			if strings.Contains(replyText(reply), agentTestJIT) {
				t.Fatal("the refusal echoes the JIT configuration")
			}
			if calls := docker.snapshot(); len(calls) != 0 {
				t.Fatalf("docker calls = %v, want none for a refused request", calls)
			}
		})
	}
}

func ptr[T any](value T) *T { return &value }

func hasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestStartAcceptsBoundaryValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*pool.StartRequest)
	}{
		{name: "deadline exactly 25h out", mutate: func(r *pool.StartRequest) { r.Deadline = agentTestNow.Add(25 * time.Hour) }},
		{name: "no idle timeout", mutate: func(r *pool.StartRequest) { r.IdleTimeoutSeconds = 0 }},
		{name: "idle timeout of a day", mutate: func(r *pool.StartRequest) { r.IdleTimeoutSeconds = 86_400 }},
		{name: "largest JIT", mutate: func(r *pool.StartRequest) { r.JITConfig = strings.Repeat("A", maxJITBytes) }},
		{name: "longest lease", mutate: func(r *pool.StartRequest) { r.LeaseID = "l" + strings.Repeat("a._-", 15) + "zz" }},
		{name: "longest runner name", mutate: func(r *pool.StartRequest) { r.RunnerName = strings.Repeat("r", 63) }},
		{name: "unregistered runner", mutate: func(r *pool.StartRequest) { r.RunnerID, r.RunnerScaleSetID = 0, 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			request := validStartRequest()
			tc.mutate(&request)
			decodeOK(t, newTestAgent(newFakeDocker()).Handle(context.Background(), "start 0", requestBody(t, request)), nil)
		})
	}
}

func TestStartRefusesABusyOrUnreadySlot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		prepare func(*fakeDocker)
		code    string
		keys    []string
	}{
		{
			name: "slot holds a container",
			prepare: func(d *fakeDocker) {
				d.containers["runneryard-slot-0"] = map[string]string{labelSlot: "0", labelLease: "lease-other"}
			},
			code: pool.CodeSlotBusy,
			keys: []string{"inspect --type"},
		},
		{
			name:    "daemon unreachable",
			prepare: func(d *fakeDocker) { d.fail["inspect --type"] = errors.New("Cannot connect to the Docker daemon") },
			code:    pool.CodeDockerFailed,
			keys:    []string{"inspect --type"},
		},
		{
			name:    "image absent",
			prepare: func(d *fakeDocker) { d.image = "" },
			code:    pool.CodeNotReady,
			keys:    []string{"inspect --type", "image inspect"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			docker := newFakeDocker()
			tc.prepare(docker)
			reply := newTestAgent(docker).Handle(context.Background(), "start 0", requestBody(t, validStartRequest()))
			requireRefusal(t, reply, tc.code)
			if keys := docker.keys(); !slices.Equal(keys, tc.keys) {
				t.Fatalf("docker calls = %q, want %q", keys, tc.keys)
			}
		})
	}
}

func TestStartDropsSlotCachesWhenDiskIsLow(t *testing.T) {
	t.Parallel()
	slotVolumes := []string{
		"volume", "rm", "--force",
		"runneryard-slot-0-docker",
		"runneryard-slot-0-cache-home-runner-.cache", "runneryard-slot-0-cache-opt-hostedtoolcache",
		"runneryard-slot-0-work",
	}
	cases := []struct {
		name       string
		freeBefore uint64
		freeAfter  uint64
		freeErr    error
		removeErr  error
		minFreeGB  int
		code       string
		keys       []string
	}{
		{
			name: "caches dropped, then the job starts cold", freeBefore: 10 << 30, freeAfter: 30 << 30, minFreeGB: 20,
			keys: []string{"inspect --type", "image inspect", "volume rm", "run --detach", "inspect runneryard-slot-0"},
		},
		{
			name: "still low after dropping caches", freeBefore: 10 << 30, freeAfter: 19 << 30, minFreeGB: 20,
			code: pool.CodeDiskFull, keys: []string{"inspect --type", "image inspect", "volume rm"},
		},
		{
			name: "caches cannot be removed", freeBefore: 10 << 30, freeAfter: 30 << 30, minFreeGB: 20,
			removeErr: errors.New("volume is in use"),
			code:      pool.CodeDiskFull, keys: []string{"inspect --type", "image inspect", "volume rm"},
		},
		{
			name: "no minimum configured", freeBefore: 0, freeAfter: 0, minFreeGB: 0,
			keys: []string{"inspect --type", "image inspect", "run --detach", "inspect runneryard-slot-0"},
		},
		{
			name: "free space unknown", freeErr: errors.New("statfs failed"), minFreeGB: 20,
			keys: []string{"inspect --type", "image inspect", "run --detach", "inspect runneryard-slot-0"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			docker := newFakeDocker()
			if tc.removeErr != nil {
				docker.fail["volume rm"] = tc.removeErr
			}
			var mu sync.Mutex
			free := tc.freeBefore
			var paths []string
			docker.onVolumeRemove = func() {
				mu.Lock()
				defer mu.Unlock()
				free = tc.freeAfter
			}
			agent := newTestAgent(docker)
			agent.Config.MinFreeDiskGB = tc.minFreeGB
			agent.Config.DockerRoot = "/srv/docker"
			agent.FreeBytes = func(path string) (uint64, error) {
				mu.Lock()
				defer mu.Unlock()
				paths = append(paths, path)
				return free, tc.freeErr
			}
			reply := agent.Handle(context.Background(), "start 0", requestBody(t, validStartRequest()))
			if tc.code != "" {
				requireRefusal(t, reply, tc.code)
			} else {
				decodeOK(t, reply, nil)
			}
			if keys := docker.keys(); !slices.Equal(keys, tc.keys) {
				t.Fatalf("docker calls = %q, want %q", keys, tc.keys)
			}
			if removals := docker.find("volume rm"); len(removals) == 1 && !slices.Equal(removals[0].args, slotVolumes) {
				t.Fatalf("volume rm args = %q, want %q", removals[0].args, slotVolumes)
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.minFreeGB == 0 && len(paths) != 0 {
				t.Fatalf("free space read %d times without a minimum", len(paths))
			}
			for _, path := range paths {
				if path != "/srv/docker" {
					t.Fatalf("free space read on %q, want the configured docker root", path)
				}
			}
		})
	}
}

func TestFailedRunRemovesOnlyTheContainerItCreated(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		leaves    string
		wantRm    bool
		remaining bool
	}{
		{name: "half-created container of this lease", leaves: "lease-0001", wantRm: true},
		{name: "container of another lease", leaves: "lease-other", remaining: true},
		{name: "nothing created"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			docker := newFakeDocker()
			docker.fail["run --detach"] = errors.New("docker: Error response from daemon: failed to create task")
			docker.failedRunLeaves = tc.leaves
			reply := newTestAgent(docker).Handle(context.Background(), "start 0", requestBody(t, validStartRequest()))
			requireRefusal(t, reply, pool.CodeDockerFailed)
			if !strings.Contains(reply.Error.Message, "failed to create task") || strings.Contains(replyText(reply), agentTestJIT) {
				t.Fatalf("message = %q, want the docker error without the JIT", reply.Error.Message)
			}
			removals := docker.find("rm --force")
			if tc.wantRm != (len(removals) == 1) {
				t.Fatalf("rm calls = %v, want removal %v", removals, tc.wantRm)
			}
			if tc.wantRm && !slices.Equal(removals[0].args, []string{"rm", "--force", "runneryard-slot-0"}) {
				t.Fatalf("rm args = %q", removals[0].args)
			}
			docker.mu.Lock()
			_, remaining := docker.containers["runneryard-slot-0"]
			docker.mu.Unlock()
			if remaining != tc.remaining {
				t.Fatalf("container remaining = %v, want %v", remaining, tc.remaining)
			}
		})
	}
}

func TestStartedContainerThatCannotBeInspectedIsAmbiguous(t *testing.T) {
	t.Parallel()
	docker := newFakeDocker()
	docker.fail["inspect runneryard-slot-0"] = errors.New("Cannot connect to the Docker daemon")
	reply := newTestAgent(docker).Handle(context.Background(), "start 0", requestBody(t, validStartRequest()))
	// docker_failed is what the pool treats as a possibly started worker; the
	// running container must be kept for removal by lease.
	requireRefusal(t, reply, pool.CodeDockerFailed)
	if removals := docker.find("rm --force"); len(removals) != 0 {
		t.Fatalf("rm calls = %v, want the started container kept", removals)
	}
}

func TestRemoveIsLeaseScoped(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		holder   string
		fail     map[string]error
		code     string
		removed  *bool
		wantRm   bool
		keepsJob bool
	}{
		{name: "empty slot", removed: ptr(false)},
		{name: "slot of another lease", holder: "lease-other", code: pool.CodeLeaseMismatch, keepsJob: true},
		{name: "slot of this lease", holder: "lease-0001", removed: ptr(true), wantRm: true},
		{
			name: "docker rm fails", holder: "lease-0001", fail: map[string]error{"rm --force": errors.New("removal in progress")},
			code: pool.CodeDockerFailed, wantRm: true, keepsJob: true,
		},
		{
			name: "daemon unreachable", holder: "lease-0001", fail: map[string]error{"inspect --type": errors.New("Cannot connect to the Docker daemon")},
			code: pool.CodeDockerFailed, keepsJob: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			docker := newFakeDocker()
			if tc.holder != "" {
				docker.containers["runneryard-slot-1"] = map[string]string{labelSlot: "1", labelLease: tc.holder}
			}
			for key, err := range tc.fail {
				docker.fail[key] = err
			}
			agent := newTestAgent(docker)
			locks := &lockRecorder{}
			agent.Lock = locks.lock
			reply := agent.Handle(context.Background(), "remove 1 lease-0001", strings.NewReader(""))
			if tc.code != "" {
				requireRefusal(t, reply, tc.code)
			} else {
				var result map[string]bool
				decodeOK(t, reply, &result)
				if result["removed"] != *tc.removed {
					t.Fatalf("result = %v, want removed %v", result, *tc.removed)
				}
			}
			removals := docker.find("rm --force")
			if tc.wantRm != (len(removals) == 1) || (tc.wantRm && !slices.Equal(removals[0].args, []string{"rm", "--force", "runneryard-slot-1"})) {
				t.Fatalf("rm calls = %v, want removal %v", removals, tc.wantRm)
			}
			docker.mu.Lock()
			_, remaining := docker.containers["runneryard-slot-1"]
			docker.mu.Unlock()
			if remaining != tc.keepsJob {
				t.Fatalf("container remaining = %v, want %v", remaining, tc.keepsJob)
			}
			if !slices.Equal(locks.locked, []int{1}) || locks.released != 1 {
				t.Fatalf("locks = %v released %d, want slot 1 locked and released once", locks.locked, locks.released)
			}
		})
	}
}

func TestHandleRejectsMalformedCommands(t *testing.T) {
	t.Parallel()
	for _, command := range []string{
		"", "   ", "INFO", "destroy 0", "sh -c id", "info now", "list all",
		"start", "start 0 1", "start 2", "start -1", "start 01", "start +1", "start one", "start 0x1",
		"remove", "remove 0", "remove 0 lease-0001 extra", "remove 2 lease-0001", "remove x lease-0001",
		"remove 0 ../lease", "remove 0 lease;id", "remove 0 -lease",
	} {
		docker := newFakeDocker()
		agent := newTestAgent(docker)
		agent.Lock = func(int) (func(), error) {
			t.Errorf("%q took a slot lock", command)
			return func() {}, nil
		}
		reply := agent.Handle(context.Background(), command, requestBody(t, validStartRequest()))
		if reply.OK || reply.Error == nil || reply.Error.Code != pool.CodeInvalid {
			t.Errorf("%q: reply = %s, want invalid", command, replyText(reply))
		}
		if calls := docker.snapshot(); len(calls) != 0 {
			t.Errorf("%q: docker calls = %v, want none", command, calls)
		}
	}
}

func TestLockFailureRefusesBeforeDocker(t *testing.T) {
	t.Parallel()
	for _, command := range []string{"start 0", "remove 0 lease-0001"} {
		docker := newFakeDocker()
		agent := newTestAgent(docker)
		agent.Lock = func(int) (func(), error) { return nil, errors.New("lock directory is read-only") }
		requireRefusal(t, agent.Handle(context.Background(), command, requestBody(t, validStartRequest())), pool.CodeDockerFailed)
		if calls := docker.snapshot(); len(calls) != 0 {
			t.Fatalf("%q: docker calls = %v, want none without the slot lock", command, calls)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	base := func() Config {
		return Config{Slots: 2, Image: agentTestImage, Caches: slices.Clone(DefaultCaches), MinFreeDiskGB: 20}
	}
	valid := map[string]func(*Config){
		"defaults":         func(*Config) {},
		"no caches":        func(c *Config) { c.Caches = nil },
		"opt cache":        func(c *Config) { c.Caches = []string{"/opt/hostedtoolcache"} },
		"nested cache":     func(c *Config) { c.Caches = []string{"/home/runner/go", "/home/runner/go/pkg/mod"} },
		"eight caches":     func(c *Config) { c.Caches = cachePaths(8) },
		"one slot":         func(c *Config) { c.Slots = 1 },
		"64 slots":         func(c *Config) { c.Slots = 64 },
		"limits":           func(c *Config) { c.SlotCPUs, c.SlotMemoryMB = 0.5, 512 },
		"no disk minimum":  func(c *Config) { c.MinFreeDiskGB = 0 },
		"docker root":      func(c *Config) { c.DockerRoot = "/srv/docker" },
		"tagged image":     func(c *Config) { c.Image = "ghcr.io/acme/runner:2.330.0" },
		"dotted cache dir": func(c *Config) { c.Caches = []string{"/home/runner/..cache"} },
	}
	for name, mutate := range valid {
		cfg := base()
		mutate(&cfg)
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s: Validate() = %v, want nil", name, err)
		}
	}
	invalid := map[string]func(*Config){
		"no slots":                  func(c *Config) { c.Slots = 0 },
		"65 slots":                  func(c *Config) { c.Slots = 65 },
		"no image":                  func(c *Config) { c.Image = "" },
		"image with a space":        func(c *Config) { c.Image = "ghcr.io/acme/runner latest" },
		"image as an option":        func(c *Config) { c.Image = "--privileged" },
		"image with a semicolon":    func(c *Config) { c.Image = "runner;id" },
		"negative CPUs":             func(c *Config) { c.SlotCPUs = -1 },
		"too many CPUs":             func(c *Config) { c.SlotCPUs = 513 },
		"negative memory":           func(c *Config) { c.SlotMemoryMB = -1 },
		"too little memory":         func(c *Config) { c.SlotMemoryMB = 511 },
		"negative disk minimum":     func(c *Config) { c.MinFreeDiskGB = -1 },
		"huge disk minimum":         func(c *Config) { c.MinFreeDiskGB = 10_001 },
		"relative docker root":      func(c *Config) { c.DockerRoot = "srv/docker" },
		"nine caches":               func(c *Config) { c.Caches = cachePaths(9) },
		"duplicate cache":           func(c *Config) { c.Caches = []string{"/home/runner/.npm", "/home/runner/.npm"} },
		"caches sharing a volume":   func(c *Config) { c.Caches = []string{"/opt/hosted-toolcache", "/opt/hosted/toolcache"} },
		"runner home":               func(c *Config) { c.Caches = []string{"/home/runner"} },
		"runner home with a slash":  func(c *Config) { c.Caches = []string{"/home/runner/"} },
		"opt itself":                func(c *Config) { c.Caches = []string{"/opt"} },
		"opt with a slash":          func(c *Config) { c.Caches = []string{"/opt/"} },
		"work directory":            func(c *Config) { c.Caches = []string{"/home/runner/_work"} },
		"docker data":               func(c *Config) { c.Caches = []string{"/var/lib/docker"} },
		"system path":               func(c *Config) { c.Caches = []string{"/etc/ssh"} },
		"sibling of runner home":    func(c *Config) { c.Caches = []string{"/home/runnerx/cache"} },
		"relative path":             func(c *Config) { c.Caches = []string{"home/runner/.cache"} },
		"parent traversal":          func(c *Config) { c.Caches = []string{"/home/runner/../../etc"} },
		"dot component":             func(c *Config) { c.Caches = []string{"/home/runner/./cache"} },
		"double slash":              func(c *Config) { c.Caches = []string{"/home/runner//cache"} },
		"trailing slash":            func(c *Config) { c.Caches = []string{"/home/runner/cache/"} },
		"space":                     func(c *Config) { c.Caches = []string{"/home/runner/my cache"} },
		"mount option injection":    func(c *Config) { c.Caches = []string{"/home/runner/x,target=/etc"} },
		"mount option with equals":  func(c *Config) { c.Caches = []string{"/home/runner/readonly=true"} },
		"empty cache path":          func(c *Config) { c.Caches = []string{""} },
		"cache path with a newline": func(c *Config) { c.Caches = []string{"/home/runner/a\nb"} },
	}
	for name, mutate := range invalid {
		cfg := base()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want an error", name)
		}
	}
}

func cachePaths(n int) []string {
	paths := make([]string, n)
	for i := range paths {
		paths[i] = fmt.Sprintf("/home/runner/cache%d", i)
	}
	return paths
}

func TestLoadConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, contents string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cfg, err := LoadConfig(write("valid.json", `{"slots":4,"image":"`+agentTestImage+`","caches":["/home/runner/.cache"],"min_free_disk_gb":30}`))
	if err != nil || cfg.Slots != 4 || cfg.Image != agentTestImage || cfg.MinFreeDiskGB != 30 || !slices.Equal(cfg.Caches, []string{"/home/runner/.cache"}) {
		t.Fatalf("LoadConfig(valid) = %+v, %v", cfg, err)
	}
	if _, err := LoadConfig(write("garbage.json", "{")); err == nil {
		t.Fatal("LoadConfig(garbage) = nil error")
	}
	if _, err := LoadConfig(write("invalid.json", `{"slots":0,"image":"`+agentTestImage+`"}`)); err == nil {
		t.Fatal("LoadConfig(zero slots) = nil error")
	}
	if _, err := LoadConfig(filepath.Join(dir, "missing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadConfig(missing) = %v, want not exist", err)
	}
}
