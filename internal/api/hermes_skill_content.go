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

func (s *Server) getHermesSkillContent(w http.ResponseWriter, r *http.Request) {
	instanceID, skillName := strings.TrimSpace(r.PathValue("instanceID")), strings.TrimSpace(r.PathValue("skillName"))
	profile := strings.TrimSpace(r.URL.Query().Get("profile"))
	if profile == "" {
		profile = "default"
	}
	snapshot, err := s.store.HermesSkillContentSnapshot(r.Context(), instanceID, profile, skillName)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "Hermes skill content has not been inspected")
		return
	}
	if err != nil {
		s.logger.Error("load Hermes skill content", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load Hermes skill content")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) refreshHermesSkillContent(w http.ResponseWriter, r *http.Request) {
	instanceID, skillName := strings.TrimSpace(r.PathValue("instanceID")), strings.TrimSpace(r.PathValue("skillName"))
	profile := strings.TrimSpace(r.URL.Query().Get("profile"))
	if profile == "" {
		profile = "default"
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
	payloadValue := domain.HermesSkillContentInspectPayload{
		HermesProfileInspectPayload: domain.HermesProfileInspectPayload{
			InstanceID: instance.ID, Name: instance.Name, ProjectName: instance.ProjectName,
			ManagedPath: instance.ManagedPath, DashboardPort: instance.DashboardPort,
		},
		SkillName: skillName, Profile: profile,
	}
	if instance.Status != domain.InstanceRunning || domain.ValidateHermesSkillContentInspectPayload(&payloadValue) != nil {
		writeError(w, http.StatusConflict, "the Hermes instance must be running with valid skill runtime metadata")
		return
	}
	host, err := s.store.GetHost(r.Context(), instance.HostID)
	now := time.Now().UTC()
	if err != nil || host.LastSeenAt.IsZero() || now.Sub(host.LastSeenAt) > s.config.OfflineAfter || host.AgentVersion != agentVersion {
		writeError(w, http.StatusConflict, "the compatible instance Host Agent is offline")
		return
	}
	payload, err := json.Marshal(payloadValue)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to prepare Hermes skill inspection")
		return
	}
	operationID, jobID, err := twoIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to allocate Hermes skill inspection identity")
		return
	}
	metadata, _ := json.Marshal(map[string]string{"skill_name": skillName, "profile": profile})
	operation := domain.Operation{
		ID: operationID, InstanceID: instance.ID, Actor: "Fleet admin", Type: "INSPECT_HERMES_SKILL",
		Status: domain.OperationPending, Summary: "Inspect Hermes skill " + skillName + " on " + instance.Name,
		Metadata: metadata, CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: jobID, OperationID: operation.ID, HostID: instance.HostID, InstanceID: instance.ID,
		Type: domain.JobInspectHermesSkillContent, Status: domain.JobPending, Payload: payload,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.QueueRunningInspection(r.Context(), operation, job); err != nil {
		if s.writeQueueAdmissionError(w, err) {
			return
		}
		if errors.Is(err, store.ErrInstanceBusy) || errors.Is(err, store.ErrStateChanged) {
			writeError(w, http.StatusConflict, "the Hermes instance is busy or changed before skill inspection could start")
			return
		}
		s.logger.Error("queue Hermes skill inspection", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to queue Hermes skill inspection")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusAccepted, operation)
}

func (s *Server) copyHermesSkillToCatalog(w http.ResponseWriter, r *http.Request) {
	instanceID, skillName := strings.TrimSpace(r.PathValue("instanceID")), strings.TrimSpace(r.PathValue("skillName"))
	profile := strings.TrimSpace(r.URL.Query().Get("profile"))
	if profile == "" {
		profile = "default"
	}
	snapshot, err := s.store.HermesSkillContentSnapshot(r.Context(), instanceID, profile, skillName)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusConflict, "inspect the Hermes skill before copying it")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load Hermes skill content")
		return
	}
	skill, err := domain.FleetSkillFromContent(snapshot.SkillName, snapshot.Content)
	if err != nil {
		writeError(w, http.StatusConflict, "the Hermes skill is not a valid Fleet skill: "+err.Error())
		return
	}
	skill.OriginType = domain.FleetSkillOriginInstance
	skill.SourceInstanceID = snapshot.InstanceID
	instance, instanceErr := s.store.GetInstance(r.Context(), snapshot.InstanceID)
	if instanceErr != nil {
		writeError(w, http.StatusConflict, "the source Hermes instance is no longer registered")
		return
	}
	skill.SourceInstanceName = instance.Name
	skill.SourceProfile = snapshot.Profile
	skill.SourceRevision = snapshot.Revision
	skill.SourceProvenance = snapshot.Provenance
	skill.SourceObservedAt = snapshot.ObservedAt
	if existing, existingErr := s.store.FleetSkill(r.Context(), skill.Name); existingErr == nil {
		if existing.Revision == skill.Revision {
			skill.CreatedAt, skill.UpdatedAt = existing.CreatedAt, time.Now().UTC()
			if err := s.store.UpdateFleetSkill(r.Context(), skill); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to record Fleet skill source")
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusOK, skill)
			return
		}
		if r.Method != http.MethodPut {
			writeError(w, http.StatusConflict, "a different Fleet catalog revision already exists")
			return
		}
		skill.CreatedAt, skill.UpdatedAt = existing.CreatedAt, time.Now().UTC()
		if err := s.store.UpdateFleetSkill(r.Context(), skill); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to replace Fleet skill catalog revision")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, skill)
		return
	} else if !errors.Is(existingErr, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "failed to inspect Fleet skill catalog")
		return
	}
	now := time.Now().UTC()
	skill.CreatedAt, skill.UpdatedAt = now, now
	if err := s.store.CreateFleetSkill(r.Context(), skill); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to copy Hermes skill to Fleet catalog")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, skill)
}
