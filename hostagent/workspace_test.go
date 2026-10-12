package hostagent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gwendall/runneryard/pool"
)

// testWorkRoot is an empty work root whose path --mount accepts: a test's
// TempDir can hold characters it would parse.
func testWorkRoot(t *testing.T) string {
	t.Helper()
	parent, err := os.MkdirTemp("", "runneryard-work")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	root := filepath.Join(parent, "workspaces")
	if !validWorkRoot(root) {
		t.Fatalf("temporary work root %q is not mountable", root)
	}
	return root
}

// writeTree makes dir hold what a job leaves: nested directories and files.
func writeTree(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "repo", "node_modules", "pkg"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"repo/README.md", "repo/node_modules/pkg/index.js"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("job output\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// workspaceNames returns the retired and the live directories in root.
func workspaceNames(t *testing.T, root string) (retired, live []string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), retiredWorkspacePrefix) {
			retired = append(retired, entry.Name())
		} else {
			live = append(live, entry.Name())
		}
	}
	return retired, live
}

func TestPrepareWorkspaceStaysInTheWorkRoot(t *testing.T) {
	t.Parallel()
	root := testWorkRoot(t)
	agent := newTestAgent(newFakeDocker())
	agent.WorkRoot = root
	agent.PurgeWorkspaces = func() {}
	if path, err := agent.prepareWorkspace(context.Background(), 0, "lease-0001"); err != nil || path != filepath.Join(root, "slot-0-lease-0001") {
		t.Fatalf("work directory = %q, %v", path, err)
	}
	for _, lease := range []string{"../escape", "a/b", "..", ".", "", "-lease", "lease,source=/", strings.Repeat("a", 64)} {
		if path, err := agent.prepareWorkspace(context.Background(), 0, lease); err == nil {
			t.Errorf("lease %q got work directory %q, want a refusal", lease, path)
		}
	}
	if entries, err := os.ReadDir(filepath.Dir(root)); err != nil || len(entries) != 1 || entries[0].Name() != "workspaces" {
		t.Fatalf("work root's parent holds %v (%v), want the work root only", entries, err)
	}
	if retired, live := workspaceNames(t, root); len(retired) != 0 || !slices.Equal(live, []string{"slot-0-lease-0001"}) {
		t.Fatalf("work root holds %q and %q: a refused lease retired or made a directory", retired, live)
	}
	for _, workRoot := range []string{"relative/work", "/srv/work,dst=/", "/srv/work=x", "/srv/../work", "/srv/work/", "/srv/work space"} {
		agent.WorkRoot = workRoot
		if path, err := agent.prepareWorkspace(context.Background(), 0, "lease-0001"); err == nil {
			t.Errorf("work root %q gave %q, want a refusal", workRoot, path)
		}
	}
}

func TestStartRefusesWhenItCannotPrepareAWorkDirectory(t *testing.T) {
	t.Parallel()
	docker := newFakeDocker()
	agent := newTestAgent(docker)
	agent.WorkRoot = testWorkRoot(t)
	// A file where the work root should be: the directory cannot be made.
	if err := os.MkdirAll(filepath.Dir(agent.WorkRoot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agent.WorkRoot, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	reply := agent.Handle(context.Background(), "start 0", requestBody(t, validStartRequest()))
	// not_ready sends the job to another host: nothing started here.
	requireRefusal(t, reply, pool.CodeNotReady)
	if runs := docker.find("run --detach"); len(runs) != 0 {
		t.Fatalf("docker run = %v, want no slot started without its work directory", runs)
	}
}

// A work directory that cannot be retired fails the removal, so the
// controller retries it instead of leaving the directory behind unnoticed.
func TestRemoveFailsWhenTheWorkDirectoryCannotBeRetired(t *testing.T) {
	t.Parallel()
	docker := newFakeDocker()
	docker.containers["runneryard-slot-1"] = map[string]string{labelSlot: "1", labelLease: "lease-0001"}
	agent := newTestAgent(docker)
	agent.WorkRoot = filepath.Join(testWorkRoot(t), "not-a-directory")
	if err := os.MkdirAll(filepath.Dir(agent.WorkRoot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agent.WorkRoot, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for attempt := range 2 {
		reply := agent.Handle(context.Background(), "remove 1 lease-0001", strings.NewReader(""))
		requireRefusal(t, reply, pool.CodeDockerFailed)
		if !strings.Contains(reply.Error.Message, "retire work directory") {
			t.Fatalf("attempt %d: message = %q", attempt, reply.Error.Message)
		}
	}
	if removals := docker.find("rm --force"); len(removals) != 1 {
		t.Fatalf("rm calls = %v, want the container removed once and the retry to find it gone", removals)
	}
}

func TestPurgeHelperDeletesOnlyRetiredDirectories(t *testing.T) {
	t.Parallel()
	docker := newFakeDocker()
	docker.image = LocalImageName(agentTestImage)
	docker.containers["runneryard-slot-1"] = map[string]string{labelSlot: "1", labelLease: "lease-0001"}
	agent := newTestAgent(docker)
	agent.LocalImage = docker.image
	agent.WorkRoot = testWorkRoot(t)
	writeTree(t, filepath.Join(agent.WorkRoot, "slot-1-lease-0001"))

	decodeOK(t, agent.Handle(context.Background(), "remove 1 lease-0001", strings.NewReader("")), nil)
	helpers := docker.find("run --rm")
	if len(helpers) != 1 {
		t.Fatalf("docker calls = %q, want one purge helper", docker.keys())
	}
	want := []string{
		"run", "--rm", "--detach",
		"--label", "runneryard.workspace-purge=true",
		"--network", "none", "--read-only", "--log-driver", "none",
		"--user", "0:0", "--cap-drop", "ALL", "--cap-add", "DAC_OVERRIDE", "--cap-add", "FOWNER",
		"--mount", "type=bind,source=" + agent.WorkRoot + ",target=/workspaces",
		"--pull", "never",
		"--entrypoint", "/usr/bin/find",
		"ghcr.io/acme/runner:sha256-2222", "/workspaces", "-mindepth", "1", "-maxdepth", "1", "-name", "deleting-*",
		"-exec", "rm", "-rf", "--", "{}", "+",
	}
	if !slices.Equal(helpers[0].args, want) {
		t.Fatalf("helper args =\n%q\nwant\n%q", helpers[0].args, want)
	}
	if helpers[0].env != nil {
		t.Fatalf("helper environment = %q, want none", helpers[0].env)
	}
	if labels := runLabels(helpers[0].args); labels[labelSlot] != "" {
		t.Fatal("the helper carries the slot label: list and boot would take it for a slot")
	}

	// A removal that retires nothing starts no helper.
	decodeOK(t, agent.Handle(context.Background(), "remove 1 lease-0001", strings.NewReader("")), nil)
	if helpers := docker.find("run --rm"); len(helpers) != 1 {
		t.Fatalf("purge helpers = %d after an empty removal, want still 1", len(helpers))
	}
}

func TestRetireWorkspacesAtBoot(t *testing.T) {
	t.Parallel()
	root := testWorkRoot(t)
	for _, name := range []string{"slot-0-lease-0001", "slot-12-lease-0002", "deleting-slot-3-lease-0003-abc"} {
		writeTree(t, filepath.Join(root, name))
	}
	if err := os.WriteFile(filepath.Join(root, "notes"), []byte("operator file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	retired, err := RetireWorkspaces(root)
	if err != nil || retired != 2 {
		t.Fatalf("retired = %d, %v, want both slot directories", retired, err)
	}
	names, live := workspaceNames(t, root)
	if len(names) != 3 || !slices.Contains(names, "deleting-slot-3-lease-0003-abc") {
		t.Fatalf("retired = %q, want the two slot directories and the one already retired", names)
	}
	if !slices.Equal(live, []string{"notes"}) {
		t.Fatalf("live = %q, want only the file that is no slot's", live)
	}
	if retired, err := RetireWorkspaces(filepath.Join(root, "missing")); err != nil || retired != 0 {
		t.Fatalf("missing root: retired = %d, %v, want nothing to do", retired, err)
	}
}

func TestWorkspacePurgerDeletesRetiredDirectories(t *testing.T) {
	t.Parallel()
	root := testWorkRoot(t)
	for _, name := range []string{"deleting-slot-0-a-x1", "deleting-slot-1-b-x2", "slot-2-c"} {
		writeTree(t, filepath.Join(root, name))
	}
	var logs bytes.Buffer
	purger := NewWorkspacePurger(root, slog.New(slog.NewTextHandler(&logs, nil)))
	purger.removeAll = func(path string) error {
		if filepath.Base(path) == "deleting-slot-1-b-x2" {
			return errors.New("operation not permitted")
		}
		return os.RemoveAll(path)
	}
	purger.purge()
	retired, live := workspaceNames(t, root)
	if !slices.Equal(retired, []string{"deleting-slot-1-b-x2"}) || !slices.Equal(live, []string{"slot-2-c"}) {
		t.Fatalf("retired %q, live %q: want the failed directory kept for the next pass and the live one untouched", retired, live)
	}
	if !strings.Contains(logs.String(), "could not delete a retired work directory") || !strings.Contains(logs.String(), "deleting-slot-1-b-x2") {
		t.Fatalf("logs = %q, want the failure reported", logs.String())
	}

	purger.removeAll = os.RemoveAll
	purger.purge()
	if retired, _ := workspaceNames(t, root); len(retired) != 0 {
		t.Fatalf("retired = %q after a pass that can delete, want none", retired)
	}
	// A work root not made yet is nothing to purge.
	NewWorkspacePurger(filepath.Join(root, "missing"), nil).purge()
}

// A large job's work directory takes a while to delete: neither the removal
// that retires it nor the listing that follows may wait for that.
func TestRemoveAndListDoNotWaitForThePurge(t *testing.T) {
	t.Parallel()
	docker := newFakeDocker()
	docker.containers["runneryard-slot-1"] = map[string]string{labelSlot: "1", labelLease: "lease-0001"}
	docker.script = func(args []string) ([]byte, error) {
		if commandKey(args) == "ps --all" {
			return []byte("\n"), nil
		}
		return nil, errUnscripted
	}
	agent := newTestAgent(docker)
	agent.WorkRoot = testWorkRoot(t)
	writeTree(t, filepath.Join(agent.WorkRoot, "slot-1-lease-0001"))

	deleting, release := make(chan string, 1), make(chan struct{})
	purger := NewWorkspacePurger(agent.WorkRoot, nil)
	purger.removeAll = func(path string) error {
		deleting <- path
		<-release
		return os.RemoveAll(path)
	}
	agent.PurgeWorkspaces = purger.Wake
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { purger.Run(ctx); close(stopped) }()
	t.Cleanup(func() { cancel(); <-stopped })

	answered := make(chan pool.Reply, 1)
	go func() { answered <- agent.Handle(context.Background(), "remove 1 lease-0001", strings.NewReader("")) }()
	select {
	case reply := <-answered:
		decodeOK(t, reply, nil)
	case <-time.After(5 * time.Second):
		t.Fatal("remove waited for the work directory's deletion")
	}
	select {
	case path := <-deleting:
		if !strings.HasPrefix(filepath.Base(path), "deleting-slot-1-lease-0001-") {
			t.Fatalf("purging %q, want slot 1's retired directory", path)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the removal did not wake the purger")
	}
	// The deletion is under way and blocked: commands still answer.
	go func() { answered <- agent.Handle(context.Background(), "list", strings.NewReader("")) }()
	select {
	case reply := <-answered:
		var slots []pool.Slot
		decodeOK(t, reply, &slots)
		if len(slots) != 0 {
			t.Fatalf("slots = %+v, want none", slots)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("list waited for the work directory's deletion")
	}
	purger.Wake()
	purger.Wake() // never blocks, even with a pass already due
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		retired, _ := workspaceNames(t, agent.WorkRoot)
		if len(retired) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("retired = %q, want the directory deleted once the purge can proceed", retired)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The previous job's directory is usually still being deleted when the next
// job starts: a start low on disk waits for it before dropping its caches.
func TestStartLowOnDiskWaitsForRetiredDirectories(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		retired     bool
		purgeFrees  uint64 // free disk once the purge ran; 0 leaves it stuck
		wantPurges  int
		wantDropped bool
	}{
		{name: "deletion frees enough", retired: true, purgeFrees: 30 << 30, wantPurges: 1},
		{name: "deletion frees too little", retired: true, purgeFrees: 12 << 30, wantPurges: 1, wantDropped: true},
		{name: "deletion stuck", retired: true, wantPurges: 1, wantDropped: true},
		{name: "nothing retired", wantDropped: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			docker := newFakeDocker()
			var mu sync.Mutex
			free := uint64(10 << 30)
			docker.onVolumeRemove = func() {
				mu.Lock()
				defer mu.Unlock()
				free = 30 << 30
			}
			agent := newTestAgent(docker)
			agent.WorkRoot = testWorkRoot(t)
			// Long enough for a deletion to be seen; a stuck one waits it out.
			agent.purgeWait = 5 * time.Second
			if tc.retired && tc.purgeFrees == 0 {
				agent.purgeWait = 300 * time.Millisecond
			}
			agent.FreeBytes = func(string) (uint64, error) {
				mu.Lock()
				defer mu.Unlock()
				return free, nil
			}
			retired := filepath.Join(agent.WorkRoot, retiredWorkspacePrefix+"slot-0-lease-0000-abc")
			if tc.retired {
				writeTree(t, retired)
			}
			purges := 0
			agent.PurgeWorkspaces = func() {
				mu.Lock()
				defer mu.Unlock()
				purges++
				if tc.purgeFrees == 0 {
					return
				}
				// The deletion runs in the background, after the start asked.
				go func() {
					time.Sleep(20 * time.Millisecond)
					mu.Lock()
					defer mu.Unlock()
					free = tc.purgeFrees
					_ = os.RemoveAll(retired)
				}()
			}
			reply := agent.Handle(context.Background(), "start 0", requestBody(t, validStartRequest()))
			decodeOK(t, reply, nil)
			if dropped := len(docker.find("volume rm")) == 1; dropped != tc.wantDropped {
				t.Fatalf("caches dropped = %v, want %v", dropped, tc.wantDropped)
			}
			mu.Lock()
			defer mu.Unlock()
			if purges != tc.wantPurges {
				t.Fatalf("purges = %d, want %d", purges, tc.wantPurges)
			}
		})
	}
}

func TestFreeDiskCountsTheWorkRootFilesystem(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		workErr error
		want    uint64
	}{
		{name: "fuller work root", want: 5 << 30},
		{name: "work root not created yet", workErr: os.ErrNotExist, want: 100 << 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			agent := newTestAgent(newFakeDocker())
			agent.Config.DockerRoot = "/srv/docker"
			agent.WorkRoot = "/srv/work"
			agent.FreeBytes = func(path string) (uint64, error) {
				switch path {
				case "/srv/docker":
					return 100 << 30, nil
				case "/srv/work":
					return 5 << 30, tc.workErr
				}
				return 0, errors.New("unexpected path " + path)
			}
			var info pool.HostInfo
			decodeOK(t, agent.Handle(context.Background(), "info", strings.NewReader("")), &info)
			if info.DiskFreeBytes != tc.want {
				t.Fatalf("free disk = %d GB, want %d GB", info.DiskFreeBytes>>30, tc.want>>30)
			}
		})
	}
}

// A job can leave links to anywhere in its work directory: deleting the
// directory deletes the links, never what they point to.
func TestWorkspacePurgerDoesNotFollowLinks(t *testing.T) {
	t.Parallel()
	root := testWorkRoot(t)
	outside := filepath.Join(filepath.Dir(root), "outside")
	writeTree(t, outside)
	retired := filepath.Join(root, "deleting-slot-0-lease-0001-x1")
	writeTree(t, retired)
	for name, target := range map[string]string{"repo/escape": outside, "repo/readme": filepath.Join(outside, "repo", "README.md"), "parent": ".."} {
		if err := os.Symlink(target, filepath.Join(retired, name)); err != nil {
			t.Fatal(err)
		}
	}
	NewWorkspacePurger(root, nil).purge()
	if _, err := os.Lstat(retired); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired directory still there: %v", err)
	}
	for _, name := range []string{"repo/README.md", "repo/node_modules/pkg/index.js"} {
		if _, err := os.Stat(filepath.Join(outside, name)); err != nil {
			t.Fatalf("%s outside the work root: %v", name, err)
		}
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("work root: %v", err)
	}
}
