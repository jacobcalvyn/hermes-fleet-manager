package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func TestControlledPolicyRolloutCanaryPauseResumeAndCancel(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	canary := environment.provisionRolloutInstance(t, hostID, hostToken, "rollout-canary")
	wave := environment.provisionRolloutInstance(t, hostID, hostToken, "rollout-wave")
	policy := environment.createControlledPolicy(t, []string{canary.ID, wave.ID})

	response := environment.request(t, http.MethodPost, "/api/v1/policies/"+policy.ID+"/rollouts", map[string]any{}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("start rollout status=%d body=%s", response.StatusCode, body)
	}
	var rollout domain.Operation
	decodeResponse(t, response, &rollout)
	response.Body.Close()
	metadata, err := decodePolicyRolloutMetadata(rollout)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.CanaryInstanceID != canary.ID || metadata.TargetVersion == "" || metadata.TargetImage == "" || metadata.TargetSource == "" {
		t.Fatalf("rollout snapshot=%+v", metadata)
	}
	assertPolicyRolloutTargetCounts(t, environment, rollout.ID, 1, 1, 0, 0)

	response = environment.request(t, http.MethodPost, "/api/v1/rollouts/"+rollout.ID+"/pause", map[string]any{}, environment.adminToken, nil)
	var view policyRolloutView
	decodeResponse(t, response, &view)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || view.ControlState != domain.PolicyRolloutControlPaused {
		t.Fatalf("paused rollout status=%d view=%+v", response.StatusCode, view)
	}
	assertPolicyRolloutTargetCounts(t, environment, rollout.ID, 1, 1, 0, 0)

	response = environment.request(t, http.MethodPost, "/api/v1/rollouts/"+rollout.ID+"/resume", map[string]any{}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusOK)
	targets, err := environment.dataStore.ListPolicyRolloutTargets(context.Background(), rollout.ID)
	if err != nil {
		t.Fatal(err)
	}
	canaryTarget := policyRolloutTarget(targets, canary.ID)
	if canaryTarget == nil || canaryTarget.ChildOperationID == "" {
		t.Fatalf("canary target=%+v", canaryTarget)
	}
	if err := environment.dataStore.UpdateControlPlaneOperation(context.Background(), canaryTarget.ChildOperationID,
		domain.OperationFailed, domain.JobProgress{Stage: "rollback_complete"}, "canary health gate failed; rollback completed", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	rollout, _ = environment.dataStore.GetOperation(context.Background(), rollout.ID)
	environment.server.reconcilePolicyRollout(context.Background(), rollout)
	view = environment.getPolicyRollout(t, rollout.ID)
	if view.ControlState != domain.PolicyRolloutControlPaused || !strings.Contains(view.ControlReason, "stopped before the next wave") {
		t.Fatalf("automatic pause=%+v", view)
	}
	assertPolicyRolloutTargetCounts(t, environment, rollout.ID, 1, 0, 0, 1)

	response = environment.request(t, http.MethodPost, "/api/v1/rollouts/"+rollout.ID+"/resume", map[string]any{}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusOK)
	assertPolicyRolloutTargetCounts(t, environment, rollout.ID, 0, 1, 0, 1)

	response = environment.request(t, http.MethodPost, "/api/v1/rollouts/"+rollout.ID+"/cancel", map[string]any{}, environment.adminToken, nil)
	decodeResponse(t, response, &view)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || view.ControlState != domain.PolicyRolloutControlCanceling {
		t.Fatalf("canceling rollout status=%d view=%+v", response.StatusCode, view)
	}
	targets, _ = environment.dataStore.ListPolicyRolloutTargets(context.Background(), rollout.ID)
	waveTarget := policyRolloutTarget(targets, wave.ID)
	if waveTarget == nil || waveTarget.ChildOperationID == "" {
		t.Fatalf("wave target=%+v", waveTarget)
	}
	if err := environment.dataStore.UpdateControlPlaneOperation(context.Background(), waveTarget.ChildOperationID,
		domain.OperationSucceeded, domain.JobProgress{Stage: "health_checked"}, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	rollout, _ = environment.dataStore.GetOperation(context.Background(), rollout.ID)
	environment.server.reconcilePolicyRollout(context.Background(), rollout)
	finished, err := environment.dataStore.GetOperation(context.Background(), rollout.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != domain.OperationFailed || finished.Progress == nil || finished.Progress.Stage != "rollout_canceled" {
		t.Fatalf("finished canceled rollout=%+v", finished)
	}
}

func TestPolicyRejectsUnboundedAllAtOnceRolloutStrategy(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	instance := environment.provisionRolloutInstance(t, hostID, hostToken, "unsafe-rollout")
	response := environment.request(t, http.MethodPost, "/api/v1/policies", map[string]any{
		"name": "Unsafe rollout", "status": "ENABLED", "desired_hermes": "LATEST_STABLE",
		"strategy": "ALL_AT_ONCE", "scope_instance_ids": []string{instance.ID},
	}, environment.adminToken, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("all-at-once policy status=%d body=%s", response.StatusCode, body)
	}
}

func (environment *apiTestEnvironment) createControlledPolicy(t *testing.T, instanceIDs []string) domain.FleetPolicy {
	t.Helper()
	response := environment.request(t, http.MethodPost, "/api/v1/policies", map[string]any{
		"name": "Controlled stable", "status": "ENABLED", "desired_hermes": "LATEST_STABLE",
		"strategy": "ONE_AT_A_TIME", "scope_instance_ids": instanceIDs,
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create controlled policy status=%d body=%s", response.StatusCode, body)
	}
	var policy domain.FleetPolicy
	decodeResponse(t, response, &policy)
	response.Body.Close()
	return policy
}

func (environment *apiTestEnvironment) provisionRolloutInstance(t *testing.T, hostID, hostToken, name string) domain.Instance {
	t.Helper()
	response := environment.request(t, http.MethodPost, "/api/v1/instances", createInstanceRequest{
		Name: name, HostID: hostID, HermesVersion: "0.18.1",
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create rollout instance status=%d body=%s", response.StatusCode, body)
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
	return environment.overviewInstance(t, instance.ID)
}

func (environment *apiTestEnvironment) getPolicyRollout(t *testing.T, rolloutID string) policyRolloutView {
	t.Helper()
	response := environment.request(t, http.MethodGet, "/api/v1/rollouts/"+rolloutID, nil, environment.adminToken, nil)
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("get policy rollout status=%d body=%s", response.StatusCode, body)
	}
	var view policyRolloutView
	decodeResponse(t, response, &view)
	response.Body.Close()
	return view
}

func assertPolicyRolloutTargetCounts(t *testing.T, environment *apiTestEnvironment, rolloutID string, pending, running, succeeded, failed int) {
	t.Helper()
	targets, err := environment.dataStore.ListPolicyRolloutTargets(context.Background(), rolloutID)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, target := range targets {
		counts[target.Status]++
	}
	actualFailed := counts[domain.PolicyTargetFailed] + counts[domain.PolicyTargetBlocked]
	if counts[domain.PolicyTargetPending] != pending || counts[domain.PolicyTargetRunning] != running ||
		counts[domain.PolicyTargetSucceeded] != succeeded || actualFailed != failed {
		t.Fatalf("rollout target counts=%v, want pending=%d running=%d succeeded=%d failed=%d", counts, pending, running, succeeded, failed)
	}
}
