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

	flyprovider "github.com/gwendall/runneryard/providers/fly"
	hetznerprovider "github.com/gwendall/runneryard/providers/hetzner"
)

// budget estimate sizes RUNNER_USAGE_BUDGET and the Fly or Hetzner compute
// bill from a workload before anything is routed. It is offline: it reads no ledger, no
// provider, and no GitHub API, only the operator's own numbers.
//
// The ledger charges each worker from its creation to its job's finish plus a
// teardown grace, so every job costs its own duration plus the machine's boot
// and registration. Each launch also reserves a full RUNNER_MAX_LIFETIME until
// it settles, so a budget sized to usage alone stops admitting workers before
// that usage is reached. Both were missed in practice: one fleet ran 8.7 days
// from exhaustion on 2026-09-05, and another workload measured on 2026-10-08
// needed 34 times the scaffold's 166h40m.

// Fly Machines rates live in providers/fly, with the pool host source that
// prices its hosts from them.

// Hetzner list prices live in providers/hetzner. Every RunnerYard worker is a
// new server; Hetzner rounds each server's usage up to the next hour and bills
// it until deletion, so a worker is billed whole hours however short its job.
// Other types and locations need --hourly-price.

// defaultJobOverhead is the worker time charged around each job: about a
// minute of boot and registration, plus the 15 s teardown grace the ledger
// adds to GitHub's finish time.
const defaultJobOverhead = 75 * time.Second

type budgetWorkload struct {
	Provider        string
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
	ServerType      string
	HourlyPriceEUR  float64
}

type budgetEstimate struct {
	PerJob            time.Duration
	Charged           time.Duration
	LaunchHeadroom    time.Duration
	SuggestedBudget   time.Duration
	PricePerMinuteUSD float64
	ComputeUSD        float64

	// Hetzner only: the hourly price used and the whole hours each job's
	// server is billed.
	PricePerHourEUR   float64
	BilledHoursPerJob int64
	ComputeEUR        float64
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
	}

	perJob := time.Duration(w.MinutesPerJob*float64(time.Minute)) + w.JobOverhead
	jobs := w.JobsPerDay * w.Window.Hours() / 24
	charged := time.Duration(jobs * float64(perJob))
	headroom := time.Duration(w.MaxRunners) * w.MaxLifetime
	suggested := time.Duration(float64(charged)*(1+w.Margin)) + headroom
	suggested = time.Duration(math.Ceil(suggested.Hours())) * time.Hour
	estimate := budgetEstimate{
		PerJob:          perJob,
		Charged:         charged,
		LaunchHeadroom:  headroom,
		SuggestedBudget: suggested,
	}

	switch w.Provider {
	case "fly":
		return estimateFlyCompute(w, estimate)
	case "hetzner":
		return estimateHetznerCompute(w, estimate, jobs)
	default:
		return budgetEstimate{}, fmt.Errorf("--provider must be fly or hetzner, got %q", w.Provider)
	}
}

func estimateFlyCompute(w budgetWorkload, e budgetEstimate) (budgetEstimate, error) {
	switch {
	case !(w.PriceMultiplier > 0):
		return budgetEstimate{}, errors.New("--price-multiplier must be greater than zero")
	case w.CPUs < 1 || w.MemoryMB < 1:
		return budgetEstimate{}, errors.New("--cpus and --memory-mb must be at least 1")
	}

	if w.CPUKind != "performance" && w.CPUKind != "shared" {
		return budgetEstimate{}, fmt.Errorf("--cpu-kind must be performance or shared, got %q", w.CPUKind)
	}
	basePerSecond, err := flyprovider.PricePerSecond(w.CPUKind, w.CPUs, w.MemoryMB)
	if err != nil {
		return budgetEstimate{}, err
	}
	pricePerSecond := basePerSecond * w.PriceMultiplier
	e.PricePerMinuteUSD = pricePerSecond * 60
	e.ComputeUSD = e.Charged.Seconds() * pricePerSecond
	return e, nil
}

// estimateHetznerCompute bills each job's server whole hours: one worker per
// job, each rounded up to the next hour.
func estimateHetznerCompute(w budgetWorkload, e budgetEstimate, jobs float64) (budgetEstimate, error) {
	price := w.HourlyPriceEUR
	switch {
	case price < 0 || math.IsNaN(price):
		return budgetEstimate{}, errors.New("--hourly-price must not be negative")
	case price == 0:
		listed, ok := hetznerprovider.HourlyPriceEUR(w.ServerType)
		if !ok {
			return budgetEstimate{}, fmt.Errorf("no built-in price for server type %q; pass --hourly-price from hetzner.com/cloud", w.ServerType)
		}
		price = listed
	}
	e.PricePerHourEUR = price
	e.BilledHoursPerJob = int64((e.PerJob + time.Hour - 1) / time.Hour)
	e.ComputeEUR = jobs * float64(e.BilledHoursPerJob) * price
	return e, nil
}

func runBudgetEstimate(args []string) error {
	flags := flag.NewFlagSet("budget estimate", flag.ContinueOnError)
	flags.SetOutput(os.Stdout)
	var w budgetWorkload
	flags.StringVar(&w.Provider, "provider", "fly", "compute provider to price: fly or hetzner")
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
	flags.StringVar(&w.ServerType, "server-type", "cpx32", "Hetzner: RUNNER_HETZNER_SERVER_TYPE")
	flags.Float64Var(&w.HourlyPriceEUR, "hourly-price", 0, "Hetzner: EUR per started hour; defaults to the built-in Germany and Finland list price")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	w.Provider = strings.ToLower(strings.TrimSpace(w.Provider))
	w.CPUKind = strings.ToLower(strings.TrimSpace(w.CPUKind))
	w.ServerType = strings.ToLower(strings.TrimSpace(w.ServerType))
	if err := rejectOtherProviderFlags(flags, w.Provider); err != nil {
		return err
	}
	estimate, err := estimateBudget(w)
	if err != nil {
		return err
	}
	writeBudgetEstimate(os.Stdout, w, estimate)
	return nil
}

// rejectOtherProviderFlags refuses a shape flag that the selected provider
// would silently ignore, so an estimate never prices a shape nobody asked for.
func rejectOtherProviderFlags(flags *flag.FlagSet, provider string) error {
	owner := map[string]string{
		"cpu-kind": "fly", "cpus": "fly", "memory-mb": "fly", "price-multiplier": "fly",
		"server-type": "hetzner", "hourly-price": "hetzner",
	}
	var err error
	flags.Visit(func(f *flag.Flag) {
		if want, ok := owner[f.Name]; ok && want != provider && err == nil {
			err = fmt.Errorf("--%s applies to --provider %s only", f.Name, want)
		}
	})
	return err
}

func writeBudgetEstimate(out io.Writer, w budgetWorkload, e budgetEstimate) {
	fmt.Fprintf(out, "Per job:          %s charged (%s of job, %s of boot, registration and teardown)\n",
		e.PerJob, time.Duration(w.MinutesPerJob*float64(time.Minute)), w.JobOverhead)
	fmt.Fprintf(out, "Charged time:     %s per %s window at %g jobs a day\n", formatHours(e.Charged), formatHours(w.Window), w.JobsPerDay)
	fmt.Fprintf(out, "Launch headroom:  %s (%d workers each reserving RUNNER_MAX_LIFETIME %s)\n", formatHours(e.LaunchHeadroom), w.MaxRunners, w.MaxLifetime)
	fmt.Fprintf(out, "Suggested:        RUNNER_USAGE_BUDGET = %q (charged time + %g%% margin + launch headroom)\n", formatHours(e.SuggestedBudget), w.Margin*100)
	if w.Provider == "hetzner" {
		used := e.PerJob.Hours() / float64(e.BilledHoursPerJob)
		fmt.Fprintf(out, "Hetzner compute:  EUR %.4f per started server hour (%s); each job's server is billed %dh for %s of use (%.0f%%), about EUR %.2f per window\n",
			e.PricePerHourEUR, w.ServerType, e.BilledHoursPerJob, e.PerJob, used*100, e.ComputeEUR)
		if used < 0.5 {
			fmt.Fprintln(out, "Hetzner rounds every server up to a whole hour and RunnerYard creates one server per job, so most of this bill is unused time. Compare with --provider fly, billed per second.")
		}
		fmt.Fprintln(out, "Not included: the always-on controller and its volume, each server's primary IPv4, image storage, network egress, workers that release themselves without a job, and taxes.")
		return
	}
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
