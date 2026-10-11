package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gwendall/runneryard/githubapp"
)

func trustServer(t *testing.T, answers ...string) (githubapp.API, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := int(calls.Add(1)) - 1
		if r.URL.Path != "/repos/acme/widgets" || r.Header.Get("Authorization") != "Bearer pat" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		answer := answers[min(call, len(answers)-1)]
		if answer == "error" {
			http.Error(w, "unavailable", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(server.Close)
	return githubapp.API{BaseURL: server.URL, Client: server.Client()}, &calls
}

const (
	privateRepository = `{"private":true,"visibility":"private"}`
	publicRepository  = `{"private":false,"visibility":"public"}`
)

func TestPoolTrustRefusesAPublicTargetAtStartup(t *testing.T) {
	cfg := appConfig{GitHubURL: "https://github.com/acme/widgets", GitHubToken: "pat"}
	api, _ := trustServer(t, privateRepository)
	if err := cfg.checkPoolTrust(context.Background(), api); err != nil {
		t.Fatalf("private repository refused: %v", err)
	}
	api, _ = trustServer(t, publicRepository)
	if err := cfg.checkPoolTrust(context.Background(), api); !errors.Is(err, githubapp.ErrPublicTarget) {
		t.Fatalf("public repository = %v, want ErrPublicTarget", err)
	}
	api, _ = trustServer(t, "error")
	if err := cfg.checkPoolTrust(context.Background(), api); err == nil || errors.Is(err, githubapp.ErrPublicTarget) {
		t.Fatalf("unavailable GitHub = %v, want a plain error that still refuses to start", err)
	}
	bad := appConfig{GitHubURL: "https://example.com/acme/widgets", GitHubToken: "pat"}
	if err := bad.checkPoolTrust(context.Background(), api); err == nil {
		t.Fatal("a non-GitHub target was accepted")
	}
}

func TestPoolTrustWatchStopsTheControllerWhenTheTargetTurnsPublic(t *testing.T) {
	cfg := appConfig{GitHubURL: "https://github.com/acme/widgets", GitHubToken: "pat"}
	api, calls := trustServer(t, privateRepository, "error", publicRepository)
	ctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)
	done := make(chan struct{})
	go func() {
		cfg.watchPoolTrust(ctx, api, 5*time.Millisecond, stop, slog.New(slog.DiscardHandler))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not stop on a public target")
	}
	if cause := context.Cause(ctx); !errors.Is(cause, githubapp.ErrPublicTarget) {
		t.Fatalf("stop cause = %v, want ErrPublicTarget", cause)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("checks = %d, want a failed check to be retried, not to stop the controller", got)
	}
}

func TestPoolTrustWatchEndsWithTheController(t *testing.T) {
	cfg := appConfig{GitHubURL: "https://github.com/acme/widgets", GitHubToken: "pat"}
	api, _ := trustServer(t, privateRepository)
	ctx, stop := context.WithCancelCause(context.Background())
	done := make(chan struct{})
	go func() {
		cfg.watchPoolTrust(ctx, api, time.Hour, stop, slog.New(slog.DiscardHandler))
		close(done)
	}()
	stop(nil)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch outlived the controller")
	}
}
