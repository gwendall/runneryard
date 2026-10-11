package fly

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gwendall/runneryard/hostagent"
	"github.com/gwendall/runneryard/pool"
	"github.com/gwendall/runneryard/provider"
	"github.com/gwendall/runneryard/provider/retry"
)

const (
	poolHostKey     = "runneryard-pool-host"
	poolNameKey     = "runneryard-pool"
	poolImageKey    = "runneryard-pool-image"
	hostIDPrefix    = "fly-"
	hostNamePrefix  = "ry-"
	dataPath        = "/data"
	defaultPoll     = 2 * time.Second
	defaultStateFor = 3 * time.Minute
	// An unattached volume of this pool older than orphanVolumeAge belongs to
	// no Machine: a create that failed halfway, or a delete whose volume step
	// failed. Younger ones may still be on their way to a Machine.
	orphanVolumeAge    = 30 * time.Minute
	volumeSweepEvery   = 10 * time.Minute
	minVolumeGB        = 10
	maxVolumeGB        = 500
	stoppedMachine     = "stopped"
	startedMachine     = "started"
	destroyedMachine   = "destroyed"
	destroyingMachine  = "destroying"
	hostBootEntrypoint = "/usr/local/bin/runneryard"
)

var safeMachineID = regexp.MustCompile(`^[a-z0-9]{1,64}$`)

// HostSourceConfig configures a pool whose hosts are Fly Machines. Each host
// is one Machine in the pool's own app with one volume for Docker's data, so
// a stopped host keeps the slots' images and caches and costs only that
// volume and its root filesystem.
type HostSourceConfig struct {
	APIToken   string
	APIBaseURL string
	// App is the pool's own Fly app: never the workers' app nor the
	// controller's, so a token for it reaches nothing else.
	App          string
	Region       string
	CPUKind      string
	CPUs         int
	MemoryMB     int
	VolumeGB     int
	ControllerID string
	Pool         string
	// HourlyPrice is the USD price of one running host hour; zero takes the
	// listed rate of Region.
	HourlyPrice float64

	RunnerImage   string
	Slots         int
	SlotCPUs      float64
	SlotMemoryMB  int
	Caches        []string
	MinFreeDiskGB int
	DockerDNS     string

	// HostKey derives a host's SSH key from its Machine name.
	HostKey func(name string) ([]byte, string, error)
	// AuthorizedKey is the controller's public key.
	AuthorizedKey string

	HTTPClient *http.Client
	Retry      retry.Policy
	// PollInterval and StateTimeout pace and bound the wait for a stop, a
	// start, or a deletion to complete.
	PollInterval time.Duration
	StateTimeout time.Duration
	Logger       *slog.Logger
	Now          func() time.Time
}

// HostSource implements pool.Source and pool.Stopper with Fly Machines.
type HostSource struct {
	cfg          HostSourceConfig
	api          *Adapter
	price        float64
	imageTag     string
	volumePrefix string
	dockerDNS    string

	mu      sync.Mutex
	sweptAt time.Time
}

// NewHostSource validates the configuration.
func NewHostSource(cfg HostSourceConfig) (*HostSource, error) {
	if !pool.ValidName(cfg.Pool) {
		return nil, fmt.Errorf("fly pool name %q must be lowercase letters, digits, and hyphens", cfg.Pool)
	}
	if cfg.APIToken == "" || cfg.App == "" || cfg.Region == "" || cfg.ControllerID == "" {
		return nil, fmt.Errorf("fly pool %s requires a token, an app, a region, and a controller ID", cfg.Pool)
	}
	if cfg.HostKey == nil || strings.TrimSpace(cfg.AuthorizedKey) == "" {
		return nil, fmt.Errorf("fly pool %s requires the controller SSH key", cfg.Pool)
	}
	if cfg.CPUKind != "shared" && cfg.CPUKind != "performance" {
		return nil, fmt.Errorf("fly pool %s CPU kind must be shared or performance", cfg.Pool)
	}
	if cfg.CPUs < 1 || cfg.MemoryMB < 1024 || cfg.MemoryMB%256 != 0 {
		return nil, fmt.Errorf("fly pool %s hosts need at least 1 CPU and 1024 MB of memory in steps of 256 MB", cfg.Pool)
	}
	if cfg.VolumeGB < minVolumeGB || cfg.VolumeGB > maxVolumeGB {
		return nil, fmt.Errorf("fly pool %s volume_gb must be between %d and %d", cfg.Pool, minVolumeGB, maxVolumeGB)
	}
	if err := hostConfig(cfg).Validate(); err != nil {
		return nil, fmt.Errorf("fly pool %s: %w", cfg.Pool, err)
	}
	dockerDNS, err := normalizeDockerDNS(cfg.DockerDNS)
	if err != nil {
		return nil, err
	}
	price := cfg.HourlyPrice
	if price < 0 {
		return nil, fmt.Errorf("fly pool %s hourly price cannot be negative", cfg.Pool)
	}
	if price == 0 {
		listed, ok := HourlyPriceUSD(cfg.CPUKind, cfg.CPUs, cfg.MemoryMB, cfg.Region)
		if !ok {
			return nil, fmt.Errorf("fly pool %s: no listed price multiplier for region %q; set hourly_price", cfg.Pool, cfg.Region)
		}
		price = listed
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = "https://api.machines.dev"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPoll
	}
	if cfg.StateTimeout <= 0 {
		cfg.StateTimeout = defaultStateFor
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	owner := sha256.Sum256([]byte(cfg.ControllerID + "|" + cfg.Pool))
	return &HostSource{
		cfg:          cfg,
		api:          &Adapter{baseURL: strings.TrimRight(cfg.APIBaseURL, "/"), token: cfg.APIToken, app: cfg.App, httpClient: cfg.HTTPClient, retryer: retry.New(cfg.Retry)},
		price:        price,
		imageTag:     imageTag(cfg.RunnerImage),
		volumePrefix: "ry_" + hex.EncodeToString(owner[:4]) + "_",
		dockerDNS:    dockerDNS,
	}, nil
}

func hostConfig(cfg HostSourceConfig) hostagent.Config {
	return hostagent.Config{
		Slots: cfg.Slots, Image: cfg.RunnerImage, SlotCPUs: cfg.SlotCPUs, SlotMemoryMB: cfg.SlotMemoryMB,
		Caches: cfg.Caches, MinFreeDiskGB: cfg.MinFreeDiskGB, DockerRoot: dataPath + "/docker",
	}
}

// imageTag fits the runner image in Machine metadata, whose values are short
// plain strings.
func imageTag(image string) string {
	sum := sha256.Sum256([]byte(image))
	return hex.EncodeToString(sum[:8])
}

// Facts: Fly bills a running Machine per second; a stopped one only for its
// volume and root filesystem.
func (s *HostSource) Facts() pool.Facts {
	return pool.Facts{Billing: pool.BillingPerSecond, HourlyPrice: s.price, Currency: "USD", CanCreate: true}
}

type poolMachine struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	State     string    `json:"state"`
	Region    string    `json:"region"`
	PrivateIP string    `json:"private_ip"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Config is decoded without its files: they hold the host's private key.
	Config struct {
		Metadata map[string]string `json:"metadata"`
		Mounts   []machineMount    `json:"mounts"`
	} `json:"config"`
}

type machineMount struct {
	Volume string `json:"volume"`
	Path   string `json:"path"`
	Name   string `json:"name,omitempty"`
}

type machineFile struct {
	GuestPath string `json:"guest_path"`
	RawValue  string `json:"raw_value"`
	Mode      uint32 `json:"mode"`
}

type poolProcess struct {
	Entrypoint       []string          `json:"entrypoint"`
	User             string            `json:"user"`
	Env              map[string]string `json:"env,omitempty"`
	IgnoreAppSecrets bool              `json:"ignore_app_secrets"`
}

type poolMachineConfig struct {
	Image       string            `json:"image"`
	Guest       guestConfig       `json:"guest"`
	Processes   []poolProcess     `json:"processes"`
	Metadata    map[string]string `json:"metadata"`
	Mounts      []machineMount    `json:"mounts"`
	Files       []machineFile     `json:"files"`
	AutoDestroy bool              `json:"auto_destroy"`
	Restart     restartPolicy     `json:"restart"`
	StopConfig  stopConfig        `json:"stop_config"`
}

type createPoolMachineRequest struct {
	Name   string            `json:"name"`
	Region string            `json:"region"`
	Config poolMachineConfig `json:"config"`
}

type flyVolume struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	State             string    `json:"state"`
	AttachedMachineID *string   `json:"attached_machine_id"`
	CreatedAt         time.Time `json:"created_at"`
}

func (v flyVolume) attached() bool { return v.AttachedMachineID != nil && *v.AttachedMachineID != "" }

type createVolumeRequest struct {
	Name              string      `json:"name"`
	Region            string      `json:"region"`
	SizeGB            int         `json:"size_gb"`
	Encrypted         bool        `json:"encrypted"`
	AutoBackupEnabled bool        `json:"auto_backup_enabled"`
	Compute           guestConfig `json:"compute"`
}

// owns accepts only Machines carrying all three pool labels, and never a
// worker Machine.
func (s *HostSource) owns(item poolMachine) bool {
	metadata := item.Config.Metadata
	return metadata[poolHostKey] == "true" && metadata[controllerIDKey] == s.cfg.ControllerID &&
		metadata[poolNameKey] == s.cfg.Pool && metadata[managedByKey] == ""
}

// List returns this pool's Machines, running and stopped.
func (s *HostSource) List(ctx context.Context) ([]pool.Host, error) {
	machines, err := s.machines(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Fly pool hosts: %w", err)
	}
	hosts := make([]pool.Host, 0, len(machines))
	mounted := make(map[string]bool)
	for _, item := range machines {
		for _, mount := range item.Config.Mounts {
			mounted[mount.Volume] = true
		}
		if !s.owns(item) || item.State == destroyingMachine || item.State == destroyedMachine {
			continue
		}
		host, err := s.toHost(item)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, host)
	}
	s.sweepVolumes(ctx, mounted)
	return hosts, nil
}

func (s *HostSource) toHost(item poolMachine) (pool.Host, error) {
	_, public, err := s.cfg.HostKey(item.Name)
	if err != nil {
		return pool.Host{}, fmt.Errorf("derive host key of %s: %w", item.Name, err)
	}
	host := pool.Host{
		ID: hostIDPrefix + item.ID, HostKey: public, CreatedAt: item.CreatedAt, StateSince: item.UpdatedAt,
	}
	switch item.State {
	case startedMachine:
		host.State = "running"
	case stoppedMachine, "suspended", "failed":
		host.State = pool.HostStopped
	default:
		host.State = item.State
	}
	if item.PrivateIP != "" {
		host.Address = net.JoinHostPort(item.PrivateIP, hostagent.BootPort)
	}
	switch tag := item.Config.Metadata[poolImageKey]; {
	case tag == s.imageTag:
		host.Image = s.cfg.RunnerImage
	case tag != "":
		// Another image: any value unequal to the runner image tells the
		// pool not to start this host again.
		host.Image = "other-image@" + tag
	}
	return host, nil
}

// Create makes the host's volume, then its Machine, which boots the runner
// image as a pool host. A failure leaves nothing behind: a Machine the
// request may have created is found by name, a volume without a Machine is
// deleted.
func (s *HostSource) Create(ctx context.Context) (pool.Host, error) {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return pool.Host{}, fmt.Errorf("name pool host: %w", err)
	}
	name := hostNamePrefix + s.cfg.Pool + "-" + hex.EncodeToString(suffix)
	privateKey, _, err := s.cfg.HostKey(name)
	if err != nil {
		return pool.Host{}, fmt.Errorf("derive host key of %s: %w", name, err)
	}
	hostJSON, err := json.MarshalIndent(hostConfig(s.cfg), "", "  ")
	if err != nil {
		return pool.Host{}, err
	}
	guest := guestConfig{CPUs: s.cfg.CPUs, MemoryMB: s.cfg.MemoryMB, CPUKind: s.cfg.CPUKind}
	volume, err := s.createVolume(ctx, s.volumePrefix+hex.EncodeToString(suffix), guest)
	if err != nil {
		return pool.Host{}, fmt.Errorf("create volume of Fly pool host %s: %w", name, err)
	}
	payload := createPoolMachineRequest{
		Name:   name,
		Region: s.cfg.Region,
		Config: poolMachineConfig{
			Image: s.cfg.RunnerImage,
			Guest: guest,
			Processes: []poolProcess{{
				Entrypoint:       []string{hostBootEntrypoint, "host", "boot"},
				User:             "root",
				Env:              map[string]string{"RUNNERYARD_DOCKER_DNS": s.dockerDNS},
				IgnoreAppSecrets: true,
			}},
			Metadata: map[string]string{
				poolHostKey:     "true",
				controllerIDKey: s.cfg.ControllerID,
				poolNameKey:     s.cfg.Pool,
				poolImageKey:    s.imageTag,
			},
			Mounts: []machineMount{{Volume: volume.ID, Path: dataPath}},
			Files: []machineFile{
				{GuestPath: hostagent.ConfigPath, RawValue: base64.StdEncoding.EncodeToString(append(hostJSON, '\n')), Mode: 0o644},
				{GuestPath: hostagent.BootHostKeyPath, RawValue: base64.StdEncoding.EncodeToString(privateKey), Mode: 0o600},
				{GuestPath: hostagent.BootAuthorizedKeyPath, RawValue: base64.StdEncoding.EncodeToString([]byte(strings.TrimSpace(s.cfg.AuthorizedKey) + "\n")), Mode: 0o644},
			},
			AutoDestroy: false,
			Restart:     restartPolicy{Policy: "no"},
			StopConfig:  stopConfig{Signal: "SIGTERM", Timeout: flyDuration(30 * time.Second)},
		},
	}
	if err := s.api.retryer.Wait(ctx); err != nil {
		return pool.Host{}, errors.Join(err, s.dropVolume(ctx, volume.ID))
	}
	var created poolMachine
	err = s.api.doJSON(ctx, http.MethodPost, s.api.machinesURL(), payload, &created)
	if err != nil && provider.IsTransient(err) {
		// The request may have created the Machine before failing.
		if found, lookupErr := s.findByName(context.WithoutCancel(ctx), name); lookupErr != nil {
			return pool.Host{}, errors.Join(fmt.Errorf("create Fly pool host %s: %w", name, err), lookupErr)
		} else if found != nil {
			created, err = *found, nil
		}
	}
	if err != nil {
		return pool.Host{}, errors.Join(fmt.Errorf("create Fly pool host %s: %w", name, err), s.dropVolume(ctx, volume.ID))
	}
	if created.ID == "" || !s.owns(created) {
		return pool.Host{}, fmt.Errorf("create Fly pool host %s: the response is not a Machine of this pool", name)
	}
	return s.toHost(created)
}

// Delete destroys the Machine after confirming it is one of this pool's
// hosts, then its volume. A Machine that no longer exists is already deleted.
func (s *HostSource) Delete(ctx context.Context, hostID string) error {
	item, err := s.ownedMachine(ctx, hostID)
	if errors.Is(err, errMachineGone) {
		return nil
	}
	if err != nil {
		return err
	}
	requestURL := s.api.machinesURL() + "/" + url.PathEscape(item.ID) + "?force=true"
	err = s.api.retryer.Do(ctx, func(ctx context.Context) error {
		err := s.api.doJSON(ctx, http.MethodDelete, requestURL, nil, nil)
		if notFound(err) {
			return nil
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("delete Fly pool host %s: %w", hostID, err)
	}
	if _, err := s.waitState(ctx, item.ID, destroyedMachine); err != nil {
		return fmt.Errorf("delete Fly pool host %s: %w", hostID, err)
	}
	for _, mount := range item.Config.Mounts {
		if err := s.dropVolume(ctx, mount.Volume); err != nil {
			// The Machine is gone; the volume sweep deletes the volume later.
			s.cfg.Logger.Warn("could not delete the volume of a deleted Fly pool host", "host", hostID, "volume", mount.Volume, "error", err)
		}
	}
	return nil
}

// Stop stops one of this pool's Machines and waits until it is stopped.
func (s *HostSource) Stop(ctx context.Context, hostID string) error {
	return s.transition(ctx, hostID, "stop", stoppedMachine)
}

// Start starts one of this pool's stopped Machines and waits until it runs.
func (s *HostSource) Start(ctx context.Context, hostID string) error {
	return s.transition(ctx, hostID, "start", startedMachine)
}

func (s *HostSource) transition(ctx context.Context, hostID, verb, want string) error {
	item, err := s.ownedMachine(ctx, hostID)
	if err != nil {
		return fmt.Errorf("%s Fly pool host %s: %w", verb, hostID, err)
	}
	// A Machine already on its way to want needs no second request.
	if item.State != want && item.State != map[string]string{stoppedMachine: "stopping", startedMachine: "starting"}[want] {
		requestURL := s.api.machinesURL() + "/" + url.PathEscape(item.ID) + "/" + verb
		if err := s.api.retryer.Do(ctx, func(ctx context.Context) error {
			return s.api.doJSON(ctx, http.MethodPost, requestURL, nil, nil)
		}); err != nil {
			return fmt.Errorf("%s Fly pool host %s: %w", verb, hostID, err)
		}
	}
	if _, err := s.waitState(ctx, item.ID, want); err != nil {
		return fmt.Errorf("%s Fly pool host %s: %w", verb, hostID, err)
	}
	return nil
}

var errMachineGone = errors.New("machine no longer exists")

// ownedMachine reads a host's Machine and refuses any Machine that is not
// one of this pool's hosts.
func (s *HostSource) ownedMachine(ctx context.Context, hostID string) (poolMachine, error) {
	id, ok := strings.CutPrefix(hostID, hostIDPrefix)
	if !ok || !safeMachineID.MatchString(id) {
		return poolMachine{}, fmt.Errorf("invalid Fly pool host id %q", hostID)
	}
	item, err := s.machine(ctx, id)
	if err != nil {
		return poolMachine{}, err
	}
	if !s.owns(item) {
		return poolMachine{}, fmt.Errorf("refusing to touch Fly Machine %s: it is not a host of pool %s", id, s.cfg.Pool)
	}
	return item, nil
}

func (s *HostSource) machine(ctx context.Context, id string) (poolMachine, error) {
	var item poolMachine
	err := s.api.retryer.Do(ctx, func(ctx context.Context) error {
		item = poolMachine{}
		return s.api.doJSON(ctx, http.MethodGet, s.api.machinesURL()+"/"+url.PathEscape(id), nil, &item)
	})
	if notFound(err) {
		return poolMachine{}, errMachineGone
	}
	if err != nil {
		return poolMachine{}, fmt.Errorf("look up Fly Machine %s: %w", id, err)
	}
	return item, nil
}

func (s *HostSource) machines(ctx context.Context) ([]poolMachine, error) {
	var machines []poolMachine
	err := s.api.retryer.Do(ctx, func(ctx context.Context) error {
		machines = nil
		return s.api.doJSON(ctx, http.MethodGet, s.api.machinesURL(), nil, &machines)
	})
	return machines, err
}

func (s *HostSource) findByName(ctx context.Context, name string) (*poolMachine, error) {
	machines, err := s.machines(ctx)
	if err != nil {
		return nil, fmt.Errorf("confirm Fly pool host absence: %w", err)
	}
	for _, item := range machines {
		if item.Name == name && s.owns(item) {
			return &item, nil
		}
	}
	return nil, nil
}

// waitState polls the Machine until it reaches want; a Machine that is gone
// counts as destroyed.
func (s *HostSource) waitState(ctx context.Context, id, want string) (poolMachine, error) {
	waitCtx, cancel := context.WithTimeout(ctx, s.cfg.StateTimeout)
	defer cancel()
	last := ""
	for {
		item, err := s.machine(waitCtx, id)
		switch {
		case errors.Is(err, errMachineGone):
			if want == destroyedMachine {
				return poolMachine{}, nil
			}
			return poolMachine{}, err
		case err == nil && item.State == want:
			return item, nil
		case err == nil:
			last = item.State
		}
		if err := sleepContext(waitCtx, s.cfg.PollInterval); err != nil {
			return poolMachine{}, fmt.Errorf("machine %s still %q after %s waiting for %q", id, last, s.cfg.StateTimeout, want)
		}
	}
}

func (s *HostSource) volumesURL() string {
	return s.api.baseURL + "/v1/apps/" + url.PathEscape(s.cfg.App) + "/volumes"
}

func (s *HostSource) createVolume(ctx context.Context, name string, guest guestConfig) (flyVolume, error) {
	payload := createVolumeRequest{
		Name: name, Region: s.cfg.Region, SizeGB: s.cfg.VolumeGB, Encrypted: true,
		AutoBackupEnabled: false, Compute: guest,
	}
	if err := s.api.retryer.Wait(ctx); err != nil {
		return flyVolume{}, err
	}
	var created flyVolume
	err := s.api.doJSON(ctx, http.MethodPost, s.volumesURL(), payload, &created)
	if err != nil && provider.IsTransient(err) {
		// A volume the failed request created is found by its unique name;
		// one not found yet is left to the sweep.
		volumes, listErr := s.volumes(context.WithoutCancel(ctx))
		if listErr != nil {
			return flyVolume{}, errors.Join(err, listErr)
		}
		for _, volume := range volumes {
			if volume.Name == name {
				return volume, nil
			}
		}
	}
	if err != nil {
		return flyVolume{}, err
	}
	if created.ID == "" || created.Name != name {
		return flyVolume{}, errors.New("the response is not the requested volume")
	}
	return created, nil
}

func (s *HostSource) volumes(ctx context.Context) ([]flyVolume, error) {
	var volumes []flyVolume
	err := s.api.retryer.Do(ctx, func(ctx context.Context) error {
		volumes = nil
		return s.api.doJSON(ctx, http.MethodGet, s.volumesURL(), nil, &volumes)
	})
	return volumes, err
}

// dropVolume deletes one of this pool's volumes, waiting for a Machine just
// destroyed to release it.
func (s *HostSource) dropVolume(ctx context.Context, id string) error {
	ctx = context.WithoutCancel(ctx)
	waitCtx, cancel := context.WithTimeout(ctx, s.cfg.StateTimeout)
	defer cancel()
	volumeURL := s.volumesURL() + "/" + url.PathEscape(id)
	for {
		var volume flyVolume
		err := s.api.retryer.Do(waitCtx, func(ctx context.Context) error {
			volume = flyVolume{}
			return s.api.doJSON(ctx, http.MethodGet, volumeURL, nil, &volume)
		})
		if notFound(err) || (err == nil && (volume.State == destroyedMachine || volume.State == "pending_destroy")) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("look up volume %s: %w", id, err)
		}
		if !strings.HasPrefix(volume.Name, s.volumePrefix) {
			return fmt.Errorf("refusing to delete volume %s: it is not a volume of pool %s", id, s.cfg.Pool)
		}
		if !volume.attached() {
			err := s.api.retryer.Do(waitCtx, func(ctx context.Context) error {
				err := s.api.doJSON(ctx, http.MethodDelete, volumeURL, nil, nil)
				if notFound(err) {
					return nil
				}
				return err
			})
			if err == nil {
				return nil
			}
			var status *retry.StatusError
			if !errors.As(err, &status) || status.Status >= 500 || status.Status == http.StatusUnauthorized || status.Status == http.StatusForbidden {
				return fmt.Errorf("delete volume %s: %w", id, err)
			}
			// Fly can refuse while the destroyed Machine still holds it.
		}
		if err := sleepContext(waitCtx, s.cfg.PollInterval); err != nil {
			return fmt.Errorf("volume %s is still attached after %s", id, s.cfg.StateTimeout)
		}
	}
}

// sweepVolumes deletes this pool's volumes that no Machine uses: named with
// the pool's prefix, attached to nothing, mounted by no listed Machine, and
// older than orphanVolumeAge. It runs at most every volumeSweepEvery.
func (s *HostSource) sweepVolumes(ctx context.Context, mounted map[string]bool) {
	now := s.cfg.Now()
	s.mu.Lock()
	if !s.sweptAt.IsZero() && now.Sub(s.sweptAt) < volumeSweepEvery {
		s.mu.Unlock()
		return
	}
	s.sweptAt = now
	s.mu.Unlock()
	volumes, err := s.volumes(ctx)
	if err != nil {
		s.cfg.Logger.Warn("could not list Fly pool volumes", "error", err)
		return
	}
	for _, volume := range volumes {
		if !strings.HasPrefix(volume.Name, s.volumePrefix) || mounted[volume.ID] || volume.attached() ||
			slices.Contains([]string{destroyingMachine, destroyedMachine, "pending_destroy"}, volume.State) ||
			volume.CreatedAt.IsZero() || now.Sub(volume.CreatedAt) < orphanVolumeAge {
			continue
		}
		if err := s.dropVolume(ctx, volume.ID); err != nil {
			s.cfg.Logger.Warn("could not delete an orphaned Fly pool volume", "volume", volume.ID, "error", err)
			continue
		}
		s.cfg.Logger.Info("deleted an orphaned Fly pool volume", "volume", volume.ID, "name", volume.Name)
	}
}

func notFound(err error) bool {
	var status *retry.StatusError
	return errors.As(err, &status) && status.Status == http.StatusNotFound
}

var (
	_ pool.Source  = (*HostSource)(nil)
	_ pool.Stopper = (*HostSource)(nil)
)
