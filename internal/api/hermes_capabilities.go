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

func (s *Server) listHermesCapabilities(w http.ResponseWriter, r *http.Request) {
	instanceID := strings.TrimSpace(r.PathValue("instanceID"))
	if instanceID == "" {
		writeError(w, http.StatusBadRequest, "a Hermes instance is required")
		return
	}
	inventory, err := s.store.HermesCapabilityInventory(r.Context(), instanceID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "Hermes instance not found")
		return
	}
	if err != nil {
		s.logger.Error("list Hermes capabilities", "error", err, "instance_id", instanceID)
		writeError(w, http.StatusInternalServerError, "failed to load Hermes capabilities")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, inventory)
}

func (s *Server) refreshHermesCapabilities(w http.ResponseWriter, r *http.Request) {
	instanceID := strings.TrimSpace(r.PathValue("instanceID"))
	instance, err := s.store.GetInstance(r.Context(), instanceID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "Hermes instance not found")
		return
	}
	if err != nil {
		s.logger.Error("get Hermes capability target", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to load Hermes instance")
		return
	}
	if instance.Status != domain.InstanceRunning || instance.ProjectName == "" || instance.ManagedPath == "" ||
		instance.APIPort < 1 || instance.APIPort > 65535 {
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
		writeError(w, http.StatusConflict, "Hermes capability inspection requires Host Agent "+agentVersion)
		return
	}
	operationID, jobID, err := twoIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to allocate Hermes capability operation identity")
		return
	}
	payload, err := json.Marshal(domain.HermesCapabilityInspectPayload{
		InstanceID: instance.ID, Name: instance.Name, ProjectName: instance.ProjectName,
		ManagedPath: instance.ManagedPath, APIPort: instance.APIPort,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to prepare Hermes capability operation")
		return
	}
	operation := domain.Operation{
		ID: operationID, InstanceID: instance.ID, Actor: "Fleet admin", Type: "REFRESH_HERMES_CAPABILITIES",
		Status: domain.OperationPending, Summary: "Refresh Hermes capabilities for " + instance.Name,
		CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: jobID, OperationID: operation.ID, HostID: instance.HostID, InstanceID: instance.ID,
		Type: domain.JobInspectHermesCapabilities, Status: domain.JobPending, Payload: payload,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.QueueRunningInspection(r.Context(), operation, job); err != nil {
		if s.writeQueueAdmissionError(w, err) {
			return
		}
		if errors.Is(err, store.ErrInstanceBusy) || errors.Is(err, store.ErrStateChanged) {
			writeError(w, http.StatusConflict, "the Hermes instance is busy or changed before capability inspection could start")
			return
		}
		s.logger.Error("queue Hermes capability operation", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to queue Hermes capability operation")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusAccepted, operation)
}
