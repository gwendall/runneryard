package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type webhookStub struct {
	mu       sync.Mutex
	messages []string
}

func (w *webhookStub) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]string
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("alert payload must be JSON: %v", err)
		}
		w.mu.Lock()
		w.messages = append(w.messages, payload["text"])
		w.mu.Unlock()
		rw.WriteHeader(http.StatusOK)
	})
}

func (w *webhookStub) wait(t *testing.T, count int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		if len(w.messages) >= count {
			copied := append([]string(nil), w.messages...)
			w.mu.Unlock()
			return copied
		}
		w.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	t.Fatalf("expected %d alert(s), got %v", count, w.messages)
	return nil
}

func alertingReporter(t *testing.T, webhook string) *statusReporter {
	t.Helper()
	reporter, err := newStatusReporter(Config{
		ControllerID: "ci-controller", ScaleSetName: "ci-linux", Provider: "fly", MaxWorkers: 4,
		StatusFile: filepath.Join(t.TempDir(), "status.json"), AlertWebhookURL: webhook,
	}, BudgetStatus{})
	if err != nil {
		t.Fatal(err)
	}
	return reporter
}

func TestAlertsOnDegradedTransitionAndRecoveryOnly(t *testing.T) {
	stub := &webhookStub{}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	reporter := alertingReporter(t, server.URL)
	reporter.start(t.Context(), nil)
	defer reporter.close()

	reporter.githubActivity("session_created") // ready: first healthy observation is silent
	reporter.degraded("provider_launch_failed")
	reporter.degraded("provider_launch_failed") // repeated condition within the hour is silent
	messages := stub.wait(t, 1)
	if !strings.Contains(messages[0], "degraded provider_launch_failed") || !strings.Contains(messages[0], "ci-controller") {
		t.Fatalf("unexpected first alert %q", messages[0])
	}
	if strings.Contains(messages[0], server.URL) {
		t.Fatal("alerts must not echo the webhook URL")
	}
	reporter.recovered()
	messages = stub.wait(t, 2)
	if !strings.Contains(messages[1], "ready") || !strings.Contains(messages[1], "Recovered") {
		t.Fatalf("expected a recovery alert, got %q", messages[1])
	}
	time.Sleep(50 * time.Millisecond)
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.messages) != 2 {
		t.Fatalf("expected exactly two alerts, got %v", stub.messages)
	}
}

func TestAlertsRemindHourlyWhileDegraded(t *testing.T) {
	stub := &webhookStub{}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	reporter := alertingReporter(t, server.URL)
	clock := time.Now()
	reporter.alerter.now = func() time.Time { return clock }
	reporter.start(t.Context(), nil)
	defer reporter.close()

	reporter.budget(BudgetStatus{RefusalReason: "usage_budget_exhausted", HorizonSeconds: 7200})
	messages := stub.wait(t, 1)
	if !strings.Contains(messages[0], "usage_budget_exhausted") || !strings.Contains(messages[0], "RUNNER_USAGE_BUDGET") {
		t.Fatalf("unexpected budget alert %q", messages[0])
	}
	clock = clock.Add(30 * time.Minute)
	reporter.budget(BudgetStatus{RefusalReason: "usage_budget_exhausted"})
	clock = clock.Add(31 * time.Minute)
	reporter.budget(BudgetStatus{RefusalReason: "usage_budget_exhausted"})
	messages = stub.wait(t, 2)
	if !strings.Contains(messages[1], "(still)") {
		t.Fatalf("expected an hourly reminder, got %q", messages[1])
	}
}

func TestReporterWithoutWebhookNeverAlerts(t *testing.T) {
	reporter := alertingReporter(t, "")
	if reporter.alerter != nil {
		t.Fatal("no webhook must mean no alerter")
	}
	reporter.degraded("provider_inventory_failed")
	reporter.recovered()
}

func TestAlerterRejectsNonHTTPWebhook(t *testing.T) {
	if _, err := newAlerter(Config{AlertWebhookURL: "ftp://example.test/hook"}); err == nil {
		t.Fatal("expected a non-http webhook to be rejected")
	}
}
