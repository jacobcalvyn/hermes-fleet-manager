package domain

import "time"

const (
	ChatProtocolVersion = 5

	ChatSessionActive = "ACTIVE"

	ChatMessagePending   = "PENDING"
	ChatMessageSucceeded = "SUCCEEDED"
	ChatMessageFailed    = "FAILED"

	ChatEventQueued    = "RUN_QUEUED"
	ChatEventStarted   = "RUN_STARTED"
	ChatEventDelta     = "ASSISTANT_DELTA"
	ChatEventActivity  = "ASSISTANT_ACTIVITY"
	ChatEventArtifact  = "ASSISTANT_ARTIFACT"
	ChatEventCompleted = "RUN_COMPLETED"
	ChatEventFailed    = "RUN_FAILED"
	ChatEventCanceled  = "RUN_CANCELED"
)

type ChatSession struct {
	ID                    string     `json:"id"`
	InstanceID            string     `json:"instance_id"`
	InstanceName          string     `json:"instance_name"`
	Title                 string     `json:"title"`
	Model                 string     `json:"model"`
	Reasoning             string     `json:"reasoning"`
	ServiceTier           string     `json:"service_tier"`
	Status                string     `json:"status"`
	LastError             string     `json:"last_error,omitempty"`
	MessageCount          int        `json:"message_count"`
	LastMessageID         string     `json:"last_message_id,omitempty"`
	LastMessageRole       string     `json:"last_message_role,omitempty"`
	LastMessagePreview    string     `json:"last_message_preview,omitempty"`
	LastMessageAt         *time.Time `json:"last_message_at,omitempty"`
	ResponseInProgress    bool       `json:"response_in_progress"`
	LastEventID           int64      `json:"last_event_id"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
	LastMessageCiphertext string     `json:"-"`
}

type ChatMessage struct {
	ID          string    `json:"id"`
	SessionID   string    `json:"session_id"`
	OperationID string    `json:"operation_id,omitempty"`
	Role        string    `json:"role"`
	Content     string    `json:"content"`
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Ciphertext  string    `json:"-"`
}

type ChatThread struct {
	ProtocolVersion int                 `json:"protocol_version"`
	Session         ChatSession         `json:"session"`
	Messages        []ChatMessage       `json:"messages"`
	Events          []ChatEvent         `json:"events,omitempty"`
	ActiveResponse  *ChatActiveResponse `json:"active_response,omitempty"`
	LastCursor      int64               `json:"last_cursor"`
}

// ChatActiveResponse is the materialized in-flight assistant response included
// in the atomic thread snapshot. Clients continue from LastCursor instead of
// replaying the session event log from zero.
type ChatActiveResponse struct {
	OperationID  string `json:"operation_id"`
	State        string `json:"state"`
	Content      string `json:"content,omitempty"`
	LastSequence int64  `json:"last_sequence"`
}

// ChatEvent is the durable, ordered transport used to replay an in-flight
// assistant response after a browser or Host Agent reconnect.
type ChatEvent struct {
	Version     int               `json:"version"`
	ID          int64             `json:"id"`
	SessionID   string            `json:"session_id"`
	OperationID string            `json:"operation_id"`
	Sequence    int64             `json:"sequence"`
	Type        string            `json:"type"`
	Content     string            `json:"content,omitempty"`
	Payload     *ChatEventPayload `json:"payload,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	Ciphertext  string            `json:"-"`
	ContentHash string            `json:"-"`
}

// ChatEventPayload preserves the upstream Hermes event envelope. Data is the
// exact SSE data field received from Hermes; Fleet must not reconstruct tool
// names, arguments, hierarchy, status, or duration from unrelated fields.
// Legacy normalized fields remain readable for events stored by older Fleet
// releases, but new activity events use Event and Data as their source of truth.
type ChatEventPayload struct {
	Kind       string        `json:"kind"`
	Event      string        `json:"event"`
	Data       string        `json:"data,omitempty"`
	Label      string        `json:"label,omitempty"`
	Status     string        `json:"status,omitempty"`
	Tool       string        `json:"tool,omitempty"`
	CallID     string        `json:"call_id,omitempty"`
	DurationMS int64         `json:"duration_ms,omitempty"`
	Artifact   *ChatArtifact `json:"artifact,omitempty"`
}

type ChatArtifact struct {
	ID         string     `json:"id,omitempty"`
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	MediaType  string     `json:"media_type,omitempty"`
	SizeBytes  int64      `json:"size_bytes,omitempty"`
	SHA256     string     `json:"sha256,omitempty"`
	Status     string     `json:"status,omitempty"`
	Error      string     `json:"error,omitempty"`
	URL        string     `json:"url,omitempty"`
	SourceTool string     `json:"source_tool,omitempty"`
	CreatedAt  *time.Time `json:"created_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// ChatArtifactUpload is local Host Agent state. LocalPath and SourcePath never
// cross the control-plane API boundary; only the normalized artifact metadata
// is persisted after an authenticated, lease-fenced upload.
type ChatArtifactUpload struct {
	Sequence   int64        `json:"-"`
	Artifact   ChatArtifact `json:"-"`
	LocalPath  string       `json:"-"`
	SourcePath string       `json:"-"`
	Error      string       `json:"-"`
}

// ChatStreamEvent is emitted by the Host Agent while it holds the job lease.
// Sequence is scoped to one operation and starts at one; zero is reserved for
// the control-plane RUN_QUEUED event.
type ChatStreamEvent struct {
	Sequence int64  `json:"sequence"`
	Type     string `json:"type"`
	Content  string `json:"content,omitempty"`
}

type ChatSendPayload struct {
	InstanceID   string `json:"instance_id"`
	InstanceName string `json:"instance_name"`
	SessionID    string `json:"session_id"`
	MessageID    string `json:"message_id"`
	ProjectName  string `json:"project_name"`
	ManagedPath  string `json:"managed_path"`
	APIPort      int    `json:"api_port"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Reasoning    string `json:"reasoning"`
	ServiceTier  string `json:"service_tier"`
}
