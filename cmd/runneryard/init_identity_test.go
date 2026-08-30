package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeriveControllerIDIsUniquePerTargetAndStable(t *testing.T) {
	widgets := deriveControllerID("runneryard-linux-x64", "https://github.com/acme/widgets")
	gadgets := deriveControllerID("runneryard-linux-x64", "https://github.com/acme/gadgets")
	if widgets == gadgets {
		t.Fatal("two repositories with the default scale-set name must get different controller identities")
	}
	if widgets != deriveControllerID("runneryard-linux-x64", "https://github.com/ACME/Widgets") {
		t.Fatal("the identity must not depend on GitHub URL casing")
	}
	if !strings.HasPrefix(widgets, "runneryard-linux-x64-") || len(widgets) != len("runneryard-linux-x64-")+8 {
		t.Fatalf("unexpected identity format %q", widgets)
	}
}

func TestInitWritesAUniqueControllerID(t *testing.T) {
	directory := t.TempDir()
	if err := runInit([]string{"--directory", directory, "--github", "https://github.com/acme/widgets"}); err != nil {
		t.Fatal(err)
	}
	expected := deriveControllerID("runneryard-linux-x64", "https://github.com/acme/widgets")
	toml, err := os.ReadFile(filepath.Join(directory, ".runneryard", "fly.controller.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(toml), `CONTROLLER_ID = "`+expected+`"`) {
		t.Fatalf("generated Fly TOML lacks the unique controller identity %q", expected)
	}
	env, err := os.ReadFile(filepath.Join(directory, ".runneryard", "controller.env.example"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), "CONTROLLER_ID") {
		t.Fatal("the secret template must not duplicate the controller identity")
	}
}
