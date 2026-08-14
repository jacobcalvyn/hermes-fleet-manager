package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/recovery"
)

type hermesUpdateQueueError struct {
	Stage string
	Err   error
}

func (err *hermesUpdateQueueError) Error() string {
	return fmt.Sprintf("%s: %v", err.Stage, err.Err)
}

func (err *hermesUpdateQueueError) Unwrap() error {
	return err.Err
}

func (s *Server) queueHermesUpdate(
	ctx context.Context,
	instance domain.Instance,
	host domain.Host,
	status hermesUpdateResponse,
	restoreStatus string,
	workflowID string,
	actor string,
) (domain.Operation, error) {
	operationID, jobID, err := twoIDs()
	if err != nil {
		return domain.Operation{}, &hermesUpdateQueueError{Stage: "identity", Err: err}
	}
	if workflowID == "" {
		workflowID = operationID
	}
	if actor == "" {
		actor = "FLEET_ADMIN"
	}
	backup, err := s.config.RecoveryPoints.Reserve(ctx, recovery.Reservation{
		InstanceID: instance.ID, InstanceName: instance.Name, HostID: instance.HostID,
		OperationID: operationID, JobID: jobID, Image: instance.Image, ImageID: instance.ImageID,
		Provider: instance.Provider, Model: instance.Model, Reasoning: instance.Reasoning,
		ServiceTier: instance.ServiceTier, CodexConfigured: instance.CodexConfigured,
		ProjectName: instance.ProjectName, DataVolume: instance.DataVolume,
		ManagedPath: instance.ManagedPath, AgentVersion: host.AgentVersion,
		Automated: true, WorkflowID: workflowID,
	})
	if err != nil {
		return domain.Operation{}, &hermesUpdateQueueError{Stage: "backup", Err: err}
	}
	backupPayload := domain.RecoveryPointPayload{
		RecoveryPointID: backup.ID, InstanceID: instance.ID, Name: instance.Name, Image: instance.Image,
		ImageID: instance.ImageID, Provider: instance.Provider, Model: instance.Model, Reasoning: instance.Reasoning,
		ServiceTier: instance.ServiceTier, CodexConfigured: instance.CodexConfigured,
		ProjectName: instance.ProjectName, DataVolume: instance.DataVolume,
		ManagedPath: instance.ManagedPath, AgentVersion: host.AgentVersion, CreatedAt: backup.CreatedAt,
		MaxBytes: s.config.MaxRecoveryPointBytes,
	}
	upgradePayload := domain.HermesUpgradePayload{
		InstanceID: instance.ID, Name: instance.Name, CurrentImage: instance.Image, CurrentImageID: instance.ImageID,
		TargetImage: status.TargetImage, TargetVersion: status.TargetVersion, TargetSource: status.TargetSource,
		RecoveryPointID: backup.ID, Provider: instance.Provider, Model: instance.Model,
		Reasoning: instance.Reasoning, ServiceTier: instance.ServiceTier, CodexConfigured: instance.CodexConfigured,
		ProjectName: instance.ProjectName, DataVolume: instance.DataVolume, ManagedPath: instance.ManagedPath,
		APIPort: instance.APIPort, DashboardPort: instance.DashboardPort,
		Rollback: domain.RecoveryRestorePayload{
			RecoveryPointID: backup.ID, InstanceID: instance.ID, Name: instance.Name, Image: backup.Image,
			ImageID: backup.ImageID, RequireImageID: true, Provider: backup.Provider, Model: backup.Model, Reasoning: backup.Reasoning,
			ServiceTier: backup.ServiceTier, CodexConfigured: backup.CodexConfigured,
			ProjectName: backup.ProjectName, DataVolume: backup.DataVolume, ManagedPath: backup.ManagedPath,
			AgentVersion: backup.AgentVersion, CreatedAt: backup.CreatedAt, RecoverySHA256: backup.SHA256,
			RecoverySizeBytes: backup.SizeBytes, MaxBytes: s.config.MaxRecoveryPointBytes,
		},
	}
	payload, err := json.Marshal(domain.HermesUpdatePayload{
		Upgrade: upgradePayload, Backup: backupPayload, OriginalStatus: restoreStatus,
	})
	if err != nil {
		_ = s.config.RecoveryPoints.Abort(backup.ID, jobID)
		return domain.Operation{}, &hermesUpdateQueueError{Stage: "encode", Err: err}
	}
	now := time.Now().UTC()
	summary := "Update Hermes " + instance.Name + " to " + status.TargetVersion
	if status.UpdateKind == hermesUpdateKindRuntimeRefresh {
		summary = "Refresh managed Hermes runtime for " + instance.Name
	}
	operation := domain.Operation{
		ID: operationID, InstanceID: instance.ID, WorkflowID: workflowID, Actor: actor,
		Type: "UPGRADE_HERMES", Status: domain.OperationPending,
		Summary: summary, CreatedAt: now, UpdatedAt: now,
		Metadata: operationMetadata(map[string]any{
			"from_version": status.CurrentVersion, "to_version": status.TargetVersion,
			"backup_id": backup.ID, "target_image": status.TargetImage, "initial_status": instance.Status,
			"original_status": restoreStatus,
			"update_kind":     status.UpdateKind,
		}),
	}
	job := domain.Job{
		ID: jobID, OperationID: operationID, HostID: instance.HostID, InstanceID: instance.ID,
		Type: "instance.hermes.update", Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.QueueAction(ctx, instance.Status, domain.InstanceUpdating, operation, job); err != nil {
		if abortErr := s.config.RecoveryPoints.Abort(backup.ID, jobID); abortErr != nil {
			s.logger.Error("abort unqueued automatic update backup", "error", abortErr)
		}
		return domain.Operation{}, &hermesUpdateQueueError{Stage: "queue", Err: err}
	}
	return operation, nil
}
