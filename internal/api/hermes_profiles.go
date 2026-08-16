package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/store"
)

type createHermesProfileRequest struct {
	Name        string `json:"name"`
	CloneFrom   string `json:"clone_from"`
	Description string `json:"description"`
}

func (s *Server) listHermesProfiles(w http.ResponseWriter, r *http.Request) {
	instanceID := strings.TrimSpace(r.PathValue("instanceID"))
	if instanceID == "" {
		writeError(w, http.StatusBadRequest, "a Hermes instance is required")
		return
	}
	inventory, err := s.store.HermesProfileInventory(r.Context(), instanceID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "Hermes instance not found")
		return
	}
	if err != nil {
		s.logger.Error("list Hermes profiles", "error", err, "instance_id", instanceID)
		writeError(w, http.StatusInternalServerError, "failed to load Hermes profiles")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, inventory)
}

func (s *Server) refreshHermesProfiles(w http.ResponseWriter, r *http.Request) {
	s.queueHermesProfileJob(w, r, domain.JobInspectHermesProfiles, createHermesProfileRequest{})
}

func (s *Server) repairHermesProfiles(w http.ResponseWriter, r *http.Request) {
	s.queueHermesProfileJob(w, r, domain.JobRepairHermesProfiles, createHermesProfileRequest{})
}

func (s *Server) createHermesProfile(w http.ResponseWriter, r *http.Request) {
	var request createHermesProfileRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	request.CloneFrom = strings.TrimSpace(request.CloneFrom)
	request.Description = strings.TrimSpace(request.Description)
	if domain.ValidateHermesProfileName(request.Name) != nil || domain.ValidateHermesProfileReference(request.CloneFrom) != nil ||
		request.Name == request.CloneFrom || len(request.Description) > 1000 {
		writeError(w, http.StatusBadRequest, "profile name and clone source must be distinct valid Hermes profile names")
		return
	}
	s.queueHermesProfileJob(w, r, domain.JobCreateHermesProfile, request)
}

func (s *Server) activateHermesProfile(w http.ResponseWriter, r *http.Request) {
	profileName := strings.TrimSpace(r.PathValue("profileName"))
	if domain.ValidateHermesProfileReference(profileName) != nil {
		writeError(w, http.StatusBadRequest, "a valid Hermes profile is required")
		return
	}
	s.queueHermesProfileJob(w, r, domain.JobActivateHermesProfile, createHermesProfileRequest{Name: profileName})
}

func (s *Server) deleteHermesProfile(w http.ResponseWriter, r *http.Request) {
	profileName := strings.TrimSpace(r.PathValue("profileName"))
	if domain.ValidateHermesProfileReference(profileName) != nil {
		writeError(w, http.StatusBadRequest, "a valid Hermes profile is required")
		return
	}
	if profileName == "default" {
		writeError(w, http.StatusBadRequest, "the default Hermes profile cannot be deleted")
		return
	}
	s.queueHermesProfileJob(w, r, domain.JobDeleteHermesProfile, createHermesProfileRequest{Name: profileName})
}

func (s *Server) queueHermesProfileJob(w http.ResponseWriter, r *http.Request, jobType string, request createHermesProfileRequest) {
	instanceID := strings.TrimSpace(r.PathValue("instanceID"))
	instance, err := s.store.GetInstance(r.Context(), instanceID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "Hermes instance not found")
		return
	}
	if err != nil {
		s.logger.Error("get Hermes profile target", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load Hermes instance")
		return
	}
	if instance.Status != domain.InstanceRunning || instance.ProjectName == "" || instance.ManagedPath == "" ||
		instance.DashboardPort < 1 || instance.DashboardPort > 65535 {
		writeError(w, http.StatusConflict, "the Hermes instance must be running with complete managed runtime metadata")
		return
	}
	host, err := s.store.GetHost(r.Context(), instance.HostID)
	now := time.Now().UTC()
	if err != nil || host.LastSeenAt.IsZero() || now.Sub(host.LastSeenAt) > s.config.OfflineAfter {
		writeError(w, http.StatusConflict, "the instance Host Agent is offline")
		return
	}
	if host.AgentVersion != agentVersion {
		writeError(w, http.StatusConflict, "Hermes profile management requires Host Agent "+agentVersion)
		return
	}
	operationID, jobID, err := twoIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to allocate Hermes profile operation identity")
		return
	}
	base := domain.HermesProfileInspectPayload{
		InstanceID: instance.ID, Name: instance.Name, ProjectName: instance.ProjectName,
		ManagedPath: instance.ManagedPath, DashboardPort: instance.DashboardPort,
	}
	var payload []byte
	operationType, summary := "REFRESH_HERMES_PROFILES", "Refresh Hermes profiles for "+instance.Name
	metadata := map[string]any{}
	switch jobType {
	case domain.JobCreateHermesProfile:
		payload, err = json.Marshal(domain.HermesProfileCreatePayload{
			HermesProfileInspectPayload: base, ProfileName: request.Name,
			CloneFrom: request.CloneFrom, Description: request.Description,
		})
		operationType, summary = "CREATE_HERMES_PROFILE", "Create Hermes profile "+request.Name+" on "+instance.Name
		metadata = map[string]any{"profile_name": request.Name, "clone_from": request.CloneFrom}
	case domain.JobRepairHermesProfiles:
		payload, err = json.Marshal(base)
		operationType, summary = "REPAIR_HERMES_PROFILE_ACCESS", "Repair Hermes profile access for "+instance.Name
	case domain.JobActivateHermesProfile:
		payload, err = json.Marshal(domain.HermesProfileMutationPayload{
			HermesProfileInspectPayload: base, ProfileName: request.Name,
		})
		operationType, summary = "ACTIVATE_HERMES_PROFILE", "Set active Hermes profile "+request.Name+" on "+instance.Name
		metadata = map[string]any{"profile_name": request.Name}
	case domain.JobDeleteHermesProfile:
		payload, err = json.Marshal(domain.HermesProfileMutationPayload{
			HermesProfileInspectPayload: base, ProfileName: request.Name,
		})
		operationType, summary = "DELETE_HERMES_PROFILE", "Delete Hermes profile "+request.Name+" from "+instance.Name
		metadata = map[string]any{"profile_name": request.Name}
	default:
		payload, err = json.Marshal(base)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to prepare Hermes profile operation")
		return
	}
	encodedMetadata, _ := json.Marshal(metadata)
	operation := domain.Operation{
		ID: operationID, InstanceID: instance.ID, Actor: "Fleet admin", Type: operationType,
		Status: domain.OperationPending, Summary: summary, Metadata: encodedMetadata, CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: jobID, OperationID: operation.ID, HostID: instance.HostID, InstanceID: instance.ID,
		Type: jobType, Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.QueueRunningInspection(r.Context(), operation, job); err != nil {
		if s.writeQueueAdmissionError(w, err) {
			return
		}
		if errors.Is(err, store.ErrInstanceBusy) || errors.Is(err, store.ErrStateChanged) {
			writeError(w, http.StatusConflict, "the Hermes instance is busy or changed before profile management could start")
			return
		}
		s.logger.Error("queue Hermes profile operation", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to queue Hermes profile operation")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusAccepted, operation)
}
