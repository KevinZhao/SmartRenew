package model

import "time"

type CoverageType string

const (
	CoverageOnDemand CoverageType = "on_demand"
	CoverageSP       CoverageType = "savings_plan"
	CoverageCB       CoverageType = "capacity_block"
	CoverageRI       CoverageType = "reserved_instance"
)

type GPUCoverage struct {
	ID           string       `json:"id"`
	AccountAlias string       `json:"account_alias"`
	AccountID    string       `json:"account_id"`
	Region       string       `json:"region"`
	AZ           string       `json:"az"`
	InstanceID   string       `json:"instance_id"`
	InstanceType string       `json:"instance_type"`
	Coverage     CoverageType `json:"coverage"`
	CoverageRef  string       `json:"coverage_ref"`
	SPRate       float64      `json:"sp_rate"`
	UpdatedAt    time.Time    `json:"updated_at"`
}
