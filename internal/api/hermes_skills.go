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

type fleetSkillRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Content     string `json:"content"`
}

type syncFleetSkillRequest struct {
	Profile string `json:"profile"`
}

func (s *Server) listFleetSkills(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListFleetSkills(r.Context())
	if err != nil {
		s.logger.Error("list Fleet skills", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load Fleet skill catalog")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) createFleetSkill(w http.ResponseWriter, r *http.Request) {
	var request fleetSkillRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now().UTC()
	skill := domain.FleetSkill{
		Name: request.Name, Description: request.Description, Category: request.Category, Content: request.Content,
		OriginType: domain.FleetSkillOriginCreated,
		CreatedAt:  now, UpdatedAt: now,
	}
	if err := domain.ValidateFleetSkill(&skill); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.store.FleetSkill(r.Context(), skill.Name); err == nil {
		writeError(w, http.StatusConflict, "a Fleet skill with this name already exists")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "failed to inspect Fleet skill catalog")
		return
	}
	if err := s.store.CreateFleetSkill(r.Context(), skill); err != nil {
		s.logger.Error("create Fleet skill", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create Fleet skill")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, skill)
}

func (s *Server) updateFleetSkill(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("skillName"))
	existing, err := s.store.FleetSkill(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "Fleet skill not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load Fleet skill")
		return
	}
	var request fleetSkillRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	skill := domain.FleetSkill{
		Name: name, Description: request.Description, Category: request.Category, Content: request.Content,
		OriginType: existing.OriginType, SourceInstanceID: existing.SourceInstanceID, SourceInstanceName: existing.SourceInstanceName,
		SourceProfile: existing.SourceProfile, SourceRevision: existing.SourceRevision,
		SourceProvenance: existing.SourceProvenance, SourceObservedAt: existing.SourceObservedAt,
		CreatedAt: existing.CreatedAt, UpdatedAt: time.Now().UTC(),
	}
	if err := domain.ValidateFleetSkill(&skill); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.UpdateFleetSkill(r.Context(), skill); err != nil {
		s.logger.Error("update Fleet skill", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update Fleet skill")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, skill)
}

func (s *Server) deleteFleetSkill(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("skillName"))
	if err := s.store.DeleteFleetSkill(r.Context(), name); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "Fleet skill not found")
		return
	} else if err != nil {
		s.logger.Error("delete Fleet skill", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete Fleet skill")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) syncFleetSkill(w http.ResponseWriter, r *http.Request) {
	instanceID := strings.TrimSpace(r.PathValue("instanceID"))
	skillName := strings.TrimSpace(r.PathValue("skillName"))
	skill, err := s.store.FleetSkill(r.Context(), skillName)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "Fleet skill not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load Fleet skill")
		return
	}
	var request syncFleetSkillRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	request.Profile = strings.TrimSpace(request.Profile)
	if request.Profile == "" {
		request.Profile = "default"
	}
	if domain.ValidateHermesProfileReference(request.Profile) != nil {
		writeError(w, http.StatusBadRequest, "a valid target profile is required")
		return
	}
	instance, err := s.store.GetInstance(r.Context(), instanceID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "Hermes instance not found")
		return
	}
	if err != nil {
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
	if err != nil || host.LastSeenAt.IsZero() || now.Sub(host.LastSeenAt) > s.config.OfflineAfter || host.AgentVersion != agentVersion {
		writeError(w, http.StatusConflict, "the compatible instance Host Agent is offline")
		return
	}
	payloadValue := domain.HermesSkillSyncPayload{
		HermesProfileInspectPayload: domain.HermesProfileInspectPayload{
			InstanceID: instance.ID, Name: instance.Name, ProjectName: instance.ProjectName,
			ManagedPath: instance.ManagedPath, DashboardPort: instance.DashboardPort,
		},
		SkillName: skill.Name, Category: skill.Category, Profile: request.Profile,
		Content: skill.Content, Revision: skill.Revision,
	}
	if err := domain.ValidateHermesSkillSyncPayload(&payloadValue); err != nil {
		writeError(w, http.StatusConflict, "the Fleet skill catalog entry is invalid")
		return
	}
	payload, err := json.Marshal(payloadValue)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to prepare skill synchronization")
		return
	}
	operationID, jobID, err := twoIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to allocate skill synchronization identity")
		return
	}
	metadata, _ := json.Marshal(map[string]any{"skill_name": skill.Name, "profile": request.Profile, "revision": skill.Revision})
	operation := domain.Operation{
		ID: operationID, InstanceID: instance.ID, Actor: "Fleet admin", Type: "SYNC_HERMES_SKILL",
		Status: domain.OperationPending, Summary: "Sync Fleet skill " + skill.Name + " to " + instance.Name,
		Metadata: metadata, CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: jobID, OperationID: operation.ID, HostID: instance.HostID, InstanceID: instance.ID,
		Type: domain.JobSyncHermesSkill, Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.QueueRunningInspection(r.Context(), operation, job); err != nil {
		if s.writeQueueAdmissionError(w, err) {
			return
		}
		if errors.Is(err, store.ErrInstanceBusy) || errors.Is(err, store.ErrStateChanged) {
			writeError(w, http.StatusConflict, "the Hermes instance is busy or changed before skill synchronization could start")
			return
		}
		s.logger.Error("queue Hermes skill synchronization", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to queue Hermes skill synchronization")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusAccepted, operation)
}

func (s *Server) removeFleetSkillFromInstance(w http.ResponseWriter, r *http.Request) {
	instanceID := strings.TrimSpace(r.PathValue("instanceID"))
	skillName := strings.TrimSpace(r.PathValue("skillName"))
	if _, err := s.store.FleetSkill(r.Context(), skillName); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "Fleet skill not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load Fleet skill")
		return
	}
	profile := strings.TrimSpace(r.URL.Query().Get("profile"))
	if profile == "" {
		profile = "default"
	}
	if domain.ValidateHermesProfileReference(profile) != nil {
		writeError(w, http.StatusBadRequest, "a valid target profile is required")
		return
	}
	instance, err := s.store.GetInstance(r.Context(), instanceID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "Hermes instance not found")
		return
	}
	if err != nil {
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
	if err != nil || host.LastSeenAt.IsZero() || now.Sub(host.LastSeenAt) > s.config.OfflineAfter || host.AgentVersion != agentVersion {
		writeError(w, http.StatusConflict, "the compatible instance Host Agent is offline")
		return
	}
	payloadValue := domain.HermesSkillRemovePayload{
		HermesProfileInspectPayload: domain.HermesProfileInspectPayload{
			InstanceID: instance.ID, Name: instance.Name, ProjectName: instance.ProjectName,
			ManagedPath: instance.ManagedPath, DashboardPort: instance.DashboardPort,
		},
		SkillName: skillName, Profile: profile,
	}
	if err := domain.ValidateHermesSkillRemovePayload(&payloadValue); err != nil {
		writeError(w, http.StatusBadRequest, "the Fleet skill removal request is invalid")
		return
	}
	payload, err := json.Marshal(payloadValue)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to prepare skill removal")
		return
	}
	operationID, jobID, err := twoIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to allocate skill removal identity")
		return
	}
	metadata, _ := json.Marshal(map[string]string{"skill_name": skillName, "profile": profile})
	operation := domain.Operation{
		ID: operationID, InstanceID: instance.ID, Actor: "Fleet admin", Type: "REMOVE_HERMES_SKILL",
		Status: domain.OperationPending, Summary: "Remove Fleet skill " + skillName + " from " + instance.Name,
		Metadata: metadata, CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: jobID, OperationID: operation.ID, HostID: instance.HostID, InstanceID: instance.ID,
		Type: domain.JobRemoveHermesSkill, Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.QueueRunningInspection(r.Context(), operation, job); err != nil {
		if s.writeQueueAdmissionError(w, err) {
			return
		}
		if errors.Is(err, store.ErrInstanceBusy) || errors.Is(err, store.ErrStateChanged) {
			writeError(w, http.StatusConflict, "the Hermes instance is busy or changed before skill removal could start")
			return
		}
		s.logger.Error("queue Hermes skill removal", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to queue Hermes skill removal")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusAccepted, operation)
}
