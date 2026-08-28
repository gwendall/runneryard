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
	if !strings.Contains(string(contents), "--preserve-env=ACTIONS_RUNNER_INPUT_JITCONFIG,RUNNERYARD_DEADLINE,RUNNERYARD_DOCKER_DNS") {
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
