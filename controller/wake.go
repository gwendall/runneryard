package controller

import (
	"context"

	"github.com/actions/scaleset"
)

// wakingSession ends the listener's long poll early when the scaler's launch
// gate reaches its retry time, and reports it as an empty poll. The listener
// answers an empty poll with HandleDesiredRunnerCount on its own goroutine,
// so the probe runs then instead of at GitHub's next message: with a pool
// host starting, the first job no longer waits up to a minute after the host
// is ready. Nothing is lost by cutting a poll short: GitHub returns a message
// again until it is deleted, and only the listener deletes one, after
// handling it.
type wakingSession struct {
	messageSession
	wake <-chan struct{}
}

func (s wakingSession) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	if s.wake == nil {
		return s.messageSession.GetMessage(ctx, lastMessageID, maxCapacity)
	}
	pollCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type poll struct {
		message *scaleset.RunnerScaleSetMessage
		err     error
	}
	done := make(chan poll, 1)
	go func() {
		message, err := s.messageSession.GetMessage(pollCtx, lastMessageID, maxCapacity)
		done <- poll{message, err}
	}()
	select {
	case result := <-done:
		return result.message, result.err
	case <-s.wake:
	}
	cancel()
	result := <-done
	if result.err != nil && ctx.Err() == nil {
		// The poll failed because this wake-up cancelled it, or failed for
		// another reason at the same moment; the next poll reports the latter.
		return nil, nil
	}
	return result.message, result.err
}
