package controller

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"
	"github.com/gwendall/runneryard/provider"
)

// blockingSession polls like a GitHub long poll with no message: it blocks
// until its context ends. It reports one assigned job.
type blockingSession struct {
	fakeSessionCloser
	reply chan *scaleset.RunnerScaleSetMessage
}

func (s *blockingSession) GetMessage(ctx context.Context, _, _ int) (*scaleset.RunnerScaleSetMessage, error) {
	select {
	case message := <-s.reply:
		return message, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (*blockingSession) DeleteMessage(context.Context, int) error { return nil }

func (*blockingSession) AcquireJobs(context.Context, []int64) ([]int64, error) { return nil, nil }

func (*blockingSession) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{SessionID: uuid.New(), Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}
}

func TestWakingSessionEndsAPollEarlyAsAnEmptyPoll(t *testing.T) {
	inner := &blockingSession{reply: make(chan *scaleset.RunnerScaleSetMessage, 1)}
	wake := make(chan struct{}, 1)
	session := wakingSession{messageSession: inner, wake: wake}

	inner.reply <- &scaleset.RunnerScaleSetMessage{MessageID: 7}
	if message, err := session.GetMessage(context.Background(), 0, 1); err != nil || message == nil || message.MessageID != 7 {
		t.Fatalf("a message must pass through: %#v %v", message, err)
	}

	wake <- struct{}{}
	done := make(chan error, 1)
	go func() {
		message, err := session.GetMessage(context.Background(), 7, 1)
		if message != nil {
			err = errors.New("unexpected message")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a wake-up must read as an empty poll: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a wake-up did not end the poll")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := session.GetMessage(ctx, 7, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("the listener's own cancellation must stay an error, got %v", err)
	}
}

func TestProbeTimerWakesOnceAndStopsWithTheGate(t *testing.T) {
	scaler := testScaler(t, newWorkerState(), &fakeCompute{})
	scaler.probeWake = make(chan struct{}, 1)

	scaler.capacityMu.Lock()
	scaler.scheduleProbeLocked(time.Millisecond)
	scaler.capacityMu.Unlock()
	select {
	case <-scaler.probeWake:
	case <-time.After(5 * time.Second):
		t.Fatal("the probe timer did not wake the listener")
	}

	scaler.capacityMu.Lock()
	scaler.scheduleProbeLocked(time.Hour)
	scaler.capacityMu.Unlock()
	scaler.recordLaunchGate(gateCapacity, "pool_full", &provider.CapacityError{Reason: "pool_full", RetryAfter: 10 * time.Second, Err: errors.New("no free slot")})
	scaler.capacityMu.Lock()
	timer := scaler.probeTimer
	scaler.capacityMu.Unlock()
	if timer == nil {
		t.Fatal("a closed gate must arm the probe timer")
	}
	scaler.clearLaunchGate()
	if scaler.probeTimer != nil {
		t.Fatal("a cleared gate must stop the probe timer")
	}
}

// signallingCompute refuses every launch with a hinted capacity error, as a
// pool does while a host starts, and reports each attempt.
type signallingCompute struct {
	mu       sync.Mutex
	workers  []provider.Worker
	launches chan struct{}
}

func (c *signallingCompute) Launch(context.Context, provider.Lease) (provider.Worker, error) {
	c.launches <- struct{}{}
	return provider.Worker{}, &provider.CapacityError{Reason: "pool_full", RetryAfter: 10 * time.Second, Err: errors.New("host starting")}
}

func (c *signallingCompute) Inventory(context.Context) ([]provider.Worker, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]provider.Worker(nil), c.workers...), nil
}

func (*signallingCompute) Destroy(context.Context, string) error { return nil }

func TestGateRetryTimeProbesWithoutWaitingForGitHub(t *testing.T) {
	compute := &signallingCompute{launches: make(chan struct{}, 4)}
	scaler, _ := supervisedScaler(t, compute)
	client := scaler.scaleSetClient.(*fakeRunnerScaleSetClient)
	client.generateJIT = &scaleset.RunnerScaleSetJitRunnerConfig{Runner: &scaleset.RunnerReference{ID: 42}, EncodedJITConfig: "jit"}
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	scaler.capacityNow = func() time.Time { return time.Unix(0, clock.Load()) }
	scaler.probeWake = make(chan struct{}, 1)
	session := &blockingSession{reply: make(chan *scaleset.RunnerScaleSetMessage)}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	// The session writes status into the test's directory until it returns.
	t.Cleanup(func() {
		cancel()
		<-finished
	})
	go func() {
		defer close(finished)
		done <- runSession(ctx, session, scaler, Config{MaxWorkers: 2, Logger: slog.New(slog.DiscardHandler)}, 1, false)
	}()
	waitLaunch := func(what string) {
		t.Helper()
		select {
		case <-compute.launches:
		case <-time.After(5 * time.Second):
			t.Fatalf("no launch attempt %s", what)
		}
	}
	waitLaunch("for the assigned job")
	for deadline := time.Now().Add(5 * time.Second); ; {
		scaler.capacityMu.Lock()
		closed := !scaler.capacityRetryAt.IsZero()
		scaler.capacityMu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the refused launch did not close the gate")
		}
		time.Sleep(time.Millisecond)
	}

	// GitHub sends nothing more. At the gate's retry time the timer wakes
	// the listener, which probes again on its own.
	clock.Add(int64(10 * time.Second))
	scaler.probeWake <- struct{}{}
	waitLaunch("at the retry time")

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("session: %v", err)
	}
}
