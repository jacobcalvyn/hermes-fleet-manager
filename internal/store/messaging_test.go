package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func TestMessagingFailureReachesTerminalState(t *testing.T) {
	tests := []struct {
		name               string
		resultStatus       string
		wantInstanceStatus string
	}{
		{
			name:               "runtime restored",
			resultStatus:       domain.InstanceRunning,
			wantInstanceStatus: domain.InstanceRunning,
		},
		{
			name:               "rollback failed",
			resultStatus:       domain.InstanceFailed,
			wantInstanceStatus: domain.InstanceFailed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, dataStore, host, instance, operation, queuedJob, config := newQueuedMessagingFixture(t, strings.ReplaceAll(test.name, " ", "-"))
			claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
			if err != nil || claimed == nil {
				t.Fatalf("ClaimJob() job=%v error=%v", claimed, err)
			}
			if claimed.ID != queuedJob.ID {
				t.Fatalf("ClaimJob() id=%q, want %q", claimed.ID, queuedJob.ID)
			}
			if err := dataStore.AcknowledgeJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
				t.Fatal(err)
			}

			result := domain.JobResult{
				Success:        false,
				Error:          "messaging apply failed",
				InstanceStatus: test.resultStatus,
			}
			if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, result, nil); err != nil {
				t.Fatalf("CompleteJob() error=%v", err)
			}

			record, err := dataStore.GetMessagingConfig(ctx, instance.ID)
			if err != nil {
				t.Fatal(err)
			}
			if record.Status != "FAILED" || record.LastError != result.Error ||
				record.DesiredRevision != config.DesiredRevision ||
				record.Ciphertext != config.Ciphertext ||
				record.AppliedRevision != "" || record.AppliedAt != nil {
				t.Fatalf("terminal messaging record=%+v", record)
			}
			storedInstance, err := dataStore.GetInstance(ctx, instance.ID)
			if err != nil {
				t.Fatal(err)
			}
			if storedInstance.Status != test.wantInstanceStatus || storedInstance.LastError != result.Error {
				t.Fatalf("terminal instance=%+v", storedInstance)
			}
			storedOperation, err := dataStore.GetOperation(ctx, operation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if storedOperation.Status != domain.OperationFailed || storedOperation.Error != result.Error {
				t.Fatalf("terminal operation=%+v", storedOperation)
			}

			beforeRetry := record
			if _, err := dataStore.db.ExecContext(ctx, `
UPDATE jobs SET lease_expires_at=? WHERE id=?`, time.Now().UTC().Add(-time.Minute), claimed.ID); err != nil {
				t.Fatal(err)
			}
			if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, result, nil); err != nil {
				t.Fatalf("identical terminal retry error=%v", err)
			}
			if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, domain.JobResult{
				Success: true, InstanceStatus: domain.InstanceRunning,
			}, nil); !errors.Is(err, ErrStateChanged) {
				t.Fatalf("contradictory terminal retry error=%v, want %v", err, ErrStateChanged)
			}
			afterRetry, err := dataStore.GetMessagingConfig(ctx, instance.ID)
			if err != nil {
				t.Fatal(err)
			}
			if afterRetry.Status != beforeRetry.Status || afterRetry.LastError != beforeRetry.LastError ||
				!afterRetry.UpdatedAt.Equal(beforeRetry.UpdatedAt) {
				t.Fatalf("terminal retry replayed messaging side effects: before=%+v after=%+v", beforeRetry, afterRetry)
			}
		})
	}
}

func TestExhaustedMessagingLeaseMarksConfigurationFailed(t *testing.T) {
	ctx, dataStore, host, instance, operation, queuedJob, config := newQueuedMessagingFixture(t, "lease-exhausted")
	var claimed *domain.Job
	for attempt := 1; attempt <= jobLeaseMaxClaims; attempt++ {
		if claimed != nil {
			if _, err := dataStore.db.ExecContext(ctx, `
UPDATE jobs SET lease_expires_at=? WHERE id=?`, time.Now().UTC().Add(-time.Minute), claimed.ID); err != nil {
				t.Fatal(err)
			}
		}
		var err error
		claimed, err = dataStore.ClaimJob(ctx, host.ID, time.Minute)
		if err != nil || claimed == nil {
			t.Fatalf("ClaimJob() attempt=%d job=%v error=%v", attempt, claimed, err)
		}
		if claimed.ID != queuedJob.ID || claimed.Attempts != attempt {
			t.Fatalf("ClaimJob() attempt=%d job=%+v", attempt, claimed)
		}
	}
	if _, err := dataStore.db.ExecContext(ctx, `
UPDATE jobs SET lease_expires_at=? WHERE id=?`, time.Now().UTC().Add(-time.Minute), claimed.ID); err != nil {
		t.Fatal(err)
	}
	next, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || next != nil {
		t.Fatalf("ClaimJob() after retry budget job=%v error=%v", next, err)
	}

	record, err := dataStore.GetMessagingConfig(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != "FAILED" || !strings.Contains(record.LastError, "manual retry is required") ||
		record.DesiredRevision != config.DesiredRevision || record.Ciphertext != config.Ciphertext {
		t.Fatalf("exhausted messaging record=%+v", record)
	}
	storedOperation, err := dataStore.GetOperation(ctx, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedOperation.Status != domain.OperationFailed ||
		!strings.Contains(storedOperation.Error, "manual retry is required") {
		t.Fatalf("exhausted operation=%+v", storedOperation)
	}
}

func newQueuedMessagingFixture(
	t *testing.T,
	suffix string,
) (
	context.Context,
	*Store,
	domain.Host,
	domain.Instance,
	domain.Operation,
	domain.Job,
	MessagingConfigRecord,
) {
	t.Helper()
	ctx, dataStore, host, instance := newFleetFixture(t, "messaging-"+suffix)
	provisionJob, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || provisionJob == nil {
		t.Fatalf("ClaimJob(provision) job=%v error=%v", provisionJob, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, provisionJob.ID, provisionJob.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CompleteJob(
		ctx,
		host.ID,
		provisionJob.ID,
		provisionJob.LeaseToken,
		successfulProvisionResult(instance),
		nil,
	); err != nil {
		t.Fatal(err)
	}
	instance, err = dataStore.GetInstance(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	revision := strings.Repeat("a", 64)
	payload, err := json.Marshal(domain.MessagingApplyPayload{
		InstanceID: instance.ID, Name: instance.Name, Image: instance.Image, ImageID: instance.ImageID,
		Provider: instance.Provider, Model: instance.Model, Reasoning: instance.Reasoning,
		ServiceTier: instance.ServiceTier, ProjectName: instance.ProjectName, DataVolume: instance.DataVolume,
		ManagedPath: instance.ManagedPath, DesiredStatus: domain.InstanceRunning,
		APIPort: instance.APIPort, DashboardPort: instance.DashboardPort, Revision: revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	operation := domain.Operation{
		ID: "operation-apply-messaging-" + suffix, InstanceID: instance.ID,
		Type: "CONFIGURE_MESSAGING", Status: domain.OperationPending,
		Summary: "Configure messaging", CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: "job-apply-messaging-" + suffix, OperationID: operation.ID, HostID: host.ID, InstanceID: instance.ID,
		Type: "instance.messaging.configure", Status: domain.JobPending, Payload: payload,
		CreatedAt: now, UpdatedAt: now,
	}
	config := MessagingConfigRecord{
		InstanceID: instance.ID, Ciphertext: "encrypted-config", DesiredRevision: revision,
		Status: "PENDING", UpdatedAt: now,
	}
	if err := dataStore.QueueMessagingConfiguration(
		ctx,
		domain.InstanceRunning,
		config,
		operation,
		job,
	); err != nil {
		t.Fatal(err)
	}
	return ctx, dataStore, host, instance, operation, job, config
}
