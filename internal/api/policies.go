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
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/releases"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/store"
)

const (
	policyPreviewCompliant = "COMPLIANT"
	policyPreviewDrifted   = "DRIFTED"
	policyPreviewBlocked   = "BLOCKED"
)

type policyListItem struct {
	domain.FleetPolicy
	Compliance    domain.PolicyComplianceSummary `json:"compliance"`
	ActiveRollout *policyRolloutView             `json:"active_rollout,omitempty"`
}

type policyRequest struct {
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	Status           string   `json:"status"`
	DesiredHermes    string   `json:"desired_hermes"`
	Strategy         string   `json:"strategy"`
	ScopeInstanceIDs []string `json:"scope_instance_ids"`
}

type policyRolloutMetadata struct {
	PolicyID            string `json:"policy_id"`
	PolicyName          string `json:"policy_name"`
	Strategy            string `json:"strategy"`
	TargetCount         int    `json:"target_count"`
	ReadyCount          int    `json:"ready_count"`
	BlockedCount        int    `json:"blocked_count"`
	ControlState        string `json:"control_state"`
	ControlReason       string `json:"control_reason,omitempty"`
	FailureAcknowledged bool   `json:"failure_acknowledged,omitempty"`
	CanaryInstanceID    string `json:"canary_instance_id"`
	TargetVersion       string `json:"target_version"`
	TargetSource        string `json:"target_source"`
	TargetImage         string `json:"target_image"`
}

type policyRolloutView struct {
	domain.Operation
	ControlState     string                       `json:"control_state"`
	ControlReason    string                       `json:"control_reason,omitempty"`
	CanaryInstanceID string                       `json:"canary_instance_id"`
	TargetVersion    string                       `json:"target_version"`
	Targets          []domain.PolicyRolloutTarget `json:"targets"`
}

func (s *Server) listPolicies(w http.ResponseWriter, r *http.Request) {
	policies, err := s.store.ListPolicies(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Fleet policies could not be loaded")
		return
	}
	active, err := s.store.ListActivePolicyRollouts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "active policy rollouts could not be loaded")
		return
	}
	activeByPolicy := make(map[string]policyRolloutView)
	for _, operation := range active {
		metadata, metadataErr := decodePolicyRolloutMetadata(operation)
		if metadataErr == nil {
			view, viewErr := s.policyRolloutView(r.Context(), operation, metadata)
			if viewErr != nil {
				writeError(w, http.StatusInternalServerError, "active policy rollout targets could not be loaded")
				return
			}
			activeByPolicy[metadata.PolicyID] = view
		}
	}
	items := make([]policyListItem, 0, len(policies))
	for _, policy := range policies {
		preview := s.evaluatePolicy(r.Context(), policy)
		item := policyListItem{FleetPolicy: policy, Compliance: preview.Summary}
		if operation, ok := activeByPolicy[policy.ID]; ok {
			copy := operation
			item.ActiveRollout = &copy
		}
		items = append(items, item)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) createPolicy(w http.ResponseWriter, r *http.Request) {
	var request policyRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	policy, err := normalizedPolicy(request, "")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	policy.ID, _, err = twoIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create policy identity")
		return
	}
	now := time.Now().UTC()
	policy.CreatedAt, policy.UpdatedAt = now, now
	if err := s.store.CreatePolicy(r.Context(), policy); err != nil {
		writeError(w, http.StatusConflict, policyWriteError(err))
		return
	}
	s.events.Publish("policies.changed", policy.ID)
	writeJSON(w, http.StatusCreated, policy)
}

func (s *Server) updatePolicy(w http.ResponseWriter, r *http.Request) {
	existing, err := s.store.GetPolicy(r.Context(), r.PathValue("policyID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "policy not found")
		return
	}
	var request policyRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	policy, err := normalizedPolicy(request, existing.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	policy.CreatedAt = existing.CreatedAt
	policy.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdatePolicy(r.Context(), policy); err != nil {
		writeError(w, http.StatusConflict, policyWriteError(err))
		return
	}
	s.events.Publish("policies.changed", policy.ID)
	writeJSON(w, http.StatusOK, policy)
}

func (s *Server) deletePolicy(w http.ResponseWriter, r *http.Request) {
	policyID := r.PathValue("policyID")
	if err := s.store.DeletePolicy(r.Context(), policyID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusConflict, "policy not found or still has an active rollout")
			return
		}
		writeError(w, http.StatusInternalServerError, "policy could not be deleted")
		return
	}
	s.events.Publish("policies.changed", policyID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) previewPolicy(w http.ResponseWriter, r *http.Request) {
	policy, err := s.store.GetPolicy(r.Context(), r.PathValue("policyID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "policy not found")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.evaluatePolicy(r.Context(), policy))
}

func (s *Server) startPolicyRollout(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperationCapacity(w) {
		return
	}
	policy, err := s.store.GetPolicy(r.Context(), r.PathValue("policyID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "policy not found")
		return
	}
	if policy.Status != domain.PolicyEnabled {
		writeError(w, http.StatusConflict, "enable the policy before starting a rollout")
		return
	}
	if policy.Strategy != domain.PolicyStrategyOneAtATime {
		writeError(w, http.StatusConflict, "controlled rollouts require the one-instance-at-a-time strategy")
		return
	}
	preview := s.evaluatePolicy(r.Context(), policy)
	readyIDs := make([]string, 0, preview.Summary.Drifted)
	rolloutTargetIDs := make([]string, 0, preview.Summary.Drifted+preview.Summary.Blocked)
	for _, target := range preview.Targets {
		if target.State == policyPreviewDrifted {
			readyIDs = append(readyIDs, target.InstanceID)
			rolloutTargetIDs = append(rolloutTargetIDs, target.InstanceID)
		} else if target.State == policyPreviewBlocked {
			rolloutTargetIDs = append(rolloutTargetIDs, target.InstanceID)
		}
	}
	if len(readyIDs) == 0 {
		writeError(w, http.StatusConflict, "no rollout-ready drift was found in this policy scope")
		return
	}
	var pinned hermesUpdateResponse
	for _, instanceID := range readyIDs {
		instance, instanceErr := s.store.GetInstance(r.Context(), instanceID)
		if instanceErr != nil {
			writeError(w, http.StatusConflict, "a rollout target is no longer available")
			return
		}
		status, statusErr := s.hermesUpdateStatus(r.Context(), instance)
		if statusErr != nil || !status.Available || !status.Eligible {
			writeError(w, http.StatusConflict, "a rollout target is no longer eligible")
			return
		}
		if pinned.TargetVersion == "" {
			pinned = status
			continue
		}
		if status.TargetVersion != pinned.TargetVersion || status.TargetSource != pinned.TargetSource || status.TargetImage != pinned.TargetImage {
			writeError(w, http.StatusConflict, "rollout targets did not resolve to one immutable Hermes release")
			return
		}
	}
	operationID, _, err := twoIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create rollout identity")
		return
	}
	now := time.Now().UTC()
	metadata := policyRolloutMetadata{
		PolicyID: policy.ID, PolicyName: policy.Name, Strategy: policy.Strategy,
		TargetCount: len(rolloutTargetIDs), ReadyCount: len(readyIDs), BlockedCount: preview.Summary.Blocked,
		ControlState: domain.PolicyRolloutControlRunning, CanaryInstanceID: readyIDs[0],
		TargetVersion: pinned.TargetVersion, TargetSource: pinned.TargetSource, TargetImage: pinned.TargetImage,
	}
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "policy rollout snapshot could not be encoded")
		return
	}
	operation := domain.Operation{
		ID: operationID, WorkflowID: operationID, Actor: "FLEET_ADMIN", Type: "ROLLOUT_POLICY",
		Status: domain.OperationPending, Summary: "Roll out policy " + policy.Name,
		Metadata: encodedMetadata,
		Progress: &domain.JobProgress{
			Stage: "canary_pending", Detail: fmt.Sprintf("Release %s frozen for %d rollout-ready instance(s)", pinned.TargetVersion, len(readyIDs)),
			Steps: []domain.OperationStep{
				{Stage: "Freeze target", Status: "succeeded", Detail: "Hermes " + pinned.TargetVersion},
				{Stage: "Canary", Status: "pending"},
				{Stage: "Roll out waves", Status: "pending"},
				{Stage: "Verify compliance", Status: "pending"},
			},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.CreatePolicyRollout(r.Context(), operation, policy.ID, rolloutTargetIDs); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	for _, target := range preview.Targets {
		if target.State == policyPreviewBlocked {
			if err := s.store.UpdatePolicyRolloutTarget(r.Context(), operation.ID, target.InstanceID, "", domain.PolicyTargetBlocked, target.Detail, now); err != nil {
				s.failPolicyRollout(r.Context(), operation.ID, "Blocked rollout targets could not be recorded")
				writeError(w, http.StatusInternalServerError, "policy rollout could not be initialized")
				return
			}
		}
	}
	s.reconcilePolicyRollout(r.Context(), operation)
	updated, err := s.store.GetOperation(r.Context(), operation.ID)
	if err == nil {
		operation = updated
	}
	s.events.Publish("policy.rollout.started", operation.ID)
	writeJSON(w, http.StatusAccepted, operation)
}

func (s *Server) getPolicyRollout(w http.ResponseWriter, r *http.Request) {
	operation, err := s.store.GetOperation(r.Context(), r.PathValue("rolloutID"))
	if err != nil || operation.Type != "ROLLOUT_POLICY" {
		writeError(w, http.StatusNotFound, "policy rollout not found")
		return
	}
	metadata, err := decodePolicyRolloutMetadata(operation)
	if err != nil {
		writeError(w, http.StatusConflict, "policy rollout metadata is invalid")
		return
	}
	view, err := s.policyRolloutView(r.Context(), operation, metadata)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "policy rollout targets could not be loaded")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) pausePolicyRollout(w http.ResponseWriter, r *http.Request) {
	s.changePolicyRolloutControl(w, r, domain.PolicyRolloutControlPaused)
}

func (s *Server) resumePolicyRollout(w http.ResponseWriter, r *http.Request) {
	s.changePolicyRolloutControl(w, r, domain.PolicyRolloutControlRunning)
}

func (s *Server) cancelPolicyRollout(w http.ResponseWriter, r *http.Request) {
	s.changePolicyRolloutControl(w, r, domain.PolicyRolloutControlCanceling)
}

func (s *Server) changePolicyRolloutControl(w http.ResponseWriter, r *http.Request, requested string) {
	rolloutID := r.PathValue("rolloutID")
	unlock := s.policyRolloutLocks.lock(rolloutID)
	defer unlock()
	operation, err := s.store.GetOperation(r.Context(), rolloutID)
	if err != nil || operation.Type != "ROLLOUT_POLICY" {
		writeError(w, http.StatusNotFound, "active policy rollout not found")
		return
	}
	if operation.Status != domain.OperationPending && operation.Status != domain.OperationRunning {
		writeError(w, http.StatusConflict, "completed policy rollouts cannot be controlled")
		return
	}
	metadata, err := decodePolicyRolloutMetadata(operation)
	if err != nil {
		writeError(w, http.StatusConflict, "policy rollout metadata is invalid")
		return
	}
	current := metadata.ControlState
	allowed := (requested == domain.PolicyRolloutControlPaused && current == domain.PolicyRolloutControlRunning) ||
		(requested == domain.PolicyRolloutControlRunning && current == domain.PolicyRolloutControlPaused) ||
		(requested == domain.PolicyRolloutControlCanceling &&
			(current == domain.PolicyRolloutControlRunning || current == domain.PolicyRolloutControlPaused))
	if !allowed {
		writeError(w, http.StatusConflict, "policy rollout control state changed; refresh and retry")
		return
	}
	targets, err := s.store.ListPolicyRolloutTargets(r.Context(), rolloutID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "policy rollout targets could not be loaded")
		return
	}
	metadata.ControlState = requested
	metadata.ControlReason = ""
	progress := controlledPolicyRolloutProgress(metadata, targets)
	switch requested {
	case domain.PolicyRolloutControlPaused:
		metadata.ControlReason = "Paused by Fleet admin; an active target may finish safely"
		progress.Stage, progress.Detail = "rollout_paused", metadata.ControlReason
	case domain.PolicyRolloutControlRunning:
		metadata.FailureAcknowledged = true
		progress.Stage, progress.Detail = "rollout_resumed", "Fleet admin resumed the next bounded wave"
	case domain.PolicyRolloutControlCanceling:
		metadata.ControlReason = "Canceled by Fleet admin; no new targets will start"
		progress.Stage, progress.Detail = "rollout_canceling", metadata.ControlReason
	}
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "policy rollout control could not be encoded")
		return
	}
	if err := s.store.UpdatePolicyRolloutControl(r.Context(), rolloutID, current, encodedMetadata, progress, time.Now().UTC()); err != nil {
		writeError(w, http.StatusConflict, "policy rollout control state changed; refresh and retry")
		return
	}
	if requested == domain.PolicyRolloutControlCanceling {
		if _, err := s.store.BlockPendingPolicyRolloutTargets(r.Context(), rolloutID, "Canceled before execution by Fleet admin", time.Now().UTC()); err != nil {
			writeError(w, http.StatusInternalServerError, "pending rollout targets could not be canceled")
			return
		}
	}
	updated, err := s.store.GetOperation(r.Context(), rolloutID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "policy rollout could not be reloaded")
		return
	}
	// Reconcile while the keyed lock is held through the control transition by
	// calling the unlocked implementation directly.
	s.reconcilePolicyRolloutUnlocked(r.Context(), updated)
	updated, _ = s.store.GetOperation(r.Context(), rolloutID)
	metadata, _ = decodePolicyRolloutMetadata(updated)
	view, err := s.policyRolloutView(r.Context(), updated, metadata)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "policy rollout targets could not be reloaded")
		return
	}
	s.events.Publish("policy.rollout.control.changed", rolloutID)
	writeJSON(w, http.StatusOK, view)
}

func normalizedPolicy(request policyRequest, id string) (domain.FleetPolicy, error) {
	policy := domain.FleetPolicy{
		ID: id, Name: strings.TrimSpace(request.Name), Description: strings.TrimSpace(request.Description),
		Status: strings.TrimSpace(request.Status), DesiredHermes: strings.TrimSpace(request.DesiredHermes),
		Strategy: strings.TrimSpace(request.Strategy), ScopeInstanceIDs: request.ScopeInstanceIDs,
	}
	if policy.Status == "" {
		policy.Status = domain.PolicyEnabled
	}
	if policy.DesiredHermes == "" {
		policy.DesiredHermes = domain.PolicyDesiredHermesLatestStable
	}
	if policy.Strategy == "" {
		policy.Strategy = domain.PolicyStrategyOneAtATime
	}
	if policy.Name == "" || len(policy.Name) > 80 {
		return domain.FleetPolicy{}, errors.New("policy name must contain 1 to 80 characters")
	}
	if len(policy.Description) > 240 {
		return domain.FleetPolicy{}, errors.New("policy description must not exceed 240 characters")
	}
	if policy.Status != domain.PolicyEnabled && policy.Status != domain.PolicyDisabled {
		return domain.FleetPolicy{}, errors.New("policy status must be ENABLED or DISABLED")
	}
	if policy.DesiredHermes != domain.PolicyDesiredHermesLatestStable {
		return domain.FleetPolicy{}, errors.New("this release supports only the Latest stable Hermes baseline")
	}
	if policy.Strategy != domain.PolicyStrategyOneAtATime {
		return domain.FleetPolicy{}, errors.New("controlled rollouts require the ONE_AT_A_TIME strategy")
	}
	if len(policy.ScopeInstanceIDs) == 0 || len(policy.ScopeInstanceIDs) > 100 {
		return domain.FleetPolicy{}, errors.New("select between 1 and 100 instances")
	}
	seen := make(map[string]bool, len(policy.ScopeInstanceIDs))
	clean := make([]string, 0, len(policy.ScopeInstanceIDs))
	for _, instanceID := range policy.ScopeInstanceIDs {
		instanceID = strings.TrimSpace(instanceID)
		if instanceID == "" || seen[instanceID] {
			return domain.FleetPolicy{}, errors.New("policy scope must contain unique instance identities")
		}
		seen[instanceID] = true
		clean = append(clean, instanceID)
	}
	sort.Strings(clean)
	policy.ScopeInstanceIDs = clean
	return policy, nil
}

func policyWriteError(err error) string {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "unique") {
		return "a policy with this name already exists"
	}
	return err.Error()
}

func (s *Server) evaluatePolicy(ctx context.Context, policy domain.FleetPolicy) domain.PolicyPreview {
	preview := domain.PolicyPreview{Policy: policy, Targets: []domain.PolicyTargetPreview{}}
	for _, instanceID := range policy.ScopeInstanceIDs {
		target := s.evaluatePolicyTarget(ctx, instanceID)
		preview.Targets = append(preview.Targets, target)
		preview.Summary.Total++
		switch target.State {
		case policyPreviewCompliant:
			preview.Summary.Compliant++
		case policyPreviewDrifted:
			preview.Summary.Drifted++
		default:
			preview.Summary.Blocked++
		}
	}
	sort.Slice(preview.Targets, func(left, right int) bool {
		return preview.Targets[left].InstanceName < preview.Targets[right].InstanceName
	})
	return preview
}

func (s *Server) evaluatePolicyTarget(ctx context.Context, instanceID string) domain.PolicyTargetPreview {
	instance, err := s.store.GetInstance(ctx, instanceID)
	if err != nil {
		return domain.PolicyTargetPreview{InstanceID: instanceID, State: policyPreviewBlocked, Detail: "Instance is unavailable"}
	}
	target := domain.PolicyTargetPreview{InstanceID: instance.ID, InstanceName: instance.Name, CurrentVersion: instance.HermesVersion}
	if instance.Status == domain.InstanceFailed {
		target.State, target.Detail = policyPreviewBlocked, "Failed instances require an explicit managed recovery action"
		return target
	}
	status, err := s.hermesUpdateStatus(ctx, instance)
	if err != nil {
		target.State, target.Detail = policyPreviewBlocked, "Official Hermes release status could not be resolved"
		return target
	}
	target.CurrentVersion = status.CurrentVersion
	if status.LatestRelease != nil {
		target.TargetVersion = status.LatestRelease.Version
	}
	if !status.Available {
		if status.OfficialStatus == "CURRENT" {
			target.State, target.Detail = policyPreviewCompliant, "Latest stable Hermes runtime is installed"
		} else {
			target.State, target.Detail = policyPreviewBlocked, status.Reason
		}
		return target
	}
	if !status.Eligible {
		target.State, target.Detail = policyPreviewBlocked, status.Reason
		return target
	}
	host, err := s.store.GetHost(ctx, instance.HostID)
	if err != nil || time.Since(host.LastSeenAt) > s.config.OfflineAfter {
		target.State, target.Detail = policyPreviewBlocked, "Host Agent is offline"
		return target
	}
	if host.AgentVersion != agentVersion {
		target.State, target.Detail = policyPreviewBlocked, "Host Agent must be upgraded to "+agentVersion
		return target
	}
	target.State, target.Detail = policyPreviewDrifted, status.Reason
	return target
}

func (s *Server) reconcilePolicyRollouts(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	operations, err := s.store.ListActivePolicyRollouts(ctx)
	if err != nil {
		s.logger.Error("list active policy rollouts", "error", err)
		return
	}
	for _, operation := range operations {
		s.reconcilePolicyRollout(ctx, operation)
	}
}

func (s *Server) reconcilePolicyRollout(ctx context.Context, operation domain.Operation) {
	unlock := s.policyRolloutLocks.lock(operation.ID)
	defer unlock()
	s.reconcilePolicyRolloutUnlocked(ctx, operation)
}

func (s *Server) reconcilePolicyRolloutUnlocked(ctx context.Context, operation domain.Operation) {
	metadata, err := decodePolicyRolloutMetadata(operation)
	if err != nil {
		s.failPolicyRollout(ctx, operation.ID, "Policy rollout metadata is invalid")
		return
	}
	targets, err := s.store.ListPolicyRolloutTargets(ctx, operation.ID)
	if err != nil {
		s.logger.Error("list policy rollout targets", "operation_id", operation.ID, "error", err)
		return
	}
	now := time.Now().UTC()
	targetFailed := false
	for index := range targets {
		target := &targets[index]
		if target.ChildOperationID == "" || target.Status != domain.PolicyTargetRunning {
			continue
		}
		child, err := s.store.GetOperation(ctx, target.ChildOperationID)
		if err != nil {
			_ = s.store.UpdatePolicyRolloutTarget(ctx, operation.ID, target.InstanceID, "", domain.PolicyTargetFailed, "Child operation is unavailable", now)
			target.Status, target.Detail = domain.PolicyTargetFailed, "Child operation is unavailable"
			targetFailed = true
			continue
		}
		switch child.Status {
		case domain.OperationSucceeded:
			_ = s.store.UpdatePolicyRolloutTarget(ctx, operation.ID, target.InstanceID, "", domain.PolicyTargetSucceeded, "Hermes baseline applied", now)
			target.Status, target.Detail = domain.PolicyTargetSucceeded, "Hermes baseline applied"
		case domain.OperationFailed:
			detail := child.Error
			if detail == "" {
				detail = "Hermes update failed"
			}
			_ = s.store.UpdatePolicyRolloutTarget(ctx, operation.ID, target.InstanceID, "", domain.PolicyTargetFailed, detail, now)
			target.Status, target.Detail = domain.PolicyTargetFailed, detail
			targetFailed = true
		}
	}
	if policyRolloutTerminal(targets) {
		s.finishPolicyRollout(ctx, operation.ID, targets, metadata)
		return
	}
	if metadata.ControlState == domain.PolicyRolloutControlCanceling {
		_, _ = s.store.BlockPendingPolicyRolloutTargets(ctx, operation.ID, "Canceled before execution by Fleet admin", now)
		targets, _ = s.store.ListPolicyRolloutTargets(ctx, operation.ID)
		if policyRolloutTerminal(targets) {
			s.finishPolicyRollout(ctx, operation.ID, targets, metadata)
		} else {
			s.updatePolicyRolloutProgress(ctx, operation.ID, targets, metadata)
		}
		return
	}
	if targetFailed && metadata.ControlState == domain.PolicyRolloutControlRunning {
		if s.autoPausePolicyRollout(ctx, operation.ID, &metadata, targets, "A rollout target failed; Fleet stopped before the next wave") {
			return
		}
	}
	if metadata.ControlState == domain.PolicyRolloutControlPaused {
		s.updatePolicyRolloutProgress(ctx, operation.ID, targets, metadata)
		return
	}
	running := 0
	for _, target := range targets {
		if target.Status == domain.PolicyTargetRunning {
			running++
		}
	}
	if running > 0 {
		s.updatePolicyRolloutProgress(ctx, operation.ID, targets, metadata)
		return
	}
	canaryIndex := policyRolloutTargetIndex(targets, metadata.CanaryInstanceID)
	if canaryIndex >= 0 && targets[canaryIndex].Status == domain.PolicyTargetPending {
		s.queuePolicyTarget(ctx, operation.ID, &targets[canaryIndex], metadata)
	} else if canaryIndex >= 0 && !metadata.FailureAcknowledged && (targets[canaryIndex].Status == domain.PolicyTargetFailed || targets[canaryIndex].Status == domain.PolicyTargetBlocked) {
		if s.autoPausePolicyRollout(ctx, operation.ID, &metadata, targets, "Canary did not pass; Fleet stopped before the first wave") {
			return
		}
	} else {
		for index := range targets {
			if targets[index].Status != domain.PolicyTargetPending {
				continue
			}
			s.queuePolicyTarget(ctx, operation.ID, &targets[index], metadata)
			if targets[index].Status == domain.PolicyTargetBlocked || targets[index].Status == domain.PolicyTargetFailed {
				if s.autoPausePolicyRollout(ctx, operation.ID, &metadata, targets, "A rollout target was blocked; Fleet stopped before the next wave") {
					return
				}
			}
			break
		}
	}
	if policyRolloutTerminal(targets) {
		s.finishPolicyRollout(ctx, operation.ID, targets, metadata)
		return
	}
	s.updatePolicyRolloutProgress(ctx, operation.ID, targets, metadata)
}

func (s *Server) queuePolicyTarget(ctx context.Context, rolloutID string, target *domain.PolicyRolloutTarget, metadata policyRolloutMetadata) {
	now := time.Now().UTC()
	instance, err := s.store.GetInstance(ctx, target.InstanceID)
	if err != nil {
		_ = s.store.UpdatePolicyRolloutTarget(ctx, rolloutID, target.InstanceID, "", domain.PolicyTargetBlocked, "Instance is unavailable", now)
		target.Status, target.Detail = domain.PolicyTargetBlocked, "Instance is unavailable"
		return
	}
	host, err := s.store.GetHost(ctx, instance.HostID)
	if err != nil || time.Since(host.LastSeenAt) > s.config.OfflineAfter {
		_ = s.store.UpdatePolicyRolloutTarget(ctx, rolloutID, target.InstanceID, "", domain.PolicyTargetBlocked, "Host Agent is unavailable", now)
		target.Status, target.Detail = domain.PolicyTargetBlocked, "Host Agent is unavailable"
		return
	}
	if host.AgentVersion != agentVersion {
		detail := "Host Agent must be upgraded to " + agentVersion
		_ = s.store.UpdatePolicyRolloutTarget(ctx, rolloutID, target.InstanceID, "", domain.PolicyTargetBlocked, detail, now)
		target.Status, target.Detail = domain.PolicyTargetBlocked, detail
		return
	}
	status, err := s.pinnedPolicyUpdateStatus(ctx, instance, metadata)
	if err == nil && !status.Available && status.OfficialStatus == "CURRENT" {
		_ = s.store.UpdatePolicyRolloutTarget(ctx, rolloutID, target.InstanceID, "", domain.PolicyTargetSucceeded, "Already on frozen Hermes "+metadata.TargetVersion, now)
		target.Status, target.Detail = domain.PolicyTargetSucceeded, "Already on frozen Hermes "+metadata.TargetVersion
		return
	}
	if err != nil || !status.Available || !status.Eligible {
		detail := "Hermes update is no longer eligible"
		if err == nil && status.Reason != "" {
			detail = status.Reason
		}
		_ = s.store.UpdatePolicyRolloutTarget(ctx, rolloutID, target.InstanceID, "", domain.PolicyTargetBlocked, detail, now)
		target.Status, target.Detail = domain.PolicyTargetBlocked, detail
		return
	}
	child, err := s.queueHermesUpdate(ctx, instance, host, status, instance.Status, rolloutID, "POLICY_CONTROLLER")
	if err != nil {
		detail := err.Error()
		_ = s.store.UpdatePolicyRolloutTarget(ctx, rolloutID, target.InstanceID, "", domain.PolicyTargetBlocked, detail, now)
		target.Status, target.Detail = domain.PolicyTargetBlocked, detail
		return
	}
	_ = s.store.UpdatePolicyRolloutTarget(ctx, rolloutID, target.InstanceID, child.ID, domain.PolicyTargetRunning, child.Summary, now)
	target.ChildOperationID, target.Status, target.Detail = child.ID, domain.PolicyTargetRunning, child.Summary
}

func policyRolloutTerminal(targets []domain.PolicyRolloutTarget) bool {
	for _, target := range targets {
		if target.Status == domain.PolicyTargetPending || target.Status == domain.PolicyTargetRunning {
			return false
		}
	}
	return true
}

func (s *Server) updatePolicyRolloutProgress(ctx context.Context, operationID string, targets []domain.PolicyRolloutTarget, metadata policyRolloutMetadata) {
	progress := controlledPolicyRolloutProgress(metadata, targets)
	if err := s.store.UpdateControlPlaneOperation(ctx, operationID, domain.OperationRunning, progress, "", time.Now().UTC()); err != nil {
		s.logger.Error("update policy rollout progress", "operation_id", operationID, "error", err)
	}
}

func (s *Server) finishPolicyRollout(ctx context.Context, operationID string, targets []domain.PolicyRolloutTarget, metadata policyRolloutMetadata) {
	failed := 0
	for _, target := range targets {
		if target.Status == domain.PolicyTargetFailed || target.Status == domain.PolicyTargetBlocked {
			failed++
		}
	}
	status, detail, operationErr := domain.OperationSucceeded, "All rollout targets reached frozen Hermes "+metadata.TargetVersion, ""
	applyStatus := "succeeded"
	verificationStatus := "succeeded"
	stage := "compliance_verified"
	if metadata.ControlState == domain.PolicyRolloutControlCanceling {
		status = domain.OperationFailed
		detail = "Rollout canceled; active work finished safely and no additional targets started"
		operationErr = detail
		applyStatus, verificationStatus, stage = "failed", "failed", "rollout_canceled"
	} else if failed > 0 {
		status = domain.OperationFailed
		detail = fmt.Sprintf("%d of %d rollout target(s) require attention", failed, len(targets))
		operationErr = detail
		applyStatus = "failed"
		verificationStatus = "failed"
	}
	progress := domain.JobProgress{
		Stage: stage, Detail: detail,
		Steps: []domain.OperationStep{
			{Stage: "Freeze target", Status: "succeeded", Detail: "Hermes " + metadata.TargetVersion},
			{Stage: "Canary", Status: policyRolloutCanaryStepStatus(targets, metadata.CanaryInstanceID)},
			{Stage: "Roll out waves", Status: applyStatus},
			{Stage: "Verify compliance", Status: verificationStatus, Detail: detail},
		},
	}
	if err := s.store.UpdateControlPlaneOperation(ctx, operationID, status, progress, operationErr, time.Now().UTC()); err != nil {
		s.logger.Error("finish policy rollout", "operation_id", operationID, "error", err)
		return
	}
	s.events.Publish("policy.rollout.completed", operationID)
}

func controlledPolicyRolloutProgress(metadata policyRolloutMetadata, targets []domain.PolicyRolloutTarget) domain.JobProgress {
	completed, running := 0, 0
	for _, target := range targets {
		switch target.Status {
		case domain.PolicyTargetSucceeded, domain.PolicyTargetFailed, domain.PolicyTargetBlocked:
			completed++
		case domain.PolicyTargetRunning:
			running++
		}
	}
	stage := "applying_rollout"
	detail := fmt.Sprintf("%d of %d complete; %d active", completed, len(targets), running)
	if metadata.ControlState == domain.PolicyRolloutControlPaused {
		stage, detail = "rollout_paused", metadata.ControlReason
	} else if metadata.ControlState == domain.PolicyRolloutControlCanceling {
		stage, detail = "rollout_canceling", metadata.ControlReason
	} else if canary := policyRolloutTarget(targets, metadata.CanaryInstanceID); canary != nil && canary.Status != domain.PolicyTargetSucceeded {
		stage = "canary"
		detail = canary.Detail
		if detail == "" {
			detail = "Waiting for the canary health gate"
		}
	}
	waveStatus := "pending"
	if policyRolloutCanaryStepStatus(targets, metadata.CanaryInstanceID) == "succeeded" {
		waveStatus = "running"
		if completed == len(targets) {
			waveStatus = "succeeded"
		}
	}
	return domain.JobProgress{
		Stage: stage, Detail: detail,
		Steps: []domain.OperationStep{
			{Stage: "Freeze target", Status: "succeeded", Detail: "Hermes " + metadata.TargetVersion},
			{Stage: "Canary", Status: policyRolloutCanaryStepStatus(targets, metadata.CanaryInstanceID)},
			{Stage: "Roll out waves", Status: waveStatus, Detail: fmt.Sprintf("%d of %d complete", completed, len(targets))},
			{Stage: "Verify compliance", Status: "pending"},
		},
	}
}

func policyRolloutCanaryStepStatus(targets []domain.PolicyRolloutTarget, canaryID string) string {
	canary := policyRolloutTarget(targets, canaryID)
	if canary == nil {
		return "pending"
	}
	switch canary.Status {
	case domain.PolicyTargetRunning:
		return "running"
	case domain.PolicyTargetSucceeded:
		return "succeeded"
	case domain.PolicyTargetFailed, domain.PolicyTargetBlocked:
		return "failed"
	default:
		return "pending"
	}
}

func policyRolloutTarget(targets []domain.PolicyRolloutTarget, instanceID string) *domain.PolicyRolloutTarget {
	index := policyRolloutTargetIndex(targets, instanceID)
	if index < 0 {
		return nil
	}
	return &targets[index]
}

func policyRolloutTargetIndex(targets []domain.PolicyRolloutTarget, instanceID string) int {
	for index := range targets {
		if targets[index].InstanceID == instanceID {
			return index
		}
	}
	return -1
}

func (s *Server) autoPausePolicyRollout(
	ctx context.Context,
	rolloutID string,
	metadata *policyRolloutMetadata,
	targets []domain.PolicyRolloutTarget,
	reason string,
) bool {
	pending := false
	for _, target := range targets {
		if target.Status == domain.PolicyTargetPending {
			pending = true
			break
		}
	}
	if !pending || metadata.ControlState != domain.PolicyRolloutControlRunning {
		return false
	}
	metadata.ControlState = domain.PolicyRolloutControlPaused
	metadata.ControlReason = reason
	metadata.FailureAcknowledged = false
	progress := controlledPolicyRolloutProgress(*metadata, targets)
	encoded, err := json.Marshal(metadata)
	if err != nil {
		s.logger.Error("encode automatic policy rollout pause", "rollout_id", rolloutID, "error", err)
		return true
	}
	if err := s.store.UpdatePolicyRolloutControl(ctx, rolloutID, domain.PolicyRolloutControlRunning, encoded, progress, time.Now().UTC()); err != nil {
		s.logger.Error("automatically pause policy rollout", "rollout_id", rolloutID, "error", err)
		return true
	}
	s.events.Publish("policy.rollout.paused", rolloutID)
	return true
}

func decodePolicyRolloutMetadata(operation domain.Operation) (policyRolloutMetadata, error) {
	var metadata policyRolloutMetadata
	if err := json.Unmarshal(operation.Metadata, &metadata); err != nil {
		return metadata, err
	}
	if metadata.PolicyID == "" || metadata.Strategy == "" {
		return metadata, errors.New("policy rollout identity is missing")
	}
	if metadata.ControlState == "" {
		metadata.ControlState = domain.PolicyRolloutControlRunning
	}
	if metadata.ControlState != domain.PolicyRolloutControlRunning &&
		metadata.ControlState != domain.PolicyRolloutControlPaused &&
		metadata.ControlState != domain.PolicyRolloutControlCanceling {
		return metadata, errors.New("policy rollout control state is invalid")
	}
	return metadata, nil
}

func (s *Server) policyRolloutView(ctx context.Context, operation domain.Operation, metadata policyRolloutMetadata) (policyRolloutView, error) {
	targets, err := s.store.ListPolicyRolloutTargets(ctx, operation.ID)
	if err != nil {
		return policyRolloutView{}, err
	}
	if metadata.CanaryInstanceID == "" {
		for _, target := range targets {
			if target.Status != domain.PolicyTargetBlocked {
				metadata.CanaryInstanceID = target.InstanceID
				break
			}
		}
	}
	return policyRolloutView{
		Operation: operation, ControlState: metadata.ControlState, ControlReason: metadata.ControlReason,
		CanaryInstanceID: metadata.CanaryInstanceID, TargetVersion: metadata.TargetVersion, Targets: targets,
	}, nil
}

func (s *Server) pinnedPolicyUpdateStatus(ctx context.Context, instance domain.Instance, metadata policyRolloutMetadata) (hermesUpdateResponse, error) {
	if metadata.TargetVersion == "" || metadata.TargetSource == "" || metadata.TargetImage == "" {
		return s.hermesUpdateStatus(ctx, instance)
	}
	catalog, err := s.hermesCatalog(ctx)
	if err != nil {
		return hermesUpdateResponse{Reason: "The verified Hermes release catalog is unavailable"}, nil
	}
	release, ok := releases.Find(catalog, metadata.TargetVersion)
	if !ok || !strings.EqualFold(release.Commit, metadata.TargetSource) || release.Image != metadata.TargetImage {
		return hermesUpdateResponse{Reason: "The frozen Hermes release is no longer in the verified catalog"}, nil
	}
	current := hermesUpdateResponse{CurrentImage: instance.Image, LatestRelease: &release, TargetVersion: release.Version,
		TargetSource: release.Commit, TargetImage: release.Image, OfficialStatus: "UPDATE_AVAILABLE"}
	targetGeneration := instance.UpdatedAt.UTC().Format(time.RFC3339Nano)
	if instance.Observation != nil && instance.Observation.TargetGeneration == targetGeneration {
		current.CurrentVersion, current.CurrentSource = instance.Observation.HermesVersion, instance.Observation.HermesSource
	}
	if current.CurrentVersion == "" || !hermesVersionPattern.MatchString(current.CurrentVersion) {
		if installed, found := releases.FindByRuntimeImage(catalog, instance.Image); found {
			current.CurrentVersion, current.CurrentSource = installed.Version, installed.Commit
		}
	}
	if current.CurrentVersion == "" {
		current.Reason = "Installed Hermes version is not authoritative"
		return current, nil
	}
	comparison := releases.Compare(current.CurrentVersion, release.Version)
	if comparison > 0 {
		current.Reason = "Instance is newer than the frozen rollout release"
		return current, nil
	}
	if comparison == 0 && instance.Image == release.Image {
		current.OfficialStatus = "CURRENT"
		current.Reason = "Frozen Hermes release is already installed"
		return current, nil
	}
	current.UpdateKind = hermesUpdateKindVersionUpdate
	if comparison == 0 {
		current.UpdateKind = hermesUpdateKindRuntimeRefresh
	}
	current.Available = true
	if instance.Status != domain.InstanceRunning && instance.Status != domain.InstanceStopped {
		current.Reason = "Wait until the instance reaches a stable running or stopped state"
		return current, nil
	}
	if instance.ImageID == "" || instance.ProjectName == "" || instance.DataVolume == "" || instance.ManagedPath == "" {
		current.Reason = "Instance runtime metadata is incomplete"
		return current, nil
	}
	if s.config.RecoveryPoints == nil {
		current.Reason = "Instance backup storage is unavailable"
		return current, nil
	}
	current.Eligible = true
	current.Reason = "Fleet will apply the frozen release with a verified rollback backup and health gate"
	return current, nil
}

func (s *Server) failPolicyRollout(ctx context.Context, operationID, detail string) {
	progress := domain.JobProgress{Stage: "rollout_failed", Detail: detail, Steps: []domain.OperationStep{
		{Stage: "Preview impact", Status: "succeeded"},
		{Stage: "Apply rollout", Status: "failed", Detail: detail},
		{Stage: "Verify compliance", Status: "pending"},
	}}
	if err := s.store.UpdateControlPlaneOperation(ctx, operationID, domain.OperationFailed, progress, detail, time.Now().UTC()); err != nil {
		s.logger.Error("fail policy rollout", "operation_id", operationID, "error", err)
	}
}
