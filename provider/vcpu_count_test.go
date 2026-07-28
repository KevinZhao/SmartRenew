package provider

import "testing"

func TestVCPUCount(t *testing.T) {
	tests := []struct {
		instanceType string
		want         int
	}{
		// Small sizes: nano/micro/small are 1 vCPU, medium is 2.
		{"t3.nano", 1},
		{"t3.micro", 1},
		{"t3.small", 1},
		{"t3.medium", 2},
		{"m5.large", 2},
		{"m5.xlarge", 4},
		{"m5.2xlarge", 8},
		{"c7i.4xlarge", 16},
		{"m5.12xlarge", 48},
		{"p4d.24xlarge", 96},
		{"p5.48xlarge", 192},
		{"x1e.32xlarge", 128},
		// Unknown / unsized forms return 0 so callers can skip them.
		{"m5.metal", 0},
		{"weird", 0},
		{"", 0},
		{"m5.999xlarge", 0},
	}
	for _, tc := range tests {
		t.Run(tc.instanceType, func(t *testing.T) {
			if got := vCPUCount(tc.instanceType); got != tc.want {
				t.Fatalf("vCPUCount(%q) = %d, want %d", tc.instanceType, got, tc.want)
			}
		})
	}
}

func TestVCPUCountScalesWithSizeMultiplier(t *testing.T) {
	// xlarge is 4 vCPU and each Nx multiplier scales from there.
	base := vCPUCount("m5.xlarge")
	if base != 4 {
		t.Fatalf("m5.xlarge = %d, want 4", base)
	}
	for _, tc := range []struct {
		it   string
		mult int
	}{
		{"m5.2xlarge", 2},
		{"m5.4xlarge", 4},
		{"m5.8xlarge", 8},
		{"m5.16xlarge", 16},
	} {
		if got, want := vCPUCount(tc.it), base*tc.mult; got != want {
			t.Errorf("vCPUCount(%q) = %d, want %d", tc.it, got, want)
		}
	}
}
