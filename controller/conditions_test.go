package controller

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gwendall/runneryard/provider"
)

type conditionCompute struct {
	reportingCompute
	conditions []provider.Condition
}

func (c *conditionCompute) Conditions() []provider.Condition {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]provider.Condition(nil), c.conditions...)
}

func (c *conditionCompute) set(conditions ...provider.Condition) {
	c.mu.Lock()
	c.conditions = conditions
	c.mu.Unlock()
}

var _ provider.ConditionReporter = (*conditionCompute)(nil)

// fakeNow is a clock the test moves by hand.
type fakeNow struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeNow) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeNow) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) count(text string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buffer.String(), text)
}

func hostDown(since time.Time) provider.Condition {
	return provider.Condition{
		Key: "pool/warm/host_unreachable/h1", Summary: "pool warm host h1 has not answered for more than 2m",
		Action: "Check the host.", Since: since,
	}
}

func poolFull(since time.Time) provider.Condition {
	return provider.Condition{
		Key: "pool/warm/pool_overflowing", Summary: "pool warm was full for 30 of 40 launches in the last hour",
		Action: "Raise max_hosts.", Since: since,
	}
}

func TestComputeConditionsAlertOnChangeWithAnHourlyReminder(t *testing.T) {
	stub := &webhookStub{}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	reporter := alertingReporter(t, server.URL)
	clock := &fakeNow{now: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
	reporter.now = clock.Now
	compute := &conditionCompute{reportingCompute: reportingCompute{state: map[string]any{}}}
	reporter.watchCompute(compute)
	reporter.start(t.Context(), nil)
	defer reporter.close()

	start := clock.Now()
	compute.set(hostDown(start))
	reporter.sampleCompute()
	messages := stub.wait(t, 1)
	if messages[0] != "RunnerYard ci-controller (ci-linux): pool warm host h1 has not answered for more than 2m. Check the host." {
		t.Fatalf("first alert = %q", messages[0])
	}

	// The same condition within the hour is silent.
	for range 5 {
		clock.advance(10 * time.Minute)
		reporter.sampleCompute()
	}
	clock.advance(11 * time.Minute)
	reporter.sampleCompute()
	messages = stub.wait(t, 2)
	if messages[1] != "RunnerYard ci-controller (ci-linux): pool warm host h1 has not answered for more than 2m (still, for 1h01m). Check the host." {
		t.Fatalf("reminder = %q", messages[1])
	}

	// One sample's changes go out in one message.
	clock.advance(9 * time.Minute)
	compute.set(poolFull(clock.Now()))
	reporter.sampleCompute()
	messages = stub.wait(t, 3)
	want := "RunnerYard ci-controller (ci-linux): 2 compute conditions changed:\n" +
		"- pool warm was full for 30 of 40 launches in the last hour. Raise max_hosts.\n" +
		"- cleared after 1h10m: pool warm host h1 has not answered for more than 2m."
	if messages[2] != want {
		t.Fatalf("change alert = %q, want %q", messages[2], want)
	}

	clock.advance(30 * time.Second)
	compute.set()
	reporter.sampleCompute()
	reporter.sampleCompute()
	messages = stub.wait(t, 4)
	if messages[3] != "RunnerYard ci-controller (ci-linux): cleared after 30s: pool warm was full for 30 of 40 launches in the last hour." {
		t.Fatalf("recovery alert = %q", messages[3])
	}
	time.Sleep(100 * time.Millisecond)
	if messages := stub.wait(t, 4); len(messages) != 4 {
		t.Fatalf("alerts = %q, want exactly four", messages)
	}
}

func TestComputeConditionsAreOnlyLoggedWithoutAWebhook(t *testing.T) {
	logs := &lockedBuffer{}
	reporter, err := newStatusReporter(Config{
		ControllerID: "ci-controller", ScaleSetName: "ci-linux", Provider: "pool", MaxWorkers: 4,
		StatusFile: filepath.Join(t.TempDir(), "status.json"), Logger: slog.New(slog.NewJSONHandler(logs, nil)),
	}, BudgetStatus{})
	if err != nil {
		t.Fatal(err)
	}
	if reporter.alerter != nil {
		t.Fatal("an alerter without a webhook")
	}
	clock := &fakeNow{now: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
	reporter.now = clock.Now
	compute := &conditionCompute{reportingCompute: reportingCompute{state: map[string]any{}}}
	compute.set(hostDown(clock.Now()))
	reporter.watchCompute(compute)
	for range 3 {
		clock.advance(time.Hour)
		reporter.sampleCompute()
	}
	compute.set()
	reporter.sampleCompute()
	reporter.sampleCompute()
	if appeared, cleared := logs.count(`"msg":"compute condition"`), logs.count(`"msg":"compute condition cleared"`); appeared != 1 || cleared != 1 {
		t.Fatalf("logged %d appearances and %d clearances, want one each:\n%s", appeared, cleared, logs.buffer.String())
	}
	if logs.count(`"condition":"pool/warm/host_unreachable/h1"`) != 2 || logs.count(`"lasted":"3h00m"`) != 1 {
		t.Fatalf("log lines lack the condition or its duration:\n%s", logs.buffer.String())
	}
}

func TestComputeConditionAlertIsBounded(t *testing.T) {
	stub := &webhookStub{}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	reporter := alertingReporter(t, server.URL)
	compute := &conditionCompute{reportingCompute: reportingCompute{state: map[string]any{}}}
	reporter.watchCompute(compute)
	reporter.start(t.Context(), nil)
	defer reporter.close()

	conditions := make([]provider.Condition, 0, 15)
	for i := range 15 {
		conditions = append(conditions, provider.Condition{
			Key: fmt.Sprintf("pool/warm/host_unreachable/h%02d", i), Summary: fmt.Sprintf("host h%02d is silent", i), Since: time.Now(),
		})
	}
	// A repeated key and a condition without a key are ignored.
	conditions = append(conditions, conditions[0], provider.Condition{Summary: "no key"})
	compute.set(conditions...)
	reporter.sampleCompute()
	message := stub.wait(t, 1)[0]
	if !strings.HasPrefix(message, "RunnerYard ci-controller (ci-linux): 15 compute conditions changed:\n- host h00 is silent.\n") ||
		strings.Count(message, "\n- ") != conditionLinesPerAlert+1 || !strings.HasSuffix(message, "\n- and 5 more; see runneryard status.") ||
		strings.Contains(message, "no key") {
		t.Fatalf("bounded alert = %q", message)
	}
}
