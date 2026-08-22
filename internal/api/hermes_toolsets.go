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

type setHermesToolsetRequest struct {
	Profile string `json:"profile"`
	Enabled bool   `json:"enabled"`
}

func (s *Server) setHermesToolset(w http.ResponseWriter, r *http.Request) {
	instanceID := strings.TrimSpace(r.PathValue("instanceID"))
	toolsetName := strings.TrimSpace(r.PathValue("toolsetName"))
	var request setHermesToolsetRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.Profile = strings.TrimSpace(request.Profile)
	if request.Profile == "" {
		request.Profile = "default"
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
	payloadValue := domain.HermesToolsetMutationPayload{
		HermesProfileInspectPayload: domain.HermesProfileInspectPayload{
			InstanceID: instance.ID, Name: instance.Name, ProjectName: instance.ProjectName,
			ManagedPath: instance.ManagedPath, DashboardPort: instance.DashboardPort,
		},
		ToolsetName: toolsetName, Profile: request.Profile, Enabled: request.Enabled,
	}
	if err := domain.ValidateHermesToolsetMutationPayload(&payloadValue); err != nil {
		writeError(w, http.StatusBadRequest, "a valid toolset and target profile are required")
		return
	}
	payload, err := json.Marshal(payloadValue)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to prepare toolset mutation")
		return
	}
	operationID, jobID, err := twoIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to allocate toolset mutation identity")
		return
	}
	metadata, _ := json.Marshal(map[string]any{"toolset_name": toolsetName, "profile": request.Profile, "enabled": request.Enabled})
	action := "Disable"
	if request.Enabled {
		action = "Enable"
	}
	operation := domain.Operation{
		ID: operationID, InstanceID: instance.ID, Actor: "Fleet admin", Type: "SET_HERMES_TOOLSET",
		Status: domain.OperationPending, Summary: action + " Hermes toolset " + toolsetName + " on " + instance.Name,
		Metadata: metadata, CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: jobID, OperationID: operation.ID, HostID: instance.HostID, InstanceID: instance.ID,
		Type: domain.JobSetHermesToolset, Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.QueueRunningInspection(r.Context(), operation, job); err != nil {
		if s.writeQueueAdmissionError(w, err) {
			return
		}
		if errors.Is(err, store.ErrInstanceBusy) || errors.Is(err, store.ErrStateChanged) {
			writeError(w, http.StatusConflict, "the Hermes instance is busy or changed before the toolset mutation could start")
			return
		}
		s.logger.Error("queue Hermes toolset mutation", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to queue Hermes toolset mutation")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusAccepted, operation)
}
