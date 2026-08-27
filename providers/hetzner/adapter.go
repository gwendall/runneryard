// Package hetzner implements the compute seam with disposable Hetzner Cloud servers.
package hetzner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gwendall/runneryard/provider"
)

const (
	managedByKey       = "runneryard-managed-by"
	controllerIDKey    = "runneryard-controller"
	leaseIDKey         = "runneryard-lease-id"
	runnerNameKey      = "runneryard-runner-name"
	defaultHTTPTimeout = 30 * time.Second
)

var safeLabelValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

type Config struct {
	APIToken     string
	APIBaseURL   string
	Location     string
	ServerType   string
	ServerImage  string
	RunnerImage  string
	ControllerID string
	FirewallID   int64
	NetworkID    int64
	HTTPClient   *http.Client
}

type Adapter struct {
	baseURL      string
	token        string
	location     string
	serverType   string
	serverImage  string
	runnerImage  string
	controllerID string
	firewallID   int64
	networkID    int64
	httpClient   *http.Client
}

func New(cfg Config) (*Adapter, error) {
	if cfg.APIToken == "" || cfg.Location == "" || cfg.ServerType == "" || cfg.ServerImage == "" || cfg.RunnerImage == "" || cfg.ControllerID == "" {
		return nil, fmt.Errorf("Hetzner token, location, server type, server image, runner image, and controller ID are required")
	}
	if cfg.FirewallID < 1 {
		return nil, fmt.Errorf("Hetzner worker firewall ID is required")
	}
	if !safeLabelValue.MatchString(cfg.ControllerID) {
		return nil, fmt.Errorf("Hetzner controller ID must be a valid label value of at most 63 characters")
	}
	if cfg.NetworkID < 0 {
		return nil, fmt.Errorf("Hetzner network ID cannot be negative")
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = "https://api.hetzner.cloud/v1"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &Adapter{
		baseURL:      strings.TrimRight(cfg.APIBaseURL, "/"),
		token:        cfg.APIToken,
		location:     cfg.Location,
		serverType:   cfg.ServerType,
		serverImage:  cfg.ServerImage,
		runnerImage:  cfg.RunnerImage,
		controllerID: cfg.ControllerID,
		firewallID:   cfg.FirewallID,
		networkID:    cfg.NetworkID,
		httpClient:   cfg.HTTPClient,
	}, nil
}

type server struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	CreatedAt time.Time         `json:"created"`
	Labels    map[string]string `json:"labels"`
}

type createServerRequest struct {
	Name             string                 `json:"name"`
	ServerType       string                 `json:"server_type"`
	Image            string                 `json:"image"`
	Location         string                 `json:"location"`
	UserData         string                 `json:"user_data"`
	StartAfterCreate bool                   `json:"start_after_create"`
	Labels           map[string]string      `json:"labels"`
	Firewalls        []createServerFirewall `json:"firewalls"`
	Networks         []int64                `json:"networks,omitempty"`
	PublicNet        createServerPublicNet  `json:"public_net"`
}

type createServerFirewall struct {
	Firewall int64 `json:"firewall"`
}

type createServerPublicNet struct {
	EnableIPv4 bool `json:"enable_ipv4"`
	EnableIPv6 bool `json:"enable_ipv6"`
}

type createServerResponse struct {
	Server server `json:"server"`
}

type serverListResponse struct {
	Servers []server `json:"servers"`
	Meta    struct {
		Pagination struct {
			NextPage *int `json:"next_page"`
		} `json:"pagination"`
	} `json:"meta"`
}

func (a *Adapter) Launch(ctx context.Context, lease provider.Lease) (provider.Worker, error) {
	userData, err := renderCloudInit(lease, a.runnerImage)
	if err != nil {
		return provider.Worker{}, fmt.Errorf("render Hetzner worker bootstrap: %w", err)
	}
	payload := createServerRequest{
		Name:             lease.RunnerName,
		ServerType:       a.serverType,
		Image:            a.serverImage,
		Location:         a.location,
		UserData:         userData,
		StartAfterCreate: true,
		Labels: map[string]string{
			managedByKey:    "true",
			controllerIDKey: a.controllerID,
			leaseIDKey:      lease.ID,
			runnerNameKey:   lease.RunnerName,
		},
		Firewalls: []createServerFirewall{{Firewall: a.firewallID}},
		PublicNet: createServerPublicNet{EnableIPv4: true, EnableIPv6: true},
	}
	if a.networkID > 0 {
		payload.Networks = []int64{a.networkID}
	}

	var created createServerResponse
	if err := a.doJSON(ctx, http.MethodPost, a.baseURL+"/servers", payload, &created); err != nil {
		if created.Server.ID != 0 {
			return provider.Worker{}, &provider.PartialLaunchError{Worker: toWorker(created.Server), Err: err}
		}
		return provider.Worker{}, fmt.Errorf("create Hetzner worker %s: %w", lease.RunnerName, err)
	}
	if created.Server.ID == 0 {
		return provider.Worker{}, fmt.Errorf("create Hetzner worker %s: response did not include a server id", lease.RunnerName)
	}
	return toWorker(created.Server), nil
}

func (a *Adapter) Inventory(ctx context.Context) ([]provider.Worker, error) {
	workers := make([]provider.Worker, 0)
	page := 1
	for {
		requestURL, err := url.Parse(a.baseURL + "/servers")
		if err != nil {
			return nil, fmt.Errorf("build Hetzner inventory URL: %w", err)
		}
		query := requestURL.Query()
		query.Set("label_selector", managedByKey+"=true,"+controllerIDKey+"="+a.controllerID)
		query.Set("page", strconv.Itoa(page))
		query.Set("per_page", "50")
		requestURL.RawQuery = query.Encode()

		var response serverListResponse
		if err := a.doJSON(ctx, http.MethodGet, requestURL.String(), nil, &response); err != nil {
			return nil, fmt.Errorf("list Hetzner servers: %w", err)
		}
		for _, item := range response.Servers {
			if item.Labels[managedByKey] != "true" || item.Labels[controllerIDKey] != a.controllerID {
				continue
			}
			workers = append(workers, toWorker(item))
		}
		if response.Meta.Pagination.NextPage == nil {
			break
		}
		page = *response.Meta.Pagination.NextPage
	}
	return workers, nil
}

func (a *Adapter) Destroy(ctx context.Context, workerID string) error {
	if workerID == "" {
		return nil
	}
	if _, err := strconv.ParseInt(workerID, 10, 64); err != nil {
		return fmt.Errorf("invalid Hetzner worker id %q", workerID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, a.baseURL+"/servers/"+url.PathEscape(workerID), nil)
	if err != nil {
		return fmt.Errorf("build delete request: %w", err)
	}
	a.setHeaders(req)
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete Hetzner worker %s: %w", workerID, err)
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

func toWorker(item server) provider.Worker {
	return provider.Worker{
		ID:         strconv.FormatInt(item.ID, 10),
		LeaseID:    item.Labels[leaseIDKey],
		RunnerName: item.Labels[runnerNameKey],
		State:      item.Status,
		CreatedAt:  item.CreatedAt,
	}
}

func renderCloudInit(lease provider.Lease, runnerImage string) (string, error) {
	leaseFile := "ACTIONS_RUNNER_INPUT_JITCONFIG=" + lease.JITConfig + "\n" +
		"RUNNERYARD_DEADLINE=" + lease.Deadline.UTC().Format(time.RFC3339) + "\n"
	bootstrap := `set -Eeuo pipefail
image=$1
runner_name=$2
lease_file=/etc/runneryard/lease.env
container=runneryard-worker
cleanup() {
  shred -u "$lease_file" 2>/dev/null || rm -f "$lease_file"
  docker rm --force "$container" >/dev/null 2>&1 || true
  shutdown -h now >/dev/null 2>&1 || true
}
trap cleanup EXIT
docker pull "$image"
docker create \
  --name "$container" \
  --hostname "$runner_name" \
  --privileged \
  --env-file "$lease_file" \
  --entrypoint /usr/bin/dumb-init \
  "$image" -- /usr/local/bin/runner-entrypoint >/dev/null
shred -u "$lease_file" 2>/dev/null || rm -f "$lease_file"
set +e
docker start --attach "$container"
runner_status=$?
set -e
exit "$runner_status"`
	cloudConfig := struct {
		WriteFiles []struct {
			Path        string `json:"path"`
			Owner       string `json:"owner"`
			Permissions string `json:"permissions"`
			Encoding    string `json:"encoding"`
			Content     string `json:"content"`
		} `json:"write_files"`
		RunCmd [][]string `json:"runcmd"`
	}{
		RunCmd: [][]string{{"bash", "-lc", bootstrap, "runneryard-bootstrap", runnerImage, lease.RunnerName}},
	}
	cloudConfig.WriteFiles = append(cloudConfig.WriteFiles, struct {
		Path        string `json:"path"`
		Owner       string `json:"owner"`
		Permissions string `json:"permissions"`
		Encoding    string `json:"encoding"`
		Content     string `json:"content"`
	}{
		Path: "/etc/runneryard/lease.env", Owner: "root:root", Permissions: "0600",
		Encoding: "b64", Content: base64.StdEncoding.EncodeToString([]byte(leaseFile)),
	})
	encoded, err := json.Marshal(cloudConfig)
	if err != nil {
		return "", err
	}
	return "#cloud-config\n" + string(encoded) + "\n", nil
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
		return errors.Join(fmt.Errorf("Hetzner API returned %s", resp.Status), readErr)
	}
	message := strings.TrimSpace(string(contents))
	if message == "" {
		return fmt.Errorf("Hetzner API returned %s", resp.Status)
	}
	return fmt.Errorf("Hetzner API returned %s: %s", resp.Status, message)
}

var _ provider.Compute = (*Adapter)(nil)
