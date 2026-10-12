package hostagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gwendall/runneryard/pool"
	"golang.org/x/crypto/ssh"
)

func newControllerKey(t *testing.T) *pool.ControllerKey {
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

type sshFixture struct {
	controller *pool.ControllerKey
	host       pool.Host
	docker     *fakeDocker
}

// startSSHServer serves a test agent on a loopback port with the host key
// the controller derives for "fly-test".
func startSSHServer(t *testing.T) sshFixture {
	t.Helper()
	controller := newControllerKey(t)
	private, public, err := controller.HostKey("fly-test")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	authorized, _, _, _, err := ssh.ParseAuthorizedKey([]byte(controller.AuthorizedKey()))
	if err != nil {
		t.Fatal(err)
	}
	docker := newFakeDocker()
	server := &SSHServer{
		Agent: newTestAgent(docker), HostKey: signer, AuthorizedKey: authorized,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return sshFixture{
		controller: controller,
		host:       pool.Host{ID: "fly-test", Address: listener.Addr().String(), HostKey: public},
		docker:     docker,
	}
}

func TestSSHServerAnswersTheControllerThroughTheRealClient(t *testing.T) {
	fixture := startSSHServer(t)
	client, err := pool.NewSSHAgent(pool.SSHAgentConfig{Signer: fixture.controller.Signer()})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx := context.Background()
	info, err := client.Info(ctx, fixture.host)
	if err != nil || !info.Ready || info.Slots != 2 || info.Image != agentTestImage {
		t.Fatalf("info = %+v, %v", info, err)
	}
	started, err := client.Start(ctx, fixture.host, 1, validStartRequest())
	if err != nil || started.LeaseID != "lease-0001" || started.Index != 1 {
		t.Fatalf("start = %+v, %v", started, err)
	}
	runs := fixture.docker.find("run --detach")
	if len(runs) != 1 || !slices.Contains(runs[0].env, "ACTIONS_RUNNER_INPUT_JITCONFIG="+agentTestJIT) {
		t.Fatalf("docker run calls = %+v, want the JIT configuration in the environment", runs)
	}
	if slices.ContainsFunc(runs[0].args, func(arg string) bool { return strings.Contains(arg, agentTestJIT) }) {
		t.Fatal("the JIT configuration reached docker's arguments")
	}
	if err := client.Remove(ctx, fixture.host, 1, "other-lease"); !pool.IsAgentRefusal(err, pool.CodeLeaseMismatch) {
		t.Fatalf("remove with another lease = %v, want lease_mismatch", err)
	}
	if err := client.Remove(ctx, fixture.host, 1, "lease-0001"); err != nil {
		t.Fatal(err)
	}
	// Several verbs at once share one connection.
	errs := make(chan error, 6)
	for range 6 {
		go func() {
			_, err := client.Info(ctx, fixture.host)
			errs <- err
		}()
	}
	for range 6 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestSSHServerAdmitsOnlyTheControllerKeyAsTheAgentUser(t *testing.T) {
	fixture := startSSHServer(t)
	stranger := newControllerKey(t)
	for name, cfg := range map[string]pool.SSHAgentConfig{
		"other key":  {Signer: stranger.Signer()},
		"other user": {Signer: fixture.controller.Signer(), User: "root"},
	} {
		client, err := pool.NewSSHAgent(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Info(context.Background(), fixture.host); !errors.Is(err, pool.ErrUnreachable) {
			t.Fatalf("%s: info = %v, want the connection refused", name, err)
		}
		client.Close()
	}
	// The client pins the host key: a server with another key is refused.
	other := fixture.host
	_, other.HostKey, _ = stranger.HostKey("fly-test")
	client, err := pool.NewSSHAgent(pool.SSHAgentConfig{Signer: fixture.controller.Signer()})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Info(context.Background(), other); err == nil {
		t.Fatal("the client accepted a host key it did not pin")
	}
}

func TestSSHServerRefusesShellsPtysAndForwarding(t *testing.T) {
	fixture := startSSHServer(t)
	pinned, _, _, _, err := ssh.ParseAuthorizedKey([]byte(fixture.host.HostKey))
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.Dial("tcp", fixture.host.Address, &ssh.ClientConfig{
		User: AgentUser, Auth: []ssh.AuthMethod{ssh.PublicKeys(fixture.controller.Signer())},
		HostKeyCallback: ssh.FixedHostKey(pinned), Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err == nil {
		t.Fatal("the server granted a pty")
	}
	if err := session.Setenv("LD_PRELOAD", "/tmp/x.so"); err == nil {
		t.Fatal("the server accepted an environment variable")
	}
	if err := session.Shell(); err == nil {
		t.Fatal("the server started a shell")
	}
	_ = session.Close()
	if _, err := client.Dial("tcp", "127.0.0.1:22"); err == nil {
		t.Fatal("the server forwarded a TCP connection")
	}
	// An unknown verb is answered, not run.
	session, err = client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	output, err := session.Output("sh -c id")
	if err != nil || !strings.Contains(string(output), pool.CodeInvalid) {
		t.Fatalf("exec of a shell command = %q, %v; want an invalid refusal", output, err)
	}
}

func TestRemoveLeftoverSlotsRemovesOnlySlotContainers(t *testing.T) {
	docker := newFakeDocker()
	docker.script = func(args []string) ([]byte, error) {
		if commandKey(args) == "ps --all" {
			if !slices.Contains(args, "label="+labelSlot) {
				t.Errorf("leftover listing %v is not filtered on the slot label", args)
			}
			return []byte("4f9c0ffee\n5a0d1eaf\n"), nil
		}
		return nil, errUnscripted
	}
	removed, err := RemoveLeftoverSlots(context.Background(), docker)
	if err != nil || removed != 2 {
		t.Fatalf("removed = %d, %v", removed, err)
	}
	if calls := docker.find("rm --force"); len(calls) != 1 || !slices.Equal(calls[0].args[2:], []string{"4f9c0ffee", "5a0d1eaf"}) {
		t.Fatalf("rm calls = %+v", calls)
	}

	empty := newFakeDocker()
	empty.script = func(args []string) ([]byte, error) {
		if commandKey(args) == "ps --all" {
			return []byte("\n"), nil
		}
		return nil, errUnscripted
	}
	if removed, err := RemoveLeftoverSlots(context.Background(), empty); err != nil || removed != 0 || len(empty.find("rm --force")) != 0 {
		t.Fatalf("empty host: removed = %d, %v, calls = %v", removed, err, empty.keys())
	}
}

func TestEnsureImagePullsUntilThePullSucceeds(t *testing.T) {
	docker := newFakeDocker()
	pulls := 0
	docker.script = func(args []string) ([]byte, error) {
		switch commandKey(args) {
		case "image inspect":
			return nil, errors.New("No such image")
		case "pull --quiet":
			pulls++
			if pulls < 3 {
				return nil, errors.New("registry unavailable")
			}
			return []byte("ok\n"), nil
		}
		return nil, errUnscripted
	}
	var waits []time.Duration
	EnsureImage(context.Background(), docker, agentTestImage, agentTestImage, nil, slog.New(slog.DiscardHandler), func(d time.Duration) { waits = append(waits, d) })
	if pulls != 3 || !slices.Equal(waits, []time.Duration{5 * time.Second, 10 * time.Second}) {
		t.Fatalf("pulls = %d, waits = %v", pulls, waits)
	}
	if tags := docker.find("tag"); len(tags) != 0 {
		t.Fatalf("tagged %q, want no tag when the local name is the image", tags[0].args)
	}

	present := newFakeDocker()
	EnsureImage(context.Background(), present, agentTestImage, agentTestImage, failingImport(t), slog.New(slog.DiscardHandler), func(time.Duration) {})
	if len(present.find("pull --quiet")) != 0 {
		t.Fatal("pulled an image that was present")
	}
}

func TestEnsureImageImportsTheRootFilesystemBeforePulling(t *testing.T) {
	local := LocalImageName(agentTestImage)
	docker := newFakeDocker()
	docker.image = ""
	var imported []string
	EnsureImage(context.Background(), docker, agentTestImage, local, func(_ context.Context, name string) error {
		imported = append(imported, name)
		return nil
	}, slog.New(slog.DiscardHandler), func(time.Duration) { t.Fatal("waited after a successful import") })
	if !slices.Equal(imported, []string{local}) {
		t.Fatalf("imported %q, want the local name %q", imported, local)
	}
	if keys := docker.keys(); !slices.Equal(keys, []string{"image inspect"}) {
		t.Fatalf("docker calls = %q, want only the presence check", keys)
	}
	if name := docker.snapshot()[0].args[4]; name != local {
		t.Fatalf("checked presence of %q, want the local name %q", name, local)
	}
}

func TestEnsureImagePullsAndTagsWhenTheImportFails(t *testing.T) {
	local := LocalImageName(agentTestImage)
	docker := newFakeDocker()
	docker.script = func(args []string) ([]byte, error) {
		switch commandKey(args) {
		case "image inspect":
			return nil, errors.New("No such image")
		case "pull --quiet", "tag " + agentTestImage:
			return nil, nil
		}
		return nil, errUnscripted
	}
	EnsureImage(context.Background(), docker, agentTestImage, local, func(context.Context, string) error {
		return errors.New("the root filesystem is not an overlay over a block device")
	}, slog.New(slog.DiscardHandler), func(time.Duration) {})
	pulls, tags := docker.find("pull --quiet"), docker.find("tag "+agentTestImage)
	if len(pulls) != 1 || pulls[0].args[2] != agentTestImage {
		t.Fatalf("pulls = %v, want one pull of the configured image", pulls)
	}
	if len(tags) != 1 || !slices.Equal(tags[0].args, []string{"tag", agentTestImage, local}) {
		t.Fatalf("tags = %v, want the pulled image tagged with the local name", tags)
	}
}

func failingImport(t *testing.T) func(context.Context, string) error {
	return func(context.Context, string) error {
		t.Helper()
		t.Fatal("imported an image that was present")
		return nil
	}
}

func TestDockerDaemonConfigKeepsDataOnTheVolume(t *testing.T) {
	config, err := DockerDaemonConfig("/data/docker", []string{"1.1.1.1", " 8.8.8.8"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(config), `{"data-root":"/data/docker","dns":["1.1.1.1","8.8.8.8"],"features":{"buildkit":true}}`+"\n"; got != want {
		t.Fatalf("daemon.json = %s, want %s", got, want)
	}
	for _, dns := range [][]string{{"1.1.1.1", "1.1.1.1"}, {"resolver"}, {"1.1.1.1", "1.0.0.1", "8.8.8.8", "8.8.4.4"}} {
		if _, err := DockerDaemonConfig("/data/docker", dns); err == nil {
			t.Fatalf("dns %v accepted", dns)
		}
	}
}

func TestLoadBootKeysRefusesAKeyWithOptions(t *testing.T) {
	controller := newControllerKey(t)
	private, _, err := controller.HostKey("fly-test")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	hostKey := filepath.Join(dir, "host")
	authorized := filepath.Join(dir, "controller.pub")
	if err := os.WriteFile(hostKey, private, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authorized, []byte(controller.AuthorizedKey()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, key, err := LoadBootKeys(hostKey, authorized); err != nil || string(ssh.MarshalAuthorizedKey(key)) != controller.AuthorizedKey()+"\n" {
		t.Fatalf("keys = %v, %v", key, err)
	}
	if err := os.WriteFile(authorized, []byte(`command="sh" `+controller.AuthorizedKey()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadBootKeys(hostKey, authorized); err == nil {
		t.Fatal("a controller key with options was accepted")
	}
}

func TestSeparateFilesystemRefusesTheRootFilesystem(t *testing.T) {
	if err := SeparateFilesystem("/"); err == nil {
		t.Fatal("the root filesystem was accepted as the data volume")
	}
	if err := SeparateFilesystem(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing data volume was accepted")
	}
}
