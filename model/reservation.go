package model

import "time"

type ResourceType string

const (
	TypeSP           ResourceType = "sp"
	TypeCB           ResourceType = "cb"
	TypeODCR         ResourceType = "odcr"
	TypeRI           ResourceType = "ri"
	TypeRDSRI        ResourceType = "rds_ri"
	TypeCacheRI      ResourceType = "cache_ri"
	TypeRedshiftRI   ResourceType = "redshift_ri"
	TypeOpenSearchRI ResourceType = "opensearch_ri"
	TypeMemoryDBRI   ResourceType = "memorydb_ri"
	TypeBedrockPT    ResourceType = "bedrock_pt"
)

type Reservation struct {
	ID           string       `json:"id"`
	AccountAlias string       `json:"account_alias"`
	AccountID    string       `json:"account_id"`
	Region       string       `json:"region"`
	Type         ResourceType `json:"type"`
	ResourceID   string       `json:"resource_id"`
	InstanceType string       `json:"instance_type"`
	Platform     string       `json:"platform"`
	Quantity     int          `json:"quantity"`
	UsedCount    int          `json:"used_count"`
	StartTime    time.Time    `json:"start_time"`
	EndTime      time.Time    `json:"end_time"`
	Status       string       `json:"status"`
	Description  string       `json:"description"`
	// HourlyRate is the SP unit $/hr:
	//   - GPU family SP → $/GPU card
	//   - CPU family SP → $/vCPU core
	//   - Compute SP    → $/vCPU core (via reference instance c7i.xlarge or fallbacks)
	// Not populated for non-SP resources.
	HourlyRate float64 `json:"hourly_rate"`
	// EquivCores is the effective vCPU capacity a Compute SP commitment can cover,
	// computed as commitment ÷ per-vCPU rate. Only populated for Compute SPs.
	EquivCores float64   `json:"equiv_cores"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type AlertLevel string

const (
	LevelCritical    AlertLevel = "critical"  // <= 3 days
	LevelWarning     AlertLevel = "warning"   // <= 7 days
	LevelAttention   AlertLevel = "attention" // <= 14 days
	LevelNormal      AlertLevel = "normal"    // <= 30 days
	LevelGPUOnDemand AlertLevel = "gpu_od"    // GPU running on on-demand pricing
)

type Alert struct {
	Reservation
	DaysLeft int        `json:"days_left"`
	Level    AlertLevel `json:"level"`
}

func CalcAlertLevel(daysLeft int) AlertLevel {
	switch {
	case daysLeft <= 3:
		return LevelCritical
	case daysLeft <= 7:
		return LevelWarning
	case daysLeft <= 14:
		return LevelAttention
	default:
		return LevelNormal
	}
}
