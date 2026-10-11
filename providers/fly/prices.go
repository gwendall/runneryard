package fly

import (
	"fmt"
	"math"
)

// Fly Machines per-second rates for iad and ewr, the lowest-priced regions
// (docs.fly.io/about/pricing, read 2026-10-11). Other regions apply a
// multiplier. Rates change; re-read the page before trusting an estimate for
// a live budget.
const (
	SharedVCPUPerSecond          = 0.0000008465
	PerformanceVCPUPerSecond     = 0.000012732
	RAMPerGBPerSecond            = 0.000002316
	SharedIncludedMBPerVCPU      = 256
	PerformanceIncludedMBPerVCPU = 2048
)

// regionMultipliers are the listed regional multipliers over the iad and ewr
// rates. A region missing here needs an explicit price.
var regionMultipliers = map[string]float64{
	"iad": 1,
	"ewr": 1,
	"cdg": 1.134615385,
}

// RegionMultiplier returns the listed price multiplier of a region.
func RegionMultiplier(region string) (float64, bool) {
	multiplier, ok := regionMultipliers[region]
	return multiplier, ok
}

// PricePerSecond is the iad and ewr price in USD of one running Machine of
// that shape: its vCPUs and the memory beyond what they include.
func PricePerSecond(cpuKind string, cpus, memoryMB int) (float64, error) {
	if cpus < 1 || memoryMB < 1 {
		return 0, fmt.Errorf("a Fly Machine has at least one vCPU and some memory")
	}
	var vcpu, includedMB float64
	switch cpuKind {
	case "performance":
		vcpu, includedMB = PerformanceVCPUPerSecond, PerformanceIncludedMBPerVCPU
	case "shared":
		vcpu, includedMB = SharedVCPUPerSecond, SharedIncludedMBPerVCPU
	default:
		return 0, fmt.Errorf("fly CPU kind must be performance or shared, got %q", cpuKind)
	}
	extraGB := math.Max(0, float64(memoryMB)-float64(cpus)*includedMB) / 1024
	return float64(cpus)*vcpu + extraGB*RAMPerGBPerSecond, nil
}

// HourlyPriceUSD is the price of one running Machine hour in a region with a
// listed multiplier; ok is false for any other region.
func HourlyPriceUSD(cpuKind string, cpus, memoryMB int, region string) (float64, bool) {
	multiplier, ok := RegionMultiplier(region)
	if !ok {
		return 0, false
	}
	perSecond, err := PricePerSecond(cpuKind, cpus, memoryMB)
	if err != nil {
		return 0, false
	}
	return perSecond * multiplier * 3600, true
}
