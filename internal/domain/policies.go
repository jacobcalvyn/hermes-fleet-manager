package domain

import "time"

const (
	PolicyEnabled  = "ENABLED"
	PolicyDisabled = "DISABLED"

	PolicyDesiredHermesLatestStable = "LATEST_STABLE"

	PolicyStrategyOneAtATime = "ONE_AT_A_TIME"
	PolicyStrategyAllAtOnce  = "ALL_AT_ONCE"

	PolicyTargetPending   = "PENDING"
	PolicyTargetRunning   = "RUNNING"
	PolicyTargetSucceeded = "SUCCEEDED"
	PolicyTargetFailed    = "FAILED"
	PolicyTargetBlocked   = "BLOCKED"

	PolicyRolloutControlRunning   = "RUNNING"
	PolicyRolloutControlPaused    = "PAUSED"
	PolicyRolloutControlCanceling = "CANCELING"
)

// FleetPolicy stores a global desired-state baseline. Scope is explicit so
// instance-owned hostnames, OAuth sessions, and secret values never leak into
// a shared policy.
type FleetPolicy struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Description      string    `json:"description,omitempty"`
	Status           string    `json:"status"`
	DesiredHermes    string    `json:"desired_hermes"`
	Strategy         string    `json:"strategy"`
	ScopeInstanceIDs []string  `json:"scope_instance_ids"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type PolicyComplianceSummary struct {
	Total     int `json:"total"`
	Compliant int `json:"compliant"`
	Drifted   int `json:"drifted"`
	Blocked   int `json:"blocked"`
}

type PolicyTargetPreview struct {
	InstanceID     string `json:"instance_id"`
	InstanceName   string `json:"instance_name"`
	CurrentVersion string `json:"current_version,omitempty"`
	TargetVersion  string `json:"target_version,omitempty"`
	State          string `json:"state"`
	Detail         string `json:"detail"`
}

type PolicyPreview struct {
	Policy  FleetPolicy             `json:"policy"`
	Summary PolicyComplianceSummary `json:"summary"`
	Targets []PolicyTargetPreview   `json:"targets"`
}

type PolicyRolloutTarget struct {
	RolloutID        string    `json:"rollout_id"`
	PolicyID         string    `json:"policy_id"`
	InstanceID       string    `json:"instance_id"`
	InstanceName     string    `json:"instance_name"`
	ChildOperationID string    `json:"child_operation_id,omitempty"`
	Status           string    `json:"status"`
	Detail           string    `json:"detail,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}
