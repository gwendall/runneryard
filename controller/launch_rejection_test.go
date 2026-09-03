package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/gwendall/runneryard/provider"
	"github.com/gwendall/runneryard/provider/retry"
)

// flyRejection mirrors what the Fly adapter returns for a permanent create
// failure that is not a capacity ceiling.
func flyRejection(status int, message string) error {
	return fmt.Errorf("create Fly worker runner-x: %w", &retry.StatusError{Provider: "fly", Status: status, Message: message})
}

func rejectionScaler(t *testing.T, launchErr error) (*scaler, *fakeCompute, string, *time.Time) {
	t.Helper()
	clock := time.Now().UTC()
	compute := &fakeCompute{launchErr: launchErr}
	scaler := testScaler(t, newWorkerState(), compute)
	scaler.maxWorkers = 3
	scaler.capacityNow = func() time.Time { return clock }
	statusFile := filepath.Join(t.TempDir(), "status.json")
	reporter, err := newStatusReporter(Config{
		ControllerID: "test", ScaleSetName: "test", Provider: "fly", MaxWorkers: 3,
		StatusFile: statusFile, Logger: slog.New(slog.DiscardHandler),
	}, BudgetStatus{})
	if err != nil {
		t.Fatal(err)
	}
	reporter.githubActivity("session_created")
	scaler.reporter = reporter
	client := scaler.scaleSetClient.(*fakeRunnerScaleSetClient)
	client.generateJIT = &scaleset.RunnerScaleSetJitRunnerConfig{
		Runner: &scaleset.RunnerReference{ID: 42}, EncodedJITConfig: "jit",
	}
	return scaler, compute, statusFile, &clock
}

func TestPermanentLaunchRejectionBacksOffWithoutStoppingTheListener(t *testing.T) {
	scaler, compute, statusFile, clock := rejectionScaler(t, flyRejection(422, `{"error":"unsupported machine shape"}`))
	client := scaler.scaleSetClient.(*fakeRunnerScaleSetClient)

	count, err := scaler.HandleDesiredRunnerCount(context.Background(), 2)
	if err != nil || count != 0 {
		t.Fatalf("a permanent launch rejection must keep the listener alive: count=%d err=%v", count, err)
	}
	status := loadFleetStatus(t, statusFile)
	if status.Health != "degraded" || status.Reason != "provider_launch_rejected" ||
		status.Launch.Rejections != 1 || status.Launch.Rejection != "fly_status_422" ||
		!status.Launch.RetryAt.Equal(clock.Add(capacityInitialBackoff)) {
		t.Fatalf("launch rejection status = %#v (reason %q)", status.Launch, status.Reason)
	}
	if status.Capacity.Rejection != "" {
		t.Fatalf("a launch rejection is not a capacity ceiling: %#v", status.Capacity)
	}
	if scaler.retirements.count() != 0 {
		t.Fatalf("the rejected launch must leave no pending retirement, got %d", scaler.retirements.count())
	}
	if len(client.removed) != 1 || client.removed[0] != 42 {
		t.Fatalf("the unused GitHub registration must be removed, got %v", client.removed)
	}
	if budget := scaler.budget.snapshot(time.Now()); budget.ReservedSeconds != 0 {
		t.Fatalf("a proven rejection must refund its launch reservation: %#v", budget)
	}

	if _, err := scaler.HandleDesiredRunnerCount(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if compute.launchCalls != 1 {
		t.Fatalf("desired-count messages inside the backoff must not hot-loop launches: %d", compute.launchCalls)
	}
	*clock = clock.Add(capacityInitialBackoff + time.Second)
	if _, err := scaler.HandleDesiredRunnerCount(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	status = loadFleetStatus(t, statusFile)
	if compute.launchCalls != 2 || status.Launch.Rejections != 2 ||
		!status.Launch.RetryAt.Equal(clock.Add(2*capacityInitialBackoff)) {
		t.Fatalf("a rejected probe must double the bounded backoff: calls=%d status=%#v", compute.launchCalls, status.Launch)
	}

	compute.launchErr = nil
	compute.launchWorker = provider.Worker{ID: "worker-one", CreatedAt: *clock}
	*clock = clock.Add(2*capacityInitialBackoff + time.Second)
	count, err = scaler.HandleDesiredRunnerCount(context.Background(), 2)
	if err != nil || count != 1 {
		t.Fatalf("the probe after the backoff must launch again: count=%d err=%v", count, err)
	}
	status = loadFleetStatus(t, statusFile)
	if status.Health != "ready" || status.Launch.Rejection != "" || status.Launch.Rejections != 2 {
		t.Fatalf("launch recovery status = %#v (health %s %s)", status.Launch, status.Health, status.Reason)
	}
}

func TestAuthorizationFailureOnLaunchStillFailsClosed(t *testing.T) {
	for _, code := range []int{401, 403} {
		scaler, compute, _, _ := rejectionScaler(t, flyRejection(code, `{"error":"unauthorized"}`))
		_, err := scaler.HandleDesiredRunnerCount(context.Background(), 1)
		if err == nil || !isHandlerFailure(err) {
			t.Fatalf("a %d from the provider must remain a fail-closed handler failure, got %v", code, err)
		}
		if compute.launchCalls != 1 {
			t.Fatalf("expected one launch attempt for %d, got %d", code, compute.launchCalls)
		}
	}
}

func TestClassifyLaunchFailureKeepsTransientAndCapacityErrors(t *testing.T) {
	transientErr := &provider.TransientError{Err: errors.New("503")}
	if classifyLaunchFailure(transientErr) != transientErr {
		t.Fatal("transient errors must pass through unchanged")
	}
	capacityErr := &provider.CapacityError{Reason: "fly_machine_limit", Err: errors.New("limit")}
	if classifyLaunchFailure(capacityErr) != capacityErr {
		t.Fatal("capacity errors must pass through unchanged")
	}
	if classifyLaunchFailure(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	generic := classifyLaunchFailure(errors.New("decode response: unexpected end of JSON input"))
	if launchRejectionReason(generic) != "provider_launch_rejected" {
		t.Fatalf("a permanent error without a status needs the generic code, got %q", launchRejectionReason(generic))
	}
}

func TestHandlerErrorsAreMarkedFailClosed(t *testing.T) {
	compute := &fakeCompute{inventoryErr: errors.New("token rejected")}
	scaler := testScaler(t, newWorkerState(), compute)
	_, err := scaler.HandleDesiredRunnerCount(context.Background(), 1)
	if !isHandlerFailure(err) {
		t.Fatalf("a permanent inventory failure must be a handler failure: %v", err)
	}
	if err.Error() != "inventory compute workers: token rejected" {
		t.Fatalf("the marker must not change the message: %q", err.Error())
	}
	if isHandlerFailure(errors.New("failed to get message: connection reset")) {
		t.Fatal("a transport error is not a handler failure")
	}
}
