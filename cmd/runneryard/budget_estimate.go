package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"
)

// budget estimate sizes RUNNER_USAGE_BUDGET and the Fly compute bill from a
// workload before anything is routed. It is offline: it reads no ledger, no
// provider, and no GitHub API, only the operator's own numbers.
//
// The ledger charges each worker from its creation to its job's finish plus a
// teardown grace, so every job costs its own duration plus the machine's boot
// and registration. Each launch also reserves a full RUNNER_MAX_LIFETIME until
// it settles, so a budget sized to usage alone stops admitting workers before
// that usage is reached. Both were missed in practice: one fleet ran 8.7 days
// from exhaustion on 2026-09-05, and another workload measured on 2026-10-08
// needed 34 times the scaffold's 166h40m.

// Fly Machines per-second rates for iad and ewr, the lowest-priced regions
// (docs.fly.io/about/pricing, read 2026-10-11). Other regions apply a
// multiplier: cdg is 1.134615385. Rates change; re-read the page before
// trusting an estimate for a live budget.
const (
	flySharedVCPUPerSecond          = 0.0000008465
	flyPerformanceVCPUPerSecond     = 0.000012732
	flyRAMPerGBPerSecond            = 0.000002316
	flySharedIncludedMBPerVCPU      = 256
	flyPerformanceIncludedMBPerVCPU = 2048
)

// defaultJobOverhead is the worker time charged around each job: about a
// minute of boot and registration, plus the 15 s teardown grace the ledger
// adds to GitHub's finish time.
const defaultJobOverhead = 75 * time.Second

type budgetWorkload struct {
	JobsPerDay      float64
	MinutesPerJob   float64
	JobOverhead     time.Duration
	MaxRunners      int
	MaxLifetime     time.Duration
	Window          time.Duration
	Margin          float64
	CPUKind         string
	CPUs            int
	MemoryMB        int
	PriceMultiplier float64
}

type budgetEstimate struct {
	PerJob            time.Duration
	Charged           time.Duration
	LaunchHeadroom    time.Duration
	SuggestedBudget   time.Duration
	PricePerMinuteUSD float64
	ComputeUSD        float64
}

func estimateBudget(w budgetWorkload) (budgetEstimate, error) {
	switch {
	case !(w.JobsPerDay > 0) || !(w.MinutesPerJob > 0):
		return budgetEstimate{}, errors.New("--jobs-per-day and --minutes-per-job must be greater than zero")
	case w.JobOverhead < 0:
		return budgetEstimate{}, errors.New("--overhead must not be negative")
	case w.MaxRunners < 1:
		return budgetEstimate{}, errors.New("--max-runners must be at least 1")
	case w.MaxLifetime < time.Minute || w.MaxLifetime > 24*time.Hour:
		return budgetEstimate{}, errors.New("--max-lifetime must be between 1m and 24h")
	case w.Window < time.Hour:
		return budgetEstimate{}, errors.New("--window must be at least 1h")
	case w.Margin < 0:
		return budgetEstimate{}, errors.New("--margin must not be negative")
	case !(w.PriceMultiplier > 0):
		return budgetEstimate{}, errors.New("--price-multiplier must be greater than zero")
	case w.CPUs < 1 || w.MemoryMB < 1:
		return budgetEstimate{}, errors.New("--cpus and --memory-mb must be at least 1")
	}

	var vcpuPerSecond, includedMBPerVCPU float64
	switch w.CPUKind {
	case "performance":
		vcpuPerSecond, includedMBPerVCPU = flyPerformanceVCPUPerSecond, flyPerformanceIncludedMBPerVCPU
	case "shared":
		vcpuPerSecond, includedMBPerVCPU = flySharedVCPUPerSecond, flySharedIncludedMBPerVCPU
	default:
		return budgetEstimate{}, fmt.Errorf("--cpu-kind must be performance or shared, got %q", w.CPUKind)
	}
	extraGB := math.Max(0, float64(w.MemoryMB)-float64(w.CPUs)*includedMBPerVCPU) / 1024
	pricePerSecond := (float64(w.CPUs)*vcpuPerSecond + extraGB*flyRAMPerGBPerSecond) * w.PriceMultiplier

	perJob := time.Duration(w.MinutesPerJob*float64(time.Minute)) + w.JobOverhead
	jobs := w.JobsPerDay * w.Window.Hours() / 24
	charged := time.Duration(jobs * float64(perJob))
	headroom := time.Duration(w.MaxRunners) * w.MaxLifetime
	suggested := time.Duration(float64(charged)*(1+w.Margin)) + headroom
	suggested = time.Duration(math.Ceil(suggested.Hours())) * time.Hour

	return budgetEstimate{
		PerJob:            perJob,
		Charged:           charged,
		LaunchHeadroom:    headroom,
		SuggestedBudget:   suggested,
		PricePerMinuteUSD: pricePerSecond * 60,
		ComputeUSD:        charged.Seconds() * pricePerSecond,
	}, nil
}

func runBudgetEstimate(args []string) error {
	flags := flag.NewFlagSet("budget estimate", flag.ContinueOnError)
	flags.SetOutput(os.Stdout)
	var w budgetWorkload
	flags.Float64Var(&w.JobsPerDay, "jobs-per-day", 0, "fleet jobs per day; every job is one worker")
	flags.Float64Var(&w.MinutesPerJob, "minutes-per-job", 0, "average job duration in minutes")
	flags.DurationVar(&w.JobOverhead, "overhead", defaultJobOverhead, "worker time charged around each job: boot, registration, teardown grace")
	flags.IntVar(&w.MaxRunners, "max-runners", 4, "MAX_RUNNERS; each running worker holds one RUNNER_MAX_LIFETIME in reserve")
	flags.DurationVar(&w.MaxLifetime, "max-lifetime", 2*time.Hour, "RUNNER_MAX_LIFETIME")
	flags.DurationVar(&w.Window, "window", 720*time.Hour, "RUNNER_BUDGET_WINDOW")
	flags.Float64Var(&w.Margin, "margin", 0.2, "headroom above the projected charge, 0.2 for 20%")
	flags.StringVar(&w.CPUKind, "cpu-kind", "performance", "RUNNER_CPU_KIND: performance or shared")
	flags.IntVar(&w.CPUs, "cpus", 2, "RUNNER_CPUS")
	flags.IntVar(&w.MemoryMB, "memory-mb", 8192, "RUNNER_MEMORY_MB")
	flags.Float64Var(&w.PriceMultiplier, "price-multiplier", 1, "Fly region multiplier over the iad/ewr rates (cdg: 1.1346)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	w.CPUKind = strings.ToLower(strings.TrimSpace(w.CPUKind))
	estimate, err := estimateBudget(w)
	if err != nil {
		return err
	}
	writeBudgetEstimate(os.Stdout, w, estimate)
	return nil
}

func writeBudgetEstimate(out io.Writer, w budgetWorkload, e budgetEstimate) {
	fmt.Fprintf(out, "Per job:          %s charged (%s of job, %s of boot, registration and teardown)\n",
		e.PerJob, time.Duration(w.MinutesPerJob*float64(time.Minute)), w.JobOverhead)
	fmt.Fprintf(out, "Charged time:     %s per %s window at %g jobs a day\n", formatHours(e.Charged), formatHours(w.Window), w.JobsPerDay)
	fmt.Fprintf(out, "Launch headroom:  %s (%d workers each reserving RUNNER_MAX_LIFETIME %s)\n", formatHours(e.LaunchHeadroom), w.MaxRunners, w.MaxLifetime)
	fmt.Fprintf(out, "Suggested:        RUNNER_USAGE_BUDGET = %q (charged time + %g%% margin + launch headroom)\n", formatHours(e.SuggestedBudget), w.Margin*100)
	fmt.Fprintf(out, "Fly compute:      $%.6f per worker-minute (%s, %d vCPU, %d MB, %gx the iad/ewr rate), about $%.2f per window\n",
		e.PricePerMinuteUSD, w.CPUKind, w.CPUs, w.MemoryMB, w.PriceMultiplier, e.ComputeUSD)
	fmt.Fprintln(out, "Not included: the always-on controller and its volume, image storage, network egress, workers that release themselves without a job, and taxes.")
}

// formatHours prints a duration as whole or fractional hours, a form
// time.ParseDuration accepts and operators can paste into the controller
// configuration.
func formatHours(d time.Duration) string {
	hours := d.Hours()
	if hours == math.Trunc(hours) {
		return fmt.Sprintf("%.0fh", hours)
	}
	return fmt.Sprintf("%.1fh", hours)
}
