package model

import (
	"strconv"
	"time"
)

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
	// EquivCores is the capacity the commitment covers, expressed in the unit
	// named by CapacityUnit — accelerator cards for GPU families with a known
	// card count, otherwise vCPU cores. Populated for SPs and Capacity Blocks.
	EquivCores float64 `json:"equiv_cores"`
	// CapacityUnit is what EquivCores counts: "cards", "cores", or "" when
	// unknown. It is stored alongside the number so the UI never has to infer
	// the unit from the instance family name.
	CapacityUnit string    `json:"capacity_unit"`
	UpdatedAt    time.Time `json:"updated_at"`
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
	// Threshold is the configured remind_days value this alert fired for, and
	// forms part of the dedup key so each configured reminder fires once.
	// Zero when the expiry is beyond every configured threshold.
	Threshold int `json:"threshold"`
}

// NotifyKey identifies which reminder this alert represents, for dedup
// purposes. It is the crossed threshold when one is configured, otherwise the
// coarse level.
//
// Keying on the threshold rather than the level is what makes every configured
// remind_days value fire: with [30,14,7,3,1] both the 3-day and the 1-day
// reminder map to level "critical", so a level-keyed log would send only the
// first of them and silently drop the 1-day warning.
func (a Alert) NotifyKey() string {
	if a.Threshold > 0 {
		return "t" + strconv.Itoa(a.Threshold)
	}
	return string(a.Level)
}

// CrossedThreshold returns the smallest configured remind_days value that is
// still >= daysLeft — that is, the reminder this resource has just reached.
// Returns 0 when the expiry is further out than every configured threshold.
//
// With remind_days [30,14,7,3,1] a resource 20 days out reports 30 (it has
// crossed the 30-day mark but not yet the 14-day one); at 10 days it reports
// 14. Because the value changes as the expiry approaches, using it in the
// dedup key makes every configured reminder fire exactly once.
func CrossedThreshold(daysLeft int, remindDays []int) int {
	best := 0
	for _, d := range remindDays {
		if d < daysLeft {
			continue
		}
		if best == 0 || d < best {
			best = d
		}
	}
	if best == 0 && daysLeft <= 0 {
		// Already expired: attribute it to the most urgent configured reminder.
		for _, d := range remindDays {
			if best == 0 || d < best {
				best = d
			}
		}
	}
	return best
}

// DaysUntil returns whole days from now until end, rounded up so that any
// remaining fraction of a day still counts as a day: a resource expiring in 2
// hours reports 1, matching how an operator reads "1 day left". An expiry in
// the past yields zero or a negative number.
//
// This is the single definition used by the API, the notifiers and the UI —
// they previously disagreed (truncation vs. rounding up vs. no rounding), so
// the same resource showed different day counts in different places.
func DaysUntil(end, now time.Time) int {
	if end.IsZero() {
		return 0
	}
	d := end.Sub(now)
	days := int(d / (24 * time.Hour))
	if d > 0 && d%(24*time.Hour) != 0 {
		days++
	}
	return days
}

// NewAlert builds an Alert with DaysLeft, Level and Threshold derived
// consistently from a single day count.
func NewAlert(r Reservation, now time.Time, remindDays []int) Alert {
	days := DaysUntil(r.EndTime, now)
	return Alert{
		Reservation: r,
		DaysLeft:    days,
		Level:       CalcAlertLevel(days),
		Threshold:   CrossedThreshold(days, remindDays),
	}
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
