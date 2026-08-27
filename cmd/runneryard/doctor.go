package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type doctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Details string `json:"details"`
}

type commandRunner func(name string, args ...string) ([]byte, error)

func runDoctor(args []string) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(os.Stdout)
	providerName := flags.String("provider", "fly", "compute provider")
	workerApp := flags.String("worker-app", strings.TrimSpace(os.Getenv("RUNNER_FLY_APP")), "secret-free Fly worker app")
	controllerApp := flags.String("controller-app", strings.TrimSpace(os.Getenv("FLY_APP_NAME")), "Fly controller app")
	jsonOutput := flags.Bool("json", false, "print machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	checks := doctor(*providerName, *controllerApp, *workerApp, execCommand)
	failed := false
	for _, check := range checks {
		if check.Status == "fail" {
			failed = true
		}
	}
	if *jsonOutput {
		encoded, _ := json.MarshalIndent(checks, "", "  ")
		fmt.Println(string(encoded))
	} else {
		for _, check := range checks {
			mark := "✓"
			if check.Status == "warn" {
				mark = "!"
			} else if check.Status == "fail" {
				mark = "✗"
			}
			fmt.Printf("%s %-28s %s\n", mark, check.Name, check.Details)
		}
	}
	if failed {
		return fmt.Errorf("doctor found unsafe or incomplete configuration")
	}
	return nil
}

func doctor(providerName, controllerApp, workerApp string, run commandRunner) []doctorCheck {
	checks := []doctorCheck{commandCheck(run, "git", "git", "--version")}
	if providerName != "fly" {
		return append(checks, doctorCheck{Name: "compute provider", Status: "fail", Details: fmt.Sprintf("%q is not bundled", providerName)})
	}
	checks = append(checks, commandCheck(run, "Fly CLI", "fly", "auth", "whoami"))
	if workerApp == "" {
		return append(checks, doctorCheck{Name: "worker app", Status: "fail", Details: "pass --worker-app to verify secret isolation"})
	}
	if controllerApp == "" {
		checks = append(checks, doctorCheck{Name: "control/worker isolation", Status: "fail", Details: "pass --controller-app to verify isolation"})
	} else if controllerApp == workerApp {
		checks = append(checks, doctorCheck{Name: "control/worker isolation", Status: "fail", Details: "controller and workers use the same app"})
	} else {
		checks = append(checks, doctorCheck{Name: "control/worker isolation", Status: "pass", Details: "apps are separate"})
	}
	output, err := run("fly", "secrets", "list", "--app", workerApp, "--json")
	if err != nil {
		checks = append(checks, doctorCheck{Name: "worker app secrets", Status: "fail", Details: compactError(output, err)})
	} else {
		var secrets []any
		if json.Unmarshal(output, &secrets) != nil {
			checks = append(checks, doctorCheck{Name: "worker app secrets", Status: "fail", Details: "could not parse Fly response"})
		} else if len(secrets) > 0 {
			checks = append(checks, doctorCheck{Name: "worker app secrets", Status: "fail", Details: fmt.Sprintf("%d app secret(s) would reach job code", len(secrets))})
		} else {
			checks = append(checks, doctorCheck{Name: "worker app secrets", Status: "pass", Details: "empty"})
		}
	}
	return checks
}

func commandCheck(run commandRunner, label, command string, args ...string) doctorCheck {
	output, err := run(command, args...)
	if err != nil {
		return doctorCheck{Name: label, Status: "fail", Details: compactError(output, err)}
	}
	first := strings.Split(strings.TrimSpace(string(output)), "\n")[0]
	if first == "" {
		first = "ready"
	}
	return doctorCheck{Name: label, Status: "pass", Details: first}
}

func compactError(output []byte, err error) string {
	message := strings.TrimSpace(string(output))
	if message == "" {
		return err.Error()
	}
	return strings.Split(message, "\n")[0]
}

func execCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}
