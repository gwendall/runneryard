package hostagent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A slot's work directory, /home/runner/_work, is a host directory of its own
// for every lease, bind-mounted into the slot. The job's checkout and build
// outputs then stay out of the container's writable layer, which docker rm
// deletes before it returns: after a large job that took long enough for the
// controller's removal call to time out and be retried while the first one
// still held the slot's lock. The container now goes at once; its directory is
// renamed aside and deleted in the background, outside Docker.
const (
	workspaceTarget = "/home/runner/_work"
	// workspacePrefix starts a slot's directory, "slot-<n>-<lease>".
	workspacePrefix = "slot-"
	// retiredWorkspacePrefix starts a directory waiting for deletion: its
	// former name and a random suffix follow.
	retiredWorkspacePrefix = "deleting-"
	// labelWorkspacePurge marks the helper container that deletes retired
	// directories for an agent that is not root.
	labelWorkspacePurge = "runneryard.workspace-purge"
	purgeTarget         = "/workspaces"
	// defaultPurgeWait leaves most of the controller's 90 s start timeout to
	// the start itself. Deleting 3.2 GB in 250,000 files took 12 s on a
	// performance Fly Machine.
	defaultPurgeWait = 30 * time.Second
	purgePoll        = 250 * time.Millisecond
)

// validWorkRoot reports whether root can be bind-mounted as given: a clean
// absolute path without the commas and equal signs --mount would parse.
func validWorkRoot(root string) bool {
	return safeCachePath.MatchString(root) && filepath.Clean(root) == root
}

func slotWorkspacePrefix(slot int) string { return workspacePrefix + strconv.Itoa(slot) + "-" }

// prepareWorkspace gives slot an empty work directory for leaseID and returns
// its path. Only a start holding the slot's lock calls it, once the slot has
// no container, so the slot's earlier directories are mounted nowhere: they
// are retired first.
func (a *Agent) prepareWorkspace(ctx context.Context, slot int, leaseID string) (string, error) {
	if !validWorkRoot(a.WorkRoot) {
		return "", fmt.Errorf("invalid work root %q", a.WorkRoot)
	}
	if !safeLabelValue.MatchString(leaseID) {
		return "", errors.New("invalid lease id")
	}
	if err := os.MkdirAll(a.WorkRoot, 0o700); err != nil {
		return "", err
	}
	if err := a.discardWorkspaces(ctx, slot); err != nil {
		return "", err
	}
	path := filepath.Join(a.WorkRoot, slotWorkspacePrefix(slot)+leaseID)
	// Mkdir, not MkdirAll: a directory already there could hold another job's
	// files. The slot's entrypoint gives it to the runner; other users inside
	// the slot may read it, as they may a work directory the runner creates.
	if err := os.Mkdir(path, 0o755); err != nil { // #nosec G301 -- readable inside the slot, under a 0700 work root
		return "", err
	}
	return path, nil
}

// discardWorkspaces retires slot's work directories and starts deleting them.
// Only a command holding the slot's lock calls it, once the slot has no
// container. A directory it cannot rename is reported and retired by the
// slot's next start or removal, or by the host's next boot.
func (a *Agent) discardWorkspaces(ctx context.Context, slot int) error {
	if a.WorkRoot == "" {
		return nil
	}
	retired, err := retireWorkspaces(a.WorkRoot, slotWorkspacePrefix(slot))
	if retired > 0 {
		a.purgeWorkspaces(ctx)
	}
	return err
}

// RetireWorkspaces renames every slot's work directory in root for deletion
// and returns how many it renamed. Host boot calls it once the previous run's
// slot containers are removed.
func RetireWorkspaces(root string) (int, error) {
	return retireWorkspaces(root, workspacePrefix)
}

// retireWorkspaces renames the directories in root whose name starts with
// prefix for deletion. A rename within root takes no time whatever the
// directory holds, and needs no permission on the directory itself, which the
// job's runner owns by then.
func retireWorkspaces(root, prefix string) (int, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	retired := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		target := retiredWorkspacePrefix + name + "-" + strings.ToLower(rand.Text())
		if err := os.Rename(filepath.Join(root, name), filepath.Join(root, target)); err != nil {
			return retired, err
		}
		retired++
	}
	return retired, nil
}

// purgeWorkspaces starts deleting the retired directories without waiting
// for it.
func (a *Agent) purgeWorkspaces(ctx context.Context) {
	if a.PurgeWorkspaces != nil {
		a.PurgeWorkspaces()
		return
	}
	// A helper that cannot start leaves the directories retired: the next
	// one, after any slot's next job, deletes them too.
	_, _ = a.Docker.Run(ctx, nil, a.purgeArgs()...)
}

// awaitPurge waits, up to purgeWait, for the retired work directories to be
// deleted or free reaches required, and returns the free disk then. It returns
// at once when no directory is retired. A start calls it when the disk is
// below its minimum, before dropping the slot's caches: the previous job's
// directory is usually still being deleted when the next job starts.
func (a *Agent) awaitPurge(ctx context.Context, required, free uint64) (uint64, error) {
	if a.WorkRoot == "" || !retiredWorkspacesLeft(a.WorkRoot) {
		return free, nil
	}
	// A deletion that failed earlier waits for the next one: start it now.
	a.purgeWorkspaces(ctx)
	wait := a.purgeWait
	if wait == 0 {
		wait = defaultPurgeWait
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	poll := time.NewTicker(purgePoll)
	defer poll.Stop()
	for {
		last := false
		select {
		case <-ctx.Done():
			return free, nil
		case <-deadline.C:
			last = true
		case <-poll.C:
		}
		current, err := a.freeBytes()
		if last || err != nil || current >= required || !retiredWorkspacesLeft(a.WorkRoot) {
			return current, err
		}
		free = current
	}
}

func retiredWorkspacesLeft(root string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), retiredWorkspacePrefix) {
			return true
		}
	}
	return false
}

// purgeArgs run a short-lived container that deletes the retired directories
// and is removed when done. An agent that is not root, as after host install,
// cannot delete what a job's root processes wrote. The container sees the work
// root only, has no network and a read-only root filesystem, and keeps only
// the capabilities rm needs to delete files it does not own.
func (a *Agent) purgeArgs() []string {
	return []string{
		"run", "--rm", "--detach",
		"--label", labelWorkspacePurge + "=true",
		"--network", "none", "--read-only", "--log-driver", "none",
		"--user", "0:0", "--cap-drop", "ALL", "--cap-add", "DAC_OVERRIDE", "--cap-add", "FOWNER",
		"--mount", "type=bind,source=" + a.WorkRoot + ",target=" + purgeTarget,
		"--pull", "never",
		"--entrypoint", "/usr/bin/find",
		a.image(), purgeTarget, "-mindepth", "1", "-maxdepth", "1", "-name", retiredWorkspacePrefix + "*",
		"-exec", "rm", "-rf", "--", "{}", "+",
	}
}

// WorkspacePurger deletes retired work directories in the background, one
// pass at a time, for an agent that runs as root and outlives its commands,
// as host boot does. Commands only wake it, so a deletion never holds one up.
type WorkspacePurger struct {
	root      string
	log       *slog.Logger
	removeAll func(string) error
	wake      chan struct{}
}

// NewWorkspacePurger returns a purger for the work directories in root.
func NewWorkspacePurger(root string, log *slog.Logger) *WorkspacePurger {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &WorkspacePurger{root: root, log: log, removeAll: os.RemoveAll, wake: make(chan struct{}, 1)}
}

// Wake asks for a pass and returns at once.
func (p *WorkspacePurger) Wake() {
	select {
	case p.wake <- struct{}{}:
	default: // a pass is already due, and will see the new directories
	}
}

// Run deletes the retired directories at every wake until ctx ends.
func (p *WorkspacePurger) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
			p.purge()
		}
	}
}

// purge deletes every retired directory in root. One it cannot delete is
// logged and tried again at the next pass.
func (p *WorkspacePurger) purge() {
	entries, err := os.ReadDir(p.root)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			p.log.Warn("could not list the retired work directories", "error", err)
		}
		return
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), retiredWorkspacePrefix) {
			continue
		}
		started := time.Now()
		if err := p.removeAll(filepath.Join(p.root, entry.Name())); err != nil {
			p.log.Warn("could not delete a retired work directory; retrying after the next job", "directory", entry.Name(), "error", err)
			continue
		}
		p.log.Info("deleted a retired work directory", "directory", entry.Name(), "duration", time.Since(started).Round(time.Millisecond).String())
	}
}
