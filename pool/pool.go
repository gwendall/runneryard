// Package pool places RunnerYard workers on shared hosts. A host runs a fixed
// number of slots; each slot runs one fresh runner container per job, keeps
// its caches between jobs, and is cleaned when the job ends. Hosts come from a
// machine source (owned machines, Hetzner Cloud, ...) and are created and
// returned according to how that source bills them.
//
// A Pool is a provider.Compute, so the controller core is unchanged: a slot
// holding a lease is a worker, and its lifecycle (launch, inventory, destroy,
// budget, retirement journal) is the one every provider already follows.
package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Billing describes how a machine source charges for a host. The pool derives
// when an empty host is returned from it, instead of a hand-tuned timer.
type Billing string

const (
	// BillingPerSecond: return an empty host after a short idle period.
	BillingPerSecond Billing = "per_second"
	// BillingPerStartedHour: every started hour is paid in full, so keep an
	// empty host until shortly before its paid hour ends, then return it.
	BillingPerStartedHour Billing = "per_started_hour"
	// BillingOwned: monthly or already paid; never returned.
	BillingOwned Billing = "owned"
)

// Facts describe a machine source. They are data, not code: placement and
// retirement read them, so a new source only states its facts.
type Facts struct {
	Billing Billing
	// HourlyPrice is the price of one host hour in Currency, zero when owned.
	HourlyPrice float64
	Currency    string
	// CanCreate reports whether the source can create and delete hosts. A
	// static source cannot: its hosts exist before the controller.
	CanCreate bool
}

// Host is one machine that runs slots.
type Host struct {
	// ID is stable and unique within the pool, such as "hetzner-123456".
	ID string
	// Address is the agent's SSH endpoint, host:port.
	Address string
	// HostKey is the host's SSH public key in authorized_keys format; the
	// controller refuses any other key.
	HostKey   string
	CreatedAt time.Time
	// State is the source's own word for the machine ("running", "off", ...).
	// A source that can stop hosts reports HostStopped for a stopped one.
	State string
	// StateSince is when the source last changed the machine's state, when it
	// knows: the start of the current running or stopped period.
	StateSince time.Time
	// Image is the runner image the source prepared the host for, when it
	// records it. A stopped host prepared for another image, or for an
	// earlier digest of a followed tag, is deleted instead of started.
	Image string
}

// HostStopped is the Host.State of a stopped host: it keeps its disk, and with
// it the slots' images and caches, and costs only its storage.
const HostStopped = "stopped"

// Source provides hosts. List must return only hosts this controller owns in
// this pool. Create and Delete are only called when Facts().CanCreate; Create
// prepares the host for the runner image it is given.
type Source interface {
	Facts() Facts
	List(context.Context) ([]Host, error)
	Create(ctx context.Context, image string) (Host, error)
	Delete(context.Context, string) error
}

// Follower follows the tag a pool's runner image names, for a pool whose
// hosts must not stay on the image the tag named when they were created.
type Follower interface {
	// Resolve returns the image pinned to the digest the tag names now, such
	// as registry.example/runner@sha256:....
	Resolve(context.Context) (string, error)
	// Related reports whether image is from the followed repository: the tag
	// itself, or one of the repository's digests. A host on such an image
	// keeps serving until the pool replaces it.
	Related(image string) bool
}

// Stopper is a source whose hosts can be stopped and started again with
// their disk. A pool over such a source stops an idle host instead of
// deleting it, and starts a stopped host before it creates a new one. Stop
// and Start act only on hosts the source owns, and return once the host has
// reached the state asked for.
type Stopper interface {
	Stop(ctx context.Context, id string) error
	Start(ctx context.Context, id string) error
}

// Slot is one runner container as the host agent reports it.
type Slot struct {
	Index            int       `json:"slot"`
	ControllerID     string    `json:"controller_id"`
	LeaseID          string    `json:"lease_id"`
	RunnerName       string    `json:"runner_name"`
	RunnerID         int64     `json:"runner_id"`
	RunnerScaleSetID int       `json:"runner_scale_set_id"`
	State            string    `json:"state"`
	CreatedAt        time.Time `json:"created_at"`
}

// HostInfo is the agent's description of its host.
type HostInfo struct {
	Version string `json:"version"`
	Slots   int    `json:"slots"`
	Image   string `json:"image"`
	// Ready is true once the image is present and the host accepts slots.
	Ready         bool   `json:"ready"`
	DiskFreeBytes uint64 `json:"disk_free_bytes"`
}

// StartRequest is sent on the agent's standard input, never on its command
// line: it carries the job's single-use JIT configuration.
type StartRequest struct {
	Image              string    `json:"image"`
	ControllerID       string    `json:"controller_id"`
	LeaseID            string    `json:"lease_id"`
	RunnerName         string    `json:"runner_name"`
	RunnerID           int64     `json:"runner_id"`
	RunnerScaleSetID   int       `json:"runner_scale_set_id"`
	JITConfig          string    `json:"jit_config"`
	Deadline           time.Time `json:"deadline"`
	IdleTimeoutSeconds int64     `json:"idle_timeout_seconds"`
}

// Agent talks to the host agent on one host.
type Agent interface {
	Info(context.Context, Host) (HostInfo, error)
	List(context.Context, Host) ([]Slot, error)
	Start(context.Context, Host, int, StartRequest) (Slot, error)
	// Remove deletes the slot's container only if it holds leaseID; a slot
	// that is already empty is not an error.
	Remove(context.Context, Host, int, string) error
}

// Reply is the host agent's answer: one JSON object on standard output.
type Reply struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ReplyError     `json:"error,omitempty"`
}

// ReplyError is a refusal stated by the host agent.
type ReplyError struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// AgentError is an answer the host agent stated itself. Every refusal but
// docker_failed means nothing was started or removed; docker_failed reports
// that Docker itself failed, possibly after a container started, so a start
// that ends with it is treated as ambiguous. Code is a stable word such as
// "slot_busy".
type AgentError struct {
	Code    string
	Message string
}

func (e *AgentError) Error() string {
	if e.Message == "" {
		return "host agent refused: " + e.Code
	}
	return fmt.Sprintf("host agent refused (%s): %s", e.Code, e.Message)
}

// Stable agent refusal codes.
const (
	CodeInvalid       = "invalid"
	CodeSlotBusy      = "slot_busy"
	CodeLeaseMismatch = "lease_mismatch"
	CodeNotReady      = "not_ready"
	CodeImageMismatch = "image_mismatch"
	CodeDiskFull      = "disk_full"
	CodeDockerFailed  = "docker_failed"
)

// IsAgentRefusal reports whether err is a stated refusal, with the given code
// when code is not empty.
func IsAgentRefusal(err error, code string) bool {
	var refusal *AgentError
	return errors.As(err, &refusal) && (code == "" || refusal.Code == code)
}

// ErrFull reports that no host of the pool has a free slot. Nothing was
// created; a placement may try its next tier.
var ErrFull = errors.New("pool has no free slot")

// ErrUnreachable reports that the host agent could not be reached, so the
// command never ran. Any other transport failure is ambiguous.
var ErrUnreachable = errors.New("host agent unreachable")

var safeName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// ValidName reports whether name can name a pool or a placement tier.
func ValidName(name string) bool { return safeName.MatchString(name) }
