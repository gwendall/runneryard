package hostagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	// BootHostKeyPath and BootAuthorizedKeyPath are where a host source that
	// boots machines from the runner image writes the host's derived SSH key
	// and the controller's public key.
	BootHostKeyPath       = "/etc/runneryard/ssh_host_ed25519_key"
	BootAuthorizedKeyPath = "/etc/runneryard/controller.pub"
	// BootDataRoot is the machine's persistent volume; Docker keeps its data,
	// and with it the slots' images and caches, under BootDataRoot/docker.
	BootDataRoot = "/data"

	dockerdConfigPath  = "/etc/docker/daemon.json"
	dockerdLogPath     = "/var/log/dockerd.log"
	dockerReadyTimeout = 90 * time.Second
	dockerStopTimeout  = 20 * time.Second
	maxPullBackoff     = time.Minute
)

// BootOptions configures a machine that boots from the runner image and
// serves as a pool host: no sshd, no installation step, a fresh root
// filesystem on every start and a persistent volume for Docker.
type BootOptions struct {
	ConfigPath        string
	HostKeyPath       string
	AuthorizedKeyPath string
	DataRoot          string
	// Listen is the agent's SSH address, such as the machine's private
	// address and port 22.
	Listen string
	// DNS is the resolvers slot containers use: Docker's bridge cannot reach
	// a resolver on the machine's private IPv6 network.
	DNS     []string
	LockDir string
	Version string
	Logger  *slog.Logger
}

// Boot prepares the machine and serves the agent until ctx ends. Every step
// is safe to repeat: a stopped machine runs it again on its next start.
func Boot(ctx context.Context, options BootOptions) error {
	if os.Geteuid() != 0 {
		return errors.New("runneryard host boot must run as root")
	}
	log := options.Logger
	if log == nil {
		log = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	cfg, err := LoadConfig(options.ConfigPath)
	if err != nil {
		return fmt.Errorf("host configuration: %w", err)
	}
	dockerRoot := filepath.Join(options.DataRoot, "docker")
	if cfg.DockerRoot != dockerRoot {
		return fmt.Errorf("host configuration docker_root is %q, want %q on the data volume", cfg.DockerRoot, dockerRoot)
	}
	signer, authorized, err := LoadBootKeys(options.HostKeyPath, options.AuthorizedKeyPath)
	if err != nil {
		return err
	}
	if err := SeparateFilesystem(options.DataRoot); err != nil {
		return err
	}
	daemon, err := DockerDaemonConfig(dockerRoot, options.DNS)
	if err != nil {
		return err
	}
	if err := writeFile(dockerdConfigPath, daemon, 0o644); err != nil {
		return err
	}
	stopDocker, dockerExited, err := startDockerd()
	if err != nil {
		return err
	}
	defer stopDocker()
	docker := CLI{}
	if err := waitForDocker(ctx, docker, dockerExited); err != nil {
		return err
	}
	removed, err := RemoveLeftoverSlots(ctx, docker)
	if err != nil {
		return err
	}
	if removed > 0 {
		log.Info("removed slot containers left by the previous run", "containers", removed)
	}
	listener, err := net.Listen("tcp", options.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", options.Listen, err)
	}
	agent := &Agent{
		Config:    cfg,
		Docker:    docker,
		FreeBytes: FreeBytes,
		Lock:      SlotLock(options.LockDir),
		Now:       time.Now,
		Version:   options.Version,
	}
	server := &SSHServer{Agent: agent, HostKey: signer, AuthorizedKey: authorized, Logger: log}
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- server.Serve(serveCtx, listener) }()
	log.Info("pool host agent listening", "address", listener.Addr().String(), "slots", cfg.Slots, "image", cfg.Image)
	// The agent answers "not ready" until the image is present, so the
	// controller sees the host while the first pull runs.
	go EnsureImage(serveCtx, docker, cfg.Image, log, func(d time.Duration) {
		select {
		case <-serveCtx.Done():
		case <-time.After(d):
		}
	})
	select {
	case err := <-served:
		return err
	case <-dockerExited:
		cancel()
		<-served
		return errors.New("dockerd exited")
	case <-ctx.Done():
		cancel()
		return <-served
	}
}

// LoadBootKeys reads the host's SSH private key and the controller's public
// key.
func LoadBootKeys(hostKeyPath, authorizedKeyPath string) (ssh.Signer, ssh.PublicKey, error) {
	hostKey, err := os.ReadFile(hostKeyPath) // #nosec G304 -- fixed boot path
	if err != nil {
		return nil, nil, fmt.Errorf("read host key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(hostKey)
	if err != nil {
		return nil, nil, fmt.Errorf("parse host key: %w", err)
	}
	contents, err := os.ReadFile(authorizedKeyPath) // #nosec G304 -- fixed boot path
	if err != nil {
		return nil, nil, fmt.Errorf("read controller key: %w", err)
	}
	authorized, _, options, rest, err := ssh.ParseAuthorizedKey(bytes.TrimSpace(contents))
	if err != nil {
		return nil, nil, fmt.Errorf("controller key is not an SSH public key: %w", err)
	}
	if len(options) > 0 || len(bytes.TrimSpace(rest)) > 0 {
		return nil, nil, errors.New("controller key must be a single public key without options")
	}
	return signer, authorized, nil
}

// SeparateFilesystem refuses a data root on the root filesystem: the root
// filesystem is reset on every start, and Docker would fill it.
func SeparateFilesystem(path string) error {
	var data, root syscall.Stat_t
	if err := syscall.Stat(path, &data); err != nil {
		return fmt.Errorf("data volume %s: %w", path, err)
	}
	if err := syscall.Stat("/", &root); err != nil {
		return err
	}
	if data.Dev == root.Dev {
		return fmt.Errorf("%s is not a mounted volume; the host needs one for Docker's data", path)
	}
	return nil
}

// DockerDaemonConfig returns the daemon.json of the host's Docker.
func DockerDaemonConfig(dataRoot string, dns []string) ([]byte, error) {
	config := map[string]any{
		"data-root": dataRoot,
		"features":  map[string]bool{"buildkit": true},
	}
	if len(dns) > 0 {
		if len(dns) > 3 {
			return nil, errors.New("at most three DNS resolvers")
		}
		seen := make(map[netip.Addr]bool, len(dns))
		resolvers := make([]string, 0, len(dns))
		for _, value := range dns {
			address, err := netip.ParseAddr(strings.TrimSpace(value))
			if err != nil || seen[address] {
				return nil, fmt.Errorf("invalid or repeated DNS resolver %q", value)
			}
			seen[address] = true
			resolvers = append(resolvers, address.String())
		}
		config["dns"] = resolvers
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func startDockerd() (func(), <-chan struct{}, error) {
	logFile, err := os.OpenFile(dockerdLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- fixed log path
	if err != nil {
		return nil, nil, err
	}
	command := exec.Command("dockerd", "--config-file", dockerdConfigPath) // #nosec G204 -- fixed arguments
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		return nil, nil, fmt.Errorf("start dockerd: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = command.Wait()
		_ = logFile.Close()
		close(exited)
	}()
	stop := func() {
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(dockerStopTimeout):
			_ = command.Process.Kill()
			<-exited
		}
	}
	return stop, exited, nil
}

func waitForDocker(ctx context.Context, docker Docker, exited <-chan struct{}) error {
	deadline := time.Now().Add(dockerReadyTimeout)
	for {
		output, err := docker.Run(ctx, nil, "info", "--format", "{{.Driver}}")
		if err == nil {
			if driver := strings.TrimSpace(string(output)); driver == "vfs" {
				return errors.New("refusing Docker storage driver vfs")
			}
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("dockerd exited before becoming ready; see %s", dockerdLogPath)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("dockerd did not become ready within %s: %w", dockerReadyTimeout, err)
		}
	}
}

// RemoveLeftoverSlots removes the slot containers a stopped machine left
// behind. Their jobs ended with the machine; the controller has already
// counted them gone. Their volumes, and with them the caches, stay.
func RemoveLeftoverSlots(ctx context.Context, docker Docker) (int, error) {
	output, err := docker.Run(ctx, nil, "ps", "--all", "--quiet", "--filter", "label="+labelSlot)
	if err != nil {
		return 0, fmt.Errorf("list leftover slot containers: %w", err)
	}
	ids := strings.Fields(string(output))
	if len(ids) == 0 {
		return 0, nil
	}
	if _, err := docker.Run(ctx, nil, append([]string{"rm", "--force"}, ids...)...); err != nil {
		return 0, fmt.Errorf("remove leftover slot containers: %w", err)
	}
	return len(ids), nil
}

// EnsureImage pulls the runner image when it is missing, retrying until it
// succeeds or ctx ends.
func EnsureImage(ctx context.Context, docker Docker, image string, log *slog.Logger, sleep func(time.Duration)) {
	if _, err := docker.Run(ctx, nil, "image", "inspect", "--format", "{{.Id}}", image); err == nil {
		return
	}
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		started := time.Now()
		_, err := docker.Run(ctx, nil, "pull", "--quiet", image)
		if err == nil {
			log.Info("pulled runner image", "image", image, "duration", time.Since(started).Round(time.Second))
			return
		}
		if ctx.Err() != nil {
			return
		}
		log.Warn("could not pull the runner image; retrying", "image", image, "retry_after", backoff, "error", err)
		sleep(backoff)
		backoff = min(backoff*2, maxPullBackoff)
	}
}
