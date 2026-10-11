package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	defaultSSHUser        = "runneryard"
	defaultDialTimeout    = 10 * time.Second
	defaultSSHMaxSessions = 8
	maxReplyBytes         = 1 << 20
	maxDiagnosticBytes    = 4 << 10
)

// SSHAgentConfig configures the SSH transport to host agents.
type SSHAgentConfig struct {
	// User is the account whose authorized_keys forces "runneryard host".
	User   string
	Signer ssh.Signer
	// DialTimeout bounds the TCP connection and the SSH handshake.
	DialTimeout time.Duration
	// MaxSessions bounds concurrent commands on one host, below sshd's own
	// per-connection limit.
	MaxSessions int
	Dial        func(ctx context.Context, network, address string) (net.Conn, error)
}

// SSHAgent reaches host agents over SSH. It authenticates with the
// controller key and accepts only the host key pinned for each host; it
// never trusts a key on first use. The host's authorized_keys entry forces
// the agent command, so the connection can do nothing else.
type SSHAgent struct {
	cfg   SSHAgentConfig
	mu    sync.Mutex
	conns map[string]*sshConn
}

type sshConn struct {
	identity string
	client   *ssh.Client
	sessions chan struct{}
}

// NewSSHAgent returns an agent client. Connections are opened on first use
// and kept per host.
func NewSSHAgent(cfg SSHAgentConfig) (*SSHAgent, error) {
	if cfg.Signer == nil {
		return nil, errors.New("the SSH host agent requires the controller key")
	}
	if cfg.User == "" {
		cfg.User = defaultSSHUser
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	if cfg.MaxSessions < 1 {
		cfg.MaxSessions = defaultSSHMaxSessions
	}
	if cfg.Dial == nil {
		dialer := &net.Dialer{}
		cfg.Dial = dialer.DialContext
	}
	return &SSHAgent{cfg: cfg, conns: make(map[string]*sshConn)}, nil
}

func (a *SSHAgent) Info(ctx context.Context, host Host) (HostInfo, error) {
	var info HostInfo
	err := a.call(ctx, host, "info", nil, &info)
	return info, err
}

func (a *SSHAgent) List(ctx context.Context, host Host) ([]Slot, error) {
	var slots []Slot
	err := a.call(ctx, host, "list", nil, &slots)
	return slots, err
}

// Start sends the request, JIT configuration included, on standard input.
func (a *SSHAgent) Start(ctx context.Context, host Host, slot int, request StartRequest) (Slot, error) {
	input, err := json.Marshal(request)
	if err != nil {
		return Slot{}, fmt.Errorf("encode start request: %w", err)
	}
	var started Slot
	err = a.call(ctx, host, "start "+strconv.Itoa(slot), input, &started)
	return started, err
}

func (a *SSHAgent) Remove(ctx context.Context, host Host, slot int, leaseID string) error {
	if leaseID == "" || strings.ContainsAny(leaseID, " \t\r\n") {
		return fmt.Errorf("invalid lease id %q", leaseID)
	}
	return a.call(ctx, host, "remove "+strconv.Itoa(slot)+" "+leaseID, nil, nil)
}

// Close closes every connection.
func (a *SSHAgent) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, conn := range a.conns {
		_ = conn.client.Close()
		delete(a.conns, id)
	}
}

// call runs one agent command. A failure before the command started wraps
// ErrUnreachable: the command did not run. A failure after it started is
// ambiguous and is returned as is.
func (a *SSHAgent) call(ctx context.Context, host Host, command string, input []byte, output any) error {
	session, conn, err := a.session(ctx, host)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnreachable, host.ID, err)
	}
	defer func() { <-conn.sessions }()
	defer session.Close()
	stdout := &limitedBuffer{limit: maxReplyBytes}
	stderr := &limitedBuffer{limit: maxDiagnosticBytes}
	session.Stdin = bytes.NewReader(input)
	session.Stdout = stdout
	session.Stderr = stderr
	if err := session.Start(command); err != nil {
		a.drop(host.ID, conn)
		return fmt.Errorf("%w: %s: start command: %w", ErrUnreachable, host.ID, err)
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	var runErr error
	select {
	case runErr = <-done:
	case <-ctx.Done():
		return fmt.Errorf("host agent %s: %s interrupted: %w", host.ID, verb(command), ctx.Err())
	}
	var reply Reply
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &reply); err != nil {
		return fmt.Errorf("host agent %s: %s returned no reply (%v): %s", host.ID, verb(command), runErr, strings.TrimSpace(stderr.String()))
	}
	if !reply.OK {
		if reply.Error == nil || reply.Error.Code == "" {
			return fmt.Errorf("host agent %s: %s failed without a reason", host.ID, verb(command))
		}
		return &AgentError{Code: reply.Error.Code, Message: reply.Error.Message}
	}
	if output == nil || len(reply.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(reply.Result, output); err != nil {
		return fmt.Errorf("host agent %s: decode %s reply: %w", host.ID, verb(command), err)
	}
	return nil
}

// session opens a session on the host's connection, reconnecting once when a
// kept connection turns out to be dead.
func (a *SSHAgent) session(ctx context.Context, host Host) (*ssh.Session, *sshConn, error) {
	var lastErr error
	for range 2 {
		conn, err := a.connect(ctx, host)
		if err != nil {
			return nil, nil, err
		}
		select {
		case conn.sessions <- struct{}{}:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		session, err := conn.client.NewSession()
		if err == nil {
			return session, conn, nil
		}
		<-conn.sessions
		a.drop(host.ID, conn)
		lastErr = err
	}
	return nil, nil, lastErr
}

func (a *SSHAgent) connect(ctx context.Context, host Host) (*sshConn, error) {
	identity := host.Address + "|" + host.HostKey
	a.mu.Lock()
	if conn, ok := a.conns[host.ID]; ok && conn.identity == identity {
		a.mu.Unlock()
		return conn, nil
	}
	a.mu.Unlock()
	pinned, _, _, _, err := ssh.ParseAuthorizedKey([]byte(host.HostKey))
	if err != nil {
		return nil, fmt.Errorf("no valid pinned host key: %w", err)
	}
	dialCtx, cancel := context.WithTimeout(ctx, a.cfg.DialTimeout)
	defer cancel()
	raw, err := a.cfg.Dial(dialCtx, "tcp", host.Address)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(dialCtx, func() { _ = raw.Close() })
	config := &ssh.ClientConfig{
		User:              a.cfg.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(a.cfg.Signer)},
		HostKeyCallback:   ssh.FixedHostKey(pinned),
		HostKeyAlgorithms: hostKeyAlgorithms(pinned),
		Timeout:           a.cfg.DialTimeout,
	}
	clientConn, channels, requests, err := ssh.NewClientConn(raw, host.Address, config)
	if !stop() {
		if err == nil {
			_ = clientConn.Close()
		}
		return nil, fmt.Errorf("ssh handshake: %w", dialCtx.Err())
	}
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	client := ssh.NewClient(clientConn, channels, requests)
	conn := &sshConn{identity: identity, client: client, sessions: make(chan struct{}, a.cfg.MaxSessions)}
	a.mu.Lock()
	if existing, ok := a.conns[host.ID]; ok && existing.identity == identity {
		a.mu.Unlock()
		_ = client.Close()
		return existing, nil
	}
	if previous, ok := a.conns[host.ID]; ok {
		_ = previous.client.Close()
	}
	a.conns[host.ID] = conn
	a.mu.Unlock()
	go func() {
		_ = client.Wait()
		a.drop(host.ID, conn)
	}()
	return conn, nil
}

func (a *SSHAgent) drop(hostID string, conn *sshConn) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if current, ok := a.conns[hostID]; ok && current == conn {
		delete(a.conns, hostID)
		_ = conn.client.Close()
	}
}

// hostKeyAlgorithms restricts negotiation to the pinned key's algorithms, so
// a server offering another host key type cannot fail the pin by accident.
func hostKeyAlgorithms(key ssh.PublicKey) []string {
	if key.Type() == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	return []string{key.Type()}
}

func verb(command string) string {
	name, _, _ := strings.Cut(command, " ")
	return name
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	if room := b.limit - b.Len(); room > 0 {
		b.Buffer.Write(data[:min(len(data), room)])
	}
	return len(data), nil
}

var _ Agent = (*SSHAgent)(nil)
