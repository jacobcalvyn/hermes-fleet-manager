package api

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/backup"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/chatartifacts"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/cloudflare"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/cloudflareoauth"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/mcpdiscovery"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/recovery"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/releases"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/remoteaccess"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/security"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/store"
)

func TestHealthEndpointAcceptsOptionalTrailingSlash(t *testing.T) {
	environment := newAPITestEnvironment(t)

	for _, path := range []string{"/healthz", "/healthz/"} {
		t.Run(path, func(t *testing.T) {
			response := environment.request(t, http.MethodGet, path, nil, "", nil)
			defer response.Body.Close()

			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
			}
			if contentType := response.Header.Get("Content-Type"); contentType != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", contentType)
			}

			var payload struct {
				Status  string `json:"status"`
				Version string `json:"version"`
				BuildID string `json:"build_id"`
			}
			if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
				t.Fatalf("decode health response: %v", err)
			}
			if payload.Status != "ok" || payload.Version != agentVersion || payload.BuildID != BuildID {
				t.Fatalf("health payload = %+v, want status ok, version %s, and build %s", payload, agentVersion, BuildID)
			}
		})
	}
}

func TestSecurityHeadersAreAppliedAndAPIResponsesAreNotCached(t *testing.T) {
	environment := newAPITestEnvironment(t)

	for _, path := range []string{"/healthz", "/api/v1/hosts"} {
		t.Run(path, func(t *testing.T) {
			response := environment.request(t, http.MethodGet, path, nil, "", nil)
			defer response.Body.Close()
			if response.Header.Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("X-Content-Type-Options = %q", response.Header.Get("X-Content-Type-Options"))
			}
			if response.Header.Get("X-Frame-Options") != "DENY" {
				t.Fatalf("X-Frame-Options = %q", response.Header.Get("X-Frame-Options"))
			}
			if !strings.Contains(response.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
				t.Fatalf("Content-Security-Policy = %q", response.Header.Get("Content-Security-Policy"))
			}
			if strings.HasPrefix(path, "/api/") && response.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", response.Header.Get("Cache-Control"))
			}
		})
	}
}

func TestDeleteMissingChatSessionReturnsNotFound(t *testing.T) {
	environment := newAPITestEnvironment(t)
	response := environment.request(t, http.MethodDelete, "/api/v1/chats/missing-session", nil, environment.adminToken, nil)
	defer response.Body.Close()

	if response.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("delete missing chat status=%d body=%s, want %d", response.StatusCode, body, http.StatusNotFound)
	}
}

func TestCreateHermesProfileRejectsReservedNameBeforeQueueing(t *testing.T) {
	environment := newAPITestEnvironment(t)
	response := environment.request(t, http.MethodPost, "/api/v1/instances/missing-instance/profiles", map[string]string{
		"name": "root", "clone_from": "default",
	}, environment.adminToken, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d body=%s, want %d", response.StatusCode, body, http.StatusBadRequest)
	}
}

func TestHermesProfileLifecycleValidatesTargetBeforeQueueing(t *testing.T) {
	environment := newAPITestEnvironment(t)
	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "activate invalid profile", method: http.MethodPost, path: "/api/v1/instances/missing-instance/profiles/INVALID/active"},
		{name: "delete invalid profile", method: http.MethodDelete, path: "/api/v1/instances/missing-instance/profiles/INVALID"},
		{name: "delete default profile", method: http.MethodDelete, path: "/api/v1/instances/missing-instance/profiles/default"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := environment.request(t, test.method, test.path, nil, environment.adminToken, nil)
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("status=%d body=%s, want %d", response.StatusCode, body, http.StatusBadRequest)
			}
		})
	}
}

func TestHermesProfileLifecycleRoutesReachInstanceAdmission(t *testing.T) {
	environment := newAPITestEnvironment(t)
	for _, test := range []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/api/v1/instances/missing-instance/profiles/research/active"},
		{method: http.MethodDelete, path: "/api/v1/instances/missing-instance/profiles/research"},
	} {
		response := environment.request(t, test.method, test.path, nil, environment.adminToken, nil)
		defer response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("%s %s status=%d body=%s, want %d", test.method, test.path, response.StatusCode, body, http.StatusNotFound)
		}
	}
}

func TestLivenessAndReadinessEndpointsAreDistinct(t *testing.T) {
	environment := newAPITestEnvironment(t)
	for _, test := range []struct {
		path  string
		field string
	}{
		{path: "/livez", field: "status"},
		{path: "/readyz", field: "ready"},
	} {
		response := environment.request(t, http.MethodGet, test.path, nil, "", nil)
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want %d", test.path, response.StatusCode, http.StatusOK)
		}
		var payload map[string]any
		if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if _, ok := payload[test.field]; !ok {
			t.Fatalf("%s payload does not contain %q: %#v", test.path, test.field, payload)
		}
	}
}

func TestRuntimeHealthAndOverviewExposeAuthoritativeRevision(t *testing.T) {
	environment := newAPITestEnvironment(t)
	response := environment.request(t, http.MethodGet, "/api/v1/overview", nil, environment.adminToken, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("initial overview status = %d", response.StatusCode)
	}
	var initial struct {
		StreamID string `json:"stream_id"`
		Revision uint64 `json:"state_revision"`
	}
	decodeResponse(t, response, &initial)
	response.Body.Close()
	if initial.StreamID == "" {
		t.Fatal("overview stream ID is empty")
	}

	environment.enrollHost(t)
	response = environment.request(t, http.MethodGet, "/api/v1/overview", nil, environment.adminToken, nil)
	var changed struct {
		StreamID string `json:"stream_id"`
		Revision uint64 `json:"state_revision"`
	}
	decodeResponse(t, response, &changed)
	response.Body.Close()
	if changed.StreamID != initial.StreamID || changed.Revision <= initial.Revision {
		t.Fatalf("state revision = %s/%d after %s/%d; want same stream and a newer revision", changed.StreamID, changed.Revision, initial.StreamID, initial.Revision)
	}

	response = environment.request(t, http.MethodGet, "/api/v1/system/runtime-health", nil, environment.adminToken, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("runtime health status = %d", response.StatusCode)
	}
	var health runtimeHealthResponse
	decodeResponse(t, response, &health)
	response.Body.Close()
	if health.StreamID != changed.StreamID || health.StateRevision != changed.Revision {
		t.Fatalf("runtime health revision = %s/%d, overview = %s/%d", health.StreamID, health.StateRevision, changed.StreamID, changed.Revision)
	}
	if health.Compatibility.HostAgentVersion != agentVersion || health.Queue.MaxPerHost != store.JobQueueMaxPerHost {
		t.Fatalf("runtime health contract = %+v", health)
	}
}

func TestRuntimeHealthAndHealthEndpointRemainAvailableWithPendingJob(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, _ := environment.enrollHost(t)
	response := environment.request(t, http.MethodPost, "/api/v1/instances", createInstanceRequest{
		Name: "fleet-queue-health", HostID: hostID, HermesVersion: "0.19.0",
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create instance status=%d body=%s", response.StatusCode, body)
	}
	response.Body.Close()

	response = environment.request(t, http.MethodGet, "/api/v1/system/runtime-health", nil, environment.adminToken, nil)
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("runtime health status=%d body=%s", response.StatusCode, body)
	}
	var health runtimeHealthResponse
	decodeResponse(t, response, &health)
	response.Body.Close()
	if health.Queue.Pending != 1 || health.Queue.Active != 1 || len(health.Queue.Hosts) != 1 || health.Queue.Hosts[0].OldestPendingAt == nil {
		t.Fatalf("runtime queue health = %+v, want one pending active job", health.Queue)
	}

	response = environment.request(t, http.MethodGet, "/healthz", nil, "", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("health status=%d body=%s", response.StatusCode, body)
	}
}

func TestRecoveryKitRequiresVerifiedBackups(t *testing.T) {
	environment := newAPITestEnvironment(t)
	response := environment.request(t, http.MethodPost, "/api/v1/system/recovery-kit/download", nil, environment.adminToken, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusConflict)
	}
}

func TestVisibleFleetHealthHistoryOmitsInactiveRemoteAccess(t *testing.T) {
	components := []store.FleetHealthState{
		{Component: "control_plane", Status: "healthy"},
		{Component: "remote_access", Status: "degraded"},
	}
	incidents := []store.FleetHealthIncident{
		{ID: 1, Component: "remote_access", Status: "degraded"},
		{ID: 2, Component: "host_queue", Status: "healthy"},
	}

	visibleComponents, visibleIncidents := visibleFleetHealthHistory(components, incidents, false)
	if len(visibleComponents) != 1 || visibleComponents[0].Component != "control_plane" {
		t.Fatalf("visible components = %+v; want only control_plane", visibleComponents)
	}
	if len(visibleIncidents) != 1 || visibleIncidents[0].Component != "host_queue" {
		t.Fatalf("visible incidents = %+v; want only host_queue", visibleIncidents)
	}

	allComponents, allIncidents := visibleFleetHealthHistory(components, incidents, true)
	if len(allComponents) != len(components) || len(allIncidents) != len(incidents) {
		t.Fatalf("configured remote access history was filtered: %+v / %+v", allComponents, allIncidents)
	}
}

func TestValidateCreateInstance(t *testing.T) {
	valid := createInstanceRequest{
		Name: "fleet-test-01", HostID: "host", Image: "local/hermes-fleet-runtime:0.18.2",
		Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
		APIPort: 8650, DashboardPort: 9130,
	}
	tests := []struct {
		name    string
		request createInstanceRequest
		wantErr bool
	}{
		{name: "valid", request: valid},
		{name: "pending Codex setup", request: withCreateRequest(valid, func(request *createInstanceRequest) {
			request.Model, request.Reasoning, request.ServiceTier = "", "", ""
		})},
		{name: "pending Grok setup", request: withCreateRequest(valid, func(request *createInstanceRequest) {
			request.Provider, request.Model, request.Reasoning, request.ServiceTier = "xai-oauth", "", "", ""
		})},
		{name: "api-key provider cannot be created", request: withCreateRequest(valid, func(request *createInstanceRequest) {
			request.Provider = "openrouter"
		}), wantErr: true},
		{name: "partial Codex setup", request: withCreateRequest(valid, func(request *createInstanceRequest) {
			request.Model, request.Reasoning, request.ServiceTier = "", "medium", ""
		}), wantErr: true},
		{name: "automatic ports", request: withCreateRequest(valid, func(request *createInstanceRequest) { request.APIPort, request.DashboardPort = 0, 0 })},
		{name: "unsafe name", request: withCreateRequest(valid, func(request *createInstanceRequest) { request.Name = "../agent" }), wantErr: true},
		{name: "missing host", request: withCreateRequest(valid, func(request *createInstanceRequest) { request.HostID = "" }), wantErr: true},
		{name: "duplicate ports", request: withCreateRequest(valid, func(request *createInstanceRequest) { request.DashboardPort = request.APIPort }), wantErr: true},
		{name: "privileged port", request: withCreateRequest(valid, func(request *createInstanceRequest) { request.APIPort = 80 }), wantErr: true},
		{name: "compose injection in image", request: withCreateRequest(valid, func(request *createInstanceRequest) { request.Image = "runtime:latest\n    volumes: [/tmp:/host]" }), wantErr: true},
		{name: "unsupported provider", request: withCreateRequest(valid, func(request *createInstanceRequest) { request.Provider = "shell" }), wantErr: true},
		{name: "unsafe model", request: withCreateRequest(valid, func(request *createInstanceRequest) { request.Model = "model\nINJECTED=value" }), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateCreateInstance(&test.request)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateCreateInstance() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestValidateObservationsAcceptsRuntimeConfigurationCheck(t *testing.T) {
	now := time.Now().UTC()
	report := domain.InstanceObservation{
		InstanceID:       "00000000-0000-4000-8000-000000000001",
		TargetGeneration: now.Add(-time.Minute).Format(time.RFC3339Nano),
		HermesVersion:    "0.18.2",
		HermesSource:     "7acaff5ef2bc",
		ModelCatalog:     []string{"gpt-5.6-sol", "gpt-5.6-terra"},
		RecommendedModel: "gpt-5.6-sol",
		Status:           domain.ObservationDegraded,
		Summary:          "Runtime drift detected",
		Checks: []domain.ObservationCheck{{
			Name: "runtime_configuration", Status: domain.ObservationCheckDrift,
			Detail: "Hermes has not applied the Fleet provider and model",
		}},
		ObservedAt: now,
	}
	if err := validateObservations([]domain.InstanceObservation{report}, now, 2*time.Minute); err != nil {
		t.Fatalf("validateObservations() rejected runtime configuration check: %v", err)
	}
	report.HermesVersion = "0.18.2\nforged"
	if err := validateObservations([]domain.InstanceObservation{report}, now, 2*time.Minute); err == nil {
		t.Fatal("validateObservations() accepted an unsafe Hermes version")
	}
	report.HermesVersion = "0.18.2"
	report.HermesSource = "7acaff5ef2bc\nforged"
	if err := validateObservations([]domain.InstanceObservation{report}, now, 2*time.Minute); err == nil {
		t.Fatal("validateObservations() accepted an unsafe Hermes source")
	}
	report.HermesSource = "7acaff5ef2bc"
	report.RecommendedModel = "not-in-catalog"
	if err := validateObservations([]domain.InstanceObservation{report}, now, 2*time.Minute); err == nil {
		t.Fatal("validateObservations() accepted a recommendation outside the model catalog")
	}
	report.RecommendedModel = "gpt-5.6-sol"
	report.ObservedAt = now.Add(-2*time.Minute - time.Nanosecond)
	if err := validateObservations([]domain.InstanceObservation{report}, now, 2*time.Minute); err == nil {
		t.Fatal("validateObservations() accepted a delayed report")
	}
}

func TestValidateJobProgressRejectsUntrustedDeviceFlowData(t *testing.T) {
	now := time.Now().UTC()
	valid := domain.JobProgress{Stage: "AWAITING_USER", VerificationURI: codexDeviceURL, UserCode: "ABCD-EFGH", ExpiresAt: now.Add(15 * time.Minute)}
	if err := validateJobProgress("instance.auth.codex", valid, now); err != nil {
		t.Fatalf("validateJobProgress(valid) error=%v", err)
	}
	grok := domain.JobProgress{Stage: "AWAITING_USER", VerificationURI: "https://auth.x.ai/oauth2/device?user_code=ABCD-EFGH", UserCode: "ABCD-EFGH"}
	if err := validateJobProgress("instance.auth.codex", grok, now); err != nil {
		t.Fatalf("validateJobProgress(grok) error=%v", err)
	}
	for name, progress := range map[string]domain.JobProgress{
		"foreign URL":   {Stage: "AWAITING_USER", VerificationURI: "https://attacker.invalid/device", UserCode: "ABCD-EFGH", ExpiresAt: now.Add(15 * time.Minute)},
		"unsafe code":   {Stage: "AWAITING_USER", VerificationURI: codexDeviceURL, UserCode: "<script>", ExpiresAt: now.Add(15 * time.Minute)},
		"long expiry":   {Stage: "AWAITING_USER", VerificationURI: codexDeviceURL, UserCode: "ABCD-EFGH", ExpiresAt: now.Add(17 * time.Minute)},
		"code in start": {Stage: "STARTING", UserCode: "ABCD-EFGH"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateJobProgress("instance.auth.codex", progress, now); err == nil {
				t.Fatalf("validateJobProgress(%+v) accepted untrusted progress", progress)
			}
		})
	}
}

func TestJSONDecodersRejectBodiesLargerThanOneMiB(t *testing.T) {
	requiredPrefix := `{"value":"ok"}`
	oversizedRequired := requiredPrefix + strings.Repeat(" ", maximumJSONBodyBytes-len(requiredPrefix)+1)
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(oversizedRequired))
	var requiredTarget struct {
		Value string `json:"value"`
	}
	if err := decodeJSON(request, &requiredTarget); err == nil || !strings.Contains(err.Error(), "body exceeds") {
		t.Fatalf("decodeJSON() error=%v, want an explicit body-size error", err)
	}

	request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat(" ", maximumJSONBodyBytes+1)))
	var optionalTarget struct{}
	if err := decodeOptionalJSON(request, &optionalTarget); err == nil || !strings.Contains(err.Error(), "body exceeds") {
		t.Fatalf("decodeOptionalJSON() error=%v, want an explicit body-size error", err)
	}
}

func TestJSONDecodersAcceptBodiesAtOneMiBBoundary(t *testing.T) {
	requiredPrefix := `{"value":"ok"}`
	boundaryRequired := requiredPrefix + strings.Repeat(" ", maximumJSONBodyBytes-len(requiredPrefix))
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(boundaryRequired))
	var requiredTarget struct {
		Value string `json:"value"`
	}
	if err := decodeJSON(request, &requiredTarget); err != nil || requiredTarget.Value != "ok" {
		t.Fatalf("decodeJSON() target=%+v error=%v", requiredTarget, err)
	}

	request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat(" ", maximumJSONBodyBytes)))
	var optionalTarget struct{}
	if err := decodeOptionalJSON(request, &optionalTarget); err != nil {
		t.Fatalf("decodeOptionalJSON() error=%v", err)
	}
}

func TestCreateInstanceLeavesOAuthConfigurationPending(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, _ := environment.enrollHost(t)
	response := environment.request(t, http.MethodPost, "/api/v1/instances", map[string]any{
		"name": "fleet-defaults-01", "host_id": hostID,
		"provider": "openrouter", "model": "untrusted-model", "reasoning": "high", "service_tier": "priority",
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create instance with API-key provider status=%d body=%s", response.StatusCode, body)
	}
	response.Body.Close()

	response = environment.request(t, http.MethodPost, "/api/v1/instances", map[string]any{
		"name": "fleet-defaults-01", "host_id": hostID,
		"model": "untrusted-model", "reasoning": "high", "service_tier": "priority",
	}, environment.adminToken, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("create instance status=%d body=%s", response.StatusCode, body)
	}
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	if instance.Provider != "openai-codex" || instance.Model != "" || instance.Reasoning != "" || instance.ServiceTier != "" || instance.Image == "" {
		t.Fatalf("Codex configuration was not left pending: %+v", instance)
	}
	if instance.APIPort < 1024 || instance.DashboardPort < 1024 || instance.APIPort == instance.DashboardPort {
		t.Fatalf("automatic ports were not allocated safely: %+v", instance)
	}
}

func TestCompleteJobAcknowledgesLostResponseForOriginalLease(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	response := environment.request(t, http.MethodPost, "/api/v1/instances", createInstanceRequest{
		Name: "fleet-completion-retry", HostID: hostID, HermesVersion: "0.19.0",
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create instance status=%d body=%s", response.StatusCode, body)
	}
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()

	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	shortID := strings.ReplaceAll(instance.ID, "-", "")[:8]
	projectName := "hermes-fleet-" + instance.Name + "-" + shortID
	result := domain.JobResult{
		Success: true, ProjectName: projectName, DataVolume: projectName + "-data",
		ManagedPath: "/managed/" + instance.Name + "-" + shortID,
		ImageID:     "sha256:" + strings.Repeat("a", 64),
	}
	headers := map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", result, hostToken, headers)
	assertStatus(t, response, http.StatusNoContent)

	// The Host Agent may not receive the first 204 even though the transaction
	// committed. Retrying the same completion under the original active lease
	// must be acknowledged without replaying lifecycle side effects.
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", result, hostToken, headers)
	assertStatus(t, response, http.StatusNoContent)
	if stored := environment.overviewInstance(t, instance.ID); stored.Status != domain.InstanceRunning ||
		stored.ProjectName != projectName {
		t.Fatalf("duplicate completion changed the provisioned instance: %+v", stored)
	}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/renew", map[string]any{}, hostToken, headers)
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: false, Error: "contradictory duplicate",
	}, hostToken, headers)
	assertStatus(t, response, http.StatusConflict)
	if stored := environment.overviewInstance(t, instance.ID); stored.Status != domain.InstanceRunning ||
		stored.ProjectName != projectName {
		t.Fatalf("contradictory duplicate changed the provisioned instance: %+v", stored)
	}

	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", result, hostToken, map[string]string{
		"X-Fleet-Host-ID": hostID, leaseTokenHeader: "different-token",
	})
	assertStatus(t, response, http.StatusConflict)
}

func TestCompletionErrorStatusSeparatesConflictsFromInternalFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "lease lost", err: store.ErrLeaseLost, want: http.StatusConflict},
		{name: "mismatched duplicate", err: store.ErrStateChanged, want: http.StatusConflict},
		{name: "invalid deterministic result", err: store.ErrInvalidJobResult, want: http.StatusConflict},
		{name: "missing recovery artifact", err: recovery.ErrNotFound, want: http.StatusConflict},
		{name: "recovery state conflict", err: recovery.ErrState, want: http.StatusConflict},
		{name: "wrapped conflict", err: fmt.Errorf("complete: %w", store.ErrLeaseLost), want: http.StatusConflict},
		{name: "wrapped invalid deterministic result", err: fmt.Errorf("complete: %w", store.ErrInvalidJobResult), want: http.StatusConflict},
		{name: "database failure", err: errors.New("database is temporarily unavailable"), want: http.StatusInternalServerError},
		{name: "filesystem failure", err: errors.New("temporary input/output error"), want: http.StatusInternalServerError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := completionErrorStatus(test.err); got != test.want {
				t.Fatalf("completionErrorStatus(%v)=%d, want %d", test.err, got, test.want)
			}
		})
	}
}

func TestCompleteJobReturnsServerErrorForTransientMetadataFailure(t *testing.T) {
	environment := newAPITestEnvironment(t)
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/agent/jobs/job-transient/complete",
		strings.NewReader(`{"success":true}`),
	)
	request.SetPathValue("jobID", "job-transient")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Fleet-Host-ID", "host-transient")
	request.Header.Set(leaseTokenHeader, "lease-transient")
	ctx, cancel := context.WithCancel(request.Context())
	cancel()
	request = request.WithContext(ctx)

	recorder := httptest.NewRecorder()
	environment.server.completeJob(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("completeJob() status=%d body=%s, want %d", recorder.Code, recorder.Body.String(), http.StatusInternalServerError)
	}
}

func TestCompleteJobReturnsServerErrorThenAcceptsIdenticalRetryAfterTransientStoreFailure(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	response := environment.request(t, http.MethodPost, "/api/v1/instances", createInstanceRequest{
		Name: "fleet-completion-store-retry", HostID: hostID, HermesVersion: "0.19.0",
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create instance status=%d body=%s", response.StatusCode, body)
	}
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()

	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	shortID := strings.ReplaceAll(instance.ID, "-", "")[:8]
	projectName := "hermes-fleet-" + instance.Name + "-" + shortID
	result := domain.JobResult{
		Success: true, ProjectName: projectName, DataVolume: projectName + "-data",
		ManagedPath: "/managed/" + instance.Name + "-" + shortID,
		ImageID:     "sha256:" + strings.Repeat("a", 64),
	}
	headers := map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken}

	// Force a database error after completion metadata has been read. The API
	// must return a retryable 5xx without committing any lifecycle side effect.
	database, err := sql.Open("sqlite", environment.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`
CREATE TRIGGER fail_job_completion
BEFORE UPDATE OF status ON jobs
WHEN NEW.status IN ('SUCCEEDED', 'FAILED')
BEGIN
  SELECT RAISE(ABORT, 'injected transient completion failure');
END`); err != nil {
		t.Fatal(err)
	}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", result, hostToken, headers)
	assertStatus(t, response, http.StatusInternalServerError)
	if stored := environment.overviewInstance(t, instance.ID); stored.Status != domain.InstanceProvisioning || stored.ProjectName != "" {
		t.Fatalf("transient completion failure committed lifecycle state: %+v", stored)
	}

	if _, err := database.Exec(`DROP TRIGGER fail_job_completion`); err != nil {
		t.Fatal(err)
	}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", result, hostToken, headers)
	assertStatus(t, response, http.StatusNoContent)
	if stored := environment.overviewInstance(t, instance.ID); stored.Status != domain.InstanceRunning ||
		stored.ProjectName != projectName {
		t.Fatalf("identical completion retry was not committed exactly once: %+v", stored)
	}
}

func TestCodexConfigurationRequiresAuthenticationAndPersistsOnlyAfterHostSuccess(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	response := environment.request(t, http.MethodPost, "/api/v1/instances", map[string]any{
		"name": "fleet-configure-01", "host_id": hostID,
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create instance status=%d body=%s", response.StatusCode, body)
	}
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()

	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	shortID := strings.ReplaceAll(instance.ID, "-", "")[:8]
	projectName := "hermes-fleet-" + instance.Name + "-" + shortID
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ProjectName: projectName, DataVolume: projectName + "-data",
		ManagedPath: "/managed/" + instance.Name + "-" + shortID, ImageID: "sha256:" + strings.Repeat("a", 64),
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)

	configuration := map[string]string{"model": "gpt-5.6-sol", "reasoning": "medium", "service_tier": "normal"}
	response = environment.request(t, http.MethodPut, "/api/v1/instances/"+instance.ID+"/codex-configuration", configuration, environment.adminToken, nil)
	assertStatus(t, response, http.StatusConflict)

	targets, err := environment.dataStore.ListObservationTargets(context.Background(), hostID)
	if err != nil || len(targets) != 1 {
		t.Fatalf("observation targets=%+v error=%v", targets, err)
	}
	report := domain.InstanceObservation{
		InstanceID: instance.ID, TargetGeneration: targets[0].Generation, Status: domain.ObservationDegraded,
		Summary:      "Codex configuration is required",
		ModelCatalog: []string{"gpt-5.6-sol", "gpt-5.6-terra"}, RecommendedModel: "gpt-5.6-sol",
		Checks: []domain.ObservationCheck{
			{Name: "codex_auth", Status: domain.ObservationCheckOK, Detail: "Codex authentication is connected"},
			{Name: "runtime_configuration", Status: domain.ObservationCheckDrift, Detail: "Choose a Codex model in Hermes Fleet"},
		},
		ObservedAt: time.Now().UTC(),
	}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/observations", map[string]any{"observations": []domain.InstanceObservation{report}}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodPut, "/api/v1/instances/"+instance.ID+"/codex-configuration", configuration, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("configure Codex status=%d body=%s", response.StatusCode, body)
	}
	response.Body.Close()
	stored := environment.overviewInstance(t, instance.ID)
	if stored.Model != "" || stored.Reasoning != "" || stored.ServiceTier != "" || stored.CodexConfigured {
		t.Fatalf("configuration was persisted before Host Agent success: %+v", stored)
	}

	job = environment.claimAndAcknowledge(t, hostID, hostToken)
	if job.Type != "instance.runtime.configure" {
		t.Fatalf("configuration job type=%q", job.Type)
	}
	var payload domain.RuntimeSyncPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Model != "gpt-5.6-sol" || payload.Reasoning != "medium" || payload.ServiceTier != "normal" {
		t.Fatalf("configuration payload=%+v", payload)
	}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, InstanceStatus: domain.InstanceRunning,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)
	stored = environment.overviewInstance(t, instance.ID)
	if stored.Model != "gpt-5.6-sol" || stored.Reasoning != "medium" || stored.ServiceTier != "normal" ||
		!stored.CodexConfigured || stored.Status != domain.InstanceRunning {
		t.Fatalf("configuration was not persisted after Host Agent success: %+v", stored)
	}

	targets, err = environment.dataStore.ListObservationTargets(context.Background(), hostID)
	if err != nil || len(targets) != 1 {
		t.Fatalf("configured observation targets=%+v error=%v", targets, err)
	}
	report.TargetGeneration = targets[0].Generation
	report.Status = domain.ObservationInSync
	report.Summary = "Hermes is ready"
	report.Checks = []domain.ObservationCheck{
		{Name: "codex_auth", Status: domain.ObservationCheckOK, Detail: "Codex authentication is connected"},
		{Name: "runtime_configuration", Status: domain.ObservationCheckOK, Detail: "Configuration matches Fleet"},
	}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/observations",
		map[string]any{"observations": []domain.InstanceObservation{report}}, hostToken,
		map[string]string{"X-Fleet-Host-ID": hostID})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodPost, "/api/v1/chats",
		map[string]string{"instance_id": instance.ID}, environment.adminToken, nil)
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create chat status=%d body=%s", response.StatusCode, body)
	}
	var session domain.ChatSession
	decodeResponse(t, response, &session)
	response.Body.Close()
	defaultTitleSuffix := strings.TrimPrefix(session.Title, "Chat ")
	defaultTitleNumber, titleErr := strconv.Atoi(defaultTitleSuffix)
	if titleErr != nil || defaultTitleNumber < 100 || defaultTitleNumber > 999 || len(defaultTitleSuffix) != 3 {
		t.Fatalf("new chat default title=%q", session.Title)
	}
	if session.Model != "gpt-5.6-sol" || session.Reasoning != "medium" || session.ServiceTier != "normal" {
		t.Fatalf("new chat did not snapshot instance configuration: %+v", session)
	}
	response = environment.request(t, http.MethodPatch, "/api/v1/chats/"+session.ID,
		map[string]string{"model": "gpt-5.6-terra", "reasoning": "high", "service_tier": "priority"},
		environment.adminToken, nil)
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("update chat configuration status=%d body=%s", response.StatusCode, body)
	}
	var updatedSession domain.ChatSession
	decodeResponse(t, response, &updatedSession)
	response.Body.Close()
	if updatedSession.Model != "gpt-5.6-terra" || updatedSession.Reasoning != "high" || updatedSession.ServiceTier != "priority" {
		t.Fatalf("session configuration was not updated: %+v", updatedSession)
	}
	stored = environment.overviewInstance(t, instance.ID)
	if stored.Model != "gpt-5.6-sol" || stored.Reasoning != "medium" || stored.ServiceTier != "normal" {
		t.Fatalf("session configuration mutated instance defaults: %+v", stored)
	}
	response = environment.request(t, http.MethodPatch, "/api/v1/chats/"+session.ID,
		map[string]string{"model": "gpt-not-in-catalog", "reasoning": "low", "service_tier": "normal"},
		environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)
	response = environment.request(t, http.MethodPatch, "/api/v1/chats/"+session.ID,
		map[string]string{"model": "gpt-5.6-sol", "reasoning": "max", "service_tier": "normal"},
		environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)
}

func TestCodexConfigurationFailureRecordsManualRecoveryStateWithoutPersistingSettings(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	response := environment.request(t, http.MethodPost, "/api/v1/instances", map[string]any{
		"name": "fleet-configure-recovery-01", "host_id": hostID,
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create instance status=%d body=%s", response.StatusCode, body)
	}
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()

	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	shortID := strings.ReplaceAll(instance.ID, "-", "")[:8]
	projectName := "hermes-fleet-" + instance.Name + "-" + shortID
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ProjectName: projectName, DataVolume: projectName + "-data",
		ManagedPath: "/managed/" + instance.Name + "-" + shortID, ImageID: "sha256:" + strings.Repeat("a", 64),
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)

	targets, err := environment.dataStore.ListObservationTargets(context.Background(), hostID)
	if err != nil || len(targets) != 1 {
		t.Fatalf("observation targets=%+v error=%v", targets, err)
	}
	report := domain.InstanceObservation{
		InstanceID: instance.ID, TargetGeneration: targets[0].Generation, Status: domain.ObservationDegraded,
		Summary:      "Codex configuration is required",
		ModelCatalog: []string{"gpt-5.6-sol"}, RecommendedModel: "gpt-5.6-sol",
		Checks: []domain.ObservationCheck{
			{Name: "codex_auth", Status: domain.ObservationCheckOK, Detail: "Codex authentication is connected"},
			{Name: "runtime_configuration", Status: domain.ObservationCheckDrift, Detail: "Choose a Codex model in Hermes Fleet"},
		},
		ObservedAt: time.Now().UTC(),
	}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/observations", map[string]any{
		"observations": []domain.InstanceObservation{report},
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodPut, "/api/v1/instances/"+instance.ID+"/codex-configuration", map[string]string{
		"provider": "xai-oauth", "model": "grok-4.6", "reasoning": "minimal", "service_tier": "normal",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)
	response.Body.Close()

	response = environment.request(t, http.MethodPut, "/api/v1/instances/"+instance.ID+"/codex-configuration", map[string]string{
		"model": "gpt-5.6-sol", "reasoning": "medium", "service_tier": "normal",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)
	response.Body.Close()

	job = environment.claimAndAcknowledge(t, hostID, hostToken)
	if job.Type != "instance.runtime.configure" {
		t.Fatalf("configuration job type=%q", job.Type)
	}
	const recoveryError = "Hermes Dashboard did not become ready; automatic rollback failed; manual recovery is required"
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: false, Error: recoveryError, InstanceStatus: domain.InstanceFailed,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)

	stored := environment.overviewInstance(t, instance.ID)
	if stored.Status != domain.InstanceFailed || stored.LastError != recoveryError {
		t.Fatalf("manual recovery state was not recorded: %+v", stored)
	}
	if stored.Model != "" || stored.Reasoning != "" || stored.ServiceTier != "" || stored.CodexConfigured {
		t.Fatalf("unverified configuration was persisted after failure: %+v", stored)
	}
}

func withCreateRequest(request createInstanceRequest, update func(*createInstanceRequest)) createInstanceRequest {
	update(&request)
	return request
}

func TestAuthenticationAndEnrollmentFlow(t *testing.T) {
	environment := newAPITestEnvironment(t)

	response := environment.request(t, http.MethodGet, "/api/v1/overview", nil, "", nil)
	assertStatus(t, response, http.StatusUnauthorized)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/enroll", map[string]string{
		"enrollment_token": "wrong-token", "name": "local-test", "hostname": "host",
		"os": "darwin", "arch": "arm64", "agent_version": agentVersion,
	}, "", nil)
	assertStatus(t, response, http.StatusUnauthorized)

	hostID, hostToken := environment.enrollHost(t)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/heartbeat", map[string]string{
		"hostname": "host", "os": "darwin", "arch": "arm64", "agent_version": agentVersion,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodGet, "/api/v1/overview", nil, environment.adminToken, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("overview status=%d", response.StatusCode)
	}
	var overview struct {
		Hosts []domain.Host `json:"hosts"`
	}
	decodeResponse(t, response, &overview)
	if len(overview.Hosts) != 1 || overview.Hosts[0].Status != domain.HostOnline {
		t.Fatalf("overview hosts=%+v", overview.Hosts)
	}

	response = environment.request(t, http.MethodGet, "/api/v1/system", nil, environment.adminToken, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("system info status=%d cache=%q", response.StatusCode, response.Header.Get("Cache-Control"))
	}
	var system systemInfoResponse
	decodeResponse(t, response, &system)
	if system.FleetVersion != agentVersion || system.BuildID != BuildID || system.OperatorURL != "http://127.0.0.1:9180" || system.DatabasePath == "" || system.BackupRetention != 20 {
		t.Fatalf("system info=%+v", system)
	}
	if system.RemoteAccess.Configured || system.RemoteAccess.State != "disabled" {
		t.Fatalf("remote access=%+v", system.RemoteAccess)
	}
	response = environment.request(t, http.MethodPost, "/api/v1/system/remote-access/reconcile", nil, environment.adminToken, nil)
	assertStatus(t, response, http.StatusConflict)
}

func TestRemoteAccessConfigurationEncryptsTunnelTokensAndKeepsThemOnBlankUpdate(t *testing.T) {
	const (
		adminTunnelToken     = "eyJ-admin-connector-token-that-is-long-enough"
		instancesTunnelToken = "eyJhIjoiYWNjb3VudCIsInQiOiIyMjIyMjIyMi0yMjIyLTQyMjItODIyMi0yMjIyMjIyMjIyMjIiLCJzIjoic2VjcmV0In0"
	)
	environment := newAPITestEnvironment(t)
	runtimeDirectory := t.TempDir()
	adminTokenPath := filepath.Join(runtimeDirectory, "admin", "token")
	instancesTokenPath := filepath.Join(runtimeDirectory, "instances", "token")
	manager, err := cloudflare.New(cloudflare.Config{
		AdminConnectorTokenPath:     adminTokenPath,
		InstancesConnectorTokenPath: instancesTokenPath,
	}, environment.dataStore.ListInstances, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	remoteAccess, err := remoteaccess.New(manager, environment.dataStore.ListInstances)
	if err != nil {
		t.Fatal(err)
	}
	environment.server.config.RemoteAccess = remoteAccess

	path := "/api/v1/system/remote-access/configuration"
	response := environment.request(t, http.MethodGet, path, nil, "", nil)
	assertStatus(t, response, http.StatusUnauthorized)
	payload := map[string]string{
		"mode":               remoteaccess.ModeManagedCloudflare,
		"admin_tunnel_token": adminTunnelToken, "instances_tunnel_token": instancesTunnelToken,
		"admin_hostname": "admin.example.com",
	}
	response = environment.request(t, http.MethodPut, path, payload, environment.adminToken, nil)
	assertStatus(t, response, http.StatusOK)

	record, err := environment.dataStore.GetRemoteAccessConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{adminTunnelToken, instancesTunnelToken} {
		if strings.Contains(record.Ciphertext, secret) {
			t.Fatal("stored Cloudflare configuration contains a plaintext tunnel token")
		}
	}
	for path, expected := range map[string]string{adminTokenPath: adminTunnelToken, instancesTokenPath: instancesTunnelToken} {
		data, readErr := os.ReadFile(path)
		if readErr != nil || strings.TrimSpace(string(data)) != expected {
			t.Fatalf("connector token file %q was not written correctly: %v", path, readErr)
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("connector token file %q stat: %v", path, statErr)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("connector token file %q mode=%v", path, info.Mode().Perm())
		}
	}

	response = environment.request(t, http.MethodGet, path, nil, environment.adminToken, nil)
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("configuration status=%d cache=%q", response.StatusCode, response.Header.Get("Cache-Control"))
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{adminTunnelToken, instancesTunnelToken} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("configuration response exposed a tunnel token: %s", body)
		}
	}
	var view remoteaccess.ConfigurationView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if view.AdminHostname != "admin.example.com" || !view.AdminTunnelTokenConfigured || !view.InstancesTunnelTokenConfigured || view.LegacyProviderManaged {
		t.Fatalf("configuration view=%+v", view)
	}
	adminDigest := sha256.Sum256([]byte(adminTunnelToken))
	instancesDigest := sha256.Sum256([]byte(instancesTunnelToken))
	if view.AdminTunnelTokenFingerprint != fmt.Sprintf("%X", adminDigest[:5]) ||
		view.InstancesTunnelTokenFingerprint != fmt.Sprintf("%X", instancesDigest[:5]) {
		t.Fatalf("configuration token fingerprints=%+v", view)
	}

	response = environment.request(t, http.MethodPut, path, map[string]string{
		"mode":           remoteaccess.ModeManagedCloudflare,
		"admin_hostname": "admin.example.com",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusOK)
	for path, expected := range map[string]string{adminTokenPath: adminTunnelToken, instancesTokenPath: instancesTunnelToken} {
		data, readErr := os.ReadFile(path)
		if readErr != nil || strings.TrimSpace(string(data)) != expected {
			t.Fatalf("blank update replaced connector token %q: %v", path, readErr)
		}
	}

	response = environment.request(t, http.MethodDelete, path, nil, environment.adminToken, nil)
	assertStatus(t, response, http.StatusOK)
	if _, err := environment.dataStore.GetRemoteAccessConfig(context.Background()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("remote access configuration remains after verified cleanup: %v", err)
	}
}

func TestCloudflareOAuthClientCanBeConfiguredAndReported(t *testing.T) {
	environment := newAPITestEnvironment(t)
	managed, err := cloudflare.New(cloudflare.Config{}, environment.dataStore.ListInstances, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	remoteAccess, err := remoteaccess.New(managed, environment.dataStore.ListInstances)
	if err != nil {
		t.Fatal(err)
	}
	oauth, err := cloudflareoauth.New(cloudflareoauth.Config{
		RedirectURL: "http://127.0.0.1:9180/api/v1/system/remote-access/cloudflare/oauth/callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	environment.server.config.RemoteAccess = remoteAccess
	environment.server.config.CloudflareOAuth = oauth

	path := "/api/v1/system/remote-access/cloudflare/oauth/client"
	response := environment.request(t, http.MethodPut, path, map[string]string{"client_id": "invalid id"}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)
	response.Body.Close()

	response = environment.request(t, http.MethodPut, path, map[string]string{"client_id": "cloudflare-oauth-client-id"}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusOK)
	response.Body.Close()
	record, err := environment.dataStore.GetCloudflareOAuthClient(context.Background())
	if err != nil || record.ClientID != "cloudflare-oauth-client-id" {
		t.Fatalf("stored OAuth client=%+v err=%v", record, err)
	}

	response = environment.request(t, http.MethodGet, "/api/v1/system/remote-access/configuration", nil, environment.adminToken, nil)
	assertStatus(t, response, http.StatusOK)
	var configuration remoteaccess.ConfigurationView
	decodeResponse(t, response, &configuration)
	response.Body.Close()
	if !configuration.CloudflareOAuthAvailable || !configuration.CloudflareOAuthSetup.ClientConfigured || configuration.CloudflareOAuthSetup.ClientID != record.ClientID {
		t.Fatalf("OAuth configuration=%+v", configuration.CloudflareOAuthSetup)
	}
	if configuration.CloudflareOAuthSetup.RedirectURL == "" || len(configuration.CloudflareOAuthSetup.Scopes) != len(cloudflareoauth.DefaultScopes()) {
		t.Fatalf("OAuth setup metadata=%+v", configuration.CloudflareOAuthSetup)
	}
}

func TestCloudflareTunnelBoundariesSaveIndependently(t *testing.T) {
	const (
		adminTunnelToken     = "eyJ-admin-boundary-token-that-is-long-enough"
		instancesTunnelToken = "eyJ-instances-boundary-token-that-is-long-enough"
	)
	environment := newAPITestEnvironment(t)
	runtimeDirectory := t.TempDir()
	adminTokenPath := filepath.Join(runtimeDirectory, "admin", "token")
	instancesTokenPath := filepath.Join(runtimeDirectory, "instances", "token")
	managed, err := cloudflare.New(cloudflare.Config{
		AdminConnectorTokenPath:     adminTokenPath,
		InstancesConnectorTokenPath: instancesTokenPath,
	}, environment.dataStore.ListInstances, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	remoteAccess, err := remoteaccess.New(managed, environment.dataStore.ListInstances)
	if err != nil {
		t.Fatal(err)
	}
	environment.server.config.RemoteAccess = remoteAccess

	adminPath := "/api/v1/system/remote-access/cloudflare/admin"
	response := environment.request(t, http.MethodPut, adminPath, map[string]string{
		"tunnel_token": adminTunnelToken,
		"hostname":     "admin.example.com",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusOK)
	if data, readErr := os.ReadFile(adminTokenPath); readErr != nil || strings.TrimSpace(string(data)) != adminTunnelToken {
		t.Fatalf("admin token=%q err=%v", data, readErr)
	}
	if _, statErr := os.Stat(instancesTokenPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("instance token unexpectedly exists: %v", statErr)
	}

	configurationPath := "/api/v1/system/remote-access/configuration"
	response = environment.request(t, http.MethodGet, configurationPath, nil, environment.adminToken, nil)
	var partial remoteaccess.ConfigurationView
	decodeResponse(t, response, &partial)
	response.Body.Close()
	if !partial.AdminTunnelTokenConfigured || partial.InstancesTunnelTokenConfigured || partial.AdminOriginService != remoteaccess.AdminOriginService {
		t.Fatalf("partial configuration=%+v", partial)
	}
	adminDigest := sha256.Sum256([]byte(adminTunnelToken))
	if partial.AdminTunnelTokenFingerprint != fmt.Sprintf("%X", adminDigest[:5]) || partial.InstancesTunnelTokenFingerprint != "" {
		t.Fatalf("partial configuration fingerprints=%+v", partial)
	}

	removedInstancesPath := "/api/v1/system/remote-access/cloudflare/instances"
	response = environment.request(t, http.MethodPut, removedInstancesPath, map[string]string{"tunnel_token": instancesTunnelToken}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusMethodNotAllowed)
	publishingPath := "/api/v1/system/remote-access/cloudflare/instance-publishing"
	response = environment.request(t, http.MethodPut, publishingPath, map[string]string{
		"tunnel_token": "invalid-connector-token", "account_id": "account", "zone_id": "zone", "api_token": "api-token",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)
	response.Body.Close()
	response = environment.request(t, http.MethodPut, publishingPath, map[string]string{
		"tunnel_token": "invalid-connector-token", "account_id": "account", "zone_id": "zone", "api_token": "api-token", "fleet_namespace": "andes",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)
	var publishingOperation domain.Operation
	decodeResponse(t, response, &publishingOperation)
	response.Body.Close()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		publishingOperation, err = environment.dataStore.GetOperation(context.Background(), publishingOperation.ID)
		if err == nil && publishingOperation.Status == domain.OperationFailed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if publishingOperation.Status != domain.OperationFailed || publishingOperation.Progress == nil ||
		publishingOperation.Progress.Stage != "VALIDATING_TUNNEL_TOKEN" || publishingOperation.Progress.ActionCode != "replace_tunnel_token" {
		t.Fatalf("failed publishing operation=%+v", publishingOperation)
	}
	if _, statErr := os.Stat(instancesTokenPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed publishing wrote an instance token: %v", statErr)
	}

	response = environment.request(t, http.MethodPut, adminPath, map[string]string{
		"hostname": "new-admin.example.com",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusOK)
	data, readErr := os.ReadFile(adminTokenPath)
	if readErr != nil || strings.TrimSpace(string(data)) != adminTunnelToken {
		t.Fatalf("blank update replaced admin token: %q err=%v", data, readErr)
	}

	response = environment.request(t, http.MethodGet, configurationPath, nil, environment.adminToken, nil)
	var complete remoteaccess.ConfigurationView
	decodeResponse(t, response, &complete)
	response.Body.Close()
	if !complete.AdminTunnelTokenConfigured || complete.InstancesTunnelTokenConfigured || complete.AdminHostname != "new-admin.example.com" {
		t.Fatalf("complete configuration=%+v", complete)
	}
	if complete.AdminTunnelTokenFingerprint != fmt.Sprintf("%X", adminDigest[:5]) || complete.InstancesTunnelTokenFingerprint != "" {
		t.Fatalf("complete configuration fingerprints=%+v", complete)
	}
}

func TestClassifyPublicationFailureReportsTheActionableStage(t *testing.T) {
	stage, detail, action := classifyPublicationFailure(errors.New("update Cloudflare instance routes: Cloudflare API token cannot edit tunnel configuration: HTTP 403"))
	if stage != "UPDATING_INGRESS" || detail != "Cloudflare API token cannot edit tunnel configuration." || action != "replace_api_token" {
		t.Fatalf("classification stage=%q detail=%q action=%q", stage, detail, action)
	}
	stage, _, action = classifyPublicationFailure(errors.New("reconcile Cloudflare instance DNS: provider timeout"))
	if stage != "CREATING_DNS" || action != "retry" {
		t.Fatalf("DNS classification stage=%q action=%q", stage, action)
	}
	stage, detail, action = classifyPublicationFailure(errors.New("Cloudflare rejected tunnel configuration: Cloudflare API HTTP 400: validation failed"))
	if stage != "UPDATING_INGRESS" || !strings.Contains(detail, "HTTP 400") || action != "retry" {
		t.Fatalf("configuration validation classification stage=%q detail=%q action=%q", stage, detail, action)
	}
}

func TestFleetNamespaceChangeLocksAfterPublication(t *testing.T) {
	instances := []domain.Instance{{Name: "test01", PublicHostname: "andes-test01.example.com", Status: domain.InstanceRunning}}
	if err := validateFleetNamespaceChange("andes", "himalaya", instances); err == nil {
		t.Fatal("expected published hostname to lock the Fleet namespace")
	}
	if err := validateFleetNamespaceChange("", "andes", instances); err != nil {
		t.Fatalf("initial namespace migration must preserve legacy publications: %v", err)
	}
	instances[0].Status = domain.InstanceDeleted
	if err := validateFleetNamespaceChange("andes", "himalaya", instances); err != nil {
		t.Fatalf("deleted instance must not lock the Fleet namespace: %v", err)
	}
}

func TestPublicationOperationStepsSeparateCloudflareConfigurationFromEndpointFailure(t *testing.T) {
	steps := publicationOperationSteps(&remoteaccess.PublishedRoute{
		DNSState: cloudflare.ResourceReady, RouteState: cloudflare.ResourceReady,
		EndpointState: cloudflare.EndpointUnavailable, ProviderDetail: "One or more publication checks failed",
		EndpointDetail: "Public endpoint returned HTTP 502",
	})
	if len(steps) != 5 {
		t.Fatalf("steps=%+v", steps)
	}
	if steps[3].Status != "succeeded" || steps[3].Detail != "Cloudflare DNS and tunnel ingress match Fleet-owned configuration" {
		t.Fatalf("Cloudflare verification step=%+v", steps[3])
	}
	if steps[4].Status != "failed" || steps[4].Detail != "Public endpoint returned HTTP 502" {
		t.Fatalf("endpoint verification step=%+v", steps[4])
	}
	if status := mapEndpointStepStatus(cloudflare.EndpointPropagating); status != "running" {
		t.Fatalf("propagating endpoint status=%q", status)
	}
}

func TestStalePublicationOutcomeUsesCurrentCloudflareState(t *testing.T) {
	publish := domain.Operation{
		ID: "publish-01", InstanceID: "instance-01",
		Metadata: json.RawMessage(`{"public_hostname":"aksa.example.com"}`),
	}
	route := &remoteaccess.PublishedRoute{
		InstanceID: "instance-01", Hostname: "aksa.example.com",
		DNSState: cloudflare.ResourceReady, RouteState: cloudflare.ResourceReady,
		EndpointState: cloudflare.EndpointReachable,
	}
	status, progress, operationErr, decided := stalePublicationOutcome(
		publish, remoteaccess.Status{Configured: true, State: "synced"}, route,
	)
	if !decided || status != domain.OperationSucceeded || operationErr != "" || progress.Stage != "CHECKING_PUBLIC_ENDPOINT" {
		t.Fatalf("verified outcome status=%q progress=%+v error=%q decided=%t", status, progress, operationErr, decided)
	}

	unpublish := publish
	unpublish.Metadata = json.RawMessage(`{"public_hostname":""}`)
	status, progress, operationErr, decided = stalePublicationOutcome(
		unpublish, remoteaccess.Status{Configured: true, State: "synced"},
		&remoteaccess.PublishedRoute{InstanceID: "instance-01"},
	)
	if !decided || status != domain.OperationSucceeded || operationErr != "" || progress.Stage != "CHECKING_PUBLIC_ENDPOINT" {
		t.Fatalf("unpublish outcome status=%q progress=%+v error=%q decided=%t", status, progress, operationErr, decided)
	}
}

func TestStalePublicationOutcomeFailsAtActualUnverifiedStage(t *testing.T) {
	operation := domain.Operation{
		ID: "publish-02", InstanceID: "instance-02",
		Metadata: json.RawMessage(`{"public_hostname":"test02.example.com"}`),
	}
	route := &remoteaccess.PublishedRoute{
		InstanceID: "instance-02", Hostname: "test02.example.com",
		DNSState: cloudflare.ResourceReady, RouteState: cloudflare.ResourceReady,
		EndpointState: cloudflare.EndpointUnavailable, EndpointDetail: "Public endpoint returned HTTP 502",
	}
	status, progress, operationErr, decided := stalePublicationOutcome(
		operation, remoteaccess.Status{Configured: true, State: "synced"}, route,
	)
	if !decided || status != domain.OperationFailed || progress.Stage != "CHECKING_PUBLIC_ENDPOINT" ||
		progress.ActionCode != "retry" || operationErr != route.EndpointDetail {
		t.Fatalf("failed outcome status=%q progress=%+v error=%q decided=%t", status, progress, operationErr, decided)
	}
}

func TestStalePublicationOutcomeWaitsForRemoteAccessSynchronization(t *testing.T) {
	operation := domain.Operation{
		ID: "publish-03", InstanceID: "instance-03",
		Metadata: json.RawMessage(`{"public_hostname":"test03.example.com"}`),
	}
	status, progress, operationErr, decided := stalePublicationOutcome(
		operation, remoteaccess.Status{Configured: true, State: "syncing"}, nil,
	)
	if decided || status != "" || operationErr != "" || progress.Stage != "" {
		t.Fatalf("syncing outcome status=%q progress=%+v error=%q decided=%t", status, progress, operationErr, decided)
	}
}

func TestExistingPublicEndpointsAreEncryptedMappedAndProviderNeutral(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, _ := environment.enrollHost(t)
	response := environment.request(t, http.MethodPost, "/api/v1/instances", createInstanceRequest{
		Name: "external-endpoint-01", HostID: hostID, HermesVersion: "0.19.0",
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create instance status=%d body=%s", response.StatusCode, body)
	}
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()

	managed, err := cloudflare.New(cloudflare.Config{}, environment.dataStore.ListInstances, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	remoteAccess, err := remoteaccess.New(managed, environment.dataStore.ListInstances)
	if err != nil {
		t.Fatal(err)
	}
	environment.server.config.RemoteAccess = remoteAccess

	path := "/api/v1/system/remote-access/configuration"
	payload := remoteAccessConfigurationRequest{
		Mode:     remoteaccess.ModeExistingEndpoints,
		AdminURL: "https://ADMIN.example.com/",
		InstanceEndpoints: []remoteaccess.InstanceEndpoint{{
			InstanceID: instance.ID, InstanceName: instance.Name,
			DashboardURL: "https://INSTANCE.example.com/",
		}},
	}
	response = environment.request(t, http.MethodPut, path, payload, environment.adminToken, nil)
	assertStatus(t, response, http.StatusOK)

	record, err := environment.dataStore.GetRemoteAccessConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(record.Ciphertext, "admin.example.com") || strings.Contains(record.Ciphertext, "instance.example.com") {
		t.Fatalf("stored endpoint configuration is not encrypted: %s", record.Ciphertext)
	}

	response = environment.request(t, http.MethodGet, path, nil, environment.adminToken, nil)
	var view remoteaccess.ConfigurationView
	decodeResponse(t, response, &view)
	response.Body.Close()
	if view.Mode != remoteaccess.ModeExistingEndpoints || view.AdminURL != "https://admin.example.com" ||
		len(view.InstanceEndpoints) != 1 || view.InstanceEndpoints[0].DashboardURL != "https://instance.example.com" {
		t.Fatalf("configuration view=%+v", view)
	}

	response = environment.request(t, http.MethodGet, "/api/v1/instances", nil, environment.adminToken, nil)
	var instances []domain.Instance
	decodeResponse(t, response, &instances)
	response.Body.Close()
	if len(instances) != 1 || instances[0].PublicDashboardURL != "https://instance.example.com" {
		t.Fatalf("instances=%+v", instances)
	}

	response = environment.request(t, http.MethodPut, path, remoteAccessConfigurationRequest{
		Mode: remoteaccess.ModeExistingEndpoints,
		InstanceEndpoints: []remoteaccess.InstanceEndpoint{
			{InstanceID: instance.ID, DashboardURL: "https://first.example.com"},
			{InstanceID: instance.ID, DashboardURL: "https://second.example.com"},
		},
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)

	response = environment.request(t, http.MethodPut, path, remoteAccessConfigurationRequest{
		Mode: remoteaccess.ModeManagedCloudflare,
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusBadGateway {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("mode switch status=%d body=%s", response.StatusCode, body)
	}
	response.Body.Close()

	response = environment.request(t, http.MethodDelete, path, nil, environment.adminToken, nil)
	assertStatus(t, response, http.StatusOK)
	if _, err := environment.dataStore.GetRemoteAccessConfig(context.Background()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("registered endpoint configuration remains after disable: %v", err)
	}
}

func TestHostCredentialRotationRequiresAdminAndInvalidatesOldToken(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, oldToken := environment.enrollHost(t)
	path := "/api/v1/hosts/" + hostID + "/credentials/rotate"
	payload := map[string]string{
		"confirm_name":  "local-test",
		"hostname":      "host",
		"os":            "darwin",
		"arch":          "arm64",
		"agent_version": agentVersion,
	}

	response := environment.request(t, http.MethodPost, path, payload, "", nil)
	assertStatus(t, response, http.StatusUnauthorized)
	response = environment.request(t, http.MethodPost, path, payload, environment.enrollmentToken, nil)
	assertStatus(t, response, http.StatusUnauthorized)

	incompatible := maps.Clone(payload)
	incompatible["agent_version"] = "0.0.0"
	response = environment.request(t, http.MethodPost, path, incompatible, environment.adminToken, nil)
	assertStatus(t, response, http.StatusConflict)

	mismatch := maps.Clone(payload)
	mismatch["hostname"] = "different-host"
	response = environment.request(t, http.MethodPost, path, mismatch, environment.adminToken, nil)
	assertStatus(t, response, http.StatusConflict)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/heartbeat", map[string]string{
		"hostname": "host", "os": "darwin", "arch": "arm64", "agent_version": agentVersion,
	}, oldToken, map[string]string{"X-Fleet-Host-ID": hostID})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodPost, path, payload, environment.adminToken, nil)
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		defer response.Body.Close()
		t.Fatalf("rotate credential status=%d cache=%q", response.StatusCode, response.Header.Get("Cache-Control"))
	}
	var rotated struct {
		HostID    string `json:"host_id"`
		HostToken string `json:"host_token"`
	}
	decodeResponse(t, response, &rotated)
	response.Body.Close()
	if rotated.HostID != hostID || rotated.HostToken == "" || rotated.HostToken == oldToken {
		t.Fatalf("rotated credential=%+v", rotated)
	}
	storedHash, err := environment.dataStore.HostTokenHash(context.Background(), hostID)
	if err != nil {
		t.Fatal(err)
	}
	if storedHash != security.HashToken(rotated.HostToken) || storedHash == rotated.HostToken {
		t.Fatal("Fleet did not store only the new host credential hash")
	}

	response = environment.request(t, http.MethodPost, "/api/v1/agent/heartbeat", map[string]string{
		"hostname": "host", "os": "darwin", "arch": "arm64", "agent_version": agentVersion,
	}, oldToken, map[string]string{"X-Fleet-Host-ID": hostID})
	assertStatus(t, response, http.StatusUnauthorized)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/heartbeat", map[string]string{
		"hostname": "host", "os": "darwin", "arch": "arm64", "agent_version": agentVersion,
	}, rotated.HostToken, map[string]string{"X-Fleet-Host-ID": hostID})
	assertStatus(t, response, http.StatusNoContent)
}

func TestGetOperationReturnsExactNoStoreResource(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, _ := environment.enrollHost(t)
	response := environment.request(t, http.MethodPost, "/api/v1/instances", createInstanceRequest{
		Name: "fleet-operation-get", HostID: hostID, HermesVersion: "0.19.0",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)

	response = environment.request(t, http.MethodGet, "/api/v1/operations", nil, environment.adminToken, nil)
	var operations []domain.Operation
	decodeResponse(t, response, &operations)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" || len(operations) == 0 {
		t.Fatalf("legacy operations status=%d cache=%q operations=%+v", response.StatusCode, response.Header.Get("Cache-Control"), operations)
	}

	response = environment.request(t, http.MethodGet, "/api/v1/operations/"+operations[0].ID, nil, "", nil)
	assertStatus(t, response, http.StatusUnauthorized)
	response = environment.request(t, http.MethodGet, "/api/v1/operations/"+operations[0].ID, nil, environment.adminToken, nil)
	var operation domain.Operation
	decodeResponse(t, response, &operation)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" ||
		operation.ID != operations[0].ID {
		t.Fatalf("operation lookup status=%d cache=%q operation=%+v", response.StatusCode, response.Header.Get("Cache-Control"), operation)
	}
	response = environment.request(
		t, http.MethodGet, "/api/v1/operations/00000000-0000-4000-8000-000000000099", nil, environment.adminToken, nil,
	)
	assertStatus(t, response, http.StatusNotFound)
}

func TestListOperationsCursorPaginationIsStableAndNoStore(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, _ := environment.enrollHost(t)
	createdAt := time.Date(2026, 7, 27, 3, 0, 0, 123, time.UTC)
	instanceIDs := []string{
		"10000000-0000-4000-8000-000000000001",
		"10000000-0000-4000-8000-000000000002",
		"10000000-0000-4000-8000-000000000003",
	}
	operationIDs := []string{
		"20000000-0000-4000-8000-000000000001",
		"20000000-0000-4000-8000-000000000002",
		"20000000-0000-4000-8000-000000000003",
	}
	jobIDs := []string{
		"30000000-0000-4000-8000-000000000001",
		"30000000-0000-4000-8000-000000000002",
		"30000000-0000-4000-8000-000000000003",
	}
	for index := range instanceIDs {
		instance := domain.Instance{
			ID: instanceIDs[index], Name: "page-instance-0" + strconv.Itoa(index+1), HostID: hostID,
			Status: domain.InstanceProvisioning, Image: "local/hermes-fleet-runtime:0.19.0",
			Provider: "openai-codex", APIPort: 8700 + index, DashboardPort: 9200 + index,
			CreatedAt: createdAt, UpdatedAt: createdAt,
		}
		operation := domain.Operation{
			ID: operationIDs[index], InstanceID: instance.ID, Actor: "FLEET_ADMIN", Type: "PROVISION",
			Status: domain.OperationPending, Summary: "Provision " + instance.Name,
			CreatedAt: createdAt, UpdatedAt: createdAt,
		}
		job := domain.Job{
			ID: jobIDs[index], OperationID: operation.ID, HostID: hostID, InstanceID: instance.ID,
			Type: "instance.provision", Status: domain.JobPending, Payload: json.RawMessage(`{}`),
			CreatedAt: createdAt, UpdatedAt: createdAt,
		}
		if err := environment.dataStore.CreateInstance(context.Background(), instance, operation, job); err != nil {
			t.Fatal(err)
		}
	}

	response := environment.request(t, http.MethodGet, "/api/v1/operations?limit=2", nil, environment.adminToken, nil)
	var firstPage operationPageResponse
	decodeResponse(t, response, &firstPage)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("first page status=%d cache=%q", response.StatusCode, response.Header.Get("Cache-Control"))
	}
	if len(firstPage.Items) != 2 || firstPage.Items[0].ID != operationIDs[2] || firstPage.Items[1].ID != operationIDs[1] || firstPage.NextCursor == "" {
		t.Fatalf("first page=%+v", firstPage)
	}

	response = environment.request(
		t, http.MethodGet, "/api/v1/operations?limit=2&cursor="+firstPage.NextCursor, nil, environment.adminToken, nil,
	)
	var secondPage operationPageResponse
	decodeResponse(t, response, &secondPage)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("second page status=%d cache=%q", response.StatusCode, response.Header.Get("Cache-Control"))
	}
	if len(secondPage.Items) != 1 || secondPage.Items[0].ID != operationIDs[0] || secondPage.NextCursor != "" {
		t.Fatalf("second page=%+v", secondPage)
	}
}

func TestPolicyLifecycleReturnsExplicitScopeAndPreview(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, _ := environment.enrollHost(t)
	now := time.Now().UTC()
	instance := domain.Instance{
		ID: "policy-instance-01", Name: "policy-target", HostID: hostID,
		Status: domain.InstanceProvisioning, Image: "local/hermes-fleet-runtime:0.19.0",
		Provider: "openai-codex", APIPort: 8781, DashboardPort: 9281,
		CreatedAt: now, UpdatedAt: now,
	}
	operation := domain.Operation{
		ID: "policy-provision-operation", InstanceID: instance.ID, Type: "PROVISION",
		Status: domain.OperationPending, Summary: "Provision policy target", CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: "policy-provision-job", OperationID: operation.ID, HostID: hostID, InstanceID: instance.ID,
		Type: "instance.provision", Status: domain.JobPending, Payload: json.RawMessage(`{}`),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := environment.dataStore.CreateInstance(context.Background(), instance, operation, job); err != nil {
		t.Fatal(err)
	}
	response := environment.request(t, http.MethodPost, "/api/v1/policies", map[string]any{
		"name": "Stable Hermes", "description": "Keep production current", "status": "ENABLED",
		"desired_hermes": "LATEST_STABLE", "strategy": "ONE_AT_A_TIME",
		"scope_instance_ids": []string{instance.ID},
	}, environment.adminToken, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("create policy status=%d body=%s", response.StatusCode, body)
	}
	var policy domain.FleetPolicy
	decodeResponse(t, response, &policy)
	if policy.Name != "Stable Hermes" || len(policy.ScopeInstanceIDs) != 1 || policy.ScopeInstanceIDs[0] != instance.ID {
		t.Fatalf("created policy=%+v", policy)
	}
	response = environment.request(t, http.MethodGet, "/api/v1/policies", nil, environment.adminToken, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("list policy status=%d cache=%q", response.StatusCode, response.Header.Get("Cache-Control"))
	}
	var policies []policyListItem
	decodeResponse(t, response, &policies)
	if len(policies) != 1 || policies[0].Compliance.Total != 1 || policies[0].Compliance.Blocked != 1 {
		t.Fatalf("policies=%+v", policies)
	}
	response = environment.request(t, http.MethodGet, "/api/v1/policies/"+policy.ID+"/preview", nil, environment.adminToken, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("preview policy status=%d", response.StatusCode)
	}
	var preview domain.PolicyPreview
	decodeResponse(t, response, &preview)
	if preview.Summary.Total != 1 || preview.Targets[0].State != policyPreviewBlocked {
		t.Fatalf("preview=%+v", preview)
	}
}

func TestPolicyRolloutQueuesHermesUpdateWithSharedWorkflow(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	response := environment.request(t, http.MethodPost, "/api/v1/instances", createInstanceRequest{
		Name: "policy-rollout-target", HostID: hostID, HermesVersion: "0.18.1",
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create instance status=%d body=%s", response.StatusCode, body)
	}
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()
	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	projectName, dataVolume, managedDirectory := domain.ManagedIdentity(instance.ID, instance.Name)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ProjectName: projectName, DataVolume: dataVolume,
		ManagedPath: "/managed/" + managedDirectory, ImageID: "sha256:" + strings.Repeat("a", 64),
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodPost, "/api/v1/policies", map[string]any{
		"name": "Production stable", "status": "ENABLED", "desired_hermes": "LATEST_STABLE",
		"strategy": "ONE_AT_A_TIME", "scope_instance_ids": []string{instance.ID},
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create policy status=%d body=%s", response.StatusCode, body)
	}
	var policy domain.FleetPolicy
	decodeResponse(t, response, &policy)
	response.Body.Close()
	response = environment.request(t, http.MethodPost, "/api/v1/policies/"+policy.ID+"/rollouts", map[string]any{}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("start rollout status=%d body=%s", response.StatusCode, body)
	}
	var rollout domain.Operation
	decodeResponse(t, response, &rollout)
	response.Body.Close()
	if rollout.Type != "ROLLOUT_POLICY" || rollout.Status != domain.OperationRunning || rollout.WorkflowID != rollout.ID {
		t.Fatalf("rollout=%+v", rollout)
	}
	job = environment.claimAndAcknowledge(t, hostID, hostToken)
	if job.Type != "instance.hermes.update" {
		t.Fatalf("rollout job type=%q", job.Type)
	}
	child, err := environment.dataStore.GetOperation(context.Background(), job.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if child.WorkflowID != rollout.ID || child.InstanceID != instance.ID || child.Actor != "POLICY_CONTROLLER" {
		t.Fatalf("child operation=%+v", child)
	}
}

func TestListOperationsRejectsInvalidPagination(t *testing.T) {
	environment := newAPITestEnvironment(t)
	invalidIDCursor := encodeOperationCursor(store.OperationCursor{
		CreatedAt: time.Now().UTC(),
		ID:        "operation-not-a-uuid",
	})
	for _, path := range []string{
		"/api/v1/operations?limit=0",
		"/api/v1/operations?limit=101",
		"/api/v1/operations?limit=not-a-number",
		"/api/v1/operations?cursor=%25%25%25",
		"/api/v1/operations?cursor=" + invalidIDCursor,
		"/api/v1/operations?unknown=value",
	} {
		t.Run(path, func(t *testing.T) {
			response := environment.request(t, http.MethodGet, path, nil, environment.adminToken, nil)
			if response.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control=%q want no-store", response.Header.Get("Cache-Control"))
			}
			assertStatus(t, response, http.StatusBadRequest)
		})
	}
}

func TestBackupLifecycleRequiresAdminAndExplicitDeleteConfirmation(t *testing.T) {
	environment := newAPITestEnvironment(t)
	response := environment.request(t, http.MethodGet, "/api/v1/backups", nil, "", nil)
	assertStatus(t, response, http.StatusUnauthorized)

	response = environment.request(t, http.MethodPost, "/api/v1/backups", nil, environment.adminToken, nil)
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create backup status=%d body=%s", response.StatusCode, body)
	}
	var created backup.Metadata
	decodeResponse(t, response, &created)
	response.Body.Close()

	response = environment.request(t, http.MethodGet, "/api/v1/backups", nil, environment.adminToken, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list backups status=%d", response.StatusCode)
	}
	var items []backup.Metadata
	decodeResponse(t, response, &items)
	response.Body.Close()
	if len(items) != 1 || items[0].ID != created.ID {
		t.Fatalf("backup list = %+v", items)
	}

	response = environment.request(t, http.MethodPost, "/api/v1/backups/"+created.ID+"/verify", nil, environment.adminToken, nil)
	assertStatus(t, response, http.StatusOK)
	response = environment.request(t, http.MethodGet, "/api/v1/backups/"+created.ID+"/download", nil, environment.adminToken, nil)
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Cache-Control"), "no-store") || !strings.Contains(response.Header.Get("Content-Disposition"), created.Filename) {
		t.Fatalf("download status=%d cache=%q disposition=%q", response.StatusCode, response.Header.Get("Cache-Control"), response.Header.Get("Content-Disposition"))
	}
	header := make([]byte, 15)
	if _, err := io.ReadFull(response.Body, header); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if string(header) != "SQLite format 3" {
		t.Fatalf("download header = %q", header)
	}

	response = environment.request(t, http.MethodDelete, "/api/v1/backups/"+created.ID, map[string]string{"confirm_filename": "wrong"}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)
	response = environment.request(t, http.MethodDelete, "/api/v1/backups/"+created.ID, map[string]string{"confirm_filename": created.Filename}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusNoContent)
}

func TestInstanceRecoveryPointLifecycleIsStoppedLeaseFencedAndEncrypted(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	workflowID := "00000000-0000-4000-8000-000000000011"
	create := createInstanceRequest{
		Name: "fleet-recovery-01", HostID: hostID, Image: "local/hermes-fleet-runtime:0.18.2",
		Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
		APIPort: 48650, DashboardPort: 49130,
	}
	response := environment.request(t, http.MethodPost, "/api/v1/instances", create, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("create instance status=%d", response.StatusCode)
	}
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()
	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	shortID := strings.ReplaceAll(instance.ID, "-", "")[:8]
	projectName := "hermes-fleet-" + instance.Name + "-" + shortID
	imageID := "sha256:" + strings.Repeat("a", 64)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ProjectName: projectName, DataVolume: projectName + "-data",
		ManagedPath: "/managed/" + instance.Name + "-" + shortID, ImageID: imageID,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/recovery-points", map[string]any{}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusConflict)
	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/actions", map[string]string{"action": "stop", "workflow_id": "invalid"}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)
	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/actions", map[string]string{"action": "stop", "workflow_id": workflowID}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)
	job = environment.claimAndAcknowledge(t, hostID, hostToken)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{Success: true}, hostToken, map[string]string{
		"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken,
	})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/recovery-points", map[string]any{"workflow_id": workflowID}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create recovery point status=%d body=%s", response.StatusCode, body)
	}
	var point recovery.Metadata
	decodeResponse(t, response, &point)
	response.Body.Close()
	if environment.overviewInstance(t, instance.ID).Status != domain.InstanceBackingUp {
		t.Fatal("instance was not fenced in BACKING_UP while recovery job was pending")
	}
	job = environment.claimAndAcknowledge(t, hostID, hostToken)
	if job.Type != "instance.recovery.create" {
		t.Fatalf("claimed job type=%q", job.Type)
	}
	archive := recoveryArchiveForAPI(t, point)
	digest := sha256.Sum256(archive)
	encodedDigest := hex.EncodeToString(digest[:])
	response = environment.rawRequest(t, http.MethodPut, "/api/v1/agent/jobs/"+job.ID+"/recovery-point", bytes.NewReader(archive), hostToken, map[string]string{
		"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken,
		"X-Fleet-Recovery-Point-ID": point.ID, "X-Fleet-Recovery-SHA256": encodedDigest,
		"Content-Type": "application/x-tar",
	})
	assertStatus(t, response, http.StatusNoContent)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, RecoveryPointID: point.ID, RecoverySHA256: encodedDigest, RecoverySizeBytes: int64(len(archive)),
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)
	if environment.overviewInstance(t, instance.ID).Status != domain.InstanceStopped {
		t.Fatal("instance did not return to STOPPED after recovery completion")
	}

	response = environment.request(t, http.MethodGet, "/api/v1/instances/"+instance.ID+"/recovery-points", nil, environment.adminToken, nil)
	var points []recovery.Metadata
	decodeResponse(t, response, &points)
	response.Body.Close()
	if len(points) != 1 || points[0].Status != recovery.StatusReady || points[0].SHA256 != encodedDigest {
		t.Fatalf("recovery points=%+v", points)
	}
	response = environment.request(t, http.MethodGet, "/api/v1/instances/"+instance.ID+"/hermes-update", nil, environment.adminToken, nil)
	var updateStatus hermesUpdateResponse
	decodeResponse(t, response, &updateStatus)
	response.Body.Close()
	if !updateStatus.Available || !updateStatus.Eligible || updateStatus.TargetVersion != "0.19.0" {
		t.Fatalf("Hermes update status=%+v", updateStatus)
	}
	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/hermes-update", map[string]string{
		"confirm_name": "wrong-instance",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)
	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/hermes-update", map[string]string{
		"confirm_name": instance.Name, "workflow_id": workflowID,
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("start Hermes update status=%d body=%s", response.StatusCode, body)
	}
	var updateOperation domain.Operation
	decodeResponse(t, response, &updateOperation)
	response.Body.Close()
	if updateOperation.WorkflowID != workflowID || updateOperation.Actor != "FLEET_ADMIN" || !bytes.Contains(updateOperation.Metadata, []byte(`"to_version":"0.19.0"`)) {
		t.Fatalf("Hermes update operation context=%+v", updateOperation)
	}
	updateJob := environment.claimAndAcknowledge(t, hostID, hostToken)
	if updateJob.Type != "instance.hermes.update" {
		t.Fatalf("claimed Hermes update job type=%q", updateJob.Type)
	}
	var updatePayload domain.HermesUpdatePayload
	if err := json.Unmarshal(updateJob.Payload, &updatePayload); err != nil {
		t.Fatal(err)
	}
	upgradePayload := updatePayload.Upgrade
	if upgradePayload.TargetImage != "local/hermes-fleet-runtime:0.19.0-8bcdef6ef2bc" || upgradePayload.TargetVersion != "0.19.0" ||
		updatePayload.Backup.RecoveryPointID == point.ID || upgradePayload.Rollback.RecoveryPointID != updatePayload.Backup.RecoveryPointID ||
		upgradePayload.Rollback.ImageID != imageID || !upgradePayload.Rollback.RequireImageID || updatePayload.OriginalStatus != domain.InstanceStopped {
		t.Fatalf("Hermes update payload=%+v", updatePayload)
	}
	autoPoint, err := environment.recoveryPoints.Get(updatePayload.Backup.RecoveryPointID)
	if err != nil {
		t.Fatal(err)
	}
	updateArchive := recoveryArchiveForAPI(t, autoPoint)
	updateDigest := sha256.Sum256(updateArchive)
	encodedUpdateDigest := hex.EncodeToString(updateDigest[:])
	response = environment.rawRequest(t, http.MethodPut, "/api/v1/agent/jobs/"+updateJob.ID+"/recovery-point", bytes.NewReader(updateArchive), hostToken, map[string]string{
		"X-Fleet-Host-ID": hostID, leaseTokenHeader: updateJob.LeaseToken,
		"X-Fleet-Recovery-Point-ID": autoPoint.ID, "X-Fleet-Recovery-SHA256": encodedUpdateDigest,
		"Content-Type": "application/x-tar",
	})
	assertStatus(t, response, http.StatusNoContent)
	response = environment.rawRequest(t, http.MethodGet, "/api/v1/agent/jobs/"+updateJob.ID+"/recovery-point", nil, hostToken, map[string]string{
		"X-Fleet-Host-ID": hostID, leaseTokenHeader: updateJob.LeaseToken,
	})
	reusedArchive, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || !bytes.Equal(reusedArchive, updateArchive) ||
		response.Header.Get("X-Fleet-Recovery-SHA256") != encodedUpdateDigest {
		t.Fatalf("reuse automatic update backup status=%d err=%v size=%d", response.StatusCode, readErr, len(reusedArchive))
	}
	autoPoint, err = environment.recoveryPoints.Get(updatePayload.Backup.RecoveryPointID)
	if err != nil || autoPoint.Status != recovery.StatusReady {
		t.Fatalf("automatic update backup was not verified for retry: point=%+v err=%v", autoPoint, err)
	}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+updateJob.ID+"/recovery-point/verify", map[string]any{
		"recovery_point_id": autoPoint.ID, "sha256": encodedUpdateDigest, "size_bytes": len(updateArchive),
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: updateJob.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)
	targetImageID := "sha256:" + strings.Repeat("b", 64)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+updateJob.ID+"/complete", domain.JobResult{
		Success: true, ImageID: targetImageID, InstanceStatus: domain.InstanceStopped,
		RecoveryPointID: autoPoint.ID, RecoverySHA256: encodedUpdateDigest, RecoverySizeBytes: int64(len(updateArchive)),
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: updateJob.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)
	updated := environment.overviewInstance(t, instance.ID)
	if updated.Status != domain.InstanceStopped || updated.Image != upgradePayload.TargetImage || updated.ImageID != targetImageID {
		t.Fatalf("updated instance=%+v", updated)
	}
	response = environment.request(t, http.MethodGet, "/api/v1/recovery-points/"+point.ID+"/download", nil, environment.adminToken, nil)
	downloaded, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || !bytes.Equal(downloaded, archive) {
		t.Fatalf("download status=%d err=%v size=%d", response.StatusCode, err, len(downloaded))
	}

	response = environment.request(t, http.MethodPost, "/api/v1/recovery-points/"+point.ID+"/restore", map[string]string{
		"confirm_name": "wrong-instance",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)
	response = environment.request(t, http.MethodPost, "/api/v1/recovery-points/"+point.ID+"/restore", map[string]string{
		"confirm_name": instance.Name,
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("restore recovery point status=%d body=%s", response.StatusCode, body)
	}
	var restoreOperation domain.Operation
	decodeResponse(t, response, &restoreOperation)
	response.Body.Close()
	if restoreOperation.Type != "RESTORE" ||
		!bytes.Contains(restoreOperation.Metadata, []byte(`"backup_id":"`+point.ID+`"`)) ||
		environment.overviewInstance(t, instance.ID).Status != domain.InstanceRestoring {
		t.Fatalf("restore operation=%+v instance=%+v", restoreOperation, environment.overviewInstance(t, instance.ID))
	}
	response = environment.request(t, http.MethodDelete, "/api/v1/recovery-points/"+point.ID, map[string]string{
		"confirm_filename": point.Filename,
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusConflict)
	restoreJob := environment.claimAndAcknowledge(t, hostID, hostToken)
	if restoreJob.Type != "instance.recovery.restore" {
		t.Fatalf("claimed restore job type=%q", restoreJob.Type)
	}
	var restorePayload domain.RecoveryRestorePayload
	if err := json.Unmarshal(restoreJob.Payload, &restorePayload); err != nil {
		t.Fatal(err)
	}
	if restorePayload.RequireImageID {
		t.Fatal("manual recovery restore unexpectedly requires the original host image ID")
	}
	response = environment.request(t, http.MethodDelete, "/api/v1/recovery-points/"+point.ID, map[string]string{
		"confirm_filename": point.Filename,
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusConflict)
	response = environment.request(t, http.MethodGet, "/api/v1/agent/jobs/"+restoreJob.ID+"/recovery-point", nil, hostToken, map[string]string{
		"X-Fleet-Host-ID": hostID, leaseTokenHeader: "stale-lease",
	})
	assertStatus(t, response, http.StatusConflict)
	response = environment.request(t, http.MethodGet, "/api/v1/agent/jobs/"+restoreJob.ID+"/recovery-point", nil, hostToken, map[string]string{
		"X-Fleet-Host-ID": hostID, leaseTokenHeader: restoreJob.LeaseToken,
	})
	restoredArchive, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" || !bytes.Equal(restoredArchive, archive) {
		t.Fatalf("restore download status=%d cache=%q err=%v size=%d", response.StatusCode, response.Header.Get("Cache-Control"), err, len(restoredArchive))
	}
	restoredImageID := "sha256:" + strings.Repeat("c", 64)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+restoreJob.ID+"/complete", domain.JobResult{
		Success: true, ImageID: restoredImageID, InstanceStatus: domain.InstanceStopped,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: restoreJob.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)
	if restored := environment.overviewInstance(t, instance.ID); restored.Status != domain.InstanceStopped || restored.ImageID != restoredImageID {
		t.Fatalf("restored instance=%+v", restored)
	}
	response = environment.request(t, http.MethodDelete, "/api/v1/recovery-points/"+point.ID, map[string]string{"confirm_filename": point.Filename}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusNoContent)
}

func TestConcurrentRestoreAndDeleteCannotAcceptRestoreWithoutArtifact(t *testing.T) {
	environment := newAPITestEnvironment(t)
	instance, point := readyStoppedRecoveryPoint(t, environment, "fleet-recovery-race")

	releaseGate := environment.server.recoveryPointLocks.lock(point.ID)
	gateReleased := false
	defer func() {
		if !gateReleased {
			releaseGate()
		}
	}()
	restoreResult := make(chan *http.Response, 1)
	deleteResult := make(chan *http.Response, 1)
	go func() {
		restoreResult <- environment.concurrentAdminRequest(http.MethodPost, "/api/v1/recovery-points/"+point.ID+"/restore", map[string]string{
			"confirm_name": instance.Name,
		})
	}()
	waitForRecoveryPointLockReferences(t, &environment.server.recoveryPointLocks, point.ID, 2)
	go func() {
		deleteResult <- environment.concurrentAdminRequest(http.MethodDelete, "/api/v1/recovery-points/"+point.ID, map[string]string{
			"confirm_filename": point.Filename,
		})
	}()
	waitForRecoveryPointLockReferences(t, &environment.server.recoveryPointLocks, point.ID, 3)

	select {
	case response := <-restoreResult:
		response.Body.Close()
		t.Fatal("restore completed while the recovery point lock was held")
	default:
	}
	select {
	case response := <-deleteResult:
		response.Body.Close()
		t.Fatal("delete completed while the recovery point lock was held")
	default:
	}
	releaseGate()
	gateReleased = true

	restoreResponse := waitForHTTPResponse(t, restoreResult, "restore")
	deleteResponse := waitForHTTPResponse(t, deleteResult, "delete")
	defer restoreResponse.Body.Close()
	defer deleteResponse.Body.Close()
	restoreAccepted := restoreResponse.StatusCode == http.StatusAccepted
	deleteAccepted := deleteResponse.StatusCode == http.StatusNoContent
	if restoreAccepted == deleteAccepted {
		restoreBody, _ := io.ReadAll(restoreResponse.Body)
		deleteBody, _ := io.ReadAll(deleteResponse.Body)
		t.Fatalf("restore status=%d body=%s delete status=%d body=%s; exactly one operation must win",
			restoreResponse.StatusCode, restoreBody, deleteResponse.StatusCode, deleteBody)
	}
	if restoreAccepted {
		if deleteResponse.StatusCode != http.StatusConflict {
			t.Fatalf("delete status=%d want=%d after accepted restore", deleteResponse.StatusCode, http.StatusConflict)
		}
		if _, err := environment.recoveryPoints.Get(point.ID); err != nil {
			t.Fatalf("accepted restore lost its recovery artifact: %v", err)
		}
		return
	}
	if restoreResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("restore status=%d want=%d after accepted delete", restoreResponse.StatusCode, http.StatusNotFound)
	}
	if _, err := environment.recoveryPoints.Get(point.ID); !errors.Is(err, recovery.ErrNotFound) {
		t.Fatalf("deleted recovery point remains available: %v", err)
	}
}

func waitForHTTPResponse(t *testing.T, responses <-chan *http.Response, operation string) *http.Response {
	t.Helper()
	select {
	case response := <-responses:
		return response
	case <-time.After(5 * time.Second):
		t.Fatalf("%s request did not complete", operation)
		return nil
	}
}

func readyStoppedRecoveryPoint(t *testing.T, environment *apiTestEnvironment, name string) (domain.Instance, recovery.Metadata) {
	t.Helper()
	hostID, hostToken := environment.enrollHostNamed(t, name+"-host")
	response := environment.request(t, http.MethodPost, "/api/v1/instances", createInstanceRequest{
		Name: name, HostID: hostID, Image: "local/hermes-fleet-runtime:0.18.2",
		Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
		APIPort: 58650, DashboardPort: 59130,
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create instance status=%d body=%s", response.StatusCode, body)
	}
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()

	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	shortID := strings.ReplaceAll(instance.ID, "-", "")[:8]
	projectName := "hermes-fleet-" + instance.Name + "-" + shortID
	imageID := "sha256:" + strings.Repeat("a", 64)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ProjectName: projectName, DataVolume: projectName + "-data",
		ManagedPath: "/managed/" + instance.Name + "-" + shortID, ImageID: imageID,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/actions", map[string]string{
		"action": "stop",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)
	job = environment.claimAndAcknowledge(t, hostID, hostToken)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{Success: true},
		hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/recovery-points", map[string]any{},
		environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create recovery point status=%d body=%s", response.StatusCode, body)
	}
	var point recovery.Metadata
	decodeResponse(t, response, &point)
	response.Body.Close()
	job = environment.claimAndAcknowledge(t, hostID, hostToken)
	archive := recoveryArchiveForAPI(t, point)
	digest := sha256.Sum256(archive)
	encodedDigest := hex.EncodeToString(digest[:])
	response = environment.rawRequest(t, http.MethodPut, "/api/v1/agent/jobs/"+job.ID+"/recovery-point", bytes.NewReader(archive),
		hostToken, map[string]string{
			"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken,
			"X-Fleet-Recovery-Point-ID": point.ID, "X-Fleet-Recovery-SHA256": encodedDigest,
			"Content-Type": "application/x-tar",
		})
	assertStatus(t, response, http.StatusNoContent)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, RecoveryPointID: point.ID, RecoverySHA256: encodedDigest, RecoverySizeBytes: int64(len(archive)),
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)
	return instance, point
}

func waitForRecoveryPointLockReferences(t *testing.T, locks *keyedMutex, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		locks.mu.Lock()
		entry := locks.entries[key]
		refs := 0
		if entry != nil {
			refs = entry.refs
		}
		locks.mu.Unlock()
		if refs >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("recovery point lock %q did not reach %d references", key, want)
}

func TestTerminalRecoveryJobReleasesIncompleteReservation(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	response := environment.request(t, http.MethodPost, "/api/v1/instances", createInstanceRequest{
		Name: "fleet-terminal-recovery", HostID: hostID, HermesVersion: "0.19.0",
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create instance status=%d body=%s", response.StatusCode, body)
	}
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()

	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	shortID := strings.ReplaceAll(instance.ID, "-", "")[:8]
	projectName := "hermes-fleet-" + instance.Name + "-" + shortID
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ProjectName: projectName, DataVolume: projectName + "-data",
		ManagedPath: "/managed/" + instance.Name + "-" + shortID,
		ImageID:     "sha256:" + strings.Repeat("a", 64),
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/actions", map[string]string{
		"action": "stop",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)
	job = environment.claimAndAcknowledge(t, hostID, hostToken)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/recovery-points", map[string]any{}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create recovery point status=%d body=%s", response.StatusCode, body)
	}
	var point recovery.Metadata
	decodeResponse(t, response, &point)
	response.Body.Close()
	job = environment.claimAndAcknowledge(t, hostID, hostToken)
	if err := environment.dataStore.CompleteJob(
		context.Background(), hostID, job.ID, job.LeaseToken,
		domain.JobResult{Success: false, Error: "lease retries exhausted", RecoveryPointID: point.ID}, nil,
	); err != nil {
		t.Fatal(err)
	}

	server := &Server{
		config: Config{RecoveryPoints: environment.recoveryPoints},
		store:  environment.dataStore,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := server.reconcileTerminalRecoveryReservations(context.Background(), hostID); err != nil {
		t.Fatal(err)
	}
	if _, err := environment.recoveryPoints.Get(point.ID); !errors.Is(err, recovery.ErrNotFound) {
		t.Fatalf("terminal recovery reservation remains accessible: err=%v", err)
	}
}

func TestHermesUpdateStatusRejectsUnavailableFeedOrReusedTarget(t *testing.T) {
	instance := domain.Instance{
		ID: "00000000-0000-4000-8000-000000000001", Name: "fleet-test-01",
		Image: "local/hermes-fleet-runtime:0.18.2", Status: domain.InstanceStopped,
		Observation: &domain.InstanceObservation{
			HermesVersion: "0.18.2", HermesSource: "7acaff5ef2bcbaa22bd23b72efe60906123a4f55",
			TargetGeneration: time.Time{}.UTC().Format(time.RFC3339Nano),
		},
	}
	server := &Server{config: Config{}}
	status, err := server.hermesUpdateStatus(context.Background(), instance)
	if err != nil || status.Available || status.Reason != "Official Hermes update information is temporarily unavailable" {
		t.Fatalf("unavailable feed status=%+v err=%v", status, err)
	}

	catalog := testHermesCatalog()
	catalog.Releases[0].Image = instance.Image
	server.config.HermesReleaseSource = staticReleaseSource{catalog: catalog}
	status, err = server.hermesUpdateStatus(context.Background(), instance)
	if err != nil || status.Available || status.Reason != "A Hermes update must use a new versioned image reference" {
		t.Fatalf("reused target status=%+v err=%v", status, err)
	}
}

func TestCreateInstanceUsesSelectedHermesRelease(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, _ := environment.enrollHost(t)

	response := environment.request(t, http.MethodGet, "/api/v1/hermes-releases", nil, environment.adminToken, nil)
	var catalog releases.Catalog
	decodeResponse(t, response, &catalog)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(catalog.Releases) != 3 {
		t.Fatalf("release catalog status=%d catalog=%+v", response.StatusCode, catalog)
	}

	response = environment.request(t, http.MethodPost, "/api/v1/instances", createInstanceRequest{
		Name: "fleet-versioned", HostID: hostID, HermesVersion: "0.18.1",
	}, environment.adminToken, nil)
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted || instance.Image != "local/hermes-fleet-runtime:0.18.1-6acaff5ef2bc" {
		t.Fatalf("create selected release status=%d instance=%+v", response.StatusCode, instance)
	}

	response = environment.request(t, http.MethodPost, "/api/v1/instances", createInstanceRequest{
		Name: "fleet-unknown", HostID: hostID, HermesVersion: "0.17.0",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)
}

func TestCreateInstanceAndUpdateStatusShareTheLiveReleaseCatalog(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, _ := environment.enrollHost(t)
	live := testHermesCatalog()
	live.CheckedAt = live.CheckedAt.Add(time.Hour)
	live.Releases[0] = releases.Release{
		Version: "0.20.0", Tag: "v0.20.0", Commit: "dddddddddddddddddddddddddddddddddddddddd",
		Image: "local/hermes-fleet-runtime:0.20.0-dddddddddddd", URL: "https://github.com/NousResearch/hermes-agent/releases/tag/v0.20.0",
		PublishedAt: live.CheckedAt,
	}
	environment.server.config.HermesReleaseSource = staticReleaseSource{catalog: live}

	response := environment.request(t, http.MethodGet, "/api/v1/hermes-releases", nil, environment.adminToken, nil)
	var listed releases.Catalog
	decodeResponse(t, response, &listed)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || listed.Releases[0].Version != "0.20.0" {
		t.Fatalf("live release list status=%d catalog=%+v", response.StatusCode, listed)
	}

	response = environment.request(t, http.MethodPost, "/api/v1/instances", createInstanceRequest{
		Name: "fleet-live-release", HostID: hostID, HermesVersion: "0.20.0",
	}, environment.adminToken, nil)
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted || instance.Image != live.Releases[0].Image {
		t.Fatalf("create from live catalog status=%d instance=%+v", response.StatusCode, instance)
	}
	job, err := environment.dataStore.ClaimJob(context.Background(), hostID, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("claim provision job=%+v error=%v", job, err)
	}
	var payload domain.ProvisionPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.HermesVersion != live.Releases[0].Version || payload.HermesSource != live.Releases[0].Commit || payload.Image != live.Releases[0].Image {
		t.Fatalf("provision release identity=%+v", payload)
	}
}

func TestCreateInstanceRequiresOnlineCompatibleHostAgent(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, _ := environment.enrollHost(t)
	ctx := context.Background()

	if err := environment.dataStore.Heartbeat(
		ctx, hostID, "host", "darwin", "arm64", agentVersion, time.Now().UTC().Add(-time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	response := environment.request(t, http.MethodPost, "/api/v1/instances", createInstanceRequest{
		Name: "fleet-offline", HostID: hostID, HermesVersion: "0.19.0",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusConflict)

	if err := environment.dataStore.Heartbeat(
		ctx, hostID, "host", "darwin", "arm64", "0.0.0", time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	response = environment.request(t, http.MethodPost, "/api/v1/instances", createInstanceRequest{
		Name: "fleet-mismatch", HostID: hostID, HermesVersion: "0.19.0",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusConflict)

	if err := environment.dataStore.Heartbeat(
		ctx, hostID, "host", "darwin", "arm64", agentVersion, time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	response = environment.request(t, http.MethodPost, "/api/v1/instances", createInstanceRequest{
		Name: "fleet-compatible", HostID: hostID, HermesVersion: "0.19.0",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)
}

func TestHermesOfficialUpdateStatusUsesReleaseFeed(t *testing.T) {
	catalog := testHermesCatalog()
	server := &Server{config: Config{
		HermesCatalog:       catalog,
		HermesReleaseSource: staticReleaseSource{catalog: catalog},
	}}
	instance := domain.Instance{
		Image: "local/hermes-fleet-runtime:0.18.2", Status: domain.InstanceRunning,
		Observation: &domain.InstanceObservation{
			HermesVersion: "0.18.2", TargetGeneration: time.Time{}.UTC().Format(time.RFC3339Nano),
		},
	}
	status, err := server.hermesUpdateStatus(context.Background(), instance)
	if err != nil || status.OfficialStatus != "UPDATE_AVAILABLE" || status.LatestRelease == nil || status.LatestRelease.Version != "0.19.0" {
		t.Fatalf("official update status=%+v err=%v", status, err)
	}

	instance.Observation.HermesVersion = "0.19.0"
	status, err = server.hermesUpdateStatus(context.Background(), instance)
	if err != nil || status.OfficialStatus != "CURRENT" || status.Available {
		t.Fatalf("current official status=%+v err=%v", status, err)
	}

	server.config.HermesReleaseSource = staticReleaseSource{err: errors.New("offline")}
	status, err = server.hermesUpdateStatus(context.Background(), instance)
	if err != nil || status.OfficialStatus != "CURRENT" || status.LatestRelease == nil ||
		status.Available || status.Eligible || status.Reason != "The latest official Hermes version is installed" {
		t.Fatalf("last-known-good official status=%+v err=%v", status, err)
	}

	server.config.HermesReleaseSource = staticReleaseSource{catalog: catalog}
	instance.Observation.HermesVersion = "0.20.0"
	status, err = server.hermesUpdateStatus(context.Background(), instance)
	if err != nil || status.OfficialStatus != "UNKNOWN" || status.Available || status.Eligible {
		t.Fatalf("ahead-of-feed Hermes version was reported as current: status=%+v err=%v", status, err)
	}
}

func TestHermesUpdateStatusIgnoresAnObservationForAnOlderGeneration(t *testing.T) {
	catalog := testHermesCatalog()
	latest := catalog.Releases[0]
	previous := catalog.Releases[1]
	updatedAt := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	server := &Server{config: Config{
		HermesCatalog:       catalog,
		HermesReleaseSource: staticReleaseSource{catalog: catalog},
	}}
	instance := domain.Instance{
		Image:     latest.Image,
		Status:    domain.InstanceRunning,
		UpdatedAt: updatedAt,
		Observation: &domain.InstanceObservation{
			TargetGeneration: updatedAt.Add(-time.Minute).Format(time.RFC3339Nano),
			HermesVersion:    previous.Version,
			HermesSource:     previous.Commit,
		},
	}

	status, err := server.hermesUpdateStatus(context.Background(), instance)
	if err != nil {
		t.Fatal(err)
	}
	if status.CurrentVersion != latest.Version || status.OfficialStatus != "CURRENT" || status.Available {
		t.Fatalf("stale observation overrode the installed release: status=%+v", status)
	}
}

func TestHermesUpdateStatusIgnoresAnObservationWithoutGeneration(t *testing.T) {
	catalog := testHermesCatalog()
	latest := catalog.Releases[0]
	previous := catalog.Releases[1]
	server := &Server{config: Config{
		HermesCatalog:       catalog,
		HermesReleaseSource: staticReleaseSource{catalog: catalog},
	}}
	instance := domain.Instance{
		Image:  latest.Image,
		Status: domain.InstanceRunning,
		Observation: &domain.InstanceObservation{
			HermesVersion: previous.Version,
			HermesSource:  previous.Commit,
		},
	}

	status, err := server.hermesUpdateStatus(context.Background(), instance)
	if err != nil {
		t.Fatal(err)
	}
	if status.CurrentVersion != latest.Version || status.OfficialStatus != "CURRENT" || status.Available {
		t.Fatalf("unfenced observation overrode the installed release: status=%+v", status)
	}
}

func TestHermesUpdateStatusSeparatesRuntimeWrapperMaintenance(t *testing.T) {
	catalog := testHermesCatalog()
	latest := &catalog.Releases[0]
	oldWrapperImage := latest.Image + "-111111111111"
	latest.Image += "-222222222222"
	recoveryManager, err := recovery.New(t.TempDir(), strings.Repeat("02", 32), 20, 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{config: Config{
		HermesCatalog: catalog, HermesReleaseSource: staticReleaseSource{catalog: catalog},
		RecoveryPoints: recoveryManager,
	}}
	instance := domain.Instance{
		Image: oldWrapperImage, ImageID: "sha256:" + strings.Repeat("a", 64),
		Status: domain.InstanceRunning, ProjectName: "hermes-fleet-wrapper",
		DataVolume: "hermes-fleet-wrapper-data", ManagedPath: "/managed/hermes-fleet-wrapper",
		Observation: &domain.InstanceObservation{
			HermesVersion: latest.Version, HermesSource: latest.Commit,
		},
	}

	status, err := server.hermesUpdateStatus(context.Background(), instance)
	if err != nil || status.OfficialStatus != "CURRENT" ||
		status.UpdateKind != hermesUpdateKindRuntimeRefresh ||
		!status.Available || !status.Eligible ||
		status.TargetVersion != latest.Version || status.TargetSource != latest.Commit ||
		status.TargetImage != latest.Image ||
		!strings.Contains(status.Reason, "remains on the same version") {
		t.Fatalf("wrapper maintenance status=%+v err=%v", status, err)
	}
	refreshRequired, err := server.runtimeRefreshRequiredForInstance(context.Background(), instance)
	if err != nil || !refreshRequired {
		t.Fatalf("wrapper refresh preflight required=%v err=%v", refreshRequired, err)
	}
	server.config.HermesReleaseSource = staticReleaseSource{err: errors.New("offline")}
	refreshRequired, err = server.runtimeRefreshRequiredForInstance(context.Background(), instance)
	if err != nil || !refreshRequired {
		t.Fatalf("offline wrapper refresh preflight required=%v err=%v", refreshRequired, err)
	}
	server.config.HermesReleaseSource = staticReleaseSource{catalog: catalog}

	instance.Image = latest.Image
	status, err = server.hermesUpdateStatus(context.Background(), instance)
	if err != nil || status.OfficialStatus != "CURRENT" ||
		status.UpdateKind != hermesUpdateKindNone || status.Available || status.Eligible {
		t.Fatalf("current wrapper status=%+v err=%v", status, err)
	}
	refreshRequired, err = server.runtimeRefreshRequiredForInstance(context.Background(), instance)
	if err != nil || refreshRequired {
		t.Fatalf("current wrapper refresh preflight required=%v err=%v", refreshRequired, err)
	}

	instance.Image = oldWrapperImage
	instance.Observation.TargetGeneration = instance.UpdatedAt.UTC().Format(time.RFC3339Nano)
	instance.Observation.HermesSource = catalog.Releases[1].Commit
	status, err = server.hermesUpdateStatus(context.Background(), instance)
	if err != nil || status.OfficialStatus != "CURRENT" ||
		status.UpdateKind != hermesUpdateKindNone || status.Available || status.Eligible {
		t.Fatalf("source mismatch was not rejected: status=%+v err=%v", status, err)
	}

	instance.Observation = nil
	status, err = server.hermesUpdateStatus(context.Background(), instance)
	if err != nil || status.CurrentVersion != latest.Version || status.CurrentSource != latest.Commit ||
		status.UpdateKind != hermesUpdateKindRuntimeRefresh || !status.Available {
		t.Fatalf("known previous wrapper was not resolved: status=%+v err=%v", status, err)
	}

	instance.Image = "local/hermes-fleet-runtime:" + latest.Version
	status, err = server.hermesUpdateStatus(context.Background(), instance)
	if err != nil || status.OfficialStatus != "UNKNOWN" ||
		status.UpdateKind != hermesUpdateKindNone || status.Available || status.Eligible {
		t.Fatalf("unqualified image identity was trusted: status=%+v err=%v", status, err)
	}
}

func TestRuntimeRefreshPreflightDoesNotBlockOlderHermesVersion(t *testing.T) {
	catalog := testHermesCatalog()
	older := catalog.Releases[1]
	staleWrapper := older.Image + "-111111111111"
	catalog.Releases[1].Image = older.Image + "-222222222222"
	server := &Server{config: Config{
		HermesCatalog: catalog, HermesReleaseSource: staticReleaseSource{catalog: catalog},
	}}
	instance := domain.Instance{
		Image: staleWrapper, ImageID: "sha256:" + strings.Repeat("a", 64),
		Status: domain.InstanceRunning,
		Observation: &domain.InstanceObservation{
			HermesVersion: older.Version, HermesSource: older.Commit,
		},
	}
	status, err := server.hermesUpdateStatus(context.Background(), instance)
	if err != nil || status.OfficialStatus != "UPDATE_AVAILABLE" ||
		status.UpdateKind != hermesUpdateKindVersionUpdate || !status.Available {
		t.Fatalf("older Hermes status=%+v err=%v", status, err)
	}
	refreshRequired, err := server.runtimeRefreshRequiredForInstance(context.Background(), instance)
	if err != nil || refreshRequired {
		t.Fatalf("version update must not block provider configuration: required=%v err=%v", refreshRequired, err)
	}
}

func TestHermesRuntimeRefreshQueuesSameVersionWorkflow(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	catalog := testHermesCatalog()
	oldWrapperImage := catalog.Releases[0].Image + "-111111111111"
	catalog.Releases[0].Image = oldWrapperImage
	environment.server.config.HermesCatalog = catalog
	environment.server.config.HermesReleaseSource = staticReleaseSource{catalog: catalog}

	response := environment.request(t, http.MethodPost, "/api/v1/instances", createInstanceRequest{
		Name: "fleet-wrapper-refresh", HostID: hostID, HermesVersion: catalog.Releases[0].Version,
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create instance status=%d body=%s", response.StatusCode, body)
	}
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()
	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	projectName, dataVolume, managedDirectory := domain.ManagedIdentity(instance.ID, instance.Name)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ProjectName: projectName, DataVolume: dataVolume,
		ManagedPath: "/managed/" + managedDirectory, ImageID: "sha256:" + strings.Repeat("a", 64),
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)

	newWrapperImage := strings.TrimSuffix(oldWrapperImage, "111111111111") + "222222222222"
	catalog.Releases[0].Image = newWrapperImage
	environment.server.config.HermesCatalog = catalog
	environment.server.config.HermesReleaseSource = staticReleaseSource{catalog: catalog}

	response = environment.request(t, http.MethodGet, "/api/v1/instances/"+instance.ID+"/hermes-update", nil, environment.adminToken, nil)
	var status hermesUpdateResponse
	decodeResponse(t, response, &status)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || status.OfficialStatus != "CURRENT" ||
		status.UpdateKind != hermesUpdateKindRuntimeRefresh || !status.Available || !status.Eligible ||
		status.CurrentVersion != status.TargetVersion || status.TargetImage != newWrapperImage {
		t.Fatalf("runtime refresh status=%+v", status)
	}

	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/hermes-update", map[string]string{
		"confirm_name": instance.Name,
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("queue runtime refresh status=%d body=%s", response.StatusCode, body)
	}
	var operation domain.Operation
	decodeResponse(t, response, &operation)
	response.Body.Close()
	if operation.Summary != "Refresh managed Hermes runtime for "+instance.Name ||
		!bytes.Contains(operation.Metadata, []byte(`"update_kind":"RUNTIME_REFRESH"`)) ||
		!bytes.Contains(operation.Metadata, []byte(`"from_version":"0.19.0"`)) ||
		!bytes.Contains(operation.Metadata, []byte(`"to_version":"0.19.0"`)) {
		t.Fatalf("runtime refresh operation=%+v", operation)
	}
	job = environment.claimAndAcknowledge(t, hostID, hostToken)
	var payload domain.HermesUpdatePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if job.Type != "instance.hermes.update" ||
		payload.Upgrade.TargetVersion != catalog.Releases[0].Version ||
		payload.Upgrade.TargetImage != newWrapperImage ||
		payload.Upgrade.CurrentImage == payload.Upgrade.TargetImage {
		t.Fatalf("runtime refresh job=%+v payload=%+v", job, payload)
	}
}

func TestFailedRuntimeRefreshUsesVerifiedRecoveryInsteadOfProvisionRetry(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	catalog := testHermesCatalog()
	oldWrapperImage := catalog.Releases[0].Image + "-111111111111"
	catalog.Releases[0].Image = oldWrapperImage
	environment.server.config.HermesCatalog = catalog
	environment.server.config.HermesReleaseSource = staticReleaseSource{catalog: catalog}

	response := environment.request(t, http.MethodPost, "/api/v1/instances", createInstanceRequest{
		Name: "fleet-wrapper-recovery", HostID: hostID, HermesVersion: catalog.Releases[0].Version,
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create instance status=%d body=%s", response.StatusCode, body)
	}
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()

	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	projectName, dataVolume, managedDirectory := domain.ManagedIdentity(instance.ID, instance.Name)
	imageID := "sha256:" + strings.Repeat("a", 64)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ProjectName: projectName, DataVolume: dataVolume,
		ManagedPath: "/managed/" + managedDirectory, ImageID: imageID,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)

	newWrapperImage := strings.TrimSuffix(oldWrapperImage, "111111111111") + "222222222222"
	catalog.Releases[0].Image = newWrapperImage
	environment.server.config.HermesCatalog = catalog
	environment.server.config.HermesReleaseSource = staticReleaseSource{catalog: catalog}

	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/actions", map[string]string{
		"action": "stop", "workflow_id": "00000000-0000-4000-8000-000000000021",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)
	job = environment.claimAndAcknowledge(t, hostID, hostToken)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: false, Error: "simulated lifecycle failure",
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)
	if stored := environment.overviewInstance(t, instance.ID); stored.Status != domain.InstanceFailed {
		t.Fatalf("failed lifecycle status=%q", stored.Status)
	}

	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/actions", map[string]string{
		"action": "retry", "workflow_id": "00000000-0000-4000-8000-000000000022",
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("legacy wrapper retry status=%d body=%s", response.StatusCode, body)
	}
	response.Body.Close()

	statusResponse := func() hermesUpdateResponse {
		t.Helper()
		response := environment.request(t, http.MethodGet, "/api/v1/instances/"+instance.ID+"/hermes-update", nil, environment.adminToken, nil)
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("Hermes update status=%d body=%s", response.StatusCode, body)
		}
		var status hermesUpdateResponse
		decodeResponse(t, response, &status)
		return status
	}
	status := statusResponse()
	if status.UpdateKind != hermesUpdateKindRuntimeRefresh || !status.Available || status.Eligible ||
		!strings.Contains(status.Reason, "Refresh diagnostics") {
		t.Fatalf("unverified failed recovery status=%+v", status)
	}

	targets, err := environment.dataStore.ListObservationTargets(context.Background(), hostID)
	if err != nil || len(targets) != 1 {
		t.Fatalf("observation targets=%+v error=%v", targets, err)
	}
	baseTime := time.Now().UTC()
	requiredChecks := []domain.ObservationCheck{
		{Name: "managed_path", Status: domain.ObservationCheckOK, Detail: "Managed instance directory exists"},
		{Name: "manifest", Status: domain.ObservationCheckOK, Detail: "Managed Compose manifest exists"},
		{Name: "environment", Status: domain.ObservationCheckOK, Detail: "Managed environment file exists"},
		{Name: "workspace", Status: domain.ObservationCheckOK, Detail: "Managed workspace directory exists"},
		{Name: "docker_daemon", Status: domain.ObservationCheckOK, Detail: "Docker daemon responded"},
		{Name: "data_volume", Status: domain.ObservationCheckOK, Detail: "Expected Fleet data volume exists"},
		{Name: "containers", Status: domain.ObservationCheckOK, Detail: "Hermes and dashboard containers exist"},
		{Name: "ownership", Status: domain.ObservationCheckOK, Detail: "Container labels match"},
		{Name: "image", Status: domain.ObservationCheckOK, Detail: "Container images match"},
		{Name: "runtime", Status: domain.ObservationCheckDrift, Detail: "Lifecycle state is FAILED"},
	}
	incompleteChecks := append([]domain.ObservationCheck(nil), requiredChecks...)
	incompleteChecks[7].Status = domain.ObservationCheckDrift
	report := domain.InstanceObservation{
		InstanceID: instance.ID, TargetGeneration: targets[0].Generation,
		HermesVersion: catalog.Releases[0].Version, HermesSource: catalog.Releases[0].Commit,
		Status: domain.ObservationDegraded, Summary: "Retained runtime requires recovery",
		Checks: incompleteChecks, ObservedAt: baseTime,
	}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/observations", map[string]any{
		"observations": []domain.InstanceObservation{report},
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID})
	assertStatus(t, response, http.StatusNoContent)
	status = statusResponse()
	if status.Eligible || !strings.Contains(status.Reason, "must pass diagnostics") {
		t.Fatalf("unsafe retained artifacts were accepted: %+v", status)
	}

	report.Checks = requiredChecks
	report.ObservedAt = baseTime.Add(time.Millisecond)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/observations", map[string]any{
		"observations": []domain.InstanceObservation{report},
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID})
	assertStatus(t, response, http.StatusNoContent)
	status = statusResponse()
	if !status.Eligible || !strings.Contains(status.Reason, "restore Hermes to RUNNING") {
		t.Fatalf("verified failed recovery status=%+v", status)
	}

	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/hermes-update", map[string]string{
		"confirm_name": instance.Name,
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)
	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/hermes-update", map[string]string{
		"confirm_name": instance.Name, "workflow_id": "00000000-0000-4000-8000-000000000023", "restore_status": domain.InstanceRunning,
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("queue failed runtime recovery status=%d body=%s", response.StatusCode, body)
	}
	var operation domain.Operation
	decodeResponse(t, response, &operation)
	response.Body.Close()
	if operation.Summary != "Refresh managed Hermes runtime for "+instance.Name ||
		!bytes.Contains(operation.Metadata, []byte(`"initial_status":"FAILED"`)) ||
		!bytes.Contains(operation.Metadata, []byte(`"original_status":"RUNNING"`)) {
		t.Fatalf("failed recovery operation=%+v", operation)
	}
	job = environment.claimAndAcknowledge(t, hostID, hostToken)
	var payload domain.HermesUpdatePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if job.Type != "instance.hermes.update" || payload.OriginalStatus != domain.InstanceRunning ||
		payload.Upgrade.CurrentImage != oldWrapperImage || payload.Upgrade.TargetImage != newWrapperImage ||
		payload.Upgrade.CurrentImageID != imageID {
		t.Fatalf("failed recovery job=%+v payload=%+v", job, payload)
	}
}

func TestCredentialRevealEndToEnd(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	create := createInstanceRequest{
		Name: "fleet-test-01", HostID: hostID, Image: "local/hermes-fleet-runtime:0.18.2",
		Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
		APIPort: 18650, DashboardPort: 19130,
	}
	response := environment.request(t, http.MethodPost, "/api/v1/instances", create, environment.adminToken, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("create instance status=%d", response.StatusCode)
	}
	var instance domain.Instance
	decodeResponse(t, response, &instance)

	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	projectName, dataVolume, directoryName := domain.ManagedIdentity(instance.ID, instance.Name)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ProjectName: projectName, DataVolume: dataVolume,
		ManagedPath: "/managed/" + directoryName, ImageID: "sha256:" + strings.Repeat("a", 64),
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/credentials", map[string]any{}, environment.adminToken, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("request credentials status=%d", response.StatusCode)
	}
	var operation domain.Operation
	decodeResponse(t, response, &operation)
	response = environment.request(t, http.MethodGet, "/api/v1/credential-reveals/"+operation.ID, nil, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted || response.Header.Get("Retry-After") != "1" {
		t.Fatalf("pending credential reveal status=%d retry-after=%q", response.StatusCode, response.Header.Get("Retry-After"))
	}
	var pending domain.Operation
	decodeResponse(t, response, &pending)
	response.Body.Close()
	if pending.ID != operation.ID || pending.Status != domain.OperationPending {
		t.Fatalf("pending credential operation=%+v", pending)
	}

	job = environment.claimAndAcknowledge(t, hostID, hostToken)
	wanted := domain.Credentials{DashboardUsername: "admin", DashboardPassword: "dashboard-secret", APIServerKey: "api-secret"}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, Credentials: &wanted,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodGet, "/api/v1/credential-reveals/"+operation.ID, nil, environment.adminToken, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("credential reveal status=%d cache=%q", response.StatusCode, response.Header.Get("Cache-Control"))
	}
	var reveal struct {
		Credentials domain.Credentials `json:"credentials"`
	}
	decodeResponse(t, response, &reveal)
	if reveal.Credentials != wanted {
		t.Fatalf("credential reveal=%+v", reveal.Credentials)
	}
}

func TestCredentialRevealRejectsBusyInstanceWithoutQueuing(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	create := createInstanceRequest{
		Name: "fleet-busy-01", HostID: hostID, Image: "local/hermes-fleet-runtime:0.18.2",
		Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
		APIPort: 28650, DashboardPort: 29130,
	}
	response := environment.request(t, http.MethodPost, "/api/v1/instances", create, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("create instance status=%d", response.StatusCode)
	}
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()

	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	projectName, dataVolume, directoryName := domain.ManagedIdentity(instance.ID, instance.Name)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ProjectName: projectName, DataVolume: dataVolume,
		ManagedPath: "/managed/" + directoryName, ImageID: "sha256:" + strings.Repeat("a", 64),
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)
	response.Body.Close()

	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/codex-auth", map[string]any{}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)
	response.Body.Close()

	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/credentials", map[string]any{}, environment.adminToken, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("busy credential request status=%d, want %d", response.StatusCode, http.StatusConflict)
	}
	var failure map[string]string
	decodeResponse(t, response, &failure)
	if !strings.Contains(failure["error"], "instance is busy") {
		t.Fatalf("busy credential error=%q", failure["error"])
	}
}

func TestLegacyProviderProfileAPIsAreNotExposed(t *testing.T) {
	environment := newAPITestEnvironment(t)
	for _, path := range []string{"/api/v1/provider-catalog", "/api/v1/provider-profiles", "/api/v1/instances/instance/provider-binding"} {
		response := environment.request(t, http.MethodGet, path, nil, environment.adminToken, nil)
		assertStatus(t, response, http.StatusNotFound)
	}
}

func TestCodexAuthenticationFlowIsFleetManagedAndLeaseFenced(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	create := createInstanceRequest{
		Name: "fleet-auth-01", HostID: hostID, Image: "local/hermes-fleet-runtime:0.18.2",
		Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
		APIPort: 48650, DashboardPort: 49130,
	}
	response := environment.request(t, http.MethodPost, "/api/v1/instances", create, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create instance status=%d body=%s", response.StatusCode, body)
	}
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()
	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	shortID := strings.ReplaceAll(instance.ID, "-", "")[:8]
	projectName := "hermes-fleet-" + instance.Name + "-" + shortID
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ProjectName: projectName, DataVolume: projectName + "-data",
		ManagedPath: "/managed/" + instance.Name + "-" + shortID, ImageID: "sha256:" + strings.Repeat("a", 64),
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/codex-auth", map[string]any{}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("start Codex auth status=%d cache=%q", response.StatusCode, response.Header.Get("Cache-Control"))
	}
	var session domain.CodexAuthSession
	decodeResponse(t, response, &session)
	response.Body.Close()
	if session.Provider != "openai-codex" {
		t.Fatalf("started auth provider=%q", session.Provider)
	}
	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/codex-auth", map[string]any{}, environment.adminToken, nil)
	var resumed domain.CodexAuthSession
	decodeResponse(t, response, &resumed)
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted || resumed.OperationID != session.OperationID || resumed.Provider != "openai-codex" {
		t.Fatalf("duplicate auth did not resume active session: status=%d session=%+v", response.StatusCode, resumed)
	}
	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/codex-auth", map[string]any{"provider": "xai-oauth"}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusConflict)
	response.Body.Close()
	authJob := environment.claimAndAcknowledge(t, hostID, hostToken)
	if authJob.Type != "instance.auth.codex" {
		t.Fatalf("auth job type=%q", authJob.Type)
	}
	headers := map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: authJob.LeaseToken}
	invalid := domain.JobProgress{Stage: "AWAITING_USER", VerificationURI: "https://attacker.invalid", UserCode: "ABCD-EFGH", ExpiresAt: time.Now().UTC().Add(15 * time.Minute)}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+authJob.ID+"/progress", invalid, hostToken, headers)
	assertStatus(t, response, http.StatusBadRequest)
	valid := domain.JobProgress{Stage: "AWAITING_USER", VerificationURI: codexDeviceURL, UserCode: "ABCD-EFGH", ExpiresAt: time.Now().UTC().Add(15 * time.Minute)}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+authJob.ID+"/progress", valid, hostToken, headers)
	assertStatus(t, response, http.StatusNoContent)
	response = environment.request(t, http.MethodGet, "/api/v1/instances/"+instance.ID+"/codex-auth/"+session.OperationID, nil, environment.adminToken, nil)
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" || !bytes.Contains(body, []byte("ABCD-EFGH")) || bytes.Contains(body, []byte("access_token")) {
		t.Fatalf("auth session response status=%d cache=%q body=%s error=%v", response.StatusCode, response.Header.Get("Cache-Control"), body, err)
	}
	response = environment.request(t, http.MethodDelete, "/api/v1/instances/"+instance.ID+"/codex-auth/"+session.OperationID, nil, environment.adminToken, nil)
	assertStatus(t, response, http.StatusNoContent)
	response.Body.Close()
	response = environment.request(t, http.MethodGet, "/api/v1/instances/"+instance.ID+"/codex-auth/"+session.OperationID, nil, environment.adminToken, nil)
	var canceled domain.CodexAuthSession
	decodeResponse(t, response, &canceled)
	response.Body.Close()
	if canceled.Status != domain.OperationFailed || canceled.Error == "" || canceled.UserCode != "" || canceled.VerificationURI != "" {
		t.Fatalf("canceled auth session=%+v", canceled)
	}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+authJob.ID+"/complete", domain.JobResult{Success: true}, hostToken, headers)
	assertStatus(t, response, http.StatusConflict)
	response.Body.Close()
	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/codex-auth", map[string]any{}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)
	var retry domain.CodexAuthSession
	decodeResponse(t, response, &retry)
	response.Body.Close()
	if retry.OperationID == session.OperationID {
		t.Fatal("canceled Codex authentication session was reused")
	}
	retryJob := environment.claimAndAcknowledge(t, hostID, hostToken)
	retryHeaders := map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: retryJob.LeaseToken}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+retryJob.ID+"/complete", domain.JobResult{Success: true}, hostToken, retryHeaders)
	assertStatus(t, response, http.StatusNoContent)
	stored := environment.overviewInstance(t, instance.ID)
	if stored.Status != domain.InstanceRunning || stored.LastError != "" {
		t.Fatalf("Codex auth changed instance lifecycle: %+v", stored)
	}
}

func TestGrokOAuthInstanceUsesSharedProviderAuthJob(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	create := createInstanceRequest{
		Name: "fleet-grok-01", HostID: hostID, Image: "local/hermes-fleet-runtime:0.18.2",
		Provider: "xai-oauth", APIPort: 58650, DashboardPort: 59130,
	}
	response := environment.request(t, http.MethodPost, "/api/v1/instances", create, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create Grok instance status=%d body=%s", response.StatusCode, body)
	}
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()
	if instance.Provider != "xai-oauth" || instance.Model != "" {
		t.Fatalf("created instance=%+v", instance)
	}
	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	shortID := strings.ReplaceAll(instance.ID, "-", "")[:8]
	projectName := "hermes-fleet-" + instance.Name + "-" + shortID
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ProjectName: projectName, DataVolume: projectName + "-data",
		ManagedPath: "/managed/" + instance.Name + "-" + shortID, ImageID: "sha256:" + strings.Repeat("a", 64),
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/codex-auth", map[string]any{}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)
	authJob := environment.claimAndAcknowledge(t, hostID, hostToken)
	var payload domain.CodexAuthPayload
	if err := json.Unmarshal(authJob.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if authJob.Type != "instance.auth.codex" || payload.Provider != "xai-oauth" {
		t.Fatalf("Grok auth job=%+v payload=%+v", authJob, payload)
	}
	headers := map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: authJob.LeaseToken}
	progress := domain.JobProgress{
		Stage: "AWAITING_USER", VerificationURI: "https://auth.x.ai/oauth2/device?user_code=WXYZ-1234",
		UserCode: "WXYZ-1234",
	}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+authJob.ID+"/progress", progress, hostToken, headers)
	assertStatus(t, response, http.StatusNoContent)
}

func TestCodexInstanceCanAuthenticateAndActivateGrok(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	create := createInstanceRequest{
		Name: "fleet-multi-oauth-01", HostID: hostID, Image: "local/hermes-fleet-runtime:0.18.2",
		Provider: "openai-codex", APIPort: 61650, DashboardPort: 62130,
	}
	response := environment.request(t, http.MethodPost, "/api/v1/instances", create, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create instance status=%d body=%s", response.StatusCode, body)
	}
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()
	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	shortID := strings.ReplaceAll(instance.ID, "-", "")[:8]
	projectName := "hermes-fleet-" + instance.Name + "-" + shortID
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ProjectName: projectName, DataVolume: projectName + "-data",
		ManagedPath: "/managed/" + instance.Name + "-" + shortID, ImageID: "sha256:" + strings.Repeat("a", 64),
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/codex-auth", map[string]any{
		"provider": "xai-oauth",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)
	authJob := environment.claimAndAcknowledge(t, hostID, hostToken)
	var authPayload domain.CodexAuthPayload
	if err := json.Unmarshal(authJob.Payload, &authPayload); err != nil {
		t.Fatal(err)
	}
	if authJob.Type != "instance.auth.codex" || authPayload.Provider != "xai-oauth" {
		t.Fatalf("Grok auth on Codex instance job=%+v payload=%+v", authJob, authPayload)
	}
	headers := map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: authJob.LeaseToken}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+authJob.ID+"/complete", domain.JobResult{Success: true}, hostToken, headers)
	assertStatus(t, response, http.StatusNoContent)

	targets, err := environment.dataStore.ListObservationTargets(context.Background(), hostID)
	if err != nil || len(targets) != 1 {
		t.Fatalf("observation targets=%+v error=%v", targets, err)
	}
	report := domain.InstanceObservation{
		InstanceID: instance.ID, TargetGeneration: targets[0].Generation, Status: domain.ObservationDegraded,
		Summary: "Grok configuration is required", ModelCatalog: []string{"gpt-5.6-sol"}, RecommendedModel: "gpt-5.6-sol",
		ProviderModelCatalogs: map[string]domain.ProviderModelCatalog{
			"openai-codex": {Models: []string{"gpt-5.6-sol"}, Recommended: "gpt-5.6-sol"},
			"xai-oauth":    {Models: []string{"grok-4.6"}, Recommended: "grok-4.6"},
		},
		Checks: []domain.ObservationCheck{
			{Name: "codex_auth", Status: domain.ObservationCheckOK, Detail: "Codex authentication is connected"},
			{Name: "provider_auth", Status: domain.ObservationCheckOK, Detail: "Grok authentication is connected"},
			{Name: "runtime_configuration", Status: domain.ObservationCheckDrift, Detail: "Choose a Grok model in Hermes Fleet"},
		},
		ObservedAt: time.Now().UTC(),
	}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/observations", map[string]any{
		"observations": []domain.InstanceObservation{report},
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodPut, "/api/v1/instances/"+instance.ID+"/codex-configuration", map[string]string{
		"provider": "xai-oauth", "model": "grok-4.6", "reasoning": "medium", "service_tier": "normal",
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("configure Grok on Codex instance status=%d body=%s", response.StatusCode, body)
	}
	response.Body.Close()
	job = environment.claimAndAcknowledge(t, hostID, hostToken)
	var payload domain.RuntimeSyncPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if job.Type != "instance.runtime.configure" || payload.Provider != "xai-oauth" || payload.Model != "grok-4.6" {
		t.Fatalf("active provider switch job=%+v payload=%+v", job, payload)
	}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, InstanceStatus: domain.InstanceRunning,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)
	stored := environment.overviewInstance(t, instance.ID)
	if stored.Provider != "xai-oauth" || stored.Model != "grok-4.6" || !stored.CodexConfigured {
		t.Fatalf("active provider was not switched to Grok: %+v", stored)
	}
}

func TestRuntimeObservationEndToEndAndEffectiveUnknownStates(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	create := createInstanceRequest{
		Name: "fleet-observe-01", HostID: hostID, Image: "local/hermes-fleet-runtime:0.18.2",
		Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
		APIPort: 28650, DashboardPort: 29130,
	}
	response := environment.request(t, http.MethodPost, "/api/v1/instances", create, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create instance status=%d body=%s", response.StatusCode, body)
	}
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()

	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	shortID := strings.ReplaceAll(instance.ID, "-", "")[:8]
	projectName := "hermes-fleet-" + instance.Name + "-" + shortID
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ProjectName: projectName, DataVolume: projectName + "-data",
		ManagedPath: "/managed/" + instance.Name + "-" + shortID, ImageID: "sha256:" + strings.Repeat("a", 64),
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodPost, "/api/v1/agent/observations/targets", map[string]any{}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("observation targets status=%d", response.StatusCode)
	}
	var targetResponse struct {
		Targets []domain.ObservationTarget `json:"targets"`
	}
	decodeResponse(t, response, &targetResponse)
	response.Body.Close()
	if len(targetResponse.Targets) != 1 || targetResponse.Targets[0].InstanceID != instance.ID {
		t.Fatalf("observation targets=%+v", targetResponse.Targets)
	}
	target := targetResponse.Targets[0]
	observedAt := time.Now().UTC()
	report := domain.InstanceObservation{
		InstanceID: instance.ID, TargetGeneration: target.Generation, Status: domain.ObservationInSync,
		Summary: "Runtime matches desired state", Checks: []domain.ObservationCheck{{Name: "runtime", Status: domain.ObservationCheckOK, Detail: "Both services are running"}},
		ObservedAt: observedAt,
	}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/observations", map[string]any{"observations": []domain.InstanceObservation{report}}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID})
	assertStatus(t, response, http.StatusNoContent)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/observations", map[string]any{"observations": []domain.InstanceObservation{report}}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID})
	assertStatus(t, response, http.StatusConflict)

	observed := environment.overviewInstance(t, instance.ID)
	if observed.Observation == nil || observed.Observation.Status != domain.ObservationInSync {
		t.Fatalf("overview observation=%+v", observed.Observation)
	}

	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/observations/refresh", map[string]any{}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("request observation refresh status=%d body=%s", response.StatusCode, body)
	}
	var refresh domain.ObservationRequest
	decodeResponse(t, response, &refresh)
	response.Body.Close()
	response = environment.request(t, http.MethodPost, "/api/v1/agent/observations/targets", map[string]any{}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID})
	decodeResponse(t, response, &targetResponse)
	response.Body.Close()
	if len(targetResponse.Targets) != 1 || targetResponse.Targets[0].RefreshRequestID != refresh.ID {
		t.Fatalf("refresh request was not delivered to agent: targets=%+v refresh=%+v", targetResponse.Targets, refresh)
	}
	report.RefreshRequestID = refresh.ID
	report.ObservedAt = observedAt.Add(time.Second)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/observations", map[string]any{"observations": []domain.InstanceObservation{report}}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID})
	assertStatus(t, response, http.StatusNoContent)
	observed = environment.overviewInstance(t, instance.ID)
	if observed.ObservationRequest != nil {
		t.Fatalf("completed refresh remains pending: %+v", observed.ObservationRequest)
	}

	if err := environment.dataStore.Heartbeat(context.Background(), hostID, "host", "darwin", "arm64", agentVersion, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	observed = environment.overviewInstance(t, instance.ID)
	if observed.Observation == nil || observed.Observation.Status != domain.ObservationUnknown || !strings.Contains(observed.Observation.Summary, "offline") {
		t.Fatalf("offline host did not force UNKNOWN: %+v", observed.Observation)
	}
	if err := environment.dataStore.Heartbeat(context.Background(), hostID, "host", "darwin", "arm64", agentVersion, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/actions", map[string]string{"action": "stop"}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)
	report.Status = domain.ObservationMissing
	report.Summary = "Old generation report"
	report.ObservedAt = observedAt.Add(2 * time.Second)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/observations", map[string]any{"observations": []domain.InstanceObservation{report}}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID})
	assertStatus(t, response, http.StatusConflict)
	observed = environment.overviewInstance(t, instance.ID)
	if observed.Observation == nil || observed.Observation.Status != domain.ObservationUnknown {
		t.Fatalf("old target generation did not become UNKNOWN: %+v", observed.Observation)
	}
}

func TestObservationAPIRejectsCrossHostAndInvalidReports(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHostNamed(t, "host-one")
	otherHostID, otherHostToken := environment.enrollHostNamed(t, "host-two")
	create := createInstanceRequest{
		Name: "fleet-api-observe", HostID: hostID, Image: "local/hermes-fleet-runtime:0.18.2",
		Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
		APIPort: 38650, DashboardPort: 39130,
	}
	response := environment.request(t, http.MethodPost, "/api/v1/instances", create, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("create instance status=%d", response.StatusCode)
	}
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()
	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	shortID := strings.ReplaceAll(instance.ID, "-", "")[:8]
	projectName := "hermes-fleet-" + instance.Name + "-" + shortID
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ProjectName: projectName, DataVolume: projectName + "-data",
		ManagedPath: "/managed/" + instance.Name + "-" + shortID, ImageID: "sha256:" + strings.Repeat("a", 64),
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)
	now := time.Now().UTC()
	targets, err := environment.dataStore.ListObservationTargets(context.Background(), hostID)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets=%+v error=%v", targets, err)
	}
	report := domain.InstanceObservation{
		InstanceID: instance.ID, TargetGeneration: targets[0].Generation, Status: domain.ObservationInSync,
		Summary: "Current", Checks: []domain.ObservationCheck{{Name: "runtime", Status: domain.ObservationCheckOK, Detail: "Running"}}, ObservedAt: now,
	}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/observations", map[string]any{"observations": []domain.InstanceObservation{report}}, otherHostToken, map[string]string{"X-Fleet-Host-ID": otherHostID})
	assertStatus(t, response, http.StatusForbidden)
	report.Status = "TRUST_ME"
	response = environment.request(t, http.MethodPost, "/api/v1/agent/observations", map[string]any{"observations": []domain.InstanceObservation{report}}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID})
	assertStatus(t, response, http.StatusBadRequest)
}

func TestTelegramTokenHintDoesNotExposeSecret(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  string
	}{
		{name: "configured", token: "123456789:super-secret", want: "123456789:••••••••"},
		{name: "empty", token: "", want: ""},
		{name: "unexpected format", token: "legacy-secret", want: "••••••••"},
		{name: "non numeric identity", token: "bot-name:super-secret", want: "••••••••"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := telegramTokenHint(test.token)
			if got != test.want {
				t.Fatalf("telegramTokenHint()=%q, want %q", got, test.want)
			}
			if strings.Contains(got, "super-secret") || strings.Contains(got, "legacy-secret") {
				t.Fatalf("telegramTokenHint() exposed secret material: %q", got)
			}
		})
	}
}

func TestMessagingConfigurationKeepsSecretsOutOfAdminAndJobPayloads(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHostNamed(t, "messaging-host")
	create := createInstanceRequest{
		Name: "fleet-messaging", HostID: hostID, Image: "local/hermes-fleet-runtime:0.18.2",
		Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
		APIPort: 48650, DashboardPort: 49130,
	}
	response := environment.request(t, http.MethodPost, "/api/v1/instances", create, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()

	provisionJob := environment.claimAndAcknowledge(t, hostID, hostToken)
	shortID := strings.ReplaceAll(instance.ID, "-", "")[:8]
	projectName := "hermes-fleet-" + instance.Name + "-" + shortID
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+provisionJob.ID+"/complete", domain.JobResult{
		Success: true, ProjectName: projectName, DataVolume: projectName + "-data",
		ManagedPath: "/managed/" + instance.Name + "-" + shortID, ImageID: "sha256:" + strings.Repeat("a", 64),
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: provisionJob.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodGet, "/api/v1/instances/"+instance.ID+"/messaging", nil, environment.adminToken, nil)
	assertStatus(t, response, http.StatusOK)
	initialBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(initialBody, []byte(`"allowed_users":[]`)) != 2 ||
		!bytes.Contains(initialBody, []byte(`"group_allowed_users":[]`)) ||
		!bytes.Contains(initialBody, []byte(`"group_allowed_chats":[]`)) {
		t.Fatalf("unconfigured messaging lists must be empty arrays: %s", initialBody)
	}

	const telegramToken = "123456789:secret-bot-token"
	response = environment.request(t, http.MethodPut, "/api/v1/instances/"+instance.ID+"/messaging", map[string]any{
		"telegram": map[string]any{
			"enabled": true, "bot_token": telegramToken, "allowed_users": []string{"42"},
			"group_allowed_users": []string{"43"}, "group_allowed_chats": []string{"-100123"},
			"require_mention": true,
		},
		"whatsapp": map[string]any{
			"enabled": true, "mode": "bot", "allowed_users": []string{"628123456789"},
			"unauthorized_dm_behavior": "ignore", "reply_prefix": "Hermes",
		},
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)
	response.Body.Close()

	record, err := environment.dataStore.GetMessagingConfig(context.Background(), instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(record.Ciphertext, telegramToken) {
		t.Fatal("stored messaging ciphertext exposed the Telegram token")
	}
	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	if job.Type != "instance.messaging.configure" {
		t.Fatalf("job type=%q, want instance.messaging.configure", job.Type)
	}
	if strings.Contains(string(job.Payload), telegramToken) {
		t.Fatal("Host Agent job payload exposed the Telegram token")
	}

	response = environment.request(t, http.MethodGet, "/api/v1/instances/"+instance.ID+"/messaging", nil, environment.adminToken, nil)
	assertStatus(t, response, http.StatusOK)
	adminBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(adminBody), telegramToken) ||
		!strings.Contains(string(adminBody), `"token_configured":true`) ||
		!strings.Contains(string(adminBody), `"token_hint":"123456789:••••••••"`) {
		t.Fatalf("admin messaging response did not redact the token: %s", adminBody)
	}

	response = environment.request(
		t, http.MethodGet, "/api/v1/agent/jobs/"+job.ID+"/messaging-config", nil, hostToken,
		map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken},
	)
	assertStatus(t, response, http.StatusOK)
	leasedBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(leasedBody), telegramToken) || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("leased messaging response did not contain the protected configuration: %s", leasedBody)
	}

	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, InstanceStatus: domain.InstanceRunning,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)
	record, err = environment.dataStore.GetMessagingConfig(context.Background(), instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "APPLIED" || record.AppliedRevision != record.DesiredRevision || record.AppliedAt == nil {
		t.Fatalf("messaging configuration was not marked applied: %+v", record)
	}
}

func TestMCPConfigurationKeepsSecretsOutOfAdminAndJobPayloads(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHostNamed(t, "mcp-host")
	create := createInstanceRequest{
		Name: "fleet-mcp", HostID: hostID, Image: "local/hermes-fleet-runtime:0.18.2",
		Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
		APIPort: 48651, DashboardPort: 49131,
	}
	response := environment.request(t, http.MethodPost, "/api/v1/instances", create, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)
	var instance domain.Instance
	decodeResponse(t, response, &instance)
	response.Body.Close()

	provisionJob := environment.claimAndAcknowledge(t, hostID, hostToken)
	shortID := strings.ReplaceAll(instance.ID, "-", "")[:8]
	projectName := "hermes-fleet-" + instance.Name + "-" + shortID
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+provisionJob.ID+"/complete", domain.JobResult{
		Success: true, ProjectName: projectName, DataVolume: projectName + "-data",
		ManagedPath: "/managed/" + instance.Name + "-" + shortID, ImageID: "sha256:" + strings.Repeat("b", 64),
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: provisionJob.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodPut, "/api/v1/instances/"+instance.ID+"/mcp", map[string]any{
		"servers": []map[string]any{{
			"name": "unsafe", "source": "stdio", "url": "https://mcp.example.com", "auth_type": "none",
			"enabled": true, "tools": []string{"search"},
		}},
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)
	response.Body.Close()

	const bearerToken = "mcp-secret-token-that-must-not-leak"
	response = environment.request(t, http.MethodPut, "/api/v1/instances/"+instance.ID+"/mcp", map[string]any{
		"servers": []map[string]any{{
			"name": "knowledge", "source": "remote", "url": "https://mcp.example.com/v1", "auth_type": "bearer",
			"bearer_token": bearerToken, "enabled": true, "tools": []string{"search", "fetch"},
		}},
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)
	operationBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(operationBody), bearerToken) {
		t.Fatal("MCP operation response exposed the bearer token")
	}

	record, err := environment.dataStore.GetMCPConfig(context.Background(), instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(record.Ciphertext, bearerToken) {
		t.Fatal("stored MCP ciphertext exposed the bearer token")
	}
	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	if job.Type != "instance.mcp.configure" {
		t.Fatalf("job type=%q, want instance.mcp.configure", job.Type)
	}
	if strings.Contains(string(job.Payload), bearerToken) {
		t.Fatal("Host Agent job payload exposed the MCP bearer token")
	}
	progressHeaders := map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/progress", domain.JobProgress{
		Stage: "VALIDATING", Detail: "Validating the Fleet-owned MCP definition",
	}, hostToken, progressHeaders)
	assertStatus(t, response, http.StatusNoContent)
	response.Body.Close()

	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/progress", domain.JobProgress{
		Stage: "WRITING_CONFIGURATION", Detail: "This update must remain lease fenced",
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: "wrong-lease-token"})
	assertStatus(t, response, http.StatusConflict)
	response.Body.Close()

	response = environment.request(t, http.MethodGet, "/api/v1/instances/"+instance.ID+"/mcp", nil, environment.adminToken, nil)
	assertStatus(t, response, http.StatusOK)
	adminBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(adminBody), bearerToken) ||
		!strings.Contains(string(adminBody), `"token_configured":true`) ||
		!strings.Contains(string(adminBody), `"token_hint":"••••••••"`) {
		t.Fatalf("admin MCP response did not redact the bearer token: %s", adminBody)
	}

	discoveryCalls := 0
	environment.server.mcpDiscover = func(_ context.Context, request mcpdiscovery.Request) ([]mcpdiscovery.Tool, error) {
		discoveryCalls++
		if request.URL != "https://mcp.example.com/v1" || request.BearerToken != bearerToken {
			t.Fatalf("discovery request did not retain the stored endpoint and token: %+v", request)
		}
		return []mcpdiscovery.Tool{{Name: "search", Description: "Search records"}, {Name: "fetch"}}, nil
	}
	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/mcp/discover", map[string]any{
		"original_name": "knowledge", "name": "knowledge", "url": "https://mcp.example.com/v1", "auth_type": "bearer",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusOK)
	discoveryBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if discoveryCalls != 1 || strings.Contains(string(discoveryBody), bearerToken) ||
		!strings.Contains(string(discoveryBody), `"name":"search"`) || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected MCP discovery response: %s", discoveryBody)
	}
	environment.server.mcpDiscover = func(_ context.Context, _ mcpdiscovery.Request) ([]mcpdiscovery.Tool, error) {
		return nil, &mcpdiscovery.StageError{Stage: "initialize", Err: &mcpdiscovery.HTTPStatusError{StatusCode: http.StatusNotFound}}
	}
	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/mcp/discover", map[string]any{
		"original_name": "knowledge", "name": "knowledge", "url": "https://mcp.example.com/v1", "auth_type": "bearer",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusFailedDependency)
	failedDiscoveryBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(failedDiscoveryBody), bearerToken) ||
		!strings.Contains(string(failedDiscoveryBody), `"stage":"Initialize MCP session"`) ||
		!strings.Contains(string(failedDiscoveryBody), `"retryable":true`) ||
		!strings.Contains(string(failedDiscoveryBody), `Replacing the token will not fix HTTP 404`) {
		t.Fatalf("unexpected MCP discovery failure: %s", failedDiscoveryBody)
	}
	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/mcp/discover", map[string]any{
		"original_name": "knowledge", "name": "knowledge", "url": "https://untrusted.example.com/mcp", "auth_type": "bearer",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)
	changedEndpointBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if discoveryCalls != 1 || strings.Contains(string(changedEndpointBody), bearerToken) {
		t.Fatalf("stored bearer token was reused for a changed endpoint: %s", changedEndpointBody)
	}

	response = environment.request(
		t, http.MethodGet, "/api/v1/agent/jobs/"+job.ID+"/mcp-config", nil, hostToken,
		map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken},
	)
	assertStatus(t, response, http.StatusOK)
	leasedBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(leasedBody), bearerToken) || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("leased MCP response did not contain the protected configuration: %s", leasedBody)
	}

	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, InstanceStatus: domain.InstanceRunning,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)
	record, err = environment.dataStore.GetMCPConfig(context.Background(), instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "APPLIED" || record.AppliedRevision != record.DesiredRevision || record.AppliedAt == nil {
		t.Fatalf("MCP configuration was not marked applied: %+v", record)
	}
}

type apiTestEnvironment struct {
	handler         http.Handler
	server          *Server
	adminToken      string
	enrollmentToken string
	databasePath    string
	dataStore       *store.Store
	recoveryPoints  *recovery.Manager
	chatArtifacts   *chatartifacts.Manager
}

func newAPITestEnvironment(t *testing.T) *apiTestEnvironment {
	t.Helper()
	root := t.TempDir()
	dataStore, err := store.Open(filepath.Join(root, "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	backupManager, err := backup.New(filepath.Join(root, "backups"), dataStore, 20)
	if err != nil {
		t.Fatal(err)
	}
	recoveryManager, err := recovery.New(filepath.Join(root, "recovery-points"), strings.Repeat("02", 32), 20, 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	chatArtifactManager, err := chatartifacts.New(filepath.Join(root, "chat-artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := security.NewSealer(strings.Repeat("01", 32))
	if err != nil {
		t.Fatal(err)
	}
	environment := &apiTestEnvironment{
		adminToken: strings.Repeat("a", 32), enrollmentToken: strings.Repeat("e", 32),
		databasePath: filepath.Join(root, "fleet.db"), dataStore: dataStore, recoveryPoints: recoveryManager,
		chatArtifacts: chatArtifactManager,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := New(Config{
		AdminToken: environment.adminToken, EnrollmentToken: environment.enrollmentToken,
		Address: "127.0.0.1:9180", OperatorURL: "http://127.0.0.1:9180", DatabasePath: filepath.Join(root, "fleet.db"), BackupRetention: 20,
		HermesCatalog: testHermesCatalog(), HermesReleaseSource: staticReleaseSource{catalog: testHermesCatalog()},
		OfflineAfter: 30 * time.Second, Sealer: sealer, Backups: backupManager,
		RecoveryPoints: recoveryManager, ChatArtifacts: chatArtifactManager, MaxRecoveryPointBytes: 10 << 20,
	}, dataStore, logger)
	environment.server = application
	environment.handler = application.Handler()
	t.Cleanup(func() {
		_ = dataStore.Close()
	})
	return environment
}

type staticReleaseSource struct {
	catalog releases.Catalog
	err     error
}

func (source staticReleaseSource) List(context.Context, int) (releases.Catalog, error) {
	return source.catalog, source.err
}

func testHermesCatalog() releases.Catalog {
	checkedAt := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	return releases.Catalog{
		Source: "NousResearch/hermes-agent GitHub Releases", CheckedAt: checkedAt,
		Releases: []releases.Release{
			{Version: "0.19.0", Tag: "v2026.8.1", Commit: "8bcdef6ef2bcbaa22bd23b72efe60906123a4f66", Image: "local/hermes-fleet-runtime:0.19.0-8bcdef6ef2bc", URL: "https://github.com/NousResearch/hermes-agent/releases/tag/v2026.8.1", PublishedAt: checkedAt},
			{Version: "0.18.2", Tag: "v2026.7.7.2", Commit: "7acaff5ef2bcbaa22bd23b72efe60906123a4f55", Image: "local/hermes-fleet-runtime:0.18.2-7acaff5ef2bc", URL: "https://github.com/NousResearch/hermes-agent/releases/tag/v2026.7.7.2", PublishedAt: checkedAt.Add(-24 * time.Hour)},
			{Version: "0.18.1", Tag: "v2026.7.7", Commit: "6acaff5ef2bcbaa22bd23b72efe60906123a4f44", Image: "local/hermes-fleet-runtime:0.18.1-6acaff5ef2bc", URL: "https://github.com/NousResearch/hermes-agent/releases/tag/v2026.7.7", PublishedAt: checkedAt.Add(-48 * time.Hour)},
		},
	}
}

func (environment *apiTestEnvironment) request(t *testing.T, method, path string, payload any, token string, headers map[string]string) *http.Response {
	t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, body)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	environment.handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

func (environment *apiTestEnvironment) concurrentAdminRequest(method, path string, payload any) *http.Response {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			panic(err)
		}
		body = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, body)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+environment.adminToken)
	recorder := httptest.NewRecorder()
	environment.handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

func (environment *apiTestEnvironment) rawRequest(t *testing.T, method, path string, body io.Reader, token string, headers map[string]string) *http.Response {
	t.Helper()
	request := httptest.NewRequest(method, path, body)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	environment.handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

func recoveryArchiveForAPI(t *testing.T, metadata recovery.Metadata) []byte {
	t.Helper()
	manifest := recovery.Manifest{
		FormatVersion: 1, RecoveryPointID: metadata.ID, InstanceID: metadata.InstanceID, InstanceName: metadata.InstanceName,
		Image: metadata.Image, ImageID: metadata.ImageID, Provider: metadata.Provider, Model: metadata.Model,
		Reasoning: metadata.Reasoning, ServiceTier: metadata.ServiceTier, ProjectName: metadata.ProjectName,
		DataVolume: metadata.DataVolume, ManagedPath: metadata.ManagedPath, AgentVersion: metadata.AgentVersion,
		CreatedAt: metadata.CreatedAt,
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	archive := tar.NewWriter(&buffer)
	for name, data := range map[string][]byte{
		"manifest.json": manifestData, "workspace/.env": []byte("secret"), "data-volume.tar": []byte("volume"),
	} {
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func (environment *apiTestEnvironment) enrollHost(t *testing.T) (string, string) {
	return environment.enrollHostNamed(t, "local-test")
}

func (environment *apiTestEnvironment) enrollHostNamed(t *testing.T, name string) (string, string) {
	t.Helper()
	response := environment.request(t, http.MethodPost, "/api/v1/agent/enroll", map[string]string{
		"enrollment_token": environment.enrollmentToken, "name": name, "hostname": "host",
		"os": "darwin", "arch": "arm64", "agent_version": agentVersion,
	}, "", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("enroll host status=%d", response.StatusCode)
	}
	var enrollment struct {
		HostID    string `json:"host_id"`
		HostToken string `json:"host_token"`
	}
	decodeResponse(t, response, &enrollment)
	return enrollment.HostID, enrollment.HostToken
}

func (environment *apiTestEnvironment) overviewInstance(t *testing.T, instanceID string) domain.Instance {
	t.Helper()
	response := environment.request(t, http.MethodGet, "/api/v1/overview", nil, environment.adminToken, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("overview status=%d", response.StatusCode)
	}
	var overview struct {
		Instances []domain.Instance `json:"instances"`
	}
	decodeResponse(t, response, &overview)
	for _, instance := range overview.Instances {
		if instance.ID == instanceID {
			return instance
		}
	}
	t.Fatalf("instance %s was not returned by overview", instanceID)
	return domain.Instance{}
}

func (environment *apiTestEnvironment) claimAndAcknowledge(t *testing.T, hostID, hostToken string) domain.Job {
	t.Helper()
	response := environment.request(t, http.MethodPost, "/api/v1/agent/jobs/claim", map[string]any{}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("claim job status=%d", response.StatusCode)
	}
	var job domain.Job
	decodeResponse(t, response, &job)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/ack", map[string]any{}, hostToken, map[string]string{
		"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken,
	})
	assertStatus(t, response, http.StatusNoContent)
	return job
}

func assertStatus(t *testing.T, response *http.Response, expected int) {
	t.Helper()
	defer response.Body.Close()
	if response.StatusCode != expected {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d want=%d body=%s", response.StatusCode, expected, body)
	}
}

func decodeResponse(t *testing.T, response *http.Response, target any) {
	t.Helper()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatal(err)
	}
}
