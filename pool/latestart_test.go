package pool

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gwendall/runneryard/provider"
)

// errLostAnswer is how the SSH agent reports a start the controller stopped
// waiting for.
var errLostAnswer = errors.New("host agent a: start interrupted: context deadline exceeded")

func runningSlot(controllerID, leaseID string) func(int, StartRequest) Slot {
	return func(slot int, request StartRequest) Slot {
		return Slot{
			Index: slot, ControllerID: controllerID, LeaseID: leaseID, RunnerName: "runner-" + leaseID,
			RunnerID: request.RunnerID, RunnerScaleSetID: request.RunnerScaleSetID, State: "started",
		}
	}
}

func lateStartHarness(t *testing.T) (*harness, *bytes.Buffer) {
	t.Helper()
	h := newHarness(t, ownedFacts, 0, 0)
	logs := &bytes.Buffer{}
	h.pool.log = slog.New(slog.NewTextHandler(logs, nil))
	h.readyHost("a", 2, h.clock.Now().Add(-time.Hour))
	h.step(t, time.Second)
	h.agent.startErr["a"] = errLostAnswer
	return h, logs
}

func TestLateStartOfThisLeaseIsKeptAsALaunch(t *testing.T) {
	h, logs := lateStartHarness(t)
	h.agent.late["a"] = runningSlot("fleet", "l1")

	worker, err := h.pool.Launch(context.Background(), lease("l1"))
	if err != nil {
		t.Fatalf("a start that answered late must be kept: %v", err)
	}
	if worker.ID != "a/0/l1" || worker.LeaseID != "l1" || worker.RunnerName != "runner-l1" {
		t.Fatalf("late worker = %+v", worker)
	}
	status := h.pool.Status()
	if status.Hosts[0].Busy != 1 || status.Launched != 1 {
		t.Fatalf("the late slot must count as one launch: %+v launched=%d", status.Hosts[0], status.Launched)
	}
	if !strings.Contains(logs.String(), `level=INFO msg="pool slot answered late; keeping its runner"`) || strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("unexpected late start log:\n%s", logs.String())
	}
	// The next lease takes the other slot, not the one just adopted.
	h.agent.startErr["a"] = nil
	next, err := h.pool.Launch(context.Background(), lease("l2"))
	if err != nil || next.ID != "a/1/l2" {
		t.Fatalf("next launch = %+v, %v; want slot 1", next, err)
	}
}

func TestLateStartIsReadAgainWhileTheContainerStarts(t *testing.T) {
	h, _ := lateStartHarness(t)
	h.agent.late["a"] = func(slot int, request StartRequest) Slot {
		started := runningSlot("fleet", "l1")(slot, request)
		started.State = "starting"
		return started
	}
	base := h.agent.listCount()
	h.agent.onList = func(slots map[int]Slot) {
		// Runs under the agent's lock: the second read-back sees it running.
		if current, ok := slots[0]; ok && current.State == "starting" && h.agent.lists >= base+2 {
			current.State = "started"
			slots[0] = current
		}
	}

	worker, err := h.pool.Launch(context.Background(), lease("l1"))
	if err != nil || worker.ID != "a/0/l1" {
		t.Fatalf("a container still starting must be read again: %+v, %v", worker, err)
	}
}

func TestLateStartOfAnotherLeaseOrControllerIsNotAdopted(t *testing.T) {
	for name, late := range map[string]func(int, StartRequest) Slot{
		"another lease":      runningSlot("fleet", "other"),
		"another controller": runningSlot("other-fleet", "l1"),
	} {
		t.Run(name, func(t *testing.T) {
			h, logs := lateStartHarness(t)
			h.agent.late["a"] = late
			lists := h.agent.listCount()

			_, err := h.pool.Launch(context.Background(), lease("l1"))
			var partial *provider.PartialLaunchError
			if !errors.As(err, &partial) || partial.Worker.ID != "a/0/l1" || !errors.Is(err, errLostAnswer) {
				t.Fatalf("launch = %v, want today's partial launch for a/0/l1", err)
			}
			if read := h.agent.listCount() - lists; read != 1 {
				t.Fatalf("a slot holding something else must end the read-back at once: %d listings", read)
			}
			if status := h.pool.Status(); status.Hosts[0].Busy != 1 || status.Launched != 0 {
				t.Fatalf("the slot must stay held and not count as launched: %+v launched=%d", status.Hosts[0], status.Launched)
			}
			if strings.Contains(logs.String(), "answered late") {
				t.Fatalf("a foreign container must not be adopted:\n%s", logs.String())
			}
		})
	}
}

func TestLateStartListingFailureKeepsTheAmbiguousStart(t *testing.T) {
	h, _ := lateStartHarness(t)
	h.agent.late["a"] = runningSlot("fleet", "l1")
	h.agent.listErr = errors.New("host agent a: list interrupted")

	_, err := h.pool.Launch(context.Background(), lease("l1"))
	var partial *provider.PartialLaunchError
	if !errors.As(err, &partial) || partial.Worker.ID != "a/0/l1" || !errors.Is(err, errLostAnswer) {
		t.Fatalf("launch = %v, want today's partial launch for a/0/l1", err)
	}
	if status := h.pool.Status(); status.Hosts[0].Busy != 1 || status.Launched != 0 {
		t.Fatalf("the slot must stay held as suspect: %+v launched=%d", status.Hosts[0], status.Launched)
	}
	// The core removes the worker by lease, which frees the slot.
	h.agent.listErr = nil
	if err := h.pool.Destroy(context.Background(), partial.Worker.ID); err != nil {
		t.Fatal(err)
	}
	if status := h.pool.Status(); status.Hosts[0].Busy != 0 {
		t.Fatalf("slot still busy after destroy: %+v", status.Hosts[0])
	}
}

func TestAnsweredStartFailureIsReadOnce(t *testing.T) {
	h, _ := lateStartHarness(t)
	h.agent.startErr["a"] = &AgentError{Code: CodeDockerFailed, Message: "docker run failed"}
	h.agent.late["a"] = func(slot int, request StartRequest) Slot {
		stopped := runningSlot("fleet", "l1")(slot, request)
		stopped.State = "stopped"
		return stopped
	}
	lists := h.agent.listCount()

	_, err := h.pool.Launch(context.Background(), lease("l1"))
	var partial *provider.PartialLaunchError
	if !errors.As(err, &partial) {
		t.Fatalf("launch = %v, want a partial launch", err)
	}
	if read := h.agent.listCount() - lists; read != 1 {
		t.Fatalf("an agent that answered needs one listing, got %d", read)
	}
}
