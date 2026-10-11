package hetzner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gwendall/runneryard/pool"
	"github.com/gwendall/runneryard/provider/retry"
)

const (
	poolHostKey     = "runneryard-pool-host"
	poolNameKey     = "runneryard-pool"
	hostIDPrefix    = "hetzner-"
	hostNamePrefix  = "ry-"
	sshPort         = "22"
	installedBinary = "/usr/local/bin/runneryard"
)

// HostSourceConfig configures Hetzner Cloud servers as pool hosts. Pool hosts
// carry their own labels and never the ephemeral worker label, so an
// ephemeral adapter of any version neither adopts nor deletes them.
type HostSourceConfig struct {
	APIToken     string
	APIBaseURL   string
	Location     string
	ServerType   string
	ServerImage  string
	ControllerID string
	Pool         string
	// FirewallID must allow SSH from the controller and should allow nothing
	// else inbound; it is verified before a host is powered on.
	FirewallID int64
	NetworkID  int64
	// HourlyPrice is the price of a host hour in EUR; zero takes the listed
	// price of ServerType.
	HourlyPrice float64

	// RunnerImage, Slots, and the slot limits are passed to
	// "runneryard host install" at first boot.
	RunnerImage   string
	Slots         int
	SlotCPUs      float64
	SlotMemoryMB  int
	Caches        []string
	MinFreeDiskGB int

	// HostKey derives a host's SSH key from its name: the OpenSSH private key
	// and its public authorized_keys line. The private key reaches the host in
	// its user data; the public key is what the controller pins.
	HostKey func(name string) ([]byte, string, error)
	// AuthorizedKey is the controller's public key, the only key the host's
	// agent account accepts.
	AuthorizedKey string

	HTTPClient         *http.Client
	ActionPollInterval time.Duration
	ActionTimeout      time.Duration
	Retry              retry.Policy
}

// HostSource creates, lists, and deletes Hetzner servers for one pool.
type HostSource struct {
	cfg   HostSourceConfig
	api   *Adapter
	price float64
}

// NewHostSource validates the configuration.
func NewHostSource(cfg HostSourceConfig) (*HostSource, error) {
	if !pool.ValidName(cfg.Pool) {
		return nil, fmt.Errorf("hetzner pool name %q must be lowercase letters, digits, and hyphens", cfg.Pool)
	}
	if cfg.HostKey == nil || strings.TrimSpace(cfg.AuthorizedKey) == "" {
		return nil, fmt.Errorf("hetzner pool %s requires the controller SSH key", cfg.Pool)
	}
	if cfg.Slots < 1 {
		return nil, fmt.Errorf("hetzner pool %s requires at least one slot per host", cfg.Pool)
	}
	if cfg.SlotCPUs < 0 || cfg.SlotMemoryMB < 0 || cfg.MinFreeDiskGB < 0 {
		return nil, fmt.Errorf("hetzner pool %s slot limits cannot be negative", cfg.Pool)
	}
	for _, path := range cfg.Caches {
		if path == "" || strings.ContainsAny(path, " \t\r\n'\"") {
			return nil, fmt.Errorf("hetzner pool %s cache path %q is not a plain path", cfg.Pool, path)
		}
	}
	price := cfg.HourlyPrice
	if price < 0 {
		return nil, fmt.Errorf("hetzner pool %s hourly price cannot be negative", cfg.Pool)
	}
	if price == 0 {
		listed, ok := HourlyPriceEUR(cfg.ServerType)
		if !ok {
			return nil, fmt.Errorf("hetzner pool %s: no listed price for server type %q; set hourly_price", cfg.Pool, cfg.ServerType)
		}
		price = listed
	}
	api, err := New(Config{
		APIToken: cfg.APIToken, APIBaseURL: cfg.APIBaseURL, Location: cfg.Location, ServerType: cfg.ServerType,
		ServerImage: cfg.ServerImage, RunnerImage: cfg.RunnerImage, ControllerID: cfg.ControllerID,
		FirewallID: cfg.FirewallID, NetworkID: cfg.NetworkID, HTTPClient: cfg.HTTPClient,
		ActionPollInterval: cfg.ActionPollInterval, ActionTimeout: cfg.ActionTimeout, Retry: cfg.Retry,
	})
	if err != nil {
		return nil, fmt.Errorf("hetzner pool %s: %w", cfg.Pool, err)
	}
	return &HostSource{cfg: cfg, api: api, price: price}, nil
}

// Facts: Hetzner bills every started hour of a server until it is deleted.
func (s *HostSource) Facts() pool.Facts {
	return pool.Facts{Billing: pool.BillingPerStartedHour, HourlyPrice: s.price, Currency: "EUR", CanCreate: true}
}

func (s *HostSource) selector() string {
	return poolHostKey + "=true," + controllerIDKey + "=" + s.cfg.ControllerID + "," + poolNameKey + "=" + s.cfg.Pool
}

func (s *HostSource) owns(item server) bool {
	return item.Labels[poolHostKey] == "true" && item.Labels[controllerIDKey] == s.cfg.ControllerID &&
		item.Labels[poolNameKey] == s.cfg.Pool && item.Labels[managedByKey] == ""
}

// List returns this pool's servers with the host key derived from each name.
func (s *HostSource) List(ctx context.Context) ([]pool.Host, error) {
	servers, err := s.api.listRaw(ctx, s.selector())
	if err != nil {
		return nil, err
	}
	hosts := make([]pool.Host, 0, len(servers))
	for _, item := range servers {
		if !s.owns(item) {
			continue
		}
		host, err := s.toHost(item)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, host)
	}
	return hosts, nil
}

func (s *HostSource) toHost(item server) (pool.Host, error) {
	_, public, err := s.cfg.HostKey(item.Name)
	if err != nil {
		return pool.Host{}, fmt.Errorf("derive host key of %s: %w", item.Name, err)
	}
	host := pool.Host{
		ID:        hostIDPrefix + strconv.FormatInt(item.ID, 10),
		HostKey:   public,
		CreatedAt: item.CreatedAt,
		State:     item.Status,
	}
	if ip := item.PublicNet.IPv4.IP; ip != "" {
		host.Address = net.JoinHostPort(ip, sshPort)
	}
	return host, nil
}

// Create makes a server, verifies its firewall while it is still off, and
// powers it on. Its first boot installs the host agent. A server that fails
// before power on is deleted; one that cannot be deleted stays labelled, and
// the pool returns it when its boot times out.
func (s *HostSource) Create(ctx context.Context) (pool.Host, error) {
	name, err := s.newName()
	if err != nil {
		return pool.Host{}, err
	}
	privateKey, publicKey, err := s.cfg.HostKey(name)
	if err != nil {
		return pool.Host{}, fmt.Errorf("derive host key of %s: %w", name, err)
	}
	userData, err := renderHostCloudInit(hostBootstrap{
		privateKey: privateKey, publicKey: publicKey, image: s.cfg.RunnerImage, slots: s.cfg.Slots,
		authorizedKey: strings.TrimSpace(s.cfg.AuthorizedKey), slotCPUs: s.cfg.SlotCPUs,
		slotMemoryMB: s.cfg.SlotMemoryMB, minFreeDiskGB: s.cfg.MinFreeDiskGB, caches: s.cfg.Caches,
	})
	if err != nil {
		return pool.Host{}, fmt.Errorf("render pool host bootstrap: %w", err)
	}
	payload := createServerRequest{
		Name:             name,
		ServerType:       s.cfg.ServerType,
		Image:            s.cfg.ServerImage,
		Location:         s.cfg.Location,
		UserData:         userData,
		StartAfterCreate: false,
		Labels: map[string]string{
			poolHostKey:     "true",
			controllerIDKey: s.cfg.ControllerID,
			poolNameKey:     s.cfg.Pool,
		},
		Firewalls: []createServerFirewall{{Firewall: s.cfg.FirewallID}},
		PublicNet: createServerPublicNet{EnableIPv4: true, EnableIPv6: true},
	}
	if s.cfg.NetworkID > 0 {
		payload.Networks = []int64{s.cfg.NetworkID}
	}
	if err := s.api.retryer.Wait(ctx); err != nil {
		return pool.Host{}, err
	}
	var created createServerResponse
	if err := s.api.doJSON(ctx, http.MethodPost, s.api.baseURL+"/servers", payload, &created); err != nil {
		err = fmt.Errorf("create Hetzner pool host %s: %w", name, err)
		if created.Server.ID != 0 {
			return pool.Host{}, errors.Join(err, s.discard(ctx, created.Server.ID))
		}
		return pool.Host{}, err
	}
	if created.Server.ID == 0 {
		return pool.Host{}, fmt.Errorf("create Hetzner pool host %s: response did not include a server id", name)
	}
	running, err := s.qualify(ctx, created)
	if err != nil {
		err = fmt.Errorf("prepare Hetzner pool host %s: %w", name, err)
		return pool.Host{}, errors.Join(err, s.discard(ctx, created.Server.ID))
	}
	return s.toHost(running)
}

func (s *HostSource) qualify(ctx context.Context, created createServerResponse) (server, error) {
	qualifyCtx, cancel := context.WithTimeout(ctx, s.api.actionTimeout)
	defer cancel()
	id := strconv.FormatInt(created.Server.ID, 10)
	if err := s.api.waitActions(qualifyCtx, append([]action{created.Action}, created.NextActions...)); err != nil {
		return server{}, err
	}
	var qualified serverResponse
	if err := s.api.doJSON(qualifyCtx, http.MethodGet, s.api.baseURL+"/servers/"+id, nil, &qualified); err != nil {
		return server{}, fmt.Errorf("verify firewall: %w", err)
	}
	if !firewallApplied(qualified.Server, s.cfg.FirewallID) {
		return server{}, fmt.Errorf("required firewall %d is not applied", s.cfg.FirewallID)
	}
	var poweredOn actionResponse
	if err := s.api.doJSON(qualifyCtx, http.MethodPost, s.api.baseURL+"/servers/"+id+"/actions/poweron", nil, &poweredOn); err != nil {
		return server{}, fmt.Errorf("power on: %w", err)
	}
	if err := s.api.waitActions(qualifyCtx, []action{poweredOn.Action}); err != nil {
		return server{}, fmt.Errorf("power on: %w", err)
	}
	var running serverResponse
	if err := s.api.doJSON(qualifyCtx, http.MethodGet, s.api.baseURL+"/servers/"+id, nil, &running); err != nil {
		return server{}, fmt.Errorf("confirm power on: %w", err)
	}
	if !s.owns(running.Server) {
		return server{}, errors.New("created server does not carry the pool labels")
	}
	return running.Server, nil
}

func (s *HostSource) discard(ctx context.Context, serverID int64) error {
	if err := s.api.Destroy(context.WithoutCancel(ctx), strconv.FormatInt(serverID, 10)); err != nil {
		return fmt.Errorf("delete failed pool host: %w", err)
	}
	return nil
}

// Delete deletes the server only after confirming it is one of this pool's
// hosts. A server that no longer exists is already deleted.
func (s *HostSource) Delete(ctx context.Context, hostID string) error {
	raw, ok := strings.CutPrefix(hostID, hostIDPrefix)
	serverID, err := strconv.ParseInt(raw, 10, 64)
	if !ok || err != nil || serverID < 1 {
		return fmt.Errorf("invalid Hetzner pool host id %q", hostID)
	}
	var current serverResponse
	err = s.api.retryer.Do(ctx, func(ctx context.Context) error {
		current = serverResponse{}
		return s.api.doJSON(ctx, http.MethodGet, s.api.baseURL+"/servers/"+raw, nil, &current)
	})
	if err != nil {
		var status *retry.StatusError
		if errors.As(err, &status) && status.Status == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("look up Hetzner pool host %s: %w", hostID, err)
	}
	if !s.owns(current.Server) {
		return fmt.Errorf("refusing to delete Hetzner server %s: it is not a host of pool %s", raw, s.cfg.Pool)
	}
	return s.api.Destroy(ctx, raw)
}

func (s *HostSource) newName() (string, error) {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("name pool host: %w", err)
	}
	return hostNamePrefix + s.cfg.Pool + "-" + hex.EncodeToString(suffix), nil
}

type hostBootstrap struct {
	privateKey    []byte
	publicKey     string
	image         string
	slots         int
	authorizedKey string
	slotCPUs      float64
	slotMemoryMB  int
	minFreeDiskGB int
	caches        []string
}

// hostBootstrapScript pulls the runner image, takes the runneryard binary out
// of it, and lets "runneryard host install" prepare the machine. The image is
// passed as an argument, never interpolated into the script.
const hostBootstrapScript = `set -Eeuo pipefail
image=$1
shift
for attempt in 1 2 3 4 5; do
  docker pull "$image" && break
  [ "$attempt" = 5 ] && exit 1
  sleep 10
done
container=$(docker create "$image")
trap 'docker rm --force "$container" >/dev/null 2>&1 || true' EXIT
docker cp "$container:` + installedBinary + `" ` + installedBinary + `.new
chmod 0755 ` + installedBinary + `.new
mv ` + installedBinary + `.new ` + installedBinary + `
` + installedBinary + ` host install --image "$image" "$@"`

// renderHostCloudInit installs the derived host key in place of generated
// ones, so the controller's pin holds from the first connection, and runs the
// bootstrap. The user data holds the host's private key and nothing that
// reaches beyond the host: the controller key stays on the controller.
func renderHostCloudInit(bootstrap hostBootstrap) (string, error) {
	if len(bootstrap.privateKey) == 0 || bootstrap.publicKey == "" || bootstrap.image == "" || bootstrap.authorizedKey == "" {
		return "", errors.New("host key, runner image, and controller key are required")
	}
	args := []string{
		"bash", "-c", hostBootstrapScript, "runneryard-host-bootstrap", bootstrap.image,
		"--slots", strconv.Itoa(bootstrap.slots),
		"--authorized-key", bootstrap.authorizedKey,
		"--min-free-disk-gb", strconv.Itoa(bootstrap.minFreeDiskGB),
		"--harden-sshd", "--block-metadata",
	}
	if bootstrap.slotCPUs > 0 {
		args = append(args, "--slot-cpus", strconv.FormatFloat(bootstrap.slotCPUs, 'f', -1, 64))
	}
	if bootstrap.slotMemoryMB > 0 {
		args = append(args, "--slot-memory-mb", strconv.Itoa(bootstrap.slotMemoryMB))
	}
	for _, path := range bootstrap.caches {
		args = append(args, "--cache", path)
	}
	cloudConfig := struct {
		SSHPasswordAuth bool              `json:"ssh_pwauth"`
		DisableRoot     bool              `json:"disable_root"`
		SSHDeleteKeys   bool              `json:"ssh_deletekeys"`
		SSHGenKeyTypes  []string          `json:"ssh_genkeytypes"`
		SSHKeys         map[string]string `json:"ssh_keys"`
		RunCmd          [][]string        `json:"runcmd"`
	}{
		SSHPasswordAuth: false,
		DisableRoot:     true,
		SSHDeleteKeys:   true,
		SSHGenKeyTypes:  []string{},
		SSHKeys: map[string]string{
			"ed25519_private": string(bootstrap.privateKey),
			"ed25519_public":  bootstrap.publicKey,
		},
		RunCmd: [][]string{args},
	}
	encoded, err := json.Marshal(cloudConfig)
	if err != nil {
		return "", err
	}
	userData := "#cloud-config\n" + string(encoded) + "\n"
	if len(userData) > 32<<10 {
		return "", fmt.Errorf("pool host user data is %d bytes, above Hetzner's 32 KiB", len(userData))
	}
	return userData, nil
}

// listRaw returns every server matching the selector, unfiltered.
func (a *Adapter) listRaw(ctx context.Context, labelSelector string) ([]server, error) {
	servers := make([]server, 0)
	err := a.eachServer(ctx, labelSelector, func(item server) { servers = append(servers, item) })
	return servers, err
}

var _ pool.Source = (*HostSource)(nil)
