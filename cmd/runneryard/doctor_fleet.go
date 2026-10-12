package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gwendall/runneryard/controller"
)

// Two checks that read the fleet as it RUNS, not only as it is configured. Both were
// missing on 2026-09-05: doctor was green while the usage budget stood 8.7 days from
// exhaustion (after which every job queues in silence), and MAX_RUNNERS had been set
// above the organization's Machine limit two days earlier (the controller died at the
// 45th worker). Configuration says what was intended; these say what is about to happen.

// doctorFlyMachineLimit is the organization's Machine limit as the operator knows it
// (Fly does not expose it through the API). Zero skips the margin check with a hint.
var doctorFlyMachineLimit int

const budgetHorizonFloor = 14 * 24 * time.Hour

// budgetHorizonCheck reads the controller's live status file over `fly ssh console` and
// judges the usage budget's horizon: how long the remaining budget lasts at the trailing
// day's burn rate.
func budgetHorizonCheck(controllerApp string, file flyConfigFile, run commandRunner) doctorCheck {
	statusFile := file.Env["RUNNER_STATUS_FILE"]
	if statusFile == "" {
		statusFile = "/var/lib/runneryard/status.json"
	}
	output, err := run("fly", "ssh", "console", "--app", controllerApp, "-C", "cat "+statusFile)
	if err != nil {
		return doctorCheck{Name: "budget horizon", Status: "warn", Details: "status unreadable over ssh (" + compactError(output, err) + "); run again once the controller is up"}
	}
	// `fly ssh console` prints a "Connecting to <address>..." banner before the command's
	// output; the document starts at the first brace.
	if i := bytes.IndexByte(output, '{'); i > 0 {
		output = output[i:]
	}
	var status controller.FleetStatus
	if err := json.Unmarshal(output, &status); err != nil {
		return doctorCheck{Name: "budget horizon", Status: "warn", Details: "status file is not the fleet status schema: " + err.Error()}
	}
	if status.Health != "" && status.Health != "ready" {
		return doctorCheck{Name: "budget horizon", Status: "fail", Details: fmt.Sprintf("fleet is %s (%s); read `runneryard status` on the controller", status.Health, status.Reason)}
	}
	if status.Budget.HorizonSeconds <= 0 {
		return doctorCheck{Name: "budget horizon", Status: "pass", Details: "no recent usage to extrapolate from"}
	}
	horizon := time.Duration(status.Budget.HorizonSeconds) * time.Second
	detail := fmt.Sprintf("%.1f days at %.0f h/day (used %.0f of %.0f h)", horizon.Hours()/24, float64(status.Budget.BurnSecondsPerDay)/3600, float64(status.Budget.UsedSeconds)/3600, float64(status.Budget.LimitSeconds)/3600)
	if horizon < budgetHorizonFloor {
		return doctorCheck{Name: "budget horizon", Status: "fail", Details: detail + "; under 14 days the budget runs out before anyone plans for it: raise RUNNER_USAGE_BUDGET or cut the fan-out (runneryard lint-workflows)"}
	}
	return doctorCheck{Name: "budget horizon", Status: "pass", Details: detail}
}

// fleetMarginCheck compares MAX_RUNNERS with the room the organization's Machine limit
// leaves once every other app's Machines are counted (stopped Machines count too). The
// limit is per organization, so only the apps of the worker app's organization count:
// `fly apps list` returns the apps of every organization the operator belongs to.
func fleetMarginCheck(workerApp string, file flyConfigFile, run commandRunner) doctorCheck {
	if doctorFlyMachineLimit <= 0 {
		return doctorCheck{Name: "fleet capacity margin", Status: "warn", Details: "pass --fly-machine-limit <n> (the organization's Machine limit, shared by every app) to compare MAX_RUNNERS with the room actually left"}
	}
	maxRunners := 0
	if _, err := fmt.Sscanf(file.Env["MAX_RUNNERS"], "%d", &maxRunners); err != nil {
		return doctorCheck{Name: "fleet capacity margin", Status: "warn", Details: "MAX_RUNNERS is not set in the committed configuration"}
	}
	output, err := run("fly", "apps", "list", "--json")
	if err != nil {
		return doctorCheck{Name: "fleet capacity margin", Status: "warn", Details: "cannot list apps: " + compactError(output, err)}
	}
	var apps []struct {
		Name         string `json:"Name"`
		Organization struct {
			Slug string `json:"Slug"`
		} `json:"Organization"`
	}
	if err := json.Unmarshal(output, &apps); err != nil {
		return doctorCheck{Name: "fleet capacity margin", Status: "warn", Details: "apps list is not the expected JSON: " + err.Error()}
	}
	organization, found := "", false
	for _, app := range apps {
		if app.Name == workerApp {
			organization, found = app.Organization.Slug, true
			break
		}
	}
	if !found {
		return doctorCheck{Name: "fleet capacity margin", Status: "warn", Details: "worker app " + workerApp + " is not in `fly apps list`; check --worker-app and the Fly account"}
	}
	others, workers := 0, 0
	for _, app := range apps {
		// A list without organizations (an older flyctl) counts every app, as before.
		if organization != "" && app.Organization.Slug != organization {
			continue
		}
		list, err := run("fly", "machines", "list", "--app", app.Name, "--json")
		if err != nil {
			return doctorCheck{Name: "fleet capacity margin", Status: "warn", Details: "cannot list the Machines of " + app.Name + ": " + compactError(list, err)}
		}
		var machines []json.RawMessage
		if err := json.Unmarshal(list, &machines); err != nil {
			return doctorCheck{Name: "fleet capacity margin", Status: "warn", Details: "Machines of " + app.Name + " are not the expected JSON: " + err.Error()}
		}
		if app.Name == workerApp {
			workers = len(machines)
		} else {
			others += len(machines)
		}
	}
	scope := "other apps"
	if organization != "" {
		scope = "other apps of organization " + organization
	}
	margin := doctorFlyMachineLimit - others
	detail := fmt.Sprintf("MAX_RUNNERS %d; %d Machines belong to %s (stopped ones count), %d to the fleet now; room for %d workers under a limit of %d", maxRunners, others, scope, workers, margin, doctorFlyMachineLimit)
	if maxRunners > margin {
		return doctorCheck{Name: "fleet capacity margin", Status: "fail", Details: detail + "; the provider will refuse the launch that crosses the limit"}
	}
	return doctorCheck{Name: "fleet capacity margin", Status: "pass", Details: detail}
}
