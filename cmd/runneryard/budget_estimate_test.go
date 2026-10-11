package main

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"
)

func estimateWorkload() budgetWorkload {
	return budgetWorkload{
		JobsPerDay:      100,
		MinutesPerJob:   3,
		JobOverhead:     time.Minute,
		MaxRunners:      10,
		MaxLifetime:     2 * time.Hour,
		Window:          720 * time.Hour,
		Margin:          0.2,
		CPUKind:         "performance",
		CPUs:            2,
		MemoryMB:        8192,
		PriceMultiplier: 1,
	}
}

func TestBudgetEstimateChargesOverheadAndReservesLaunchHeadroom(t *testing.T) {
	estimate, err := estimateBudget(estimateWorkload())
	if err != nil {
		t.Fatal(err)
	}
	// 3 000 jobs in 30 days at 3 min of job plus 1 min of boot each.
	if estimate.PerJob != 4*time.Minute || estimate.Charged != 200*time.Hour {
		t.Fatalf("per job %s, charged %s; want 4m0s and 200h0m0s", estimate.PerJob, estimate.Charged)
	}
	if estimate.LaunchHeadroom != 20*time.Hour {
		t.Fatalf("launch headroom = %s, want 10 workers x 2h", estimate.LaunchHeadroom)
	}
	// 200h + 20% + 20h of reservations.
	if estimate.SuggestedBudget != 260*time.Hour {
		t.Fatalf("suggested budget = %s, want 260h", estimate.SuggestedBudget)
	}
	// 2 performance vCPUs include 4 GB, so 4 GB are extra.
	pricePerSecond := 2*flyPerformanceVCPUPerSecond + 4*flyRAMPerGBPerSecond
	if math.Abs(estimate.ComputeUSD-720_000*pricePerSecond) > 0.01 {
		t.Fatalf("compute = $%.4f, want $%.4f", estimate.ComputeUSD, 720_000*pricePerSecond)
	}
}

func TestBudgetEstimateRoundsTheSuggestionUpToAWholeHour(t *testing.T) {
	workload := estimateWorkload()
	workload.Margin = 0
	workload.MaxRunners = 1
	workload.MaxLifetime = 90 * time.Minute
	estimate, err := estimateBudget(workload)
	if err != nil {
		t.Fatal(err)
	}
	if estimate.SuggestedBudget != 202*time.Hour {
		t.Fatalf("suggested budget = %s, want 201h30m rounded up to 202h", estimate.SuggestedBudget)
	}
}

func TestBudgetEstimatePricesSharedShapesAndRegions(t *testing.T) {
	workload := estimateWorkload()
	workload.CPUKind = "shared"
	workload.MemoryMB = 512 // exactly what 2 shared vCPUs include
	workload.PriceMultiplier = 1.134615385
	estimate, err := estimateBudget(workload)
	if err != nil {
		t.Fatal(err)
	}
	want := 2 * flySharedVCPUPerSecond * 1.134615385 * 60
	if math.Abs(estimate.PricePerMinuteUSD-want) > 1e-12 {
		t.Fatalf("price per minute = %g, want %g", estimate.PricePerMinuteUSD, want)
	}
}

func TestBudgetEstimateRejectsImpossibleWorkloads(t *testing.T) {
	for name, mutate := range map[string]func(*budgetWorkload){
		"no jobs":              func(w *budgetWorkload) { w.JobsPerDay = 0 },
		"no job time":          func(w *budgetWorkload) { w.MinutesPerJob = 0 },
		"NaN job time":         func(w *budgetWorkload) { w.MinutesPerJob = math.NaN() },
		"negative overhead":    func(w *budgetWorkload) { w.JobOverhead = -time.Second },
		"no runners":           func(w *budgetWorkload) { w.MaxRunners = 0 },
		"lifetime over 24h":    func(w *budgetWorkload) { w.MaxLifetime = 25 * time.Hour },
		"window under an hour": func(w *budgetWorkload) { w.Window = 30 * time.Minute },
		"negative margin":      func(w *budgetWorkload) { w.Margin = -0.1 },
		"free region":          func(w *budgetWorkload) { w.PriceMultiplier = 0 },
		"unknown cpu kind":     func(w *budgetWorkload) { w.CPUKind = "exotic" },
		"no memory":            func(w *budgetWorkload) { w.MemoryMB = 0 },
	} {
		workload := estimateWorkload()
		mutate(&workload)
		if _, err := estimateBudget(workload); err == nil {
			t.Errorf("%s: estimate accepted %+v", name, workload)
		}
	}
}

func TestBudgetEstimatePrintsABudgetTheControllerAccepts(t *testing.T) {
	workload := estimateWorkload()
	estimate, err := estimateBudget(workload)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	writeBudgetEstimate(&out, workload, estimate)
	line := `RUNNER_USAGE_BUDGET = "260h"`
	if !strings.Contains(out.String(), line) {
		t.Fatalf("output does not suggest %s:\n%s", line, out.String())
	}
	parsed, err := time.ParseDuration("260h")
	if err != nil || parsed < workload.MaxLifetime {
		t.Fatalf("suggestion %v must parse and cover one RUNNER_MAX_LIFETIME", parsed)
	}
}

func TestBudgetDispatchesEstimate(t *testing.T) {
	if err := runBudget([]string{"estimate", "--jobs-per-day", "0", "--minutes-per-job", "3"}); err == nil {
		t.Fatal("budget estimate must reject a workload without jobs")
	}
	if err := runBudget([]string{"estimate", "--jobs-per-day", "100", "--minutes-per-job", "3", "extra"}); err == nil {
		t.Fatal("budget estimate must reject a stray argument")
	}
}
