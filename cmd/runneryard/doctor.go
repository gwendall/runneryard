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
	firewallID := flags.String("firewall-id", strings.TrimSpace(os.Getenv("RUNNER_HETZNER_FIREWALL_ID")), "Hetzner worker firewall ID")
	jsonOutput := flags.Bool("json", false, "print machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	checks := doctor(*providerName, *controllerApp, *workerApp, *firewallID, execCommand)
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

func doctor(providerName, controllerApp, workerApp, firewallID string, run commandRunner) []doctorCheck {
	checks := []doctorCheck{commandCheck(run, "git", "git", "--version")}
	switch providerName {
	case "fly":
		return append(checks, doctorFly(controllerApp, workerApp, run)...)
	case "hetzner":
		return append(checks, doctorHetzner(firewallID, run)...)
	default:
		return append(checks, doctorCheck{Name: "compute provider", Status: "fail", Details: fmt.Sprintf("%q is not bundled", providerName)})
	}

}

func doctorFly(controllerApp, workerApp string, run commandRunner) []doctorCheck {
	checks := make([]doctorCheck, 0, 5)
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
	if controllerApp != "" {
		checks = append(checks, controllerPolicySecretCheck(controllerApp, run))
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

var flyPolicyEnvironment = map[string]struct{}{
	"COMPUTE_PROVIDER": {}, "CONTROLLER_ID": {}, "GITHUB_CONFIG_URL": {},
	"LOG_LEVEL": {}, "MAX_RUNNERS": {}, "MIN_RUNNERS": {}, "RUNNER_BUDGET_FILE": {},
	"RUNNER_BUDGET_WINDOW": {}, "RUNNER_CPUS": {}, "RUNNER_CPU_KIND": {},
	"RUNNER_FLY_APP": {}, "RUNNER_FLY_REGION": {}, "RUNNER_GROUP": {},
	"RUNNER_IMAGE": {}, "RUNNER_MAX_LIFETIME": {}, "RUNNER_MEMORY_MB": {},
	"RUNNER_ROOTFS_GB": {}, "RUNNER_USAGE_BUDGET": {}, "SCALE_SET_NAME": {},
}

func controllerPolicySecretCheck(controllerApp string, run commandRunner) doctorCheck {
	output, err := run("fly", "secrets", "list", "--app", controllerApp, "--json")
	if err != nil {
		return doctorCheck{Name: "controller policy source", Status: "fail", Details: compactError(output, err)}
	}
	var secrets []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(output, &secrets); err != nil {
		return doctorCheck{Name: "controller policy source", Status: "fail", Details: "could not parse Fly response"}
	}
	shadows := make([]string, 0)
	for _, secret := range secrets {
		if _, exists := flyPolicyEnvironment[secret.Name]; exists {
			shadows = append(shadows, secret.Name)
		}
	}
	if len(shadows) > 0 {
		return doctorCheck{
			Name: "controller policy source", Status: "fail",
			Details: "app secrets override non-secret policy: " + strings.Join(shadows, ", "),
		}
	}
	return doctorCheck{Name: "controller policy source", Status: "pass", Details: "no policy values shadowed by secrets"}
}

func doctorHetzner(firewallID string, run commandRunner) []doctorCheck {
	checks := []doctorCheck{commandCheck(run, "Hetzner CLI", "hcloud", "version")}
	output, err := run("hcloud", "server", "list", "--selector", "runneryard-managed-by=true", "-o", "json")
	if err != nil {
		checks = append(checks, doctorCheck{Name: "Hetzner API", Status: "fail", Details: compactError(output, err)})
	} else {
		checks = append(checks, doctorCheck{Name: "Hetzner API", Status: "pass", Details: "authenticated project access"})
	}
	if firewallID == "" {
		return append(checks, doctorCheck{Name: "worker firewall", Status: "fail", Details: "pass --firewall-id for the deny-inbound firewall"})
	}
	output, err = run("hcloud", "firewall", "describe", firewallID, "-o", "json")
	if err != nil {
		return append(checks, doctorCheck{Name: "worker firewall", Status: "fail", Details: compactError(output, err)})
	}
	var firewall struct {
		Rules []struct {
			Direction string `json:"direction"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(output, &firewall); err != nil {
		return append(checks, doctorCheck{Name: "worker firewall", Status: "fail", Details: "could not parse Hetzner response"})
	}
	inbound := 0
	for _, rule := range firewall.Rules {
		if rule.Direction == "in" {
			inbound++
		}
	}
	if inbound > 0 {
		return append(checks, doctorCheck{Name: "worker firewall", Status: "fail", Details: fmt.Sprintf("%d inbound rule(s) expose disposable workers", inbound)})
	}
	return append(checks, doctorCheck{Name: "worker firewall", Status: "pass", Details: "no inbound rules"})
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
