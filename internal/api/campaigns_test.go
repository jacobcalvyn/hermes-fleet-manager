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

func TestDiagnosticsCampaignBoundsConcurrencyAndCompletesFromFreshObservations(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	instances := []domain.Instance{
		environment.provisionRunningInstance(t, hostID, hostToken, "campaign-01"),
		environment.provisionRunningInstance(t, hostID, hostToken, "campaign-02"),
		environment.provisionRunningInstance(t, hostID, hostToken, "campaign-03"),
	}
	response := environment.request(t, http.MethodPost, "/api/v1/campaigns", map[string]any{
		"action": "REFRESH_DIAGNOSTICS", "concurrency": 2,
		"instance_ids": []string{instances[0].ID, instances[1].ID, instances[2].ID},
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create campaign status=%d body=%s", response.StatusCode, body)
	}
	var campaign domain.Campaign
	decodeResponse(t, response, &campaign)
	response.Body.Close()
	assertCampaignTargetCounts(t, campaign, 1, 2, 0)

	response = environment.request(t, http.MethodPost, "/api/v1/instances/"+instances[0].ID+"/observations/refresh", map[string]any{}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusConflict)

	targets := environment.observationTargets(t, hostID, hostToken)
	requested := 0
	now := time.Now().UTC()
	reports := make([]domain.InstanceObservation, 0, 2)
	for _, target := range targets {
		if target.RefreshRequestID == "" {
			continue
		}
		requested++
		reports = append(reports, domain.InstanceObservation{
			InstanceID: target.InstanceID, TargetGeneration: target.Generation, RefreshRequestID: target.RefreshRequestID,
			Status: domain.ObservationInSync, Summary: "Runtime matches desired state",
			Checks:     []domain.ObservationCheck{{Name: "runtime", Status: domain.ObservationCheckOK, Detail: "Services are healthy"}},
			ObservedAt: now,
		})
	}
	if requested != 2 {
		t.Fatalf("active refresh requests=%d, want concurrency limit 2", requested)
	}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/observations", map[string]any{"observations": reports}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID})
	assertStatus(t, response, http.StatusNoContent)
	environment.server.reconcileCampaigns(context.Background())
	campaign = environment.getCampaign(t, campaign.ID)
	assertCampaignTargetCounts(t, campaign, 0, 1, 2)

	targets = environment.observationTargets(t, hostID, hostToken)
	var finalTarget *domain.ObservationTarget
	for index := range targets {
		if targets[index].RefreshRequestID != "" {
			finalTarget = &targets[index]
			break
		}
	}
	if finalTarget == nil {
		t.Fatal("third campaign target was not queued after capacity became available")
	}
	response = environment.request(t, http.MethodPost, "/api/v1/agent/observations", map[string]any{"observations": []domain.InstanceObservation{{
		InstanceID: finalTarget.InstanceID, TargetGeneration: finalTarget.Generation, RefreshRequestID: finalTarget.RefreshRequestID,
		Status: domain.ObservationInSync, Summary: "Runtime matches desired state",
		Checks:     []domain.ObservationCheck{{Name: "runtime", Status: domain.ObservationCheckOK, Detail: "Services are healthy"}},
		ObservedAt: now.Add(time.Second),
	}}}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID})
	assertStatus(t, response, http.StatusNoContent)
	environment.server.reconcileCampaigns(context.Background())
	campaign = environment.getCampaign(t, campaign.ID)
	if campaign.Status != domain.OperationSucceeded {
		t.Fatalf("campaign status=%s error=%q targets=%+v", campaign.Status, campaign.Error, campaign.Targets)
	}
	assertCampaignTargetCounts(t, campaign, 0, 0, 3)
}

func TestDiagnosticsCampaignRetryCreatesANewAuditedRun(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	instance := environment.provisionRunningInstance(t, hostID, hostToken, "campaign-retry")
	if err := environment.dataStore.Heartbeat(context.Background(), hostID, "host", "darwin", "arm64", agentVersion, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	response := environment.request(t, http.MethodPost, "/api/v1/campaigns", map[string]any{
		"action": "REFRESH_DIAGNOSTICS", "concurrency": 1, "instance_ids": []string{instance.ID},
	}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create campaign status=%d body=%s", response.StatusCode, body)
	}
	var failed domain.Campaign
	decodeResponse(t, response, &failed)
	response.Body.Close()
	if failed.Status != domain.OperationFailed || len(failed.Targets) != 1 || failed.Targets[0].Status != domain.CampaignTargetBlocked {
		t.Fatalf("offline campaign=%+v", failed)
	}
	if err := environment.dataStore.Heartbeat(context.Background(), hostID, "host", "darwin", "arm64", agentVersion, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	response = environment.request(t, http.MethodPost, "/api/v1/campaigns/"+failed.ID+"/retry", map[string]any{}, environment.adminToken, nil)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("retry campaign status=%d body=%s", response.StatusCode, body)
	}
	var retried domain.Campaign
	decodeResponse(t, response, &retried)
	response.Body.Close()
	if retried.ID == failed.ID || retried.RetryOf != failed.ID || retried.Status != domain.OperationRunning {
		t.Fatalf("retried campaign=%+v failed=%+v", retried, failed)
	}
	if preserved := environment.getCampaign(t, failed.ID); preserved.Status != domain.OperationFailed {
		t.Fatalf("retry mutated original campaign status=%s", preserved.Status)
	}
}

func (environment *apiTestEnvironment) provisionRunningInstance(t *testing.T, hostID, hostToken, name string) domain.Instance {
	t.Helper()
	response := environment.request(t, http.MethodPost, "/api/v1/instances", createInstanceRequest{
		Name: name, HostID: hostID, HermesVersion: "0.19.0",
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
	return environment.overviewInstance(t, instance.ID)
}

func (environment *apiTestEnvironment) observationTargets(t *testing.T, hostID, hostToken string) []domain.ObservationTarget {
	t.Helper()
	response := environment.request(t, http.MethodPost, "/api/v1/agent/observations/targets", map[string]any{}, hostToken, map[string]string{"X-Fleet-Host-ID": hostID})
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("observation targets status=%d body=%s", response.StatusCode, body)
	}
	var payload struct {
		Targets []domain.ObservationTarget `json:"targets"`
	}
	decodeResponse(t, response, &payload)
	response.Body.Close()
	return payload.Targets
}

func (environment *apiTestEnvironment) getCampaign(t *testing.T, campaignID string) domain.Campaign {
	t.Helper()
	response := environment.request(t, http.MethodGet, "/api/v1/campaigns/"+campaignID, nil, environment.adminToken, nil)
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("get campaign status=%d body=%s", response.StatusCode, body)
	}
	var campaign domain.Campaign
	decodeResponse(t, response, &campaign)
	response.Body.Close()
	return campaign
}

func assertCampaignTargetCounts(t *testing.T, campaign domain.Campaign, pending, running, succeeded int) {
	t.Helper()
	counts := map[string]int{}
	for _, target := range campaign.Targets {
		counts[target.Status]++
	}
	if counts[domain.CampaignTargetPending] != pending || counts[domain.CampaignTargetRunning] != running || counts[domain.CampaignTargetSucceeded] != succeeded {
		t.Fatalf("campaign target counts=%v, want pending=%d running=%d succeeded=%d", counts, pending, running, succeeded)
	}
}
