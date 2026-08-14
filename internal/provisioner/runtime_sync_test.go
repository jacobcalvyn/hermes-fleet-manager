package provisioner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func TestRuntimeSyncRecreatesServicesAndVerifiesCompleteConfiguration(t *testing.T) {
	p, payload, commands := newRuntimeSyncTestProvisioner(t)

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	result := p.Execute(context.Background(), domain.Job{Type: "instance.runtime.sync", Payload: encoded})
	if !result.Success || result.InstanceStatus != domain.InstanceRunning {
		t.Fatalf("runtime sync result=%+v", result)
	}

	applyCalls, recreateCalls, restartCalls := 0, 0, 0
	for _, command := range *commands {
		if applyIndex := slices.Index(command, runtimeStateApply); applyIndex >= 0 {
			applyCalls++
			got := command[applyIndex+1:]
			want := []string{
				payload.Provider, payload.Model, payload.Reasoning, payload.ServiceTier,
				strconv.Itoa(runtimeStateSchemaVersion), testRuntimeBuildID,
			}
			if !slices.Equal(got, want) {
				t.Fatalf("runtime apply metadata=%v, want %v", got, want)
			}
			if slices.Contains(got, payload.ImageID) {
				t.Fatalf("runtime apply used Docker image identity as wrapper build identity: %v", got)
			}
		}
		if slices.Contains(command, "restart") {
			restartCalls++
		}
		if slices.Contains(command, "--force-recreate") {
			recreateCalls++
			if !slices.Equal(command[len(command)-2:], []string{"hermes", "dashboard"}) {
				t.Fatalf("runtime sync recreated the wrong services: %v", command)
			}
		}
	}
	if applyCalls != 1 || recreateCalls != 1 || restartCalls != 0 {
		t.Fatalf("unexpected synchronization commands: %+v", *commands)
	}
}

func TestLegacyRuntimeMigrationKeepsSchemaV1AndRecreatesBothServices(t *testing.T) {
	root := t.TempDir()
	p, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	imageID := "sha256:" + strings.Repeat("a", 64)
	capability := runtimeImageCapability{SchemaVersion: 1, BuildID: testRuntimeBuildID}
	provider, model, reasoning, serviceTier := "openai-codex", "gpt-5.6-sol", "medium", "normal"
	var commands [][]string
	probeCalls := 0
	p.dockerRun = func(_ context.Context, args ...string) (string, error) {
		commands = append(commands, append([]string(nil), args...))
		switch {
		case args[0] == "image" && slices.Contains(args, "{{json .Config.Labels}}"):
			return runtimeImageLabelsJSON(capability.SchemaVersion, capability.BuildID), nil
		case args[0] == "compose" && slices.Contains(args, runtimeStateProbe):
			probeCalls++
			if probeCalls == 1 {
				return readyRuntimeStateJSON(provider, model, reasoning, serviceTier), nil
			}
			return readyRuntimeStateJSONForCapability(provider, model, reasoning, serviceTier, capability), nil
		case args[0] == "compose" && slices.Contains(args, runtimeStateApply):
			applyIndex := slices.Index(args, runtimeStateApply)
			got := args[applyIndex+1:]
			want := []string{provider, model, reasoning, serviceTier, "1", capability.BuildID}
			if !slices.Equal(got, want) {
				t.Fatalf("legacy migration metadata=%v, want %v", got, want)
			}
			return "", nil
		case args[0] == "compose" && slices.Contains(args, "--force-recreate"):
			if !slices.Equal(args[len(args)-2:], []string{"hermes", "dashboard"}) {
				t.Fatalf("legacy migration recreated the wrong services: %v", args)
			}
			return "services recreated", nil
		default:
			return "", fmt.Errorf("unexpected Docker command: %v", args)
		}
	}
	p.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Request:    request,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("ok")),
		}, nil
	})}

	if err := p.ensureManagedRuntimeReady(
		context.Background(), filepath.Join(root, "fleet-test-01-00000000"),
		"hermes-fleet-fleet-test-01-00000000", provider, model, reasoning, serviceTier,
		imageID, 19130,
	); err != nil {
		t.Fatal(err)
	}
	if probeCalls != 2 {
		t.Fatalf("legacy migration readiness probes=%d, want 2", probeCalls)
	}
	for _, command := range commands {
		if slices.Contains(command, "restart") {
			t.Fatalf("legacy migration used a dashboard-only restart: %v", command)
		}
	}
}

func TestVerifyRuntimeStateRejectsMarkerOnlyAgentSettings(t *testing.T) {
	capability := runtimeImageCapability{
		SchemaVersion: runtimeStateSchemaVersion,
		BuildID:       testRuntimeBuildID,
	}
	var observation map[string]any
	if err := json.Unmarshal(
		[]byte(readyRuntimeStateJSONForCapability(
			"openai-codex", "gpt-5.6-sol", "medium", "normal", capability,
		)),
		&observation,
	); err != nil {
		t.Fatal(err)
	}
	observation["agent"].(map[string]any)["service_tier"] = "priority"
	encoded, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyRuntimeState(
		string(encoded), "openai-codex", "gpt-5.6-sol", "medium", "normal", &capability,
	); err == nil || !strings.Contains(err.Error(), "did not activate") {
		t.Fatalf("verifyRuntimeState() error=%v, want effective agent configuration rejection", err)
	}
}

func TestRuntimeStateApplyWritesMarkerSchemaOwnedByWrapper(t *testing.T) {
	for _, test := range []struct {
		name       string
		schema     int
		revision   string
		hasDetails bool
	}{
		{
			name:     "legacy-v1",
			schema:   1,
			revision: legacyRuntimeConfigurationRevision("openai-codex", "gpt-5.6-sol"),
		},
		{
			name:       "current-v2",
			schema:     runtimeStateSchemaVersion,
			revision:   runtimeConfigurationRevision("openai-codex", "gpt-5.6-sol", "medium", "normal"),
			hasDetails: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			moduleRoot := t.TempDir()
			packagePath := filepath.Join(moduleRoot, "hermes_cli")
			if err := os.MkdirAll(packagePath, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(packagePath, "__init__.py"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			stub := `import json, os
from pathlib import Path
def _path():
    return Path(os.environ["HERMES_HOME"]) / "config.json"
def read_raw_config():
    try:
        return json.loads(_path().read_text(encoding="utf-8"))
    except FileNotFoundError:
        return {}
def load_config():
    return read_raw_config()
def save_config(config, **_kwargs):
    _path().write_text(json.dumps(config), encoding="utf-8")
`
			if err := os.WriteFile(filepath.Join(packagePath, "config.py"), []byte(stub), 0o600); err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(moduleRoot, "data")
			command := exec.Command(
				"python3", "-c", runtimeStateApply,
				"openai-codex", "gpt-5.6-sol", "medium", "normal",
				strconv.Itoa(test.schema), testRuntimeBuildID,
			)
			command.Env = append(
				os.Environ(),
				"PYTHONPATH="+moduleRoot,
				"HERMES_HOME="+home,
			)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("runtimeStateApply failed: %v\n%s", err, output)
			}
			var config map[string]map[string]any
			configBytes, err := os.ReadFile(filepath.Join(home, "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(configBytes, &config); err != nil {
				t.Fatal(err)
			}
			if config["model"]["default"] != "gpt-5.6-sol" ||
				config["model"]["provider"] != "openai-codex" ||
				config["agent"]["reasoning_effort"] != "medium" ||
				config["agent"]["service_tier"] != "normal" {
				t.Fatalf("effective runtime config=%v", config)
			}
			var state map[string]any
			stateBytes, err := os.ReadFile(filepath.Join(home, ".fleet-runtime-ready.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(stateBytes, &state); err != nil {
				t.Fatal(err)
			}
			if int(state["schema_version"].(float64)) != test.schema ||
				state["configuration_revision"] != test.revision ||
				state["runtime_build_id"] != testRuntimeBuildID {
				t.Fatalf("runtime readiness marker=%v", state)
			}
			_, hasReasoning := state["reasoning"]
			_, hasServiceTier := state["service_tier"]
			if hasReasoning != test.hasDetails || hasServiceTier != test.hasDetails {
				t.Fatalf("runtime marker detail fields=%v, want present=%t", state, test.hasDetails)
			}
		})
	}
}

func TestRuntimeSyncStopsRollbackWhenLeaseIsCanceled(t *testing.T) {
	p, payload, commands := newRuntimeSyncTestProvisioner(t)
	ctx, cancel := context.WithCancel(context.Background())
	originalDockerRun := p.dockerRun
	p.dockerRun = func(callContext context.Context, args ...string) (string, error) {
		if slices.Contains(args, runtimeStateApply) {
			cancel()
			return "", errors.New("lease canceled during apply")
		}
		return originalDockerRun(callContext, args...)
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	result := p.Execute(ctx, domain.Job{Type: "instance.runtime.sync", Payload: encoded})
	if result.Success || !strings.Contains(result.Error, context.Canceled.Error()) {
		t.Fatalf("runtime sync result=%+v, want canceled lease failure", result)
	}
	for _, command := range *commands {
		if slices.Contains(command, runtimeConfigRestoreScript) || slices.Contains(command, "--force-recreate") {
			t.Fatalf("rollback mutated runtime after lease cancellation: %v", command)
		}
	}
}

func TestRuntimeSyncRestoresEnvironmentConfigAndMarkerAfterVerificationFailure(t *testing.T) {
	p, payload, commands := newRuntimeSyncTestProvisioner(t)
	envPath := filepath.Join(payload.ManagedPath, ".env")
	originalEnv, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	originalDockerRun := p.dockerRun
	probeCalls, restoreCalls := 0, 0
	p.dockerRun = func(callContext context.Context, args ...string) (string, error) {
		switch {
		case slices.Contains(args, runtimeConfigRestoreScript):
			*commands = append(*commands, append([]string(nil), args...))
			restoreCalls++
			return "", nil
		case slices.Contains(args, runtimeStateProbe) && probeCalls == 0:
			*commands = append(*commands, append([]string(nil), args...))
			probeCalls++
			return readyRuntimeStateJSON(payload.Provider, payload.Model, "low", payload.ServiceTier), nil
		default:
			return originalDockerRun(callContext, args...)
		}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	result := p.Execute(context.Background(), domain.Job{Type: "instance.runtime.sync", Payload: encoded})
	if result.Success || !strings.Contains(result.Error, "previous runtime configuration was restored") {
		t.Fatalf("runtime sync result=%+v, want verified rollback", result)
	}
	restoredEnv, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restoredEnv) != string(originalEnv) {
		t.Fatalf("runtime rollback did not restore .env\nactual:\n%s\nwant:\n%s", restoredEnv, originalEnv)
	}
	if restoreCalls != 1 {
		t.Fatalf("runtime rollback restore calls=%d, want 1", restoreCalls)
	}
	for _, command := range *commands {
		if slices.Contains(command, runtimeConfigRestoreScript) && !slices.Contains(command, "-i") {
			t.Fatalf("runtime rollback did not attach snapshot stdin: %v", command)
		}
	}
	recreateCalls := 0
	for _, command := range *commands {
		if slices.Contains(command, "--force-recreate") {
			recreateCalls++
		}
	}
	if recreateCalls != 2 {
		t.Fatalf("runtime apply and rollback recreate calls=%d, want 2: %v", recreateCalls, *commands)
	}
}

func TestRuntimeSyncMarksInstanceFailedWhenRollbackCannotBeCompleted(t *testing.T) {
	p, payload, commands := newRuntimeSyncTestProvisioner(t)
	originalDockerRun := p.dockerRun
	p.dockerRun = func(callContext context.Context, args ...string) (string, error) {
		switch {
		case slices.Contains(args, runtimeStateProbe):
			*commands = append(*commands, append([]string(nil), args...))
			return readyRuntimeStateJSON(payload.Provider, payload.Model, "low", payload.ServiceTier), nil
		case slices.Contains(args, runtimeConfigRestoreScript):
			*commands = append(*commands, append([]string(nil), args...))
			return "restore rejected", errors.New("restore failed")
		default:
			return originalDockerRun(callContext, args...)
		}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	result := p.Execute(context.Background(), domain.Job{Type: "instance.runtime.sync", Payload: encoded})
	if result.Success || result.InstanceStatus != domain.InstanceFailed ||
		!strings.Contains(result.Error, "manual recovery is required") {
		t.Fatalf("runtime sync result=%+v, want terminal recovery failure", result)
	}
	for _, command := range *commands {
		if slices.Contains(command, runtimeConfigRestoreScript) && !slices.Contains(command, "-i") {
			t.Fatalf("failed rollback did not attach snapshot stdin: %v", command)
		}
	}
}

func TestRuntimeVolumeInputCommandAttachesStdinWithoutChangingIsolation(t *testing.T) {
	p, payload, _ := newRuntimeSyncTestProvisioner(t)
	command := p.runtimeVolumeInputCommand(payload, "python", "-c", runtimeConfigRestoreScript)
	if len(command) < 4 || command[0] != "run" || !slices.Contains(command, "--rm") ||
		!slices.Contains(command, "-i") || !slices.Contains(command, "--network") ||
		!slices.Contains(command, "none") || !slices.Contains(command, "no-new-privileges") {
		t.Fatalf("runtime input command lost Docker isolation or stdin attachment: %v", command)
	}
}

func TestDashboardReadyStatusRejectsErrorAndThrottleResponses(t *testing.T) {
	for _, test := range []struct {
		status int
		ready  bool
	}{
		{status: http.StatusOK, ready: true},
		{status: http.StatusNoContent, ready: true},
		{status: http.StatusUnauthorized, ready: true},
		{status: http.StatusNotFound, ready: false},
		{status: http.StatusTooManyRequests, ready: false},
		{status: http.StatusInternalServerError, ready: false},
	} {
		if actual := dashboardReadyStatus(test.status); actual != test.ready {
			t.Fatalf("dashboardReadyStatus(%d)=%t, want %t", test.status, actual, test.ready)
		}
	}
}

func TestProvisionDashboardReadinessAllowsSlowColdStarts(t *testing.T) {
	if provisionDashboardReadyTimeout < 2*time.Minute {
		t.Fatalf("provision dashboard readiness timeout=%s, want at least 2m", provisionDashboardReadyTimeout)
	}
	if provisionDashboardReadyTimeout > 3*time.Minute {
		t.Fatalf("provision dashboard readiness timeout=%s, want a bounded timeout", provisionDashboardReadyTimeout)
	}
}

func TestWaitForDashboardRetriesTransientColdStart(t *testing.T) {
	p, err := New(t.TempDir(), "docker")
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	p.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts++
		status := http.StatusServiceUnavailable
		if attempts == 3 {
			status = http.StatusOK
		}
		return &http.Response{
			StatusCode: status,
			Request:    request,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("dashboard")),
		}, nil
	})}

	if err := p.waitForDashboardAtInterval(context.Background(), 19130, 100*time.Millisecond, time.Millisecond); err != nil {
		t.Fatalf("waitForDashboardAtInterval() error=%v", err)
	}
	if attempts != 3 {
		t.Fatalf("dashboard readiness attempts=%d, want 3", attempts)
	}
}

func newRuntimeSyncTestProvisioner(
	t *testing.T,
) (*Provisioner, domain.RuntimeSyncPayload, *[][]string) {
	t.Helper()
	root := t.TempDir()
	managedPath := filepath.Join(root, "fleet-test-01-00000000")
	if err := os.MkdirAll(managedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedPath, "compose.yaml"), []byte("managed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(managedPath, ".env"),
		[]byte("HERMES_INFERENCE_PROVIDER=old\nHERMES_INFERENCE_MODEL=old\nHERMES_REASONING_EFFORT=low\nHERMES_SERVICE_TIER=normal\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	p, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	imageID := "sha256:" + strings.Repeat("a", 64)
	payload := domain.RuntimeSyncPayload{
		InstanceID: "00000000-0000-4000-8000-000000000001", Name: "fleet-test-01",
		Image: "runtime:latest", ImageID: imageID, Provider: "openai-codex", Model: "gpt-5.6-sol",
		Reasoning: "medium", ServiceTier: "normal", ProjectName: "hermes-fleet-fleet-test-01-00000000",
		DataVolume: "hermes-fleet-fleet-test-01-00000000-data", ManagedPath: managedPath,
		DesiredStatus: domain.InstanceRunning, DashboardPort: 19130,
	}
	commands := &[][]string{}
	snapshot := runtimeConfigSnapshot{
		Config: runtimeConfigFileSnapshot{
			Exists: true,
			Data:   base64.StdEncoding.EncodeToString([]byte("model:\n  default: old\n")),
		},
		Marker: runtimeConfigFileSnapshot{
			Exists: true,
			Data:   base64.StdEncoding.EncodeToString([]byte(`{"schema_version":1}`)),
		},
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	p.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Request:    request,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("ok")),
		}, nil
	})}
	p.dockerRun = func(_ context.Context, args ...string) (string, error) {
		*commands = append(*commands, append([]string(nil), args...))
		switch {
		case args[0] == "compose" && slices.Contains(args, "--images"):
			return "runtime:latest\nruntime:latest\n", nil
		case args[0] == "volume":
			return payload.ProjectName + "\n", nil
		case args[0] == "ps":
			return "aaaaaaaaaaaa\nbbbbbbbbbbbb\n", nil
		case args[0] == "inspect":
			containers := []map[string]any{
				{"Id": "aaaaaaaaaaaa", "Image": imageID, "Config": map[string]any{"Labels": map[string]string{
					"io.hermes-fleet.managed": "true", "io.hermes-fleet.instance-id": payload.InstanceID,
					"com.docker.compose.project": payload.ProjectName, "com.docker.compose.service": "hermes",
				}}, "State": map[string]any{"Status": "running"}},
				{"Id": "bbbbbbbbbbbb", "Image": imageID, "Config": map[string]any{"Labels": map[string]string{
					"io.hermes-fleet.managed": "true", "io.hermes-fleet.instance-id": payload.InstanceID,
					"com.docker.compose.project": payload.ProjectName, "com.docker.compose.service": "dashboard",
				}}, "State": map[string]any{"Status": "running"}},
			}
			encoded, marshalErr := json.Marshal(containers)
			return string(encoded), marshalErr
		case args[0] == "image" && slices.Contains(args, "{{json .Config.Labels}}"):
			return runtimeImageLabelsJSON(runtimeStateSchemaVersion, testRuntimeBuildID), nil
		case args[0] == "image":
			return imageID + "\n", nil
		case args[0] == "run" && slices.Contains(args, runtimeConfigSnapshotScript):
			return string(snapshotJSON), nil
		case args[0] == "exec" && slices.Contains(args, runtimeStateApply):
			return "", nil
		case args[0] == "compose" && slices.Contains(args, "--force-recreate"):
			return "services recreated", nil
		case args[0] == "compose" && slices.Contains(args, runtimeStateProbe):
			return readyRuntimeStateJSON(payload.Provider, payload.Model, payload.Reasoning, payload.ServiceTier), nil
		default:
			return "", fmt.Errorf("unexpected Docker command: %v", args)
		}
	}
	return p, payload, commands
}

func readyRuntimeStateJSON(provider, model, reasoning, serviceTier string) string {
	return readyRuntimeStateJSONForCapability(
		provider, model, reasoning, serviceTier,
		runtimeImageCapability{SchemaVersion: runtimeStateSchemaVersion, BuildID: testRuntimeBuildID},
	)
}

var testRuntimeBuildID = strings.Repeat("b", 64)

func readyRuntimeStateJSONForCapability(
	provider, model, reasoning, serviceTier string,
	capability runtimeImageCapability,
) string {
	revision := legacyRuntimeConfigurationRevision(provider, model)
	state := map[string]any{
		"schema_version":         capability.SchemaVersion,
		"configuration_revision": revision,
		"provider":               provider,
		"model":                  model,
		"runtime_build_id":       capability.BuildID,
	}
	if capability.SchemaVersion == runtimeStateSchemaVersion {
		state["configuration_revision"] = runtimeConfigurationRevision(provider, model, reasoning, serviceTier)
		state["reasoning"] = reasoning
		state["service_tier"] = serviceTier
	}
	encoded, _ := json.Marshal(map[string]any{
		"agent": map[string]any{
			"reasoning_effort": reasoning,
			"service_tier":     serviceTier,
		},
		"environment": map[string]any{
			"provider":     provider,
			"model":        model,
			"reasoning":    reasoning,
			"service_tier": serviceTier,
		},
		"model": map[string]any{
			"default":  model,
			"provider": provider,
		},
		"state": state,
	})
	return string(encoded)
}

func runtimeImageLabelsJSON(schemaVersion int, buildID string) string {
	encoded, _ := json.Marshal(map[string]string{
		"io.hermes-fleet.runtime-config-schema": strconv.Itoa(schemaVersion),
		"io.hermes-fleet.runtime-build-id":      buildID,
	})
	return string(encoded)
}
