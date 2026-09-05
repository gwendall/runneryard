package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// flyConfigFile is the subset of "fly config show --local" output that the
// controller Machine is compared against. Fly parses the TOML, so RunnerYard
// never interprets the file format itself.
type flyConfigFile struct {
	App   string `json:"app"`
	Build struct {
		Image string `json:"image"`
	} `json:"build"`
	Env     map[string]string `json:"env"`
	Restart []struct {
		Policy string `json:"policy"`
	} `json:"restart"`
	Mounts []struct {
		Destination string `json:"destination"`
	} `json:"mounts"`
}

// flyMachine is the subset of "fly machine list --json" that carries the
// effective controller configuration, including values changed with
// "fly machine update" that never reached the reviewed file.
type flyMachine struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Config struct {
		Image   string            `json:"image"`
		Env     map[string]string `json:"env"`
		Restart struct {
			Policy string `json:"policy"`
		} `json:"restart"`
		Mounts []struct {
			Path string `json:"path"`
		} `json:"mounts"`
	} `json:"config"`
}

const maxDriftDetails = 6

// controllerConfigChecks compares the committed Fly configuration with every
// live controller Machine. A limit raised by hand, a worker shape changed
// during an incident, or a forgotten image pin shows up here before the next
// "fly deploy" silently reverts it.
func controllerConfigChecks(controllerApp, configPath string, run commandRunner) []doctorCheck {
	if configPath == "" {
		return []doctorCheck{{Name: "controller configuration", Status: "warn", Details: "no committed configuration to compare; pass --config .runneryard/fly.controller.toml"}}
	}
	output, err := run("fly", "config", "show", "--local", "--config", configPath)
	if err != nil {
		return []doctorCheck{{Name: "controller configuration", Status: "fail", Details: compactError(output, err)}}
	}
	file, err := parseFlyConfigFile(output)
	if err != nil {
		return []doctorCheck{{Name: "controller configuration", Status: "fail", Details: err.Error()}}
	}
	if file.App != controllerApp {
		return []doctorCheck{{Name: "controller configuration", Status: "fail", Details: fmt.Sprintf("%s targets app %q, not %q", configPath, file.App, controllerApp)}}
	}
	checks := []doctorCheck{
		{Name: "controller configuration", Status: "pass", Details: configPath},
		imagePinCheck(file),
	}
	output, err = run("fly", "machine", "list", "--app", controllerApp, "--json")
	if err != nil {
		return append(checks, doctorCheck{Name: "controller drift", Status: "fail", Details: compactError(output, err)})
	}
	machines, err := parseFlyMachines(output)
	if err != nil {
		return append(checks, doctorCheck{Name: "controller drift", Status: "fail", Details: err.Error()})
	}
	live := make([]flyMachine, 0, len(machines))
	for _, machine := range machines {
		if strings.EqualFold(strings.TrimSpace(machine.State), "destroyed") {
			continue
		}
		live = append(live, machine)
	}
	if len(live) == 0 {
		return append(checks, doctorCheck{Name: "controller drift", Status: "warn", Details: "no controller Machine to compare; deploy from " + configPath})
	}
	if len(live) > 1 {
		checks = append(checks, doctorCheck{Name: "controller instances", Status: "fail", Details: fmt.Sprintf("%d Machines would contend for one scale set; keep exactly one controller", len(live))})
	}
	drift := make([]string, 0)
	for _, machine := range live {
		drift = append(drift, compareControllerMachine(file, machine)...)
	}
	if len(drift) > 0 {
		details := drift
		if len(details) > maxDriftDetails {
			details = append(append([]string{}, drift[:maxDriftDetails]...), fmt.Sprintf("and %d more", len(drift)-maxDriftDetails))
		}
		return append(checks, doctorCheck{Name: "controller drift", Status: "fail", Details: strings.Join(details, "; ")})
	}
	return append(checks, doctorCheck{Name: "controller drift", Status: "pass", Details: fmt.Sprintf("%d Machine(s) match %s", len(live), configPath)})
}

func imagePinCheck(file flyConfigFile) doctorCheck {
	if file.Build.Image == "" {
		return doctorCheck{Name: "controller image pin", Status: "warn", Details: "[build] image is not set; the file does not record the deployed release"}
	}
	runnerImage, ok := file.Env["RUNNER_IMAGE"]
	if !ok || runnerImage == file.Build.Image {
		return doctorCheck{Name: "controller image pin", Status: "pass", Details: file.Build.Image}
	}
	// A derived worker image (docs/derived-images.md) is built FROM the
	// release the controller runs and declares that release through
	// RUNNER_IMAGE_BASE, so the pin discipline survives: the base must be the
	// controller's own release, and a worker image without a declared base is
	// still the divergence this check exists to catch.
	if declaredBase, ok := file.Env["RUNNER_IMAGE_BASE"]; ok {
		if declaredBase == file.Build.Image {
			return doctorCheck{Name: "controller image pin", Status: "pass", Details: fmt.Sprintf("%s; workers run %s, declared as derived from it", file.Build.Image, runnerImage)}
		}
		return doctorCheck{Name: "controller image pin", Status: "fail", Details: fmt.Sprintf("RUNNER_IMAGE_BASE %s is not the controller release %s that RUNNER_IMAGE %s must be built from", declaredBase, file.Build.Image, runnerImage)}
	}
	return doctorCheck{Name: "controller image pin", Status: "fail", Details: fmt.Sprintf("[build] image %s and RUNNER_IMAGE %s differ; a derived worker image declares its base with RUNNER_IMAGE_BASE (docs/derived-images.md)", file.Build.Image, runnerImage)}
}

// flyInjectedEnvironment reports the variables Fly adds to every Machine that
// never appear in a configuration file.
func flyInjectedEnvironment(key string) bool {
	return strings.HasPrefix(key, "FLY_") || key == "PRIMARY_REGION"
}

func compareControllerMachine(file flyConfigFile, machine flyMachine) []string {
	drift := make([]string, 0)
	prefix := machine.ID + ": "
	if file.Build.Image != "" && machine.Config.Image != file.Build.Image {
		drift = append(drift, prefix+"image is "+machine.Config.Image+", file pins "+file.Build.Image)
	}
	keys := make(map[string]struct{})
	for key := range file.Env {
		keys[key] = struct{}{}
	}
	for key := range machine.Config.Env {
		if !flyInjectedEnvironment(key) {
			keys[key] = struct{}{}
		}
	}
	sorted := make([]string, 0, len(keys))
	for key := range keys {
		sorted = append(sorted, key)
	}
	sort.Strings(sorted)
	for _, key := range sorted {
		fileValue, inFile := file.Env[key]
		liveValue, inLive := machine.Config.Env[key]
		switch {
		case !inFile:
			drift = append(drift, prefix+key+"="+liveValue+" is set on the Machine only")
		case !inLive:
			drift = append(drift, prefix+key+"="+fileValue+" is set in the file only")
		case fileValue != liveValue:
			drift = append(drift, prefix+key+" is "+liveValue+" on the Machine, "+fileValue+" in the file")
		}
	}
	if len(file.Restart) > 0 && file.Restart[0].Policy != machine.Config.Restart.Policy {
		drift = append(drift, prefix+"restart policy is "+orNone(machine.Config.Restart.Policy)+", file sets "+file.Restart[0].Policy)
	}
	filePaths := make([]string, 0, len(file.Mounts))
	for _, mount := range file.Mounts {
		filePaths = append(filePaths, mount.Destination)
	}
	livePaths := make([]string, 0, len(machine.Config.Mounts))
	for _, mount := range machine.Config.Mounts {
		livePaths = append(livePaths, mount.Path)
	}
	sort.Strings(filePaths)
	sort.Strings(livePaths)
	if strings.Join(filePaths, ",") != strings.Join(livePaths, ",") {
		drift = append(drift, prefix+"mounts are "+orNone(strings.Join(livePaths, ","))+", file sets "+orNone(strings.Join(filePaths, ",")))
	}
	return drift
}

func orNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

func parseFlyConfigFile(output []byte) (flyConfigFile, error) {
	var file flyConfigFile
	trimmed := bytes.TrimSpace(output)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return file, errors.New("could not parse Fly response as a configuration file")
	}
	if err := json.Unmarshal(trimmed, &file); err != nil {
		return file, errors.New("could not parse Fly response as a configuration file")
	}
	if strings.TrimSpace(file.App) == "" {
		return file, errors.New("configuration file does not name an app")
	}
	return file, nil
}

func parseFlyMachines(output []byte) ([]flyMachine, error) {
	trimmed := bytes.TrimSpace(output)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, errors.New("could not parse Fly response as a Machine list")
	}
	var machines []flyMachine
	if err := json.Unmarshal(trimmed, &machines); err != nil {
		return nil, errors.New("could not parse Fly response as a Machine list")
	}
	for _, machine := range machines {
		if strings.TrimSpace(machine.ID) == "" {
			return nil, errors.New("fly Machine response contains a record without an id")
		}
	}
	return machines, nil
}
