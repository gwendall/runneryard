package main

import (
	"errors"
	"strings"
	"testing"
)

const committedControllerConfig = `{
  "app": "control",
  "primary_region": "cdg",
  "build": {"image": "ghcr.io/gwendall/runneryard:1.2.3"},
  "env": {
    "COMPUTE_PROVIDER": "fly",
    "CONTROLLER_ID": "fleet",
    "MAX_RUNNERS": "24",
    "RUNNER_IMAGE": "ghcr.io/gwendall/runneryard:1.2.3",
    "RUNNER_STATUS_FILE": "/var/lib/runneryard/status.json"
  },
  "mounts": [{"source": "runneryard_state", "destination": "/var/lib/runneryard"}],
  "restart": [{"policy": "always"}]
}`

const matchingControllerMachines = `[
  {"id": "old", "state": "destroyed", "config": {"image": "ghcr.io/gwendall/runneryard:1.0.0", "env": {"MAX_RUNNERS": "2"}}},
  {"id": "m1", "state": "started", "config": {
    "image": "ghcr.io/gwendall/runneryard:1.2.3",
    "env": {
      "COMPUTE_PROVIDER": "fly",
      "CONTROLLER_ID": "fleet",
      "FLY_PROCESS_GROUP": "app",
      "PRIMARY_REGION": "cdg",
      "MAX_RUNNERS": "24",
      "RUNNER_IMAGE": "ghcr.io/gwendall/runneryard:1.2.3",
      "RUNNER_STATUS_FILE": "/var/lib/runneryard/status.json"
    },
    "restart": {"policy": "always"},
    "mounts": [{"volume": "vol_1", "path": "/var/lib/runneryard"}]
  }}
]`

func flyDoctorRunner(configJSON, machinesJSON string) commandRunner {
	return func(name string, args ...string) ([]byte, error) {
		if name != "fly" || len(args) == 0 {
			return []byte("ready"), nil
		}
		switch args[0] {
		case "secrets":
			return []byte(`[]`), nil
		case "config":
			return []byte(configJSON), nil
		case "machine":
			return []byte(machinesJSON), nil
		}
		return []byte("ready"), nil
	}
}

func doctorDetails(checks []doctorCheck, name string) string {
	for _, check := range checks {
		if check.Name == name {
			return check.Details
		}
	}
	return ""
}

func TestDoctorAcceptsControllerThatMatchesCommittedConfig(t *testing.T) {
	checks := doctor("fly", "control", "workers", "", ".runneryard/fly.controller.toml", flyDoctorRunner(committedControllerConfig, matchingControllerMachines))
	for _, name := range []string{"controller configuration", "controller image pin", "controller drift"} {
		if !hasDoctorStatus(checks, name, "pass") {
			t.Fatalf("%s should pass: %#v", name, checks)
		}
	}
	if strings.Contains(doctorDetails(checks, "controller drift"), "FLY_PROCESS_GROUP") {
		t.Fatal("Fly-injected variables must not count as drift")
	}
	if hasDoctorStatus(checks, "controller instances", "fail") {
		t.Fatal("destroyed Machines must not count as controller instances")
	}
}

func TestDoctorReportsControllerDrift(t *testing.T) {
	drifted := strings.NewReplacer(
		`"MAX_RUNNERS": "24"`, `"MAX_RUNNERS": "60", "RUNNER_CPUS": "4"`,
		`"restart": {"policy": "always"}`, `"restart": {"policy": "on-failure"}`,
		`"image": "ghcr.io/gwendall/runneryard:1.2.3",
    "env"`, `"image": "ghcr.io/gwendall/runneryard:1.2.2",
    "env"`,
	).Replace(matchingControllerMachines)
	checks := doctor("fly", "control", "workers", "", ".runneryard/fly.controller.toml", flyDoctorRunner(committedControllerConfig, drifted))
	if !hasDoctorStatus(checks, "controller drift", "fail") {
		t.Fatalf("checks = %#v", checks)
	}
	details := doctorDetails(checks, "controller drift")
	for _, expected := range []string{
		"m1: image is ghcr.io/gwendall/runneryard:1.2.2, file pins ghcr.io/gwendall/runneryard:1.2.3",
		"MAX_RUNNERS is 60 on the Machine, 24 in the file",
		"RUNNER_CPUS=4 is set on the Machine only",
		"restart policy is on-failure, file sets always",
	} {
		if !strings.Contains(details, expected) {
			t.Fatalf("drift details %q lack %q", details, expected)
		}
	}
	if strings.Contains(details, "\n") {
		t.Fatal("doctor output should be one line")
	}
}

func TestDoctorReportsValuesMissingFromTheMachine(t *testing.T) {
	missing := strings.Replace(matchingControllerMachines, `"RUNNER_STATUS_FILE": "/var/lib/runneryard/status.json"
    },`, `"RUNNER_STATUS_FILE": "/var/lib/runneryard/status.json"
    },`, 1)
	missing = strings.Replace(missing, `"mounts": [{"volume": "vol_1", "path": "/var/lib/runneryard"}]`, `"mounts": []`, 1)
	missing = strings.Replace(missing, `"CONTROLLER_ID": "fleet",
      "FLY_PROCESS_GROUP"`, `"FLY_PROCESS_GROUP"`, 1)
	checks := doctor("fly", "control", "workers", "", "fleet.toml", flyDoctorRunner(committedControllerConfig, missing))
	details := doctorDetails(checks, "controller drift")
	for _, expected := range []string{"CONTROLLER_ID=fleet is set in the file only", "mounts are none, file sets /var/lib/runneryard"} {
		if !strings.Contains(details, expected) {
			t.Fatalf("drift details %q lack %q", details, expected)
		}
	}
}

func TestDoctorBoundsDriftDetails(t *testing.T) {
	extra := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		extra = append(extra, `"EXTRA_`+string(rune('A'+i))+`": "1"`)
	}
	noisy := strings.Replace(matchingControllerMachines, `"FLY_PROCESS_GROUP": "app",`, strings.Join(extra, ",\n")+`, "FLY_PROCESS_GROUP": "app",`, 1)
	checks := doctor("fly", "control", "workers", "", "fleet.toml", flyDoctorRunner(committedControllerConfig, noisy))
	details := doctorDetails(checks, "controller drift")
	if !strings.Contains(details, "and 4 more") || strings.Count(details, "EXTRA_") != maxDriftDetails {
		t.Fatalf("drift details should be bounded: %q", details)
	}
}

func TestDoctorRejectsConfigForAnotherApp(t *testing.T) {
	checks := doctor("fly", "other-control", "workers", "", "fleet.toml", flyDoctorRunner(committedControllerConfig, matchingControllerMachines))
	if !hasDoctorStatus(checks, "controller configuration", "fail") {
		t.Fatalf("checks = %#v", checks)
	}
	if doctorDetails(checks, "controller drift") != "" {
		t.Fatal("a file for another app must not be compared with this controller")
	}
}

func TestDoctorRejectsDivergentImagePins(t *testing.T) {
	divergent := strings.Replace(committedControllerConfig, `"RUNNER_IMAGE": "ghcr.io/gwendall/runneryard:1.2.3"`, `"RUNNER_IMAGE": "ghcr.io/gwendall/runneryard:1.2.4"`, 1)
	checks := doctor("fly", "control", "workers", "", "fleet.toml", flyDoctorRunner(divergent, matchingControllerMachines))
	if !hasDoctorStatus(checks, "controller image pin", "fail") {
		t.Fatalf("checks = %#v", checks)
	}
}

// A derived worker image (docs/derived-images.md): RUNNER_IMAGE names an
// image built FROM the controller release, and RUNNER_IMAGE_BASE declares
// that release. The file and the live Machine carry both, so drift stays
// comparable.
func derivedWorkerImageConfig(declaredBase string) (string, string) {
	file := strings.Replace(committedControllerConfig,
		`"RUNNER_IMAGE": "ghcr.io/gwendall/runneryard:1.2.3",`,
		`"RUNNER_IMAGE": "registry.fly.io/workers:current", "RUNNER_IMAGE_BASE": "`+declaredBase+`",`, 1)
	machines := strings.Replace(matchingControllerMachines,
		`"RUNNER_IMAGE": "ghcr.io/gwendall/runneryard:1.2.3",`,
		`"RUNNER_IMAGE": "registry.fly.io/workers:current", "RUNNER_IMAGE_BASE": "`+declaredBase+`",`, 1)
	return file, machines
}

func TestDoctorAcceptsDerivedWorkerImageDeclaringItsBase(t *testing.T) {
	file, machines := derivedWorkerImageConfig("ghcr.io/gwendall/runneryard:1.2.3")
	checks := doctor("fly", "control", "workers", "", "fleet.toml", flyDoctorRunner(file, machines))
	if !hasDoctorStatus(checks, "controller image pin", "pass") {
		t.Fatalf("checks = %#v", checks)
	}
	if details := doctorDetails(checks, "controller image pin"); !strings.Contains(details, "registry.fly.io/workers:current") || !strings.Contains(details, "derived") {
		t.Fatalf("the pass must name the worker image and say it is derived: %q", details)
	}
	if !hasDoctorStatus(checks, "controller drift", "pass") {
		t.Fatalf("a declared base is policy like any other key and must compare clean: %#v", checks)
	}
}

func TestDoctorRejectsDerivedWorkerImageWithForeignBase(t *testing.T) {
	file, machines := derivedWorkerImageConfig("ghcr.io/gwendall/runneryard:1.2.4")
	checks := doctor("fly", "control", "workers", "", "fleet.toml", flyDoctorRunner(file, machines))
	if !hasDoctorStatus(checks, "controller image pin", "fail") {
		t.Fatalf("a base that is not the controller release must fail: %#v", checks)
	}
}

func TestDoctorRejectsWorkerImageWithoutDeclaredBase(t *testing.T) {
	file := strings.Replace(committedControllerConfig, `"RUNNER_IMAGE": "ghcr.io/gwendall/runneryard:1.2.3"`, `"RUNNER_IMAGE": "registry.fly.io/workers:current"`, 1)
	checks := doctor("fly", "control", "workers", "", "fleet.toml", flyDoctorRunner(file, matchingControllerMachines))
	if !hasDoctorStatus(checks, "controller image pin", "fail") {
		t.Fatalf("checks = %#v", checks)
	}
	if details := doctorDetails(checks, "controller image pin"); !strings.Contains(details, "RUNNER_IMAGE_BASE") {
		t.Fatalf("the failure must name the knob that declares a derived image: %q", details)
	}
}

func TestDoctorWarnsWhenImagePinIsAbsent(t *testing.T) {
	unpinned := strings.Replace(committedControllerConfig, `"build": {"image": "ghcr.io/gwendall/runneryard:1.2.3"},`, "", 1)
	checks := doctor("fly", "control", "workers", "", "fleet.toml", flyDoctorRunner(unpinned, matchingControllerMachines))
	if !hasDoctorStatus(checks, "controller image pin", "warn") {
		t.Fatalf("checks = %#v", checks)
	}
	if !hasDoctorStatus(checks, "controller drift", "pass") {
		t.Fatalf("an unpinned file must still compare environment and mounts: %#v", checks)
	}
}

func TestDoctorFlagsMultipleControllers(t *testing.T) {
	two := strings.Replace(matchingControllerMachines, `{"id": "m1", "state": "started",`, `{"id": "m0", "state": "stopped", "config": {"image": "ghcr.io/gwendall/runneryard:1.2.3", "env": {"COMPUTE_PROVIDER": "fly", "CONTROLLER_ID": "fleet", "MAX_RUNNERS": "24", "RUNNER_IMAGE": "ghcr.io/gwendall/runneryard:1.2.3", "RUNNER_STATUS_FILE": "/var/lib/runneryard/status.json"}, "restart": {"policy": "always"}, "mounts": [{"path": "/var/lib/runneryard"}]}},
  {"id": "m1", "state": "started",`, 1)
	checks := doctor("fly", "control", "workers", "", "fleet.toml", flyDoctorRunner(committedControllerConfig, two))
	if !hasDoctorStatus(checks, "controller instances", "fail") {
		t.Fatalf("checks = %#v", checks)
	}
}

func TestDoctorWarnsWithoutCommittedConfig(t *testing.T) {
	checks := doctor("fly", "control", "workers", "", "", flyDoctorRunner(committedControllerConfig, matchingControllerMachines))
	if !hasDoctorStatus(checks, "controller configuration", "warn") {
		t.Fatalf("checks = %#v", checks)
	}
}

func TestDoctorWarnsWhenNoControllerMachineExists(t *testing.T) {
	checks := doctor("fly", "control", "workers", "", "fleet.toml", flyDoctorRunner(committedControllerConfig, `[]`))
	if !hasDoctorStatus(checks, "controller drift", "warn") {
		t.Fatalf("checks = %#v", checks)
	}
}

func TestDoctorReportsUnusableConfigResponses(t *testing.T) {
	for name, response := range map[string]string{
		"not json": "Error: no such file",
		"no app":   `{"env": {}}`,
	} {
		t.Run(name, func(t *testing.T) {
			checks := doctor("fly", "control", "workers", "", "fleet.toml", flyDoctorRunner(response, matchingControllerMachines))
			if !hasDoctorStatus(checks, "controller configuration", "fail") {
				t.Fatalf("checks = %#v", checks)
			}
		})
	}
	run := func(name string, args ...string) ([]byte, error) {
		if name == "fly" && len(args) > 0 && args[0] == "config" {
			return []byte("Error: failed to parse fly.toml"), errors.New("exit 1")
		}
		return flyDoctorRunner(committedControllerConfig, matchingControllerMachines)(name, args...)
	}
	checks := doctor("fly", "control", "workers", "", "fleet.toml", run)
	if !hasDoctorStatus(checks, "controller configuration", "fail") {
		t.Fatalf("checks = %#v", checks)
	}
	checks = doctor("fly", "control", "workers", "", "fleet.toml", flyDoctorRunner(committedControllerConfig, `[{"state": "started"}]`))
	if !hasDoctorStatus(checks, "controller drift", "fail") {
		t.Fatalf("a Machine without an id must not be compared: %#v", checks)
	}
}
