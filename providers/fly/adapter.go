// Package fly implements the compute seam with ephemeral Fly Machines.
package fly

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gwendall/runneryard/provider"
	"github.com/gwendall/runneryard/provider/retry"
)

const (
	managedByKey        = "runneryard-managed-by"
	controllerIDKey     = "runneryard-controller"
	leaseIDKey          = "runneryard-lease-id"
	runnerNameKey       = "runneryard-runner-name"
	runnerIDKey         = "runneryard-runner-id"
	runnerScaleSetIDKey = "runneryard-runner-scale-set-id"
	deadlineKey         = "runneryard-deadline"
	runnerEntrypoint    = "/usr/local/bin/runner-entrypoint"
	defaultHTTPTimeout  = 30 * time.Second
	defaultDockerDNS    = "1.1.1.1,8.8.8.8"
)

type Config struct {
	APIToken     string
	APIBaseURL   string
	App          string
	Region       string
	Image        string
	ControllerID string
	CPUKind      string
	CPUs         int
	MemoryMB     int
	RootFSGB     int
	DockerDNS    string
	HTTPClient   *http.Client
	// Retry bounds repeated attempts and paces requests to the Machines API.
	// Zero fields take the retry package defaults.
	Retry retry.Policy
}

type Adapter struct {
	baseURL      string
	token        string
	app          string
	region       string
	image        string
	controllerID string
	cpuKind      string
	cpus         int
	memoryMB     int
	rootFSGB     int
	dockerDNS    string
	httpClient   *http.Client
	retryer      *retry.Retryer
}

func New(cfg Config) (*Adapter, error) {
	if cfg.APIToken == "" || cfg.App == "" || cfg.Region == "" || cfg.Image == "" || cfg.ControllerID == "" {
		return nil, fmt.Errorf("fly token, worker app, region, image, and controller ID are required")
	}
	if cfg.CPUKind != "shared" && cfg.CPUKind != "performance" {
		return nil, fmt.Errorf("fly CPU kind must be shared or performance")
	}
	if cfg.CPUs < 1 || cfg.MemoryMB < 512 || cfg.RootFSGB < 10 {
		return nil, fmt.Errorf("fly worker shape must have at least 1 CPU, 512 MB RAM, and 10 GB rootfs")
	}
	dockerDNS, err := normalizeDockerDNS(cfg.DockerDNS)
	if err != nil {
		return nil, err
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = "https://api.machines.dev"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &Adapter{
		baseURL:      strings.TrimRight(cfg.APIBaseURL, "/"),
		token:        cfg.APIToken,
		app:          cfg.App,
		region:       cfg.Region,
		image:        cfg.Image,
		controllerID: cfg.ControllerID,
		cpuKind:      cfg.CPUKind,
		cpus:         cfg.CPUs,
		memoryMB:     cfg.MemoryMB,
		rootFSGB:     cfg.RootFSGB,
		dockerDNS:    dockerDNS,
		httpClient:   cfg.HTTPClient,
		retryer:      retry.New(cfg.Retry),
	}, nil
}

func normalizeDockerDNS(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		value = defaultDockerDNS
	}
	items := strings.Split(value, ",")
	if len(items) < 1 || len(items) > 3 {
		return "", fmt.Errorf("fly Docker DNS must contain between one and three comma-separated IP addresses")
	}
	normalized := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		ip := net.ParseIP(strings.TrimSpace(item))
		if ip == nil {
			return "", fmt.Errorf("fly Docker DNS entry %q must be a literal IP address", item)
		}
		canonical := ip.String()
		if _, exists := seen[canonical]; exists {
			return "", fmt.Errorf("fly Docker DNS contains duplicate address %q", canonical)
		}
		seen[canonical] = struct{}{}
		normalized = append(normalized, canonical)
	}
	return strings.Join(normalized, ","), nil
}

type machine struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	State     string        `json:"state"`
	Region    string        `json:"region"`
	CreatedAt time.Time     `json:"created_at"`
	Config    machineConfig `json:"config"`
}

type machineConfig struct {
	Image       string            `json:"image"`
	Processes   []processConfig   `json:"processes,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	AutoDestroy bool              `json:"auto_destroy"`
	Restart     restartPolicy     `json:"restart"`
	Guest       guestConfig       `json:"guest"`
	RootFS      rootFSConfig      `json:"rootfs"`
	StopConfig  stopConfig        `json:"stop_config,omitempty"`
}

type rootFSConfig struct {
	SizeGB int `json:"size_gb"`
}

type processConfig struct {
	Entrypoint       []string          `json:"entrypoint"`
	Env              map[string]string `json:"env,omitempty"`
	IgnoreAppSecrets bool              `json:"ignore_app_secrets"`
}

type restartPolicy struct {
	Policy string `json:"policy"`
}

type guestConfig struct {
	CPUs     int    `json:"cpus"`
	MemoryMB int    `json:"memory_mb"`
	CPUKind  string `json:"cpu_kind"`
}

type stopConfig struct {
	Signal  string      `json:"signal,omitempty"`
	Timeout flyDuration `json:"timeout,omitempty"`
}

type flyDuration time.Duration

func (d flyDuration) MarshalJSON() ([]byte, error) { return json.Marshal(int64(d)) }

func (d *flyDuration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		parsed, err := time.ParseDuration(text)
		if err != nil {
			return fmt.Errorf("parse Fly duration %q: %w", text, err)
		}
		*d = flyDuration(parsed)
		return nil
	}
	var nanoseconds int64
	if err := json.Unmarshal(data, &nanoseconds); err != nil {
		return fmt.Errorf("parse Fly duration: %w", err)
	}
	*d = flyDuration(time.Duration(nanoseconds))
	return nil
}

type createMachineRequest struct {
	Name   string        `json:"name"`
	Region string        `json:"region"`
	Config machineConfig `json:"config"`
}

func (a *Adapter) Launch(ctx context.Context, lease provider.Lease) (provider.Worker, error) {
	payload := createMachineRequest{
		Name:   lease.RunnerName,
		Region: a.region,
		Config: machineConfig{
			Image: a.image,
			Processes: []processConfig{{
				Entrypoint: []string{"/usr/bin/dumb-init", "--", runnerEntrypoint},
				Env: map[string]string{
					"ACTIONS_RUNNER_INPUT_JITCONFIG": lease.JITConfig,
					"RUNNERYARD_DEADLINE":            lease.Deadline.UTC().Format(time.RFC3339),
					"RUNNERYARD_DOCKER_DNS":          a.dockerDNS,
					"RUNNERYARD_IDLE_TIMEOUT":        strconv.FormatInt(int64(lease.IdleTimeout/time.Second), 10),
				},
				IgnoreAppSecrets: true,
			}},
			Metadata: map[string]string{
				managedByKey:        "true",
				controllerIDKey:     a.controllerID,
				leaseIDKey:          lease.ID,
				runnerNameKey:       lease.RunnerName,
				runnerIDKey:         strconv.FormatInt(lease.RunnerID, 10),
				runnerScaleSetIDKey: strconv.Itoa(lease.RunnerScaleSetID),
				deadlineKey:         lease.Deadline.UTC().Format(time.RFC3339),
			},
			AutoDestroy: true,
			Restart:     restartPolicy{Policy: "no"},
			Guest:       guestConfig{CPUs: a.cpus, MemoryMB: a.memoryMB, CPUKind: a.cpuKind},
			RootFS:      rootFSConfig{SizeGB: a.rootFSGB},
			StopConfig:  stopConfig{Signal: "SIGTERM", Timeout: flyDuration(30 * time.Second)},
		},
	}

	// A create request is only repeated after inventory proves the previous
	// attempt did not produce a Machine for this lease. A transport failure can
	// hide a successful create, and two Machines for one lease would share a
	// single-use JIT configuration.
	var lastErr error
	attempts := a.retryer.Policy().Attempts
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := a.retryer.Wait(ctx); err != nil {
			return provider.Worker{}, err
		}
		var created machine
		err := a.doJSON(ctx, http.MethodPost, a.machinesURL(), payload, &created)
		if err == nil {
			if created.ID == "" {
				return provider.Worker{}, fmt.Errorf("create Fly worker %s: response did not include a machine id", lease.RunnerName)
			}
			return withLeaseProof(toWorker(created), lease), nil
		}
		if created.ID != "" {
			return provider.Worker{}, &provider.PartialLaunchError{Worker: withLeaseProof(toWorker(created), lease), Err: err}
		}
		lastErr = fmt.Errorf("create Fly worker %s: %w", lease.RunnerName, err)
		if !provider.IsTransient(err) {
			return provider.Worker{}, lastErr
		}
		existing, lookupErr := a.findByLease(ctx, lease.ID)
		if lookupErr != nil {
			return provider.Worker{}, errors.Join(lastErr, fmt.Errorf("confirm Fly worker absence before retry: %w", lookupErr))
		}
		if existing != nil {
			return withLeaseProof(*existing, lease), nil
		}
		if attempt == attempts {
			break
		}
		if err := sleepContext(ctx, a.retryer.Backoff(attempt)); err != nil {
			return provider.Worker{}, errors.Join(lastErr, err)
		}
	}
	return provider.Worker{}, lastErr
}

// findByLease returns the owned Machine carrying the lease, or nil.
func (a *Adapter) findByLease(ctx context.Context, leaseID string) (*provider.Worker, error) {
	workers, err := a.Inventory(ctx)
	if err != nil {
		return nil, err
	}
	for _, worker := range workers {
		if worker.LeaseID == leaseID {
			return &worker, nil
		}
	}
	return nil, nil
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (a *Adapter) Inventory(ctx context.Context) ([]provider.Worker, error) {
	var machines []machine
	err := a.retryer.Do(ctx, func(ctx context.Context) error {
		machines = nil
		return a.doJSON(ctx, http.MethodGet, a.machinesURL(), nil, &machines)
	})
	if err != nil {
		return nil, fmt.Errorf("list Fly Machines: %w", err)
	}
	workers := make([]provider.Worker, 0, len(machines))
	for _, item := range machines {
		if item.Config.Metadata[managedByKey] != "true" || item.Config.Metadata[controllerIDKey] != a.controllerID {
			continue
		}
		workers = append(workers, toWorker(item))
	}
	return workers, nil
}

func (a *Adapter) Destroy(ctx context.Context, workerID string) error {
	if workerID == "" {
		return nil
	}
	requestURL := a.machinesURL() + "/" + url.PathEscape(workerID) + "?force=true"
	err := a.retryer.Do(ctx, func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, requestURL, nil)
		if err != nil {
			return fmt.Errorf("build delete request: %w", err)
		}
		a.setHeaders(req)
		resp, err := a.httpClient.Do(req)
		if err != nil {
			return retry.ClassifyRequestError(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return responseError(resp)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete Fly worker %s: %w", workerID, err)
	}
	return nil
}

func toWorker(item machine) provider.Worker {
	runnerID, _ := strconv.ParseInt(item.Config.Metadata[runnerIDKey], 10, 64)
	runnerScaleSetID, _ := strconv.Atoi(item.Config.Metadata[runnerScaleSetIDKey])
	return provider.Worker{
		ID:               item.ID,
		LeaseID:          item.Config.Metadata[leaseIDKey],
		RunnerName:       item.Config.Metadata[runnerNameKey],
		RunnerID:         runnerID,
		RunnerScaleSetID: runnerScaleSetID,
		State:            item.State,
		CreatedAt:        item.CreatedAt,
	}
}

func withLeaseProof(worker provider.Worker, lease provider.Lease) provider.Worker {
	if worker.LeaseID == "" {
		worker.LeaseID = lease.ID
	}
	if worker.RunnerName == "" {
		worker.RunnerName = lease.RunnerName
	}
	if worker.RunnerID == 0 {
		worker.RunnerID = lease.RunnerID
	}
	if worker.RunnerScaleSetID == 0 {
		worker.RunnerScaleSetID = lease.RunnerScaleSetID
	}
	return worker
}

func (a *Adapter) machinesURL() string {
	return a.baseURL + "/v1/apps/" + url.PathEscape(a.app) + "/machines"
}

func (a *Adapter) doJSON(ctx context.Context, method, requestURL string, body, target any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	a.setHeaders(req)
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return retry.ClassifyRequestError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseError(resp)
	}
	if target == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (a *Adapter) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/json")
	if req.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
}

// responseError classifies a non-2xx response. Throttling and provider-side
// errors are transient; a shortage of Machines, hosts, or resources is a
// bounded capacity ceiling; every other status is a permanent failure.
func responseError(resp *http.Response) error {
	contents, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr != nil {
		return errors.Join(retry.ClassifyStatus("fly", resp.StatusCode, ""), readErr)
	}
	message := strings.TrimSpace(string(contents))
	if reason := capacityReason(resp.StatusCode, message); reason != "" {
		return &provider.CapacityError{
			Reason: reason,
			Err:    fmt.Errorf("fly API returned %d: %s", resp.StatusCode, capacityDescriptions[reason]),
		}
	}
	return retry.ClassifyStatus("fly", resp.StatusCode, message)
}

// capacityDescriptions keeps the raw provider payload out of the error that
// reaches logs, status, and alerts.
var capacityDescriptions = map[string]string{
	"fly_machine_limit":          "organization machine limit reached",
	"fly_insufficient_resources": "region could not reserve the requested resources",
	"fly_placement_unavailable":  "no host could place the machine",
}

// capacityReason maps the Fly Machines API's shortage responses to stable,
// non-secret rejection codes. Fly reports every one of them with a client
// error status, so without this mapping they would be permanent failures that
// stopped the controller. On 2026-09-03 the organization machine limit did
// exactly that, with the whole queue behind it. Only well-known phrases match;
// a validation error that merely mentions resources stays permanent.
func capacityReason(status int, message string) string {
	if status < 400 || status >= 500 || status == http.StatusTooManyRequests {
		return ""
	}
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "machine limit"):
		return "fly_machine_limit"
	case strings.Contains(lower, "could not reserve resource"),
		strings.Contains(lower, "insufficient memory"),
		strings.Contains(lower, "insufficient cpu"),
		strings.Contains(lower, "insufficient resources"),
		strings.Contains(lower, "not enough capacity"),
		strings.Contains(lower, "no capacity"),
		strings.Contains(lower, "capacity available"):
		return "fly_insufficient_resources"
	case strings.Contains(lower, "no host"),
		strings.Contains(lower, "find a host"),
		strings.Contains(lower, "unable to place"),
		strings.Contains(lower, "failed to place"),
		strings.Contains(lower, "could not place"):
		return "fly_placement_unavailable"
	}
	return ""
}

var _ provider.Compute = (*Adapter)(nil)
