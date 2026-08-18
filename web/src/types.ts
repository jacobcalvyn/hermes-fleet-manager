export type Host = {
  id: string
  name: string
  hostname: string
  os: string
  arch: string
  agent_version: string
  status: 'ONLINE' | 'OFFLINE'
  last_seen_at: string
  created_at: string
}

export type Instance = {
  id: string
  name: string
  host_id: string
  host_name: string
  status: string
  image: string
  image_id?: string
  provider: string
  model: string
  reasoning: string
  service_tier: string
	codex_configured: boolean
  api_port: number
	dashboard_port: number
	public_hostname?: string
	public_dashboard_url?: string
  project_name?: string
  data_volume?: string
  managed_path?: string
  last_error?: string
  hermes_version?: string
  hermes_source?: string
  hermes_version_verified?: boolean
  observation?: InstanceObservation
  observation_request?: ObservationRequest
  runtime_remediation?: RuntimeRemediation
  created_at: string
  updated_at: string
}

export type RuntimeRemediation = {
  instance_id: string
  workflow_id: string
  status: 'MONITORING' | 'READY' | 'QUEUED' | 'VERIFYING' | 'WAITING' | 'COOLDOWN' | 'EXHAUSTED' | 'CANCELED'
  phase: number
  attempt_in_phase: number
  total_attempts: number
  max_phases: number
  max_attempts: number
  consecutive_drift: number
  last_attempt_at?: string
  next_attempt_at?: string
  last_error?: string
  updated_at: string
}

export type ObservationCheck = {
  name: string
  status: 'OK' | 'DRIFT' | 'MISSING' | 'UNKNOWN'
  detail: string
}

export type ProviderModelCatalog = {
	models: string[]
	recommended?: string
}

export type InstanceObservation = {
  instance_id: string
	target_generation?: string
	hermes_version?: string
	hermes_source?: string
	model_catalog?: string[]
	recommended_model?: string
	provider_model_catalogs?: Record<string, ProviderModelCatalog>
  status: 'IN_SYNC' | 'DEGRADED' | 'MISSING' | 'UNKNOWN'
  summary: string
  checks: ObservationCheck[] | null
  observed_at?: string
  received_at?: string
}

export type ObservationRequest = {
  id: string
  instance_id: string
  requested_at: string
}

export type Operation = {
  id: string
  instance_id: string
  workflow_id?: string
  actor: string
  type: string
  status: 'PENDING' | 'RUNNING' | 'SUCCEEDED' | 'FAILED'
  summary: string
  metadata?: Record<string, unknown>
  progress?: {
    stage: string
	detail?: string
	action_code?: string
	steps?: Array<{ stage: string; status: 'pending' | 'running' | 'succeeded' | 'failed'; detail?: string }>
    verification_uri?: string
    user_code?: string
    expires_at?: string
  }
  error?: string
  created_at: string
  updated_at: string
}

export type OperationPage = {
  items: Operation[] | null
  next_cursor?: string | null
}

export type HermesProfile = {
	name: string
	description?: string
	provider?: string
	model?: string
	active: boolean
	default: boolean
	gateway_running: boolean
}

export type HermesProfileInventory = {
	instance_id: string
	profiles: HermesProfile[]
	observed_at?: string
}

export type ChatSession = {
	id: string
	instance_id: string
	instance_name: string
	title: string
	model: string
	reasoning: string
	service_tier: string
	status: 'ACTIVE'
	last_error?: string
	message_count: number
	last_message_id?: string
	last_message_role?: 'user' | 'assistant'
	last_message_preview?: string
	last_message_at?: string
	response_in_progress: boolean
	last_event_id: number
	created_at: string
	updated_at: string
}

export type ChatMessage = {
	id: string
	session_id: string
	operation_id?: string
	role: 'user' | 'assistant'
	content: string
	status: 'PENDING' | 'SUCCEEDED' | 'FAILED'
	error?: string
	created_at: string
	updated_at: string
}

export type ChatThread = {
	protocol_version: number
	session: ChatSession
	messages: ChatMessage[]
	events?: ChatEvent[]
	active_response?: {
		operation_id: string
		state: 'RUN_QUEUED' | 'RUN_STARTED'
		content?: string
		last_sequence: number
	}
	last_cursor: number
}

export type ChatEvent = {
	version: number
	id: number
	session_id: string
	operation_id: string
	sequence: number
	type: 'RUN_QUEUED' | 'RUN_STARTED' | 'ASSISTANT_DELTA' | 'ASSISTANT_ACTIVITY' | 'ASSISTANT_ARTIFACT' | 'RUN_COMPLETED' | 'RUN_FAILED' | 'RUN_CANCELED'
	content?: string
	payload?: {
		kind: 'activity' | 'artifact'
		event: string
		data?: string
		label?: string
		status?: string
		tool?: string
		call_id?: string
		duration_ms?: number
		artifact?: {
			id?: string
			name: string
			status?: 'preparing' | 'ready' | 'rejected' | 'missing' | 'expired' | 'failed'
			error?: string
			sha256?: string
			kind: 'file' | 'image' | 'audio' | 'video'
			media_type?: string
			size_bytes?: number
			url?: string
			source_tool?: string
			created_at?: string
			expires_at?: string
		}
	}
	created_at: string
}

export type DataArtifactPreview = {
	kind: 'table'
	columns: string[]
	rows: string[][]
	row_numbers?: number[]
	sheets?: string[]
	sheet?: string
	total_rows: number
	total_rows_exact: boolean
	truncated_rows: boolean
	truncated_columns: boolean
	truncated_cells: boolean
}

export type OutputStatus = 'preparing' | 'ready' | 'rejected' | 'missing' | 'expired' | 'failed' | 'deleted'

export type OutputArtifact = {
	id: string
	instance_id: string
	instance_name?: string
	session_id: string
	session_title?: string
	operation_id: string
	name: string
	kind: 'file' | 'image' | 'audio' | 'video'
	media_type?: string
	size_bytes: number
	sha256?: string
	status: OutputStatus
	error?: string
	created_at: string
	expires_at?: string
	deleted_at?: string
	download_url?: string
}

export type OutputArtifactPage = {
	items: OutputArtifact[]
	next_cursor?: string
}

export type OutputUsage = {
	total_bytes: number
	total_max_bytes: number
	session_max_bytes: number
	instance_max_bytes: number
	retention_hours: number
	status_counts: Partial<Record<OutputStatus, number>>
	instances: Record<string, number>
	sessions: Record<string, number>
}

export type PolicyComplianceSummary = {
  total: number
  compliant: number
  drifted: number
  blocked: number
}

export type FleetPolicy = {
  id: string
  name: string
  description?: string
  status: 'ENABLED' | 'DISABLED'
  desired_hermes: 'LATEST_STABLE'
  strategy: 'ONE_AT_A_TIME' | 'ALL_AT_ONCE'
  scope_instance_ids: string[]
  created_at: string
  updated_at: string
  compliance?: PolicyComplianceSummary
	active_rollout?: ControlledPolicyRollout
}

export type PolicyRolloutTarget = {
	rollout_id: string
	policy_id: string
	instance_id: string
	instance_name: string
	child_operation_id?: string
	status: 'PENDING' | 'RUNNING' | 'SUCCEEDED' | 'FAILED' | 'BLOCKED'
	detail?: string
	created_at: string
	updated_at: string
}

export type ControlledPolicyRollout = Operation & {
	control_state: 'RUNNING' | 'PAUSED' | 'CANCELING'
	control_reason?: string
	canary_instance_id: string
	target_version: string
	targets: PolicyRolloutTarget[]
}

export type PolicyTargetPreview = {
  instance_id: string
  instance_name: string
  current_version?: string
  target_version?: string
  state: 'COMPLIANT' | 'DRIFTED' | 'BLOCKED'
  detail: string
}

export type PolicyPreview = {
  policy: FleetPolicy
  summary: PolicyComplianceSummary
  targets: PolicyTargetPreview[]
}

export type Overview = {
  hosts: Host[] | null
  instances: Instance[] | null
  operations: Operation[] | null
	stream_id?: string
	state_revision?: number
}

export type FleetStateEvent = {
	stream_id: string
	revision: number
	type: string
	resource_id?: string
	occurred_at: string
}

export type Credentials = {
  dashboard_username: string
  dashboard_password: string
  api_server_key: string
}

export type CredentialReveal = {
  credentials: Credentials
  expires_at: string
}

export type MessagingConfiguration = {
  status: 'NOT_CONFIGURED' | 'PENDING' | 'APPLIED' | 'FAILED'
  last_error?: string
  desired_revision?: string
  applied_revision?: string
  updated_at?: string
  applied_at?: string
  telegram: {
    enabled: boolean
    token_configured: boolean
    token_hint: string
    allowed_users: string[]
    group_allowed_users: string[]
    group_allowed_chats: string[]
    require_mention: boolean
    proxy_url: string
  }
  whatsapp: {
    enabled: boolean
    mode: 'bot' | 'self-chat'
    allowed_users: string[]
    unauthorized_dm_behavior: 'ignore' | 'pair'
    reply_prefix: string
  }
}

export type MCPServerConfiguration = {
  name: string
  source: 'remote'
  url: string
  auth_type: 'none' | 'bearer'
  token_configured: boolean
  token_hint: string
  enabled: boolean
  tools: string[]
}

export type MCPConfiguration = {
  status: 'NOT_CONFIGURED' | 'PENDING' | 'APPLIED' | 'FAILED'
  last_error?: string
  desired_revision?: string
  applied_revision?: string
  updated_at?: string
  applied_at?: string
  servers: MCPServerConfiguration[]
}

export type MCPDiscoveredTool = {
  name: string
  description?: string
}

export type MCPDiscoveryResult = {
  tools: MCPDiscoveredTool[]
}

export type CodexAuthSession = {
  operation_id: string
  instance_id: string
  provider: string
  status: 'PENDING' | 'RUNNING' | 'SUCCEEDED' | 'FAILED'
  stage?: 'STARTING' | 'AWAITING_USER' | 'VERIFYING' | 'COMPLETED'
  verification_uri?: string
  user_code?: string
  expires_at?: string
  error?: string
  created_at: string
  updated_at: string
}

export type Backup = {
  id: string
  filename: string
  size_bytes: number
  sha256: string
  created_at: string
  verified_at: string
}

export type RecoveryPoint = {
  id: string
  operation_id?: string
  instance_id: string
  instance_name: string
  filename: string
  status: 'CREATING' | 'UPLOADED' | 'READY' | 'FAILED'
  size_bytes: number
  encrypted_size_bytes: number
  sha256?: string
  image: string
  image_id: string
	provider: string
	model: string
	reasoning: string
	service_tier: string
	codex_configured: boolean
  project_name: string
  data_volume: string
  agent_version: string
  error?: string
  created_at: string
  uploaded_at?: string
  verified_at?: string
}

export type HermesUpdate = {
  current_version?: string
  current_source?: string
  current_image: string
  official_status: 'CURRENT' | 'UPDATE_AVAILABLE' | 'UNKNOWN' | 'CHECK_FAILED'
  update_kind: 'NONE' | 'VERSION_UPDATE' | 'RUNTIME_REFRESH'
  official_source?: string
  official_checked_at?: string
  official_stale?: boolean
  latest_release?: HermesRelease
  target_version: string
  target_source: string
  target_image: string
  available: boolean
  eligible: boolean
  reason: string
}

export type HermesRelease = {
  version: string
  tag: string
  commit: string
  image: string
  url: string
  published_at: string
}

export type HermesReleaseCatalog = {
  source: string
  checked_at: string
  releases: HermesRelease[]
  stale?: boolean
}

export type CapacityStatus = {
	free_bytes: number
	total_bytes: number
	free_percent: number
	free_inodes: number
	minimum_free_bytes: number
	minimum_free_percent: number
	minimum_free_inodes: number
	operations_safe: boolean
	blocking_reason?: string
}

export type ReadinessStatus = {
	ready: boolean
	database: string
	storage: string
	release_catalog: string
	capacity: CapacityStatus
	last_checked: string
}

export type RecoveryDrillStatus = {
	status: 'NEVER_RUN' | 'RUNNING' | 'PASSED' | 'INCOMPLETE' | 'FAILED'
	started_at?: string
	completed_at?: string
	control_plane_backup_checked: boolean
	instance_backups_checked: number
	instances_without_backup: number
	error?: string
}

export type SystemInfo = {
  fleet_version: string
  build_id: string
  operator_url: string
	database_path: string
	backup_retention: number
	readiness: ReadinessStatus
	capacity: CapacityStatus
	recovery_drill: RecoveryDrillStatus
	remote_access: RemoteAccessStatus
	capabilities: CompatibilityManifest
}

export type CompatibilityManifest = {
	control_plane_version: string
	host_agent_version: string
	runtime_config_schemas: number[]
	default_job_concurrency: number
	maximum_job_concurrency: number
	features: string[]
}

export type RuntimeHealth = {
	status: 'healthy' | 'degraded'
	stream_id: string
	state_revision: number
	event_subscribers: number
	compatibility: CompatibilityManifest
	queue: {
		pending: number
		active: number
		expired_leases: number
		admission_rejected: boolean
		max_per_host: number
		hosts: Array<{
			host_id: string
			host_name: string
			pending: number
			active: number
			expired_leases: number
			oldest_pending_at?: string
			admission_open: boolean
		}>
	}
	metrics: {
		started_at: string
		uptime_seconds: number
		http_requests: number
		http_failures: number
		http_in_flight: number
		duration_samples: number
		average_http_ms: number
		p95_http_ms: number
		p99_http_ms: number
		max_http_ms: number
		mutations: number
		queue_rejected: number
		jobs_reconciled: number
	}
	components: Array<{
		component: string
		status: 'healthy' | 'degraded'
		detail: string
		updated_at: string
		last_success_at?: string
	}>
	recent_incidents: Array<{
		id: number
		component: string
		previous_status?: string
		status: 'healthy' | 'degraded'
		detail: string
		occurred_at: string
	}>
}

export type RemoteAccessBoundary = {
	tunnel_id?: string
	hostname?: string
	url?: string
	routes: number
	synced: boolean
	connector_state?: 'disabled' | 'starting' | 'running' | 'retrying' | 'unreachable' | 'stopping'
	connector_checked_at?: string
	connector_error?: string
	endpoint_state?: 'unchecked' | 'checking' | 'propagating' | 'reachable' | 'access_protected' | 'unavailable'
	endpoint_detail?: string
	endpoint_checked_at?: string
}

export type RemoteAccessMode = 'managed_cloudflare' | 'existing_endpoints'

export type RemoteAccessStatus = {
	configured: boolean
	mode?: RemoteAccessMode
	state: 'disabled' | 'pending' | 'syncing' | 'synced' | 'registered' | 'degraded' | 'error' | 'cleanup_pending'
	admin: RemoteAccessBoundary
	instances: RemoteAccessBoundary
	last_sync_at?: string
	last_error?: string
}

export type RemoteAccessInstanceEndpoint = {
	instance_id: string
	instance_name: string
	dashboard_url: string
}

export type RemoteAccessPublishedRoute = {
	instance_id: string
	instance_name: string
	hostname?: string
	origin_service: string
	provider_state?: 'ready_to_publish' | 'publishing' | 'published' | 'configuration_mismatch' | 'failed'
	provider_detail?: string
	provider_checked_at?: string
	dns_state?: 'pending' | 'ready' | 'conflict' | 'failed'
	dns_detail?: string
	dns_checked_at?: string
	route_state?: 'pending' | 'ready' | 'conflict' | 'failed'
	route_detail?: string
	route_checked_at?: string
	endpoint_state?: 'unchecked' | 'checking' | 'propagating' | 'reachable' | 'access_protected' | 'unavailable'
	endpoint_detail?: string
	endpoint_checked_at?: string
	published: boolean
	revalidating?: boolean
}

export type RemoteAccessConfiguration = {
	mode?: RemoteAccessMode
	admin_tunnel_id?: string
	instances_tunnel_id?: string
	admin_hostname: string
	admin_credential_available?: boolean
	instances_credential_available?: boolean
	admin_tunnel_token_configured: boolean
	instances_tunnel_token_configured: boolean
	admin_tunnel_token_fingerprint?: string
	instances_tunnel_token_fingerprint?: string
	instance_publishing_configured?: boolean
	instance_publishing_account_id?: string
	instance_publishing_zone_id?: string
	instance_publishing_zone?: string
	instance_publishing_fleet_namespace?: string
	instance_publishing_tunnel_id?: string
	instance_publishing_token_fingerprint?: string
	legacy_provider_managed: boolean
	admin_url: string
	instance_endpoints: RemoteAccessInstanceEndpoint[]
	admin_origin_service: string
	instance_routes: RemoteAccessPublishedRoute[]
}
