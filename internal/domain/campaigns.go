package domain

import "time"

const (
	CampaignActionRefreshDiagnostics = "REFRESH_DIAGNOSTICS"
	CampaignOperationType            = "REFRESH_DIAGNOSTICS_CAMPAIGN"

	CampaignTargetPending   = "PENDING"
	CampaignTargetRunning   = "RUNNING"
	CampaignTargetSucceeded = "SUCCEEDED"
	CampaignTargetFailed    = "FAILED"
	CampaignTargetBlocked   = "BLOCKED"
)

type CampaignTarget struct {
	CampaignID   string     `json:"campaign_id"`
	InstanceID   string     `json:"instance_id"`
	InstanceName string     `json:"instance_name"`
	RequestID    string     `json:"request_id,omitempty"`
	Status       string     `json:"status"`
	Detail       string     `json:"detail,omitempty"`
	RequestedAt  *time.Time `json:"requested_at,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type Campaign struct {
	ID          string           `json:"id"`
	Action      string           `json:"action"`
	Status      string           `json:"status"`
	Summary     string           `json:"summary"`
	Concurrency int              `json:"concurrency"`
	RetryOf     string           `json:"retry_of,omitempty"`
	Targets     []CampaignTarget `json:"targets"`
	Progress    *JobProgress     `json:"progress,omitempty"`
	Error       string           `json:"error,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
}
