package provider

import (
	"strconv"
	"strings"
)

func parseFloatSafe(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

// vCPUCount returns the vCPU count for an EC2 instance type based on its
// size suffix. Works for all modern AWS instance families where size naming
// follows the large/xlarge/2xlarge/... convention (m/c/r/t/p/g/etc).
// Returns 0 for unknown formats (e.g. bare-metal variants without explicit sizing).
func vCPUCount(instanceType string) int {
	idx := strings.LastIndex(instanceType, ".")
	if idx < 0 {
		return 0
	}
	size := instanceType[idx+1:]
	switch size {
	case "nano", "micro", "small":
		return 1
	case "medium":
		return 2
	case "large":
		return 2
	case "xlarge":
		return 4
	case "2xlarge":
		return 8
	case "3xlarge":
		return 12
	case "4xlarge":
		return 16
	case "6xlarge":
		return 24
	case "8xlarge":
		return 32
	case "9xlarge":
		return 36
	case "12xlarge":
		return 48
	case "16xlarge":
		return 64
	case "18xlarge":
		return 72
	case "24xlarge":
		return 96
	case "32xlarge":
		return 128
	case "48xlarge":
		return 192
	case "56xlarge":
		return 224
	case "96xlarge":
		return 384
	case "metal-16xl":
		return 64
	case "metal-24xl":
		return 96
	case "metal-32xl":
		return 128
	case "metal-48xl":
		return 192
	}
	return 0
}

// Compute SP reference instance — used to derive a per-vCPU rate from a
// Compute SP (which spans all families and has no single "family rate").
// c7i.xlarge is a modern general-purpose Intel instance available in most regions.
// Ordered fallback list: try each in turn when the SP's rate table lacks c7i.xlarge.
var computeSPReferenceFallbacks = []string{
	"c7i.xlarge", "c7g.xlarge", "c6i.xlarge", "c6g.xlarge",
	"m7i.xlarge", "m7g.xlarge", "m6i.xlarge", "m6g.xlarge",
}

// normalizeSPRate converts a raw SP rate table into a unit $/hr figure for the UI:
//   - GPU family SP → top rate ÷ GPU card count (unit = $/card; equivCores = commitment ÷ unit)
//   - CPU family SP → top rate ÷ vCPU count     (unit = $/vCPU; equivCores = commitment ÷ unit)
//   - Compute SP    → reference instance rate ÷ vCPU (unit = $/vCPU; equivCores = commitment ÷ unit)
func normalizeSPRate(spType string, rates map[string]float64, commitmentStr string) (unitRate, equivCores float64) {
	if len(rates) == 0 {
		return 0, 0
	}
	commitment, _ := parseFloatSafe(commitmentStr)

	if spType == "Compute" {
		var refIType string
		var refRate float64
		for _, cand := range computeSPReferenceFallbacks {
			if r, ok := rates[cand]; ok && r > 0 {
				refIType = cand
				refRate = r
				break
			}
		}
		if refIType == "" {
			return 0, 0
		}
		vcpu := vCPUCount(refIType)
		if vcpu <= 0 {
			return 0, 0
		}
		unitRate = refRate / float64(vcpu)
		if unitRate > 0 && commitment > 0 {
			equivCores = commitment / unitRate
		}
		return unitRate, equivCores
	}

	// Family-scoped SP (EC2Instance type): pick highest rate in the family's rate table.
	var topRate float64
	var topIType string
	for itype, r := range rates {
		if r > topRate {
			topRate = r
			topIType = itype
		}
	}
	if topIType == "" {
		return 0, 0
	}
	if cards := GPUCardCount(topIType); cards > 0 {
		unitRate = topRate / float64(cards)
	} else if vcpu := vCPUCount(topIType); vcpu > 0 {
		unitRate = topRate / float64(vcpu)
	} else {
		unitRate = topRate
	}
	if unitRate > 0 && commitment > 0 {
		equivCores = commitment / unitRate
	}
	return unitRate, equivCores
}
