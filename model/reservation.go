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
	StartTime    time.Time    `json:"start_time"`
	EndTime      time.Time    `json:"end_time"`
	Status       string       `json:"status"`
	Description  string       `json:"description"`
	UpdatedAt    time.Time    `json:"updated_at"`
}

type AlertLevel string

const (
	LevelCritical  AlertLevel = "critical"  // <= 3 days
	LevelWarning   AlertLevel = "warning"   // <= 7 days
	LevelAttention AlertLevel = "attention" // <= 14 days
	LevelNormal    AlertLevel = "normal"    // <= 30 days
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
