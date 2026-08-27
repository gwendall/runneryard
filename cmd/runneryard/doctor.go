package main

import (
	"bytes"
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
		checks = append(checks, controllerSecretChecks(controllerApp, run)...)
	}
	output, err := run("fly", "secrets", "list", "--app", workerApp, "--json")
	if err != nil {
		checks = append(checks, doctorCheck{Name: "worker app secrets", Status: "fail", Details: compactError(output, err)})
	} else {
		secrets, parseErr := parseFlySecretNames(output)
		if parseErr != nil {
			checks = append(checks, doctorCheck{Name: "worker app secrets", Status: "fail", Details: parseErr.Error()})
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

func controllerSecretChecks(controllerApp string, run commandRunner) []doctorCheck {
	output, err := run("fly", "secrets", "list", "--app", controllerApp, "--json")
	if err != nil {
		details := compactError(output, err)
		return []doctorCheck{
			{Name: "controller policy source", Status: "fail", Details: details},
			{Name: "controller GitHub auth", Status: "fail", Details: details},
		}
	}
	secrets, err := parseFlySecretNames(output)
	if err != nil {
		return []doctorCheck{
			{Name: "controller policy source", Status: "fail", Details: err.Error()},
			{Name: "controller GitHub auth", Status: "fail", Details: err.Error()},
		}
	}
	shadows := make([]string, 0)
	for _, name := range secrets {
		if _, exists := flyPolicyEnvironment[name]; exists {
			shadows = append(shadows, name)
		}
	}
	if len(shadows) > 0 {
		return []doctorCheck{{
			Name: "controller policy source", Status: "fail",
			Details: "app secrets override non-secret policy: " + strings.Join(shadows, ", "),
		}, githubAuthSecretCheck(secrets)}
	}
	return []doctorCheck{
		{Name: "controller policy source", Status: "pass", Details: "no policy values shadowed by secrets"},
		githubAuthSecretCheck(secrets),
	}
}

func githubAuthSecretCheck(secrets []string) doctorCheck {
	present := map[string]bool{}
	for _, name := range secrets {
		present[name] = true
	}
	appNames := []string{"GITHUB_APP_CLIENT_ID", "GITHUB_APP_INSTALLATION_ID", "GITHUB_APP_PRIVATE_KEY"}
	appCount := 0
	for _, name := range appNames {
		if present[name] {
			appCount++
		}
	}
	if present["GITHUB_TOKEN"] && appCount > 0 {
		return doctorCheck{Name: "controller GitHub auth", Status: "fail", Details: "both a user token and GitHub App credentials are configured"}
	}
	if appCount > 0 && appCount < len(appNames) {
		return doctorCheck{Name: "controller GitHub auth", Status: "fail", Details: "GitHub App secret set is incomplete"}
	}
	if appCount == len(appNames) {
		return doctorCheck{Name: "controller GitHub auth", Status: "pass", Details: "dedicated GitHub App secret set is complete"}
	}
	if present["GITHUB_TOKEN"] {
		return doctorCheck{Name: "controller GitHub auth", Status: "warn", Details: "user token configured; migrate with runneryard auth github create"}
	}
	return doctorCheck{Name: "controller GitHub auth", Status: "fail", Details: "no GitHub App credentials or fallback user token configured"}
}

func parseFlySecretNames(output []byte) ([]string, error) {
	trimmed := bytes.TrimSpace(output)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, fmt.Errorf("could not parse Fly response as a secret list")
	}
	var records []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(trimmed, &records); err != nil {
		return nil, fmt.Errorf("could not parse Fly response as a secret list")
	}
	names := make([]string, 0, len(records))
	for _, record := range records {
		name := strings.TrimSpace(record.Name)
		if name == "" {
			return nil, fmt.Errorf("fly secret response contains a record without a name")
		}
		names = append(names, name)
	}
	return names, nil
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
	// Only fixed diagnostic binaries are allowed. Arguments are passed directly
	// without a shell; dynamic app and firewall values are individual arguments.
	var command *exec.Cmd
	switch name {
	case "git":
		command = exec.Command("git", args...) // #nosec G204 G702 -- fixed executable, discrete arguments, no shell
	case "fly":
		command = exec.Command("fly", args...) // #nosec G204 G702 -- fixed executable, discrete arguments, no shell
	case "hcloud":
		command = exec.Command("hcloud", args...) // #nosec G204 G702 -- fixed executable, discrete arguments, no shell
	default:
		return nil, fmt.Errorf("diagnostic command %q is not allowed", name)
	}
	return command.CombinedOutput()
}
