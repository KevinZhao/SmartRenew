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

// CapacityUnit says what a capacity figure counts. It travels with the number
// so the UI cannot mislabel it: the label and the value are derived from the
// same data. Guessing the unit from the instance family name instead produced
// "4608 cards" for a g7e plan whose figure was really a vCPU count, because
// g7e has no known card count but the name starts with "g7".
type CapacityUnit string

const (
	CapacityUnknown CapacityUnit = ""
	CapacityCards   CapacityUnit = "cards"
	CapacityCores   CapacityUnit = "cores"
)

// normalizeSPRate converts a raw SP rate table into a unit $/hr figure plus the
// capacity that unit rate implies, and reports which unit the capacity is in:
//   - GPU family SP → top rate ÷ card count (unit = $/card, capacity in cards)
//   - CPU family SP → top rate ÷ vCPU count (unit = $/vCPU, capacity in cores)
//   - Compute SP    → reference instance rate ÷ vCPU (unit = $/vCPU, cores)
//
// A GPU family absent from the card-count table yields cores, not cards: the
// only divisor available is the vCPU count, so the resulting figure is vCPUs.
// Add the family to Config.GPUCardCounts to get a per-card view instead.
func normalizeSPRate(spType string, rates map[string]float64, commitmentStr string) (unitRate, capacity float64, unit CapacityUnit) {
	if len(rates) == 0 {
		return 0, 0, CapacityUnknown
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
			return 0, 0, CapacityUnknown
		}
		vcpu := vCPUCount(refIType)
		if vcpu <= 0 {
			return 0, 0, CapacityUnknown
		}
		unitRate = refRate / float64(vcpu)
		return unitRate, capacityFor(commitment, unitRate), CapacityCores
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
		return 0, 0, CapacityUnknown
	}
	// The divisor must come from the same instance type as topRate, so the unit
	// rate and the capacity it implies describe the same thing.
	if cards := GPUCardCount(topIType); cards > 0 {
		unitRate = topRate / float64(cards)
		return unitRate, capacityFor(commitment, unitRate), CapacityCards
	}
	if vcpu := vCPUCount(topIType); vcpu > 0 {
		unitRate = topRate / float64(vcpu)
		return unitRate, capacityFor(commitment, unitRate), CapacityCores
	}
	// Neither card nor vCPU count is known: report the raw rate and no capacity,
	// rather than a number with no meaningful unit.
	return topRate, 0, CapacityUnknown
}

func capacityFor(commitment, unitRate float64) float64 {
	if unitRate <= 0 || commitment <= 0 {
		return 0
	}
	return commitment / unitRate
}
