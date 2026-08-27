// Package fly implements the compute seam with ephemeral Fly Machines.
package fly

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gwendall/runneryard/provider"
)

const (
	managedByKey       = "runneryard-managed-by"
	controllerIDKey    = "runneryard-controller"
	leaseIDKey         = "runneryard-lease-id"
	runnerNameKey      = "runneryard-runner-name"
	deadlineKey        = "runneryard-deadline"
	runnerEntrypoint   = "/usr/local/bin/runner-entrypoint"
	defaultHTTPTimeout = 30 * time.Second
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
	httpClient   *http.Client
}

func New(cfg Config) (*Adapter, error) {
	if cfg.APIToken == "" || cfg.App == "" || cfg.Region == "" || cfg.Image == "" || cfg.ControllerID == "" {
		return nil, fmt.Errorf("Fly token, worker app, region, image, and controller ID are required")
	}
	if cfg.CPUKind != "shared" && cfg.CPUKind != "performance" {
		return nil, fmt.Errorf("Fly CPU kind must be shared or performance")
	}
	if cfg.CPUs < 1 || cfg.MemoryMB < 512 || cfg.RootFSGB < 10 {
		return nil, fmt.Errorf("Fly worker shape must have at least 1 CPU, 512 MB RAM, and 10 GB rootfs")
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
		httpClient:   cfg.HTTPClient,
	}, nil
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
	Image        string            `json:"image"`
	Processes    []processConfig   `json:"processes,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	AutoDestroy  bool              `json:"auto_destroy"`
	Restart      restartPolicy     `json:"restart"`
	Guest        guestConfig       `json:"guest"`
	RootFSSizeGB int               `json:"rootfs_size_gb,omitempty"`
	StopConfig   stopConfig        `json:"stop_config,omitempty"`
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
				},
				IgnoreAppSecrets: true,
			}},
			Metadata: map[string]string{
				managedByKey:    "true",
				controllerIDKey: a.controllerID,
				leaseIDKey:      lease.ID,
				runnerNameKey:   lease.RunnerName,
				deadlineKey:     lease.Deadline.UTC().Format(time.RFC3339),
			},
			AutoDestroy:  true,
			Restart:      restartPolicy{Policy: "no"},
			Guest:        guestConfig{CPUs: a.cpus, MemoryMB: a.memoryMB, CPUKind: a.cpuKind},
			RootFSSizeGB: a.rootFSGB,
			StopConfig:   stopConfig{Signal: "SIGTERM", Timeout: flyDuration(30 * time.Second)},
		},
	}

	var created machine
	if err := a.doJSON(ctx, http.MethodPost, a.machinesURL(), payload, &created); err != nil {
		if created.ID != "" {
			return provider.Worker{}, &provider.PartialLaunchError{Worker: toWorker(created), Err: err}
		}
		return provider.Worker{}, fmt.Errorf("create Fly worker %s: %w", lease.RunnerName, err)
	}
	if created.ID == "" {
		return provider.Worker{}, fmt.Errorf("create Fly worker %s: response did not include a machine id", lease.RunnerName)
	}
	return toWorker(created), nil
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
	return provider.Worker{
		ID:         item.ID,
		LeaseID:    item.Config.Metadata[leaseIDKey],
		RunnerName: item.Config.Metadata[runnerNameKey],
		State:      item.State,
		CreatedAt:  item.CreatedAt,
	}
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
		return errors.Join(fmt.Errorf("Fly API returned %s", resp.Status), readErr)
	}
	message := strings.TrimSpace(string(contents))
	if message == "" {
		return fmt.Errorf("Fly API returned %s", resp.Status)
	}
	return fmt.Errorf("Fly API returned %s: %s", resp.Status, message)
}

var _ provider.Compute = (*Adapter)(nil)
