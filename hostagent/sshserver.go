package hostagent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	sshHandshakeTimeout   = 15 * time.Second
	sshMaxConnections     = 16
	sshMaxSessionsPerConn = 16
	defaultCommandTimeout = 2 * time.Minute
)

// SSHServer serves the agent over SSH without sshd, for a host that boots
// from the runner image (a Fly Machine): the agent is its only SSH endpoint.
// It admits the controller key as the runneryard user and one exec request
// per session, and nothing else: no shell, no pty, no forwarding, no other
// channel. The exec request carries the verb, as SSH_ORIGINAL_COMMAND does
// under sshd's forced command.
type SSHServer struct {
	Agent         *Agent
	HostKey       ssh.Signer
	AuthorizedKey ssh.PublicKey
	// CommandTimeout bounds one verb (default 2 minutes).
	CommandTimeout time.Duration
	Logger         *slog.Logger
}

func (s *SSHServer) config() *ssh.ServerConfig {
	authorized := s.AuthorizedKey.Marshal()
	config := &ssh.ServerConfig{
		MaxAuthTries:  3,
		ServerVersion: "SSH-2.0-runneryard",
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			userOK := subtle.ConstantTimeCompare([]byte(meta.User()), []byte(AgentUser)) == 1
			keyOK := subtle.ConstantTimeCompare(key.Marshal(), authorized) == 1
			if !userOK || !keyOK {
				return nil, errors.New("unauthorized")
			}
			return &ssh.Permissions{}, nil
		},
	}
	config.AddHostKey(s.HostKey)
	return config
}

// Serve accepts connections until ctx ends, then waits for running verbs.
func (s *SSHServer) Serve(ctx context.Context, listener net.Listener) error {
	if s.Agent == nil || s.HostKey == nil || s.AuthorizedKey == nil {
		return errors.New("the agent SSH server requires an agent, a host key, and the controller key")
	}
	if s.Agent.Now == nil {
		s.Agent.Now = time.Now
	}
	if s.Logger == nil {
		s.Logger = slog.New(slog.DiscardHandler)
	}
	config := s.config()
	connections := make(chan struct{}, sshMaxConnections)
	var wait sync.WaitGroup
	defer wait.Wait()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	for {
		raw, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			return err
		}
		select {
		case connections <- struct{}{}:
		default:
			_ = raw.Close()
			continue
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			defer func() { <-connections }()
			s.serveConn(ctx, raw, config)
		}()
	}
}

func (s *SSHServer) serveConn(ctx context.Context, raw net.Conn, config *ssh.ServerConfig) {
	defer func() { _ = raw.Close() }()
	_ = raw.SetDeadline(time.Now().Add(sshHandshakeTimeout))
	conn, channels, requests, err := ssh.NewServerConn(raw, config)
	if err != nil {
		s.Logger.Debug("agent SSH handshake failed", "remote", raw.RemoteAddr().String(), "error", err)
		return
	}
	_ = raw.SetDeadline(time.Time{})
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	go ssh.DiscardRequests(requests)
	sessions := make(chan struct{}, sshMaxSessionsPerConn)
	var wait sync.WaitGroup
	defer wait.Wait()
	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		select {
		case sessions <- struct{}{}:
		default:
			_ = newChannel.Reject(ssh.ResourceShortage, "too many sessions")
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			<-sessions
			continue
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			defer func() { <-sessions }()
			s.serveSession(ctx, channel, channelRequests)
		}()
	}
}

// serveSession runs the session's exec request; every other request (pty,
// shell, env, subsystem, signals) is refused.
func (s *SSHServer) serveSession(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request) {
	defer func() { _ = channel.Close() }()
	for request := range requests {
		if request.Type != "exec" {
			_ = request.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
			_ = request.Reply(false, nil)
			return
		}
		_ = request.Reply(true, nil)
		go ssh.DiscardRequests(requests)
		s.run(ctx, channel, payload.Command)
		return
	}
}

func (s *SSHServer) run(ctx context.Context, channel ssh.Channel, command string) {
	timeout := s.CommandTimeout
	if timeout <= 0 {
		timeout = defaultCommandTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	reply := s.Agent.Handle(callCtx, command, channel)
	if verb, _, _ := strings.Cut(strings.TrimSpace(command), " "); verb == "start" || verb == "remove" || !reply.OK {
		code := ""
		if reply.Error != nil {
			code = reply.Error.Code
		}
		s.Logger.Info("agent command", "command", firstFields(command, 2), "ok", reply.OK, "code", code, "duration", time.Since(started).Round(time.Millisecond))
	}
	encoded, err := json.Marshal(reply)
	if err != nil {
		return
	}
	_, _ = channel.Write(append(encoded, '\n'))
	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
	_ = channel.CloseWrite()
}

// firstFields keeps a command's verb and slot for the log; a lease ID is not
// secret, but nothing else from the connection is logged.
func firstFields(command string, count int) string {
	fields := strings.Fields(command)
	return strings.Join(fields[:min(count, len(fields))], " ")
}
