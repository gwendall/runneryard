package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/gwendall/runneryard/hostagent"
	"github.com/gwendall/runneryard/pool"
	"github.com/gwendall/runneryard/provider"
)

// writePoolKey writes a fresh ed25519 controller key in OpenSSH format.
func writePoolKey(t *testing.T) string {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "controller")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pool_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// staticHostKey returns a host public key in authorized_keys format.
func staticHostKey(t *testing.T) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

// loadPoolsYAML loads contents as RUNNER_POOLS_FILE, without the key file
// override unless the test sets it.
func loadPoolsYAML(t *testing.T, contents string) (*poolsFile, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pools.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return loadPoolsFile(path)
}

func poolsAppConfig() appConfig {
	return appConfig{
		ComputeProvider: "hetzner", ControllerID: "test-controller", RunnerImage: "ghcr.io/gwendall/runneryard:0.5.0",
		HetznerAPIToken: "hcloud-token", HetznerAPIBaseURL: "http://127.0.0.1:9", HetznerLocation: "fsn1",
		HetznerFirewallID: 42, RunnerMaxLifetime: 2 * time.Hour,
		FlyAPIToken: "fly-token", FlyAPIBaseURL: "http://127.0.0.1:9", FlyWorkerApp: "ci-workers",
		ControllerFlyApp: "ci-controller", FlyRegion: "cdg",
	}
}

// poolsYAML is a pools file with the given top-level lines before the pools.
func poolsYAML(top string, pools ...string) string {
	return top + "pools:\n" + strings.Join(pools, "")
}

const privateTop = "trust: private\nssh_key_file: /keys/pool_ed25519\n"

// hetznerPool is a valid Hetzner pool entry; an override "key: -" drops the
// key, any other "key: value" replaces it.
func hetznerPool(name string, overrides ...string) string {
	return poolEntry(map[string]string{
		"name": name, "source": "hetzner", "server_type": "cx33", "firewall_id": "77", "slots": "4", "max_hosts": "3",
	}, overrides...)
}

// flyPool is a valid Fly pool entry, overridden like hetznerPool.
func flyPool(name string, overrides ...string) string {
	return poolEntry(map[string]string{
		"name": name, "source": "fly", "fly_app": "ci-pool", "cpus": "8", "memory_mb": "16384",
		"slots": "4", "slot_memory_mb": "3584", "min_hosts": "1", "max_hosts": "2",
	}, overrides...)
}

func poolEntry(fields map[string]string, overrides ...string) string {
	for _, override := range overrides {
		key, value, _ := strings.Cut(override, ": ")
		if value == "-" {
			delete(fields, key)
			continue
		}
		fields[key] = value
	}
	lines := make([]string, 0, len(fields))
	for key, value := range fields {
		lines = append(lines, key+": "+value)
	}
	sort.Strings(lines)
	return "  - " + strings.Join(lines, "\n    ") + "\n"
}

func staticPool(name, hostKey string, extra ...string) string {
	lines := append([]string{
		"name: " + name, "source: static",
		"hosts:", "  - name: " + name + "-1", "    address: 192.0.2.10", "    host_key: " + hostKey,
	}, extra...)
	return "  - " + strings.Join(lines, "\n    ") + "\n"
}

func TestPoolsFileRejectsUnknownFieldsAndOversizedFiles(t *testing.T) {
	t.Setenv(poolSSHKeyFileEnv, "")
	hostKey := staticHostKey(t)
	for name, contents := range map[string]string{
		"top-level typo":   poolsYAML(privateTop+"placment: [fast]\n", hetznerPool("fast")),
		"pool typo":        poolsYAML(privateTop, hetznerPool("fast", "slot: 4")),
		"static host typo": poolsYAML(privateTop, staticPool("lab", hostKey, "  - name: lab-2", "    hostkey: "+hostKey)),
		"not a mapping":    "- trust: private\n",
		"oversized":        privateTop + "# " + strings.Repeat("x", maximumPoolsFile) + "\n" + "pools:\n" + hetznerPool("fast"),
	} {
		t.Run(name, func(t *testing.T) {
			if parsed, err := loadPoolsYAML(t, contents); err == nil {
				t.Fatalf("expected refusal, parsed %#v", parsed)
			}
		})
	}
	if _, err := loadPoolsFile(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("expected a missing pools file to fail")
	}
}

func TestPoolsFileSSHKeyFileEnvironmentOverride(t *testing.T) {
	for name, test := range map[string]struct {
		top  string
		env  string
		want string
	}{
		"environment only":         {top: "trust: private\n", env: "/run/secrets/pool_ed25519", want: "/run/secrets/pool_ed25519"},
		"environment wins":         {top: privateTop, env: "/run/secrets/pool_ed25519", want: "/run/secrets/pool_ed25519"},
		"blank environment":        {top: privateTop, env: "  ", want: "/keys/pool_ed25519"},
		"neither file nor env var": {top: "trust: private\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(poolSSHKeyFileEnv, test.env)
			parsed, err := loadPoolsYAML(t, poolsYAML(test.top, hetznerPool("fast")))
			if err != nil {
				t.Fatal(err)
			}
			if parsed.SSHKeyFile != test.want {
				t.Fatalf("ssh_key_file = %q, want %q", parsed.SSHKeyFile, test.want)
			}
			err = parsed.validate(poolsAppConfig())
			if test.want == "" {
				if err == nil || !strings.Contains(err.Error(), poolSSHKeyFileEnv) {
					t.Fatalf("err = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPoolsFileValidateRefusals(t *testing.T) {
	t.Setenv(poolSSHKeyFileEnv, "")
	hostKey := staticHostKey(t)
	for name, test := range map[string]struct {
		contents string
		mutate   func(*appConfig)
		want     string
	}{
		"trust missing":            {contents: poolsYAML("ssh_key_file: /keys/pool_ed25519\n", hetznerPool("fast")), want: "trust: private"},
		"trust public":             {contents: poolsYAML("trust: public\nssh_key_file: /keys/pool_ed25519\n", hetznerPool("fast")), want: "trust: private"},
		"trust capitalised":        {contents: poolsYAML("trust: Private\nssh_key_file: /keys/pool_ed25519\n", hetznerPool("fast")), want: "trust: private"},
		"no pools":                 {contents: privateTop + "pools: []\n", want: "declares no pools"},
		"ssh_key_file missing":     {contents: poolsYAML("trust: private\n", hetznerPool("fast")), want: "ssh_key_file"},
		"duplicate pool":           {contents: poolsYAML(privateTop, hetznerPool("fast"), hetznerPool("fast")), want: "declared twice"},
		"pool named ephemeral":     {contents: poolsYAML(privateTop, hetznerPool("ephemeral")), want: `not "ephemeral"`},
		"pool name not a label":    {contents: poolsYAML(privateTop, hetznerPool("Fast_Pool")), want: "lowercase letters"},
		"unknown source":           {contents: poolsYAML(privateTop, hetznerPool("fast", "source: aws")), want: "source must be"},
		"missing source":           {contents: poolsYAML(privateTop, hetznerPool("fast", "source: -")), want: "source must be"},
		"no runner image":          {contents: poolsYAML(privateTop, hetznerPool("fast")), mutate: func(c *appConfig) { c.RunnerImage = "" }, want: "runner_image"},
		"invalid cache path":       {contents: poolsYAML(privateTop, hetznerPool("fast", "caches: [/etc/shadow]")), want: "cache path"},
		"no HCLOUD_TOKEN":          {contents: poolsYAML(privateTop, hetznerPool("fast")), mutate: func(c *appConfig) { c.HetznerAPIToken = "" }, want: "HCLOUD_TOKEN"},
		"no firewall":              {contents: poolsYAML(privateTop, hetznerPool("fast", "firewall_id: -")), want: "firewall_id"},
		"negative firewall":        {contents: poolsYAML(privateTop, hetznerPool("fast", "firewall_id: -5")), want: "firewall_id"},
		"negative network":         {contents: poolsYAML(privateTop, hetznerPool("fast", "network_id: -3")), want: "network_id"},
		"no server type":           {contents: poolsYAML(privateTop, hetznerPool("fast", "server_type: -")), want: "server_type"},
		"no location anywhere":     {contents: poolsYAML(privateTop, hetznerPool("fast")), mutate: func(c *appConfig) { c.HetznerLocation = "" }, want: "location"},
		"worker firewall reused":   {contents: poolsYAML(privateTop, hetznerPool("fast", "firewall_id: 42")), want: "its own firewall"},
		"zero slots":               {contents: poolsYAML(privateTop, hetznerPool("fast", "slots: -")), want: "slots must be between 1 and 64"},
		"too many slots":           {contents: poolsYAML(privateTop, hetznerPool("fast", "slots: 65")), want: "slots must be between"},
		"min above max":            {contents: poolsYAML(privateTop, hetznerPool("fast", "min_hosts: 4", "max_hosts: 3")), want: "min_hosts <= max_hosts"},
		"negative min":             {contents: poolsYAML(privateTop, hetznerPool("fast", "min_hosts: -1")), want: "min_hosts <= max_hosts"},
		"no max":                   {contents: poolsYAML(privateTop, hetznerPool("fast", "max_hosts: -")), want: "max_hosts >= 1"},
		"max above the cap":        {contents: poolsYAML(privateTop, hetznerPool("fast", "max_hosts: 101")), want: "max_hosts <= 100"},
		"hetzner with static keys": {contents: poolsYAML(privateTop, hetznerPool("fast", "billing: owned")), want: "only apply to static pools"},
		"static min_hosts":         {contents: poolsYAML(privateTop, staticPool("lab", hostKey, "min_hosts: 1")), want: "remove min_hosts and max_hosts"},
		"static max_hosts":         {contents: poolsYAML(privateTop, staticPool("lab", hostKey, "max_hosts: 2")), want: "remove min_hosts and max_hosts"},
		"static slots":             {contents: poolsYAML(privateTop, staticPool("lab", hostKey, "slots: 4")), want: "its own host install"},
		"static bad host key":      {contents: poolsYAML(privateTop, staticPool("lab", "ssh-ed25519 not-base64")), want: "SSH host public key"},
		"static no host key":       {contents: poolsYAML(privateTop, staticPool("lab", `""`)), want: "SSH host public key"},
		"static unknown billing":   {contents: poolsYAML(privateTop, staticPool("lab", hostKey, "billing: monthly")), want: "unknown billing"},
		"static no hosts":          {contents: poolsYAML(privateTop, "  - name: lab\n    source: static\n"), want: "lists no hosts"},
		"placement unknown pool":   {contents: poolsYAML(privateTop+"placement: [fast, slow]\n", hetznerPool("fast")), want: "slow, which is not a declared pool"},
		"placement listed twice":   {contents: poolsYAML(privateTop+"placement: [fast, fast]\n", hetznerPool("fast")), want: "fast twice"},
		"fly no token": {
			contents: poolsYAML(privateTop, flyPool("fly")),
			mutate:   func(c *appConfig) { c.FlyAPIToken = "" },
			want:     poolFlyTokenEnv,
		},
		"fly no app":                {contents: poolsYAML(privateTop, flyPool("fly", "fly_app: -")), want: "requires fly_app"},
		"fly worker app reused":     {contents: poolsYAML(privateTop, flyPool("fly", "fly_app: ci-workers")), want: "its own fly_app"},
		"fly controller app reused": {contents: poolsYAML(privateTop, flyPool("fly", "fly_app: ci-controller")), want: "its own fly_app"},
		"fly no region anywhere": {
			contents: poolsYAML(privateTop, flyPool("fly")),
			mutate:   func(c *appConfig) { c.FlyRegion = "" },
			want:     "requires region",
		},
		"fly no cpus":              {contents: poolsYAML(privateTop, flyPool("fly", "cpus: -")), want: "cpus, and memory_mb"},
		"fly memory step":          {contents: poolsYAML(privateTop, flyPool("fly", "memory_mb: 16500")), want: "steps of 256 MB"},
		"fly slots overflow":       {contents: poolsYAML(privateTop, flyPool("fly", "slots: 5")), want: "do not fit in memory_mb"},
		"fly no room for the host": {contents: poolsYAML(privateTop, flyPool("fly", "slot_memory_mb: 4096")), want: "1024 MB for the host"},
		"fly default share small":  {contents: poolsYAML(privateTop, flyPool("fly", "memory_mb: 2048", "slot_memory_mb: -")), want: "at least 512 MB each"},
		"fly zero slots":           {contents: poolsYAML(privateTop, flyPool("fly", "slots: -")), want: "slots must be between"},
		"fly min above max":        {contents: poolsYAML(privateTop, flyPool("fly", "min_hosts: 3")), want: "min_hosts <= max_hosts"},
		"fly keep above max":       {contents: poolsYAML(privateTop, flyPool("fly", "keep_stopped: 3")), want: "keep_stopped"},
		"fly short idle stop":      {contents: poolsYAML(privateTop, flyPool("fly", "idle_stop_after: 30s")), want: "at least one minute"},
		"fly short stopped delete": {contents: poolsYAML(privateTop, flyPool("fly", "delete_stopped_after: 10s")), want: "at least one minute"},
		"fly cpu kind":             {contents: poolsYAML(privateTop, flyPool("fly", "cpu_kind: gpu")), want: "shared or performance"},
		"fly small volume":         {contents: poolsYAML(privateTop, flyPool("fly", "volume_gb: 5")), want: "volume_gb"},
		"fly unpriced region":      {contents: poolsYAML(privateTop, flyPool("fly", "region: syd")), want: "set hourly_price"},
		"fly with hetzner keys":    {contents: poolsYAML(privateTop, flyPool("fly", "firewall_id: 77")), want: "only apply to Hetzner pools"},
		"fly with static keys":     {contents: poolsYAML(privateTop, flyPool("fly", "billing: owned")), want: "only apply to static pools"},
		"hetzner with fly keys":    {contents: poolsYAML(privateTop, hetznerPool("fast", "keep_stopped: 1")), want: "only apply to Fly pools"},
		"static with fly keys":     {contents: poolsYAML(privateTop, staticPool("lab", hostKey, "fly_app: ci-pool")), want: "only apply to Fly pools"},
		"placement ephemeral, none": {
			contents: poolsYAML(privateTop+"placement: [fast, ephemeral]\n", hetznerPool("fast")),
			mutate:   func(c *appConfig) { c.ComputeProvider = computeProviderNone },
			want:     "COMPUTE_PROVIDER is none",
		},
	} {
		t.Run(name, func(t *testing.T) {
			parsed, err := loadPoolsYAML(t, test.contents)
			if err != nil {
				t.Fatal(err)
			}
			cfg := poolsAppConfig()
			if test.mutate != nil {
				test.mutate(&cfg)
			}
			err = parsed.validate(cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want it to mention %q", err, test.want)
			}
		})
	}
}

func TestPoolsFileValidateFillsDefaults(t *testing.T) {
	t.Setenv(poolSSHKeyFileEnv, "")
	hostKey := staticHostKey(t)
	contents := poolsYAML(privateTop,
		hetznerPool("fast"),
		hetznerPool("big", "runner_image: ghcr.io/gwendall/runneryard:0.5.1", "location: hel1", "server_image: ubuntu-24.04", "min_free_disk_gb: 0", "caches: [/opt/cache]"),
		staticPool("lab", hostKey),
	)
	for name, test := range map[string]struct {
		provider  string
		placement []string
	}{
		"with the ephemeral tier": {provider: "hetzner", placement: []string{"fast", "big", "lab", ephemeralTier}},
		"without it":              {provider: computeProviderNone, placement: []string{"fast", "big", "lab"}},
	} {
		t.Run(name, func(t *testing.T) {
			parsed, err := loadPoolsYAML(t, contents)
			if err != nil {
				t.Fatal(err)
			}
			cfg := poolsAppConfig()
			cfg.ComputeProvider = test.provider
			cfg.HetznerLocation = "nbg1"
			if err := parsed.validate(cfg); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(parsed.Placement, test.placement) {
				t.Fatalf("placement = %v, want %v", parsed.Placement, test.placement)
			}
			fast, big := parsed.Pools[0], parsed.Pools[1]
			if fast.RunnerImage != cfg.RunnerImage || fast.Location != "nbg1" || fast.ServerImage != defaultHetznerImage ||
				fast.MinFreeDiskGB == nil || *fast.MinFreeDiskGB != defaultPoolDiskGB || !slices.Equal(fast.Caches, hostagent.DefaultCaches) {
				t.Fatalf("fast defaults = %#v", fast)
			}
			// An explicit zero is the operator's choice, not a missing value.
			if big.RunnerImage != "ghcr.io/gwendall/runneryard:0.5.1" || big.Location != "hel1" || big.ServerImage != "ubuntu-24.04" ||
				big.MinFreeDiskGB == nil || *big.MinFreeDiskGB != 0 || !slices.Equal(big.Caches, []string{"/opt/cache"}) {
				t.Fatalf("big settings = %#v", big)
			}
		})
	}
}

func TestFlyPoolDefaults(t *testing.T) {
	t.Setenv(poolSSHKeyFileEnv, "")
	parsed, err := loadPoolsYAML(t, poolsYAML(privateTop,
		flyPool("fly"),
		flyPool("solo", "fly_app: ci-solo", "max_hosts: 1", "min_hosts: 0", "region: iad", "cpu_kind: shared", "cpus: 4", "memory_mb: 4096",
			"slots: 2", "slot_memory_mb: 1536", "volume_gb: 20", "idle_stop_after: 2m", "keep_stopped: 0", "delete_stopped_after: 1h"),
	))
	if err != nil {
		t.Fatal(err)
	}
	cfg := poolsAppConfig()
	if err := parsed.validate(cfg); err != nil {
		t.Fatal(err)
	}
	fly, solo := parsed.Pools[0], parsed.Pools[1]
	if fly.Region != "cdg" || fly.CPUKind != "performance" || fly.VolumeGB != defaultFlyVolumeGB || fly.IdleStopAfter != defaultFlyIdleStop ||
		fly.KeepStopped == nil || *fly.KeepStopped != 1 || fly.DeleteStoppedAfter != defaultDeleteAfter {
		t.Fatalf("fly defaults = %#v", fly)
	}
	if solo.Region != "iad" || solo.CPUKind != "shared" || solo.VolumeGB != 20 || solo.IdleStopAfter != 2*time.Minute ||
		*solo.KeepStopped != 0 || solo.DeleteStoppedAfter != time.Hour {
		t.Fatalf("solo settings = %#v", solo)
	}
	if source := fly.flySourceConfig(cfg); source.APIToken != "fly-token" {
		t.Fatal("the pool did not fall back to FLY_API_TOKEN")
	}
	cfg.PoolFlyAPIToken = "pool-token"
	if source := fly.flySourceConfig(cfg); source.APIToken != "pool-token" || source.App != "ci-pool" {
		t.Fatalf("the pool token %s did not take precedence", poolFlyTokenEnv)
	}
}

func TestFlyPoolSlotsShareWhatTheHostKeepsFree(t *testing.T) {
	t.Setenv(poolSSHKeyFileEnv, "")
	parsed, err := loadPoolsYAML(t, poolsYAML(privateTop, flyPool("fly", "slot_memory_mb: -")))
	if err != nil {
		t.Fatal(err)
	}
	if err := parsed.validate(poolsAppConfig()); err != nil {
		t.Fatal(err)
	}
	if got, want := parsed.Pools[0].SlotMemoryMB, (16384-flyHostReservedMB)/4; got != want {
		t.Fatalf("slot_memory_mb = %d, want %d: an equal share of memory_mb less the host's part", got, want)
	}
}

func TestWithPoolsBuildsAFlyPool(t *testing.T) {
	t.Setenv(poolSSHKeyFileEnv, "")
	keyFile := writePoolKey(t)
	parsed, err := loadPoolsYAML(t, poolsYAML("trust: private\nssh_key_file: "+keyFile+"\n", flyPool("fly")))
	if err != nil {
		t.Fatal(err)
	}
	cfg := poolsAppConfig()
	cfg.ComputeProvider = computeProviderNone
	if err := parsed.validate(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Pools = parsed
	compute, err := cfg.withPools(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	status := placementStatus(t, compute)
	if !slices.Equal(status.Tiers, []string{"fly"}) || len(status.Pools) != 1 {
		t.Fatalf("status = %#v", status)
	}
}

func TestComputeProviderNoneRequiresPools(t *testing.T) {
	setValidEnv(t)
	t.Setenv("COMPUTE_PROVIDER", computeProviderNone)
	t.Setenv("HCLOUD_TOKEN", "hcloud-token")
	t.Setenv("HCLOUD_API_BASE_URL", "http://127.0.0.1:9")
	t.Setenv(poolSSHKeyFileEnv, "")
	t.Setenv(poolsFileEnv, "")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "COMPUTE_PROVIDER=none requires "+poolsFileEnv) {
		t.Fatalf("err = %v", err)
	}

	keyFile := writePoolKey(t)
	write := func(contents string) string {
		path := filepath.Join(t.TempDir(), "pools.yaml")
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	top := "trust: private\nssh_key_file: " + keyFile + "\n"

	t.Setenv(poolsFileEnv, write(poolsYAML(top+"placement: [fast, ephemeral]\n", hetznerPool("fast"))))
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "COMPUTE_PROVIDER is none") {
		t.Fatalf("err = %v", err)
	}

	t.Setenv(poolsFileEnv, write(poolsYAML(top, hetznerPool("fast"))))
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Pools.Placement, []string{"fast"}) {
		t.Fatalf("placement = %v", cfg.Pools.Placement)
	}
	compute, err := cfg.compute(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	status := placementStatus(t, compute)
	if !slices.Equal(status.Tiers, []string{"fast"}) || len(status.Draining) != 0 {
		t.Fatalf("status = %#v", status)
	}
}

type poolTestCompute struct{}

func (poolTestCompute) Launch(context.Context, provider.Lease) (provider.Worker, error) {
	return provider.Worker{}, nil
}

func (poolTestCompute) Inventory(context.Context) ([]provider.Worker, error) { return nil, nil }

func (poolTestCompute) Destroy(context.Context, string) error { return nil }

func placementStatus(t *testing.T, compute provider.Compute) pool.PlacementStatus {
	t.Helper()
	placement, ok := compute.(*pool.Placement)
	if !ok {
		t.Fatalf("compute is a %T, want *pool.Placement", compute)
	}
	status, ok := placement.ComputeStatus().(pool.PlacementStatus)
	if !ok {
		t.Fatalf("status is a %T", placement.ComputeStatus())
	}
	return status
}

func TestWithPoolsFollowsThePlacementOrder(t *testing.T) {
	t.Setenv(poolSSHKeyFileEnv, "")
	keyFile := writePoolKey(t)
	hostKey := staticHostKey(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, test := range map[string]struct {
		placement    string
		provider     string
		wantTiers    []string
		wantDraining []string
	}{
		"default order":           {provider: "hetzner", wantTiers: []string{"fast", "lab", ephemeralTier}},
		"pools reordered":         {provider: "hetzner", placement: "[lab, fast]", wantTiers: []string{"lab", "fast"}, wantDraining: []string{ephemeralTier}},
		"ephemeral first":         {provider: "hetzner", placement: "[ephemeral, lab]", wantTiers: []string{ephemeralTier, "lab"}, wantDraining: []string{"fast"}},
		"one tier, two draining":  {provider: "hetzner", placement: "[fast]", wantTiers: []string{"fast"}, wantDraining: []string{ephemeralTier, "lab"}},
		"no ephemeral provider":   {provider: computeProviderNone, wantTiers: []string{"fast", "lab"}},
		"no ephemeral, one tier":  {provider: computeProviderNone, placement: "[lab]", wantTiers: []string{"lab"}, wantDraining: []string{"fast"}},
		"only the ephemeral tier": {provider: "hetzner", placement: "[ephemeral]", wantTiers: []string{ephemeralTier}, wantDraining: []string{"fast", "lab"}},
	} {
		t.Run(name, func(t *testing.T) {
			top := "trust: private\nssh_key_file: " + keyFile + "\n"
			if test.placement != "" {
				top += "placement: " + test.placement + "\n"
			}
			parsed, err := loadPoolsYAML(t, poolsYAML(top, hetznerPool("fast"), staticPool("lab", hostKey)))
			if err != nil {
				t.Fatal(err)
			}
			cfg := poolsAppConfig()
			cfg.ComputeProvider = test.provider
			if err := parsed.validate(cfg); err != nil {
				t.Fatal(err)
			}
			cfg.Pools = parsed
			var ephemeral provider.Compute
			if test.provider != computeProviderNone {
				ephemeral = poolTestCompute{}
			}
			compute, err := cfg.withPools(ephemeral, logger)
			if err != nil {
				t.Fatal(err)
			}
			status := placementStatus(t, compute)
			if !slices.Equal(status.Tiers, test.wantTiers) || !slices.Equal(status.Draining, test.wantDraining) {
				t.Fatalf("tiers = %v draining = %v, want %v and %v", status.Tiers, status.Draining, test.wantTiers, test.wantDraining)
			}
			if len(status.Pools) != 2 {
				t.Fatalf("pool statuses = %#v, want both pools, draining or not", status.Pools)
			}
		})
	}
}

func TestWithPoolsMakesPoolsSpeculativeUnlessTheyOptOut(t *testing.T) {
	t.Setenv(poolSSHKeyFileEnv, "")
	keyFile := writePoolKey(t)
	hostKey := staticHostKey(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, test := range map[string]struct {
		placement string
		lab       []string
		want      []string
	}{
		"on by default":            {want: []string{"fast", "lab"}},
		"opted out":                {lab: []string{"speculative: false"}, want: []string{"fast"}},
		"explicitly on":            {lab: []string{"speculative: true"}, want: []string{"fast", "lab"}},
		"pools after ephemeral":    {placement: "[fast, ephemeral, lab]", want: []string{"fast"}},
		"ephemeral first, no pool": {placement: "[ephemeral, fast, lab]"},
	} {
		t.Run(name, func(t *testing.T) {
			top := "trust: private\nssh_key_file: " + keyFile + "\n"
			if test.placement != "" {
				top += "placement: " + test.placement + "\n"
			}
			parsed, err := loadPoolsYAML(t, poolsYAML(top, hetznerPool("fast"), staticPool("lab", hostKey, test.lab...)))
			if err != nil {
				t.Fatal(err)
			}
			cfg := poolsAppConfig()
			if err := parsed.validate(cfg); err != nil {
				t.Fatal(err)
			}
			cfg.Pools = parsed
			compute, err := cfg.withPools(poolTestCompute{}, logger)
			if err != nil {
				t.Fatal(err)
			}
			if got := placementStatus(t, compute).Speculative; !slices.Equal(got, test.want) {
				t.Fatalf("speculative pools = %v, want %v", got, test.want)
			}
		})
	}
}

func TestWithPoolsRefusesABadControllerKey(t *testing.T) {
	t.Setenv(poolSSHKeyFileEnv, "")
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalECPrivateKey(ecKey)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	keys := map[string][]byte{
		"ecdsa key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: ecDER}),
		"garbage":   []byte("not a key\n"),
	}
	paths := map[string]string{"missing file": filepath.Join(directory, "missing")}
	for name, contents := range keys {
		path := filepath.Join(directory, strings.ReplaceAll(name, " ", "-"))
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		paths[name] = path
	}
	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			parsed, err := loadPoolsYAML(t, poolsYAML("trust: private\nssh_key_file: "+path+"\n", hetznerPool("fast")))
			if err != nil {
				t.Fatal(err)
			}
			cfg := poolsAppConfig()
			if err := parsed.validate(cfg); err != nil {
				t.Fatal(err)
			}
			cfg.Pools = parsed
			if compute, err := cfg.withPools(poolTestCompute{}, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
				t.Fatalf("expected refusal, got %T", compute)
			}
		})
	}
}
