package pool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gwendall/runneryard/provider"
)

const (
	defaultTick             = 10 * time.Second
	defaultHostListInterval = 30 * time.Second
	defaultBootTimeout      = 10 * time.Minute
	defaultUnreachableGrace = 2 * time.Minute
	defaultIdleReturn       = 2 * time.Minute
	defaultHourMargin       = 2 * time.Minute
	defaultCallTimeout      = 20 * time.Second
	defaultStartTimeout     = 90 * time.Second
	defaultDeleteStopped    = 6 * time.Hour
	defaultImageRefresh     = 5 * time.Minute
	hostRefusalBackoff      = time.Minute
	initialCreateBackoff    = time.Minute
	maximumCreateBackoff    = 15 * time.Minute
	maxStartAttempts        = 3
	// maxStartFailures is how many failed starts a stopped host gets before
	// it is deleted: its disk sits on one physical machine, which may stay
	// full.
	maxStartFailures = 2
	maxParallelCalls = 8
	// defaultLateStartWait is how long a start whose answer was lost or late
	// is read back before it counts as failed: the host agent's own two-minute
	// limit on a command, less the 90 s StartTimeout. After that the agent has
	// started the container or given up on it.
	defaultLateStartWait = 30 * time.Second
	defaultLateStartPoll = 2 * time.Second
	// CapacityReasonFull is the stable reason a full pool reports to the core.
	CapacityReasonFull = "pool_full"
)

// imageRetry paces the next resolution of a followed tag after a failed one;
// replaceInterval spaces out the replacement of hosts on an earlier digest.
const (
	imageRetry      = time.Minute
	replaceInterval = 5 * time.Minute
)

// Config configures one pool.
type Config struct {
	Name         string
	ControllerID string
	// Image is the exact runner image every slot runs. A host prepared for
	// another image is drained and, when the source can create hosts, replaced.
	Image    string
	MinHosts int
	MaxHosts int
	Source   Source
	Agent    Agent
	// Follow, when set, follows the tag Image names, for a source that
	// creates hosts. Every ImageRefresh (default 5m) the pool resolves the
	// tag and creates new hosts from that digest. A host on another image of
	// the same repository keeps serving, and is replaced once it is idle or
	// drained, one host at a time.
	Follow       Follower
	ImageRefresh time.Duration
	// MaxLifetime bounds every slot container. A host that has not answered
	// for longer cannot still be running a job and is returned.
	MaxLifetime time.Duration
	Logger      *slog.Logger
	Now         func() time.Time

	Tick             time.Duration
	HostListInterval time.Duration
	BootTimeout      time.Duration
	UnreachableGrace time.Duration
	IdleReturn       time.Duration
	HourMargin       time.Duration
	CallTimeout      time.Duration
	StartTimeout     time.Duration

	// IdleStop, KeepStopped, and DeleteStoppedAfter apply when the source is
	// a Stopper. An empty host is stopped after IdleStop (default IdleReturn)
	// instead of deleted. The KeepStopped most recently stopped hosts are
	// kept; the others are deleted once stopped for DeleteStoppedAfter
	// (default 6h).
	IdleStop           time.Duration
	KeepStopped        int
	DeleteStoppedAfter time.Duration

	// Speculative lets the controller start a second runner in a free slot
	// for a job that already waits on a later tier's booting worker. See
	// Placement.LaunchSpare.
	Speculative bool
}

type phase string

const (
	phaseCreating phase = "creating"
	phaseBooting  phase = "booting"
	phaseReady    phase = "ready"
	phaseDraining phase = "draining"
	phaseDeleting phase = "deleting"
	phaseStopping phase = "stopping"
	phaseStopped  phase = "stopped"
	phaseStarting phase = "starting"
)

type hostState struct {
	host     Host
	phase    phase
	info     HostInfo
	slots    map[int]Slot
	reserved map[int]string
	// firstSeen is when this process first saw the host; lastContact and
	// lastListed are the last successful agent call and slot listing.
	firstSeen   time.Time
	lastContact time.Time
	lastListed  time.Time
	everReady   bool
	// ownBoot marks a host seen during its boot window: it never ran a slot
	// before this process, so a failed boot can be returned safely.
	ownBoot     bool
	emptySince  time.Time
	notBefore   time.Time
	drainReason string
	warnedAt    time.Time

	// bootSince is when the pool last started a stopped host; zero for a
	// host still on its first boot.
	bootSince time.Time
	// runningSince starts the current billed period, zero while stopped;
	// billed holds the hours of earlier periods.
	runningSince time.Time
	billed       float64
	stoppedSince time.Time
	// startedByPool marks a host this process started that is not ready
	// yet; startFailures counts its failed starts and boots.
	startedByPool bool
	startFailures int
	// changedAt is the pool's last stop or start of the host. A listing that
	// began before it may predate the change and does not override it.
	changedAt time.Time
	// createdImage is the image this process created the host from.
	createdImage string

	// meter measures the host since firstSeen; billedBefore is the hours it
	// had been billed when this process first saw it running.
	meter        meter
	billedBefore float64
}

func (h *hostState) occupied() int { return len(h.slots) }

func (h *hostState) freeSlot() (int, bool) {
	for index := range h.info.Slots {
		if _, busy := h.slots[index]; busy {
			continue
		}
		if _, reserved := h.reserved[index]; reserved {
			continue
		}
		return index, true
	}
	return 0, false
}

func (h *hostState) freeCount() int {
	free := 0
	for index := range h.info.Slots {
		_, busy := h.slots[index]
		_, reserved := h.reserved[index]
		if !busy && !reserved {
			free++
		}
	}
	return free
}

// Pool runs workers in the slots of shared hosts. It implements
// provider.Compute; Run keeps the hosts matched to demand.
type Pool struct {
	cfg     Config
	facts   Facts
	stopper Stopper
	log     *slog.Logger

	mu              sync.Mutex
	hosts           map[string]*hostState
	hostsListedAt   time.Time
	hostsKnown      bool
	demand          int
	createSeq       int
	createNotBefore time.Time
	createBackoff   time.Duration
	launched        uint64
	overflowed      uint64
	speculated      uint64
	returnedHosts   int
	returnedHours   float64
	hostStarts      int
	hostStops       int
	creations       sync.WaitGroup
	wake            chan struct{}
	// roomReady, when set, is called when a host becomes ready, so the
	// controller can use its slots before its next GitHub message.
	roomReady func()

	// image is what new hosts are created from: Config.Image, or the digest
	// its followed tag named at the last resolution (imageResolved).
	image          string
	imageResolved  bool
	imageCheckAt   time.Time
	imageCheckedAt time.Time
	imageError     string
	// replaceNotBefore spaces out the replacement of hosts on an earlier
	// digest. imageFailed records a creation or a boot from the current
	// image that failed: the next replacement then waits for a host on that
	// image to be ready, so an image that cannot serve costs one host at most.
	replaceNotBefore time.Time
	imageFailed      bool
	imageReplaced    int
	// lateStartWait and lateStartPoll bound the read-back of a late start;
	// tests shorten them.
	lateStartWait time.Duration
	lateStartPoll time.Duration

	// measuredSince starts the usage figures; retired holds the usage of
	// hosts no longer in hosts, retiredBefore the billedBefore of returned
	// ones; slotsSeen is the most slots a host has reported.
	measuredSince time.Time
	retired       usage
	retiredBefore float64
	slotsSeen     int
}

// New validates the configuration and returns an idle pool.
func New(cfg Config) (*Pool, error) {
	if !ValidName(cfg.Name) {
		return nil, fmt.Errorf("pool name %q must be lowercase letters, digits, and hyphens", cfg.Name)
	}
	if cfg.ControllerID == "" || cfg.Image == "" || cfg.Source == nil || cfg.Agent == nil {
		return nil, fmt.Errorf("pool %s requires a controller ID, a runner image, a host source, and a host agent", cfg.Name)
	}
	facts := cfg.Source.Facts()
	switch facts.Billing {
	case BillingPerSecond, BillingPerStartedHour, BillingOwned:
	default:
		return nil, fmt.Errorf("pool %s source has unknown billing %q", cfg.Name, facts.Billing)
	}
	if facts.CanCreate {
		if cfg.MinHosts < 0 || cfg.MaxHosts < 1 || cfg.MinHosts > cfg.MaxHosts {
			return nil, fmt.Errorf("pool %s host bounds must satisfy 0 <= min_hosts <= max_hosts and max_hosts >= 1", cfg.Name)
		}
	} else if cfg.MinHosts != 0 || cfg.MaxHosts != 0 {
		return nil, fmt.Errorf("pool %s source cannot create hosts, so min_hosts and max_hosts do not apply", cfg.Name)
	}
	if cfg.Follow != nil && !facts.CanCreate {
		return nil, fmt.Errorf("pool %s source cannot create hosts, so it cannot follow an image tag", cfg.Name)
	}
	if cfg.MaxLifetime < time.Minute {
		return nil, fmt.Errorf("pool %s requires the worker maximum lifetime", cfg.Name)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	defaults := []struct {
		value    *time.Duration
		fallback time.Duration
	}{
		{&cfg.Tick, defaultTick},
		{&cfg.HostListInterval, defaultHostListInterval},
		{&cfg.BootTimeout, defaultBootTimeout},
		{&cfg.UnreachableGrace, defaultUnreachableGrace},
		{&cfg.IdleReturn, defaultIdleReturn},
		{&cfg.HourMargin, defaultHourMargin},
		{&cfg.CallTimeout, defaultCallTimeout},
		{&cfg.StartTimeout, defaultStartTimeout},
		{&cfg.DeleteStoppedAfter, defaultDeleteStopped},
		{&cfg.ImageRefresh, defaultImageRefresh},
	}
	for _, item := range defaults {
		if *item.value <= 0 {
			*item.value = item.fallback
		}
	}
	if cfg.IdleStop <= 0 {
		cfg.IdleStop = cfg.IdleReturn
	}
	if facts.Billing == BillingPerStartedHour && cfg.HourMargin <= cfg.Tick {
		return nil, fmt.Errorf("pool %s must check its hosts more often than its paid-hour margin", cfg.Name)
	}
	var stopper Stopper
	if facts.CanCreate {
		stopper, _ = cfg.Source.(Stopper)
	}
	if stopper == nil && cfg.KeepStopped != 0 {
		return nil, fmt.Errorf("pool %s source cannot stop hosts, so keep_stopped does not apply", cfg.Name)
	}
	if cfg.KeepStopped < 0 || cfg.KeepStopped > cfg.MaxHosts {
		return nil, fmt.Errorf("pool %s must keep between 0 and max_hosts stopped hosts", cfg.Name)
	}
	return &Pool{
		cfg:     cfg,
		facts:   facts,
		stopper: stopper,
		log:     cfg.Logger.With("pool", cfg.Name),
		hosts:   make(map[string]*hostState),
		wake:    make(chan struct{}, 1),
		image:   cfg.Image,

		lateStartWait: defaultLateStartWait,
		lateStartPoll: defaultLateStartPoll,
		measuredSince: cfg.Now(),
	}, nil
}

// Name returns the pool name.
func (p *Pool) Name() string { return p.cfg.Name }

// Launch starts the lease in a free slot. A full pool returns a capacity error
// with reason pool_full and creates nothing.
func (p *Pool) Launch(ctx context.Context, lease provider.Lease) (provider.Worker, error) {
	return p.launch(ctx, lease, true)
}

// launchSpare starts a speculative lease in a free slot. A full pool returns
// the same capacity error as Launch but asks for no host: the job already has
// a worker on its way.
func (p *Pool) launchSpare(ctx context.Context, lease provider.Lease) (provider.Worker, error) {
	worker, err := p.launch(ctx, lease, false)
	if err == nil {
		p.mu.Lock()
		p.speculated++
		p.mu.Unlock()
	}
	return worker, err
}

// speculative reports whether the pool takes speculative leases.
func (p *Pool) speculative() bool { return p.cfg.Speculative }

// onRoomReady sets the function called when a host becomes ready.
func (p *Pool) onRoomReady(notify func()) {
	p.mu.Lock()
	p.roomReady = notify
	p.mu.Unlock()
}

// freeSlots counts the slots a launch could take now, on ready hosts only.
func (p *Pool) freeSlots() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	free := 0
	for _, h := range p.hosts {
		if p.takesLeases(h, now) {
			free += h.freeCount()
		}
	}
	return free
}

func (p *Pool) launch(ctx context.Context, lease provider.Lease, grow bool) (provider.Worker, error) {
	if lease.ID == "" || lease.RunnerName == "" || lease.JITConfig == "" || strings.Contains(lease.ID, "/") {
		return provider.Worker{}, errors.New("pool launch requires a lease ID, a runner name, and a JIT configuration")
	}
	request := StartRequest{
		ControllerID:       p.cfg.ControllerID,
		LeaseID:            lease.ID,
		RunnerName:         lease.RunnerName,
		RunnerID:           lease.RunnerID,
		RunnerScaleSetID:   lease.RunnerScaleSetID,
		JITConfig:          lease.JITConfig,
		Deadline:           lease.Deadline.UTC(),
		IdleTimeoutSeconds: int64(lease.IdleTimeout / time.Second),
	}
	for range maxStartAttempts {
		host, slot, image, ok := p.reserve(lease.ID)
		if !ok {
			break
		}
		// The host's own image: the pool's, or while the host waits for its
		// replacement, an earlier digest of the followed tag.
		request.Image = image
		startCtx, cancel := context.WithTimeout(ctx, p.cfg.StartTimeout)
		began := time.Now()
		started, err := p.cfg.Agent.Start(startCtx, host, slot, request)
		cancel()
		if err == nil {
			return p.commit(host.ID, slot, started, lease), nil
		}
		switch {
		case errors.Is(err, ErrUnreachable):
			p.release(host.ID, slot)
			p.refuseHost(host.ID, "unreachable", err)
		case IsAgentRefusal(err, CodeSlotBusy):
			p.release(host.ID, slot)
			p.markForeign(host.ID, slot)
		case IsAgentRefusal(err, CodeNotReady), IsAgentRefusal(err, CodeImageMismatch), IsAgentRefusal(err, CodeDiskFull):
			p.release(host.ID, slot)
			p.refuseHost(host.ID, refusalCode(err), err)
		case IsAgentRefusal(err, CodeInvalid):
			p.release(host.ID, slot)
			return provider.Worker{}, fmt.Errorf("pool %s host %s rejected the lease: %w", p.cfg.Name, host.ID, err)
		default:
			// The agent may have created the container: docker reported a
			// failure, or the session ended before the answer. A container
			// that runs this lease is the start, only late: keep it.
			// Otherwise keep the slot taken and hand the worker to the core,
			// which removes it by lease.
			if late, ok := p.readLateStart(ctx, host, slot, lease, err); ok {
				p.log.Info("pool slot answered late; keeping its runner", "host", host.ID, "slot", slot, "lease", lease.ID,
					"after", time.Since(began).Round(time.Second).String(), "error", err)
				return p.commit(host.ID, slot, late, lease), nil
			}
			worker := slotWorker(host.ID, slot, Slot{Index: slot, ControllerID: p.cfg.ControllerID, State: "starting", CreatedAt: p.cfg.Now()}, lease)
			p.holdSuspect(host.ID, slot, lease)
			return provider.Worker{}, &provider.PartialLaunchError{Worker: worker, Err: fmt.Errorf("start slot %d on %s: %w", slot, host.ID, err)}
		}
	}
	if !grow {
		return provider.Worker{}, &provider.CapacityError{Reason: CapacityReasonFull, Err: fmt.Errorf("pool %s: %w", p.cfg.Name, ErrFull)}
	}
	retryAfter := p.noteFull()
	return provider.Worker{}, &provider.CapacityError{Reason: CapacityReasonFull, RetryAfter: retryAfter, Err: fmt.Errorf("pool %s: %w", p.cfg.Name, ErrFull)}
}

// Inventory lists this controller's slots on every known host. A host that
// does not answer keeps its last listing for UnreachableGrace, so a short
// network failure does not make running jobs look departed.
func (p *Pool) Inventory(ctx context.Context) ([]provider.Worker, error) {
	if err := p.refreshHosts(ctx, false); err != nil {
		if !p.known() {
			return nil, fmt.Errorf("pool %s: list hosts: %w", p.cfg.Name, err)
		}
		p.log.Warn("host source unavailable; using the last known hosts", "error", err)
	}
	p.listSlots(ctx, p.hostsIn(func(h *hostState) bool {
		return h.host.Address != "" && h.running() && (h.everReady || !h.ownBoot)
	}))
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	workers := make([]provider.Worker, 0)
	for _, id := range p.sortedIDs() {
		h := p.hosts[id]
		if h.lastListed.IsZero() || now.Sub(h.lastListed) > p.cfg.UnreachableGrace {
			continue
		}
		for _, index := range sortedSlots(h.slots) {
			slot := h.slots[index]
			if slot.ControllerID != p.cfg.ControllerID || slot.LeaseID == "" || slot.RunnerName == "" {
				continue
			}
			workers = append(workers, slotWorker(id, index, slot, provider.Lease{}))
		}
	}
	return workers, nil
}

// Destroy removes the slot container if it still holds the lease. A slot that
// is empty or holds another lease, and a host that no longer exists, are
// already destroyed.
func (p *Pool) Destroy(ctx context.Context, workerID string) error {
	hostID, slot, leaseID, err := parseWorkerID(workerID)
	if err != nil {
		return err
	}
	host, ok := p.host(hostID)
	if !ok {
		if err := p.refreshHosts(ctx, true); err != nil {
			return &provider.TransientError{Err: fmt.Errorf("pool %s: confirm host %s: %w", p.cfg.Name, hostID, err)}
		}
		if host, ok = p.host(hostID); !ok {
			return nil
		}
	}
	if p.stoppedHost(hostID) {
		// A stopped host runs nothing, and its next boot removes every slot
		// container before it serves again.
		p.cleared(hostID, slot, leaseID)
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, p.cfg.CallTimeout)
	defer cancel()
	err = p.cfg.Agent.Remove(callCtx, host, slot, leaseID)
	if err == nil || IsAgentRefusal(err, CodeLeaseMismatch) {
		p.cleared(hostID, slot, leaseID)
		return nil
	}
	// Only a malformed request fails for good; a Docker or lock failure on
	// the host, or no answer at all, can pass on the next attempt.
	if IsAgentRefusal(err, CodeInvalid) {
		return fmt.Errorf("pool %s: remove slot %d on %s: %w", p.cfg.Name, slot, hostID, err)
	}
	return &provider.TransientError{Err: fmt.Errorf("pool %s: remove slot %d on %s: %w", p.cfg.Name, slot, hostID, err)}
}

// Run keeps the pool's hosts matched to demand until ctx ends: it adopts and
// probes hosts, creates them for the minimum and for overflow, and returns
// empty ones when their billing says so.
func (p *Pool) Run(ctx context.Context) error {
	defer p.creations.Wait()
	ticker := time.NewTicker(p.cfg.Tick)
	defer ticker.Stop()
	for {
		p.maintain(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-p.wake:
		}
	}
}

func (p *Pool) maintain(ctx context.Context) {
	p.followTag(ctx)
	if err := p.refreshHosts(ctx, false); err != nil {
		p.log.Warn("could not list pool hosts", "error", err)
	}
	p.probe(ctx)
	p.listSlots(ctx, p.hostsIn(func(h *hostState) bool {
		return h.host.Address != "" && (h.phase == phaseReady || h.phase == phaseDraining)
	}))
	p.replaceStale()
	p.returnHosts(ctx)
	p.grow(ctx)
}

// followTag resolves the followed tag when it is due. Until the first
// resolution succeeds, new hosts are created from the tag itself, as a pool
// that does not follow it would; after one, a failure keeps the last digest.
func (p *Pool) followTag(ctx context.Context) {
	if p.cfg.Follow == nil {
		return
	}
	p.mu.Lock()
	due := !p.cfg.Now().Before(p.imageCheckAt)
	p.mu.Unlock()
	if !due {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, p.cfg.CallTimeout)
	image, err := p.cfg.Follow.Resolve(callCtx)
	cancel()
	if err == nil && (image == p.cfg.Image || !p.cfg.Follow.Related(image)) {
		err = fmt.Errorf("resolved %q, which is not a digest of %s", image, p.cfg.Image)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	if err != nil {
		p.imageCheckAt = now.Add(imageRetry)
		p.imageError = err.Error()
		p.log.Warn("could not resolve the runner image tag; hosts keep their image", "tag", p.cfg.Image, "image", p.image, "retry_after", imageRetry, "error", err)
		return
	}
	p.imageCheckAt = now.Add(p.cfg.ImageRefresh)
	p.imageCheckedAt = now
	p.imageError = ""
	if image == p.image {
		return
	}
	if p.imageResolved {
		p.log.Info("runner image tag moved; replacing hosts on the earlier digest one at a time", "tag", p.cfg.Image, "image", image, "previous", p.image)
	} else {
		p.log.Info("resolved the runner image tag", "tag", p.cfg.Image, "image", image)
	}
	p.image, p.imageResolved = image, true
	p.imageFailed = false
}

// accepts reports whether a host on image may serve slots: the pool's image,
// or with a followed tag, any image of its repository. Callers hold p.mu.
func (p *Pool) accepts(image string) bool {
	return image == p.image || image == p.cfg.Image || (p.cfg.Follow != nil && p.cfg.Follow.Related(image))
}

// stale reports whether a host on image is due for replacement: the followed
// tag has been resolved, to another image. Callers hold p.mu.
func (p *Pool) stale(image string) bool {
	return p.cfg.Follow != nil && p.imageResolved && image != p.image
}

// replaceStale drains one ready host on an earlier image of the followed tag,
// so it takes no new job, is deleted once its jobs end, and the pool creates
// its replacement from the current digest as it would for any drained host.
// It replaces one host at a time: not while another is draining for it, a
// host is on its way, or within replaceInterval of the previous one, and
// after a failed creation or boot from the current image only once a host on
// that image is ready. An idle host goes first.
func (p *Pool) replaceStale() {
	if p.cfg.Follow == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	if !p.imageResolved || now.Before(p.replaceNotBefore) {
		return
	}
	var candidate *hostState
	candidateBusy, current := 0, false
	for _, id := range p.sortedIDs() {
		h := p.hosts[id]
		switch h.phase {
		case phaseCreating, phaseStarting, phaseStopping, phaseDeleting:
			return
		case phaseBooting:
			if h.ownBoot && !h.everReady {
				return
			}
		case phaseDraining:
			if h.drainReason == "image_stale" {
				return
			}
		case phaseReady:
			if !p.stale(h.info.Image) {
				current = true
				continue
			}
			busy := h.occupied() + len(h.reserved)
			if candidate == nil || busy < candidateBusy || (busy == candidateBusy && h.host.CreatedAt.Before(candidate.host.CreatedAt)) {
				candidate, candidateBusy = h, busy
			}
		}
	}
	if candidate == nil || (p.imageFailed && !current) {
		return
	}
	candidate.phase = phaseDraining
	candidate.drainReason = "image_stale"
	p.accrue(now)
	p.replaceNotBefore = now.Add(replaceInterval)
	p.imageReplaced++
	p.log.Info("replacing pool host on an earlier runner image; it takes no new job and is deleted once empty",
		"host", candidate.host.ID, "host_image", candidate.info.Image, "image", p.image, "busy_slots", candidateBusy)
}

func (p *Pool) known() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hostsKnown
}

func (p *Pool) host(id string) (Host, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.hosts[id]
	if !ok || h.phase == phaseCreating {
		return Host{}, false
	}
	return h.host, true
}

func (p *Pool) stoppedHost(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.hosts[id]
	return ok && h.phase == phaseStopped
}

// running reports whether the host's machine may be up and its agent
// reachable.
func (h *hostState) running() bool {
	switch h.phase {
	case phaseCreating, phaseStopped, phaseStarting, phaseStopping:
		return false
	}
	return true
}

func (p *Pool) hostsIn(match func(*hostState) bool) []Host {
	p.mu.Lock()
	defer p.mu.Unlock()
	hosts := make([]Host, 0, len(p.hosts))
	for _, id := range p.sortedIDs() {
		if h := p.hosts[id]; match(h) {
			hosts = append(hosts, h.host)
		}
	}
	return hosts
}

// sortedIDs returns host IDs in a stable order; callers hold p.mu.
func (p *Pool) sortedIDs() []string {
	ids := make([]string, 0, len(p.hosts))
	for id := range p.hosts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (p *Pool) refreshHosts(ctx context.Context, force bool) error {
	p.mu.Lock()
	fresh := p.hostsKnown && p.cfg.Now().Sub(p.hostsListedAt) < p.cfg.HostListInterval
	p.mu.Unlock()
	if fresh && !force {
		return nil
	}
	started := p.cfg.Now()
	listed, err := p.cfg.Source.List(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	seen := make(map[string]struct{}, len(listed))
	for _, host := range listed {
		if host.ID == "" || strings.ContainsAny(host.ID, "/:") {
			p.log.Warn("ignoring pool host with an invalid ID", "host", host.ID)
			continue
		}
		seen[host.ID] = struct{}{}
		if existing, ok := p.hosts[host.ID]; ok {
			existing.host = host
			if !existing.changedAt.After(started) {
				p.reconcile(existing, now)
			}
			continue
		}
		h := &hostState{
			host:      host,
			phase:     phaseBooting,
			slots:     map[int]Slot{},
			reserved:  map[int]string{},
			firstSeen: now,
			ownBoot:   !host.CreatedAt.IsZero() && now.Sub(host.CreatedAt) < p.cfg.BootTimeout,
		}
		if p.stopper != nil && host.State == HostStopped {
			h.phase = phaseStopped
			h.stoppedSince = earliest(host.StateSince, now)
		} else {
			h.runningSince = firstNonZero(host.StateSince, host.CreatedAt, now)
			h.billedBefore = p.billedHours(h.runningSince, now)
		}
		p.hosts[host.ID] = h
		p.log.Info("found pool host", "host", host.ID, "created_at", host.CreatedAt, "phase", h.phase)
	}
	for id, h := range p.hosts {
		// A host created while the listing was in flight is not in it yet.
		if _, ok := seen[id]; ok || h.phase == phaseCreating || h.firstSeen.After(started) {
			continue
		}
		if h.occupied() > 0 {
			p.log.Warn("pool host disappeared from its source with slots in use", "host", id, "slots", h.occupied())
		}
		p.forget(id, now)
	}
	p.accrue(now)
	p.hostsListedAt = now
	p.hostsKnown = true
	return nil
}

// reconcile follows a stop or a start the pool did not make itself: a crash,
// the provider's maintenance, or an operator. Callers hold p.mu.
func (p *Pool) reconcile(h *hostState, now time.Time) {
	if p.stopper == nil {
		return
	}
	stopped := h.host.State == HostStopped
	switch {
	case stopped && (h.phase == phaseBooting || h.phase == phaseReady || h.phase == phaseDraining):
		if h.occupied() > 0 {
			p.log.Warn("pool host stopped with slots in use", "host", h.host.ID, "slots", h.occupied())
		} else {
			p.log.Warn("pool host stopped outside the pool", "host", h.host.ID, "phase", h.phase)
		}
		if h.startedByPool {
			h.startFailures++
			h.notBefore = now.Add(hostRefusalBackoff)
		}
		p.markStopped(h, earliest(h.host.StateSince, now))
	case !stopped && h.phase == phaseStopped:
		p.log.Warn("stopped pool host started outside the pool", "host", h.host.ID, "state", h.host.State)
		h.phase = phaseBooting
		h.bootSince = now
		h.ownBoot = false
		h.everReady = false
		h.lastContact = time.Time{}
		h.runningSince = firstNonZero(h.host.StateSince, now)
		h.stoppedSince = time.Time{}
	}
}

// markStopped records a host as stopped: its slot containers died with it.
// Callers hold p.mu.
func (p *Pool) markStopped(h *hostState, at time.Time) {
	if !h.runningSince.IsZero() {
		h.billed += p.billedHours(h.runningSince, at)
	}
	h.phase = phaseStopped
	h.runningSince = time.Time{}
	h.stoppedSince = at
	h.slots = map[int]Slot{}
	h.lastListed = time.Time{}
	h.emptySince = time.Time{}
	h.startedByPool = false
}

// probe asks every booting host whether it is ready.
func (p *Pool) probe(ctx context.Context) {
	booting := p.hostsIn(func(h *hostState) bool { return h.phase == phaseBooting && h.host.Address != "" })
	results := make([]HostInfo, len(booting))
	failures := make([]error, len(booting))
	p.parallel(len(booting), func(i int) {
		callCtx, cancel := context.WithTimeout(ctx, p.cfg.CallTimeout)
		defer cancel()
		results[i], failures[i] = p.cfg.Agent.Info(callCtx, booting[i])
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	defer p.accrue(now)
	for i, host := range booting {
		h, ok := p.hosts[host.ID]
		if !ok || h.phase != phaseBooting {
			continue
		}
		if failures[i] != nil {
			continue
		}
		info := results[i]
		h.lastContact = now
		h.info = info
		p.slotsSeen = max(p.slotsSeen, info.Slots)
		switch {
		case !p.accepts(info.Image):
			h.phase = phaseDraining
			h.drainReason = "image_mismatch"
			p.log.Warn("pool host runs another runner image; draining it", "host", host.ID, "host_image", info.Image, "image", p.image)
		case info.Ready && info.Slots > 0:
			h.phase = phaseReady
			h.everReady = true
			h.startedByPool = false
			h.startFailures = 0
			p.log.Info("pool host ready", "host", host.ID, "slots", info.Slots, "boot", now.Sub(firstNonZero(h.bootSince, host.CreatedAt, h.firstSeen)).Round(time.Second))
			if p.roomReady != nil {
				// A non-blocking signal, safe under p.mu.
				p.roomReady()
			}
		}
	}
}

func (p *Pool) listSlots(ctx context.Context, hosts []Host) {
	results := make([][]Slot, len(hosts))
	failures := make([]error, len(hosts))
	p.parallel(len(hosts), func(i int) {
		callCtx, cancel := context.WithTimeout(ctx, p.cfg.CallTimeout)
		defer cancel()
		results[i], failures[i] = p.cfg.Agent.List(callCtx, hosts[i])
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	defer p.accrue(now)
	for i, host := range hosts {
		h, ok := p.hosts[host.ID]
		if !ok {
			continue
		}
		if failures[i] != nil {
			if h.everReady && now.Sub(h.warnedAt) > p.cfg.UnreachableGrace {
				h.warnedAt = now
				p.log.Warn("pool host did not answer", "host", host.ID, "error", failures[i])
			}
			continue
		}
		slots := make(map[int]Slot, len(results[i]))
		for _, slot := range results[i] {
			slots[slot.Index] = slot
		}
		// A slot whose start is still in flight is not listed yet; it stays
		// reserved, so the listing cannot hand it out twice.
		h.slots = slots
		h.lastContact = now
		h.lastListed = now
		if len(slots) == 0 && len(h.reserved) == 0 {
			if h.emptySince.IsZero() {
				h.emptySince = now
			}
		} else {
			h.emptySince = time.Time{}
		}
	}
}

func (p *Pool) parallel(count int, call func(int)) {
	if count == 0 {
		return
	}
	limit := make(chan struct{}, maxParallelCalls)
	var wait sync.WaitGroup
	for i := range count {
		wait.Add(1)
		limit <- struct{}{}
		go func() {
			defer wait.Done()
			defer func() { <-limit }()
			call(i)
		}()
	}
	wait.Wait()
}

// reserve picks the slot for a lease and the image to start it with: the
// busiest ready host that still has room, so load concentrates and spare
// hosts empty out and can be returned. A host on the pool's image goes before
// one waiting for its replacement, which then empties out first.
func (p *Pool) reserve(leaseID string) (Host, int, string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	var best *hostState
	bestFree, bestStale := 0, false
	for _, id := range p.sortedIDs() {
		h := p.hosts[id]
		if !p.takesLeases(h, now) {
			continue
		}
		free := h.freeCount()
		if free == 0 {
			continue
		}
		stale := p.stale(h.info.Image)
		if best == nil || (bestStale && !stale) ||
			(stale == bestStale && (free < bestFree || (free == bestFree && h.host.CreatedAt.Before(best.host.CreatedAt)))) {
			best, bestFree, bestStale = h, free, stale
		}
	}
	if best == nil {
		return Host{}, 0, "", false
	}
	slot, _ := best.freeSlot()
	best.reserved[slot] = leaseID
	best.emptySince = time.Time{}
	p.accrue(now)
	return best.host, slot, best.info.Image, true
}

// takesLeases reports whether a host may receive a lease now: ready, not
// backing off after a refusal, and heard from recently. Callers hold p.mu.
func (p *Pool) takesLeases(h *hostState, now time.Time) bool {
	return h.phase == phaseReady && !now.Before(h.notBefore) && !h.lastContact.IsZero() && now.Sub(h.lastContact) <= p.cfg.UnreachableGrace
}

func (p *Pool) release(hostID string, slot int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.hosts[hostID]; ok {
		delete(h.reserved, slot)
	}
	p.accrue(p.cfg.Now())
}

func (p *Pool) commit(hostID string, slot int, started Slot, lease provider.Lease) provider.Worker {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	started.Index = slot
	started.ControllerID = p.cfg.ControllerID
	started.LeaseID = lease.ID
	started.RunnerName = lease.RunnerName
	if started.RunnerID == 0 {
		started.RunnerID = lease.RunnerID
	}
	if started.RunnerScaleSetID == 0 {
		started.RunnerScaleSetID = lease.RunnerScaleSetID
	}
	if started.State == "" {
		started.State = "started"
	}
	if started.CreatedAt.IsZero() {
		started.CreatedAt = now
	}
	if h, ok := p.hosts[hostID]; ok {
		delete(h.reserved, slot)
		h.slots[slot] = started
		h.lastContact = now
		h.emptySince = time.Time{}
	}
	p.launched++
	p.accrue(now)
	return slotWorker(hostID, slot, started, lease)
}

// readLateStart reads a slot back after a start that failed without a clear
// refusal, while the slot is still reserved for the lease. It returns the slot
// once the host lists it running this lease's runner for this controller. A
// slot that holds anything else, or stops, is never adopted, and a host that
// cannot tell within lateStartWait leaves the start failed. An agent that
// answered has finished its start, so one listing tells; a lost answer may
// come before the container, so the slot is read again until the wait ends.
func (p *Pool) readLateStart(ctx context.Context, host Host, slot int, lease provider.Lease, startErr error) (Slot, bool) {
	var refusal *AgentError
	answered := errors.As(startErr, &refusal)
	deadline := time.Now().Add(p.lateStartWait)
	for ctx.Err() == nil {
		callCtx, cancel := context.WithTimeout(ctx, min(p.cfg.CallTimeout, max(time.Until(deadline), time.Millisecond)))
		slots, err := p.cfg.Agent.List(callCtx, host)
		cancel()
		if err == nil {
			for _, listed := range slots {
				if listed.Index != slot {
					continue
				}
				if listed.ControllerID != p.cfg.ControllerID || listed.LeaseID != lease.ID || listed.RunnerName != lease.RunnerName {
					return Slot{}, false
				}
				switch listed.State {
				case "started":
					return listed, true
				case "starting":
				default:
					return Slot{}, false
				}
			}
		}
		if answered || !time.Now().Add(p.lateStartPoll).Before(deadline) {
			return Slot{}, false
		}
		select {
		case <-ctx.Done():
		case <-time.After(p.lateStartPoll):
		}
	}
	return Slot{}, false
}

// holdSuspect keeps a slot whose start is ambiguous taken until the core
// removes the worker or a listing shows the slot's real content.
func (p *Pool) holdSuspect(hostID string, slot int, lease provider.Lease) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.hosts[hostID]; ok {
		delete(h.reserved, slot)
		h.slots[slot] = Slot{
			Index: slot, ControllerID: p.cfg.ControllerID, LeaseID: lease.ID, RunnerName: lease.RunnerName,
			RunnerID: lease.RunnerID, RunnerScaleSetID: lease.RunnerScaleSetID, State: "starting", CreatedAt: p.cfg.Now(),
		}
		h.notBefore = p.cfg.Now().Add(hostRefusalBackoff)
	}
	p.accrue(p.cfg.Now())
}

func (p *Pool) markForeign(hostID string, slot int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.hosts[hostID]; ok {
		if _, known := h.slots[slot]; !known {
			h.slots[slot] = Slot{Index: slot, State: "unknown"}
		}
	}
	p.accrue(p.cfg.Now())
}

func (p *Pool) refuseHost(hostID, reason string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.hosts[hostID]; ok {
		h.notBefore = p.cfg.Now().Add(hostRefusalBackoff)
		if reason == CodeImageMismatch && h.phase == phaseReady {
			h.phase = phaseDraining
			h.drainReason = "image_mismatch"
		}
	}
	p.accrue(p.cfg.Now())
	p.log.Warn("pool host refused a slot; trying another", "host", hostID, "reason", reason, "error", err)
}

func (p *Pool) cleared(hostID string, slot int, leaseID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.hosts[hostID]
	if !ok {
		return
	}
	if current, taken := h.slots[slot]; taken && (current.LeaseID == leaseID || current.LeaseID == "") {
		delete(h.slots, slot)
	}
	if len(h.slots) == 0 && len(h.reserved) == 0 && h.emptySince.IsZero() {
		h.emptySince = p.cfg.Now()
	}
	p.accrue(p.cfg.Now())
}

// noteFull records an overflow for the next growth pass. It returns one tick
// when room is on its way, so the core probes again as soon as the host can
// be ready instead of after its capacity backoff, and zero otherwise.
func (p *Pool) noteFull() time.Duration {
	p.mu.Lock()
	p.demand++
	p.overflowed++
	coming := p.roomComing(p.cfg.Now())
	p.mu.Unlock()
	p.signal()
	if coming {
		return p.cfg.Tick
	}
	return 0
}

// roomComing reports whether a host is being created, started, or booted, or
// whether the next growth pass can start or create one. Callers hold p.mu.
func (p *Pool) roomComing(now time.Time) bool {
	for _, h := range p.hosts {
		if h.phase == phaseCreating || h.phase == phaseStarting || h.phase == phaseBooting {
			return true
		}
	}
	if p.startable(now) != nil {
		return true
	}
	return p.facts.CanCreate && len(p.hosts) < p.cfg.MaxHosts && !now.Before(p.createNotBefore)
}

// signal runs the maintenance loop now instead of at its next tick.
func (p *Pool) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

type returnCandidate struct {
	host     Host
	previous phase
	reason   string
	// stop stops the host, keeping its disk, instead of deleting it.
	stop bool
}

// returnHosts gives empty hosts back to their source when the source's
// billing says keeping them costs more than it saves.
func (p *Pool) returnHosts(ctx context.Context) {
	if !p.facts.CanCreate {
		p.warnStuckStaticHosts()
		return
	}
	candidates := p.returnCandidates()
	for _, candidate := range candidates {
		if candidate.previous != phaseStopped && candidate.reason != "boot_timeout" && candidate.reason != "unreachable" {
			callCtx, cancel := context.WithTimeout(ctx, p.cfg.CallTimeout)
			slots, err := p.cfg.Agent.List(callCtx, candidate.host)
			cancel()
			if err != nil || len(slots) > 0 {
				p.restorePhase(candidate.host.ID, candidate.previous)
				continue
			}
		}
		if candidate.stop {
			p.stopHost(ctx, candidate)
			continue
		}
		if err := p.cfg.Source.Delete(ctx, candidate.host.ID); err != nil {
			p.log.Warn("could not return pool host", "host", candidate.host.ID, "reason", candidate.reason, "error", err)
			p.restorePhase(candidate.host.ID, candidate.previous)
			continue
		}
		p.mu.Lock()
		now := p.cfg.Now()
		if h, ok := p.hosts[candidate.host.ID]; ok {
			p.returnedHours += p.hostBilled(h, now)
			p.retiredBefore += h.billedBefore
			p.forget(candidate.host.ID, now)
		}
		p.returnedHosts++
		p.mu.Unlock()
		p.log.Info("returned pool host", "host", candidate.host.ID, "reason", candidate.reason, "age", p.cfg.Now().Sub(candidate.host.CreatedAt).Round(time.Second))
	}
}

// stopHost stops an idle host. Its disk keeps the slots' images and caches
// for the next start.
func (p *Pool) stopHost(ctx context.Context, candidate returnCandidate) {
	if err := p.stopper.Stop(ctx, candidate.host.ID); err != nil {
		p.log.Warn("could not stop pool host", "host", candidate.host.ID, "error", err)
		p.restorePhase(candidate.host.ID, candidate.previous)
		return
	}
	p.mu.Lock()
	now := p.cfg.Now()
	if h, ok := p.hosts[candidate.host.ID]; ok {
		p.markStopped(h, now)
		h.changedAt = now
	}
	p.accrue(now)
	p.hostStops++
	p.mu.Unlock()
	p.log.Info("stopped idle pool host", "host", candidate.host.ID, "reason", candidate.reason)
}

func (p *Pool) returnCandidates() []returnCandidate {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	defer p.accrue(now)
	base := p.baseHosts()
	candidates := make([]returnCandidate, 0)
	for _, id := range p.sortedIDs() {
		h := p.hosts[id]
		reason := ""
		stop := false
		bootStart := firstNonZero(h.bootSince, h.host.CreatedAt)
		switch {
		case h.phase == phaseBooting && h.ownBoot && !h.everReady && !bootStart.IsZero() && now.Sub(bootStart) > p.cfg.BootTimeout:
			reason = "boot_timeout"
			if h.createdImage != "" && h.createdImage == p.image {
				p.imageFailed = true
			}
		case (h.phase == phaseReady || h.phase == phaseDraining || h.phase == phaseBooting) && now.Sub(latest(h.lastContact, h.firstSeen, h.bootSince)) > p.cfg.MaxLifetime+p.cfg.UnreachableGrace:
			// Every slot container has a deadline within the maximum lifetime,
			// so nothing on a host silent for longer can still be a job.
			reason = "unreachable"
		case h.phase != phaseReady && h.phase != phaseDraining:
			continue
		case h.occupied() > 0 || len(h.reserved) > 0 || h.lastListed.IsZero() || now.Sub(h.lastListed) > 3*p.cfg.Tick:
			continue
		case h.phase == phaseDraining:
			reason = h.drainReason
		case base[id] || p.facts.Billing == BillingOwned:
			continue
		case p.facts.Billing == BillingPerStartedHour:
			if remainingPaid(now.Sub(h.host.CreatedAt)) <= p.cfg.HourMargin {
				reason = "paid_hour_ending"
			}
		case p.facts.Billing == BillingPerSecond:
			idle := p.cfg.IdleReturn
			if p.stopper != nil {
				idle = p.cfg.IdleStop
			}
			if !h.emptySince.IsZero() && now.Sub(h.emptySince) >= idle {
				reason = "idle"
				stop = p.stopper != nil
				// A stopped host on an earlier digest would be deleted anyway.
				if stop && p.stale(h.info.Image) {
					reason, stop = "image_stale", false
				}
			}
		}
		if reason == "" {
			continue
		}
		candidates = append(candidates, returnCandidate{host: h.host, previous: h.phase, reason: reason, stop: stop})
		if stop {
			h.phase = phaseStopping
		} else {
			h.phase = phaseDeleting
		}
	}
	return append(candidates, p.stoppedCandidates(now)...)
}

// stoppedCandidates picks the stopped hosts to delete: at once those that
// cannot serve again, and, beyond the KeepStopped most recently stopped,
// those stopped for DeleteStoppedAfter. Callers hold p.mu.
func (p *Pool) stoppedCandidates(now time.Time) []returnCandidate {
	stopped := make([]*hostState, 0)
	for _, h := range p.hosts {
		if h.phase == phaseStopped {
			stopped = append(stopped, h)
		}
	}
	sort.Slice(stopped, func(i, j int) bool {
		if !stopped[i].stoppedSince.Equal(stopped[j].stoppedSince) {
			return stopped[i].stoppedSince.After(stopped[j].stoppedSince)
		}
		return stopped[i].host.ID < stopped[j].host.ID
	})
	candidates := make([]returnCandidate, 0)
	kept := 0
	for _, h := range stopped {
		reason := p.unusable(h)
		switch {
		case reason != "":
		case kept < p.cfg.KeepStopped:
			kept++
			continue
		case now.Sub(h.stoppedSince) >= p.cfg.DeleteStoppedAfter:
			reason = "stopped_expired"
		default:
			continue
		}
		candidates = append(candidates, returnCandidate{host: h.host, previous: phaseStopped, reason: reason})
		h.phase = phaseDeleting
	}
	return candidates
}

// unusable says why a stopped host must not be started again, or "".
// Callers hold p.mu.
func (p *Pool) unusable(h *hostState) string {
	switch {
	case h.drainReason != "":
		return h.drainReason
	case h.host.Image != "" && !p.accepts(h.host.Image):
		return "image_mismatch"
	case h.host.Image != "" && p.stale(h.host.Image):
		return "image_stale"
	case h.startFailures >= maxStartFailures:
		return "start_failed"
	}
	return ""
}

// baseHosts returns the min_hosts oldest ready hosts, which are kept warm;
// callers hold p.mu.
func (p *Pool) baseHosts() map[string]bool {
	ready := make([]*hostState, 0, len(p.hosts))
	for _, h := range p.hosts {
		if h.phase == phaseReady {
			ready = append(ready, h)
		}
	}
	sort.Slice(ready, func(i, j int) bool {
		if !ready[i].host.CreatedAt.Equal(ready[j].host.CreatedAt) {
			return ready[i].host.CreatedAt.Before(ready[j].host.CreatedAt)
		}
		return ready[i].host.ID < ready[j].host.ID
	})
	base := make(map[string]bool, p.cfg.MinHosts)
	for i := 0; i < len(ready) && i < p.cfg.MinHosts; i++ {
		base[ready[i].host.ID] = true
	}
	return base
}

func (p *Pool) restorePhase(hostID string, previous phase) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.hosts[hostID]; ok && (h.phase == phaseDeleting || h.phase == phaseStopping) {
		h.phase = previous
	}
	p.accrue(p.cfg.Now())
}

func (p *Pool) warnStuckStaticHosts() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	for _, id := range p.sortedIDs() {
		h := p.hosts[id]
		if (h.phase == phaseBooting || h.phase == phaseDraining) && now.Sub(h.warnedAt) > 5*time.Minute && now.Sub(h.firstSeen) > p.cfg.Tick {
			h.warnedAt = now
			p.log.Warn("static pool host is not serving slots", "host", id, "phase", h.phase, "reason", h.drainReason)
		}
	}
}

// grow creates hosts up to min_hosts, and one more when a launch overflowed
// and no host is already on its way, within max_hosts.
func (p *Pool) grow(ctx context.Context) {
	if !p.facts.CanCreate {
		return
	}
	p.mu.Lock()
	now := p.cfg.Now()
	active, pending := 0, 0
	for _, h := range p.hosts {
		switch h.phase {
		case phaseCreating, phaseStarting:
			active++
			pending++
		case phaseBooting:
			active++
			if !h.everReady {
				pending++
			}
		case phaseReady:
			active++
		}
	}
	want := max(0, p.cfg.MinHosts-active)
	if want == 0 && p.demand > 0 && pending == 0 {
		want = 1
	}
	p.demand = 0
	// A stopped host comes back with its images and caches, and starting it
	// adds nothing to max_hosts, so stopped hosts go first.
	for ; want > 0; want-- {
		h := p.startable(now)
		if h == nil {
			break
		}
		h.phase = phaseStarting
		h.changedAt = now
		p.creations.Add(1)
		go p.start(ctx, h.host.ID)
	}
	want = min(want, max(0, p.cfg.MaxHosts-len(p.hosts)))
	if now.Before(p.createNotBefore) {
		want = 0
	}
	for range want {
		p.createSeq++
		placeholder := "creating-" + strconv.Itoa(p.createSeq)
		p.hosts[placeholder] = &hostState{phase: phaseCreating, firstSeen: now, slots: map[int]Slot{}, reserved: map[int]string{}}
		p.creations.Add(1)
		go p.create(ctx, placeholder, p.image)
	}
	p.mu.Unlock()
}

// startable returns the most recently stopped host that may be started, or
// nil; callers hold p.mu.
func (p *Pool) startable(now time.Time) *hostState {
	if p.stopper == nil {
		return nil
	}
	var best *hostState
	for _, id := range p.sortedIDs() {
		h := p.hosts[id]
		if h.phase != phaseStopped || now.Before(h.notBefore) || p.unusable(h) != "" {
			continue
		}
		if best == nil || h.stoppedSince.After(best.stoppedSince) {
			best = h
		}
	}
	return best
}

// start starts a stopped host. A failed start leaves it stopped and backs
// it off; a host that fails to start maxStartFailures times, or is refused
// for capacity, is deleted on the next pass and replaced by a new one.
func (p *Pool) start(ctx context.Context, id string) {
	defer p.creations.Done()
	err := p.stopper.Start(ctx, id)
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.hosts[id]
	if !ok {
		return
	}
	now := p.cfg.Now()
	h.changedAt = now
	defer p.signal()
	defer p.accrue(now)
	if err != nil {
		h.phase = phaseStopped
		h.startFailures++
		var capacity *provider.CapacityError
		if errors.As(err, &capacity) {
			h.startFailures = maxStartFailures
		}
		h.notBefore = now.Add(hostRefusalBackoff)
		// The demand that asked for the start still waits: the next pass
		// starts another stopped host or creates one.
		p.demand++
		p.log.Warn("could not start stopped pool host", "host", id, "failures", h.startFailures, "error", err)
		return
	}
	p.log.Info("started stopped pool host", "host", id, "stopped_for", now.Sub(h.stoppedSince).Round(time.Second))
	h.phase = phaseBooting
	h.bootSince = now
	h.runningSince = now
	h.ownBoot = true
	h.everReady = false
	h.startedByPool = true
	h.lastContact = time.Time{}
	h.stoppedSince = time.Time{}
	h.notBefore = time.Time{}
	p.hostStarts++
	// The next pass reads the host again, with its address after the start.
	p.hostsListedAt = time.Time{}
}

func (p *Pool) create(ctx context.Context, placeholder, image string) {
	defer p.creations.Done()
	host, err := p.cfg.Source.Create(ctx, image)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	p.forget(placeholder, now)
	defer p.accrue(now)
	if err != nil {
		// Creations started in the same pass fail together; only the first
		// failure of a round lengthens the backoff.
		if !now.Before(p.createNotBefore) {
			p.createBackoff = min(max(p.createBackoff*2, initialCreateBackoff), maximumCreateBackoff)
			p.createNotBefore = now.Add(p.createBackoff)
		}
		if image == p.image {
			p.imageFailed = true
		}
		p.log.Warn("could not create pool host", "image", image, "error", err, "retry_after", p.createNotBefore.Sub(now))
		return
	}
	p.createBackoff = 0
	if host.ID == "" || strings.ContainsAny(host.ID, "/:") {
		p.log.Error("host source returned an invalid host ID", "host", host.ID)
		return
	}
	if existing, ok := p.hosts[host.ID]; ok {
		existing.host = host
		existing.ownBoot = true
		existing.createdImage = image
		return
	}
	p.hosts[host.ID] = &hostState{
		host: host, phase: phaseBooting, firstSeen: now, ownBoot: true, createdImage: image,
		slots: map[int]Slot{}, reserved: map[int]string{}, runningSince: firstNonZero(host.CreatedAt, now),
	}
	p.log.Info("created pool host", "host", host.ID, "image", image)
}

// hostBilled returns the host hours this process counts for h: its earlier
// running periods and the current one. Callers hold p.mu.
func (p *Pool) hostBilled(h *hostState, now time.Time) float64 {
	billed := h.billed
	if !h.runningSince.IsZero() {
		billed += p.billedHours(h.runningSince, now)
	}
	return billed
}

func (p *Pool) billedHours(createdAt, now time.Time) float64 {
	if createdAt.IsZero() || !now.After(createdAt) {
		return 0
	}
	age := now.Sub(createdAt)
	switch p.facts.Billing {
	case BillingPerStartedHour:
		return math.Ceil(age.Hours())
	case BillingPerSecond:
		return age.Hours()
	default:
		return 0
	}
}

// Status is the pool's share of the fleet status.
type Status struct {
	Name          string  `json:"name"`
	Billing       Billing `json:"billing"`
	HourlyPrice   float64 `json:"hourly_price"`
	Currency      string  `json:"currency,omitempty"`
	MinHosts      int     `json:"min_hosts"`
	MaxHosts      int     `json:"max_hosts"`
	SlotsTotal    int     `json:"slots_total"`
	SlotsBusy     int     `json:"slots_busy"`
	Launched      uint64  `json:"launched"`
	Overflowed    uint64  `json:"overflowed"`
	ReturnedHosts int     `json:"returned_hosts"`
	// Speculated counts the launches, within Launched, of a second runner
	// for a job already waiting on a later tier's booting worker.
	Speculated uint64 `json:"speculated"`
	// HostStops and HostStarts count idle hosts stopped and stopped hosts
	// started again, for sources that stop hosts.
	HostStops  int `json:"host_stops,omitempty"`
	HostStarts int `json:"host_starts,omitempty"`
	// Image is the image new hosts are created from. With a followed tag,
	// Follows names the tag, ImageCheckedAt is its last resolution and
	// ImageError the failure of the attempts since; ImageReplacements counts
	// the hosts drained because they ran an earlier digest.
	Image             string    `json:"image"`
	Follows           string    `json:"follows,omitempty"`
	ImageCheckedAt    time.Time `json:"image_checked_at,omitzero"`
	ImageError        string    `json:"image_error,omitempty"`
	ImageReplacements int       `json:"image_replacements,omitempty"`
	// BilledHours counts host hours this process has seen billed: returned
	// hosts in full, live hosts up to now, stopped hosts while they ran. Cost
	// is BilledHours * HourlyPrice; a stopped host's storage is not in it.
	BilledHours float64      `json:"billed_hours"`
	Cost        float64      `json:"cost"`
	Hosts       []HostStatus `json:"hosts"`

	// MeasuredSince is when this process started measuring the pool: the
	// usage below, MeasuredCost and EphemeralEquivalent cover the time
	// since, and start again at zero when the controller restarts.
	MeasuredSince time.Time `json:"measured_since"`
	// Usage adds up every host's usage, returned hosts included.
	Usage
	// MeasuredCost is Cost without the hours hosts had been billed before
	// this process first saw them, so it covers the same time as Usage.
	MeasuredCost float64 `json:"measured_cost"`
	// EphemeralEquivalent prices BusySlotSeconds as ephemeral workers, when
	// the ephemeral tier knows its own hourly price.
	EphemeralEquivalent *EphemeralEquivalent `json:"ephemeral_equivalent,omitempty"`
}

// HostStatus describes one host.
type HostStatus struct {
	ID        string    `json:"id"`
	Phase     string    `json:"phase"`
	Slots     int       `json:"slots"`
	Busy      int       `json:"busy"`
	Reachable bool      `json:"reachable"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	StoppedAt time.Time `json:"stopped_at,omitzero"`
	// Image is the runner image the host reported, or for a host that has
	// not answered, the one its source prepared it for. Stale marks an
	// earlier digest of a followed tag: the host is due for replacement.
	Image string `json:"image,omitempty"`
	Stale bool   `json:"stale,omitempty"`
	Usage
}

// Status reports the pool's hosts, slots, and billed hours.
func (p *Pool) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	p.accrue(now)
	status := Status{
		Name: p.cfg.Name, Billing: p.facts.Billing, HourlyPrice: p.facts.HourlyPrice, Currency: p.facts.Currency,
		MinHosts: p.cfg.MinHosts, MaxHosts: p.cfg.MaxHosts, Launched: p.launched, Overflowed: p.overflowed,
		ReturnedHosts: p.returnedHosts, HostStops: p.hostStops, HostStarts: p.hostStarts, Speculated: p.speculated,
		BilledHours: p.returnedHours, Hosts: make([]HostStatus, 0, len(p.hosts)),
		Image: p.image, ImageReplacements: p.imageReplaced, MeasuredSince: p.measuredSince,
	}
	if p.cfg.Follow != nil {
		status.Follows, status.ImageCheckedAt, status.ImageError = p.cfg.Image, p.imageCheckedAt, p.imageError
	}
	used, billedBefore := p.retired, p.retiredBefore
	for _, id := range p.sortedIDs() {
		h := p.hosts[id]
		busy := h.occupied() + len(h.reserved)
		image := firstNonEmpty(h.info.Image, h.host.Image)
		if !h.running() {
			image = firstNonEmpty(h.host.Image, h.info.Image)
		}
		hostUsage := p.usageOf(h)
		host := HostStatus{
			ID: id, Phase: string(h.phase), Slots: h.info.Slots, Busy: busy, CreatedAt: h.host.CreatedAt,
			Reachable: h.running() && !h.lastContact.IsZero() && now.Sub(h.lastContact) <= p.cfg.UnreachableGrace,
			StoppedAt: h.stoppedSince, Image: image, Stale: image != "" && p.accepts(image) && p.stale(image),
			Usage: hostUsage.report(),
		}
		if h.phase == phaseReady {
			status.SlotsTotal += h.info.Slots
			status.SlotsBusy += min(busy, h.info.Slots)
		}
		status.BilledHours += p.hostBilled(h, now)
		billedBefore += h.billedBefore
		used = used.add(hostUsage)
		status.Hosts = append(status.Hosts, host)
	}
	status.Usage = used.report()
	status.Cost = priceHours(status.BilledHours, p.facts.HourlyPrice)
	status.MeasuredCost = priceHours(max(0, status.BilledHours-billedBefore), p.facts.HourlyPrice)
	return status
}

func remainingPaid(age time.Duration) time.Duration {
	if age < 0 {
		return time.Hour
	}
	return time.Hour - age%time.Hour
}

// latest returns the latest of values.
func latest(values ...time.Time) time.Time {
	var result time.Time
	for _, value := range values {
		if value.After(result) {
			result = value
		}
	}
	return result
}

// earliest returns value when it is set and before limit, otherwise limit.
func earliest(value, limit time.Time) time.Time {
	if !value.IsZero() && value.Before(limit) {
		return value
	}
	return limit
}

func firstNonZero(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value
		}
	}
	return time.Time{}
}

func refusalCode(err error) string {
	var refusal *AgentError
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	return ""
}

func sortedSlots(slots map[int]Slot) []int {
	indexes := make([]int, 0, len(slots))
	for index := range slots {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	return indexes
}

// WorkerID names a slot worker: host, slot, and lease. The lease keeps a late
// destroy from removing the next job's container in the same slot.
func WorkerID(hostID string, slot int, leaseID string) string {
	return hostID + "/" + strconv.Itoa(slot) + "/" + leaseID
}

func parseWorkerID(id string) (string, int, string, error) {
	parts := strings.Split(id, "/")
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return "", 0, "", fmt.Errorf("invalid pool worker id %q", id)
	}
	slot, err := strconv.Atoi(parts[1])
	if err != nil || slot < 0 {
		return "", 0, "", fmt.Errorf("invalid pool worker id %q", id)
	}
	return parts[0], slot, parts[2], nil
}

func slotWorker(hostID string, index int, slot Slot, lease provider.Lease) provider.Worker {
	worker := provider.Worker{
		ID:               WorkerID(hostID, index, firstNonEmpty(slot.LeaseID, lease.ID)),
		LeaseID:          firstNonEmpty(slot.LeaseID, lease.ID),
		RunnerName:       firstNonEmpty(slot.RunnerName, lease.RunnerName),
		RunnerID:         slot.RunnerID,
		RunnerScaleSetID: slot.RunnerScaleSetID,
		State:            slot.State,
		CreatedAt:        slot.CreatedAt,
	}
	if worker.RunnerID == 0 {
		worker.RunnerID = lease.RunnerID
	}
	if worker.RunnerScaleSetID == 0 {
		worker.RunnerScaleSetID = lease.RunnerScaleSetID
	}
	return worker
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

var _ provider.Compute = (*Pool)(nil)
