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
