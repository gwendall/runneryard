package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gwendall/runneryard/hostagent"
	"github.com/gwendall/runneryard/pool"
	"github.com/gwendall/runneryard/provider"
	flyprovider "github.com/gwendall/runneryard/providers/fly"
	hetznerprovider "github.com/gwendall/runneryard/providers/hetzner"
)

const (
	ephemeralTier       = "ephemeral"
	maximumPoolsFile    = 256 << 10
	maximumPoolSlots    = 64
	maximumPoolHosts    = 100
	defaultPoolDiskGB   = 20
	poolTrustPrivate    = "private"
	poolSourceHetzner   = "hetzner"
	poolSourceStatic    = "static"
	poolSourceFly       = "fly"
	poolFlyTokenEnv     = "RUNNER_POOL_FLY_API_TOKEN" // #nosec G101 -- the variable name, not a credential
	defaultFlyVolumeGB  = 50
	defaultFlyIdleStop  = 5 * time.Minute
	defaultKeepStopped  = 1
	defaultDeleteAfter  = 6 * time.Hour
	poolSSHKeyFileEnv   = "RUNNER_POOL_SSH_KEY_FILE"
	poolsFileEnv        = "RUNNER_POOLS_FILE"
	defaultHetznerImage = "docker-ce"
)

// poolsFile is the operator's RUNNER_POOLS_FILE. Without it the controller
// starts one ephemeral worker per job, as before.
type poolsFile struct {
	// Trust must be "private": a pool runs jobs one after another on shared
	// machines, in privileged containers, so every repository the fleet
	// serves must be one trust domain.
	Trust      string       `yaml:"trust"`
	SSHKeyFile string       `yaml:"ssh_key_file"`
	Placement  []string     `yaml:"placement"`
	Pools      []poolConfig `yaml:"pools"`
}

type poolConfig struct {
	Name          string   `yaml:"name"`
	Source        string   `yaml:"source"`
	RunnerImage   string   `yaml:"runner_image"`
	Slots         int      `yaml:"slots"`
	MinHosts      int      `yaml:"min_hosts"`
	MaxHosts      int      `yaml:"max_hosts"`
	HourlyPrice   float64  `yaml:"hourly_price"`
	SlotCPUs      float64  `yaml:"slot_cpus"`
	SlotMemoryMB  int      `yaml:"slot_memory_mb"`
	MinFreeDiskGB *int     `yaml:"min_free_disk_gb"`
	Caches        []string `yaml:"caches"`

	// Hetzner
	ServerType  string `yaml:"server_type"`
	Location    string `yaml:"location"`
	ServerImage string `yaml:"server_image"`
	FirewallID  int64  `yaml:"firewall_id"`
	NetworkID   int64  `yaml:"network_id"`

	// Static
	Billing  string            `yaml:"billing"`
	Currency string            `yaml:"currency"`
	Hosts    []pool.StaticHost `yaml:"hosts"`

	// Fly
	FlyApp             string        `yaml:"fly_app"`
	Region             string        `yaml:"region"`
	CPUKind            string        `yaml:"cpu_kind"`
	CPUs               int           `yaml:"cpus"`
	MemoryMB           int           `yaml:"memory_mb"`
	VolumeGB           int           `yaml:"volume_gb"`
	IdleStopAfter      time.Duration `yaml:"idle_stop_after"`
	KeepStopped        *int          `yaml:"keep_stopped"`
	DeleteStoppedAfter time.Duration `yaml:"delete_stopped_after"`
}

// flyFields lists the Fly keys a pool sets, so another source can refuse
// them.
func (p *poolConfig) flyFields() bool {
	return p.FlyApp != "" || p.Region != "" || p.CPUKind != "" || p.CPUs != 0 || p.MemoryMB != 0 || p.VolumeGB != 0 ||
		p.IdleStopAfter != 0 || p.KeepStopped != nil || p.DeleteStoppedAfter != 0
}

// loadPoolsFile reads and validates the pools file. The controller key is
// read here too, so a broken key fails at startup instead of at first launch.
func loadPoolsFile(path string) (*poolsFile, error) {
	file, err := os.Open(path) // #nosec G304 -- operator-selected configuration path
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", poolsFileEnv, err)
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maximumPoolsFile+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", poolsFileEnv, err)
	}
	if len(contents) > maximumPoolsFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", poolsFileEnv, maximumPoolsFile)
	}
	var parsed poolsFile
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	if err := decoder.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parse %s: %w", poolsFileEnv, err)
	}
	if keyFile := strings.TrimSpace(os.Getenv(poolSSHKeyFileEnv)); keyFile != "" {
		parsed.SSHKeyFile = keyFile
	}
	return &parsed, nil
}

func (f *poolsFile) validate(cfg appConfig) error {
	if f.Trust != poolTrustPrivate {
		return fmt.Errorf(`%s must declare trust: private; pool hosts run one job after another in privileged containers, so they only serve private repositories that trust each other`, poolsFileEnv)
	}
	if len(f.Pools) == 0 {
		return fmt.Errorf("%s declares no pools", poolsFileEnv)
	}
	if strings.TrimSpace(f.SSHKeyFile) == "" {
		return fmt.Errorf("%s requires ssh_key_file or %s: the controller's ed25519 key toward its hosts", poolsFileEnv, poolSSHKeyFileEnv)
	}
	names := map[string]bool{}
	for i := range f.Pools {
		p := &f.Pools[i]
		if !pool.ValidName(p.Name) || p.Name == ephemeralTier {
			return fmt.Errorf("pool name %q must be lowercase letters, digits, and hyphens, and not %q", p.Name, ephemeralTier)
		}
		if names[p.Name] {
			return fmt.Errorf("pool %s is declared twice", p.Name)
		}
		names[p.Name] = true
		if p.RunnerImage == "" {
			p.RunnerImage = cfg.RunnerImage
		}
		if p.RunnerImage == "" {
			return fmt.Errorf("pool %s requires runner_image or RUNNER_IMAGE", p.Name)
		}
		if p.MinFreeDiskGB == nil {
			disk := defaultPoolDiskGB
			p.MinFreeDiskGB = &disk
		}
		if len(p.Caches) == 0 {
			p.Caches = append([]string(nil), hostagent.DefaultCaches...)
		}
		check := hostagent.Config{
			Slots: max(p.Slots, 1), Image: p.RunnerImage, SlotCPUs: p.SlotCPUs, SlotMemoryMB: p.SlotMemoryMB,
			Caches: p.Caches, MinFreeDiskGB: *p.MinFreeDiskGB,
		}
		if err := check.Validate(); err != nil {
			return fmt.Errorf("pool %s: %w", p.Name, err)
		}
		switch p.Source {
		case poolSourceHetzner:
			if err := p.validateHetzner(cfg); err != nil {
				return err
			}
		case poolSourceFly:
			if err := p.validateFly(cfg); err != nil {
				return err
			}
		case poolSourceStatic:
			if p.flyFields() {
				return fmt.Errorf("static pool %s: fly_app, region, cpu_kind, cpus, memory_mb, volume_gb, and the stop settings only apply to Fly pools", p.Name)
			}
			if len(p.Hosts) == 0 {
				return fmt.Errorf("static pool %s lists no hosts", p.Name)
			}
			if p.MinHosts != 0 || p.MaxHosts != 0 {
				return fmt.Errorf("static pool %s cannot create hosts; remove min_hosts and max_hosts", p.Name)
			}
			if p.Slots != 0 {
				return fmt.Errorf("static pool %s: each host states its slots in its own host install", p.Name)
			}
			if _, err := pool.NewStaticSource(pool.Billing(p.Billing), p.HourlyPrice, p.Currency, p.Hosts); err != nil {
				return fmt.Errorf("static pool %s: %w", p.Name, err)
			}
		default:
			return fmt.Errorf("pool %s source must be %q, %q, or %q", p.Name, poolSourceFly, poolSourceHetzner, poolSourceStatic)
		}
	}
	ephemeral := cfg.ComputeProvider != computeProviderNone
	if len(f.Placement) == 0 {
		for _, p := range f.Pools {
			f.Placement = append(f.Placement, p.Name)
		}
		if ephemeral {
			f.Placement = append(f.Placement, ephemeralTier)
		}
	}
	seen := map[string]bool{}
	for _, name := range f.Placement {
		if seen[name] {
			return fmt.Errorf("placement lists %s twice", name)
		}
		seen[name] = true
		if name == ephemeralTier {
			if !ephemeral {
				return fmt.Errorf("placement lists %s but COMPUTE_PROVIDER is none", ephemeralTier)
			}
			continue
		}
		if !names[name] {
			return fmt.Errorf("placement lists %s, which is not a declared pool", name)
		}
	}
	return nil
}

func (p *poolConfig) validateHetzner(cfg appConfig) error {
	if cfg.HetznerAPIToken == "" {
		return fmt.Errorf("hetzner pool %s requires HCLOUD_TOKEN", p.Name)
	}
	if p.Location == "" {
		p.Location = cfg.HetznerLocation
	}
	if p.ServerImage == "" {
		p.ServerImage = defaultHetznerImage
	}
	if p.NetworkID < 0 {
		return fmt.Errorf("hetzner pool %s network_id must be a network ID or omitted", p.Name)
	}
	if p.ServerType == "" || p.Location == "" || p.FirewallID < 1 {
		return fmt.Errorf("hetzner pool %s requires server_type, location, and firewall_id (a firewall that admits SSH from the controller only)", p.Name)
	}
	if p.FirewallID == cfg.HetznerFirewallID {
		return fmt.Errorf("hetzner pool %s must use its own firewall: worker firewalls admit no SSH, pool hosts must admit the controller", p.Name)
	}
	if p.Slots < 1 || p.Slots > maximumPoolSlots {
		return fmt.Errorf("hetzner pool %s slots must be between 1 and %d", p.Name, maximumPoolSlots)
	}
	if p.MinHosts < 0 || p.MaxHosts < 1 || p.MinHosts > p.MaxHosts || p.MaxHosts > maximumPoolHosts {
		return fmt.Errorf("hetzner pool %s hosts must satisfy 0 <= min_hosts <= max_hosts <= %d and max_hosts >= 1", p.Name, maximumPoolHosts)
	}
	if p.Billing != "" || p.Currency != "" || len(p.Hosts) > 0 {
		return fmt.Errorf("hetzner pool %s: billing, currency, and hosts only apply to static pools", p.Name)
	}
	if p.flyFields() {
		return fmt.Errorf("hetzner pool %s: fly_app, region, cpu_kind, cpus, memory_mb, volume_gb, and the stop settings only apply to Fly pools", p.Name)
	}
	return nil
}

// poolFlyToken is the token of the Fly pools' app.
func (c appConfig) poolFlyToken() string {
	if c.PoolFlyAPIToken != "" {
		return c.PoolFlyAPIToken
	}
	return c.FlyAPIToken
}

func (p *poolConfig) validateFly(cfg appConfig) error {
	if cfg.poolFlyToken() == "" {
		return fmt.Errorf("fly pool %s requires %s or FLY_API_TOKEN", p.Name, poolFlyTokenEnv)
	}
	if p.FlyApp == "" {
		return fmt.Errorf("fly pool %s requires fly_app: an app of its own for the pool's Machines", p.Name)
	}
	if p.FlyApp == cfg.FlyWorkerApp || p.FlyApp == cfg.ControllerFlyApp {
		return fmt.Errorf("fly pool %s must use its own fly_app, not the worker or controller app", p.Name)
	}
	if p.Region == "" {
		p.Region = cfg.FlyRegion
	}
	if p.CPUKind == "" {
		p.CPUKind = "performance"
	}
	if p.VolumeGB == 0 {
		p.VolumeGB = defaultFlyVolumeGB
	}
	if p.IdleStopAfter == 0 {
		p.IdleStopAfter = defaultFlyIdleStop
	}
	if p.KeepStopped == nil {
		keep := min(defaultKeepStopped, p.MaxHosts)
		p.KeepStopped = &keep
	}
	if p.DeleteStoppedAfter == 0 {
		p.DeleteStoppedAfter = defaultDeleteAfter
	}
	if p.Region == "" || p.CPUs < 1 || p.MemoryMB < 1 {
		return fmt.Errorf("fly pool %s requires region (or RUNNER_FLY_REGION), cpus, and memory_mb", p.Name)
	}
	if p.Slots < 1 || p.Slots > maximumPoolSlots {
		return fmt.Errorf("fly pool %s slots must be between 1 and %d", p.Name, maximumPoolSlots)
	}
	if p.SlotMemoryMB > 0 && p.Slots*p.SlotMemoryMB > p.MemoryMB {
		return fmt.Errorf("fly pool %s: %d slots of %d MB do not fit in memory_mb %d", p.Name, p.Slots, p.SlotMemoryMB, p.MemoryMB)
	}
	if p.MinHosts < 0 || p.MaxHosts < 1 || p.MinHosts > p.MaxHosts || p.MaxHosts > maximumPoolHosts {
		return fmt.Errorf("fly pool %s hosts must satisfy 0 <= min_hosts <= max_hosts <= %d and max_hosts >= 1", p.Name, maximumPoolHosts)
	}
	if p.IdleStopAfter < time.Minute || p.DeleteStoppedAfter < time.Minute {
		return fmt.Errorf("fly pool %s idle_stop_after and delete_stopped_after must be at least one minute", p.Name)
	}
	if *p.KeepStopped < 0 || *p.KeepStopped > p.MaxHosts {
		return fmt.Errorf("fly pool %s keep_stopped must be between 0 and max_hosts", p.Name)
	}
	if p.ServerType != "" || p.Location != "" || p.ServerImage != "" || p.FirewallID != 0 || p.NetworkID != 0 {
		return fmt.Errorf("fly pool %s: server_type, location, server_image, firewall_id, and network_id only apply to Hetzner pools", p.Name)
	}
	if p.Billing != "" || p.Currency != "" || len(p.Hosts) > 0 {
		return fmt.Errorf("fly pool %s: billing, currency, and hosts only apply to static pools", p.Name)
	}
	// The source checks the rest: CPU kind, memory steps, volume size, and
	// price. The controller key is read later, at startup.
	check := p.flySourceConfig(cfg)
	check.HostKey = func(string) ([]byte, string, error) { return nil, "", nil }
	check.AuthorizedKey = "read at startup"
	if _, err := flyprovider.NewHostSource(check); err != nil {
		return err
	}
	return nil
}

func (p poolConfig) flySourceConfig(cfg appConfig) flyprovider.HostSourceConfig {
	return flyprovider.HostSourceConfig{
		APIToken: cfg.poolFlyToken(), APIBaseURL: cfg.FlyAPIBaseURL, App: p.FlyApp, Region: p.Region,
		CPUKind: p.CPUKind, CPUs: p.CPUs, MemoryMB: p.MemoryMB, VolumeGB: p.VolumeGB,
		ControllerID: cfg.ControllerID, Pool: p.Name, HourlyPrice: p.HourlyPrice,
		RunnerImage: p.RunnerImage, Slots: p.Slots, SlotCPUs: p.SlotCPUs, SlotMemoryMB: p.SlotMemoryMB,
		Caches: p.Caches, MinFreeDiskGB: *p.MinFreeDiskGB, DockerDNS: cfg.RunnerDockerDNS, Retry: cfg.retryPolicy(),
	}
}

// withPools places workers on the configured pools, in placement order, with
// the ephemeral provider as one more tier. A configured tier left out of the
// order still drains: its workers stay inventoried until they are gone.
func (c appConfig) withPools(ephemeral provider.Compute, logger *slog.Logger) (provider.Compute, error) {
	f := c.Pools
	if logger == nil {
		logger = slog.Default()
	}
	contents, err := os.ReadFile(f.SSHKeyFile) // #nosec G304 -- operator-selected key path
	if err != nil {
		return nil, fmt.Errorf("read pool SSH key: %w", err)
	}
	key, err := pool.ParseControllerKey(contents)
	if err != nil {
		return nil, err
	}
	agent, err := pool.NewSSHAgent(pool.SSHAgentConfig{Signer: key.Signer()})
	if err != nil {
		return nil, err
	}
	computes := map[string]provider.Compute{}
	if ephemeral != nil {
		computes[ephemeralTier] = ephemeral
	}
	for _, p := range f.Pools {
		source, err := c.poolSource(p, key, logger)
		if err != nil {
			return nil, err
		}
		if p.Source == poolSourceFly && os.Getenv("FLY_PRIVATE_IP") == "" {
			logger.Warn("Fly pool hosts listen on Fly's private network only; run the controller as a Fly Machine in the same organization", "pool", p.Name)
		}
		poolCfg := pool.Config{
			Name: p.Name, ControllerID: c.ControllerID, Image: p.RunnerImage,
			MinHosts: p.MinHosts, MaxHosts: p.MaxHosts, Source: source, Agent: agent,
			MaxLifetime: c.RunnerMaxLifetime, Logger: logger.WithGroup("pool"),
		}
		if p.Source == poolSourceFly {
			poolCfg.IdleStop, poolCfg.KeepStopped, poolCfg.DeleteStoppedAfter = p.IdleStopAfter, *p.KeepStopped, p.DeleteStoppedAfter
		}
		created, err := pool.New(poolCfg)
		if err != nil {
			return nil, err
		}
		computes[p.Name] = created
	}
	tiers := make([]pool.Tier, 0, len(computes))
	for _, name := range f.Placement {
		tiers = append(tiers, pool.Tier{Name: name, Compute: computes[name]})
	}
	names := make([]string, 0, len(computes))
	for name := range computes {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if !slices.Contains(f.Placement, name) {
			logger.Warn("tier is not in the placement order; draining it", "tier", name)
			tiers = append(tiers, pool.Tier{Name: name, Compute: computes[name], Drain: true})
		}
	}
	return pool.NewPlacement(tiers...)
}

func (c appConfig) poolSource(p poolConfig, key *pool.ControllerKey, logger *slog.Logger) (pool.Source, error) {
	switch p.Source {
	case poolSourceFly:
		cfg := p.flySourceConfig(c)
		cfg.HostKey, cfg.AuthorizedKey = key.HostKey, key.AuthorizedKey()
		cfg.Logger = logger.With("pool", p.Name)
		return flyprovider.NewHostSource(cfg)
	case poolSourceHetzner:
		return hetznerprovider.NewHostSource(hetznerprovider.HostSourceConfig{
			APIToken: c.HetznerAPIToken, APIBaseURL: c.HetznerAPIBaseURL, Location: p.Location,
			ServerType: p.ServerType, ServerImage: p.ServerImage, ControllerID: c.ControllerID, Pool: p.Name,
			FirewallID: p.FirewallID, NetworkID: p.NetworkID, HourlyPrice: p.HourlyPrice,
			RunnerImage: p.RunnerImage, Slots: p.Slots, SlotCPUs: p.SlotCPUs, SlotMemoryMB: p.SlotMemoryMB,
			Caches: p.Caches, MinFreeDiskGB: *p.MinFreeDiskGB, HostKey: key.HostKey,
			AuthorizedKey: key.AuthorizedKey(), Retry: c.retryPolicy(),
		})
	case poolSourceStatic:
		return pool.NewStaticSource(pool.Billing(p.Billing), p.HourlyPrice, p.Currency, p.Hosts)
	default:
		return nil, errors.New("unknown pool source " + p.Source)
	}
}
