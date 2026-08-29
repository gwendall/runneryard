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

	var created machine
	if err := a.doJSON(ctx, http.MethodPost, a.machinesURL(), payload, &created); err != nil {
		if created.ID != "" {
			return provider.Worker{}, &provider.PartialLaunchError{Worker: withLeaseProof(toWorker(created), lease), Err: err}
		}
		return provider.Worker{}, fmt.Errorf("create Fly worker %s: %w", lease.RunnerName, err)
	}
	if created.ID == "" {
		return provider.Worker{}, fmt.Errorf("create Fly worker %s: response did not include a machine id", lease.RunnerName)
	}
	return withLeaseProof(toWorker(created), lease), nil
}

func (a *Adapter) Inventory(ctx context.Context) ([]provider.Worker, error) {
	var machines []machine
	if err := a.doJSON(ctx, http.MethodGet, a.machinesURL(), nil, &machines); err != nil {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, requestURL, nil)
	if err != nil {
		return fmt.Errorf("build delete request: %w", err)
	}
	a.setHeaders(req)
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete Fly worker %s: %w", workerID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseError(resp)
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
		return err
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

func responseError(resp *http.Response) error {
	contents, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr != nil {
		return errors.Join(fmt.Errorf("fly API returned %s", resp.Status), readErr)
	}
	message := strings.TrimSpace(string(contents))
	if message == "" {
		return fmt.Errorf("fly API returned %s", resp.Status)
	}
	return fmt.Errorf("fly API returned %s: %s", resp.Status, message)
}

var _ provider.Compute = (*Adapter)(nil)
