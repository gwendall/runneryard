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
	"github.com/gwendall/runneryard/provider/retry"
)

const (
	managedByKey         = "runneryard-managed-by"
	controllerIDKey      = "runneryard-controller"
	leaseIDKey           = "runneryard-lease-id"
	runnerNameKey        = "runneryard-runner-name"
	runnerIDKey          = "runneryard-runner-id"
	runnerScaleSetIDKey  = "runneryard-runner-scale-set-id"
	defaultHTTPTimeout   = 30 * time.Second
	defaultActionTimeout = 3 * time.Minute
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
	// ActionPollInterval controls how often asynchronous Hetzner actions are
	// checked. It is primarily configurable so tests do not have to sleep.
	ActionPollInterval time.Duration
	// ActionTimeout bounds the complete provider-side qualification sequence.
	ActionTimeout time.Duration
	// Retry bounds repeated attempts and paces requests to the Cloud API.
	// Zero fields take the retry package defaults.
	Retry retry.Policy
}

type Adapter struct {
	baseURL            string
	token              string
	location           string
	serverType         string
	serverImage        string
	runnerImage        string
	controllerID       string
	firewallID         int64
	networkID          int64
	httpClient         *http.Client
	actionPollInterval time.Duration
	actionTimeout      time.Duration
	retryer            *retry.Retryer
}

func New(cfg Config) (*Adapter, error) {
	if cfg.APIToken == "" || cfg.Location == "" || cfg.ServerType == "" || cfg.ServerImage == "" || cfg.RunnerImage == "" || cfg.ControllerID == "" {
		return nil, fmt.Errorf("hetzner token, location, server type, server image, runner image, and controller ID are required")
	}
	if cfg.FirewallID < 1 {
		return nil, fmt.Errorf("hetzner worker firewall ID is required")
	}
	if !safeLabelValue.MatchString(cfg.ControllerID) {
		return nil, fmt.Errorf("hetzner controller ID must be a valid label value of at most 63 characters")
	}
	if cfg.NetworkID < 0 {
		return nil, fmt.Errorf("hetzner network ID cannot be negative")
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = "https://api.hetzner.cloud/v1"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if cfg.ActionPollInterval <= 0 {
		cfg.ActionPollInterval = 500 * time.Millisecond
	}
	if cfg.ActionTimeout <= 0 {
		cfg.ActionTimeout = defaultActionTimeout
	}
	return &Adapter{
		baseURL:            strings.TrimRight(cfg.APIBaseURL, "/"),
		token:              cfg.APIToken,
		location:           cfg.Location,
		serverType:         cfg.ServerType,
		serverImage:        cfg.ServerImage,
		runnerImage:        cfg.RunnerImage,
		controllerID:       cfg.ControllerID,
		firewallID:         cfg.FirewallID,
		networkID:          cfg.NetworkID,
		httpClient:         cfg.HTTPClient,
		actionPollInterval: cfg.ActionPollInterval,
		actionTimeout:      cfg.ActionTimeout,
		retryer:            retry.New(cfg.Retry),
	}, nil
}

type server struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	CreatedAt time.Time         `json:"created"`
	Labels    map[string]string `json:"labels"`
	PublicNet struct {
		Firewalls []serverFirewall `json:"firewalls"`
	} `json:"public_net"`
}

type serverFirewall struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

type action struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
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
	Server      server   `json:"server"`
	Action      action   `json:"action"`
	NextActions []action `json:"next_actions"`
}

type serverResponse struct {
	Server server `json:"server"`
}

type actionResponse struct {
	Action action `json:"action"`
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
		StartAfterCreate: false,
		Labels: map[string]string{
			managedByKey:        "true",
			controllerIDKey:     a.controllerID,
			leaseIDKey:          lease.ID,
			runnerNameKey:       lease.RunnerName,
			runnerIDKey:         strconv.FormatInt(lease.RunnerID, 10),
			runnerScaleSetIDKey: strconv.Itoa(lease.RunnerScaleSetID),
		},
		Firewalls: []createServerFirewall{{Firewall: a.firewallID}},
		PublicNet: createServerPublicNet{EnableIPv4: true, EnableIPv6: true},
	}
	if a.networkID > 0 {
		payload.Networks = []int64{a.networkID}
	}

	created, err := a.createServer(ctx, lease, payload)
	if err != nil {
		return provider.Worker{}, err
	}
	worker := withLeaseProof(toWorker(created.Server), lease)
	qualifyCtx, cancelQualification := context.WithTimeout(ctx, a.actionTimeout)
	defer cancelQualification()
	actions := append([]action{created.Action}, created.NextActions...)
	if err := a.waitActions(qualifyCtx, actions); err != nil {
		return provider.Worker{}, &provider.PartialLaunchError{Worker: worker, Err: fmt.Errorf("qualify Hetzner worker: %w", err)}
	}
	var qualified serverResponse
	if err := a.doJSON(qualifyCtx, http.MethodGet, a.baseURL+"/servers/"+strconv.FormatInt(created.Server.ID, 10), nil, &qualified); err != nil {
		return provider.Worker{}, &provider.PartialLaunchError{Worker: worker, Err: fmt.Errorf("verify Hetzner worker firewall: %w", err)}
	}
	if !firewallApplied(qualified.Server, a.firewallID) {
		return provider.Worker{}, &provider.PartialLaunchError{
			Worker: worker,
			Err:    fmt.Errorf("required firewall %d is not applied", a.firewallID),
		}
	}
	var poweredOn actionResponse
	if err := a.doJSON(qualifyCtx, http.MethodPost, a.baseURL+"/servers/"+strconv.FormatInt(created.Server.ID, 10)+"/actions/poweron", nil, &poweredOn); err != nil {
		return provider.Worker{}, &provider.PartialLaunchError{Worker: worker, Err: fmt.Errorf("power on qualified Hetzner worker: %w", err)}
	}
	if err := a.waitActions(qualifyCtx, []action{poweredOn.Action}); err != nil {
		return provider.Worker{}, &provider.PartialLaunchError{Worker: worker, Err: fmt.Errorf("power on qualified Hetzner worker: %w", err)}
	}
	var running serverResponse
	if err := a.doJSON(qualifyCtx, http.MethodGet, a.baseURL+"/servers/"+strconv.FormatInt(created.Server.ID, 10), nil, &running); err != nil {
		return provider.Worker{}, &provider.PartialLaunchError{Worker: worker, Err: fmt.Errorf("confirm powered-on Hetzner worker: %w", err)}
	}
	if running.Server.Status != "running" {
		return provider.Worker{}, &provider.PartialLaunchError{
			Worker: worker,
			Err:    fmt.Errorf("powered-on Hetzner worker has status %q", running.Server.Status),
		}
	}
	return withLeaseProof(toWorker(running.Server), lease), nil
}

// createServer repeats the create request only after the lease selector proves
// the previous attempt did not produce a server. Two servers for one lease
// would share a single-use JIT configuration.
func (a *Adapter) createServer(ctx context.Context, lease provider.Lease, payload createServerRequest) (createServerResponse, error) {
	var lastErr error
	attempts := a.retryer.Policy().Attempts
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := a.retryer.Wait(ctx); err != nil {
			return createServerResponse{}, err
		}
		var created createServerResponse
		err := a.doJSON(ctx, http.MethodPost, a.baseURL+"/servers", payload, &created)
		if err == nil {
			if created.Server.ID == 0 {
				return createServerResponse{}, fmt.Errorf("create Hetzner worker %s: response did not include a server id", lease.RunnerName)
			}
			return created, nil
		}
		if created.Server.ID != 0 {
			return createServerResponse{}, &provider.PartialLaunchError{Worker: withLeaseProof(toWorker(created.Server), lease), Err: err}
		}
		lastErr = fmt.Errorf("create Hetzner worker %s: %w", lease.RunnerName, err)
		if !provider.IsTransient(err) {
			return createServerResponse{}, lastErr
		}
		existing, lookupErr := a.findByLease(ctx, lease.ID)
		if lookupErr != nil {
			return createServerResponse{}, errors.Join(lastErr, fmt.Errorf("confirm Hetzner worker absence before retry: %w", lookupErr))
		}
		if existing != nil {
			// The server exists but its qualification did not complete; report
			// it as partial so the core cleans it up and launches again.
			return createServerResponse{}, &provider.PartialLaunchError{Worker: withLeaseProof(*existing, lease), Err: lastErr}
		}
		if attempt == attempts {
			break
		}
		if err := sleepContext(ctx, a.retryer.Backoff(attempt)); err != nil {
			return createServerResponse{}, errors.Join(lastErr, err)
		}
	}
	return createServerResponse{}, lastErr
}

// findByLease returns the owned server carrying the lease, or nil.
func (a *Adapter) findByLease(ctx context.Context, leaseID string) (*provider.Worker, error) {
	if !safeLabelValue.MatchString(leaseID) {
		return nil, fmt.Errorf("lease id %q is not a valid label value", leaseID)
	}
	workers, err := a.listServers(ctx, managedByKey+"=true,"+controllerIDKey+"="+a.controllerID+","+leaseIDKey+"="+leaseID)
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
	return a.listServers(ctx, managedByKey+"=true,"+controllerIDKey+"="+a.controllerID)
}

func (a *Adapter) listServers(ctx context.Context, labelSelector string) ([]provider.Worker, error) {
	workers := make([]provider.Worker, 0)
	page := 1
	for {
		requestURL, err := url.Parse(a.baseURL + "/servers")
		if err != nil {
			return nil, fmt.Errorf("build Hetzner inventory URL: %w", err)
		}
		query := requestURL.Query()
		query.Set("label_selector", labelSelector)
		query.Set("page", strconv.Itoa(page))
		query.Set("per_page", "50")
		requestURL.RawQuery = query.Encode()

		var response serverListResponse
		err = a.retryer.Do(ctx, func(ctx context.Context) error {
			response = serverListResponse{}
			return a.doJSON(ctx, http.MethodGet, requestURL.String(), nil, &response)
		})
		if err != nil {
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
	var deleted actionResponse
	err := a.retryer.Do(ctx, func(ctx context.Context) error {
		deleted = actionResponse{}
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, a.baseURL+"/servers/"+url.PathEscape(workerID), nil)
		if err != nil {
			return fmt.Errorf("build delete request: %w", err)
		}
		a.setHeaders(req)
		resp, err := a.httpClient.Do(req)
		if err != nil {
			return retry.ClassifyRequestError(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent {
			return nil
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return responseError(resp)
		}
		if err := json.NewDecoder(resp.Body).Decode(&deleted); err != nil {
			return fmt.Errorf("decode Hetzner delete action: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete Hetzner worker %s: %w", workerID, err)
	}
	if deleted.Action.ID == 0 {
		return nil
	}
	actionCtx, cancelAction := context.WithTimeout(ctx, a.actionTimeout)
	defer cancelAction()
	if err := a.waitActions(actionCtx, []action{deleted.Action}); err != nil {
		return fmt.Errorf("delete Hetzner worker %s: %w", workerID, err)
	}
	return nil
}

func firewallApplied(item server, firewallID int64) bool {
	for _, firewall := range item.PublicNet.Firewalls {
		if firewall.ID == firewallID && firewall.Status == "applied" {
			return true
		}
	}
	return false
}

func (a *Adapter) waitActions(ctx context.Context, actions []action) error {
	for _, current := range actions {
		if current.ID < 1 {
			return fmt.Errorf("hetzner response did not include an action id")
		}
		if err := a.waitAction(ctx, current); err != nil {
			return err
		}
	}
	return nil
}

func (a *Adapter) waitAction(ctx context.Context, current action) error {
	for {
		switch current.Status {
		case "success":
			return nil
		case "error":
			if current.Error != nil {
				return fmt.Errorf("hetzner action %d failed (%s): %s", current.ID, current.Error.Code, current.Error.Message)
			}
			return fmt.Errorf("hetzner action %d failed", current.ID)
		case "running", "":
		default:
			return fmt.Errorf("hetzner action %d has unknown status %q", current.ID, current.Status)
		}

		timer := time.NewTimer(a.actionPollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}

		var response actionResponse
		err := a.retryer.Do(ctx, func(ctx context.Context) error {
			response = actionResponse{}
			return a.doJSON(ctx, http.MethodGet, a.baseURL+"/actions/"+strconv.FormatInt(current.ID, 10), nil, &response)
		})
		if err != nil {
			return fmt.Errorf("poll Hetzner action %d: %w", current.ID, err)
		}
		current = response.Action
	}
}

func toWorker(item server) provider.Worker {
	runnerID, _ := strconv.ParseInt(item.Labels[runnerIDKey], 10, 64)
	runnerScaleSetID, _ := strconv.Atoi(item.Labels[runnerScaleSetIDKey])
	return provider.Worker{
		ID:               strconv.FormatInt(item.ID, 10),
		LeaseID:          item.Labels[leaseIDKey],
		RunnerName:       item.Labels[runnerNameKey],
		RunnerID:         runnerID,
		RunnerScaleSetID: runnerScaleSetID,
		State:            item.Status,
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
// errors are transient; every other status is a permanent failure.
func responseError(resp *http.Response) error {
	contents, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr != nil {
		return errors.Join(retry.ClassifyStatus("hetzner", resp.StatusCode, ""), readErr)
	}
	return retry.ClassifyStatus("hetzner", resp.StatusCode, strings.TrimSpace(string(contents)))
}

var _ provider.Compute = (*Adapter)(nil)
