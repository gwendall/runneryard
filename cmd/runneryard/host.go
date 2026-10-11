package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gwendall/runneryard/hostagent"
)

const hostCommandTimeout = 2 * time.Minute

// runHost is the pool host agent. Over SSH the controller's authorized_keys
// entry forces "runneryard host", and sshd passes the requested verb in
// SSH_ORIGINAL_COMMAND. Run locally, the verb is given as arguments.
func runHost(args []string) error {
	if len(args) > 0 && args[0] == "install" {
		return runHostInstall(args[1:])
	}
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		printHostHelp(os.Stdout)
		return nil
	}
	command := strings.TrimSpace(os.Getenv("SSH_ORIGINAL_COMMAND"))
	if command == "" {
		command = strings.Join(args, " ")
	}
	if command == "" {
		printHostHelp(os.Stderr)
		return errors.New("runneryard host needs a verb: info, list, start N, or remove N LEASE")
	}
	cfg, cfgErr := hostagent.LoadConfig(hostagent.ConfigPath)
	agent := &hostagent.Agent{
		Config:    cfg,
		ConfigErr: cfgErr,
		Docker:    hostagent.CLI{},
		FreeBytes: hostagent.FreeBytes,
		Lock:      hostagent.SlotLock(hostLockDirectory()),
		Version:   version,
	}
	ctx, cancel := context.WithTimeout(context.Background(), hostCommandTimeout)
	defer cancel()
	reply := agent.Handle(ctx, command, os.Stdin)
	return json.NewEncoder(os.Stdout).Encode(reply)
}

func hostLockDirectory() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "locks")
	}
	return filepath.Join(hostagent.AgentHome, "locks")
}

type cacheFlags []string

func (c *cacheFlags) String() string { return strings.Join(*c, ",") }

func (c *cacheFlags) Set(value string) error {
	*c = append(*c, value)
	return nil
}

func runHostInstall(args []string) error {
	flags := flag.NewFlagSet("host install", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	image := flags.String("image", "", "exact runner image every slot runs")
	slots := flags.Int("slots", 0, "runner slots on this host")
	authorizedKey := flags.String("authorized-key", "", "controller SSH public key (the .pub of RUNNER_POOL_SSH_KEY_FILE)")
	slotCPUs := flags.Float64("slot-cpus", 0, "CPU limit per slot, 0 for none")
	slotMemoryMB := flags.Int("slot-memory-mb", 0, "memory limit per slot in MB, 0 for none")
	minFreeDiskGB := flags.Int("min-free-disk-gb", 20, "free disk a start requires; below it the slot's caches are dropped")
	hardenSSHD := flags.Bool("harden-sshd", false, "disable passwords, root login, and forwarding in sshd (machines dedicated to the pool)")
	blockMetadata := flags.Bool("block-metadata", false, "drop container traffic to the cloud metadata address 169.254.169.254")
	var caches cacheFlags
	flags.Var(&caches, "cache", "runner path kept between a slot's jobs (repeatable; default ~/.cache, ~/.npm, ~/go)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHostHelp(os.Stdout)
			return nil
		}
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if len(caches) == 0 {
		caches = append(caches, hostagent.DefaultCaches...)
	}
	return hostagent.Install(context.Background(), hostagent.InstallOptions{
		Config: hostagent.Config{
			Slots: *slots, Image: *image, SlotCPUs: *slotCPUs, SlotMemoryMB: *slotMemoryMB,
			Caches: caches, MinFreeDiskGB: *minFreeDiskGB,
		},
		AuthorizedKey: *authorizedKey,
		HardenSSHD:    *hardenSSHD,
		BlockMetadata: *blockMetadata,
		Out:           os.Stdout,
	})
}

func printHostHelp(output io.Writer) {
	fmt.Fprint(output, `runneryard host runs on a pool host.

Usage:
  runneryard host install --image IMAGE --slots N --authorized-key "ssh-ed25519 ..." [flags]
      Prepare this machine (as root): the runneryard account, its forced-command
      key, the host configuration, and the runner image.
        --slot-cpus N          CPU limit per slot
        --slot-memory-mb N     memory limit per slot
        --cache PATH           runner path kept between jobs (repeatable)
        --min-free-disk-gb N   free disk a start requires (default 20)
        --harden-sshd          keys only, no root login, no forwarding
        --block-metadata       keep containers away from 169.254.169.254
  runneryard host info | list
      Show what the controller sees (as the runneryard user).

Over SSH, the controller key's authorized_keys entry forces "runneryard host"
and the agent answers one of: info, list, start N, remove N LEASE.
`)
}
