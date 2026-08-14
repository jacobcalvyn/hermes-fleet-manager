package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func TestLifecycleCompletionCannotRewriteProvisionedImageID(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	instance, oldImageID := provisionAPIInstance(t, environment, hostID, hostToken, "fleet-image-scope")

	response := environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/actions", map[string]string{"action": "stop"}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)
	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	newImageID := "sha256:" + strings.Repeat("b", 64)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ImageID: newImageID,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusConflict)
	stored := environment.overviewInstance(t, instance.ID)
	if stored.ImageID != oldImageID || stored.Status != domain.InstanceProvisioning {
		t.Fatalf("unauthorized lifecycle metadata was accepted: %+v", stored)
	}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)
}

func TestReconcileImageActionIsStoppedConfirmedAndLeaseFenced(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	instance, oldImageID := provisionAPIInstance(t, environment, hostID, hostToken, "fleet-image-reconcile")
	path := "/api/v1/instances/" + instance.ID + "/actions"

	response := environment.request(t, http.MethodPost, path, map[string]string{
		"action": "reconcile-image", "confirm_name": instance.Name,
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusConflict)

	response = environment.request(t, http.MethodPost, path, map[string]string{"action": "stop"}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)
	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{Success: true}, hostToken, map[string]string{
		"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken,
	})
	assertStatus(t, response, http.StatusNoContent)

	response = environment.request(t, http.MethodPost, path, map[string]string{
		"action": "reconcile-image", "confirm_name": "wrong-name",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)
	if err := environment.dataStore.Heartbeat(context.Background(), hostID, "host", "darwin", "arm64", "0.6.0", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	response = environment.request(t, http.MethodPost, path, map[string]string{
		"action": "reconcile-image", "confirm_name": instance.Name,
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusConflict)
	if err := environment.dataStore.Heartbeat(context.Background(), hostID, "host", "darwin", "arm64", agentVersion, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	response = environment.request(t, http.MethodPost, path, map[string]string{
		"action": "reconcile-image", "confirm_name": instance.Name,
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("reconcile image status=%d body=%s", response.StatusCode, body)
	}
	response.Body.Close()
	stored := environment.overviewInstance(t, instance.ID)
	if stored.Status != domain.InstanceReconciling || stored.ImageID != oldImageID {
		t.Fatalf("instance was not fenced without changing image metadata: %+v", stored)
	}

	job = environment.claimAndAcknowledge(t, hostID, hostToken)
	if job.Type != "instance.image.reconcile" {
		t.Fatalf("claimed job type=%q", job.Type)
	}
	var payload domain.ImageReconcilePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.PreviousImageID != oldImageID || payload.Image != instance.Image || payload.ProjectName == "" || payload.DataVolume == "" {
		t.Fatalf("reconcile payload=%+v", payload)
	}
	newImageID := "sha256:" + strings.Repeat("b", 64)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ImageID: "invalid", InstanceStatus: domain.InstanceStopped,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusConflict)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ImageID: newImageID, InstanceStatus: domain.InstanceStopped,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)
	stored = environment.overviewInstance(t, instance.ID)
	if stored.Status != domain.InstanceStopped || stored.ImageID != newImageID {
		t.Fatalf("verified reconcile result was not recorded: %+v", stored)
	}
}

func TestFixImageDriftActionRequiresCurrentDriftAndRestoresRunningState(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	instance, oldImageID := provisionAPIInstance(t, environment, hostID, hostToken, "fleet-image-fix")
	path := "/api/v1/instances/" + instance.ID + "/actions"

	response := environment.request(t, http.MethodPost, path, map[string]string{
		"action": "fix-image-drift", "confirm_name": instance.Name,
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusConflict)

	targets, err := environment.dataStore.ListObservationTargets(context.Background(), hostID)
	if err != nil || len(targets) != 1 {
		t.Fatalf("ListObservationTargets() targets=%+v error=%v", targets, err)
	}
	now := time.Now().UTC()
	observation := domain.InstanceObservation{
		InstanceID: instance.ID, TargetGeneration: targets[0].Generation,
		Status: domain.ObservationDegraded, Summary: "Runtime drift detected",
		Checks: []domain.ObservationCheck{{
			Name: "image", Status: domain.ObservationCheckDrift,
			Detail: "Container images differ from the provisioned image",
		}},
		ObservedAt: now,
	}
	if err := environment.dataStore.RecordObservations(context.Background(), hostID, []domain.InstanceObservation{observation}, now); err != nil {
		t.Fatal(err)
	}

	response = environment.request(t, http.MethodPost, path, map[string]string{
		"action": "fix-image-drift", "confirm_name": "wrong-name",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)
	response = environment.request(t, http.MethodPost, path, map[string]string{
		"action": "fix-image-drift", "confirm_name": instance.Name,
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)

	stored := environment.overviewInstance(t, instance.ID)
	if stored.Status != domain.InstanceReconciling || stored.ImageID != oldImageID {
		t.Fatalf("instance was not fenced before image repair: %+v", stored)
	}
	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	if job.Type != "instance.image.repair" {
		t.Fatalf("claimed job type=%q", job.Type)
	}
	var payload domain.ImageRepairPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Restart || payload.APIPort != instance.APIPort || payload.PreviousImageID != oldImageID {
		t.Fatalf("image repair payload=%+v", payload)
	}
	newImageID := "sha256:" + strings.Repeat("b", 64)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ImageID: newImageID, InstanceStatus: domain.InstanceStopped,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusConflict)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ImageID: newImageID, InstanceStatus: domain.InstanceRunning,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)
	stored = environment.overviewInstance(t, instance.ID)
	if stored.Status != domain.InstanceRunning || stored.ImageID != newImageID {
		t.Fatalf("successful automatic repair was not recorded: %+v", stored)
	}
}

func TestSyncRuntimeActionRequiresCurrentDriftAndPreservesRunningState(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	instance, oldImageID := provisionAPIInstance(t, environment, hostID, hostToken, "fleet-runtime-sync")
	path := "/api/v1/instances/" + instance.ID + "/actions"

	response := environment.request(t, http.MethodPost, path, map[string]string{
		"action": "sync-runtime", "confirm_name": instance.Name,
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusConflict)

	targets, err := environment.dataStore.ListObservationTargets(context.Background(), hostID)
	if err != nil || len(targets) != 1 {
		t.Fatalf("ListObservationTargets() targets=%+v error=%v", targets, err)
	}
	if targets[0].Provider != instance.Provider || targets[0].Model != instance.Model {
		t.Fatalf("runtime configuration missing from observation target: %+v", targets[0])
	}
	now := time.Now().UTC()
	observation := domain.InstanceObservation{
		InstanceID: instance.ID, TargetGeneration: targets[0].Generation,
		Status: domain.ObservationDegraded, Summary: "Runtime drift detected",
		Checks: []domain.ObservationCheck{{
			Name: "runtime_configuration", Status: domain.ObservationCheckDrift,
			Detail: "Hermes has not applied the Fleet provider and model",
		}},
		ObservedAt: now,
	}
	if err := environment.dataStore.RecordObservations(context.Background(), hostID, []domain.InstanceObservation{observation}, now); err != nil {
		t.Fatal(err)
	}

	response = environment.request(t, http.MethodPost, path, map[string]string{
		"action": "sync-runtime", "confirm_name": "wrong-name",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)
	response = environment.request(t, http.MethodPost, path, map[string]string{
		"action": "sync-runtime", "confirm_name": instance.Name,
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)

	stored := environment.overviewInstance(t, instance.ID)
	if stored.Status != domain.InstanceUpdating || stored.ImageID != oldImageID {
		t.Fatalf("instance was not fenced before runtime synchronization: %+v", stored)
	}
	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	if job.Type != "instance.runtime.sync" {
		t.Fatalf("claimed job type=%q", job.Type)
	}
	var payload domain.RuntimeSyncPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.DesiredStatus != domain.InstanceRunning || payload.Provider != instance.Provider || payload.Model != instance.Model || payload.ImageID != oldImageID {
		t.Fatalf("runtime synchronization payload=%+v", payload)
	}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, InstanceStatus: domain.InstanceStopped,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusConflict)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, InstanceStatus: domain.InstanceRunning,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)
	stored = environment.overviewInstance(t, instance.ID)
	if stored.Status != domain.InstanceRunning || stored.ImageID != oldImageID {
		t.Fatalf("successful runtime synchronization changed runtime identity: %+v", stored)
	}
}

func TestRepairRuntimeActionRequiresCurrentDriftAndVerifiesManagedRestart(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	instance, oldImageID := provisionAPIInstance(t, environment, hostID, hostToken, "fleet-runtime-repair")
	path := "/api/v1/instances/" + instance.ID + "/actions"

	response := environment.request(t, http.MethodPost, path, map[string]string{
		"action": "repair-runtime", "confirm_name": instance.Name,
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusConflict)

	targets, err := environment.dataStore.ListObservationTargets(context.Background(), hostID)
	if err != nil || len(targets) != 1 {
		t.Fatalf("ListObservationTargets() targets=%+v error=%v", targets, err)
	}
	now := time.Now().UTC()
	observation := domain.InstanceObservation{
		InstanceID: instance.ID, TargetGeneration: targets[0].Generation,
		Status: domain.ObservationDegraded, Summary: "Runtime drift detected",
		Checks:     repairableRuntimeChecks(),
		ObservedAt: now,
	}
	if err := environment.dataStore.RecordObservations(context.Background(), hostID, []domain.InstanceObservation{observation}, now); err != nil {
		t.Fatal(err)
	}

	response = environment.request(t, http.MethodPost, path, map[string]string{
		"action": "repair-runtime", "confirm_name": "wrong-name",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)

	response = environment.request(t, http.MethodPost, path, map[string]string{
		"action": "repair-runtime", "confirm_name": instance.Name,
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)

	stored := environment.overviewInstance(t, instance.ID)
	if stored.Status != domain.InstanceRestarting || stored.ImageID != oldImageID {
		t.Fatalf("instance was not fenced before runtime repair: %+v", stored)
	}
	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	if job.Type != "instance.runtime.repair" {
		t.Fatalf("claimed job type=%q", job.Type)
	}
	var payload domain.RuntimeRepairPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.InstanceID != instance.ID || payload.Name != instance.Name || payload.ImageID != oldImageID ||
		payload.ProjectName == "" || payload.ManagedPath == "" || payload.APIPort != instance.APIPort ||
		payload.Phase != 1 || payload.Attempt != 1 || payload.Trigger != "manual" {
		t.Fatalf("runtime repair payload=%+v", payload)
	}

	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)
	stored = environment.overviewInstance(t, instance.ID)
	if stored.Status != domain.InstanceRunning || stored.ImageID != oldImageID || stored.LastError != "" {
		t.Fatalf("successful runtime repair was not recorded: %+v", stored)
	}
}

func TestRuntimeDriftQueuesBoundedAutomaticRepairAndFailureRemainsRetryable(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	instance, oldImageID := provisionAPIInstance(t, environment, hostID, hostToken, "fleet-runtime-auto-repair")
	targets, err := environment.dataStore.ListObservationTargets(context.Background(), hostID)
	if err != nil || len(targets) != 1 {
		t.Fatalf("ListObservationTargets() targets=%+v error=%v", targets, err)
	}
	now := time.Now().UTC()
	observation := domain.InstanceObservation{
		InstanceID: instance.ID, TargetGeneration: targets[0].Generation,
		Status: domain.ObservationDegraded, Summary: "Runtime drift detected",
		Checks:     repairableRuntimeChecks(),
		ObservedAt: now,
	}
	headers := map[string]string{"X-Fleet-Host-ID": hostID}
	response := environment.request(
		t, http.MethodPost, "/api/v1/agent/observations",
		map[string]any{"observations": []domain.InstanceObservation{observation}}, hostToken, headers,
	)
	assertStatus(t, response, http.StatusNoContent)
	stored := environment.overviewInstance(t, instance.ID)
	if stored.Status != domain.InstanceRunning || stored.RuntimeRemediation == nil ||
		stored.RuntimeRemediation.Status != "MONITORING" || stored.RuntimeRemediation.TotalAttempts != 0 {
		t.Fatalf("first drift should only arm remediation: %+v", stored)
	}

	observation.ObservedAt = now.Add(time.Second)
	response = environment.request(
		t, http.MethodPost, "/api/v1/agent/observations",
		map[string]any{"observations": []domain.InstanceObservation{observation}}, hostToken, headers,
	)
	assertStatus(t, response, http.StatusNoContent)
	stored = environment.overviewInstance(t, instance.ID)
	if stored.Status != domain.InstanceRestarting || stored.ImageID != oldImageID ||
		stored.RuntimeRemediation == nil || stored.RuntimeRemediation.TotalAttempts != 1 ||
		stored.RuntimeRemediation.Status != "QUEUED" {
		t.Fatalf("second drift did not queue bounded remediation: %+v", stored)
	}

	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	if job.Type != "instance.runtime.repair" {
		t.Fatalf("automatic repair job type=%q", job.Type)
	}
	var repairPayload domain.RuntimeRepairPayload
	if err := json.Unmarshal(job.Payload, &repairPayload); err != nil {
		t.Fatal(err)
	}
	if repairPayload.Phase != 1 || repairPayload.Attempt != 1 || repairPayload.Trigger != "automatic" {
		t.Fatalf("automatic runtime repair payload=%+v", repairPayload)
	}
	response = environment.request(
		t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete",
		domain.JobResult{Success: false, Error: "managed services did not become healthy"},
		hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken},
	)
	assertStatus(t, response, http.StatusNoContent)
	stored = environment.overviewInstance(t, instance.ID)
	if stored.Status != domain.InstanceRunning || stored.RuntimeRemediation == nil ||
		stored.RuntimeRemediation.Status != "WAITING" ||
		stored.RuntimeRemediation.LastError != "managed services did not become healthy" {
		t.Fatalf("failed automatic repair was not left retryable: %+v", stored)
	}
}

func repairableRuntimeChecks() []domain.ObservationCheck {
	return []domain.ObservationCheck{
		{Name: "managed_path", Status: domain.ObservationCheckOK, Detail: "Managed path exists"},
		{Name: "manifest", Status: domain.ObservationCheckOK, Detail: "Manifest exists"},
		{Name: "environment", Status: domain.ObservationCheckOK, Detail: "Environment exists"},
		{Name: "workspace", Status: domain.ObservationCheckOK, Detail: "Workspace exists"},
		{Name: "docker_daemon", Status: domain.ObservationCheckOK, Detail: "Docker daemon responded"},
		{Name: "data_volume", Status: domain.ObservationCheckOK, Detail: "Data volume exists"},
		{
			Name: "runtime", Status: domain.ObservationCheckDrift,
			Detail: "Desired RUNNING state does not match container state or health",
		},
	}
}

func provisionAPIInstance(t *testing.T, environment *apiTestEnvironment, hostID, hostToken, name string) (domain.Instance, string) {
	t.Helper()
	instance, oldImageID := provisionPendingAPIInstance(t, environment, hostID, hostToken, name)
	targets, err := environment.dataStore.ListObservationTargets(context.Background(), hostID)
	if err != nil || len(targets) != 1 {
		t.Fatalf("observation targets=%+v error=%v", targets, err)
	}
	report := domain.InstanceObservation{
		InstanceID: instance.ID, TargetGeneration: targets[0].Generation,
		Status: domain.ObservationDegraded, Summary: "Codex configuration is required",
		ModelCatalog: []string{"gpt-5.6-sol", "gpt-5.6-terra"}, RecommendedModel: "gpt-5.6-sol",
		Checks: []domain.ObservationCheck{
			{Name: "codex_auth", Status: domain.ObservationCheckOK, Detail: "Codex authentication is connected"},
			{Name: "runtime_configuration", Status: domain.ObservationCheckDrift, Detail: "Codex configuration has not been saved in Hermes Fleet"},
		},
		ObservedAt: time.Now().UTC(),
	}
	response := environment.request(t, http.MethodPost, "/api/v1/agent/observations", map[string]any{
		"observations": []domain.InstanceObservation{report},
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID})
	assertStatus(t, response, http.StatusNoContent)
	response.Body.Close()
	response = environment.request(t, http.MethodPut, "/api/v1/instances/"+instance.ID+"/codex-configuration", map[string]string{
		"model": "gpt-5.6-sol", "reasoning": "medium", "service_tier": "normal",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)
	response.Body.Close()
	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	if job.Type != "instance.runtime.configure" {
		t.Fatalf("configuration job type=%q", job.Type)
	}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, InstanceStatus: domain.InstanceRunning,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)
	response.Body.Close()
	return environment.overviewInstance(t, instance.ID), oldImageID
}

func provisionPendingAPIInstance(t *testing.T, environment *apiTestEnvironment, hostID, hostToken, name string) (domain.Instance, string) {
	t.Helper()
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
	oldImageID := "sha256:" + strings.Repeat("a", 64)
	response = environment.request(t, http.MethodPost, "/api/v1/agent/jobs/"+job.ID+"/complete", domain.JobResult{
		Success: true, ProjectName: projectName, DataVolume: projectName + "-data",
		ManagedPath: "/managed/" + instance.Name + "-" + shortID, ImageID: oldImageID,
	}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID, leaseTokenHeader: job.LeaseToken})
	assertStatus(t, response, http.StatusNoContent)
	return environment.overviewInstance(t, instance.ID), oldImageID
}

func TestUnconfiguredCodexDriftDoesNotQueueRuntimeSync(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	instance, _ := provisionPendingAPIInstance(t, environment, hostID, hostToken, "fleet-runtime-pending")
	targets, err := environment.dataStore.ListObservationTargets(context.Background(), hostID)
	if err != nil || len(targets) != 1 || targets[0].CodexConfigured {
		t.Fatalf("pending observation target=%+v error=%v", targets, err)
	}
	report := domain.InstanceObservation{
		InstanceID: instance.ID, TargetGeneration: targets[0].Generation,
		Status: domain.ObservationDegraded, Summary: "Codex configuration is required",
		Checks: []domain.ObservationCheck{{
			Name: "runtime_configuration", Status: domain.ObservationCheckDrift,
			Detail: "Codex configuration has not been saved in Hermes Fleet",
		}},
		ObservedAt: time.Now().UTC(),
	}
	path := "/api/v1/agent/observations"
	headers := map[string]string{"X-Fleet-Host-ID": hostID}
	for attempt := 0; attempt < 2; attempt++ {
		response := environment.request(t, http.MethodPost, path, map[string]any{"observations": []domain.InstanceObservation{report}}, hostToken, headers)
		assertStatus(t, response, http.StatusNoContent)
		response.Body.Close()
		report.ObservedAt = report.ObservedAt.Add(time.Second)
	}
	stored := environment.overviewInstance(t, instance.ID)
	if stored.Status != domain.InstanceRunning || stored.LastError != "" {
		t.Fatalf("pending Codex configuration changed runtime state: %+v", stored)
	}
	job, err := environment.dataStore.ClaimJob(context.Background(), hostID, time.Minute)
	if err != nil || job != nil {
		t.Fatalf("pending Codex configuration queued job=%+v error=%v", job, err)
	}
	response := environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/actions", map[string]string{
		"action": "sync-runtime", "confirm_name": instance.Name,
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusConflict)
	response.Body.Close()
}

func TestRuntimeConfigurationDriftQueuesFencedAutomaticSync(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	instance, _ := provisionAPIInstance(t, environment, hostID, hostToken, "fleet-runtime-auto")
	targets, err := environment.dataStore.ListObservationTargets(context.Background(), hostID)
	if err != nil || len(targets) != 1 {
		t.Fatalf("observation targets=%+v error=%v", targets, err)
	}
	report := domain.InstanceObservation{
		InstanceID: instance.ID, TargetGeneration: targets[0].Generation,
		Status: domain.ObservationDegraded, Summary: "Runtime drift detected",
		Checks: []domain.ObservationCheck{{
			Name: "runtime_configuration", Status: domain.ObservationCheckDrift, Detail: "Stale Fleet readiness marker",
		}},
		ObservedAt: time.Now().UTC(),
	}
	path := "/api/v1/agent/observations"
	headers := map[string]string{"X-Fleet-Host-ID": hostID}
	response := environment.request(t, http.MethodPost, path, map[string]any{"observations": []domain.InstanceObservation{report}}, hostToken, headers)
	assertStatus(t, response, http.StatusNoContent)
	if stored := environment.overviewInstance(t, instance.ID); stored.Status != domain.InstanceRunning {
		t.Fatalf("first drift changed instance state: %+v", stored)
	}
	report.ObservedAt = report.ObservedAt.Add(time.Second)
	response = environment.request(t, http.MethodPost, path, map[string]any{"observations": []domain.InstanceObservation{report}}, hostToken, headers)
	assertStatus(t, response, http.StatusNoContent)
	if stored := environment.overviewInstance(t, instance.ID); stored.Status != domain.InstanceUpdating {
		t.Fatalf("second drift did not fence automatic synchronization: %+v", stored)
	}
	job := environment.claimAndAcknowledge(t, hostID, hostToken)
	if job.Type != "instance.runtime.sync" || job.InstanceID != instance.ID {
		t.Fatalf("automatic synchronization job=%+v", job)
	}
	var payload domain.RuntimeSyncPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.DesiredStatus != domain.InstanceRunning || payload.Provider != instance.Provider ||
		payload.Model != instance.Model || payload.DashboardPort != instance.DashboardPort {
		t.Fatalf("automatic synchronization payload=%+v", payload)
	}
}

func TestRuntimeRefreshBlocksManualAndAutomaticConfigurationSync(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	instance, _ := provisionAPIInstance(t, environment, hostID, hostToken, "fleet-runtime-refresh")

	catalog := testHermesCatalog()
	latest := catalog.Releases[0]
	oldWrapperImage := latest.Image + "-111111111111"
	catalog.Releases[0].Image = latest.Image + "-222222222222"
	environment.server.config.HermesCatalog = catalog
	environment.server.config.HermesReleaseSource = staticReleaseSource{catalog: catalog}

	database, err := sql.Open("sqlite", environment.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`UPDATE instances SET image = ? WHERE id = ?`, oldWrapperImage, instance.ID); err != nil {
		t.Fatal(err)
	}

	targets, err := environment.dataStore.ListObservationTargets(context.Background(), hostID)
	if err != nil || len(targets) != 1 {
		t.Fatalf("observation targets=%+v error=%v", targets, err)
	}
	report := domain.InstanceObservation{
		InstanceID: instance.ID, TargetGeneration: targets[0].Generation,
		HermesVersion: latest.Version, HermesSource: latest.Commit,
		Status: domain.ObservationDegraded, Summary: "Runtime configuration drift",
		Checks: []domain.ObservationCheck{
			{Name: "codex_auth", Status: domain.ObservationCheckOK, Detail: "Codex authentication is connected"},
			{Name: "runtime_configuration", Status: domain.ObservationCheckDrift, Detail: "Stale Fleet readiness marker"},
		},
		ObservedAt: time.Now().UTC(),
	}
	path := "/api/v1/agent/observations"
	headers := map[string]string{"X-Fleet-Host-ID": hostID}
	for attempt := 0; attempt < 2; attempt++ {
		response := environment.request(t, http.MethodPost, path, map[string]any{
			"observations": []domain.InstanceObservation{report},
		}, hostToken, headers)
		assertStatus(t, response, http.StatusNoContent)
		response.Body.Close()
		report.ObservedAt = report.ObservedAt.Add(time.Second)
	}

	job, err := environment.dataStore.ClaimJob(context.Background(), hostID, time.Minute)
	if err != nil || job != nil {
		t.Fatalf("runtime refresh requirement queued automatic synchronization job=%+v error=%v", job, err)
	}
	response := environment.request(t, http.MethodPost, "/api/v1/instances/"+instance.ID+"/actions", map[string]string{
		"action": "sync-runtime", "confirm_name": instance.Name,
	}, environment.adminToken, nil)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusConflict ||
		!strings.Contains(string(body), "refresh the managed runtime before synchronizing runtime configuration") {
		t.Fatalf("manual synchronization status=%d body=%s", response.StatusCode, body)
	}
}
