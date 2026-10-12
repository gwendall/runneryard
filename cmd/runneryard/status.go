package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gwendall/runneryard/controller"
	"github.com/gwendall/runneryard/pool"
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
	fmt.Fprintf(output, "RunnerYard %s - %s", status.Health, status.Controller.Provider)
	if status.Reason != "" {
		fmt.Fprintf(output, " (%s)", status.Reason)
	}
	fmt.Fprintln(output)
	fmt.Fprintf(output, "Controller  %s  %s (%s)\n", status.Controller.ID, status.Controller.Version, status.Controller.CommitSHA)
	if len(status.GitHub.ScaleSets) == 0 {
		fmt.Fprintf(output, "Scale set   %s  desired %d from %d assigned job(s)\n", status.GitHub.ScaleSet, status.GitHub.DesiredWorkers, status.GitHub.AssignedJobs)
	}
	for _, scaleSet := range status.GitHub.ScaleSets {
		fmt.Fprintf(output, "Scale set   %s  desired %d from %d assigned job(s)  %d worker(s)\n", scaleSet.Name, scaleSet.DesiredWorkers, scaleSet.AssignedJobs, scaleSet.Workers)
	}
	capacity := ""
	if status.Workers.Saturated {
		capacity = "  saturated"
	}
	fmt.Fprintf(output, "Workers     %d actual  %d starting  %d busy  %d idle  %d unknown  %d orphan candidate(s)  %d retirement(s) pending  max %d%s\n",
		status.Workers.Actual, status.Workers.Starting, status.Workers.Busy, status.Workers.Idle, status.Workers.Unknown, status.Workers.OrphanCandidates, status.Workers.PendingRetirements, status.Workers.Maximum, capacity)
	configuredCapacity := status.Capacity.Configured
	effectiveCapacity := status.Capacity.Effective
	if configuredCapacity == 0 {
		// Capacity is additive to schema v2. Keep status files written by an
		// older controller useful during a rolling controller/CLI upgrade.
		configuredCapacity = status.Workers.Maximum
		effectiveCapacity = status.Workers.Maximum
	}
	fmt.Fprintf(output, "Capacity    %d effective / %d configured  %d provider rejection(s)", effectiveCapacity, configuredCapacity, status.Capacity.Rejections)
	if status.Capacity.Rejection != "" {
		fmt.Fprintf(output, "  %s", status.Capacity.Rejection)
	}
	if !status.Capacity.RetryAt.IsZero() {
		fmt.Fprintf(output, "  retry after %s", status.Capacity.RetryAt.Format(time.RFC3339))
	}
	fmt.Fprintln(output)
	if status.Launch.Rejections > 0 || status.Launch.Rejection != "" {
		fmt.Fprintf(output, "Launch      %d provider rejection(s)", status.Launch.Rejections)
		if status.Launch.Rejection != "" {
			fmt.Fprintf(output, "  %s", status.Launch.Rejection)
		}
		if !status.Launch.RetryAt.IsZero() {
			fmt.Fprintf(output, "  retry after %s", status.Launch.RetryAt.Format(time.RFC3339))
		}
		fmt.Fprintln(output)
	}
	writeLatency(output, "Create", status.Latency.ProviderCreate, true)
	writeLatency(output, "Assignment", status.Latency.Assignment, false)
	fmt.Fprintf(output, "Budget      %s used  %s reserved  %s remaining / %s per %s\n",
		seconds(status.Budget.UsedSeconds), seconds(status.Budget.ReservedSeconds), seconds(status.Budget.RemainingSeconds), seconds(status.Budget.LimitSeconds), seconds(status.Budget.WindowSeconds))
	if status.Budget.BurnSecondsPerDay > 0 {
		fmt.Fprintf(output, "Burn        %s per day  horizon %s at this rate\n", seconds(status.Budget.BurnSecondsPerDay), seconds(status.Budget.HorizonSeconds))
	}
	if !status.GitHub.LastActivityAt.IsZero() {
		fmt.Fprintf(output, "GitHub      %s at %s\n", status.GitHub.LastEvent, status.GitHub.LastActivityAt.Format(time.RFC3339))
	}
	if status.Budget.RefusalReason != "" && !status.Budget.NextAvailableAt.IsZero() {
		fmt.Fprintf(output, "Admission   %s; next release %s\n", status.Budget.RefusalReason, status.Budget.NextAvailableAt.Format(time.RFC3339))
	}
	writePools(output, status.Compute)
	fmt.Fprintf(output, "Updated     %s\n", status.UpdatedAt.Format(time.RFC3339))
	return nil
}

// writePools prints each pool's occupancy and cost since it started measuring,
// then each host's, when the compute status has pools.
func writePools(output io.Writer, compute json.RawMessage) {
	var placement pool.PlacementStatus
	if len(compute) == 0 || json.Unmarshal(compute, &placement) != nil {
		return
	}
	for _, p := range placement.Pools {
		fmt.Fprintf(output, "Pool        %s  %d host(s)  occupancy %s (ready %s)  cost %s",
			p.Name, len(p.Hosts), percent(p.Occupancy), percent(p.ReadyOccupancy), money(p.MeasuredCost, p.Currency))
		if p.EphemeralEquivalent != nil {
			fmt.Fprintf(output, "  as ephemeral %s", money(p.EphemeralEquivalent.Cost, p.EphemeralEquivalent.Currency))
		}
		fmt.Fprintf(output, "  since %s\n", p.MeasuredSince.Format(time.RFC3339))
		for _, host := range p.Hosts {
			fmt.Fprintf(output, "  %s  %s  %d slot(s)  running %s  occupancy %s (ready %s)\n",
				host.ID, host.Phase, host.Slots, seconds(host.RunningSeconds), percent(host.Occupancy), percent(host.ReadyOccupancy))
		}
	}
}

func percent(share float64) string { return fmt.Sprintf("%.1f%%", share*100) }

func money(amount float64, currency string) string {
	if currency == "" {
		return fmt.Sprintf("%.2f", amount)
	}
	return fmt.Sprintf("%s %.2f", currency, amount)
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
