package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gwendall/runneryard/controller"
)

func runStatus(args []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(os.Stdout)
	defaultFile := envOr("RUNNER_STATUS_FILE", "/var/lib/runneryard/status.json")
	statusFile := flags.String("file", defaultFile, "controller fleet status file")
	asJSON := flags.Bool("json", false, "emit the stable JSON status schema")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("status does not accept positional arguments")
	}
	status, err := controller.LoadStatus(*statusFile)
	if err != nil {
		return err
	}
	return writeFleetStatus(os.Stdout, status, *asJSON)
}

func writeFleetStatus(output io.Writer, status controller.FleetStatus, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(output).Encode(status)
	}
	fmt.Fprintf(output, "RunnerYard %s — %s", status.Health, status.Controller.Provider)
	if status.Reason != "" {
		fmt.Fprintf(output, " (%s)", status.Reason)
	}
	fmt.Fprintln(output)
	fmt.Fprintf(output, "Controller  %s  %s (%s)\n", status.Controller.ID, status.Controller.Version, status.Controller.CommitSHA)
	fmt.Fprintf(output, "Scale set   %s  desired %d from %d assigned job(s)\n", status.GitHub.ScaleSet, status.GitHub.DesiredWorkers, status.GitHub.AssignedJobs)
	capacity := ""
	if status.Workers.Saturated {
		capacity = "  saturated"
	}
	fmt.Fprintf(output, "Workers     %d actual  %d starting  %d busy  %d idle  %d unknown  %d orphan candidate(s)  %d retirement(s) pending  max %d%s\n",
		status.Workers.Actual, status.Workers.Starting, status.Workers.Busy, status.Workers.Idle, status.Workers.Unknown, status.Workers.OrphanCandidates, status.Workers.PendingRetirements, status.Workers.Maximum, capacity)
	writeLatency(output, "Create", status.Latency.ProviderCreate, true)
	writeLatency(output, "Assignment", status.Latency.Assignment, false)
	fmt.Fprintf(output, "Budget      %s used  %s reserved  %s remaining / %s per %s\n",
		seconds(status.Budget.UsedSeconds), seconds(status.Budget.ReservedSeconds), seconds(status.Budget.RemainingSeconds), seconds(status.Budget.LimitSeconds), seconds(status.Budget.WindowSeconds))
	if !status.GitHub.LastActivityAt.IsZero() {
		fmt.Fprintf(output, "GitHub      %s at %s\n", status.GitHub.LastEvent, status.GitHub.LastActivityAt.Format(time.RFC3339))
	}
	if status.Budget.RefusalReason != "" && !status.Budget.NextAvailableAt.IsZero() {
		fmt.Fprintf(output, "Admission   %s; next release %s\n", status.Budget.RefusalReason, status.Budget.NextAvailableAt.Format(time.RFC3339))
	}
	fmt.Fprintf(output, "Updated     %s\n", status.UpdatedAt.Format(time.RFC3339))
	return nil
}

func writeLatency(output io.Writer, label string, stats controller.LatencyStats, failures bool) {
	if stats.Samples == 0 {
		fmt.Fprintf(output, "%-11s no samples\n", label)
		return
	}
	fmt.Fprintf(output, "%-11s last %s  avg %s  max %s", label, milliseconds(stats.LastMS), milliseconds(int64(stats.AverageMS)), milliseconds(stats.MaximumMS))
	if failures {
		fmt.Fprintf(output, "  %d failure(s) / %d sample(s)", stats.Failures, stats.Samples)
	} else {
		fmt.Fprintf(output, "  %d sample(s)", stats.Samples)
	}
	fmt.Fprintln(output)
}

func milliseconds(value int64) time.Duration { return time.Duration(value) * time.Millisecond }
func seconds(value int64) time.Duration      { return time.Duration(value) * time.Second }
