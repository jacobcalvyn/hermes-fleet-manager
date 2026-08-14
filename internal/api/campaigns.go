package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/identity"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/store"
)

type campaignRequest struct {
	Action      string   `json:"action"`
	InstanceIDs []string `json:"instance_ids"`
	Concurrency int      `json:"concurrency"`
}

type campaignMetadata struct {
	Action      string `json:"action"`
	Concurrency int    `json:"concurrency"`
	TargetCount int    `json:"target_count"`
	RetryOf     string `json:"retry_of,omitempty"`
}

func (s *Server) listCampaigns(w http.ResponseWriter, r *http.Request) {
	operations, err := s.store.ListCampaignOperations(r.Context(), 50, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "campaigns could not be loaded")
		return
	}
	items := make([]domain.Campaign, 0, len(operations))
	for _, operation := range operations {
		campaign, err := s.campaignView(r.Context(), operation)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "campaign targets could not be loaded")
			return
		}
		items = append(items, campaign)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) getCampaign(w http.ResponseWriter, r *http.Request) {
	operation, err := s.store.GetOperation(r.Context(), r.PathValue("campaignID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "campaign not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "campaign could not be loaded")
		return
	}
	if operation.Type != domain.CampaignOperationType {
		writeError(w, http.StatusNotFound, "campaign not found")
		return
	}
	campaign, err := s.campaignView(r.Context(), operation)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "campaign targets could not be loaded")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, campaign)
}

func (s *Server) createCampaign(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperationCapacity(w) {
		return
	}
	var request campaignRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	action, instanceIDs, concurrency, err := normalizeCampaignRequest(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	campaign, err := s.startCampaign(r.Context(), action, instanceIDs, concurrency, "")
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, campaign)
}

func (s *Server) retryCampaign(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperationCapacity(w) {
		return
	}
	operation, err := s.store.GetOperation(r.Context(), r.PathValue("campaignID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "campaign not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "campaign could not be loaded")
		return
	}
	if operation.Type != domain.CampaignOperationType {
		writeError(w, http.StatusNotFound, "campaign not found")
		return
	}
	if operation.Status == domain.OperationPending || operation.Status == domain.OperationRunning {
		writeError(w, http.StatusConflict, "an active campaign cannot be retried")
		return
	}
	metadata, err := decodeCampaignMetadata(operation)
	if err != nil {
		writeError(w, http.StatusConflict, "campaign metadata is invalid")
		return
	}
	targets, err := s.store.ListCampaignTargets(r.Context(), operation.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "campaign targets could not be loaded")
		return
	}
	instanceIDs := make([]string, 0, len(targets))
	for _, target := range targets {
		if target.Status == domain.CampaignTargetFailed || target.Status == domain.CampaignTargetBlocked {
			instanceIDs = append(instanceIDs, target.InstanceID)
		}
	}
	if len(instanceIDs) == 0 {
		writeError(w, http.StatusConflict, "this campaign has no failed targets to retry")
		return
	}
	concurrency := metadata.Concurrency
	if concurrency > len(instanceIDs) {
		concurrency = len(instanceIDs)
	}
	campaign, err := s.startCampaign(r.Context(), metadata.Action, instanceIDs, concurrency, operation.ID)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, campaign)
}

func normalizeCampaignRequest(request campaignRequest) (string, []string, int, error) {
	action := strings.TrimSpace(request.Action)
	if action == "" {
		action = domain.CampaignActionRefreshDiagnostics
	}
	if action != domain.CampaignActionRefreshDiagnostics {
		return "", nil, 0, errors.New("this release supports only REFRESH_DIAGNOSTICS campaigns")
	}
	if len(request.InstanceIDs) == 0 || len(request.InstanceIDs) > 100 {
		return "", nil, 0, errors.New("select between 1 and 100 instances")
	}
	seen := make(map[string]bool, len(request.InstanceIDs))
	instanceIDs := make([]string, 0, len(request.InstanceIDs))
	for _, instanceID := range request.InstanceIDs {
		instanceID = strings.TrimSpace(instanceID)
		if instanceID == "" || seen[instanceID] {
			return "", nil, 0, errors.New("campaign targets must contain unique instance identities")
		}
		seen[instanceID] = true
		instanceIDs = append(instanceIDs, instanceID)
	}
	sort.Strings(instanceIDs)
	concurrency := request.Concurrency
	if concurrency == 0 {
		concurrency = 2
	}
	if concurrency < 1 || concurrency > 10 {
		return "", nil, 0, errors.New("campaign concurrency must be between 1 and 10")
	}
	if concurrency > len(instanceIDs) {
		concurrency = len(instanceIDs)
	}
	return action, instanceIDs, concurrency, nil
}

func (s *Server) startCampaign(ctx context.Context, action string, instanceIDs []string, concurrency int, retryOf string) (domain.Campaign, error) {
	operationID, _, err := twoIDs()
	if err != nil {
		return domain.Campaign{}, errors.New("could not create campaign identity")
	}
	now := time.Now().UTC()
	metadata := campaignMetadata{Action: action, Concurrency: concurrency, TargetCount: len(instanceIDs), RetryOf: retryOf}
	operation := domain.Operation{
		ID: operationID, WorkflowID: operationID, Actor: "FLEET_ADMIN", Type: domain.CampaignOperationType,
		Status: domain.OperationPending, Summary: fmt.Sprintf("Refresh diagnostics on %d instance(s)", len(instanceIDs)),
		Metadata: operationMetadata(map[string]any{
			"action": metadata.Action, "concurrency": metadata.Concurrency,
			"target_count": metadata.TargetCount, "retry_of": metadata.RetryOf,
		}),
		Progress: &domain.JobProgress{Stage: "targets_selected", Detail: fmt.Sprintf("%d target(s), concurrency %d", len(instanceIDs), concurrency), Steps: []domain.OperationStep{
			{Stage: "Select targets", Status: "succeeded", Detail: fmt.Sprintf("%d instance(s)", len(instanceIDs))},
			{Stage: "Refresh diagnostics", Status: "pending"},
			{Stage: "Verify observations", Status: "pending"},
		}},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.CreateCampaign(ctx, operation, instanceIDs); err != nil {
		return domain.Campaign{}, err
	}
	s.reconcileCampaign(ctx, operation)
	updated, err := s.store.GetOperation(ctx, operation.ID)
	if err == nil {
		operation = updated
	}
	s.events.Publish("campaign.started", operation.ID)
	return s.campaignView(ctx, operation)
}

func (s *Server) reconcileCampaigns(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	operations, err := s.store.ListCampaignOperations(ctx, 100, true)
	if err != nil {
		s.logger.Error("list active campaigns", "error", err)
		return
	}
	for _, operation := range operations {
		s.reconcileCampaign(ctx, operation)
	}
}

func (s *Server) reconcileCampaign(ctx context.Context, operation domain.Operation) {
	unlock := s.campaignLocks.lock(operation.ID)
	defer unlock()
	metadata, err := decodeCampaignMetadata(operation)
	if err != nil || metadata.Action != domain.CampaignActionRefreshDiagnostics || metadata.Concurrency < 1 {
		s.failCampaign(ctx, operation.ID, "Campaign metadata is invalid")
		return
	}
	targets, err := s.store.ListCampaignTargets(ctx, operation.ID)
	if err != nil {
		s.logger.Error("list campaign targets", "campaign_id", operation.ID, "error", err)
		return
	}
	now := time.Now().UTC()
	timeout := 2 * s.config.ObservationStaleAfter
	if timeout < 2*time.Minute {
		timeout = 2 * time.Minute
	}
	for index := range targets {
		target := &targets[index]
		if target.Status != domain.CampaignTargetRunning || target.RequestedAt == nil {
			continue
		}
		instance, err := s.store.GetInstance(ctx, target.InstanceID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			s.finishCampaignTarget(ctx, target, domain.CampaignTargetFailed, "Instance is unavailable", now)
		case err != nil:
			s.logger.Error("inspect campaign target", "campaign_id", operation.ID, "instance_id", target.InstanceID, "error", err)
		case instance.ObservationRequest == nil && instance.Observation != nil && !instance.Observation.ReceivedAt.Before(*target.RequestedAt):
			s.finishCampaignTarget(ctx, target, domain.CampaignTargetSucceeded, "Fresh diagnostics received", now)
		case instance.ObservationRequest != nil && instance.ObservationRequest.ID != target.RequestID:
			s.finishCampaignTarget(ctx, target, domain.CampaignTargetFailed, "Observation request was superseded", now)
		case now.Sub(*target.RequestedAt) >= timeout:
			s.finishCampaignTarget(ctx, target, domain.CampaignTargetFailed, "Timed out waiting for Host Agent diagnostics", now)
		}
	}
	if campaignTargetsTerminal(targets) {
		s.finishCampaign(ctx, operation.ID, targets)
		return
	}
	running := 0
	for _, target := range targets {
		if target.Status == domain.CampaignTargetRunning {
			running++
		}
	}
	for index := range targets {
		if running >= metadata.Concurrency {
			break
		}
		if targets[index].Status != domain.CampaignTargetPending {
			continue
		}
		s.queueCampaignTarget(ctx, &targets[index], now)
		if targets[index].Status == domain.CampaignTargetRunning {
			running++
		}
	}
	if campaignTargetsTerminal(targets) {
		s.finishCampaign(ctx, operation.ID, targets)
		return
	}
	s.updateCampaignProgress(ctx, operation.ID, targets)
}

func (s *Server) queueCampaignTarget(ctx context.Context, target *domain.CampaignTarget, now time.Time) {
	instance, err := s.store.GetInstance(ctx, target.InstanceID)
	if err != nil {
		s.finishCampaignTarget(ctx, target, domain.CampaignTargetBlocked, "Instance is unavailable", now)
		return
	}
	host, err := s.store.GetHost(ctx, instance.HostID)
	if err != nil || now.Sub(host.LastSeenAt) > s.config.OfflineAfter {
		s.finishCampaignTarget(ctx, target, domain.CampaignTargetBlocked, "Host Agent is offline", now)
		return
	}
	if host.AgentVersion != agentVersion {
		s.finishCampaignTarget(ctx, target, domain.CampaignTargetBlocked, "Host Agent must be upgraded to "+agentVersion, now)
		return
	}
	requestID, err := identity.New()
	if err != nil {
		s.finishCampaignTarget(ctx, target, domain.CampaignTargetFailed, "Observation request identity could not be created", now)
		return
	}
	err = s.store.StartCampaignObservation(ctx, target.CampaignID, target.InstanceID, requestID, now)
	if err == nil {
		target.RequestID, target.Status, target.Detail = requestID, domain.CampaignTargetRunning, "Waiting for a fresh Host Agent observation"
		target.RequestedAt = &now
		return
	}
	detail, status := "Diagnostics refresh could not be queued", domain.CampaignTargetFailed
	if errors.Is(err, store.ErrObservationBusy) {
		detail, status = "Another diagnostics refresh is already pending", domain.CampaignTargetBlocked
	} else if errors.Is(err, store.ErrObservationNotReady) {
		detail, status = "Instance is not ready for diagnostics", domain.CampaignTargetBlocked
	}
	s.finishCampaignTarget(ctx, target, status, detail, now)
}

func (s *Server) finishCampaignTarget(ctx context.Context, target *domain.CampaignTarget, status, detail string, now time.Time) {
	if err := s.store.FinishCampaignTarget(ctx, target.CampaignID, target.InstanceID, target.RequestID, status, detail, now); err != nil && !errors.Is(err, store.ErrStateChanged) {
		s.logger.Error("finish campaign target", "campaign_id", target.CampaignID, "instance_id", target.InstanceID, "error", err)
		return
	}
	target.Status, target.Detail, target.CompletedAt, target.UpdatedAt = status, detail, &now, now
	s.events.Publish("campaign.target.changed", target.CampaignID)
}

func campaignTargetsTerminal(targets []domain.CampaignTarget) bool {
	for _, target := range targets {
		if target.Status == domain.CampaignTargetPending || target.Status == domain.CampaignTargetRunning {
			return false
		}
	}
	return true
}

func campaignCounts(targets []domain.CampaignTarget) (completed, running, failed int) {
	for _, target := range targets {
		switch target.Status {
		case domain.CampaignTargetRunning:
			running++
		case domain.CampaignTargetSucceeded:
			completed++
		case domain.CampaignTargetFailed, domain.CampaignTargetBlocked:
			completed++
			failed++
		}
	}
	return
}

func (s *Server) updateCampaignProgress(ctx context.Context, campaignID string, targets []domain.CampaignTarget) {
	completed, running, _ := campaignCounts(targets)
	progress := domain.JobProgress{Stage: "refreshing_diagnostics", Detail: fmt.Sprintf("%d of %d complete; %d active", completed, len(targets), running), Steps: []domain.OperationStep{
		{Stage: "Select targets", Status: "succeeded"},
		{Stage: "Refresh diagnostics", Status: "running", Detail: fmt.Sprintf("%d of %d complete", completed, len(targets))},
		{Stage: "Verify observations", Status: "pending"},
	}}
	if err := s.store.UpdateControlPlaneOperation(ctx, campaignID, domain.OperationRunning, progress, "", time.Now().UTC()); err != nil {
		s.logger.Error("update campaign progress", "campaign_id", campaignID, "error", err)
	}
}

func (s *Server) finishCampaign(ctx context.Context, campaignID string, targets []domain.CampaignTarget) {
	_, _, failed := campaignCounts(targets)
	status, detail, operationErr, refreshStatus, verifyStatus := domain.OperationSucceeded,
		"Fresh diagnostics were received for every target", "", "succeeded", "succeeded"
	if failed > 0 {
		status = domain.OperationFailed
		detail = fmt.Sprintf("%d of %d target(s) require attention", failed, len(targets))
		operationErr, refreshStatus, verifyStatus = detail, "failed", "failed"
	}
	progress := domain.JobProgress{Stage: "diagnostics_verified", Detail: detail, Steps: []domain.OperationStep{
		{Stage: "Select targets", Status: "succeeded"},
		{Stage: "Refresh diagnostics", Status: refreshStatus},
		{Stage: "Verify observations", Status: verifyStatus, Detail: detail},
	}}
	if err := s.store.UpdateControlPlaneOperation(ctx, campaignID, status, progress, operationErr, time.Now().UTC()); err != nil {
		s.logger.Error("finish campaign", "campaign_id", campaignID, "error", err)
		return
	}
	s.events.Publish("campaign.completed", campaignID)
}

func (s *Server) failCampaign(ctx context.Context, campaignID, detail string) {
	progress := domain.JobProgress{Stage: "campaign_failed", Detail: detail, Steps: []domain.OperationStep{
		{Stage: "Select targets", Status: "failed", Detail: detail},
		{Stage: "Refresh diagnostics", Status: "pending"},
		{Stage: "Verify observations", Status: "pending"},
	}}
	if err := s.store.UpdateControlPlaneOperation(ctx, campaignID, domain.OperationFailed, progress, detail, time.Now().UTC()); err != nil {
		s.logger.Error("fail campaign", "campaign_id", campaignID, "error", err)
	}
}

func decodeCampaignMetadata(operation domain.Operation) (campaignMetadata, error) {
	var metadata campaignMetadata
	if err := json.Unmarshal(operation.Metadata, &metadata); err != nil {
		return metadata, err
	}
	if metadata.Action == "" || metadata.Concurrency < 1 || metadata.TargetCount < 1 {
		return metadata, errors.New("campaign metadata is incomplete")
	}
	return metadata, nil
}

func (s *Server) campaignView(ctx context.Context, operation domain.Operation) (domain.Campaign, error) {
	metadata, err := decodeCampaignMetadata(operation)
	if err != nil {
		return domain.Campaign{}, err
	}
	targets, err := s.store.ListCampaignTargets(ctx, operation.ID)
	if err != nil {
		return domain.Campaign{}, err
	}
	return domain.Campaign{ID: operation.ID, Action: metadata.Action, Status: operation.Status,
		Summary: operation.Summary, Concurrency: metadata.Concurrency, RetryOf: metadata.RetryOf,
		Targets: targets, Progress: operation.Progress, Error: operation.Error,
		CreatedAt: operation.CreatedAt, UpdatedAt: operation.UpdatedAt}, nil
}
