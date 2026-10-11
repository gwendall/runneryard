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

	"gopkg.in/yaml.v3"

	"github.com/gwendall/runneryard/hostagent"
	"github.com/gwendall/runneryard/pool"
	"github.com/gwendall/runneryard/provider"
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
		case poolSourceStatic:
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
			return fmt.Errorf("pool %s source must be %q or %q", p.Name, poolSourceHetzner, poolSourceStatic)
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
	return nil
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
		source, err := c.poolSource(p, key)
		if err != nil {
			return nil, err
		}
		created, err := pool.New(pool.Config{
			Name: p.Name, ControllerID: c.ControllerID, Image: p.RunnerImage,
			MinHosts: p.MinHosts, MaxHosts: p.MaxHosts, Source: source, Agent: agent,
			MaxLifetime: c.RunnerMaxLifetime, Logger: logger.WithGroup("pool"),
		})
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

func (c appConfig) poolSource(p poolConfig, key *pool.ControllerKey) (pool.Source, error) {
	switch p.Source {
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
