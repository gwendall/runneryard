package controller

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"
	"github.com/gwendall/runneryard/provider"
)

// supervisedSession is a scale-set session whose first poll fails the way a
// GitHub transport failure does, or blocks until the context ends.
type supervisedSession struct {
	fakeSessionCloser
	pollErr error
	// onPoll runs once, on the first poll, so a test can end the session
	// exactly when it is known to be listening.
	onPoll func()
	polls  int
	mu     sync.Mutex
}

func (s *supervisedSession) GetMessage(ctx context.Context, _, _ int) (*scaleset.RunnerScaleSetMessage, error) {
	s.mu.Lock()
	s.polls++
	err := s.pollErr
	first := s.polls == 1
	onPoll := s.onPoll
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if first && onPoll != nil {
		onPoll()
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *supervisedSession) pollCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.polls
}

func (*supervisedSession) DeleteMessage(context.Context, int) error { return nil }

func (*supervisedSession) AcquireJobs(context.Context, []int64) ([]int64, error) { return nil, nil }

func (*supervisedSession) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{SessionID: uuid.New(), Statistics: &scaleset.RunnerScaleSetStatistic{}}
}

func supervisedScaler(t *testing.T, compute provider.Compute) (*scaler, string) {
	t.Helper()
	scaler := testScaler(t, newWorkerState(), compute)
	scaler.maxWorkers = 2
	statusFile := filepath.Join(t.TempDir(), "status.json")
	reporter, err := newStatusReporter(Config{
		ControllerID: "test", ScaleSetName: "test", Provider: "fly", MaxWorkers: 2,
		StatusFile: statusFile, Logger: slog.New(slog.DiscardHandler),
	}, BudgetStatus{})
	if err != nil {
		t.Fatal(err)
	}
	scaler.reporter = reporter
	return scaler, statusFile
}

func TestSupervisorReopensSessionAfterTransportFailureWithoutLosingWorkers(t *testing.T) {
	worker := provider.Worker{ID: "worker-one", LeaseID: "lease-one", RunnerName: "runner-00000001", CreatedAt: time.Now()}
	compute := &fakeCompute{workers: []provider.Worker{worker}}
	scaler, statusFile := supervisedScaler(t, compute)
	cfg := Config{MaxWorkers: 2, Logger: slog.New(slog.DiscardHandler)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make([]string, 0, 4)
	sessions := make([]*supervisedSession, 0, 2)
	var mu sync.Mutex
	open := func(context.Context) (messageSession, error) {
		mu.Lock()
		defer mu.Unlock()
		session := &supervisedSession{fakeSessionCloser: fakeSessionCloser{events: &events}}
		if len(sessions) == 0 {
			session.pollErr = errors.New("request GET .../message failed: connection reset by peer")
		} else {
			// The second session is healthy; end the test once it is polling.
			session.onPoll = cancel
		}
		sessions = append(sessions, session)
		return session, nil
	}

	err := superviseSessions(ctx, open, scaler, cfg, 1, time.Millisecond, 4*time.Millisecond)
	if err != nil {
		t.Fatalf("a transport failure must not end the controller: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sessions) != 2 {
		t.Fatalf("expected the session to be reopened once, got %d sessions", len(sessions))
	}
	if sessions[0].pollCount() != 1 || sessions[1].pollCount() != 1 {
		t.Fatalf("each session must be polled once: %d and %d", sessions[0].pollCount(), sessions[1].pollCount())
	}
	if len(events) != 2 || events[0] != "session" || events[1] != "session" {
		t.Fatalf("both sessions must be released, got %#v", events)
	}
	if scaler.state.count() != 1 {
		t.Fatalf("the recovered worker must survive the session restart, got %d workers", scaler.state.count())
	}
	if compute.launchCalls != 0 || len(compute.destroyed) != 0 {
		t.Fatalf("a session restart must neither launch nor destroy workers: launches=%d destroyed=%#v", compute.launchCalls, compute.destroyed)
	}
	status := loadFleetStatus(t, statusFile)
	if status.Health != "ready" || status.Reason == "github_session_restarting" {
		t.Fatalf("the reopened session must clear the restart condition: health=%s reason=%s", status.Health, status.Reason)
	}
}

func TestSupervisorEndsOnHandlerFailure(t *testing.T) {
	// A permanent inventory failure is a fail-closed handler error: the
	// supervisor must not reopen the session around it.
	compute := &fakeCompute{inventoryErr: errors.New("token rejected")}
	scaler, _ := supervisedScaler(t, compute)
	cfg := Config{MaxWorkers: 2, Logger: slog.New(slog.DiscardHandler)}
	opened := 0
	events := make([]string, 0, 1)
	open := func(context.Context) (messageSession, error) {
		opened++
		return &supervisedSession{fakeSessionCloser: fakeSessionCloser{events: &events}}, nil
	}
	err := superviseSessions(context.Background(), open, scaler, cfg, 1, time.Millisecond, time.Millisecond)
	if err == nil || !isHandlerFailure(err) {
		t.Fatalf("a handler failure must end the controller, got %v", err)
	}
	if opened != 1 {
		t.Fatalf("a handler failure must not reopen the session, got %d sessions", opened)
	}
	if len(events) != 1 || events[0] != "session" {
		t.Fatalf("the session must still be released: %#v", events)
	}
}

func TestSupervisorRetriesSessionCreationWithBoundedBackoff(t *testing.T) {
	compute := &fakeCompute{}
	scaler, _ := supervisedScaler(t, compute)
	cfg := Config{MaxWorkers: 2, Logger: slog.New(slog.DiscardHandler)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	events := make([]string, 0, 1)
	open := func(context.Context) (messageSession, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("request POST .../sessions failed: 503")
		}
		return &supervisedSession{fakeSessionCloser: fakeSessionCloser{events: &events}, onPoll: cancel}, nil
	}
	if err := superviseSessions(ctx, open, scaler, cfg, 1, time.Millisecond, 4*time.Millisecond); err != nil {
		t.Fatalf("session creation failures must be retried, got %v", err)
	}
	if attempts != 3 {
		t.Fatalf("expected two failed creations before the session, got %d attempts", attempts)
	}
	if len(events) != 1 {
		t.Fatalf("the session must be released on cancellation: %#v", events)
	}
}

func TestNextSessionBackoffDoublesToTheMaximum(t *testing.T) {
	backoff := nextSessionBackoff(0, 5*time.Second, 2*time.Minute)
	if backoff != 5*time.Second {
		t.Fatalf("first backoff = %s", backoff)
	}
	for range 10 {
		backoff = nextSessionBackoff(backoff, 5*time.Second, 2*time.Minute)
	}
	if backoff != 2*time.Minute {
		t.Fatalf("backoff must be capped at the maximum, got %s", backoff)
	}
}
