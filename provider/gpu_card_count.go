package provider

// Built-in GPU card count per EC2 instance type.
// Source: AWS EC2 documentation (p5/p5en=8×H100/H200, p4d=8×A100, g5=variable, etc).
// Users can override/extend via Config.GPUCardCounts.
var defaultGPUCardCount = map[string]int{
	// P-series (NVIDIA H100 / H200 / A100)
	"p3.2xlarge": 1, "p3.8xlarge": 4, "p3.16xlarge": 8, "p3dn.24xlarge": 8,
	"p4d.24xlarge": 8, "p4de.24xlarge": 8,
	"p5.48xlarge": 8, "p5e.48xlarge": 8, "p5en.48xlarge": 8,
	"p6-b200.48xlarge": 8, "p6-b300.48xlarge": 8,
	"p6e-gb200.36xlarge": 4, "p6e-gb300.36xlarge": 4,

	// G5 (NVIDIA A10G)
	"g5.xlarge": 1, "g5.2xlarge": 1, "g5.4xlarge": 1, "g5.8xlarge": 1,
	"g5.16xlarge": 1, "g5.12xlarge": 4, "g5.24xlarge": 4, "g5.48xlarge": 8,
	// G5g (NVIDIA T4G)
	"g5g.xlarge": 1, "g5g.2xlarge": 1, "g5g.4xlarge": 1, "g5g.8xlarge": 1,
	"g5g.16xlarge": 2, "g5g.metal": 2,
	// G6 / G6e (NVIDIA L4)
	"g6.xlarge": 1, "g6.2xlarge": 1, "g6.4xlarge": 1, "g6.8xlarge": 1,
	"g6.16xlarge": 1, "g6.12xlarge": 4, "g6.24xlarge": 4, "g6.48xlarge": 8,
	"g6e.xlarge": 1, "g6e.2xlarge": 1, "g6e.4xlarge": 1, "g6e.8xlarge": 1,
	"g6e.16xlarge": 1, "g6e.12xlarge": 4, "g6e.24xlarge": 4, "g6e.48xlarge": 8,

	// G4 (NVIDIA T4 / AMD Radeon Pro V520)
	"g4dn.xlarge": 1, "g4dn.2xlarge": 1, "g4dn.4xlarge": 1, "g4dn.8xlarge": 1,
	"g4dn.16xlarge": 1, "g4dn.12xlarge": 4, "g4dn.metal": 8,
	"g4ad.xlarge": 1, "g4ad.2xlarge": 1, "g4ad.4xlarge": 1, "g4ad.8xlarge": 2,
	"g4ad.16xlarge": 4,

	// Inferentia
	"inf1.xlarge": 1, "inf1.2xlarge": 1, "inf1.6xlarge": 4, "inf1.24xlarge": 16,
	"inf2.xlarge": 1, "inf2.8xlarge": 1, "inf2.24xlarge": 6, "inf2.48xlarge": 12,

	// Trainium
	"trn1.2xlarge": 1, "trn1.32xlarge": 16, "trn1n.32xlarge": 16,
	"trn2.48xlarge": 16, "trn2u.48xlarge": 16,

	// Deep Learning
	"dl1.24xlarge": 8, "dl2q.24xlarge": 8,
}

// gpuCardOverrides holds runtime overrides (populated by main from config).
var gpuCardOverrides = map[string]int{}

// SetGPUCardOverrides replaces the runtime override table.
// Keys can be either full instance_type (e.g. "p5.48xlarge") or family (e.g. "p5").
// Family-level keys apply to any instance_type sharing that family.
func SetGPUCardOverrides(m map[string]int) {
	gpuCardOverrides = map[string]int{}
	for k, v := range m {
		if v > 0 {
			gpuCardOverrides[k] = v
		}
	}
}

// GPUCardCount returns the number of accelerator cards per instance for the given
// instance type. Resolution order: user override (exact) → user override (family)
// → built-in default. Returns 0 when unknown (caller should treat as non-GPU / skip).
func GPUCardCount(instanceType string) int {
	if n, ok := gpuCardOverrides[instanceType]; ok {
		return n
	}
	family := instanceFamily(instanceType)
	if n, ok := gpuCardOverrides[family]; ok {
		return n
	}
	if n, ok := defaultGPUCardCount[instanceType]; ok {
		return n
	}
	return 0
}
