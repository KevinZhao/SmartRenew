package model

import "time"

type OrgAccount struct {
	AccountID    string    `json:"account_id"`
	AccountName  string    `json:"account_name"`
	Email        string    `json:"email"`
	Status       string    `json:"status"`
	JoinedMethod string    `json:"joined_method"`
	JoinedAt     time.Time `json:"joined_at"`
	Tag          string    `json:"tag"`
	UpdatedAt    time.Time `json:"updated_at"`
}
