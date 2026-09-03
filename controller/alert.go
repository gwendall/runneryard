package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	alertQueueDepth     = 32
	alertRequestTimeout = 5 * time.Second
	// alertRepeatInterval bounds reminders while the same condition persists.
	alertRepeatInterval = time.Hour
)

// alerter pushes health transitions to an operator webhook. It never blocks
// the status path: events are queued and delivered by one goroutine, and a
// full queue drops the oldest reminder rather than the reporter.
type alerter struct {
	url        string
	client     *http.Client
	logger     *slog.Logger
	controller string
	scaleSet   string
	now        func() time.Time

	mu       sync.Mutex
	lastKey  string
	lastSent time.Time
	queue    chan string
	done     chan struct{}
}

func newAlerter(cfg Config) (*alerter, error) {
	target := strings.TrimSpace(cfg.AlertWebhookURL)
	if target == "" {
		return nil, nil
	}
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return nil, fmt.Errorf("alert webhook must be an http(s) URL")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &alerter{
		url:        target,
		client:     &http.Client{Timeout: alertRequestTimeout},
		logger:     logger.WithGroup("alert"),
		controller: cfg.ControllerID,
		scaleSet:   cfg.ScaleSetName,
		now:        time.Now,
		queue:      make(chan string, alertQueueDepth),
	}, nil
}

// observe records the latest derived status and queues a message on every
// health transition, plus one reminder per hour while a condition persists.
func (a *alerter) observe(status FleetStatus) {
	if a == nil {
		return
	}
	key := status.Health + ":" + status.Reason
	if status.Health == "starting" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	transition := key != a.lastKey
	reminder := !transition && status.Health == "degraded" && now.Sub(a.lastSent) >= alertRepeatInterval
	if !transition && !reminder {
		return
	}
	if transition && status.Health == "ready" && a.lastKey == "" {
		// First observation of a healthy controller is not an incident.
		a.lastKey, a.lastSent = key, now
		return
	}
	a.lastKey, a.lastSent = key, now
	a.enqueue(a.message(status, reminder))
}

func (a *alerter) enqueue(text string) {
	select {
	case a.queue <- text:
	default:
		// Drop the oldest queued message so the newest state wins.
		select {
		case <-a.queue:
		default:
		}
		select {
		case a.queue <- text:
		default:
		}
	}
}

func (a *alerter) message(status FleetStatus, reminder bool) string {
	var text strings.Builder
	fmt.Fprintf(&text, "RunnerYard %s (%s): %s", a.controller, a.scaleSet, status.Health)
	if status.Reason != "" {
		fmt.Fprintf(&text, " %s", status.Reason)
	}
	if reminder {
		text.WriteString(" (still)")
	}
	switch {
	case status.Reason == "usage_budget_exhausted":
		text.WriteString(". New jobs stay queued until the rolling window releases usage; raise RUNNER_USAGE_BUDGET to admit them.")
	case status.Reason == "provider_capacity_exhausted":
		fmt.Fprintf(&text, ". Provider capacity is %d of %d configured workers (%s); lower MAX_RUNNERS or raise the provider quota", status.Capacity.Effective, status.Capacity.Configured, status.Capacity.Rejection)
		if !status.Capacity.RetryAt.IsZero() {
			fmt.Fprintf(&text, "; the next bounded probe is after %s", status.Capacity.RetryAt.Format(time.RFC3339))
		}
		text.WriteString(".")
	case status.Reason == "provider_launch_rejected":
		fmt.Fprintf(&text, ". The provider rejected worker launches (%s); check the worker image, shape, region, and token permissions", status.Launch.Rejection)
		if !status.Launch.RetryAt.IsZero() {
			fmt.Fprintf(&text, "; the next bounded probe is after %s", status.Launch.RetryAt.Format(time.RFC3339))
		}
		text.WriteString(".")
	case status.Reason == "github_session_restarting":
		text.WriteString(". The GitHub scale-set session ended on a transport failure; the controller reopens it with bounded backoff and existing workers keep running.")
	case strings.HasPrefix(status.Reason, "provider_"):
		text.WriteString(". The provider is unavailable; the controller keeps retrying on every message.")
	case status.Reason == "orphan_candidates":
		text.WriteString(". Provider inventory holds workers the controller does not recognize; inspect them before deleting anything.")
	case status.Reason == "runner_retirements_pending":
		text.WriteString(". A worker's GitHub registration has stayed pending beyond the retirement grace; reconciliation keeps retrying it.")
	case status.Health == "ready":
		text.WriteString(". Recovered.")
	}
	fmt.Fprintf(&text, " Workers %d/%d", status.Workers.Actual, status.Workers.Maximum)
	if status.Budget.HorizonSeconds > 0 {
		fmt.Fprintf(&text, ", budget horizon %s", (time.Duration(status.Budget.HorizonSeconds) * time.Second).Round(time.Hour))
	}
	text.WriteString(".")
	return text.String()
}

// run delivers queued messages until ctx ends, then drains what is left with
// a short grace period.
func (a *alerter) run(ctx context.Context) {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.done != nil {
		a.mu.Unlock()
		return
	}
	a.done = make(chan struct{})
	done := a.done
	a.mu.Unlock()
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				a.drain()
				return
			case text := <-a.queue:
				a.deliver(context.WithoutCancel(ctx), text)
			}
		}
	}()
}

func (a *alerter) drain() {
	for {
		select {
		case text := <-a.queue:
			a.deliver(context.Background(), text)
		default:
			return
		}
	}
}

func (a *alerter) close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	done := a.done
	a.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (a *alerter) deliver(ctx context.Context, text string) {
	payload, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, alertRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url, bytes.NewReader(payload))
	if err != nil {
		a.logger.Error("failed to build alert request", "error", err)
		return
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		// The URL may carry a token; never log it.
		a.logger.Error("failed to deliver alert", "error", err.Error())
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		a.logger.Error("alert webhook rejected the message", "status", response.StatusCode)
	}
}
