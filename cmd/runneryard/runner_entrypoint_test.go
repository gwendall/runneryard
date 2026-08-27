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
	if !strings.Contains(string(contents), "--preserve-env=ACTIONS_RUNNER_INPUT_JITCONFIG,RUNNERYARD_DEADLINE") {
		t.Fatal("sudo must preserve the JIT configuration and lease deadline")
	}
	if output, err := exec.Command("bash", "-n", entrypoint).CombinedOutput(); err != nil {
		t.Fatalf("runner entrypoint syntax: %v: %s", err, output)
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
