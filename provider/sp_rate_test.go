package provider

import (
	"math"
	"testing"
)

func TestNormalizeSPRateEmptyTable(t *testing.T) {
	unit, cap, kind := normalizeSPRate("EC2Instance", nil, "10.0")
	if unit != 0 || cap != 0 || kind != CapacityUnknown {
		t.Fatalf("got (%v, %v, %v), want zeroes and CapacityUnknown", unit, cap, kind)
	}
}

func TestNormalizeSPRateGPUFamily(t *testing.T) {
	// p5.48xlarge is 8×H100 at (say) $8/hr on-plan.
	rates := map[string]float64{"p5.48xlarge": 8.0}
	// A $16/hr commitment buys 2 instances = 16 cards.
	unit, cap, kind := normalizeSPRate("EC2Instance", rates, "16.0")

	if kind != CapacityCards {
		t.Fatalf("kind = %v, want CapacityCards", kind)
	}
	if want := 8.0 / 8.0; math.Abs(unit-want) > 1e-9 {
		t.Errorf("unit rate = %v, want %v ($/card)", unit, want)
	}
	if want := 16.0; math.Abs(cap-want) > 1e-9 {
		t.Errorf("capacity = %v, want %v cards", cap, want)
	}
}

func TestNormalizeSPRateCPUFamily(t *testing.T) {
	// m5.4xlarge = 16 vCPU at $0.64/hr → $0.04/vCPU.
	rates := map[string]float64{"m5.4xlarge": 0.64}
	unit, cap, kind := normalizeSPRate("EC2Instance", rates, "1.28")

	if kind != CapacityCores {
		t.Fatalf("kind = %v, want CapacityCores", kind)
	}
	if want := 0.04; math.Abs(unit-want) > 1e-9 {
		t.Errorf("unit rate = %v, want %v ($/vCPU)", unit, want)
	}
	if want := 32.0; math.Abs(cap-want) > 1e-9 {
		t.Errorf("capacity = %v, want %v cores", cap, want)
	}
}

// TestNormalizeSPRateUnknownGPUFamilyIsNotLabelledCards is the regression test
// for the g7e report: a GPU family missing from the card-count table fell
// through to the vCPU branch, so the figure was a vCPU count — but the UI
// labelled it "cards" purely from the "g7" name prefix, showing 4608 cards for
// what was really 4608 vCPUs (an 8x overstatement for an 8-card instance).
//
// The unit kind must be derived from the same data as the number.
func TestNormalizeSPRateUnknownGPUFamilyIsNotLabelledCards(t *testing.T) {
	// g7e is absent from defaultGPUCardCount, so only vCPU data is available.
	if GPUCardCount("g7e.48xlarge") != 0 {
		t.Skip("g7e now has a card count; this regression no longer applies")
	}
	rates := map[string]float64{"g7e.48xlarge": 24.349}
	unit, cap, kind := normalizeSPRate("EC2Instance", rates, "584.3712")

	if kind == CapacityCards {
		t.Errorf("kind = CapacityCards for a family with no known card count; "+
			"the value %.0f is a vCPU count and must not be presented as cards", cap)
	}
	if kind != CapacityCores {
		t.Errorf("kind = %v, want CapacityCores", kind)
	}
	// Sanity: the number itself is a vCPU total.
	if want := 584.3712 / (24.349 / 192); math.Abs(cap-want) > 1.0 {
		t.Errorf("capacity = %v, want ~%v vCPU", cap, want)
	}
	_ = unit
}

func TestNormalizeSPRateComputeUsesReferenceInstance(t *testing.T) {
	// c7i.xlarge = 4 vCPU at $0.12/hr → $0.03/vCPU.
	rates := map[string]float64{"c7i.xlarge": 0.12, "m5.24xlarge": 5.0}
	unit, cap, kind := normalizeSPRate("Compute", rates, "3.0")

	if kind != CapacityCores {
		t.Fatalf("kind = %v, want CapacityCores (Compute SP is always vCPU-based)", kind)
	}
	if want := 0.03; math.Abs(unit-want) > 1e-9 {
		t.Errorf("unit rate = %v, want %v", unit, want)
	}
	if want := 100.0; math.Abs(cap-want) > 1e-9 {
		t.Errorf("capacity = %v, want %v cores", cap, want)
	}
}

func TestNormalizeSPRateComputeFallbackOrder(t *testing.T) {
	// Without c7i.xlarge it must fall through the ordered candidate list.
	rates := map[string]float64{"m6g.xlarge": 0.08, "r5.4xlarge": 1.0}
	unit, _, kind := normalizeSPRate("Compute", rates, "1.0")
	if kind != CapacityCores {
		t.Fatalf("kind = %v, want CapacityCores", kind)
	}
	if want := 0.08 / 4; math.Abs(unit-want) > 1e-9 {
		t.Errorf("unit rate = %v, want %v (from m6g.xlarge)", unit, want)
	}
}

func TestNormalizeSPRateComputeNoUsableReference(t *testing.T) {
	// No candidate present → cannot derive a per-vCPU rate.
	rates := map[string]float64{"r5.4xlarge": 1.0}
	unit, cap, kind := normalizeSPRate("Compute", rates, "1.0")
	if unit != 0 || cap != 0 || kind != CapacityUnknown {
		t.Fatalf("got (%v, %v, %v), want zeroes and CapacityUnknown", unit, cap, kind)
	}
}

func TestNormalizeSPRateZeroCommitment(t *testing.T) {
	// A missing/unparsable commitment yields a unit rate but no capacity.
	rates := map[string]float64{"p5.48xlarge": 8.0}
	unit, cap, kind := normalizeSPRate("EC2Instance", rates, "")
	if unit <= 0 {
		t.Errorf("unit rate = %v, want a positive rate", unit)
	}
	if cap != 0 {
		t.Errorf("capacity = %v, want 0 when the commitment is unknown", cap)
	}
	if kind != CapacityCards {
		t.Errorf("kind = %v, want CapacityCards", kind)
	}
}

// TestNormalizeSPRatePicksSizeConsistentTopRate guards the unit/number pairing:
// the divisor must come from the very instance type whose rate was used.
func TestNormalizeSPRatePicksSizeConsistentTopRate(t *testing.T) {
	// Two sizes of the same GPU family. Whichever rate is picked, the per-card
	// figure must be identical because rate scales with card count.
	rates := map[string]float64{
		"g6.12xlarge": 2.0, // 4 cards → $0.50/card
		"g6.48xlarge": 4.0, // 8 cards → $0.50/card
	}
	unit, _, kind := normalizeSPRate("EC2Instance", rates, "8.0")
	if kind != CapacityCards {
		t.Fatalf("kind = %v, want CapacityCards", kind)
	}
	if want := 0.5; math.Abs(unit-want) > 1e-9 {
		t.Errorf("unit rate = %v, want %v — the divisor did not match the rate's instance type", unit, want)
	}
}

func TestNormalizeSPRateUnsizedInstanceType(t *testing.T) {
	// A rate table key with no parsable size gives neither cards nor cores.
	rates := map[string]float64{"weird-type": 1.5}
	unit, cap, kind := normalizeSPRate("EC2Instance", rates, "3.0")
	if kind != CapacityUnknown {
		t.Errorf("kind = %v, want CapacityUnknown for an unparsable instance type", kind)
	}
	if cap != 0 {
		t.Errorf("capacity = %v, want 0 — no basis to compute capacity", cap)
	}
	if want := 1.5; math.Abs(unit-want) > 1e-9 {
		t.Errorf("unit rate = %v, want the raw rate %v", unit, want)
	}
}
