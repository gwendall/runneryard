package hostagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

const (
	// AgentUser is the account the controller connects as.
	AgentUser = "runneryard"
	// AgentHome holds the account's authorized_keys and slot locks.
	AgentHome = "/var/lib/runneryard"
	// BinaryPath is the agent command forced by authorized_keys.
	BinaryPath       = "/usr/local/bin/runneryard"
	sshdDropInPath   = "/etc/ssh/sshd_config.d/10-runneryard.conf"
	metadataUnitName = "runneryard-metadata-block.service"
	metadataUnitPath = "/etc/systemd/system/" + metadataUnitName
)

// SSHDHardening leaves key authentication as the only way in and turns off
// every forwarding, so the agent account is a command and nothing else.
const SSHDHardening = `# Written by runneryard host install.
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
AllowAgentForwarding no
AllowTcpForwarding no
X11Forwarding no
PermitTunnel no
MaxSessions 32
`

// MetadataBlockUnit keeps slot containers, and the containers jobs start
// inside them, away from the cloud metadata service, which serves the host's
// user data.
const MetadataBlockUnit = `# Written by runneryard host install.
[Unit]
Description=Keep RunnerYard slot containers away from the cloud metadata service
After=docker.service
Requires=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c 'iptables -C DOCKER-USER -d 169.254.169.254/32 -j DROP 2>/dev/null || iptables -I DOCKER-USER -d 169.254.169.254/32 -j DROP'

[Install]
WantedBy=multi-user.target
`

// AuthorizedKeysLine returns the agent account's only authorized key: the
// controller key, restricted (no forwarding, no pty) and forced to the agent
// command whatever the client asks.
func AuthorizedKeysLine(controllerKey string) (string, error) {
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(controllerKey)))
	if err != nil {
		return "", fmt.Errorf("controller key is not an SSH public key: %w", err)
	}
	if len(options) > 0 || len(bytes.TrimSpace(rest)) > 0 {
		return "", errors.New("controller key must be a single public key without options")
	}
	return `restrict,command="` + BinaryPath + ` host" ` + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), nil
}

// InstallOptions configures a host.
type InstallOptions struct {
	Config        Config
	AuthorizedKey string
	// HardenSSHD writes an sshd drop-in that disables passwords, root login,
	// and forwarding. Meant for machines that only serve the pool.
	HardenSSHD bool
	// BlockMetadata installs a unit that drops container traffic to the
	// cloud metadata address.
	BlockMetadata bool
	Out           io.Writer
}

// Install prepares this machine as a pool host. It runs as root and is
// idempotent. The authorized key is written last, so the controller can
// connect only to a host that is fully prepared.
func Install(ctx context.Context, options InstallOptions) error {
	if os.Geteuid() != 0 {
		return errors.New("runneryard host install must run as root")
	}
	if err := options.Config.Validate(); err != nil {
		return err
	}
	line, err := AuthorizedKeysLine(options.AuthorizedKey)
	if err != nil {
		return err
	}
	if options.Out == nil {
		options.Out = io.Discard
	}
	if err := installBinary(); err != nil {
		return err
	}
	uid, gid, err := ensureUser(ctx)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(options.Config, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(ConfigPath, append(encoded, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(options.Out, "pulling %s\n", options.Config.Image)
	if err := run(ctx, "docker", "pull", options.Config.Image); err != nil {
		return err
	}
	if options.BlockMetadata {
		if err := writeFile(metadataUnitPath, []byte(MetadataBlockUnit), 0o644); err != nil {
			return err
		}
		if err := run(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
		if err := run(ctx, "systemctl", "enable", "--now", metadataUnitName); err != nil {
			return err
		}
	}
	if options.HardenSSHD {
		if err := hardenSSHD(ctx); err != nil {
			return err
		}
	}
	if err := writeAuthorizedKey(line, uid, gid); err != nil {
		return err
	}
	fmt.Fprintf(options.Out, "pool host ready: %d slots, image %s\n", options.Config.Slots, options.Config.Image)
	if agent := (&Agent{Config: options.Config, CPUs: OnlineCPUs()}); options.Config.SlotCPUs > 0 && !agent.SlotsPinned() {
		fmt.Fprintf(options.Out, "warning: slots get a CPU quota and see all %d host CPUs; a whole --slot-cpus with slots x slot CPUs within the host's pins each slot to its own cores\n", len(agent.CPUs))
	}
	return nil
}

func installBinary() error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate runneryard binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	if executable == BinaryPath {
		return nil
	}
	contents, err := os.ReadFile(executable) // #nosec G304 -- the running binary itself
	if err != nil {
		return fmt.Errorf("read runneryard binary: %w", err)
	}
	return writeFile(BinaryPath, contents, 0o755)
}

func ensureUser(ctx context.Context) (int, int, error) {
	if _, err := user.Lookup(AgentUser); err != nil {
		var unknown user.UnknownUserError
		if !errors.As(err, &unknown) {
			return 0, 0, fmt.Errorf("look up %s: %w", AgentUser, err)
		}
		if err := run(ctx, "useradd", "--system", "--create-home", "--home-dir", AgentHome, "--shell", "/bin/sh", AgentUser); err != nil {
			return 0, 0, err
		}
	}
	if err := run(ctx, "usermod", "--append", "--groups", "docker", AgentUser); err != nil {
		return 0, 0, err
	}
	account, err := user.Lookup(AgentUser)
	if err != nil {
		return 0, 0, fmt.Errorf("look up %s: %w", AgentUser, err)
	}
	uid, uidErr := strconv.Atoi(account.Uid)
	gid, gidErr := strconv.Atoi(account.Gid)
	if uidErr != nil || gidErr != nil {
		return 0, 0, fmt.Errorf("%s has a non-numeric uid or gid", AgentUser)
	}
	if account.HomeDir != AgentHome {
		return 0, 0, fmt.Errorf("%s exists with home %s instead of %s", AgentUser, account.HomeDir, AgentHome)
	}
	return uid, gid, nil
}

func hardenSSHD(ctx context.Context) error {
	if err := writeFile(sshdDropInPath, []byte(SSHDHardening), 0o644); err != nil {
		return err
	}
	if err := run(ctx, "sshd", "-t"); err != nil {
		_ = os.Remove(sshdDropInPath)
		return fmt.Errorf("sshd rejected the hardening drop-in, removed it: %w", err)
	}
	// Ubuntu names the unit ssh, most other distributions sshd.
	if err := run(ctx, "systemctl", "try-reload-or-restart", "ssh.service"); err != nil {
		return run(ctx, "systemctl", "try-reload-or-restart", "sshd.service")
	}
	return nil
}

func writeAuthorizedKey(line string, uid, gid int) error {
	directory := filepath.Join(AgentHome, ".ssh")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	path := filepath.Join(directory, "authorized_keys")
	if err := writeFile(path, []byte(line+"\n"), 0o600); err != nil {
		return err
	}
	for _, target := range []string{AgentHome, directory, path} {
		if err := os.Chown(target, uid, gid); err != nil {
			return err
		}
	}
	return os.Chmod(directory, 0o700) // #nosec G302 -- a directory needs its search bit; owner only
}

// writeFile replaces path atomically.
func writeFile(path string, contents []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { // #nosec G301 -- system configuration directories
		return err
	}
	temporary := path + ".runneryard-tmp"
	if err := os.WriteFile(temporary, contents, mode); err != nil { // #nosec G306 G703 -- fixed install paths; mode chosen per file
		return err
	}
	if err := os.Chmod(temporary, mode); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func run(ctx context.Context, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...) // #nosec G204 -- fixed system tools, discrete arguments, no shell
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if len(message) > 2048 {
			message = message[len(message)-2048:]
		}
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, message)
	}
	return nil
}
