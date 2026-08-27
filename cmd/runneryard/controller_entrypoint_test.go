package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestControllerEntrypointPreparesOnlyDedicatedBudgetVolume(t *testing.T) {
	entrypoint := filepath.Join("..", "..", "controller-entrypoint")
	contents, err := os.ReadFile(entrypoint)
	if err != nil {
		t.Fatal(err)
	}
	script := string(contents)
	if !strings.Contains(script, `== /var/lib/runneryard/*`) || !strings.Contains(script, "-o runner -g runner -m 0700") {
		t.Fatal("controller entrypoint must narrowly prepare the dedicated volume for the unprivileged process")
	}
	if output, err := exec.Command("bash", "-n", entrypoint).CombinedOutput(); err != nil {
		t.Fatalf("controller entrypoint syntax: %v: %s", err, output)
	}
}
