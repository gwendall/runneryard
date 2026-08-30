// Package retry provides the bounded retry, backoff, and rate-limiting
// primitives shared by compute adapters. Adapters decide which operations are
// safe to repeat; this package only paces and classifies them.
package retry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"sync"
	"syscall"
	"time"

	"github.com/gwendall/runneryard/provider"
)

const (
	DefaultAttempts       = 5
	DefaultInitialBackoff = 500 * time.Millisecond
	DefaultMaxBackoff     = 8 * time.Second
	DefaultRate           = 5.0
	DefaultBurst          = 10
)

// Policy bounds how often an adapter repeats a transient failure and how many
// requests per second it sends to its provider.
type Policy struct {
	// Attempts is the total number of tries, including the first one.
	Attempts int
	// InitialBackoff is the delay after the first failure; it doubles on every
	// further failure up to MaxBackoff, with jitter.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// Rate is the sustained request rate per second; Burst is the token bucket
	// depth. A zero Rate disables pacing.
	Rate  float64
	Burst int
}

func (p Policy) withDefaults() Policy {
	if p.Attempts < 1 {
		p.Attempts = DefaultAttempts
	}
	if p.InitialBackoff <= 0 {
		p.InitialBackoff = DefaultInitialBackoff
	}
	if p.MaxBackoff < p.InitialBackoff {
		p.MaxBackoff = max(DefaultMaxBackoff, p.InitialBackoff)
	}
	if p.Rate < 0 {
		p.Rate = 0
	}
	if p.Burst < 1 {
		p.Burst = DefaultBurst
	}
	return p
}

// Retryer paces and repeats provider calls according to one Policy.
type Retryer struct {
	policy  Policy
	limiter *Limiter
	sleep   func(context.Context, time.Duration) error
	random  func() float64
}

// New returns a Retryer for the policy. Zero policy fields take defaults.
func New(policy Policy) *Retryer {
	policy = policy.withDefaults()
	return &Retryer{
		policy:  policy,
		limiter: NewLimiter(policy.Rate, policy.Burst),
		sleep:   sleepContext,
		random:  rand.Float64,
	}
}

// Policy returns the effective policy after defaults.
func (r *Retryer) Policy() Policy { return r.policy }

// Wait blocks until the rate limiter admits one request.
func (r *Retryer) Wait(ctx context.Context) error { return r.limiter.Wait(ctx) }

// Do runs operation until it succeeds, fails with a non-transient error, the
// context ends, or the attempt budget is exhausted. Every attempt is paced by
// the rate limiter. The last error is returned unchanged, so an exhausted
// transient failure still satisfies provider.IsTransient.
func (r *Retryer) Do(ctx context.Context, operation func(context.Context) error) error {
	var last error
	for attempt := 1; attempt <= r.policy.Attempts; attempt++ {
		if err := r.limiter.Wait(ctx); err != nil {
			return err
		}
		last = operation(ctx)
		if last == nil || !provider.IsTransient(last) {
			return last
		}
		if attempt == r.policy.Attempts {
			break
		}
		if err := r.sleep(ctx, r.Backoff(attempt)); err != nil {
			return errors.Join(last, err)
		}
	}
	return last
}

// Backoff returns the jittered delay after the given failed attempt (1-based).
func (r *Retryer) Backoff(attempt int) time.Duration {
	delay := r.policy.InitialBackoff
	for i := 1; i < attempt && delay < r.policy.MaxBackoff; i++ {
		delay *= 2
	}
	delay = min(delay, r.policy.MaxBackoff)
	// Jitter between 75% and 125% of the nominal delay.
	jitter := 0.75 + r.random()*0.5
	return time.Duration(float64(delay) * jitter)
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Limiter is a token bucket. It has no external dependency and is safe for
// concurrent use.
type Limiter struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error
}

// NewLimiter returns a bucket that refills at rate tokens per second up to
// burst tokens. A rate of zero or less disables pacing.
func NewLimiter(rate float64, burst int) *Limiter {
	if burst < 1 {
		burst = 1
	}
	return &Limiter{rate: rate, burst: float64(burst), tokens: float64(burst), now: time.Now, sleep: sleepContext}
}

// Wait consumes one token, sleeping until it is available.
func (l *Limiter) Wait(ctx context.Context) error {
	if l == nil || l.rate <= 0 {
		return ctx.Err()
	}
	l.mu.Lock()
	now := l.now()
	if !l.last.IsZero() {
		l.tokens = min(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.rate)
	}
	l.last = now
	if l.tokens >= 1 {
		l.tokens--
		l.mu.Unlock()
		return ctx.Err()
	}
	wait := time.Duration((1 - l.tokens) / l.rate * float64(time.Second))
	l.tokens = 0
	l.mu.Unlock()
	return l.sleep(ctx, wait)
}

// Transient wraps err as a provider.TransientError unless it already is one or
// is nil.
func Transient(err error) error {
	if err == nil || provider.IsTransient(err) {
		return err
	}
	return &provider.TransientError{Err: err}
}

// RetryableStatus reports whether an HTTP status indicates a failure the
// provider may recover from on its own: throttling or a server-side error.
func RetryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// ClassifyRequestError wraps transport-level failures from http.Client.Do as
// transient. A canceled or expired context is returned as is so callers stop.
func ClassifyRequestError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if errors.Is(urlErr.Err, context.Canceled) {
			return err
		}
		return Transient(err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) {
		return Transient(err)
	}
	return err
}

// StatusError describes a non-2xx provider response with its status code so
// adapters can decide idempotent follow-ups.
type StatusError struct {
	Provider string
	Status   int
	Message  string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s API returned %d", e.Provider, e.Status)
	}
	return fmt.Sprintf("%s API returned %d: %s", e.Provider, e.Status, e.Message)
}

// ClassifyStatus builds the error for a non-2xx response, marking retryable
// statuses as transient.
func ClassifyStatus(providerName string, status int, message string) error {
	err := &StatusError{Provider: providerName, Status: status, Message: message}
	if RetryableStatus(status) {
		return Transient(err)
	}
	return err
}
