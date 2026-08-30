package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunnerEntrypointPreservesCompleteLeaseAcrossPrivilegeEscalation(t *testing.T) {
	entrypoint := filepath.Join("..", "..", "runner-entrypoint")
	contents, err := os.ReadFile(entrypoint)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "--preserve-env=ACTIONS_RUNNER_INPUT_JITCONFIG,RUNNERYARD_DEADLINE,RUNNERYARD_DOCKER_DNS,RUNNERYARD_DIAG_HOLD,RUNNERYARD_IDLE_TIMEOUT") {
		t.Fatal("sudo must preserve the JIT configuration, lease deadline, and provider Docker DNS policy")
	}
	if output, err := exec.Command("bash", "-n", entrypoint).CombinedOutput(); err != nil {
		t.Fatalf("runner entrypoint syntax: %v: %s", err, output)
	}
}

func TestRunnerEntrypointValidatesDockerDNSBeforeStartingDaemon(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "runner-entrypoint"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(contents)
	for _, expected := range []string{"RUNNERYARD_DOCKER_DNS", "ipaddress.ip_address", `config["dns"] = dns`} {
		if !strings.Contains(source, expected) {
			t.Fatalf("runner entrypoint is missing Docker DNS guard %q", expected)
		}
	}
	if strings.Index(source, "ipaddress.ip_address") > strings.Index(source, "dockerd --config-file") {
		t.Fatal("Docker DNS must be validated before dockerd starts")
	}
}

func TestRunnerEntrypointNormalizesUnprivilegedEnvironment(t *testing.T) {
	entrypoint := filepath.Join("..", "..", "runner-entrypoint")
	contents, err := os.ReadFile(entrypoint)
	if err != nil {
		t.Fatal(err)
	}
	want := "env HOME=/home/runner USER=runner LOGNAME=runner"
	if !strings.Contains(string(contents), want) {
		t.Fatal("job environment must not inherit root's home after privilege escalation")
	}
	if !strings.Contains(string(contents), "XDG_CONFIG_HOME=/home/runner/.config") {
		t.Fatal("job tools must resolve configuration beneath the runner home")
	}
}

func TestRunnerEntrypointStartsRunnerBeforeWaitingForDocker(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "runner-entrypoint"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(contents)
	runnerStart := strings.Index(source, "/home/runner/run.sh &")
	dockerWait := strings.Index(source, `wait "$docker_ready_pid"`)
	if runnerStart < 0 || dockerWait < 0 || runnerStart > dockerWait {
		t.Fatal("the runner must start before the entrypoint waits for Docker readiness")
	}
	if !strings.Contains(source, "docker_ready &") {
		t.Fatal("Docker readiness must be checked in the background")
	}
	for _, guard := range []string{`refusing Docker storage driver vfs`, `return 78`, `kill -TERM "$runner_pid"`} {
		if !strings.Contains(source, guard) {
			t.Fatalf("Docker readiness must still fail closed; missing %q", guard)
		}
	}
}

func TestRunnerEntrypointPrintsDiagnosticsAndHoldsBeforeFailing(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "runner-entrypoint"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(contents)
	for _, expected := range []string{"/home/runner/_diag/*.log", "/var/log/dockerd.log", `RUNNERYARD_DIAG_HOLD:-30`, `sleep "$diag_hold_seconds"`, `fail "$runner_status"`} {
		if !strings.Contains(source, expected) {
			t.Fatalf("runner entrypoint is missing failure diagnostics %q", expected)
		}
	}
}

func TestRunnerEntrypointReleasesIdleWorkersThroughTheJobStartedHook(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "runner-entrypoint"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(contents)
	for _, expected := range []string{
		`RUNNERYARD_IDLE_TIMEOUT:-600`,
		`ACTIONS_RUNNER_HOOK_JOB_STARTED="$job_started_hook"`,
		`job_started_hook=/usr/local/bin/runneryard-job-started.sh`,
		`install -d -o runner -g runner -m 0755 "$marker_dir"`,
		`idle_watchdog &`,
		`kill -TERM "$runner_pid"`,
		`idle worker released before any job; exiting cleanly`,
	} {
		if !strings.Contains(source, expected) {
			t.Fatalf("runner entrypoint is missing idle watchdog piece %q", expected)
		}
	}
	hook, err := os.ReadFile(filepath.Join("..", "..", "runneryard-job-started.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hook), `: > "$marker_dir/job-started"`) {
		t.Fatal("job-started hook must record the assignment marker")
	}
	if output, err := exec.Command("bash", "-n", filepath.Join("..", "..", "runneryard-job-started.sh")).CombinedOutput(); err != nil {
		t.Fatalf("hook syntax: %v: %s", err, output)
	}
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerfile), "COPY runneryard-job-started.sh /usr/local/bin/runneryard-job-started.sh") {
		t.Fatal("runtime image must ship the job-started hook")
	}
}
