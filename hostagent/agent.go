// Package hostagent runs on a pool host. The controller reaches it over SSH,
// where the controller key's authorized_keys entry forces "runneryard host":
// the connection can only ask for one of four verbs. The agent holds no
// credential; a job's single-use JIT configuration arrives on standard input
// and goes to Docker through the environment, never through a command line.
//
//	info              host readiness, slot count, image
//	list              slot containers
//	start N           start a runner in slot N (request JSON on stdin)
//	remove N LEASE    remove slot N's container if it holds LEASE
package hostagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gwendall/runneryard/pool"
)

// ConfigPath is where "runneryard host install" writes the host configuration.
const ConfigPath = "/etc/runneryard/host.json"

const (
	labelSlot       = "runneryard.slot"
	labelController = "runneryard.controller"
	labelLease      = "runneryard.lease"
	labelRunnerName = "runneryard.runner-name"
	labelRunnerID   = "runneryard.runner-id"
	labelScaleSetID = "runneryard.scale-set-id"
	labelImage      = "runneryard.image"
	maxRequestBytes = 64 << 10
	maxJITBytes     = 48 << 10
	maxSlots        = 64
	maxCaches       = 8
	defaultDockerFS = "/var/lib/docker"
)

// MinimumSlotMemoryMB is the smallest memory limit a slot accepts.
const MinimumSlotMemoryMB = 512

var (
	safeLabelValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	safeHostname   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,62}$`)
	safeJIT        = regexp.MustCompile(`^[A-Za-z0-9+/=]+$`)
	safeImage      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,254}$`)
	safeCachePath  = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
)

// DefaultCaches are the runner paths a slot keeps between its jobs.
var DefaultCaches = []string{"/home/runner/.cache", "/home/runner/.npm", "/home/runner/go"}

// Config is the host configuration written by install.
type Config struct {
	Slots        int      `json:"slots"`
	Image        string   `json:"image"`
	SlotCPUs     float64  `json:"slot_cpus,omitempty"`
	SlotMemoryMB int      `json:"slot_memory_mb,omitempty"`
	Caches       []string `json:"caches"`
	// MinFreeDiskGB is the free space a start requires on the Docker data
	// filesystem; below it the slot's caches are dropped first.
	MinFreeDiskGB int    `json:"min_free_disk_gb,omitempty"`
	DockerRoot    string `json:"docker_root,omitempty"`
}

// Validate checks the configuration.
func (c Config) Validate() error {
	if c.Slots < 1 || c.Slots > maxSlots {
		return fmt.Errorf("slots must be between 1 and %d", maxSlots)
	}
	if !safeImage.MatchString(c.Image) {
		return fmt.Errorf("invalid runner image %q", c.Image)
	}
	if c.SlotCPUs < 0 || c.SlotCPUs > 512 || c.SlotMemoryMB < 0 || (c.SlotMemoryMB > 0 && c.SlotMemoryMB < MinimumSlotMemoryMB) {
		return fmt.Errorf("slot CPUs must be positive and slot memory at least %d MB when set", MinimumSlotMemoryMB)
	}
	if len(c.Caches) > maxCaches {
		return fmt.Errorf("at most %d cache paths", maxCaches)
	}
	seen := make(map[string]struct{}, len(c.Caches))
	for _, path := range c.Caches {
		if !validCachePath(path) {
			return fmt.Errorf("cache path %q must be a clean absolute path under /home/runner or /opt", path)
		}
		// Distinct paths can share a volume name: /opt/a-b and /opt/a/b.
		if _, duplicate := seen[cacheVolumeSuffix(path)]; duplicate {
			return fmt.Errorf("cache path %q appears twice or shares its volume with another path", path)
		}
		seen[cacheVolumeSuffix(path)] = struct{}{}
	}
	if c.MinFreeDiskGB < 0 || c.MinFreeDiskGB > 10_000 {
		return errors.New("min_free_disk_gb must be between 0 and 10000")
	}
	if c.DockerRoot != "" && !filepath.IsAbs(c.DockerRoot) {
		return errors.New("docker_root must be an absolute path")
	}
	return nil
}

func validCachePath(path string) bool {
	if !safeCachePath.MatchString(path) || filepath.Clean(path) != path {
		return false
	}
	if path == "/home/runner/_work" || path == "/var/lib/docker" {
		return false
	}
	for _, root := range []string{"/home/runner/", "/opt/"} {
		if strings.HasPrefix(path, root) && len(path) > len(root) {
			return true
		}
	}
	return false
}

// LoadConfig reads and validates the host configuration.
func LoadConfig(path string) (Config, error) {
	contents, err := os.ReadFile(path) // #nosec G304 -- fixed host configuration path
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(contents, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode %s: %w", path, err)
	}
	return cfg, cfg.Validate()
}

// Docker runs the docker CLI.
type Docker interface {
	// Run returns standard output even on failure; the error carries a
	// bounded standard error. env is added to the CLI's environment.
	Run(ctx context.Context, env []string, args ...string) ([]byte, error)
}

// Agent answers one verb per invocation.
type Agent struct {
	Config Config
	// ConfigErr is set when the host configuration could not be loaded; every
	// verb but info then refuses with not_ready.
	ConfigErr error
	Docker    Docker
	FreeBytes func(path string) (uint64, error)
	// Lock serializes start and remove on one slot.
	Lock    func(slot int) (func(), error)
	Now     func() time.Time
	Version string
}

// Handle runs one command. It never returns the JIT configuration or any
// other request content in its reply.
func (a *Agent) Handle(ctx context.Context, command string, stdin io.Reader) pool.Reply {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return refuse(pool.CodeInvalid, "empty command")
	}
	if a.Now == nil {
		a.Now = time.Now
	}
	if fields[0] == "info" && len(fields) == 1 {
		return a.info(ctx)
	}
	if a.ConfigErr != nil {
		return refuse(pool.CodeNotReady, "host is not installed: "+a.ConfigErr.Error())
	}
	switch {
	case fields[0] == "list" && len(fields) == 1:
		return a.list(ctx)
	case fields[0] == "start" && len(fields) == 2:
		slot, ok := a.slotIndex(fields[1])
		if !ok {
			return refuse(pool.CodeInvalid, "invalid slot")
		}
		return a.locked(slot, func() pool.Reply { return a.start(ctx, slot, stdin) })
	case fields[0] == "remove" && len(fields) == 3:
		slot, ok := a.slotIndex(fields[1])
		if !ok || !safeLabelValue.MatchString(fields[2]) {
			return refuse(pool.CodeInvalid, "invalid slot or lease")
		}
		return a.locked(slot, func() pool.Reply { return a.remove(ctx, slot, fields[2]) })
	default:
		return refuse(pool.CodeInvalid, "unknown command")
	}
}

func (a *Agent) slotIndex(value string) (int, bool) {
	slot, err := strconv.Atoi(value)
	if err != nil || slot < 0 || slot >= a.Config.Slots || strconv.Itoa(slot) != value {
		return 0, false
	}
	return slot, true
}

func (a *Agent) locked(slot int, run func() pool.Reply) pool.Reply {
	if a.Lock == nil {
		return run()
	}
	unlock, err := a.Lock(slot)
	if err != nil {
		return refuse(pool.CodeDockerFailed, "lock slot: "+err.Error())
	}
	defer unlock()
	return run()
}

func (a *Agent) info(ctx context.Context) pool.Reply {
	info := pool.HostInfo{Version: a.Version}
	if a.ConfigErr == nil {
		info.Slots = a.Config.Slots
		info.Image = a.Config.Image
		info.Ready = a.imagePresent(ctx)
		if free, err := a.freeBytes(); err == nil {
			info.DiskFreeBytes = free
		}
	}
	return succeed(info)
}

func (a *Agent) list(ctx context.Context) pool.Reply {
	slots, err := a.slots(ctx)
	if err != nil {
		return refuse(pool.CodeDockerFailed, err.Error())
	}
	return succeed(slots)
}

func (a *Agent) start(ctx context.Context, slot int, stdin io.Reader) pool.Reply {
	contents, err := io.ReadAll(io.LimitReader(stdin, maxRequestBytes+1))
	if err != nil || len(contents) > maxRequestBytes {
		return refuse(pool.CodeInvalid, "unreadable or oversized start request")
	}
	var request pool.StartRequest
	if err := json.Unmarshal(contents, &request); err != nil {
		return refuse(pool.CodeInvalid, "start request is not valid JSON")
	}
	if request.Image != a.Config.Image {
		return refuse(pool.CodeImageMismatch, "host serves "+a.Config.Image)
	}
	if err := a.validate(request); err != nil {
		return refuse(pool.CodeInvalid, err.Error())
	}
	name := containerName(slot)
	if _, found, err := a.containerLabels(ctx, name); err != nil {
		return refuse(pool.CodeDockerFailed, err.Error())
	} else if found {
		return refuse(pool.CodeSlotBusy, "slot already holds a container")
	}
	if !a.imagePresent(ctx) {
		return refuse(pool.CodeNotReady, "runner image is not present")
	}
	if reply, ok := a.ensureDisk(ctx, slot); !ok {
		return reply
	}
	env := []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=" + request.JITConfig}
	if _, err := a.Docker.Run(ctx, env, a.runArgs(slot, request)...); err != nil {
		// Remove a half-created container, but only one this request made.
		if labels, found, inspectErr := a.containerLabels(context.WithoutCancel(ctx), name); inspectErr == nil && found && labels[labelLease] == request.LeaseID {
			_, _ = a.Docker.Run(context.WithoutCancel(ctx), nil, "rm", "--force", name)
		}
		return refuse(pool.CodeDockerFailed, "docker run: "+err.Error())
	}
	started, err := a.slotByName(ctx, name)
	if err != nil {
		return refuse(pool.CodeDockerFailed, "inspect started slot: "+err.Error())
	}
	return succeed(started)
}

func (a *Agent) validate(request pool.StartRequest) error {
	now := a.Now()
	switch {
	case !safeLabelValue.MatchString(request.ControllerID):
		return errors.New("invalid controller id")
	case !safeLabelValue.MatchString(request.LeaseID):
		return errors.New("invalid lease id")
	case !safeHostname.MatchString(request.RunnerName):
		return errors.New("invalid runner name")
	case request.RunnerID < 0 || request.RunnerScaleSetID < 0:
		return errors.New("invalid runner registration")
	case request.JITConfig == "" || len(request.JITConfig) > maxJITBytes || !safeJIT.MatchString(request.JITConfig):
		return errors.New("invalid JIT configuration")
	case !request.Deadline.After(now) || request.Deadline.After(now.Add(25*time.Hour)):
		return errors.New("deadline must be in the future and within 25h")
	case request.IdleTimeoutSeconds < 0 || request.IdleTimeoutSeconds > 86_400:
		return errors.New("invalid idle timeout")
	}
	return nil
}

func (a *Agent) runArgs(slot int, request pool.StartRequest) []string {
	name := containerName(slot)
	args := []string{
		"run", "--detach",
		"--name", name,
		"--hostname", request.RunnerName,
		"--privileged",
		"--label", labelSlot + "=" + strconv.Itoa(slot),
		"--label", labelController + "=" + request.ControllerID,
		"--label", labelLease + "=" + request.LeaseID,
		"--label", labelRunnerName + "=" + request.RunnerName,
		"--label", labelRunnerID + "=" + strconv.FormatInt(request.RunnerID, 10),
		"--label", labelScaleSetID + "=" + strconv.Itoa(request.RunnerScaleSetID),
		"--label", labelImage + "=" + request.Image,
		"--log-driver", "json-file", "--log-opt", "max-size=20m", "--log-opt", "max-file=2",
	}
	if a.Config.SlotCPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(a.Config.SlotCPUs, 'f', -1, 64))
	}
	if a.Config.SlotMemoryMB > 0 {
		args = append(args, "--memory", strconv.Itoa(a.Config.SlotMemoryMB)+"m")
	}
	args = append(args,
		// The value comes from the CLI's environment, never from argv.
		"--env", "ACTIONS_RUNNER_INPUT_JITCONFIG",
		"--env", "RUNNERYARD_DEADLINE="+request.Deadline.UTC().Format(time.RFC3339),
		"--env", "RUNNERYARD_IDLE_TIMEOUT="+strconv.FormatInt(request.IdleTimeoutSeconds, 10),
		"--env", "RUNNERYARD_REUSED_SLOT=1",
	)
	for _, volume := range a.volumes(slot) {
		args = append(args, "--mount", "type=volume,source="+volume.name+",target="+volume.target)
	}
	return append(args,
		"--entrypoint", "/usr/bin/dumb-init",
		request.Image, "--", "/usr/local/bin/runner-entrypoint",
	)
}

type volume struct {
	name   string
	target string
}

// volumes are the slot's persistent paths: the nested Docker data (images
// and build cache), the runner work directory, and the configured caches.
func (a *Agent) volumes(slot int) []volume {
	prefix := containerName(slot)
	volumes := []volume{
		{name: prefix + "-docker", target: "/var/lib/docker"},
		{name: prefix + "-work", target: "/home/runner/_work"},
	}
	for _, path := range a.Config.Caches {
		volumes = append(volumes, volume{name: prefix + "-cache-" + cacheVolumeSuffix(path), target: path})
	}
	return volumes
}

func cacheVolumeSuffix(path string) string {
	return strings.ReplaceAll(strings.TrimPrefix(path, "/"), "/", "-")
}

func (a *Agent) ensureDisk(ctx context.Context, slot int) (pool.Reply, bool) {
	if a.Config.MinFreeDiskGB == 0 {
		return pool.Reply{}, true
	}
	required := uint64(a.Config.MinFreeDiskGB) << 30 // #nosec G115 -- Validate bounds it to 0..10000
	free, err := a.freeBytes()
	if err != nil || free >= required {
		return pool.Reply{}, true
	}
	// The slot has no container, so its volumes are unused: drop them and let
	// this job start cold rather than fill the disk.
	args := []string{"volume", "rm", "--force"}
	for _, volume := range a.volumes(slot) {
		args = append(args, volume.name)
	}
	if _, err := a.Docker.Run(ctx, nil, args...); err != nil {
		return refuse(pool.CodeDiskFull, "free disk below minimum and slot caches could not be removed: "+err.Error()), false
	}
	if free, err = a.freeBytes(); err == nil && free < required {
		return refuse(pool.CodeDiskFull, fmt.Sprintf("free disk %d GB below minimum %d GB", free>>30, a.Config.MinFreeDiskGB)), false
	}
	return pool.Reply{}, true
}

func (a *Agent) remove(ctx context.Context, slot int, leaseID string) pool.Reply {
	name := containerName(slot)
	labels, found, err := a.containerLabels(ctx, name)
	if err != nil {
		return refuse(pool.CodeDockerFailed, err.Error())
	}
	if !found {
		return succeed(map[string]bool{"removed": false})
	}
	if labels[labelLease] != leaseID {
		return refuse(pool.CodeLeaseMismatch, "slot holds another lease")
	}
	if _, err := a.Docker.Run(ctx, nil, "rm", "--force", name); err != nil {
		return refuse(pool.CodeDockerFailed, "docker rm: "+err.Error())
	}
	return succeed(map[string]bool{"removed": true})
}

func (a *Agent) imagePresent(ctx context.Context) bool {
	_, err := a.Docker.Run(ctx, nil, "image", "inspect", "--format", "{{.Id}}", a.Config.Image)
	return err == nil
}

func (a *Agent) freeBytes() (uint64, error) {
	if a.FreeBytes == nil {
		return 0, errors.New("disk space unknown")
	}
	root := a.Config.DockerRoot
	if root == "" {
		root = defaultDockerFS
	}
	return a.FreeBytes(root)
}

type container struct {
	Name    string    `json:"Name"`
	Created time.Time `json:"Created"`
	State   struct {
		Status string `json:"Status"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

func (a *Agent) slots(ctx context.Context) ([]pool.Slot, error) {
	output, err := a.Docker.Run(ctx, nil, "ps", "--all", "--no-trunc", "--filter", "label="+labelSlot, "--format", "{{.ID}}")
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	ids := strings.Fields(string(output))
	slots := make([]pool.Slot, 0, len(ids))
	if len(ids) == 0 {
		return slots, nil
	}
	// A container removed between ps and inspect fails the inspect for that
	// one ID only; the others are still printed.
	output, err = a.Docker.Run(ctx, nil, append([]string{"inspect"}, ids...)...)
	var containers []container
	if decodeErr := json.Unmarshal(output, &containers); decodeErr != nil {
		if err != nil {
			return nil, fmt.Errorf("docker inspect: %w", err)
		}
		return nil, fmt.Errorf("decode docker inspect: %w", decodeErr)
	}
	for _, item := range containers {
		if slot, ok := toSlot(item); ok {
			slots = append(slots, slot)
		}
	}
	return slots, nil
}

func (a *Agent) slotByName(ctx context.Context, name string) (pool.Slot, error) {
	output, err := a.Docker.Run(ctx, nil, "inspect", name)
	if err != nil {
		return pool.Slot{}, err
	}
	var containers []container
	if err := json.Unmarshal(output, &containers); err != nil || len(containers) != 1 {
		return pool.Slot{}, errors.New("unexpected docker inspect output")
	}
	slot, ok := toSlot(containers[0])
	if !ok {
		return pool.Slot{}, errors.New("started container lacks slot labels")
	}
	return slot, nil
}

func (a *Agent) containerLabels(ctx context.Context, name string) (map[string]string, bool, error) {
	output, err := a.Docker.Run(ctx, nil, "inspect", "--type", "container", name)
	if err != nil {
		if notFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("docker inspect: %w", err)
	}
	var containers []container
	if err := json.Unmarshal(output, &containers); err != nil || len(containers) != 1 {
		return nil, false, errors.New("unexpected docker inspect output")
	}
	return containers[0].Config.Labels, true, nil
}

func toSlot(item container) (pool.Slot, bool) {
	labels := item.Config.Labels
	index, err := strconv.Atoi(labels[labelSlot])
	if err != nil || index < 0 {
		return pool.Slot{}, false
	}
	runnerID, _ := strconv.ParseInt(labels[labelRunnerID], 10, 64)
	scaleSetID, _ := strconv.Atoi(labels[labelScaleSetID])
	return pool.Slot{
		Index:            index,
		ControllerID:     labels[labelController],
		LeaseID:          labels[labelLease],
		RunnerName:       labels[labelRunnerName],
		RunnerID:         runnerID,
		RunnerScaleSetID: scaleSetID,
		State:            workerState(item.State.Status),
		CreatedAt:        item.Created,
	}, true
}

// workerState maps a Docker container state to the provider words the core
// understands: an exited container is a stopped worker and is retired.
func workerState(status string) string {
	switch status {
	case "running":
		return "started"
	case "created", "restarting":
		return "starting"
	case "removing":
		return "destroying"
	case "exited", "dead", "paused":
		return "stopped"
	default:
		return "unknown"
	}
}

func containerName(slot int) string { return "runneryard-slot-" + strconv.Itoa(slot) }

func notFound(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "no such")
}

func succeed(result any) pool.Reply {
	encoded, err := json.Marshal(result)
	if err != nil {
		return refuse(pool.CodeDockerFailed, "encode reply")
	}
	return pool.Reply{OK: true, Result: encoded}
}

func refuse(code, message string) pool.Reply {
	return pool.Reply{Error: &pool.ReplyError{Code: code, Message: message}}
}
