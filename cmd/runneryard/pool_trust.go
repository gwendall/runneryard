package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gwendall/runneryard/githubapp"
)

const (
	poolTrustTimeout  = 30 * time.Second
	poolTrustInterval = time.Hour
)

// checkPoolTrust confirms with GitHub that the fleet only serves private
// code before any pool host runs a job. The pools file's "trust: private" is
// the operator's word; this is GitHub's.
func (c appConfig) checkPoolTrust(ctx context.Context, api githubapp.API) error {
	ctx, cancel := context.WithTimeout(ctx, poolTrustTimeout)
	defer cancel()
	canonical, owner, err := normalizeGitHubConfigURL(c.GitHubURL)
	if err != nil {
		return fmt.Errorf("GITHUB_CONFIG_URL %w", err)
	}
	target := githubapp.Target{ConfigURL: canonical, Owner: owner}
	if _, repository, ok := strings.Cut(strings.TrimPrefix(canonical, "https://github.com/"), "/"); ok {
		target.Repository = repository
	}
	token := c.GitHubToken
	if token == "" {
		token, err = api.InstallationToken(ctx, githubapp.Credentials{
			ClientID: c.GitHubApp.ClientID, InstallationID: c.GitHubApp.InstallationID, PrivateKey: c.GitHubApp.PrivateKey,
		})
		if err != nil {
			return err
		}
	}
	if err := api.CheckPrivateTarget(ctx, token, target, c.RunnerGroup); err != nil {
		return fmt.Errorf("pool hosts refused: %w", err)
	}
	return nil
}

// watchPoolTrust checks again every interval and stops the controller as soon
// as GitHub says the target can run public code. A failed check is only
// logged: the answer was not definite.
func (c appConfig) watchPoolTrust(ctx context.Context, api githubapp.API, interval time.Duration, stop context.CancelCauseFunc, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		err := c.checkPoolTrust(ctx, api)
		switch {
		case err == nil:
		case errors.Is(err, githubapp.ErrPublicTarget):
			logger.Error("the GitHub target can now run public code; stopping the controller", "error", err)
			stop(err)
			return
		default:
			logger.Warn("could not confirm that the GitHub target is still private", "error", err)
		}
	}
}
