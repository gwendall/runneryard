package pool_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gwendall/runneryard/hostagent"
	"github.com/gwendall/runneryard/pool"
	"golang.org/x/crypto/ssh"
)

const sshTestImage = "ghcr.io/acme/runner@sha256:1111"

var sshTestNow = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

// agentHandler answers one exec request with the agent's standard output.
type agentHandler func(command string, stdin []byte) []byte

type sshExec struct {
	command string
	stdin   []byte
}

// sshTestServer stands in for sshd on loopback: it presents the host key
// derived for its name, accepts only the controller key for the runneryard
// user, and answers every exec request through its handler.
type sshTestServer struct {
	listener net.Listener
	config   *ssh.ServerConfig
	handle   agentHandler
	done     chan struct{}

	mu         sync.Mutex
	conns      []*ssh.ServerConn
	handshakes int
	execs      []sshExec
}

func startSSHTestServer(t *testing.T, controller *pool.ControllerKey, name string, handle agentHandler) (*sshTestServer, pool.Host) {
	t.Helper()
	signer, _, public := hostKeyPair(t, controller, name)
	allowed := controller.Signer().PublicKey().Marshal()
	server := &sshTestServer{handle: handle, done: make(chan struct{})}
	server.config = &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if meta.User() != "runneryard" || !bytes.Equal(key.Marshal(), allowed) {
				return nil, errors.New("key not authorized")
			}
			return nil, nil
		},
	}
	server.config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server.listener = listener
	go server.serve()
	t.Cleanup(server.close)
	return server, pool.Host{ID: name, Address: listener.Addr().String(), HostKey: public}
}

func (s *sshTestServer) serve() {
	defer close(s.done)
	for {
		raw, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.serveConn(raw)
	}
}

func (s *sshTestServer) serveConn(raw net.Conn) {
	conn, channels, requests, err := ssh.NewServerConn(raw, s.config)
	if err != nil {
		_ = raw.Close()
		return
	}
	s.mu.Lock()
	s.conns = append(s.conns, conn)
	s.handshakes++
	s.mu.Unlock()
	go ssh.DiscardRequests(requests)
	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "sessions only")
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go s.serveSession(channel, channelRequests)
	}
}

func (s *sshTestServer) serveSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	for request := range requests {
		var exec struct{ Command string }
		if request.Type != "exec" || ssh.Unmarshal(request.Payload, &exec) != nil {
			_ = request.Reply(false, nil)
			continue
		}
		_ = request.Reply(true, nil)
		go ssh.DiscardRequests(requests)
		stdin, _ := io.ReadAll(channel)
		s.mu.Lock()
		s.execs = append(s.execs, sshExec{command: exec.Command, stdin: stdin})
		s.mu.Unlock()
		_, _ = channel.Write(s.handle(exec.Command, stdin))
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
		return
	}
}

// dropConnections closes every accepted connection from the server side.
func (s *sshTestServer) dropConnections() {
	s.mu.Lock()
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (s *sshTestServer) close() {
	_ = s.listener.Close()
	<-s.done
	s.dropConnections()
}

func (s *sshTestServer) recorded() []sshExec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.execs)
}

func (s *sshTestServer) commands() []string {
	var commands []string
	for _, exec := range s.recorded() {
		commands = append(commands, exec.command)
	}
	return commands
}

func (s *sshTestServer) handshakeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handshakes
}

func mustJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

func okReply(result any) []byte {
	return append(mustJSON(pool.Reply{OK: true, Result: mustJSON(result)}), '\n')
}

func refusalReply(code, message string) []byte {
	return append(mustJSON(pool.Reply{Error: &pool.ReplyError{Code: code, Message: message}}), '\n')
}

func newTestSSHAgent(t *testing.T, signer ssh.Signer, configure ...func(*pool.SSHAgentConfig)) *pool.SSHAgent {
	t.Helper()
	cfg := pool.SSHAgentConfig{Signer: signer, DialTimeout: 5 * time.Second}
	for _, apply := range configure {
		apply(&cfg)
	}
	agent, err := pool.NewSSHAgent(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(agent.Close)
	return agent
}

func sshStartRequest(lease string) pool.StartRequest {
	return pool.StartRequest{
		Image:              sshTestImage,
		ControllerID:       "ctl-main",
		LeaseID:            lease,
		RunnerName:         "ry-" + lease,
		RunnerID:           33,
		RunnerScaleSetID:   7,
		JITConfig:          "c2luZ2xlLXVzZS1qaXQtc2VjcmV0",
		Deadline:           sshTestNow.Add(time.Hour),
		IdleTimeoutSeconds: 600,
	}
}

func TestSSHAgentRoundTrip(t *testing.T) {
	t.Parallel()
	controller := generateControllerKey(t)
	created := time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)
	info := pool.HostInfo{Version: "0.5.0", Slots: 4, Image: sshTestImage, Ready: true, DiskFreeBytes: 80 << 30}
	listed := []pool.Slot{
		{Index: 0, ControllerID: "ctl-main", LeaseID: "lease-1", RunnerName: "ry-lease-1", RunnerID: 11, RunnerScaleSetID: 7, State: "started", CreatedAt: created},
		{Index: 2, ControllerID: "ctl-other", LeaseID: "lease-9", RunnerName: "ry-lease-9", RunnerID: 99, RunnerScaleSetID: 3, State: "stopped", CreatedAt: created},
	}
	started := pool.Slot{Index: 3, ControllerID: "ctl-main", LeaseID: "lease-3", RunnerName: "ry-lease-3", RunnerID: 33, RunnerScaleSetID: 7, State: "starting", CreatedAt: created}
	server, host := startSSHTestServer(t, controller, "h1", func(command string, _ []byte) []byte {
		switch command {
		case "info":
			return okReply(info)
		case "list":
			return okReply(listed)
		case "start 3":
			return okReply(started)
		case "remove 3 lease-3":
			return okReply(map[string]bool{"removed": true})
		}
		return refusalReply(pool.CodeInvalid, "unknown command")
	})
	agent := newTestSSHAgent(t, controller.Signer())
	ctx := context.Background()

	gotInfo, err := agent.Info(ctx, host)
	if err != nil || gotInfo != info {
		t.Fatalf("Info() = %+v, %v, want %+v", gotInfo, err, info)
	}
	gotSlots, err := agent.List(ctx, host)
	if err != nil || !bytes.Equal(mustJSON(gotSlots), mustJSON(listed)) {
		t.Fatalf("List() = %+v, %v, want %+v", gotSlots, err, listed)
	}
	request := sshStartRequest("lease-3")
	gotSlot, err := agent.Start(ctx, host, 3, request)
	if err != nil || !bytes.Equal(mustJSON(gotSlot), mustJSON(started)) {
		t.Fatalf("Start() = %+v, %v, want %+v", gotSlot, err, started)
	}
	if err := agent.Remove(ctx, host, 3, "lease-3"); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	execs := server.recorded()
	if commands := server.commands(); !slices.Equal(commands, []string{"info", "list", "start 3", "remove 3 lease-3"}) {
		t.Fatalf("commands = %q", commands)
	}
	for _, exec := range execs {
		if strings.Contains(exec.command, request.JITConfig) {
			t.Fatalf("the JIT configuration is on the command line: %q", exec.command)
		}
		if exec.command != "start 3" && len(exec.stdin) != 0 {
			t.Fatalf("%q got stdin %q, want none", exec.command, exec.stdin)
		}
	}
	var sent pool.StartRequest
	if err := json.Unmarshal(execs[2].stdin, &sent); err != nil {
		t.Fatalf("start stdin %q: %v", execs[2].stdin, err)
	}
	if !bytes.Equal(mustJSON(sent), mustJSON(request)) {
		t.Fatalf("start stdin = %+v, want %+v", sent, request)
	}
	if count := server.handshakeCount(); count != 1 {
		t.Fatalf("handshakes = %d, want one connection reused by every call", count)
	}

	agent.Close()
	if _, err := agent.Info(ctx, host); err != nil {
		t.Fatalf("Info() after Close() error = %v", err)
	}
	if count := server.handshakeCount(); count != 2 {
		t.Fatalf("handshakes after Close() = %d, want a new connection", count)
	}
}

// hostDocker is just enough of the Docker CLI for the host agent: the image
// is present and a started container is listed until it is removed.
type hostDocker struct {
	mu         sync.Mutex
	containers map[string]map[string]string
	envs       [][]string
}

func (d *hostDocker) Run(_ context.Context, env []string, args ...string) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(env) > 0 {
		d.envs = append(d.envs, slices.Clone(env))
	}
	switch args[0] {
	case "image":
		return []byte("sha256:1111\n"), nil
	case "run":
		labels, name := map[string]string{}, ""
		for i := 0; i+1 < len(args); i++ {
			switch args[i] {
			case "--label":
				key, value, _ := strings.Cut(args[i+1], "=")
				labels[key] = value
			case "--name":
				name = args[i+1]
			}
		}
		d.containers[name] = labels
		return []byte("4f9c0ffee\n"), nil
	case "rm":
		delete(d.containers, args[len(args)-1])
		return nil, nil
	case "ps":
		var names []string
		for name := range d.containers {
			names = append(names, name)
		}
		slices.Sort(names)
		return []byte(strings.Join(names, "\n")), nil
	case "inspect":
		names := args[1:]
		if args[1] == "--type" {
			names = args[3:]
		}
		out := make([]map[string]any, 0, len(names))
		for _, name := range names {
			labels, ok := d.containers[name]
			if !ok {
				return mustJSON(out), errors.New("Error: No such object: " + name)
			}
			out = append(out, map[string]any{
				"Name": "/" + name, "Created": "2026-10-11T12:00:01Z",
				"State": map[string]any{"Status": "running"}, "Config": map[string]any{"Labels": labels},
			})
		}
		return mustJSON(out), nil
	}
	return nil, fmt.Errorf("unexpected docker command %q", args)
}

func TestSSHAgentDrivesTheHostAgent(t *testing.T) {
	t.Parallel()
	controller := generateControllerKey(t)
	docker := &hostDocker{containers: map[string]map[string]string{}}
	onHost := &hostagent.Agent{
		Config:    hostagent.Config{Slots: 2, Image: sshTestImage, Caches: hostagent.DefaultCaches, MinFreeDiskGB: 20},
		Docker:    docker,
		FreeBytes: func(string) (uint64, error) { return 50 << 30, nil },
		Now:       func() time.Time { return sshTestNow },
		Version:   "0.5.0",
	}
	var handling sync.Mutex
	_, host := startSSHTestServer(t, controller, "h1", func(command string, stdin []byte) []byte {
		handling.Lock()
		defer handling.Unlock()
		return mustJSON(onHost.Handle(context.Background(), command, bytes.NewReader(stdin)))
	})
	client := newTestSSHAgent(t, controller.Signer())
	ctx := context.Background()

	info, err := client.Info(ctx, host)
	if err != nil || !info.Ready || info.Slots != 2 || info.Image != sshTestImage || info.Version != "0.5.0" {
		t.Fatalf("Info() = %+v, %v", info, err)
	}
	request := sshStartRequest("lease-1")
	slot, err := client.Start(ctx, host, 1, request)
	if err != nil || slot.Index != 1 || slot.LeaseID != "lease-1" || slot.ControllerID != "ctl-main" || slot.RunnerID != 33 || slot.State != "started" {
		t.Fatalf("Start() = %+v, %v", slot, err)
	}
	if _, err := client.Start(ctx, host, 1, sshStartRequest("lease-2")); !pool.IsAgentRefusal(err, pool.CodeSlotBusy) || errors.Is(err, pool.ErrUnreachable) {
		t.Fatalf("Start() on a busy slot error = %v, want slot_busy", err)
	}
	if _, err := client.Start(ctx, host, 2, request); !pool.IsAgentRefusal(err, pool.CodeInvalid) {
		t.Fatalf("Start() on slot 2 of 2 error = %v, want invalid", err)
	}
	slots, err := client.List(ctx, host)
	if err != nil || len(slots) != 1 || slots[0].Index != 1 || slots[0].LeaseID != "lease-1" {
		t.Fatalf("List() = %+v, %v", slots, err)
	}
	if err := client.Remove(ctx, host, 1, "lease-2"); !pool.IsAgentRefusal(err, pool.CodeLeaseMismatch) {
		t.Fatalf("Remove() with another lease error = %v, want lease_mismatch", err)
	}
	for range 2 {
		if err := client.Remove(ctx, host, 1, "lease-1"); err != nil {
			t.Fatalf("Remove() error = %v, want removal then an already empty slot", err)
		}
	}
	if slots, err := client.List(ctx, host); err != nil || len(slots) != 0 {
		t.Fatalf("List() after Remove() = %+v, %v", slots, err)
	}
	docker.mu.Lock()
	defer docker.mu.Unlock()
	if want := [][]string{{"ACTIONS_RUNNER_INPUT_JITCONFIG=" + request.JITConfig}}; !slices.EqualFunc(docker.envs, want, slices.Equal) {
		t.Fatalf("docker environments = %q, want the JIT configuration once", docker.envs)
	}
}

func TestSSHAgentNeverTrustsAnUnpinnedHostKey(t *testing.T) {
	t.Parallel()
	controller := generateControllerKey(t)
	_, _, otherHostKey := hostKeyPair(t, controller, "h2")
	cases := map[string]string{
		"another host's key": otherHostKey,
		"no pinned key":      "",
		"garbage pinned key": "ssh-ed25519 not-base64",
	}
	for name, pinned := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server, host := startSSHTestServer(t, controller, "h1", func(string, []byte) []byte {
				return okReply(pool.HostInfo{Ready: true})
			})
			host.HostKey = pinned
			var dials atomic.Int32
			agent := newTestSSHAgent(t, controller.Signer(), func(cfg *pool.SSHAgentConfig) {
				cfg.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
					dials.Add(1)
					var dialer net.Dialer
					return dialer.DialContext(ctx, network, address)
				}
			})
			_, err := agent.Info(context.Background(), host)
			if !errors.Is(err, pool.ErrUnreachable) {
				t.Fatalf("Info() error = %v, want ErrUnreachable", err)
			}
			if commands := server.commands(); len(commands) != 0 {
				t.Fatalf("commands = %q, want none against an unpinned key", commands)
			}
			if count := server.handshakeCount(); count != 0 {
				t.Fatalf("handshakes = %d, want none", count)
			}
			if pinned != otherHostKey && dials.Load() != 0 {
				t.Fatalf("dialed %d times without a valid pinned key", dials.Load())
			}
		})
	}
}

func TestSSHAgentFailsWhenTheHostRefusesTheControllerKey(t *testing.T) {
	t.Parallel()
	controller := generateControllerKey(t)
	server, host := startSSHTestServer(t, controller, "h1", func(string, []byte) []byte {
		return okReply(pool.HostInfo{Ready: true})
	})
	stranger := newTestSSHAgent(t, generateControllerKey(t).Signer())
	if _, err := stranger.Info(context.Background(), host); !errors.Is(err, pool.ErrUnreachable) {
		t.Fatalf("Info() with another controller key error = %v, want ErrUnreachable", err)
	}
	if commands := server.commands(); len(commands) != 0 {
		t.Fatalf("commands = %q, want none", commands)
	}
}

func TestSSHAgentReportsWhatTheHostAgentSaid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		output  []byte
		code    string
		message string
	}{
		{name: "stated refusal", output: refusalReply(pool.CodeSlotBusy, "slot already holds a container"), code: pool.CodeSlotBusy},
		{name: "refusal without a code", output: append(mustJSON(pool.Reply{}), '\n'), message: "failed without a reason"},
		{name: "no reply", output: []byte("sh: runneryard: not found\n"), message: "returned no reply"},
		{name: "undecodable result", output: okReply(map[string]string{"slot": "three"}), message: "decode start reply"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			controller := generateControllerKey(t)
			_, host := startSSHTestServer(t, controller, "h1", func(string, []byte) []byte { return tc.output })
			agent := newTestSSHAgent(t, controller.Signer())
			_, err := agent.Start(context.Background(), host, 0, sshStartRequest("lease-1"))
			if err == nil || errors.Is(err, pool.ErrUnreachable) {
				t.Fatalf("Start() error = %v, want a non-transport failure", err)
			}
			var refusal *pool.AgentError
			if tc.code == "" {
				if errors.As(err, &refusal) || !strings.Contains(err.Error(), tc.message) {
					t.Fatalf("Start() error = %v, want %q and no stated refusal", err, tc.message)
				}
				return
			}
			if !errors.As(err, &refusal) || refusal.Code != tc.code || refusal.Message != "slot already holds a container" {
				t.Fatalf("Start() error = %#v, want AgentError %s", err, tc.code)
			}
			if !pool.IsAgentRefusal(err, tc.code) || pool.IsAgentRefusal(err, pool.CodeInvalid) {
				t.Fatalf("IsAgentRefusal mismatch for %v", err)
			}
		})
	}
}

func TestSSHAgentDeadlineAfterTheCommandStartedIsAmbiguous(t *testing.T) {
	t.Parallel()
	controller := generateControllerKey(t)
	entered := make(chan string, 1)
	release := make(chan struct{})
	server, host := startSSHTestServer(t, controller, "h1", func(command string, _ []byte) []byte {
		if command != "info" {
			entered <- command
			<-release
		}
		return okReply(pool.HostInfo{Ready: true})
	})
	t.Cleanup(sync.OnceFunc(func() { close(release) }))
	agent := newTestSSHAgent(t, controller.Signer())
	if _, err := agent.Info(context.Background(), host); err != nil {
		t.Fatalf("Info() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := agent.Start(ctx, host, 0, sshStartRequest("lease-1"))
	if err == nil || errors.Is(err, pool.ErrUnreachable) || pool.IsAgentRefusal(err, "") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start() error = %v, want an ambiguous deadline error", err)
	}
	select {
	case command := <-entered:
		if command != "start 0" {
			t.Fatalf("host received %q, want start 0", command)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the start command never reached the host")
	}
	// The interrupted call gave its session back.
	if _, err := agent.Info(context.Background(), host); err != nil {
		t.Fatalf("Info() after the interrupted call error = %v", err)
	}
	if count := server.handshakeCount(); count != 1 {
		t.Fatalf("handshakes = %d, want the connection kept", count)
	}
}

func TestSSHAgentCallWaitingForASessionDidNotRun(t *testing.T) {
	t.Parallel()
	controller := generateControllerKey(t)
	entered := make(chan string, 1)
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	server, host := startSSHTestServer(t, controller, "h1", func(command string, _ []byte) []byte {
		if command != "info" {
			entered <- command
			<-release
			return okReply(pool.Slot{Index: 0, LeaseID: "lease-1", State: "started"})
		}
		return okReply(pool.HostInfo{Ready: true})
	})
	t.Cleanup(releaseOnce)
	agent := newTestSSHAgent(t, controller.Signer(), func(cfg *pool.SSHAgentConfig) { cfg.MaxSessions = 1 })
	if _, err := agent.Info(context.Background(), host); err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	first := make(chan error, 1)
	go func() {
		_, err := agent.Start(context.Background(), host, 0, sshStartRequest("lease-1"))
		first <- err
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := agent.Info(ctx, host); !errors.Is(err, pool.ErrUnreachable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued Info() error = %v, want ErrUnreachable from the deadline", err)
	}
	releaseOnce()
	if err := <-first; err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if commands := server.commands(); !slices.Equal(commands, []string{"info", "start 0"}) {
		t.Fatalf("commands = %q, want the queued call never sent", commands)
	}
}

func TestSSHAgentRedialsADroppedConnection(t *testing.T) {
	t.Parallel()
	controller := generateControllerKey(t)
	server, host := startSSHTestServer(t, controller, "h1", func(string, []byte) []byte {
		return okReply(pool.HostInfo{Ready: true})
	})
	agent := newTestSSHAgent(t, controller.Signer())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := agent.Info(ctx, host); err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	server.dropConnections()
	if _, err := agent.Info(ctx, host); err != nil {
		t.Fatalf("Info() after the host dropped the connection error = %v", err)
	}
	if count := server.handshakeCount(); count != 2 {
		t.Fatalf("handshakes = %d, want a new connection", count)
	}
}

func TestSSHAgentReconnectsWhenAHostChangesIdentity(t *testing.T) {
	t.Parallel()
	controller := generateControllerKey(t)
	before, host := startSSHTestServer(t, controller, "h1", func(string, []byte) []byte {
		return okReply(pool.HostInfo{Version: "before"})
	})
	after, rebuilt := startSSHTestServer(t, controller, "h1-rebuilt", func(string, []byte) []byte {
		return okReply(pool.HostInfo{Version: "after"})
	})
	rebuilt.ID = host.ID
	agent := newTestSSHAgent(t, controller.Signer())
	if info, err := agent.Info(context.Background(), host); err != nil || info.Version != "before" {
		t.Fatalf("Info() = %+v, %v", info, err)
	}
	if info, err := agent.Info(context.Background(), rebuilt); err != nil || info.Version != "after" {
		t.Fatalf("Info() on the rebuilt host = %+v, %v, want the new address and key", info, err)
	}
	if len(before.commands()) != 1 || len(after.commands()) != 1 {
		t.Fatalf("commands = %q then %q, want one each", before.commands(), after.commands())
	}
}

func TestSSHAgentRejectsAnUnsafeLeaseLocally(t *testing.T) {
	t.Parallel()
	controller := generateControllerKey(t)
	_, _, public := hostKeyPair(t, controller, "h1")
	var dials atomic.Int32
	agent := newTestSSHAgent(t, controller.Signer(), func(cfg *pool.SSHAgentConfig) {
		cfg.Dial = func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("dial not expected")
		}
	})
	host := pool.Host{ID: "h1", Address: "127.0.0.1:1", HostKey: public}
	for _, lease := range []string{"", " ", "lease 1", "lease\t1", "lease\n1", "lease-1\r", " lease-1"} {
		err := agent.Remove(context.Background(), host, 0, lease)
		if err == nil || errors.Is(err, pool.ErrUnreachable) {
			t.Errorf("Remove(%q) error = %v, want a local refusal", lease, err)
		}
	}
	if dials.Load() != 0 {
		t.Fatalf("dialed %d times for leases refused locally", dials.Load())
	}
}

func TestNewSSHAgentRequiresTheControllerKey(t *testing.T) {
	t.Parallel()
	if _, err := pool.NewSSHAgent(pool.SSHAgentConfig{}); err == nil {
		t.Fatal("NewSSHAgent() without a signer = nil error")
	}
}
