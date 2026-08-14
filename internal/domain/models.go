package domain

import (
	"encoding/json"
	"time"
)

const (
	HostOnline  = "ONLINE"
	HostOffline = "OFFLINE"

	InstanceProvisioning = "PROVISIONING"
	InstanceRunning      = "RUNNING"
	InstanceRestarting   = "RESTARTING"
	InstanceUpdating     = "UPDATING"
	InstanceStopped      = "STOPPED"
	InstanceReconciling  = "RECONCILING"
	InstanceBackingUp    = "BACKING_UP"
	InstanceRestoring    = "RESTORING"
	InstanceDeleting     = "DELETING"
	InstanceDeleted      = "DELETED"
	InstanceFailed       = "FAILED"

	JobPending   = "PENDING"
	JobLeased    = "LEASED"
	JobRunning   = "RUNNING"
	JobSucceeded = "SUCCEEDED"
	JobFailed    = "FAILED"
	JobExpired   = "EXPIRED"

	OperationPending   = "PENDING"
	OperationRunning   = "RUNNING"
	OperationSucceeded = "SUCCEEDED"
	OperationFailed    = "FAILED"

	ObservationInSync   = "IN_SYNC"
	ObservationDegraded = "DEGRADED"
	ObservationMissing  = "MISSING"
	ObservationUnknown  = "UNKNOWN"

	ObservationCheckOK      = "OK"
	ObservationCheckDrift   = "DRIFT"
	ObservationCheckMissing = "MISSING"
	ObservationCheckUnknown = "UNKNOWN"
)

type Host struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Hostname     string    `json:"hostname"`
	OS           string    `json:"os"`
	Arch         string    `json:"arch"`
	AgentVersion string    `json:"agent_version"`
	Status       string    `json:"status"`
	LastSeenAt   time.Time `json:"last_seen_at"`
	CreatedAt    time.Time `json:"created_at"`
}

type Instance struct {
	ID                    string               `json:"id"`
	Name                  string               `json:"name"`
	HostID                string               `json:"host_id"`
	HostName              string               `json:"host_name,omitempty"`
	Status                string               `json:"status"`
	Image                 string               `json:"image"`
	ImageID               string               `json:"image_id,omitempty"`
	Provider              string               `json:"provider"`
	Model                 string               `json:"model"`
	Reasoning             string               `json:"reasoning"`
	ServiceTier           string               `json:"service_tier"`
	CodexConfigured       bool                 `json:"codex_configured"`
	APIPort               int                  `json:"api_port"`
	DashboardPort         int                  `json:"dashboard_port"`
	PublicHostname        string               `json:"public_hostname,omitempty"`
	PublicDashboardURL    string               `json:"public_dashboard_url,omitempty"`
	ProjectName           string               `json:"project_name,omitempty"`
	DataVolume            string               `json:"data_volume,omitempty"`
	ManagedPath           string               `json:"managed_path,omitempty"`
	LastError             string               `json:"last_error,omitempty"`
	HermesVersion         string               `json:"hermes_version,omitempty"`
	HermesSource          string               `json:"hermes_source,omitempty"`
	HermesVersionVerified bool                 `json:"hermes_version_verified"`
	Observation           *InstanceObservation `json:"observation,omitempty"`
	ObservationRequest    *ObservationRequest  `json:"observation_request,omitempty"`
	RuntimeRemediation    *RuntimeRemediation  `json:"runtime_remediation,omitempty"`
	CreatedAt             time.Time            `json:"created_at"`
	UpdatedAt             time.Time            `json:"updated_at"`
}

type RuntimeRemediation struct {
	InstanceID       string     `json:"instance_id"`
	WorkflowID       string     `json:"workflow_id"`
	Status           string     `json:"status"`
	Phase            int        `json:"phase"`
	AttemptInPhase   int        `json:"attempt_in_phase"`
	TotalAttempts    int        `json:"total_attempts"`
	MaxPhases        int        `json:"max_phases"`
	MaxAttempts      int        `json:"max_attempts"`
	ConsecutiveDrift int        `json:"consecutive_drift"`
	LastAttemptAt    *time.Time `json:"last_attempt_at,omitempty"`
	NextAttemptAt    *time.Time `json:"next_attempt_at,omitempty"`
	LastError        string     `json:"last_error,omitempty"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

type ObservationTarget struct {
	InstanceID       string `json:"instance_id"`
	Name             string `json:"name"`
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	CodexConfigured  bool   `json:"codex_configured"`
	DesiredStatus    string `json:"desired_status"`
	Image            string `json:"image"`
	ImageID          string `json:"image_id,omitempty"`
	ProjectName      string `json:"project_name"`
	DataVolume       string `json:"data_volume"`
	ManagedPath      string `json:"managed_path"`
	APIPort          int    `json:"api_port"`
	DashboardPort    int    `json:"dashboard_port"`
	Generation       string `json:"generation"`
	RefreshRequestID string `json:"refresh_request_id,omitempty"`
}

type ObservationCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type InstanceObservation struct {
	InstanceID       string             `json:"instance_id"`
	HostID           string             `json:"-"`
	TargetGeneration string             `json:"target_generation"`
	RefreshRequestID string             `json:"refresh_request_id,omitempty"`
	HermesVersion    string             `json:"hermes_version,omitempty"`
	HermesSource     string             `json:"hermes_source,omitempty"`
	ModelCatalog     []string           `json:"model_catalog,omitempty"`
	RecommendedModel string             `json:"recommended_model,omitempty"`
	Status           string             `json:"status"`
	Summary          string             `json:"summary"`
	Checks           []ObservationCheck `json:"checks"`
	ObservedAt       time.Time          `json:"observed_at"`
	ReceivedAt       time.Time          `json:"received_at,omitempty"`
}

type ObservationRequest struct {
	ID          string    `json:"id"`
	InstanceID  string    `json:"instance_id"`
	RequestedAt time.Time `json:"requested_at"`
}

type Job struct {
	ID             string          `json:"id"`
	OperationID    string          `json:"operation_id"`
	HostID         string          `json:"host_id"`
	InstanceID     string          `json:"instance_id"`
	Type           string          `json:"type"`
	Status         string          `json:"status"`
	Payload        json.RawMessage `json:"payload"`
	Attempts       int             `json:"attempts"`
	LeaseToken     string          `json:"lease_token,omitempty"`
	LeaseExpiresAt *time.Time      `json:"lease_expires_at,omitempty"`
	InputArtifact  string          `json:"-"`
	InputSecret    []byte          `json:"-"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

type JobProgress struct {
	Stage           string          `json:"stage"`
	Detail          string          `json:"detail,omitempty"`
	ActionCode      string          `json:"action_code,omitempty"`
	Steps           []OperationStep `json:"steps,omitempty"`
	VerificationURI string          `json:"verification_uri,omitempty"`
	UserCode        string          `json:"user_code,omitempty"`
	ExpiresAt       time.Time       `json:"expires_at,omitempty"`
}

type OperationStep struct {
	Stage  string `json:"stage"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type RemoteAccessResource struct {
	InstanceID    string    `json:"instance_id"`
	Kind          string    `json:"kind"`
	ResourceID    string    `json:"resource_id,omitempty"`
	Hostname      string    `json:"hostname"`
	TunnelID      string    `json:"tunnel_id"`
	ZoneID        string    `json:"zone_id"`
	OriginService string    `json:"origin_service,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type CodexAuthPayload struct {
	InstanceID  string `json:"instance_id"`
	Name        string `json:"name"`
	ProjectName string `json:"project_name"`
	ManagedPath string `json:"managed_path"`
}

type CodexAuthSession struct {
	OperationID     string    `json:"operation_id"`
	InstanceID      string    `json:"instance_id"`
	Status          string    `json:"status"`
	Stage           string    `json:"stage,omitempty"`
	VerificationURI string    `json:"verification_uri,omitempty"`
	UserCode        string    `json:"user_code,omitempty"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
	Error           string    `json:"error,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type Operation struct {
	ID         string          `json:"id"`
	InstanceID string          `json:"instance_id"`
	WorkflowID string          `json:"workflow_id,omitempty"`
	Actor      string          `json:"actor"`
	Type       string          `json:"type"`
	Status     string          `json:"status"`
	Summary    string          `json:"summary"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	Progress   *JobProgress    `json:"progress,omitempty"`
	Error      string          `json:"error,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

type ProvisionPayload struct {
	InstanceID    string `json:"instance_id"`
	Name          string `json:"name"`
	Image         string `json:"image"`
	HermesVersion string `json:"hermes_version,omitempty"`
	HermesSource  string `json:"hermes_source,omitempty"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Reasoning     string `json:"reasoning"`
	ServiceTier   string `json:"service_tier"`
	APIPort       int    `json:"api_port"`
	DashboardPort int    `json:"dashboard_port"`
}

type ActionPayload struct {
	InstanceID    string `json:"instance_id"`
	Name          string `json:"name"`
	Image         string `json:"image,omitempty"`
	ProjectName   string `json:"project_name"`
	ManagedPath   string `json:"managed_path"`
	ImageID       string `json:"image_id,omitempty"`
	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model,omitempty"`
	Reasoning     string `json:"reasoning,omitempty"`
	ServiceTier   string `json:"service_tier,omitempty"`
	APIPort       int    `json:"api_port"`
	DashboardPort int    `json:"dashboard_port,omitempty"`
	PreserveData  bool   `json:"preserve_data"`
}

type RuntimeRepairPayload struct {
	ActionPayload
	Phase   int    `json:"phase"`
	Attempt int    `json:"attempt"`
	Trigger string `json:"trigger"`
}

type ImageReconcilePayload struct {
	InstanceID      string `json:"instance_id"`
	Name            string `json:"name"`
	Image           string `json:"image"`
	PreviousImageID string `json:"previous_image_id"`
	ProjectName     string `json:"project_name"`
	DataVolume      string `json:"data_volume"`
	ManagedPath     string `json:"managed_path"`
}

type ImageRepairPayload struct {
	InstanceID      string `json:"instance_id"`
	Name            string `json:"name"`
	Image           string `json:"image"`
	PreviousImageID string `json:"previous_image_id"`
	ProjectName     string `json:"project_name"`
	DataVolume      string `json:"data_volume"`
	ManagedPath     string `json:"managed_path"`
	APIPort         int    `json:"api_port"`
	Restart         bool   `json:"restart"`
}

type RuntimeSyncPayload struct {
	InstanceID    string `json:"instance_id"`
	Name          string `json:"name"`
	Image         string `json:"image"`
	ImageID       string `json:"image_id"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Reasoning     string `json:"reasoning"`
	ServiceTier   string `json:"service_tier"`
	ProjectName   string `json:"project_name"`
	DataVolume    string `json:"data_volume"`
	ManagedPath   string `json:"managed_path"`
	DesiredStatus string `json:"desired_status"`
	DashboardPort int    `json:"dashboard_port"`
}

type TelegramMessagingConfiguration struct {
	Enabled           bool     `json:"enabled"`
	BotToken          string   `json:"bot_token,omitempty"`
	AllowedUsers      []string `json:"allowed_users,omitempty"`
	GroupAllowedUsers []string `json:"group_allowed_users,omitempty"`
	GroupAllowedChats []string `json:"group_allowed_chats,omitempty"`
	RequireMention    bool     `json:"require_mention"`
	ProxyURL          string   `json:"proxy_url,omitempty"`
}

type WhatsAppMessagingConfiguration struct {
	Enabled                bool     `json:"enabled"`
	Mode                   string   `json:"mode"`
	AllowedUsers           []string `json:"allowed_users,omitempty"`
	UnauthorizedDMBehavior string   `json:"unauthorized_dm_behavior"`
	ReplyPrefix            string   `json:"reply_prefix,omitempty"`
}

type MessagingConfiguration struct {
	Telegram TelegramMessagingConfiguration `json:"telegram"`
	WhatsApp WhatsAppMessagingConfiguration `json:"whatsapp"`
}

type MessagingApplyPayload struct {
	InstanceID    string `json:"instance_id"`
	Name          string `json:"name"`
	Image         string `json:"image"`
	ImageID       string `json:"image_id"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Reasoning     string `json:"reasoning"`
	ServiceTier   string `json:"service_tier"`
	ProjectName   string `json:"project_name"`
	DataVolume    string `json:"data_volume"`
	ManagedPath   string `json:"managed_path"`
	DesiredStatus string `json:"desired_status"`
	APIPort       int    `json:"api_port"`
	DashboardPort int    `json:"dashboard_port"`
	Revision      string `json:"revision"`
}

type MCPServerConfiguration struct {
	Name        string   `json:"name"`
	Source      string   `json:"source"`
	URL         string   `json:"url"`
	AuthType    string   `json:"auth_type"`
	BearerToken string   `json:"bearer_token,omitempty"`
	Enabled     bool     `json:"enabled"`
	Tools       []string `json:"tools,omitempty"`
}

type MCPConfiguration struct {
	Servers []MCPServerConfiguration `json:"servers"`
}

type MCPApplyPayload struct {
	InstanceID    string `json:"instance_id"`
	Name          string `json:"name"`
	Image         string `json:"image"`
	ImageID       string `json:"image_id"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Reasoning     string `json:"reasoning"`
	ServiceTier   string `json:"service_tier"`
	ProjectName   string `json:"project_name"`
	DataVolume    string `json:"data_volume"`
	ManagedPath   string `json:"managed_path"`
	DesiredStatus string `json:"desired_status"`
	APIPort       int    `json:"api_port"`
	DashboardPort int    `json:"dashboard_port"`
	Revision      string `json:"revision"`
}

type HermesUpgradePayload struct {
	InstanceID      string                 `json:"instance_id"`
	Name            string                 `json:"name"`
	CurrentImage    string                 `json:"current_image"`
	CurrentImageID  string                 `json:"current_image_id"`
	TargetImage     string                 `json:"target_image"`
	TargetVersion   string                 `json:"target_version"`
	TargetSource    string                 `json:"target_source"`
	RecoveryPointID string                 `json:"recovery_point_id"`
	Provider        string                 `json:"provider"`
	Model           string                 `json:"model"`
	Reasoning       string                 `json:"reasoning"`
	ServiceTier     string                 `json:"service_tier"`
	CodexConfigured bool                   `json:"codex_configured"`
	ProjectName     string                 `json:"project_name"`
	DataVolume      string                 `json:"data_volume"`
	ManagedPath     string                 `json:"managed_path"`
	APIPort         int                    `json:"api_port"`
	DashboardPort   int                    `json:"dashboard_port"`
	Rollback        RecoveryRestorePayload `json:"rollback"`
}

type HermesUpdatePayload struct {
	Upgrade        HermesUpgradePayload `json:"upgrade"`
	Backup         RecoveryPointPayload `json:"backup"`
	OriginalStatus string               `json:"original_status"`
}

type RecoveryPointPayload struct {
	RecoveryPointID string    `json:"recovery_point_id"`
	InstanceID      string    `json:"instance_id"`
	Name            string    `json:"name"`
	Image           string    `json:"image"`
	ImageID         string    `json:"image_id"`
	Provider        string    `json:"provider"`
	Model           string    `json:"model"`
	Reasoning       string    `json:"reasoning"`
	ServiceTier     string    `json:"service_tier"`
	CodexConfigured bool      `json:"codex_configured"`
	ProjectName     string    `json:"project_name"`
	DataVolume      string    `json:"data_volume"`
	ManagedPath     string    `json:"managed_path"`
	AgentVersion    string    `json:"agent_version"`
	CreatedAt       time.Time `json:"created_at"`
	MaxBytes        int64     `json:"max_bytes"`
}

type RecoveryRestorePayload struct {
	RecoveryPointID   string    `json:"recovery_point_id"`
	InstanceID        string    `json:"instance_id"`
	Name              string    `json:"name"`
	Image             string    `json:"image"`
	ImageID           string    `json:"image_id"`
	RequireImageID    bool      `json:"require_image_id,omitempty"`
	Provider          string    `json:"provider"`
	Model             string    `json:"model"`
	Reasoning         string    `json:"reasoning"`
	ServiceTier       string    `json:"service_tier"`
	CodexConfigured   bool      `json:"codex_configured"`
	ProjectName       string    `json:"project_name"`
	DataVolume        string    `json:"data_volume"`
	ManagedPath       string    `json:"managed_path"`
	AgentVersion      string    `json:"agent_version"`
	CreatedAt         time.Time `json:"created_at"`
	RecoverySHA256    string    `json:"recovery_sha256"`
	RecoverySizeBytes int64     `json:"recovery_size_bytes"`
	MaxBytes          int64     `json:"max_bytes"`
}

type JobResult struct {
	Success           bool                 `json:"success"`
	Message           string               `json:"message,omitempty"`
	Error             string               `json:"error,omitempty"`
	ProjectName       string               `json:"project_name,omitempty"`
	DataVolume        string               `json:"data_volume,omitempty"`
	ManagedPath       string               `json:"managed_path,omitempty"`
	ImageID           string               `json:"image_id,omitempty"`
	Credentials       *Credentials         `json:"credentials,omitempty"`
	InstanceStatus    string               `json:"instance_status,omitempty"`
	RecoveryPointID   string               `json:"recovery_point_id,omitempty"`
	RecoverySHA256    string               `json:"recovery_sha256,omitempty"`
	RecoverySizeBytes int64                `json:"recovery_size_bytes,omitempty"`
	RecoveryArtifact  string               `json:"-"`
	RecoveryKey       []byte               `json:"-"`
	ChatMessage       string               `json:"chat_message,omitempty"`
	ChatCiphertext    string               `json:"-"`
	ChatArtifacts     []ChatArtifactUpload `json:"-"`
}

type Credentials struct {
	DashboardUsername string `json:"dashboard_username"`
	DashboardPassword string `json:"dashboard_password"`
	APIServerKey      string `json:"api_server_key"`
}
