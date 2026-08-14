package compatibility

const (
	HostAgentVersion      = "0.12.1"
	RuntimeSchemaLegacy   = 1
	RuntimeSchemaCurrent  = 2
	DefaultJobConcurrency = 4
	MaximumJobConcurrency = 8
)

type Manifest struct {
	ControlPlaneVersion   string   `json:"control_plane_version"`
	HostAgentVersion      string   `json:"host_agent_version"`
	RuntimeConfigSchemas  []int    `json:"runtime_config_schemas"`
	DefaultJobConcurrency int      `json:"default_job_concurrency"`
	MaximumJobConcurrency int      `json:"maximum_job_concurrency"`
	Features              []string `json:"features"`
}

func Current(controlPlaneVersion string) Manifest {
	return Manifest{
		ControlPlaneVersion:   controlPlaneVersion,
		HostAgentVersion:      HostAgentVersion,
		RuntimeConfigSchemas:  []int{RuntimeSchemaLegacy, RuntimeSchemaCurrent},
		DefaultJobConcurrency: DefaultJobConcurrency,
		MaximumJobConcurrency: MaximumJobConcurrency,
		Features: []string{
			"authoritative-state-stream",
			"bounded-job-queue",
			"chat-artifact-transfer",
			"instance-mcp-configuration",
			"runtime-auto-remediation",
			"runtime-schema-preflight",
		},
	}
}

func SupportsRuntimeSchema(version int) bool {
	return version == RuntimeSchemaLegacy || version == RuntimeSchemaCurrent
}

func HostAgentCompatible(version string) bool {
	return version == HostAgentVersion
}
