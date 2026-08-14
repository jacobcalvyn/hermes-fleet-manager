package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func TestQueueAdmissionRejectsWorkAtTheHostBoundary(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "queue-admission")
	now := time.Now().UTC()
	tx, err := dataStore.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for index := 1; index < JobQueueMaxPerHost; index++ {
		operation := domain.Operation{
			ID: fmt.Sprintf("operation-capacity-%03d", index), InstanceID: instance.ID,
			Type: "TEST", Status: domain.OperationPending, Summary: "Capacity test",
			CreatedAt: now.Add(time.Duration(index) * time.Millisecond), UpdatedAt: now,
		}
		job := domain.Job{
			ID: fmt.Sprintf("job-capacity-%03d", index), OperationID: operation.ID,
			HostID: host.ID, InstanceID: instance.ID, Type: "instance.credentials.inspect",
			Status: domain.JobPending, Payload: json.RawMessage(`{}`), CreatedAt: operation.CreatedAt, UpdatedAt: now,
		}
		if err := insertOperationAndJob(ctx, tx, operation, job); err != nil {
			t.Fatalf("fill queue item %d: %v", index, err)
		}
	}
	overflowOperation := domain.Operation{
		ID: "operation-capacity-overflow", InstanceID: instance.ID, Type: "TEST",
		Status: domain.OperationPending, Summary: "Overflow", CreatedAt: now, UpdatedAt: now,
	}
	overflowJob := domain.Job{
		ID: "job-capacity-overflow", OperationID: overflowOperation.ID, HostID: host.ID,
		InstanceID: instance.ID, Type: "instance.credentials.inspect", Status: domain.JobPending,
		Payload: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	}
	if err := insertOperationAndJob(ctx, tx, overflowOperation, overflowJob); !errors.Is(err, ErrQueueCapacity) {
		t.Fatalf("overflow error = %v; want ErrQueueCapacity", err)
	}
}

func TestClaimJobPrioritizesSafetyWork(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "queue-priority")
	now := time.Now().UTC().Add(time.Minute)
	tx, err := dataStore.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	operation := domain.Operation{
		ID: "operation-priority-stop", InstanceID: instance.ID, Type: "STOP",
		Status: domain.OperationPending, Summary: "Stop", CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: "job-priority-stop", OperationID: operation.ID, HostID: host.ID, InstanceID: instance.ID,
		Type: "instance.stop", Status: domain.JobPending, Payload: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	}
	if err := insertOperationAndJob(ctx, tx, operation, job); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil || claimed.ID != job.ID {
		t.Fatalf("claimed = %+v, error = %v; want safety stop job", claimed, err)
	}
}

func TestExpiredLeaseReconciliationDoesNotRequireAnotherClaim(t *testing.T) {
	ctx, dataStore, _, instance := newFleetFixture(t, "lease-reconcile")
	now := time.Now().UTC()
	if _, err := dataStore.db.ExecContext(ctx, `
UPDATE jobs SET status=?, attempts=?, lease_token='expired', lease_expires_at=?, updated_at=?
WHERE instance_id=?`, domain.JobRunning, jobLeaseMaxClaims, now.Add(-time.Minute), now.Add(-time.Minute), instance.ID); err != nil {
		t.Fatal(err)
	}
	count, err := dataStore.ReconcileExpiredJobs(ctx, now)
	if err != nil || count != 1 {
		t.Fatalf("ReconcileExpiredJobs() count=%d error=%v; want 1", count, err)
	}
	var status string
	if err := dataStore.db.QueryRowContext(ctx, `SELECT status FROM jobs WHERE instance_id=?`, instance.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != domain.JobFailed {
		t.Fatalf("job status=%q; want FAILED", status)
	}
}
