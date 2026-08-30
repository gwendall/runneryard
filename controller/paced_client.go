package controller

import (
	"context"

	"github.com/actions/scaleset"
	"github.com/gwendall/runneryard/provider/retry"
)

// pacedScaleSetClient spreads the controller's own GitHub API calls so a burst
// of launches or retirements stays under GitHub's request limits. The scale
// set client already retries transport failures; this layer only paces.
type pacedScaleSetClient struct {
	inner   runnerScaleSetClient
	limiter *retry.Limiter
}

func newPacedScaleSetClient(inner runnerScaleSetClient, rate float64, burst int) runnerScaleSetClient {
	if rate <= 0 {
		return inner
	}
	return &pacedScaleSetClient{inner: inner, limiter: retry.NewLimiter(rate, burst)}
}

func (c *pacedScaleSetClient) GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	return c.inner.GenerateJitRunnerConfig(ctx, setting, scaleSetID)
}

func (c *pacedScaleSetClient) GetRunnerByName(ctx context.Context, name string) (*scaleset.RunnerReference, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	return c.inner.GetRunnerByName(ctx, name)
}

func (c *pacedScaleSetClient) RemoveRunner(ctx context.Context, id int64) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	return c.inner.RemoveRunner(ctx, id)
}
