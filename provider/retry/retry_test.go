package retry

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/gwendall/runneryard/provider"
)

func fastPolicy(attempts int) Policy {
	return Policy{Attempts: attempts, InitialBackoff: time.Millisecond, MaxBackoff: 4 * time.Millisecond, Rate: 10000, Burst: 10000}
}

func TestDoRetriesTransientFailuresUntilSuccess(t *testing.T) {
	retryer := New(fastPolicy(4))
	calls := 0
	err := retryer.Do(context.Background(), func(context.Context) error {
		calls++
		if calls < 3 {
			return Transient(errors.New("provider busy"))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts, got %d", calls)
	}
}

func TestDoStopsOnPermanentFailure(t *testing.T) {
	retryer := New(fastPolicy(4))
	calls := 0
	permanent := errors.New("unauthorized")
	err := retryer.Do(context.Background(), func(context.Context) error {
		calls++
		return permanent
	})
	if !errors.Is(err, permanent) || provider.IsTransient(err) {
		t.Fatalf("expected the permanent error unchanged, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("permanent failures must not be retried, got %d attempts", calls)
	}
}

func TestDoReportsExhaustedRetriesAsTransient(t *testing.T) {
	retryer := New(fastPolicy(3))
	calls := 0
	err := retryer.Do(context.Background(), func(context.Context) error {
		calls++
		return Transient(errors.New("still down"))
	})
	if !provider.IsTransient(err) {
		t.Fatalf("exhausted transient retries must stay transient, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts, got %d", calls)
	}
}

func TestDoHonorsCanceledContextBetweenAttempts(t *testing.T) {
	retryer := New(Policy{Attempts: 5, InitialBackoff: time.Second, MaxBackoff: time.Second, Rate: 10000, Burst: 10000})
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := retryer.Do(ctx, func(context.Context) error {
		calls++
		cancel()
		return Transient(errors.New("down"))
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected one attempt before cancellation, got %d", calls)
	}
}

func TestBackoffDoublesWithBoundedJitter(t *testing.T) {
	retryer := New(Policy{Attempts: 5, InitialBackoff: 100 * time.Millisecond, MaxBackoff: 350 * time.Millisecond, Rate: 1, Burst: 1})
	retryer.random = func() float64 { return 0.5 }
	expected := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 350 * time.Millisecond, 350 * time.Millisecond}
	for i, want := range expected {
		if got := retryer.Backoff(i + 1); got != want {
			t.Fatalf("attempt %d: expected %s, got %s", i+1, want, got)
		}
	}
	retryer.random = func() float64 { return 0 }
	if got := retryer.Backoff(1); got != 75*time.Millisecond {
		t.Fatalf("expected the low jitter bound, got %s", got)
	}
}

func TestLimiterPacesBeyondBurst(t *testing.T) {
	limiter := NewLimiter(10, 2)
	now := time.Unix(0, 0)
	limiter.now = func() time.Time { return now }
	var slept []time.Duration
	limiter.sleep = func(_ context.Context, delay time.Duration) error {
		slept = append(slept, delay)
		now = now.Add(delay)
		return nil
	}
	ctx := context.Background()
	for range 2 {
		if err := limiter.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(slept) != 0 {
		t.Fatalf("burst requests must not sleep, got %v", slept)
	}
	if err := limiter.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 || slept[0] != 100*time.Millisecond {
		t.Fatalf("expected one 100ms pause for the third request, got %v", slept)
	}
}

func TestLimiterDisabledWhenRateIsZero(t *testing.T) {
	limiter := NewLimiter(0, 1)
	for range 1000 {
		if err := limiter.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClassifyStatusMarksThrottlingAndServerErrorsTransient(t *testing.T) {
	for _, status := range []int{429, 500, 502, 503, 504} {
		if !provider.IsTransient(ClassifyStatus("fly", status, "")) {
			t.Fatalf("status %d must be transient", status)
		}
	}
	for _, status := range []int{400, 401, 403, 404, 409, 422} {
		err := ClassifyStatus("fly", status, "no")
		if provider.IsTransient(err) {
			t.Fatalf("status %d must be permanent", status)
		}
		var statusErr *StatusError
		if !errors.As(err, &statusErr) || statusErr.Status != status {
			t.Fatalf("expected a StatusError carrying %d, got %v", status, err)
		}
	}
	if got := ClassifyStatus("fly", http.StatusBadRequest, "bad input").Error(); got != "fly API returned 400: bad input" {
		t.Fatalf("unexpected message %q", got)
	}
}

func TestClassifyRequestErrorKeepsCancellationAndWrapsTransport(t *testing.T) {
	if err := ClassifyRequestError(context.Canceled); !errors.Is(err, context.Canceled) || provider.IsTransient(err) {
		t.Fatalf("cancellation must pass through unchanged, got %v", err)
	}
	transport := &url.Error{Op: "Get", URL: "https://api.example.test", Err: errors.New("connection reset by peer")}
	if err := ClassifyRequestError(transport); !provider.IsTransient(err) {
		t.Fatalf("transport failures must be transient, got %v", err)
	}
	canceled := &url.Error{Op: "Get", URL: "https://api.example.test", Err: context.Canceled}
	if err := ClassifyRequestError(canceled); provider.IsTransient(err) {
		t.Fatalf("a canceled request must not be transient, got %v", err)
	}
	if err := ClassifyRequestError(nil); err != nil {
		t.Fatalf("nil must stay nil, got %v", err)
	}
}

func TestTransientDoesNotDoubleWrap(t *testing.T) {
	inner := Transient(errors.New("down"))
	if Transient(inner) != inner {
		t.Fatal("an existing transient error must be returned unchanged")
	}
	if Transient(nil) != nil {
		t.Fatal("nil must stay nil")
	}
}
