package main

import (
	"errors"
	"strings"
	"testing"
)

func TestDoctorRejectsSecretsOnWorkerApp(t *testing.T) {
	run := func(name string, args ...string) ([]byte, error) {
		if name == "fly" && len(args) > 1 && args[0] == "secrets" {
			return []byte(`[{"Name":"TOKEN"}]`), nil
		}
		return []byte("ready"), nil
	}
	checks := doctor("fly", "control", "workers", "", "", run)
	if !hasDoctorStatus(checks, "worker app secrets", "fail") {
		t.Fatalf("checks = %#v", checks)
	}
}

func TestDoctorRejectsSharedApp(t *testing.T) {
	run := func(name string, args ...string) ([]byte, error) {
		if name == "fly" && len(args) > 1 && args[0] == "secrets" {
			return []byte(`[]`), nil
		}
		return []byte("ready"), nil
	}
	checks := doctor("fly", "same", "same", "", "", run)
	if !hasDoctorStatus(checks, "control/worker isolation", "fail") {
		t.Fatalf("checks = %#v", checks)
	}
}

func TestDoctorRejectsControllerSecretThatShadowsPolicy(t *testing.T) {
	for _, policy := range []string{"MAX_RUNNERS", "RUNNER_STATUS_FILE"} {
		t.Run(policy, func(t *testing.T) {
			run := func(name string, args ...string) ([]byte, error) {
				if name == "fly" && len(args) > 3 && args[0] == "secrets" {
					if args[3] == "control" {
						return []byte(`[{"name":"FLY_API_TOKEN"},{"name":"` + policy + `"}]`), nil
					}
					return []byte(`[]`), nil
				}
				return []byte("ready"), nil
			}
			checks := doctor("fly", "control", "workers", "", "", run)
			if !hasDoctorStatus(checks, "controller policy source", "fail") {
				t.Fatalf("checks = %#v", checks)
			}
		})
	}
}

func TestDoctorAcceptsCompleteGitHubAppSecrets(t *testing.T) {
	run := func(name string, args ...string) ([]byte, error) {
		if name == "fly" && len(args) > 3 && args[0] == "secrets" {
			if args[3] == "control" {
				return []byte(`[{"name":"GITHUB_APP_CLIENT_ID"},{"name":"GITHUB_APP_INSTALLATION_ID"},{"name":"GITHUB_APP_PRIVATE_KEY"}]`), nil
			}
			return []byte(`[]`), nil
		}
		return []byte("ready"), nil
	}
	checks := doctor("fly", "control", "workers", "", "", run)
	if !hasDoctorStatus(checks, "controller GitHub auth", "pass") {
		t.Fatalf("checks = %#v", checks)
	}
}

func TestDoctorWarnsForUserTokenAndRejectsMixedAuth(t *testing.T) {
	for name, secrets := range map[string]string{
		"user token": `[{"name":"GITHUB_TOKEN"}]`,
		"mixed":      `[{"name":"GITHUB_TOKEN"},{"name":"GITHUB_APP_CLIENT_ID"},{"name":"GITHUB_APP_INSTALLATION_ID"},{"name":"GITHUB_APP_PRIVATE_KEY"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			run := func(command string, args ...string) ([]byte, error) {
				if command == "fly" && len(args) > 3 && args[0] == "secrets" {
					if args[3] == "control" {
						return []byte(secrets), nil
					}
					return []byte(`[]`), nil
				}
				return []byte("ready"), nil
			}
			checks := doctor("fly", "control", "workers", "", "", run)
			expected := "warn"
			if name == "mixed" {
				expected = "fail"
			}
			if !hasDoctorStatus(checks, "controller GitHub auth", expected) {
				t.Fatalf("checks = %#v", checks)
			}
		})
	}
}

func TestDoctorRejectsUnusableFlySecretResponses(t *testing.T) {
	for name, response := range map[string]string{
		"null":         `null`,
		"missing name": `[{"digest":"abc"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			run := func(command string, args ...string) ([]byte, error) {
				if command == "fly" && len(args) > 3 && args[0] == "secrets" {
					if args[3] == "control" {
						return []byte(response), nil
					}
					return []byte(`[]`), nil
				}
				return []byte("ready"), nil
			}
			checks := doctor("fly", "control", "workers", "", "", run)
			if !hasDoctorStatus(checks, "controller policy source", "fail") {
				t.Fatalf("checks = %#v", checks)
			}
		})
	}
}

func TestDoctorRejectsNullWorkerSecretResponse(t *testing.T) {
	run := func(command string, args ...string) ([]byte, error) {
		if command == "fly" && len(args) > 3 && args[0] == "secrets" {
			if args[3] == "workers" {
				return []byte(`null`), nil
			}
			return []byte(`[]`), nil
		}
		return []byte("ready"), nil
	}
	checks := doctor("fly", "control", "workers", "", "", run)
	if !hasDoctorStatus(checks, "worker app secrets", "fail") {
		t.Fatalf("checks = %#v", checks)
	}
}

func TestDoctorRequiresBothAppsToProveIsolation(t *testing.T) {
	run := func(name string, args ...string) ([]byte, error) {
		if name == "fly" && len(args) > 1 && args[0] == "secrets" {
			return []byte(`[]`), nil
		}
		return []byte("ready"), nil
	}
	checks := doctor("fly", "", "workers", "", "", run)
	if !hasDoctorStatus(checks, "control/worker isolation", "fail") {
		t.Fatalf("checks = %#v", checks)
	}
}

func TestDoctorReportsMissingFlyCLI(t *testing.T) {
	run := func(name string, args ...string) ([]byte, error) {
		if name == "fly" {
			return []byte("not found"), errors.New("exit 1")
		}
		return []byte("ready"), nil
	}
	checks := doctor("fly", "control", "workers", "", "", run)
	if !hasDoctorStatus(checks, "Fly CLI", "fail") {
		t.Fatalf("checks = %#v", checks)
	}
	for _, check := range checks {
		if strings.Contains(check.Details, "\n") {
			t.Fatalf("doctor output should be one line: %#v", check)
		}
	}
}

func TestDoctorAcceptsHetznerFirewallWithoutInboundRules(t *testing.T) {
	run := func(name string, args ...string) ([]byte, error) {
		if name == "hcloud" && len(args) > 1 && args[0] == "firewall" {
			return []byte(`{"rules":[{"direction":"out"}]}`), nil
		}
		if name == "hcloud" && len(args) > 1 && args[0] == "server" {
			return []byte(`[]`), nil
		}
		return []byte("ready"), nil
	}
	checks := doctor("hetzner", "", "", "42", "", run)
	if !hasDoctorStatus(checks, "Hetzner API", "pass") || !hasDoctorStatus(checks, "worker firewall", "pass") {
		t.Fatalf("checks = %#v", checks)
	}
}

func TestDoctorRejectsHetznerInboundRules(t *testing.T) {
	run := func(name string, args ...string) ([]byte, error) {
		if name == "hcloud" && len(args) > 1 && args[0] == "firewall" {
			return []byte(`{"rules":[{"direction":"in"}]}`), nil
		}
		return []byte(`[]`), nil
	}
	checks := doctor("hetzner", "", "", "42", "", run)
	if !hasDoctorStatus(checks, "worker firewall", "fail") {
		t.Fatalf("checks = %#v", checks)
	}
}

func TestDoctorRequiresHetznerFirewall(t *testing.T) {
	run := func(_ string, _ ...string) ([]byte, error) { return []byte(`[]`), nil }
	checks := doctor("hetzner", "", "", "", "", run)
	if !hasDoctorStatus(checks, "worker firewall", "fail") {
		t.Fatalf("checks = %#v", checks)
	}
}

func hasDoctorStatus(checks []doctorCheck, name, status string) bool {
	for _, check := range checks {
		if check.Name == name && check.Status == status {
			return true
		}
	}
	return false
}

func TestDoctorRequiresAlertWebhook(t *testing.T) {
	for name, tc := range map[string]struct {
		secrets  string
		expected string
	}{
		"no webhook":   {`[{"name":"GITHUB_TOKEN"}]`, "fail"},
		"with webhook": {`[{"name":"GITHUB_TOKEN"},{"name":"ALERT_WEBHOOK_URL"}]`, "pass"},
	} {
		t.Run(name, func(t *testing.T) {
			run := func(command string, args ...string) ([]byte, error) {
				if command == "fly" && len(args) > 3 && args[0] == "secrets" {
					if args[3] == "control" {
						return []byte(tc.secrets), nil
					}
					return []byte(`[]`), nil
				}
				return []byte("ready"), nil
			}
			checks := doctor("fly", "control", "workers", "", "", run)
			if !hasDoctorStatus(checks, "controller alerting", tc.expected) {
				t.Fatalf("checks = %#v", checks)
			}
		})
	}
}

func fleetRunner(statusJSON string, machinesByApp map[string]int) commandRunner {
	return func(command string, args ...string) ([]byte, error) {
		if command != "fly" {
			return []byte("ready"), nil
		}
		switch {
		case len(args) > 0 && args[0] == "secrets":
			if len(args) > 3 && args[3] == "control" {
				return []byte(`[{"name":"GITHUB_TOKEN"},{"name":"ALERT_WEBHOOK_URL"}]`), nil
			}
			return []byte(`[]`), nil
		case len(args) > 1 && args[0] == "config" && args[1] == "show":
			return []byte(`{"app":"control","build":{"image":"ghcr.io/gwendall/runneryard:0.4.7"},"env":{"RUNNER_IMAGE":"ghcr.io/gwendall/runneryard:0.4.7","MAX_RUNNERS":"40","RUNNER_STATUS_FILE":"/var/lib/runner-fleet/status.json"}}`), nil
		case len(args) > 1 && args[0] == "machine" && args[1] == "list":
			return []byte(`[]`), nil
		case len(args) > 1 && args[0] == "ssh" && args[1] == "console":
			return []byte(statusJSON), nil
		case len(args) > 1 && args[0] == "apps" && args[1] == "list":
			out := "["
			first := true
			for name := range machinesByApp {
				if !first {
					out += ","
				}
				first = false
				out += `{"Name":"` + name + `"}`
			}
			return []byte(out + "]"), nil
		case len(args) > 1 && args[0] == "machines" && args[1] == "list":
			n := machinesByApp[args[3]]
			out := "["
			for i := 0; i < n; i++ {
				if i > 0 {
					out += ","
				}
				out += `{"id":"m"}`
			}
			return []byte(out + "]"), nil
		}
		return []byte("ready"), nil
	}
}

func TestDoctorReadsTheBudgetHorizonFromTheLiveStatus(t *testing.T) {
	short := `{"health":"ready","budget":{"limit_seconds":36000000,"used_seconds":12000000,"burn_seconds_per_day":1051223,"horizon_seconds":750033}}`
	checks := doctor("fly", "control", "workers", "", ".runneryard/fly.controller.toml", fleetRunner(short, map[string]int{"workers": 0}))
	if !hasDoctorStatus(checks, "budget horizon", "fail") {
		t.Fatalf("8.7 days must fail: %#v", checks)
	}
	long := `{"health":"ready","budget":{"limit_seconds":36000000,"used_seconds":12000000,"burn_seconds_per_day":1051223,"horizon_seconds":2030000}}`
	checks = doctor("fly", "control", "workers", "", ".runneryard/fly.controller.toml", fleetRunner(long, map[string]int{"workers": 0}))
	if !hasDoctorStatus(checks, "budget horizon", "pass") {
		t.Fatalf("23 days must pass: %#v", checks)
	}
	degraded := `{"health":"degraded","reason":"usage_budget_exhausted","budget":{"horizon_seconds":0}}`
	checks = doctor("fly", "control", "workers", "", ".runneryard/fly.controller.toml", fleetRunner(degraded, map[string]int{"workers": 0}))
	if !hasDoctorStatus(checks, "budget horizon", "fail") {
		t.Fatalf("a degraded fleet must fail: %#v", checks)
	}
}

func TestDoctorComparesMaxRunnersWithTheOrganizationMargin(t *testing.T) {
	status := `{"health":"ready","budget":{"horizon_seconds":2030000}}`
	doctorFlyMachineLimit = 0
	checks := doctor("fly", "control", "workers", "", ".runneryard/fly.controller.toml", fleetRunner(status, map[string]int{"workers": 4, "other-a": 30, "other-b": 26}))
	if !hasDoctorStatus(checks, "fleet capacity margin", "warn") {
		t.Fatalf("without a limit the check is a hint: %#v", checks)
	}
	doctorFlyMachineLimit = 100
	defer func() { doctorFlyMachineLimit = 0 }()
	// 56 Machines belong to other apps: room for 44, MAX_RUNNERS 40 fits.
	checks = doctor("fly", "control", "workers", "", ".runneryard/fly.controller.toml", fleetRunner(status, map[string]int{"workers": 4, "other-a": 30, "other-b": 26}))
	if !hasDoctorStatus(checks, "fleet capacity margin", "pass") {
		t.Fatalf("40 under a margin of 44 must pass: %#v", checks)
	}
	// 62 elsewhere: room for 38, MAX_RUNNERS 40 does not fit (the 2026-09-03 shape).
	checks = doctor("fly", "control", "workers", "", ".runneryard/fly.controller.toml", fleetRunner(status, map[string]int{"workers": 4, "other-a": 36, "other-b": 26}))
	if !hasDoctorStatus(checks, "fleet capacity margin", "fail") {
		t.Fatalf("40 over a margin of 38 must fail: %#v", checks)
	}
}
