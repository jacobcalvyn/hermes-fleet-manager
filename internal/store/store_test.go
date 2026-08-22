package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func TestQuarantineInvalidSQLiteSharedMemorySidecarAllowsReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.db")
	dataStore, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := dataStore.Close(); err != nil {
		t.Fatal(err)
	}
	sharedMemoryPath := path + "-shm"
	if err := os.WriteFile(sharedMemoryPath, []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-wal", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	quarantinedPath, err := quarantineInvalidSQLiteSHM(path)
	if err != nil {
		t.Fatal(err)
	}

	dataStore, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	var result string
	if err := dataStore.db.QueryRow(`PRAGMA quick_check`).Scan(&result); err != nil {
		t.Fatal(err)
	}
	if result != "ok" {
		t.Fatalf("quick_check=%q, want ok", result)
	}
	contents, err := os.ReadFile(quarantinedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "bad" {
		t.Fatalf("quarantined contents=%q, want bad", contents)
	}
}

func TestPrepareCleanHostRecoveryFencesWorkAndStopsInstances(t *testing.T) {
	ctx := context.Background()
	dataStore, err := Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	now := time.Now().UTC()
	if _, err := dataStore.db.Exec(`
INSERT INTO hosts (id,name,hostname,os,arch,agent_version,token_hash,last_seen_at,created_at)
VALUES ('host-1','host','host','linux','amd64','0.12.1','hash',?,?);
INSERT INTO instances (id,name,host_id,status,image,image_id,provider,model,reasoning,service_tier,codex_configured,api_port,dashboard_port,project_name,data_volume,managed_path,created_at,updated_at)
VALUES ('instance-1','fleet-test','host-1','RUNNING','runtime:test',?,'openai-codex','model','medium','normal',1,8650,9130,'project','volume','/managed/instance',?,?);
INSERT INTO operations (id,instance_id,type,status,summary,created_at,updated_at)
VALUES ('operation-1','instance-1','TEST','RUNNING','test',?,?);
INSERT INTO jobs (id,operation_id,host_id,instance_id,type,status,payload,lease_token,lease_expires_at,created_at,updated_at)
VALUES ('job-1','operation-1','host-1','instance-1','instance.start','RUNNING','{}','lease',?, ?, ?);
INSERT INTO instance_observations (instance_id,host_id,target_generation,status,summary,checks,observed_at,received_at)
VALUES ('instance-1','host-1','generation','IN_SYNC','ok','[]',?,?)`,
		now, now, "sha256:"+strings.Repeat("a", 64), now, now, now, now, now.Add(time.Minute), now, now, now, now); err != nil {
		t.Fatal(err)
	}
	preparedAt := now.Add(2 * time.Minute)
	if err := dataStore.PrepareCleanHostRecovery(ctx, preparedAt); err != nil {
		t.Fatal(err)
	}
	var instanceStatus, jobStatus, leaseToken, operationStatus, operationError string
	if err := dataStore.db.QueryRow(`SELECT status FROM instances WHERE id='instance-1'`).Scan(&instanceStatus); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.db.QueryRow(`SELECT status,lease_token FROM jobs WHERE id='job-1'`).Scan(&jobStatus, &leaseToken); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.db.QueryRow(`SELECT status,error FROM operations WHERE id='operation-1'`).Scan(&operationStatus, &operationError); err != nil {
		t.Fatal(err)
	}
	if instanceStatus != domain.InstanceStopped || jobStatus != domain.JobFailed || leaseToken != "" || operationStatus != domain.OperationFailed || operationError == "" {
		t.Fatalf("recovered state instance=%s job=%s lease=%q operation=%s error=%q", instanceStatus, jobStatus, leaseToken, operationStatus, operationError)
	}
	var observations int
	if err := dataStore.db.QueryRow(`SELECT COUNT(*) FROM instance_observations`).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if observations != 0 {
		t.Fatalf("restored observations=%d, want 0", observations)
	}
}

func TestReplacementSQLiteConnectionRetainsRequiredPragmas(t *testing.T) {
	dataStore, err := Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()

	assertPragmas := func() {
		t.Helper()
		var foreignKeys int
		var busyTimeout int
		var journalMode string
		if err := dataStore.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
			t.Fatal(err)
		}
		if err := dataStore.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
			t.Fatal(err)
		}
		if err := dataStore.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
			t.Fatal(err)
		}
		if foreignKeys != 1 || busyTimeout != 5000 || strings.ToLower(journalMode) != "wal" {
			t.Fatalf("sqlite pragmas foreign_keys=%d busy_timeout=%d journal_mode=%q", foreignKeys, busyTimeout, journalMode)
		}
	}

	assertPragmas()
	connection, err := dataStore.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Raw(func(any) error { return driver.ErrBadConn }); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("discard connection error=%v, want driver.ErrBadConn", err)
	}
	_ = connection.Close()
	assertPragmas()
}

func TestLegacyChatEventSchemaAcceptsStructuredHermesEventsAfterMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-chat-events.db")
	dataStore, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := dataStore.db.ExecContext(ctx, `
INSERT INTO hosts (id,name,hostname,os,arch,agent_version,token_hash,last_seen_at,created_at)
VALUES ('chat-host','chat-host','chat-host','linux','amd64','0.12.1','hash',?,?);
INSERT INTO instances (id,name,host_id,status,image,provider,model,reasoning,service_tier,api_port,dashboard_port,created_at,updated_at)
VALUES ('chat-instance','chat-instance','chat-host','RUNNING','runtime:test','openai-codex','','','',8650,9130,?,?);
INSERT INTO operations (id,instance_id,type,status,summary,created_at,updated_at)
VALUES ('chat-operation','chat-instance','CHAT_MESSAGE','RUNNING','Chat migration',?,?);
INSERT INTO chat_sessions (id,instance_id,title,status,created_at,updated_at)
VALUES ('chat-session','chat-instance','Migration chat','ACTIVE',?,?);
INSERT INTO chat_events (session_id,operation_id,sequence,type,created_at)
VALUES ('chat-session','chat-operation',0,'RUN_QUEUED',?);`, now, now, now, now, now, now, now, now, now); err != nil {
		dataStore.Close()
		t.Fatal(err)
	}
	if err := dataStore.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
DROP INDEX idx_chat_events_session_cursor;
CREATE TABLE chat_events_legacy (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
  operation_id TEXT NOT NULL REFERENCES operations(id) ON DELETE CASCADE,
  sequence INTEGER NOT NULL CHECK(sequence >= 0),
  type TEXT NOT NULL CHECK(type IN ('RUN_QUEUED', 'RUN_STARTED', 'ASSISTANT_DELTA', 'RUN_COMPLETED', 'RUN_FAILED', 'RUN_CANCELED')),
  ciphertext TEXT NOT NULL DEFAULT '',
  content_hash TEXT NOT NULL DEFAULT '',
  created_at DATETIME NOT NULL,
  UNIQUE(operation_id, sequence)
);
INSERT INTO chat_events_legacy
  (id,session_id,operation_id,sequence,type,ciphertext,content_hash,created_at)
SELECT id,session_id,operation_id,sequence,type,ciphertext,content_hash,created_at FROM chat_events;
DROP TABLE chat_events;
ALTER TABLE chat_events_legacy RENAME TO chat_events;
CREATE INDEX idx_chat_events_session_cursor ON chat_events(session_id, id);
DELETE FROM schema_migrations WHERE name='20260812_chat_event_payloads_v1';`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	dataStore, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	var queuedEvents int
	if err := dataStore.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_events WHERE type='RUN_QUEUED'`).Scan(&queuedEvents); err != nil {
		t.Fatal(err)
	}
	if queuedEvents != 1 {
		t.Fatalf("preserved queued events=%d, want 1", queuedEvents)
	}
	for sequence, eventType := range []string{domain.ChatEventActivity, domain.ChatEventArtifact} {
		if _, err := dataStore.db.ExecContext(ctx, `
INSERT INTO chat_events (session_id,operation_id,sequence,type,created_at)
VALUES ('chat-session','chat-operation',?,?,?)`, sequence+1, eventType, now.Add(time.Duration(sequence+1)*time.Second)); err != nil {
			t.Fatalf("insert migrated event %s: %v", eventType, err)
		}
	}
	var applied bool
	if err := dataStore.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=?)`, chatEventPayloadMigration).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("chat event payload migration was not recorded")
	}
}

func TestReadyRecoversAfterInterruptedSQLiteQuery(t *testing.T) {
	dataStore, err := Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()

	queryContext, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	var sum int64
	err = dataStore.db.QueryRowContext(queryContext, `
WITH RECURSIVE numbers(value) AS (
  VALUES(0)
  UNION ALL
  SELECT value + 1 FROM numbers WHERE value < 1000000000
)
SELECT sum(value) FROM numbers
`).Scan(&sum)
	if err == nil {
		t.Fatal("long-running sqlite query unexpectedly completed before cancellation")
	}

	readyContext, readyCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readyCancel()
	if err := dataStore.Ready(readyContext); err != nil {
		t.Fatalf("Ready() did not recover the interrupted sqlite connection: %v", err)
	}
}

func TestRotateHostCredentialPreservesIdentityAndUpdatesCredential(t *testing.T) {
	ctx := context.Background()
	dataStore, err := Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()

	createdAt := time.Date(2026, 7, 27, 1, 0, 0, 0, time.UTC)
	host := domain.Host{
		ID: "host-rotate", Name: "local-mac", Hostname: "mac.local", OS: "darwin", Arch: "arm64",
		AgentVersion: "0.9.0", LastSeenAt: createdAt, CreatedAt: createdAt,
	}
	if err := dataStore.EnrollHost(ctx, host, "old-token-hash"); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.RotateHostCredential(
		ctx, host.ID, host.Name, host.Hostname, host.OS, host.Arch, "new-token-hash",
	); err != nil {
		t.Fatal(err)
	}

	hash, err := dataStore.HostTokenHash(ctx, host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hash != "new-token-hash" {
		t.Fatalf("token hash=%q, want new-token-hash", hash)
	}
	stored, err := dataStore.GetHost(ctx, host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != host.ID || stored.Name != host.Name || stored.Hostname != host.Hostname ||
		stored.OS != host.OS || stored.Arch != host.Arch || stored.AgentVersion != host.AgentVersion ||
		!stored.CreatedAt.Equal(createdAt) || !stored.LastSeenAt.Equal(createdAt) {
		t.Fatalf("rotated host identity changed unexpectedly: %+v", stored)
	}
}

func TestRotateHostCredentialRejectsIdentityMismatchWithoutMutation(t *testing.T) {
	tests := []struct {
		name        string
		hostID      string
		confirmName string
		hostname    string
		osName      string
		arch        string
		wantErr     error
	}{
		{name: "unknown host", hostID: "missing", confirmName: "local-mac", hostname: "mac.local", osName: "darwin", arch: "arm64", wantErr: ErrNotFound},
		{name: "name", hostID: "host-rotate", confirmName: "other-mac", hostname: "mac.local", osName: "darwin", arch: "arm64", wantErr: ErrHostIdentityMismatch},
		{name: "hostname", hostID: "host-rotate", confirmName: "local-mac", hostname: "other.local", osName: "darwin", arch: "arm64", wantErr: ErrHostIdentityMismatch},
		{name: "os", hostID: "host-rotate", confirmName: "local-mac", hostname: "mac.local", osName: "linux", arch: "arm64", wantErr: ErrHostIdentityMismatch},
		{name: "arch", hostID: "host-rotate", confirmName: "local-mac", hostname: "mac.local", osName: "darwin", arch: "amd64", wantErr: ErrHostIdentityMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			dataStore, err := Open(filepath.Join(t.TempDir(), "fleet.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer dataStore.Close()
			now := time.Now().UTC()
			host := domain.Host{
				ID: "host-rotate", Name: "local-mac", Hostname: "mac.local", OS: "darwin", Arch: "arm64",
				AgentVersion: "0.9.0", LastSeenAt: now, CreatedAt: now,
			}
			if err := dataStore.EnrollHost(ctx, host, "old-token-hash"); err != nil {
				t.Fatal(err)
			}

			err = dataStore.RotateHostCredential(
				ctx, test.hostID, test.confirmName, test.hostname, test.osName, test.arch,
				"new-token-hash",
			)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("RotateHostCredential() error=%v, want %v", err, test.wantErr)
			}
			hash, err := dataStore.HostTokenHash(ctx, host.ID)
			if err != nil {
				t.Fatal(err)
			}
			if hash != "old-token-hash" {
				t.Fatalf("failed confirmation changed token hash to %q", hash)
			}
		})
	}
}

func TestRotateHostCredentialRejectsEveryNonterminalJobState(t *testing.T) {
	for _, status := range []string{domain.JobPending, domain.JobLeased, domain.JobRunning} {
		t.Run(strings.ToLower(status), func(t *testing.T) {
			suffix := "rotate-" + strings.ToLower(status)
			ctx, dataStore, host, _ := newFleetFixture(t, suffix)
			if _, err := dataStore.db.ExecContext(ctx, `UPDATE jobs SET status=? WHERE host_id=?`, status, host.ID); err != nil {
				t.Fatal(err)
			}
			err := dataStore.RotateHostCredential(
				ctx, host.ID, host.Name, host.Hostname, host.OS, host.Arch,
				"new-token-hash",
			)
			if !errors.Is(err, ErrHostBusy) {
				t.Fatalf("RotateHostCredential() error=%v, want ErrHostBusy", err)
			}
			hash, err := dataStore.HostTokenHash(ctx, host.ID)
			if err != nil {
				t.Fatal(err)
			}
			if hash != "token-hash" {
				t.Fatalf("busy host token hash=%q, want original hash", hash)
			}
		})
	}
}

func TestInstanceJobLifecycle(t *testing.T) {
	ctx := context.Background()
	dataStore, err := Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	now := time.Now().UTC()
	host := domain.Host{ID: "host-1", Name: "local-mac", Hostname: "mac", OS: "darwin", Arch: "arm64", AgentVersion: "test", LastSeenAt: now, CreatedAt: now}
	if err := dataStore.EnrollHost(ctx, host, "token-hash"); err != nil {
		t.Fatal(err)
	}
	instance := domain.Instance{ID: "instance-1", Name: "fleet-test-01", HostID: host.ID, Status: domain.InstanceProvisioning, Image: "image", Provider: "openai-codex", Model: "gpt", Reasoning: "medium", ServiceTier: "normal", CodexConfigured: true, APIPort: 8650, DashboardPort: 9130, CreatedAt: now, UpdatedAt: now}
	operation := domain.Operation{
		ID: "operation-1", InstanceID: instance.ID, WorkflowID: "00000000-0000-4000-8000-000000000001",
		Actor: "FLEET_ADMIN", Type: "PROVISION", Status: domain.OperationPending, Summary: "Provision",
		Metadata: json.RawMessage(`{"workflow_step":"create"}`), CreatedAt: now, UpdatedAt: now,
	}
	payload, _ := json.Marshal(domain.ProvisionPayload{InstanceID: instance.ID, Name: instance.Name})
	job := domain.Job{ID: "job-1", OperationID: operation.ID, HostID: host.ID, InstanceID: instance.ID, Type: "instance.provision", Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now}
	if err := dataStore.CreateInstance(ctx, instance, operation, job); err != nil {
		t.Fatal(err)
	}
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimJob() job=%v error=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	result := successfulProvisionResult(instance)
	if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, result, nil); err != nil {
		t.Fatal(err)
	}
	stored, err := dataStore.GetInstance(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.InstanceRunning || stored.DataVolume != result.DataVolume {
		t.Fatalf("unexpected instance after completion: %+v", stored)
	}
	storedOperation, err := dataStore.GetOperation(ctx, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	var storedMetadata map[string]any
	if err := json.Unmarshal(storedOperation.Metadata, &storedMetadata); err != nil {
		t.Fatal(err)
	}
	if storedOperation.WorkflowID != operation.WorkflowID || storedOperation.Actor != operation.Actor || storedMetadata["workflow_step"] != "create" || storedMetadata["attempt"] != float64(1) {
		t.Fatalf("operation workflow context was not preserved: %+v", storedOperation)
	}
}

func TestCompletedJobAcknowledgesOnlyOriginalLeaseAndIdenticalResult(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "completion-retry")
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimJob() job=%v error=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	result := successfulProvisionResult(instance)
	if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, result, nil); err != nil {
		t.Fatal(err)
	}
	wantCompletionHash, err := JobResultCompletionHash(result)
	if err != nil {
		t.Fatal(err)
	}
	var storedCompletionHash string
	if err := dataStore.db.QueryRowContext(ctx, `SELECT completion_hash FROM jobs WHERE id=?`, claimed.ID).
		Scan(&storedCompletionHash); err != nil {
		t.Fatal(err)
	}
	if storedCompletionHash != wantCompletionHash {
		t.Fatalf("completion_hash=%q, want %q", storedCompletionHash, wantCompletionHash)
	}

	storedBefore, err := dataStore.GetInstance(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	operationBefore, err := dataStore.GetOperation(ctx, claimed.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	_, _, completionStatus, err := dataStore.CompletionJobMetadata(ctx, host.ID, claimed.ID, claimed.LeaseToken)
	if err != nil {
		t.Fatalf("CompletionJobMetadata() rejected the original terminal lease: %v", err)
	}
	if completionStatus != domain.JobSucceeded {
		t.Fatalf("CompletionJobMetadata() status=%q, want %q", completionStatus, domain.JobSucceeded)
	}
	var completionLeaseExpiresAt time.Time
	if err := dataStore.db.QueryRowContext(ctx, `SELECT lease_expires_at FROM jobs WHERE id=?`, claimed.ID).
		Scan(&completionLeaseExpiresAt); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.RenewJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatalf("RenewJob() rejected the original terminal lease during response retry: %v", err)
	}
	var renewedTerminalLeaseExpiresAt time.Time
	if err := dataStore.db.QueryRowContext(ctx, `SELECT lease_expires_at FROM jobs WHERE id=?`, claimed.ID).
		Scan(&renewedTerminalLeaseExpiresAt); err != nil {
		t.Fatal(err)
	}
	if !renewedTerminalLeaseExpiresAt.Equal(completionLeaseExpiresAt) {
		t.Fatalf("terminal lease was extended from %s to %s", completionLeaseExpiresAt, renewedTerminalLeaseExpiresAt)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, result, nil); err != nil {
		t.Fatalf("identical duplicate CompleteJob() error=%v", err)
	}
	if err := dataStore.CompleteJob(
		ctx, host.ID, claimed.ID, claimed.LeaseToken,
		domain.JobResult{Success: false, Error: "contradictory duplicate"},
		&EncryptedReveal{Ciphertext: "must-not-be-stored", ExpiresAt: time.Now().UTC().Add(time.Minute)},
	); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("contradictory duplicate CompleteJob() error=%v, want %v", err, ErrStateChanged)
	}
	storedAfter, err := dataStore.GetInstance(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	operationAfter, err := dataStore.GetOperation(ctx, claimed.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if storedAfter.Status != storedBefore.Status || storedAfter.UpdatedAt != storedBefore.UpdatedAt ||
		operationAfter.Status != operationBefore.Status || operationAfter.UpdatedAt != operationBefore.UpdatedAt {
		t.Fatalf("duplicate completion replayed side effects: instance before=%+v after=%+v operation before=%+v after=%+v",
			storedBefore, storedAfter, operationBefore, operationAfter)
	}
	var revealCount int
	if err := dataStore.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM credential_reveals`).Scan(&revealCount); err != nil {
		t.Fatal(err)
	}
	if revealCount != 0 {
		t.Fatalf("contradictory duplicate stored %d credential reveals", revealCount)
	}

	if _, _, _, err := dataStore.CompletionJobMetadata(ctx, host.ID, claimed.ID, "different-token"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("CompletionJobMetadata(different token) error=%v, want %v", err, ErrLeaseLost)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, "different-token", result, nil); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("CompleteJob(different token) error=%v, want %v", err, ErrLeaseLost)
	}
	if err := dataStore.RenewJob(ctx, host.ID, claimed.ID, "different-token", time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("RenewJob(different token) error=%v, want %v", err, ErrLeaseLost)
	}

	if _, err := dataStore.db.ExecContext(ctx, `
UPDATE jobs SET lease_expires_at=? WHERE id=?`, time.Now().UTC().Add(-time.Minute), claimed.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, status, err := dataStore.CompletionJobMetadata(ctx, host.ID, claimed.ID, claimed.LeaseToken); err != nil ||
		status != domain.JobSucceeded {
		t.Fatalf("CompletionJobMetadata(expired original lease) status=%q error=%v", status, err)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, result, nil); err != nil {
		t.Fatalf("CompleteJob(expired original lease, identical result) error=%v", err)
	}
	if err := dataStore.CompleteJob(
		ctx, host.ID, claimed.ID, claimed.LeaseToken,
		domain.JobResult{Success: false, Error: "contradictory expired duplicate"}, nil,
	); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("CompleteJob(expired original lease, contradictory result) error=%v, want %v", err, ErrStateChanged)
	}
	if err := dataStore.RenewJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatalf("RenewJob(expired original terminal lease) error=%v", err)
	}
}

func TestInvalidCompletionHashIsInvalidJobResult(t *testing.T) {
	ctx, dataStore, host, _ := newFleetFixture(t, "invalid-completion-hash")
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimJob() job=%v error=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	err = dataStore.CompleteJobWithHash(
		ctx,
		host.ID,
		claimed.ID,
		claimed.LeaseToken,
		"not-a-sha256",
		domain.JobResult{Success: false, Error: "ignored"},
		nil,
	)
	if !errors.Is(err, ErrInvalidJobResult) {
		t.Fatalf("CompleteJobWithHash() error=%v, want %v", err, ErrInvalidJobResult)
	}
}

func TestStaleJobLeaseCannotMutateState(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "lease")
	first, err := dataStore.ClaimJob(ctx, host.ID, 10*time.Millisecond)
	if err != nil || first == nil {
		t.Fatalf("first ClaimJob() job=%v error=%v", first, err)
	}
	time.Sleep(25 * time.Millisecond)
	second, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || second == nil {
		t.Fatalf("second ClaimJob() job=%v error=%v", second, err)
	}
	if first.LeaseToken == second.LeaseToken {
		t.Fatal("reclaimed job reused its lease token")
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, first.ID, first.LeaseToken, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale AcknowledgeJob() error=%v, want ErrLeaseLost", err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, second.ID, second.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	result := successfulProvisionResult(instance)
	if err := dataStore.CompleteJob(ctx, host.ID, first.ID, first.LeaseToken, result, nil); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale CompleteJob() error=%v, want ErrLeaseLost", err)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, second.ID, second.LeaseToken, result, nil); err != nil {
		t.Fatal(err)
	}
	stored, err := dataStore.GetInstance(ctx, instance.ID)
	if err != nil || stored.ProjectName != result.ProjectName {
		t.Fatalf("current lease result was not stored: instance=%+v error=%v", stored, err)
	}
}

func TestExpiredJobLeaseStopsAfterThreeClaims(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "lease-exhausted")
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
		if claimed.Attempts != attempt {
			t.Fatalf("ClaimJob() attempt count=%d want=%d", claimed.Attempts, attempt)
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
	var jobStatus string
	var jobAttempts int
	if err := dataStore.db.QueryRowContext(ctx, `SELECT status, attempts FROM jobs WHERE id=?`, claimed.ID).
		Scan(&jobStatus, &jobAttempts); err != nil {
		t.Fatal(err)
	}
	if jobStatus != domain.JobFailed || jobAttempts != jobLeaseMaxClaims {
		t.Fatalf("exhausted job status=%q attempts=%d", jobStatus, jobAttempts)
	}
	operation, err := dataStore.GetOperation(ctx, claimed.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(operation.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if operation.Status != domain.OperationFailed ||
		!strings.Contains(operation.Error, "manual retry is required") ||
		metadata["failure"] != "lease-retry-exhausted" ||
		metadata["lease_claims"] != float64(jobLeaseMaxClaims) {
		t.Fatalf("exhausted operation=%+v metadata=%v", operation, metadata)
	}
	stored, err := dataStore.GetInstance(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.InstanceFailed || !strings.Contains(stored.LastError, "manual retry is required") {
		t.Fatalf("exhausted instance=%+v", stored)
	}
}

func TestExpiredHermesUpdateLeaseRestoresOriginalStatus(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "hermes-lease")
	now := time.Now().UTC()
	completeTestProvisionForAction(t, ctx, dataStore, "hermes-lease", instance.ID, now)
	currentImage := "local/hermes-fleet-runtime:0.18.2"
	targetImage := "local/hermes-fleet-runtime:0.19.0"
	currentImageID := "sha256:" + strings.Repeat("a", 64)
	if _, err := dataStore.db.ExecContext(ctx, `
UPDATE instances SET image=?, image_id=? WHERE id=?`, currentImage, currentImageID, instance.ID); err != nil {
		t.Fatal(err)
	}
	job := queueTestHermesUpdate(t, ctx, dataStore, host, instance, currentImage, currentImageID, targetImage, domain.InstanceStopped)
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
		if err != nil || claimed == nil || claimed.ID != job.ID {
			t.Fatalf("ClaimJob() attempt=%d job=%v error=%v", attempt, claimed, err)
		}
	}
	if _, err := dataStore.db.ExecContext(ctx, `
UPDATE jobs SET lease_expires_at=? WHERE id=?`, time.Now().UTC().Add(-time.Minute), claimed.ID); err != nil {
		t.Fatal(err)
	}
	next, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || next != nil {
		t.Fatalf("ClaimJob() after Hermes update retry budget job=%v error=%v", next, err)
	}
	stored, err := dataStore.GetInstance(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.InstanceStopped || !strings.Contains(stored.LastError, "manual retry is required") {
		t.Fatalf("exhausted Hermes update instance=%+v", stored)
	}
}

func TestFailedHermesUpdateCompletionPersistsVerifiedTargetImage(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "hermes-image")
	now := time.Now().UTC()
	completeTestProvisionForAction(t, ctx, dataStore, "hermes-image", instance.ID, now)
	currentImage := "local/hermes-fleet-runtime:0.18.2"
	targetImage := "local/hermes-fleet-runtime:0.19.0"
	currentImageID := "sha256:" + strings.Repeat("a", 64)
	targetImageID := "sha256:" + strings.Repeat("b", 64)
	if _, err := dataStore.db.ExecContext(ctx, `
UPDATE instances SET image=?, image_id=? WHERE id=?`, currentImage, currentImageID, instance.ID); err != nil {
		t.Fatal(err)
	}
	job := queueTestHermesUpdate(t, ctx, dataStore, host, instance, currentImage, currentImageID, targetImage, domain.InstanceRunning)
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil || claimed.ID != job.ID {
		t.Fatalf("ClaimJob() job=%v error=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	result := domain.JobResult{
		Success: false, Error: "Hermes update installed but the job lease was lost before restoring runtime state",
		RecoveryPointID: "recovery-" + strings.Repeat("c", 32),
		ImageID:         targetImageID, InstanceStatus: domain.InstanceStopped,
	}
	if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, result, nil); err != nil {
		t.Fatal(err)
	}
	stored, err := dataStore.GetInstance(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.InstanceStopped || stored.Image != targetImage || stored.ImageID != targetImageID {
		t.Fatalf("failed Hermes update did not persist the installed image: %+v", stored)
	}
}

func TestProvisionResultCannotRebindManagedIdentity(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "identity")
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimJob() job=%v error=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	result := domain.JobResult{
		Success: true, ProjectName: "hermes-fleet-peer-00000000", DataVolume: "hermes-fleet-peer-00000000-data",
		ManagedPath: "/managed/peer-00000000", ImageID: "sha256:" + strings.Repeat("a", 64),
	}
	if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, result, nil); !errors.Is(err, ErrInvalidJobResult) {
		t.Fatalf("CompleteJob() error=%v, want %v", err, ErrInvalidJobResult)
	}
	stored, err := dataStore.GetInstance(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ProjectName != "" || stored.ManagedPath != "" || stored.DataVolume != "" {
		t.Fatalf("untrusted provisioning metadata was persisted: %+v", stored)
	}
}

func TestCodexAuthProgressIsLeaseFencedAndDoesNotMutateInstanceLifecycle(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "codex-auth")
	provisionJob, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || provisionJob == nil {
		t.Fatalf("ClaimJob() job=%v error=%v", provisionJob, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, provisionJob.ID, provisionJob.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	provisionResult := successfulProvisionResult(instance)
	if err := dataStore.CompleteJob(ctx, host.ID, provisionJob.ID, provisionJob.LeaseToken, provisionResult, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	operation := domain.Operation{ID: "operation-auth-session", InstanceID: instance.ID, Type: "CODEX_AUTH", Status: domain.OperationPending, Summary: "Authenticate Codex", CreatedAt: now, UpdatedAt: now}
	job := domain.Job{ID: "job-auth-session", OperationID: operation.ID, HostID: host.ID, InstanceID: instance.ID, Type: "instance.auth.codex", Status: domain.JobPending, Payload: json.RawMessage(`{"provider":"openai-codex"}`), CreatedAt: now, UpdatedAt: now}
	if err := dataStore.QueueCodexAuth(ctx, operation, job); err != nil {
		t.Fatal(err)
	}
	active, err := dataStore.GetActiveCodexAuthSession(ctx, instance.ID, "openai-codex")
	if err != nil || active.OperationID != operation.ID || active.Provider != "openai-codex" {
		t.Fatalf("GetActiveCodexAuthSession() session=%+v error=%v", active, err)
	}
	if _, err := dataStore.GetActiveCodexAuthSession(ctx, instance.ID, "xai-oauth"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Grok authentication resumed Codex session: error=%v", err)
	}
	if err := dataStore.QueueCodexAuth(ctx, domain.Operation{ID: "operation-duplicate", InstanceID: instance.ID}, domain.Job{ID: "job-duplicate", InstanceID: instance.ID}); !errors.Is(err, ErrInstanceBusy) {
		t.Fatalf("duplicate QueueCodexAuth() error=%v, want ErrInstanceBusy", err)
	}
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil || claimed.ID != job.ID {
		t.Fatalf("ClaimJob() job=%+v error=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	progress := domain.JobProgress{Stage: "AWAITING_USER", VerificationURI: "https://auth.openai.com/codex/device", UserCode: "ABCD-EFGH", ExpiresAt: now.Add(15 * time.Minute)}
	if err := dataStore.UpdateJobProgress(ctx, host.ID, claimed.ID, claimed.LeaseToken, progress); err != nil {
		t.Fatal(err)
	}
	session, err := dataStore.GetCodexAuthSession(ctx, instance.ID, operation.ID)
	if err != nil || session.UserCode != progress.UserCode || session.VerificationURI != progress.VerificationURI {
		t.Fatalf("GetCodexAuthSession() session=%+v error=%v", session, err)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, domain.JobResult{Success: true}, nil); err != nil {
		t.Fatal(err)
	}
	stored, err := dataStore.GetInstance(ctx, instance.ID)
	if err != nil || stored.Status != domain.InstanceRunning || stored.LastError != "" || stored.ProjectName != provisionResult.ProjectName {
		t.Fatalf("Codex authentication changed instance lifecycle: instance=%+v error=%v", stored, err)
	}
	if err := dataStore.UpdateJobProgress(ctx, host.ID, claimed.ID, claimed.LeaseToken, progress); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("completed UpdateJobProgress() error=%v, want ErrLeaseLost", err)
	}
}

func TestCancelCodexAuthRevokesLeaseWithoutMutatingInstance(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "codex-auth-cancel")
	provisionJob, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || provisionJob == nil {
		t.Fatalf("ClaimJob() job=%v error=%v", provisionJob, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, provisionJob.ID, provisionJob.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, provisionJob.ID, provisionJob.LeaseToken, successfulProvisionResult(instance), nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	operation := domain.Operation{
		ID: "operation-auth-cancel", InstanceID: instance.ID, Type: "CODEX_AUTH", Status: domain.OperationPending,
		Summary: "Authenticate Codex", CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: "job-auth-cancel", OperationID: operation.ID, HostID: host.ID, InstanceID: instance.ID,
		Type: "instance.auth.codex", Status: domain.JobPending, Payload: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	}
	if err := dataStore.QueueCodexAuth(ctx, operation, job); err != nil {
		t.Fatal(err)
	}
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil || claimed.ID != job.ID {
		t.Fatalf("ClaimJob() job=%+v error=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	const reason = "Codex authentication canceled by administrator"
	if err := dataStore.CancelCodexAuth(ctx, instance.ID, operation.ID, reason); err != nil {
		t.Fatal(err)
	}
	session, err := dataStore.GetCodexAuthSession(ctx, instance.ID, operation.ID)
	if err != nil || session.Status != domain.OperationFailed || session.Error != reason {
		t.Fatalf("canceled session=%+v error=%v", session, err)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, domain.JobResult{Success: true}, nil); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("canceled CompleteJob() error=%v, want ErrLeaseLost", err)
	}
	stored, err := dataStore.GetInstance(ctx, instance.ID)
	if err != nil || stored.Status != domain.InstanceRunning || stored.LastError != "" {
		t.Fatalf("cancellation changed instance lifecycle: instance=%+v error=%v", stored, err)
	}
	if err := dataStore.CancelCodexAuth(ctx, instance.ID, operation.ID, reason); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("duplicate CancelCodexAuth() error=%v, want ErrStateChanged", err)
	}
	if err := dataStore.CancelCodexAuth(ctx, instance.ID, "missing-operation", reason); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing CancelCodexAuth() error=%v, want ErrNotFound", err)
	}
}

func TestQueueInspectionRejectsBusyHostWithoutPersistingOperation(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "inspection-busy")
	provisionJob, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || provisionJob == nil {
		t.Fatalf("ClaimJob() job=%v error=%v", provisionJob, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, provisionJob.ID, provisionJob.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, provisionJob.ID, provisionJob.LeaseToken, successfulProvisionResult(instance), nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	authOperation := domain.Operation{
		ID: "operation-busy-auth", InstanceID: instance.ID, Type: "CODEX_AUTH", Status: domain.OperationPending,
		Summary: "Authenticate Codex", CreatedAt: now, UpdatedAt: now,
	}
	authJob := domain.Job{
		ID: "job-busy-auth", OperationID: authOperation.ID, HostID: host.ID, InstanceID: instance.ID,
		Type: "instance.auth.codex", Status: domain.JobPending, Payload: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	}
	if err := dataStore.QueueCodexAuth(ctx, authOperation, authJob); err != nil {
		t.Fatal(err)
	}
	inspectionOperation := domain.Operation{
		ID: "operation-blocked-inspection", InstanceID: instance.ID, Type: "CREDENTIAL_REVEAL", Status: domain.OperationPending,
		Summary: "Reveal credentials", CreatedAt: now.Add(time.Millisecond), UpdatedAt: now.Add(time.Millisecond),
	}
	inspectionJob := domain.Job{
		ID: "job-blocked-inspection", OperationID: inspectionOperation.ID, HostID: host.ID, InstanceID: instance.ID,
		Type: "instance.credentials.inspect", Status: domain.JobPending, Payload: json.RawMessage(`{}`),
		CreatedAt: inspectionOperation.CreatedAt, UpdatedAt: inspectionOperation.UpdatedAt,
	}
	if err := dataStore.QueueInspection(ctx, inspectionOperation, inspectionJob); !errors.Is(err, ErrInstanceBusy) {
		t.Fatalf("QueueInspection() error=%v, want ErrInstanceBusy", err)
	}
	if _, err := dataStore.GetOperation(ctx, inspectionOperation.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("busy inspection operation was persisted: %v", err)
	}
}

func TestQueueRunningInspectionRejectsStoppedInstanceAtomically(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "running-inspection")
	provisionJob, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || provisionJob == nil {
		t.Fatalf("ClaimJob() job=%v error=%v", provisionJob, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, provisionJob.ID, provisionJob.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, provisionJob.ID, provisionJob.LeaseToken, successfulProvisionResult(instance), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.db.ExecContext(ctx, `UPDATE instances SET status=? WHERE id=?`, domain.InstanceStopped, instance.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	operation := domain.Operation{
		ID: "operation-stopped-profile", InstanceID: instance.ID, Type: "REFRESH_HERMES_PROFILES",
		Status: domain.OperationPending, Summary: "Refresh profiles", CreatedAt: now, UpdatedAt: now,
	}
	payload, _ := json.Marshal(domain.HermesProfileInspectPayload{InstanceID: instance.ID, Name: instance.Name})
	job := domain.Job{
		ID: "job-stopped-profile", OperationID: operation.ID, HostID: host.ID, InstanceID: instance.ID,
		Type: domain.JobInspectHermesProfiles, Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now,
	}
	if err := dataStore.QueueRunningInspection(ctx, operation, job); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("QueueRunningInspection() error=%v, want ErrStateChanged", err)
	}
	if _, err := dataStore.GetOperation(ctx, operation.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stopped-instance profile operation was persisted: %v", err)
	}
}

func TestProfileCompletionPersistsInventoryAfterInstanceStops(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "profile-completion-stop")
	provisionJob, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || provisionJob == nil {
		t.Fatalf("ClaimJob() job=%v error=%v", provisionJob, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, provisionJob.ID, provisionJob.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, provisionJob.ID, provisionJob.LeaseToken, successfulProvisionResult(instance), nil); err != nil {
		t.Fatal(err)
	}
	storedInstance, err := dataStore.GetInstance(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	operation := domain.Operation{
		ID: "operation-profile-refresh-after-stop", InstanceID: instance.ID, Type: "REFRESH_HERMES_PROFILES",
		Status: domain.OperationPending, Summary: "Refresh profiles", CreatedAt: now, UpdatedAt: now,
	}
	payload, _ := json.Marshal(domain.HermesProfileInspectPayload{
		InstanceID: instance.ID, Name: instance.Name, ProjectName: storedInstance.ProjectName,
		ManagedPath: storedInstance.ManagedPath, DashboardPort: storedInstance.DashboardPort,
	})
	job := domain.Job{
		ID: "job-profile-refresh-after-stop", OperationID: operation.ID, HostID: host.ID, InstanceID: instance.ID,
		Type: domain.JobInspectHermesProfiles, Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now,
	}
	if err := dataStore.QueueRunningInspection(ctx, operation, job); err != nil {
		t.Fatal(err)
	}
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimJob() profile job=%v error=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.db.ExecContext(ctx, `UPDATE instances SET status=? WHERE id=?`, domain.InstanceStopped, instance.ID); err != nil {
		t.Fatal(err)
	}
	result := domain.JobResult{Success: true, HermesProfiles: &domain.HermesProfileInventory{
		InstanceID: instance.ID, Profiles: []domain.HermesProfile{{Name: "default", Default: true}},
	}}
	if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, result, nil); err != nil {
		t.Fatalf("CompleteJob() after stop: %v", err)
	}
	inventory, err := dataStore.HermesProfileInventory(ctx, instance.ID)
	if err != nil || len(inventory.Profiles) != 1 || inventory.Profiles[0].Name != "default" {
		t.Fatalf("inventory=%+v error=%v", inventory, err)
	}
	var jobStatus string
	if err := dataStore.db.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id=?`, claimed.ID).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if jobStatus != domain.JobSucceeded {
		t.Fatalf("job status=%q, want %q", jobStatus, domain.JobSucceeded)
	}
}

func TestCapabilityCompletionPersistsSanitizedInventory(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "capability-completion")
	provisionJob, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || provisionJob == nil {
		t.Fatalf("ClaimJob() job=%v error=%v", provisionJob, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, provisionJob.ID, provisionJob.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, provisionJob.ID, provisionJob.LeaseToken, successfulProvisionResult(instance), nil); err != nil {
		t.Fatal(err)
	}
	storedInstance, err := dataStore.GetInstance(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	operation := domain.Operation{
		ID: "operation-capability-refresh", InstanceID: instance.ID, Type: "REFRESH_HERMES_CAPABILITIES",
		Status: domain.OperationPending, Summary: "Refresh capabilities", CreatedAt: now, UpdatedAt: now,
	}
	payload, _ := json.Marshal(domain.HermesCapabilityInspectPayload{
		InstanceID: instance.ID, Name: instance.Name, ProjectName: storedInstance.ProjectName,
		ManagedPath: storedInstance.ManagedPath, APIPort: storedInstance.APIPort,
	})
	job := domain.Job{
		ID: "job-capability-refresh", OperationID: operation.ID, HostID: host.ID, InstanceID: instance.ID,
		Type: domain.JobInspectHermesCapabilities, Status: domain.JobPending, Payload: payload,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := dataStore.QueueRunningInspection(ctx, operation, job); err != nil {
		t.Fatal(err)
	}
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimJob() capability job=%v error=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	result := domain.JobResult{Success: true, HermesCapabilities: &domain.HermesCapabilityInventory{
		InstanceID: instance.ID, Platform: "hermes-agent", RuntimeMode: "server_agent",
		Features: map[string]bool{"skills_api": true},
		Skills:   []domain.HermesSkillCapability{{Name: "browser-session"}},
		Toolsets: []domain.HermesToolsetCapability{{Name: "browser", Enabled: true, Configured: true, Tools: []string{"browser_exec"}}},
		Browser:  domain.HermesBrowserCapability{Available: true, Implementation: "playwright-chromium.v1"},
	}}
	if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, result, nil); err != nil {
		t.Fatal(err)
	}
	inventory, err := dataStore.HermesCapabilityInventory(ctx, instance.ID)
	if err != nil || inventory.ObservedAt.IsZero() || !inventory.Browser.Available ||
		len(inventory.Skills) != 1 || inventory.Skills[0].Name != "browser-session" {
		t.Fatalf("inventory=%+v error=%v", inventory, err)
	}
}

func TestSkillContentCompletionPersistsLeaseFencedSnapshot(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "skill-content-completion")
	provisionJob, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || provisionJob == nil {
		t.Fatalf("ClaimJob() job=%v error=%v", provisionJob, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, provisionJob.ID, provisionJob.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, provisionJob.ID, provisionJob.LeaseToken, successfulProvisionResult(instance), nil); err != nil {
		t.Fatal(err)
	}
	storedInstance, err := dataStore.GetInstance(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	payloadValue := domain.HermesSkillContentInspectPayload{
		HermesProfileInspectPayload: domain.HermesProfileInspectPayload{
			InstanceID: instance.ID, Name: instance.Name, ProjectName: storedInstance.ProjectName,
			ManagedPath: storedInstance.ManagedPath, DashboardPort: storedInstance.DashboardPort,
		},
		SkillName: "browser-report", Profile: "default",
	}
	payload, _ := json.Marshal(payloadValue)
	operation := domain.Operation{ID: "operation-skill-content", InstanceID: instance.ID, Type: "INSPECT_HERMES_SKILL", Status: domain.OperationPending, Summary: "Inspect skill", CreatedAt: now, UpdatedAt: now}
	job := domain.Job{ID: "job-skill-content", OperationID: operation.ID, HostID: host.ID, InstanceID: instance.ID, Type: domain.JobInspectHermesSkillContent, Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now}
	if err := dataStore.QueueRunningInspection(ctx, operation, job); err != nil {
		t.Fatal(err)
	}
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimJob() skill job=%v error=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: browser-report\ndescription: Browser report\n---\nUse Chromium."
	digest := sha256.Sum256([]byte(content))
	result := domain.JobResult{Success: true, HermesSkillContent: &domain.HermesSkillContentSnapshot{
		InstanceID: instance.ID, SkillName: "browser-report", Profile: "default", Provenance: "agent",
		Content: content, Revision: hex.EncodeToString(digest[:]),
	}}
	if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, result, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := dataStore.HermesSkillContentSnapshot(ctx, instance.ID, "default", "browser-report")
	if err != nil || snapshot.Content != content || snapshot.Provenance != "agent" || snapshot.ObservedAt.IsZero() {
		t.Fatalf("snapshot=%+v error=%v", snapshot, err)
	}
}

func TestClaimJobAllowsDifferentInstancesButSerializesEachInstance(t *testing.T) {
	ctx, dataStore, host, first := newFleetFixture(t, "parallel-claims")
	second := testInstance("instance-parallel-02", "fleet-parallel-02", host.ID, 18650, 19130, time.Now().UTC().Add(time.Second))
	if err := createTestInstance(ctx, dataStore, second, "parallel-02"); err != nil {
		t.Fatal(err)
	}
	now := first.CreatedAt.Add(2 * time.Second)
	tx, err := dataStore.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	deferredOperation := domain.Operation{
		ID: "operation-parallel-deferred", InstanceID: first.ID, Type: "CREDENTIAL_REVEAL",
		Status: domain.OperationPending, Summary: "Deferred same-instance job", CreatedAt: now, UpdatedAt: now,
	}
	deferredJob := domain.Job{
		ID: "job-parallel-deferred", OperationID: deferredOperation.ID, HostID: host.ID, InstanceID: first.ID,
		Type: "instance.credentials.inspect", Status: domain.JobPending, Payload: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	}
	if err := insertOperationAndJob(ctx, tx, deferredOperation, deferredJob); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	firstClaim, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || firstClaim == nil || firstClaim.InstanceID != first.ID {
		t.Fatalf("first ClaimJob() job=%+v error=%v", firstClaim, err)
	}
	secondClaim, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || secondClaim == nil || secondClaim.InstanceID != second.ID {
		t.Fatalf("second ClaimJob() job=%+v error=%v", secondClaim, err)
	}
	blockedClaim, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || blockedClaim != nil {
		t.Fatalf("third ClaimJob() job=%+v error=%v, want same-instance job blocked", blockedClaim, err)
	}
}

func TestInstancePortAllocationAndReuseAfterDelete(t *testing.T) {
	ctx, dataStore, host, first := newFleetFixture(t, "allocation")

	colliding := testInstance("instance-collision", "fleet-collision", host.ID, 9130, 9131, time.Now().UTC())
	if err := createTestInstance(ctx, dataStore, colliding, "collision"); err == nil {
		t.Fatal("CreateInstance() accepted an API port already used as a dashboard port")
	}

	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimJob() job=%v error=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, successfulProvisionResult(first), nil); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Add(time.Second)
	deleteOperation := domain.Operation{ID: "operation-delete", InstanceID: first.ID, Type: "DELETE", Status: domain.OperationPending, Summary: "Delete", CreatedAt: now, UpdatedAt: now}
	deleteJob := domain.Job{ID: "job-delete", OperationID: deleteOperation.ID, HostID: host.ID, InstanceID: first.ID, Type: "instance.delete", Status: domain.JobPending, Payload: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now}
	if err := dataStore.QueueAction(ctx, domain.InstanceRunning, domain.InstanceDeleting, deleteOperation, deleteJob); err != nil {
		t.Fatal(err)
	}
	claimed, err = dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimJob() delete job=%v error=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, domain.JobResult{Success: true}, nil); err != nil {
		t.Fatal(err)
	}
	pending, err := dataStore.PendingInstanceDeletion(ctx, first.ID)
	if err != nil {
		t.Fatalf("PendingInstanceDeletion() after runtime deletion: %v", err)
	}
	if pending.InstanceID != first.ID || pending.OperationID != deleteOperation.ID {
		t.Fatalf("pending deletion=%+v", pending)
	}
	deleting, err := dataStore.GetInstance(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deleting.Status != domain.InstanceDeleting {
		t.Fatalf("instance status after runtime deletion=%q, want %q", deleting.Status, domain.InstanceDeleting)
	}
	deleteState, err := dataStore.GetOperation(ctx, deleteOperation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deleteState.Status != domain.OperationRunning {
		t.Fatalf("delete operation after runtime deletion=%q, want %q", deleteState.Status, domain.OperationRunning)
	}

	failedProgress := domain.JobProgress{Stage: "VERIFYING_ROUTE_REMOVAL", Steps: []domain.OperationStep{{Stage: "VERIFYING_ROUTE_REMOVAL", Status: "failed"}}}
	if err := dataStore.UpdateInstanceDeletionCleanup(ctx, pending, failedProgress, "Fleet-owned DNS remains", false, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	deleting, err = dataStore.GetInstance(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	deleteState, err = dataStore.GetOperation(ctx, deleteOperation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deleting.Status != domain.InstanceDeleting || deleting.LastError == "" || deleteState.Status != domain.OperationFailed {
		t.Fatalf("failed cleanup instance=%+v operation=%+v", deleting, deleteState)
	}

	finalProgress := domain.JobProgress{Stage: "FINALIZING_DELETION", Steps: []domain.OperationStep{{Stage: "FINALIZING_DELETION", Status: "succeeded"}}}
	if err := dataStore.UpdateInstanceDeletionCleanup(ctx, pending, finalProgress, "", true, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	visible, err := dataStore.ListInstances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, listed := range visible {
		if listed.ID == first.ID {
			t.Fatal("deleted instance remained visible in the Fleet instance list")
		}
	}

	replacement := testInstance("instance-replacement", first.Name, host.ID, first.APIPort, first.DashboardPort, now.Add(time.Second))
	if err := createTestInstance(ctx, dataStore, replacement, "replacement"); err != nil {
		t.Fatalf("CreateInstance() could not reuse deleted allocation: %v", err)
	}
}

func TestQueueActionRejectsStaleInstanceStatus(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "stale-action")
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimJob() job=%v error=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, successfulProvisionResult(instance), nil); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	firstOperation := domain.Operation{ID: "operation-first-action", InstanceID: instance.ID, Type: "STOP", Status: domain.OperationPending, Summary: "Stop", CreatedAt: now, UpdatedAt: now}
	firstJob := domain.Job{ID: "job-first-action", OperationID: firstOperation.ID, HostID: host.ID, InstanceID: instance.ID, Type: "instance.stop", Status: domain.JobPending, Payload: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now}
	if err := dataStore.QueueAction(ctx, domain.InstanceRunning, domain.InstanceProvisioning, firstOperation, firstJob); err != nil {
		t.Fatal(err)
	}

	secondOperation := domain.Operation{ID: "operation-stale-delete", InstanceID: instance.ID, Type: "DELETE", Status: domain.OperationPending, Summary: "Delete", CreatedAt: now, UpdatedAt: now}
	secondJob := domain.Job{ID: "job-stale-delete", OperationID: secondOperation.ID, HostID: host.ID, InstanceID: instance.ID, Type: "instance.delete", Status: domain.JobPending, Payload: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now}
	if err := dataStore.QueueAction(ctx, domain.InstanceRunning, domain.InstanceDeleting, secondOperation, secondJob); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("QueueAction() error=%v, want ErrStateChanged", err)
	}
	if _, err := dataStore.GetOperation(ctx, secondOperation.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale operation was persisted: %v", err)
	}
}

func TestLegacyInstanceConstraintsAreMigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := database.Exec(`
CREATE TABLE hosts (
  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, hostname TEXT NOT NULL, os TEXT NOT NULL,
  arch TEXT NOT NULL, agent_version TEXT NOT NULL, token_hash TEXT NOT NULL,
  last_seen_at DATETIME NOT NULL, created_at DATETIME NOT NULL
);
CREATE TABLE instances (
  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, host_id TEXT NOT NULL REFERENCES hosts(id),
  status TEXT NOT NULL, image TEXT NOT NULL, image_id TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL,
  model TEXT NOT NULL, reasoning TEXT NOT NULL, service_tier TEXT NOT NULL,
  provider_profile_id TEXT NOT NULL DEFAULT '', api_port INTEGER NOT NULL, dashboard_port INTEGER NOT NULL,
  project_name TEXT NOT NULL DEFAULT '', data_volume TEXT NOT NULL DEFAULT '', managed_path TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '', created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
  UNIQUE(host_id, api_port), UNIQUE(host_id, dashboard_port)
);
INSERT INTO hosts VALUES ('host-legacy', 'host-legacy', 'host', 'test', 'test', 'test', 'hash', ?, ?);
INSERT INTO instances (
  id, name, host_id, status, image, provider, model, reasoning, service_tier,
  api_port, dashboard_port, last_error, created_at, updated_at
) VALUES ('instance-legacy', 'fleet-legacy', 'host-legacy', 'DELETED', 'image',
  'openai-codex', 'gpt', 'medium', 'normal', 8650, 9130,
  'runtime synchronization refused: model contains unsupported characters or length', ?, ?);
`, now, now, now, now); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	dataStore, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	stored, err := dataStore.GetInstance(context.Background(), "instance-legacy")
	if err != nil || stored.Name != "fleet-legacy" {
		t.Fatalf("legacy instance was not preserved: instance=%+v error=%v", stored, err)
	}
	if stored.CodexConfigured || stored.Model != "" || stored.Reasoning != "" || stored.ServiceTier != "" || stored.LastError != "" {
		t.Fatalf("implicit legacy Codex configuration was not normalized: %+v", stored)
	}
	var violationTable string
	err = dataStore.db.QueryRow(`SELECT "table" FROM pragma_foreign_key_check LIMIT 1`).Scan(&violationTable)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign_key_check failed: %v", err)
	}
	if err == nil {
		t.Fatalf("migration left a foreign key violation in %s", violationTable)
	}
	replacement := testInstance("instance-new", "fleet-legacy", "host-legacy", 8650, 9130, now.Add(time.Second))
	if err := createTestInstance(context.Background(), dataStore, replacement, "legacy-new"); err != nil {
		t.Fatalf("migrated schema did not release deleted allocation: %v", err)
	}
}

func TestRemovedProviderProfileSchemaIsMigratedWithoutChangingInstanceRuntime(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "provider-profile.db")
	dataStore, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	host := domain.Host{ID: "host-migration", Name: "host-migration", Hostname: "host", OS: "test", Arch: "test", AgentVersion: "test", LastSeenAt: now, CreatedAt: now}
	if err := dataStore.EnrollHost(ctx, host, "hash"); err != nil {
		t.Fatal(err)
	}
	instance := testInstance("instance-migration", "fleet-migration", host.ID, 18650, 19130, now)
	instance.Provider = "openai-codex"
	instance.Model = "gpt-5.6-sol"
	if err := createTestInstance(ctx, dataStore, instance, "provider-migration"); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
ALTER TABLE instances ADD COLUMN provider_profile_id TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN provider_profile_id TEXT NOT NULL DEFAULT '';
CREATE TABLE provider_profiles (
  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, provider TEXT NOT NULL,
  secret_ciphertext TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_jobs_provider_profile ON jobs(provider_profile_id, status);
INSERT INTO provider_profiles (id, name, provider, secret_ciphertext)
VALUES ('profile-legacy', 'Legacy', 'openai-codex', 'encrypted-secret');
UPDATE instances SET provider_profile_id='profile-legacy';
UPDATE jobs SET provider_profile_id='profile-legacy';
`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	dataStore, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	stored, err := dataStore.GetInstance(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Provider != instance.Provider || stored.Model != instance.Model {
		t.Fatalf("migration changed instance runtime: provider=%q model=%q", stored.Provider, stored.Model)
	}
	for _, tableColumn := range [][2]string{{"instances", "provider_profile_id"}, {"jobs", "provider_profile_id"}} {
		exists, err := dataStore.hasColumn(tableColumn[0], tableColumn[1])
		if err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("legacy column %s.%s still exists", tableColumn[0], tableColumn[1])
		}
	}
	var profileTableCount int
	if err := dataStore.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='provider_profiles'`).Scan(&profileTableCount); err != nil {
		t.Fatal(err)
	}
	if profileTableCount != 0 {
		t.Fatal("legacy provider_profiles table still exists")
	}
}

func TestLegacyJobsSchemaAddsCompletionHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-jobs.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
CREATE TABLE jobs (
  id TEXT PRIMARY KEY,
  operation_id TEXT NOT NULL,
  host_id TEXT NOT NULL,
  instance_id TEXT NOT NULL,
  type TEXT NOT NULL,
  status TEXT NOT NULL,
  payload BLOB NOT NULL,
  progress BLOB NOT NULL DEFAULT '{}',
  attempts INTEGER NOT NULL DEFAULT 0,
  lease_token TEXT NOT NULL DEFAULT '',
  lease_expires_at DATETIME,
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	dataStore, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	exists, err := dataStore.hasColumn("jobs", "completion_hash")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("legacy jobs schema was not migrated with completion_hash")
	}
}

func TestCodexConfigurationMigrationDoesNotReapplyToExplicitRestoredState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "codex-migration.db")
	dataStore, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	host := domain.Host{ID: "host-codex-migration", Name: "host-codex-migration", Hostname: "host", OS: "test", Arch: "test", AgentVersion: "test", LastSeenAt: now, CreatedAt: now}
	if err := dataStore.EnrollHost(ctx, host, "hash"); err != nil {
		t.Fatal(err)
	}
	instance := testInstance("instance-codex-migration", "fleet-codex-migration", host.ID, 18650, 19130, now)
	if err := createTestInstance(ctx, dataStore, instance, "codex-migration"); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.db.ExecContext(ctx, `
INSERT INTO operations (
  id, instance_id, workflow_id, actor, type, status, summary, metadata, error, created_at, updated_at
) VALUES (?, ?, '', 'FLEET_ADMIN', 'CONFIGURE_CODEX', 'SUCCEEDED', 'Configure Codex', '{}', '', ?, ?)`,
		"operation-configure-codex-history", instance.ID, now.Add(time.Second), now.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.db.ExecContext(ctx, `
UPDATE instances
SET codex_configured=0, model='', reasoning='', service_tier=''
WHERE id=?`, instance.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.db.ExecContext(ctx, `
DELETE FROM schema_migrations WHERE name=?`, codexConfigurationStateMigration); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.Close(); err != nil {
		t.Fatal(err)
	}

	for reopen := 1; reopen <= 2; reopen++ {
		dataStore, err = Open(path)
		if err != nil {
			t.Fatalf("Open() pass %d: %v", reopen, err)
		}
		stored, getErr := dataStore.GetInstance(ctx, instance.ID)
		if getErr != nil {
			dataStore.Close()
			t.Fatal(getErr)
		}
		if stored.CodexConfigured || stored.Model != "" || stored.Reasoning != "" || stored.ServiceTier != "" {
			dataStore.Close()
			t.Fatalf("migration pass %d overwrote restored Codex state: %+v", reopen, stored)
		}
		if err := dataStore.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBackupCreatesConsistentSQLiteSnapshot(t *testing.T) {
	ctx, dataStore, _, instance := newFleetFixture(t, "backup")
	destination := filepath.Join(t.TempDir(), "fleet-backup.sqlite")
	if err := dataStore.CreateBackup(ctx, destination); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.VerifyBackup(ctx, destination); err != nil {
		t.Fatalf("VerifyBackup() rejected a fresh snapshot: %v", err)
	}

	backupStore, err := Open(destination)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer backupStore.Close()
	stored, err := backupStore.GetInstance(ctx, instance.ID)
	if err != nil {
		t.Fatalf("read backed-up instance: %v", err)
	}
	if stored.Name != instance.Name {
		t.Fatalf("backup instance name = %q, want %q", stored.Name, instance.Name)
	}
}

func TestBackupCreatesConsistentSnapshotUnderReadLoad(t *testing.T) {
	ctx, dataStore, _, instance := newFleetFixture(t, "backup-read-load")
	destination := filepath.Join(t.TempDir(), "fleet-backup.sqlite")
	readErrors := make(chan error, 32)
	for index := 0; index < cap(readErrors); index++ {
		go func() {
			_, err := dataStore.GetInstance(ctx, instance.ID)
			readErrors <- err
		}()
	}
	if err := dataStore.CreateBackup(ctx, destination); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < cap(readErrors); index++ {
		if err := <-readErrors; err != nil {
			t.Fatalf("concurrent read failed: %v", err)
		}
	}
	if err := dataStore.VerifyBackup(ctx, destination); err != nil {
		t.Fatalf("VerifyBackup() rejected a snapshot created under read load: %v", err)
	}
}

func TestBackupRefusesExistingDestination(t *testing.T) {
	ctx, dataStore, _, _ := newFleetFixture(t, "backup-existing")
	destination := filepath.Join(t.TempDir(), "fleet-backup.sqlite")
	if err := os.WriteFile(destination, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CreateBackup(ctx, destination); err == nil {
		t.Fatal("CreateBackup() overwrote an existing destination")
	}
	contents, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "preserve" {
		t.Fatalf("existing destination changed to %q", contents)
	}
}

func TestObservationReportsAreHostGenerationAndTimeFenced(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "observation")
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimJob() job=%v error=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, successfulProvisionResult(instance), nil); err != nil {
		t.Fatal(err)
	}

	targets, err := dataStore.ListObservationTargets(ctx, host.ID)
	if err != nil || len(targets) != 1 {
		t.Fatalf("ListObservationTargets() targets=%+v error=%v", targets, err)
	}
	target := targets[0]
	if target.InstanceID != instance.ID || target.Generation == "" || target.DesiredStatus != domain.InstanceRunning {
		t.Fatalf("unexpected observation target: %+v", target)
	}

	observedAt := time.Now().UTC()
	current := domain.InstanceObservation{
		InstanceID: instance.ID, TargetGeneration: target.Generation, HermesVersion: "0.18.2", HermesSource: "7acaff5ef2bc", Status: domain.ObservationInSync,
		ModelCatalog: []string{"gpt-5.6-sol", "gpt-5.6-terra"}, RecommendedModel: "gpt-5.6-sol",
		ProviderModelCatalogs: map[string]domain.ProviderModelCatalog{
			"openai-codex": {Models: []string{"gpt-5.6-sol", "gpt-5.6-terra"}, Recommended: "gpt-5.6-sol"},
			"xai-oauth":    {Models: []string{"grok-4.6"}, Recommended: "grok-4.6"},
		},
		Summary: "Runtime matches desired state", Checks: []domain.ObservationCheck{{Name: "runtime", Status: domain.ObservationCheckOK, Detail: "Both services are running"}},
		ObservedAt: observedAt,
	}
	if err := dataStore.RecordObservations(ctx, host.ID, []domain.InstanceObservation{current}, observedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	hasCurrentCheck, err := dataStore.HasFreshObservationCheck(ctx, instance.ID, "runtime", domain.ObservationCheckOK, observedAt)
	if err != nil || !hasCurrentCheck {
		t.Fatalf("HasFreshObservationCheck() current=%v error=%v", hasCurrentCheck, err)
	}
	hasStaleCheck, err := dataStore.HasFreshObservationCheck(ctx, instance.ID, "runtime", domain.ObservationCheckOK, observedAt.Add(2*time.Second))
	if err != nil || hasStaleCheck {
		t.Fatalf("HasFreshObservationCheck() stale=%v error=%v", hasStaleCheck, err)
	}

	clockCorrected := current
	clockCorrected.Status = domain.ObservationDegraded
	clockCorrected.Summary = "An older host observation must not replace current state"
	clockCorrected.ObservedAt = observedAt.Add(-time.Second)
	if err := dataStore.RecordObservations(ctx, host.ID, []domain.InstanceObservation{clockCorrected}, observedAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	stored, err := dataStore.ListInstances(ctx)
	if err != nil || len(stored) != 1 || stored[0].Observation == nil {
		t.Fatalf("ListInstances() instances=%+v error=%v", stored, err)
	}
	if stored[0].Observation.Status != domain.ObservationInSync || stored[0].Observation.HermesVersion != "0.18.2" || stored[0].Observation.HermesSource != "7acaff5ef2bc" ||
		len(stored[0].Observation.ModelCatalog) != 2 || stored[0].Observation.RecommendedModel != "gpt-5.6-sol" ||
		len(stored[0].Observation.ProviderModelCatalogs["xai-oauth"].Models) != 1 {
		t.Fatalf("older host observation replaced the current observation: %+v", stored[0].Observation)
	}
	grokModels, grokRecommended, err := dataStore.GetInstanceProviderModelCatalog(ctx, instance.ID, "xai-oauth")
	if err != nil || len(grokModels) != 1 || grokModels[0] != "grok-4.6" || grokRecommended != "grok-4.6" {
		t.Fatalf("GetInstanceProviderModelCatalog(xai-oauth)=%v %q error=%v", grokModels, grokRecommended, err)
	}
	if models, recommended, err := dataStore.GetInstanceProviderModelCatalog(ctx, instance.ID, "missing-oauth"); !errors.Is(err, ErrNotFound) || len(models) != 0 || recommended != "" {
		t.Fatalf("missing provider catalog leaked active catalog: models=%v recommended=%q error=%v", models, recommended, err)
	}
	equalReplay := current
	equalReplay.Status = domain.ObservationDegraded
	equalReplay.Summary = "An equal host observation must remain idempotent"
	if err := dataStore.RecordObservations(ctx, host.ID, []domain.InstanceObservation{equalReplay}, observedAt.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	stored, err = dataStore.ListInstances(ctx)
	if err != nil || stored[0].Observation == nil || stored[0].Observation.Status != domain.ObservationInSync {
		t.Fatalf("equal host observation replaced current state: instances=%+v error=%v", stored, err)
	}
	delayedReplay := current
	delayedReplay.Status = domain.ObservationMissing
	delayedReplay.Summary = "A newer host observation wins even when its request committed late"
	delayedReplay.ObservedAt = observedAt.Add(20 * time.Second)
	if err := dataStore.RecordObservations(ctx, host.ID, []domain.InstanceObservation{delayedReplay}, observedAt.Add(500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	stored, err = dataStore.ListInstances(ctx)
	if err != nil || stored[0].Observation == nil || stored[0].Observation.Status != domain.ObservationMissing ||
		!stored[0].Observation.ReceivedAt.Equal(observedAt.Add(time.Second)) {
		t.Fatalf("newer host observation or monotonic receipt time was not preserved: instances=%+v error=%v", stored, err)
	}
	detailed, err := dataStore.GetInstance(ctx, instance.ID)
	if err != nil || detailed.Observation == nil || detailed.Observation.HermesVersion != "0.18.2" ||
		detailed.Observation.HermesSource != "7acaff5ef2bc" {
		t.Fatalf("GetInstance() omitted the current Hermes observation: instance=%+v error=%v", detailed, err)
	}

	otherHost := domain.Host{ID: "host-observation-other", Name: "host-observation-other", Hostname: "other", OS: "test", Arch: "test", AgentVersion: "test", LastSeenAt: observedAt, CreatedAt: observedAt}
	if err := dataStore.EnrollHost(ctx, otherHost, "other-token"); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.RecordObservations(ctx, otherHost.ID, []domain.InstanceObservation{current}, observedAt.Add(3*time.Second)); !errors.Is(err, ErrObservationOwnership) {
		t.Fatalf("cross-host RecordObservations() error=%v, want ErrObservationOwnership", err)
	}

	now := observedAt.Add(4 * time.Second)
	operation := domain.Operation{ID: "operation-observation-stop", InstanceID: instance.ID, Type: "STOP", Status: domain.OperationPending, Summary: "Stop", CreatedAt: now, UpdatedAt: now}
	job := domain.Job{ID: "job-observation-stop", OperationID: operation.ID, HostID: host.ID, InstanceID: instance.ID, Type: "instance.stop", Status: domain.JobPending, Payload: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now}
	if err := dataStore.QueueAction(ctx, domain.InstanceRunning, domain.InstanceProvisioning, operation, job); err != nil {
		t.Fatal(err)
	}
	newerOldGeneration := current
	newerOldGeneration.Status = domain.ObservationMissing
	newerOldGeneration.Summary = "Old lifecycle generation"
	newerOldGeneration.ObservedAt = observedAt.Add(10 * time.Second)
	if err := dataStore.RecordObservations(ctx, host.ID, []domain.InstanceObservation{newerOldGeneration}, observedAt.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	stored, err = dataStore.ListInstances(ctx)
	if err != nil || stored[0].Observation == nil || stored[0].Observation.Status != domain.ObservationMissing {
		t.Fatalf("old generation replaced observation: instances=%+v error=%v", stored, err)
	}
}

func TestObservationRefreshRequestClearsOnlyAfterMatchingReport(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "observation-refresh")
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimJob() job=%v error=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, successfulProvisionResult(instance), nil); err != nil {
		t.Fatal(err)
	}
	targets, err := dataStore.ListObservationTargets(ctx, host.ID)
	if err != nil || len(targets) != 1 {
		t.Fatalf("ListObservationTargets() targets=%+v error=%v", targets, err)
	}
	requestedAt := time.Now().UTC()
	request, err := dataStore.RequestObservation(ctx, targets[0].InstanceID, "refresh-current", requestedAt)
	if err != nil {
		t.Fatal(err)
	}
	if request.ID != "refresh-current" {
		t.Fatalf("RequestObservation() request=%+v", request)
	}
	targets, err = dataStore.ListObservationTargets(ctx, host.ID)
	if err != nil || targets[0].RefreshRequestID != request.ID {
		t.Fatalf("refresh request was not attached to target: targets=%+v error=%v", targets, err)
	}

	report := domain.InstanceObservation{
		InstanceID: targets[0].InstanceID, TargetGeneration: targets[0].Generation,
		RefreshRequestID: "refresh-stale", Status: domain.ObservationInSync, Summary: "Current",
		Checks: []domain.ObservationCheck{{Name: "runtime", Status: domain.ObservationCheckOK, Detail: "Running"}}, ObservedAt: requestedAt.Add(time.Second),
	}
	if err := dataStore.RecordObservations(ctx, host.ID, []domain.InstanceObservation{report}, requestedAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	instances, err := dataStore.ListInstances(ctx)
	if err != nil || instances[0].ObservationRequest == nil || instances[0].ObservationRequest.ID != request.ID {
		t.Fatalf("mismatched report cleared refresh request: instances=%+v error=%v", instances, err)
	}
	report.RefreshRequestID = request.ID
	report.ObservedAt = requestedAt.Add(3 * time.Second)
	if err := dataStore.RecordObservations(ctx, host.ID, []domain.InstanceObservation{report}, requestedAt.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	instances, err = dataStore.ListInstances(ctx)
	if err != nil || instances[0].ObservationRequest != nil {
		t.Fatalf("matching report did not clear refresh request: instances=%+v error=%v", instances, err)
	}
}

func TestRuntimeConfigurationAutoSyncRequiresConsecutiveAcceptedDrift(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "runtime-auto-sync")
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimJob() job=%v error=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, successfulProvisionResult(instance), nil); err != nil {
		t.Fatal(err)
	}
	targets, err := dataStore.ListObservationTargets(ctx, host.ID)
	if err != nil || len(targets) != 1 {
		t.Fatalf("ListObservationTargets() targets=%+v error=%v", targets, err)
	}
	base := time.Now().UTC()
	observation := domain.InstanceObservation{
		InstanceID: targets[0].InstanceID, TargetGeneration: targets[0].Generation,
		Status: domain.ObservationDegraded, Summary: "Runtime drift",
		Checks:     []domain.ObservationCheck{{Name: "runtime_configuration", Status: domain.ObservationCheckDrift, Detail: "Stale"}},
		ObservedAt: base,
	}
	if err := dataStore.RecordObservations(ctx, host.ID, []domain.InstanceObservation{observation}, base.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	queue, err := dataStore.TrackRuntimeConfigurationObservation(ctx, observation, base.Add(time.Second))
	if err != nil || queue {
		t.Fatalf("first drift queue=%v error=%v", queue, err)
	}
	observation.ObservedAt = base.Add(2 * time.Second)
	if err := dataStore.RecordObservations(ctx, host.ID, []domain.InstanceObservation{observation}, base.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	queue, err = dataStore.TrackRuntimeConfigurationObservation(ctx, observation, base.Add(3*time.Second))
	if err != nil || !queue {
		t.Fatalf("second drift queue=%v error=%v", queue, err)
	}
	if err := dataStore.RecordRuntimeConfigurationQueueFailure(ctx, instance.ID, base.Add(4*time.Second)); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("mismatched queue failure error=%v, want ErrStateChanged", err)
	}
	var attempts int
	var lastAttemptAt sql.NullTime
	if err := dataStore.db.QueryRowContext(ctx, `
SELECT attempts, last_attempt_at FROM runtime_reconcile_state WHERE instance_id=?`, instance.ID).
		Scan(&attempts, &lastAttemptAt); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || !lastAttemptAt.Valid || !lastAttemptAt.Time.Equal(base.Add(3*time.Second)) {
		t.Fatalf("mismatched queue failure mutated state: attempts=%d last_attempt_at=%v", attempts, lastAttemptAt)
	}
	if err := dataStore.RecordRuntimeConfigurationQueueFailure(ctx, instance.ID, base.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.db.QueryRowContext(ctx, `
SELECT attempts, last_attempt_at FROM runtime_reconcile_state WHERE instance_id=?`, instance.ID).
		Scan(&attempts, &lastAttemptAt); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || lastAttemptAt.Valid {
		t.Fatalf("queue failure consumed retry budget: attempts=%d last_attempt_at=%v", attempts, lastAttemptAt)
	}
	queue, err = dataStore.TrackRuntimeConfigurationObservation(ctx, observation, base.Add(4*time.Second))
	if err != nil || queue {
		t.Fatalf("duplicate drift queue=%v error=%v", queue, err)
	}

	observation.Status = domain.ObservationInSync
	observation.Summary = "Ready"
	observation.Checks[0].Status = domain.ObservationCheckOK
	observation.ObservedAt = base.Add(5 * time.Second)
	if err := dataStore.RecordObservations(ctx, host.ID, []domain.InstanceObservation{observation}, base.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	queue, err = dataStore.TrackRuntimeConfigurationObservation(ctx, observation, base.Add(6*time.Second))
	if err != nil || queue {
		t.Fatalf("ready observation queue=%v error=%v", queue, err)
	}
}

func TestRuntimeHealthAutoRemediationUsesThreeBoundedPhasesAndClearsWhenHealthy(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "runtime-auto-remediation")
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("ClaimJob() job=%v error=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, successfulProvisionResult(instance), nil); err != nil {
		t.Fatal(err)
	}
	targets, err := dataStore.ListObservationTargets(ctx, host.ID)
	if err != nil || len(targets) != 1 {
		t.Fatalf("ListObservationTargets() targets=%+v error=%v", targets, err)
	}

	base := time.Now().UTC()
	observation := domain.InstanceObservation{
		InstanceID: targets[0].InstanceID, TargetGeneration: targets[0].Generation,
		Status: domain.ObservationDegraded, Summary: "Runtime drift",
		Checks:     runtimeRepairableDriftChecks(),
		ObservedAt: base,
	}
	recordAndTrack := func(observedAt, attemptedAt time.Time) RuntimeRemediationDecision {
		t.Helper()
		currentTargets, err := dataStore.ListObservationTargets(ctx, host.ID)
		if err != nil || len(currentTargets) != 1 {
			t.Fatalf("ListObservationTargets() targets=%+v error=%v", currentTargets, err)
		}
		observation.TargetGeneration = currentTargets[0].Generation
		observation.ObservedAt = observedAt
		if err := dataStore.RecordObservations(ctx, host.ID, []domain.InstanceObservation{observation}, attemptedAt); err != nil {
			t.Fatal(err)
		}
		decision, err := dataStore.TrackRuntimeHealthObservation(
			ctx, observation, attemptedAt, "11111111-1111-4111-8111-111111111111",
		)
		if err != nil {
			t.Fatal(err)
		}
		return decision
	}

	first := recordAndTrack(base, base.Add(time.Second))
	if first.Queue || first.State.Status != "MONITORING" || first.State.ConsecutiveDrift != 1 {
		t.Fatalf("first drift decision=%+v", first)
	}
	second := recordAndTrack(base.Add(2*time.Second), base.Add(3*time.Second))
	if !second.Queue || second.State.TotalAttempts != 1 || second.State.Phase != 1 || second.State.AttemptInPhase != 1 {
		t.Fatalf("second drift decision=%+v", second)
	}
	if err := dataStore.RecordRuntimeRemediationQueueFailure(ctx, instance.ID, "test failure", base.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	state, err := dataStore.GetRuntimeRemediation(ctx, instance.ID)
	if err != nil || state == nil || state.TotalAttempts != 0 || state.AttemptInPhase != 0 || state.NextAttemptAt == nil {
		t.Fatalf("queue failure consumed an attempt: state=%+v error=%v", state, err)
	}

	queueAndFail := func(expectedAttempt int, decision RuntimeRemediationDecision) {
		t.Helper()
		now := decision.State.LastAttemptAt.Add(time.Millisecond)
		operationID := fmt.Sprintf("runtime-repair-operation-%d", expectedAttempt)
		jobID := fmt.Sprintf("runtime-repair-job-%d", expectedAttempt)
		operation := domain.Operation{
			ID: operationID, InstanceID: instance.ID, WorkflowID: decision.State.WorkflowID,
			Actor: "SYSTEM", Type: "REPAIR_RUNTIME", Status: domain.OperationPending,
			Summary: "Test automatic runtime repair", CreatedAt: now, UpdatedAt: now,
		}
		job := domain.Job{
			ID: jobID, OperationID: operationID, HostID: host.ID, InstanceID: instance.ID,
			Type: "instance.runtime.repair", Status: domain.JobPending, Payload: json.RawMessage(`{}`),
			CreatedAt: now, UpdatedAt: now,
		}
		if err := dataStore.QueueRuntimeRepair(ctx, domain.InstanceRunning, operation, job, decision.State); err != nil {
			t.Fatalf("QueueRuntimeRepair(attempt=%d): %v", expectedAttempt, err)
		}
		claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
		if err != nil || claimed == nil || claimed.ID != jobID {
			t.Fatalf("ClaimJob(attempt=%d) job=%+v error=%v", expectedAttempt, claimed, err)
		}
		if err := dataStore.AcknowledgeJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := dataStore.CompleteJob(ctx, host.ID, claimed.ID, claimed.LeaseToken, domain.JobResult{
			Success: false, Error: "runtime still unhealthy",
		}, nil); err != nil {
			t.Fatal(err)
		}
	}

	observedAt := base.Add(5 * time.Second)
	attemptedAt := state.NextAttemptAt.Add(time.Millisecond)
	for expectedAttempt := 1; expectedAttempt <= 9; expectedAttempt++ {
		decision := recordAndTrack(observedAt, attemptedAt)
		if !decision.Queue || decision.State.TotalAttempts != expectedAttempt {
			t.Fatalf("attempt %d decision=%+v", expectedAttempt, decision)
		}
		expectedPhase := (expectedAttempt-1)/runtimeRemediationAttemptsPerPhase + 1
		if decision.State.Phase != expectedPhase {
			t.Fatalf("attempt %d phase=%d want=%d", expectedAttempt, decision.State.Phase, expectedPhase)
		}
		queueAndFail(expectedAttempt, decision)
		if expectedAttempt < 9 {
			state, err = dataStore.GetRuntimeRemediation(ctx, instance.ID)
			if err != nil || state == nil || state.NextAttemptAt == nil {
				t.Fatalf("attempt %d state=%+v error=%v", expectedAttempt, state, err)
			}
			attemptedAt = state.NextAttemptAt.Add(time.Millisecond)
		}
		observedAt = observedAt.Add(time.Second)
	}
	exhausted, err := dataStore.GetRuntimeRemediation(ctx, instance.ID)
	if err != nil || exhausted == nil || exhausted.Status != "EXHAUSTED" || exhausted.TotalAttempts != 9 ||
		exhausted.Phase != 3 || exhausted.AttemptInPhase != 3 || exhausted.NextAttemptAt != nil {
		t.Fatalf("exhausted state=%+v error=%v", exhausted, err)
	}

	observation.Status = domain.ObservationInSync
	observation.Summary = "Healthy"
	for index := range observation.Checks {
		if observation.Checks[index].Name == "runtime" {
			observation.Checks[index].Status = domain.ObservationCheckOK
		}
	}
	observation.ObservedAt = observedAt.Add(time.Second)
	healthyAttemptAt := base.Add(21 * time.Minute)
	currentTargets, err := dataStore.ListObservationTargets(ctx, host.ID)
	if err != nil || len(currentTargets) != 1 {
		t.Fatalf("ListObservationTargets() targets=%+v error=%v", currentTargets, err)
	}
	observation.TargetGeneration = currentTargets[0].Generation
	if err := dataStore.RecordObservations(ctx, host.ID, []domain.InstanceObservation{observation}, healthyAttemptAt); err != nil {
		t.Fatal(err)
	}
	decision, err := dataStore.TrackRuntimeHealthObservation(
		ctx, observation, healthyAttemptAt, "22222222-2222-4222-8222-222222222222",
	)
	if err != nil || decision.Queue {
		t.Fatalf("healthy decision=%+v error=%v", decision, err)
	}
	cleared, err := dataStore.GetRuntimeRemediation(ctx, instance.ID)
	if err != nil || cleared != nil {
		t.Fatalf("healthy observation did not clear remediation: state=%+v error=%v", cleared, err)
	}
}

func TestRuntimeHealthAggregationIncludesEndpointAndRequiresSafePrerequisites(t *testing.T) {
	observation := domain.InstanceObservation{Checks: []domain.ObservationCheck{
		{Name: "runtime", Status: domain.ObservationCheckOK},
		{Name: "health_endpoint", Status: domain.ObservationCheckDrift},
	}}
	if status := runtimeHealthCheckStatus(observation); status != domain.ObservationCheckDrift {
		t.Fatalf("runtimeHealthCheckStatus()=%q want=%q", status, domain.ObservationCheckDrift)
	}
	if runtimeRepairPrerequisitesOK(observation) {
		t.Fatal("runtime repair accepted an observation without managed resource prerequisites")
	}
	observation.Checks = append(observation.Checks, runtimeRepairableDriftChecks()[:6]...)
	if !runtimeRepairPrerequisitesOK(observation) {
		t.Fatal("runtime repair rejected complete managed resource prerequisites")
	}
}

func TestOperationsCursorPaginationIsStableForEqualTimestamps(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "operations-page")
	if _, err := dataStore.db.ExecContext(ctx, `DELETE FROM jobs; DELETE FROM operations`); err != nil {
		t.Fatal(err)
	}
	createdAt := time.Now().UTC()
	for _, id := range []string{"operation-a", "operation-b", "operation-c", "operation-d", "operation-e"} {
		if _, err := dataStore.db.ExecContext(ctx, `
INSERT INTO operations (
  id, instance_id, workflow_id, actor, type, status, summary, metadata, error, created_at, updated_at
) VALUES (?, ?, '', 'FLEET_ADMIN', 'TEST', 'SUCCEEDED', ?, '{}', '', ?, ?)`,
			id, instance.ID, id, createdAt, createdAt,
		); err != nil {
			t.Fatal(err)
		}
	}
	for _, job := range []struct {
		id        string
		stage     string
		updatedAt time.Time
	}{
		{id: "job-operation-e-old", stage: "STOPPING", updatedAt: createdAt.Add(time.Second)},
		{id: "job-operation-e-new", stage: "VERIFYING_VERSION", updatedAt: createdAt.Add(2 * time.Second)},
	} {
		progress, err := json.Marshal(domain.JobProgress{Stage: job.stage})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dataStore.db.ExecContext(ctx, `
INSERT INTO jobs (
  id, operation_id, host_id, instance_id, type, status, payload, progress, created_at, updated_at
) VALUES (?, 'operation-e', ?, ?, 'test.operation', 'RUNNING', '{}', ?, ?, ?)`,
			job.id, host.ID, instance.ID, progress, createdAt, job.updatedAt,
		); err != nil {
			t.Fatal(err)
		}
	}

	var (
		cursor         *OperationCursor
		ids            []string
		newestProgress string
	)
	for {
		page, err := dataStore.ListOperationsPage(ctx, 2, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, operation := range page.Items {
			ids = append(ids, operation.ID)
			if operation.ID == "operation-e" && operation.Progress != nil {
				newestProgress = operation.Progress.Stage
			}
		}
		if page.NextCursor == nil {
			break
		}
		cursor = page.NextCursor
	}
	want := []string{"operation-e", "operation-d", "operation-c", "operation-b", "operation-a"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("paginated operation ids=%v want=%v", ids, want)
	}
	if newestProgress != "VERIFYING_VERSION" {
		t.Fatalf("paginated operation progress=%q want latest progress", newestProgress)
	}
	storedOperation, err := dataStore.GetOperation(ctx, "operation-e")
	if err != nil {
		t.Fatal(err)
	}
	if storedOperation.Progress == nil || storedOperation.Progress.Stage != "VERIFYING_VERSION" {
		t.Fatalf("GetOperation() progress=%+v want latest progress", storedOperation.Progress)
	}
	legacy, err := dataStore.ListOperations(ctx, 2)
	if err != nil || len(legacy) != 2 || legacy[0].ID != "operation-e" || legacy[1].ID != "operation-d" {
		t.Fatalf("ListOperations() compatibility result=%+v error=%v", legacy, err)
	}
	if _, err := dataStore.ListOperationsPage(ctx, 2, &OperationCursor{}); err == nil {
		t.Fatal("ListOperationsPage() accepted an incomplete cursor")
	}
}

func TestOperationQueryPlansUseSupportingIndexes(t *testing.T) {
	ctx, dataStore, _, _ := newFleetFixture(t, "operations-query-plan")
	rows, err := dataStore.db.QueryContext(ctx, `
EXPLAIN QUERY PLAN
SELECT o.id,
       COALESCE((
         SELECT j.progress
         FROM jobs j
         WHERE j.operation_id=o.id
         ORDER BY j.updated_at DESC, j.id DESC
         LIMIT 1
       ), '{}')
FROM operations o
ORDER BY o.created_at DESC, o.id DESC
LIMIT 101`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var details []string
	for rows.Next() {
		var (
			id      int
			parent  int
			notUsed int
			detail  string
		)
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(details, "\n")
	if !strings.Contains(plan, "idx_jobs_operation_progress") {
		t.Fatalf("operation progress query plan does not use latest-progress index:\n%s", plan)
	}
	if !strings.Contains(plan, "idx_operations_cursor") {
		t.Fatalf("operation pagination query plan does not use cursor index:\n%s", plan)
	}
	if strings.Contains(plan, "USE TEMP B-TREE") {
		t.Fatalf("operation pagination query plan still requires a temporary sort:\n%s", plan)
	}
}

func runtimeRepairableDriftChecks() []domain.ObservationCheck {
	return []domain.ObservationCheck{
		{Name: "managed_path", Status: domain.ObservationCheckOK, Detail: "Managed path exists"},
		{Name: "manifest", Status: domain.ObservationCheckOK, Detail: "Manifest exists"},
		{Name: "environment", Status: domain.ObservationCheckOK, Detail: "Environment exists"},
		{Name: "workspace", Status: domain.ObservationCheckOK, Detail: "Workspace exists"},
		{Name: "docker_daemon", Status: domain.ObservationCheckOK, Detail: "Docker daemon responded"},
		{Name: "data_volume", Status: domain.ObservationCheckOK, Detail: "Data volume exists"},
		{
			Name: "runtime", Status: domain.ObservationCheckDrift,
			Detail: "Desired RUNNING state does not match container state or health",
		},
	}
}

func newFleetFixture(t *testing.T, suffix string) (context.Context, *Store, domain.Host, domain.Instance) {
	t.Helper()
	ctx := context.Background()
	dataStore, err := Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	now := time.Now().UTC()
	host := domain.Host{ID: "host-" + suffix, Name: "host-" + suffix, Hostname: "host", OS: "test", Arch: "test", AgentVersion: "test", LastSeenAt: now, CreatedAt: now}
	if err := dataStore.EnrollHost(ctx, host, "token-hash"); err != nil {
		t.Fatal(err)
	}
	instance := testInstance("instance-"+suffix, "fleet-"+suffix, host.ID, 8650, 9130, now)
	if err := createTestInstance(ctx, dataStore, instance, suffix); err != nil {
		t.Fatal(err)
	}
	return ctx, dataStore, host, instance
}

func TestQueueHealthReadsOldestPendingSQLiteAggregate(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "queue-health")

	health, err := dataStore.QueueHealth(ctx, instance.CreatedAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("QueueHealth() with a pending job: %v", err)
	}
	if health.Pending != 1 || health.Active != 1 || len(health.Hosts) != 1 {
		t.Fatalf("QueueHealth() = %+v, want one pending active job", health)
	}
	hostHealth := health.Hosts[0]
	if hostHealth.HostID != host.ID || hostHealth.OldestPendingAt == nil {
		t.Fatalf("host queue health = %+v, want oldest pending timestamp", hostHealth)
	}
	if !hostHealth.OldestPendingAt.Equal(instance.CreatedAt) {
		t.Fatalf("oldest pending = %s, want %s", hostHealth.OldestPendingAt, instance.CreatedAt)
	}
}

func testInstance(id, name, hostID string, apiPort, dashboardPort int, now time.Time) domain.Instance {
	return domain.Instance{
		ID: id, Name: name, HostID: hostID, Status: domain.InstanceProvisioning, Image: "image",
		Provider: "openai-codex", Model: "gpt", Reasoning: "medium", ServiceTier: "normal", CodexConfigured: true,
		APIPort: apiPort, DashboardPort: dashboardPort, CreatedAt: now, UpdatedAt: now,
	}
}

func createTestInstance(ctx context.Context, dataStore *Store, instance domain.Instance, suffix string) error {
	operation := domain.Operation{ID: "operation-" + suffix, InstanceID: instance.ID, Type: "PROVISION", Status: domain.OperationPending, Summary: "Provision", CreatedAt: instance.CreatedAt, UpdatedAt: instance.UpdatedAt}
	payload, _ := json.Marshal(domain.ProvisionPayload{InstanceID: instance.ID, Name: instance.Name})
	job := domain.Job{ID: "job-" + suffix, OperationID: operation.ID, HostID: instance.HostID, InstanceID: instance.ID, Type: "instance.provision", Status: domain.JobPending, Payload: payload, CreatedAt: instance.CreatedAt, UpdatedAt: instance.UpdatedAt}
	return dataStore.CreateInstance(ctx, instance, operation, job)
}

func TestInstancePublishingStateIsAtomicUniqueAndOwnershipScoped(t *testing.T) {
	ctx, dataStore, host, first := newFleetFixture(t, "publishing-a")
	now := time.Now().UTC()
	second := testInstance("instance-publishing-b", "fleet-publishing-b", host.ID, 8651, 9131, now)
	if err := createTestInstance(ctx, dataStore, second, "publishing-b"); err != nil {
		t.Fatal(err)
	}
	operation := domain.Operation{
		ID: "operation-publish-a", InstanceID: first.ID, Type: "PUBLISH_DASHBOARD",
		Status: domain.OperationPending, Summary: "Publish dashboard", CreatedAt: now, UpdatedAt: now,
		Progress: &domain.JobProgress{Stage: "VALIDATING_HOSTNAME", Steps: []domain.OperationStep{{Stage: "VALIDATING_HOSTNAME", Status: "running"}}},
	}
	if err := dataStore.StartInstancePublishing(ctx, first.ID, "aksa.example.com", operation); err != nil {
		t.Fatal(err)
	}
	stored, err := dataStore.GetInstance(ctx, first.ID)
	if err != nil || stored.PublicHostname != "aksa.example.com" {
		t.Fatalf("published instance=%+v err=%v", stored, err)
	}
	storedOperation, err := dataStore.GetOperation(ctx, operation.ID)
	if err != nil || storedOperation.Progress == nil || storedOperation.Progress.Stage != "VALIDATING_HOSTNAME" {
		t.Fatalf("publishing operation=%+v err=%v", storedOperation, err)
	}
	conflicting := operation
	conflicting.ID = "operation-publish-b"
	conflicting.InstanceID = second.ID
	if err := dataStore.StartInstancePublishing(ctx, second.ID, "aksa.example.com", conflicting); err == nil || !strings.Contains(err.Error(), "already assigned") {
		t.Fatalf("duplicate hostname error=%v", err)
	}
	if _, err := dataStore.GetOperation(ctx, conflicting.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("conflicting operation was not rolled back: %v", err)
	}

	resource := domain.RemoteAccessResource{
		InstanceID: first.ID, Kind: "dns", ResourceID: "dns-record-a", Hostname: "aksa.example.com",
		TunnelID: "22222222-2222-4222-8222-222222222222", ZoneID: "zone-a", CreatedAt: now, UpdatedAt: now,
	}
	if err := dataStore.PutRemoteAccessResource(ctx, resource); err != nil {
		t.Fatal(err)
	}
	resources, err := dataStore.ListRemoteAccessResources(ctx)
	if err != nil || len(resources) != 1 || resources[0].ResourceID != resource.ResourceID {
		t.Fatalf("ownership resources=%+v err=%v", resources, err)
	}
	if err := dataStore.DeleteRemoteAccessResource(ctx, first.ID, resource.Kind, resource.Hostname); err != nil {
		t.Fatal(err)
	}
	resources, err = dataStore.ListRemoteAccessResources(ctx)
	if err != nil || len(resources) != 0 {
		t.Fatalf("ownership cleanup=%+v err=%v", resources, err)
	}
}

func TestControlPlaneOperationDoesNotRequireAnInstance(t *testing.T) {
	ctx, dataStore, _, _ := newFleetFixture(t, "system-operation")
	now := time.Now().UTC()
	operation := domain.Operation{
		ID: "operation-instance-publishing", Type: "CONNECT_INSTANCE_PUBLISHING", Status: domain.OperationPending,
		Summary: "Connect and verify instance publishing", CreatedAt: now, UpdatedAt: now,
	}
	if err := dataStore.CreateControlPlaneOperation(ctx, operation); err != nil {
		t.Fatal(err)
	}
	stored, err := dataStore.GetOperation(ctx, operation.ID)
	if err != nil || stored.InstanceID != "" || stored.Type != operation.Type {
		t.Fatalf("control-plane operation=%+v err=%v", stored, err)
	}
}

func TestControlPlaneOperationRejectsLateNonTerminalUpdate(t *testing.T) {
	ctx, dataStore, _, instance := newFleetFixture(t, "operation-terminal-fence")
	now := time.Now().UTC()
	operation := domain.Operation{
		ID: "operation-terminal-fence", InstanceID: instance.ID, Type: "PUBLISH_DASHBOARD",
		Status: domain.OperationPending, Summary: "Publish dashboard", CreatedAt: now, UpdatedAt: now,
	}
	if err := dataStore.CreateControlPlaneOperation(ctx, operation); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.UpdateControlPlaneOperation(ctx, operation.ID, domain.OperationSucceeded,
		domain.JobProgress{Stage: "CHECKING_PUBLIC_ENDPOINT"}, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.UpdateControlPlaneOperation(ctx, operation.ID, domain.OperationRunning,
		domain.JobProgress{Stage: "CREATING_DNS"}, "", now.Add(2*time.Second)); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("late update error=%v, want ErrStateChanged", err)
	}
	stored, err := dataStore.GetOperation(ctx, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.OperationSucceeded || stored.Progress == nil || stored.Progress.Stage != "CHECKING_PUBLIC_ENDPOINT" {
		t.Fatalf("terminal operation changed: %+v", stored)
	}
}

func TestStalePublishingOperationsUseTimestampFencing(t *testing.T) {
	ctx, dataStore, _, instance := newFleetFixture(t, "stale-publishing")
	now := time.Now().UTC()
	oldAt := now.Add(-10 * time.Minute)
	operations := []domain.Operation{
		{
			ID: "publish-stale", InstanceID: instance.ID, Type: "PUBLISH_DASHBOARD",
			Status: domain.OperationRunning, Summary: "Publish stale dashboard",
			Metadata: json.RawMessage(`{"public_hostname":"stale.example.com"}`), CreatedAt: oldAt, UpdatedAt: oldAt,
		},
		{
			ID: "publish-fresh", InstanceID: instance.ID, Type: "PUBLISH_DASHBOARD",
			Status: domain.OperationRunning, Summary: "Publish fresh dashboard",
			Metadata: json.RawMessage(`{"public_hostname":"fresh.example.com"}`), CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "other-stale", InstanceID: instance.ID, Type: "TEST",
			Status: domain.OperationRunning, Summary: "Other stale operation", CreatedAt: oldAt, UpdatedAt: oldAt,
		},
	}
	for _, operation := range operations {
		if err := dataStore.CreateControlPlaneOperation(ctx, operation); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := now.Add(-5 * time.Minute)
	stale, err := dataStore.ListStalePublishingOperations(ctx, cutoff, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].ID != "publish-stale" {
		t.Fatalf("stale publishing operations=%+v", stale)
	}

	if err := dataStore.UpdateControlPlaneOperation(ctx, "publish-stale", domain.OperationRunning,
		domain.JobProgress{Stage: "CHECKING_PUBLIC_ENDPOINT"}, "", now); err != nil {
		t.Fatal(err)
	}
	changed, err := dataStore.FinalizeStalePublishingOperation(ctx, "publish-stale", cutoff,
		domain.OperationFailed, domain.JobProgress{Stage: "CHECKING_PUBLIC_ENDPOINT"}, "interrupted", now.Add(time.Second))
	if err != nil || changed {
		t.Fatalf("finalize after fresh progress changed=%t err=%v", changed, err)
	}

	if _, err := dataStore.db.ExecContext(ctx, `UPDATE operations SET updated_at=? WHERE id=?`, oldAt, "publish-stale"); err != nil {
		t.Fatal(err)
	}
	changed, err = dataStore.FinalizeStalePublishingOperation(ctx, "publish-stale", cutoff,
		domain.OperationFailed, domain.JobProgress{Stage: "CHECKING_PUBLIC_ENDPOINT"}, "interrupted", now.Add(2*time.Second))
	if err != nil || !changed {
		t.Fatalf("finalize stale operation changed=%t err=%v", changed, err)
	}
	stored, err := dataStore.GetOperation(ctx, "publish-stale")
	if err != nil || stored.Status != domain.OperationFailed || stored.Error != "interrupted" {
		t.Fatalf("finalized stale operation=%+v err=%v", stored, err)
	}
}

func TestHasActiveRecoveryPointReference(t *testing.T) {
	const targetID = "recovery-point-target"
	cases := []struct {
		name     string
		jobType  string
		status   string
		payload  any
		expected bool
	}{
		{
			name: "pending restore", jobType: "instance.recovery.restore", status: domain.JobPending,
			payload: domain.RecoveryRestorePayload{RecoveryPointID: targetID}, expected: true,
		},
		{
			name: "leased restore", jobType: "instance.recovery.restore", status: domain.JobLeased,
			payload: domain.RecoveryRestorePayload{RecoveryPointID: targetID}, expected: true,
		},
		{
			name: "running restore", jobType: "instance.recovery.restore", status: domain.JobRunning,
			payload: domain.RecoveryRestorePayload{RecoveryPointID: targetID}, expected: true,
		},
		{
			name: "succeeded restore", jobType: "instance.recovery.restore", status: domain.JobSucceeded,
			payload: domain.RecoveryRestorePayload{RecoveryPointID: targetID}, expected: false,
		},
		{
			name: "failed restore", jobType: "instance.recovery.restore", status: domain.JobFailed,
			payload: domain.RecoveryRestorePayload{RecoveryPointID: targetID}, expected: false,
		},
		{
			name: "pending update backup", jobType: "instance.hermes.update", status: domain.JobPending,
			payload: domain.HermesUpdatePayload{
				Backup: domain.RecoveryPointPayload{RecoveryPointID: targetID},
			},
			expected: true,
		},
		{
			name: "running update rollback", jobType: "instance.hermes.update", status: domain.JobRunning,
			payload: domain.HermesUpdatePayload{
				Upgrade: domain.HermesUpgradePayload{
					Rollback: domain.RecoveryRestorePayload{RecoveryPointID: targetID},
				},
			},
			expected: true,
		},
		{
			name: "leased upgrade", jobType: "instance.hermes.upgrade", status: domain.JobLeased,
			payload:  domain.HermesUpgradePayload{RecoveryPointID: targetID},
			expected: true,
		},
		{
			name: "active unrelated restore", jobType: "instance.recovery.restore", status: domain.JobPending,
			payload: domain.RecoveryRestorePayload{RecoveryPointID: "other-point"}, expected: false,
		},
	}

	for index, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			suffix := fmt.Sprintf("recovery-reference-%d", index)
			ctx, dataStore, host, instance := newFleetFixture(t, suffix)
			now := time.Now().UTC()
			completeTestProvisionForAction(t, ctx, dataStore, suffix, instance.ID, now)
			payload, err := json.Marshal(testCase.payload)
			if err != nil {
				t.Fatal(err)
			}
			operation := domain.Operation{
				ID: "operation-reference-" + strconv.Itoa(index), InstanceID: instance.ID,
				Type: "RESTORE", Status: domain.OperationPending, Summary: "Restore",
				CreatedAt: now, UpdatedAt: now,
			}
			job := domain.Job{
				ID: "job-reference-" + strconv.Itoa(index), OperationID: operation.ID,
				HostID: host.ID, InstanceID: instance.ID, Type: testCase.jobType,
				Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now,
			}
			if err := dataStore.QueueAction(ctx, domain.InstanceStopped, domain.InstanceRestoring, operation, job); err != nil {
				t.Fatal(err)
			}
			if testCase.status != domain.JobPending {
				if _, err := dataStore.db.ExecContext(ctx, `UPDATE jobs SET status=? WHERE id=?`, testCase.status, job.ID); err != nil {
					t.Fatal(err)
				}
			}

			active, err := dataStore.HasActiveRecoveryPointReference(ctx, targetID)
			if err != nil {
				t.Fatal(err)
			}
			if active != testCase.expected {
				t.Fatalf("HasActiveRecoveryPointReference()=%t want=%t", active, testCase.expected)
			}
		})
	}
}

func TestHasActiveRecoveryPointReferenceRejectsMalformedActivePayload(t *testing.T) {
	ctx, dataStore, host, instance := newFleetFixture(t, "recovery-reference-invalid")
	now := time.Now().UTC()
	completeTestProvisionForAction(t, ctx, dataStore, "recovery-reference-invalid", instance.ID, now)
	operation := domain.Operation{
		ID: "operation-reference-invalid", InstanceID: instance.ID, Type: "RESTORE",
		Status: domain.OperationPending, Summary: "Restore", CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: "job-reference-invalid", OperationID: operation.ID, HostID: host.ID,
		InstanceID: instance.ID, Type: "instance.recovery.restore", Status: domain.JobPending,
		Payload: json.RawMessage(`{"recovery_point_id":`), CreatedAt: now, UpdatedAt: now,
	}
	if err := dataStore.QueueAction(ctx, domain.InstanceStopped, domain.InstanceRestoring, operation, job); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.HasActiveRecoveryPointReference(ctx, "recovery-point-target"); err == nil {
		t.Fatal("HasActiveRecoveryPointReference() accepted a malformed active job payload")
	}
}

func TestFleetPolicyAndRolloutStateAreDurable(t *testing.T) {
	ctx, dataStore, _, instance := newFleetFixture(t, "policy")
	now := time.Now().UTC()
	policy := domain.FleetPolicy{
		ID: "policy-01", Name: "Stable Hermes", Description: "Keep selected instances current",
		Status: domain.PolicyEnabled, DesiredHermes: domain.PolicyDesiredHermesLatestStable,
		Strategy: domain.PolicyStrategyOneAtATime, ScopeInstanceIDs: []string{instance.ID},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := dataStore.CreatePolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	policies, err := dataStore.ListPolicies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(policies) != 1 || len(policies[0].ScopeInstanceIDs) != 1 || policies[0].ScopeInstanceIDs[0] != instance.ID {
		t.Fatalf("ListPolicies()=%+v", policies)
	}
	rollout := domain.Operation{
		ID: "policy-rollout-01", WorkflowID: "policy-rollout-01", Type: "ROLLOUT_POLICY",
		Status: domain.OperationPending, Summary: "Roll out Stable Hermes",
		Metadata:  json.RawMessage(`{"policy_id":"policy-01","strategy":"ONE_AT_A_TIME"}`),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := dataStore.CreatePolicyRollout(ctx, rollout, policy.ID, []string{instance.ID}); err != nil {
		t.Fatal(err)
	}
	active, err := dataStore.ListActivePolicyRollouts(ctx)
	if err != nil || len(active) != 1 || active[0].ID != rollout.ID {
		t.Fatalf("ListActivePolicyRollouts()=%+v err=%v", active, err)
	}
	if err := dataStore.UpdatePolicyRolloutTarget(ctx, rollout.ID, instance.ID, "", domain.PolicyTargetBlocked, "Host offline", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	targets, err := dataStore.ListPolicyRolloutTargets(ctx, rollout.ID)
	if err != nil || len(targets) != 1 || targets[0].Status != domain.PolicyTargetBlocked || targets[0].Detail != "Host offline" {
		t.Fatalf("ListPolicyRolloutTargets()=%+v err=%v", targets, err)
	}
	if err := dataStore.UpdateControlPlaneOperation(ctx, rollout.ID, domain.OperationFailed, domain.JobProgress{Stage: "compliance_verified"}, "blocked", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	policy.Description = "Updated after rollout"
	policy.UpdatedAt = now.Add(2 * time.Second)
	if err := dataStore.UpdatePolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	stored, err := dataStore.GetPolicy(ctx, policy.ID)
	if err != nil || stored.Description != policy.Description {
		t.Fatalf("GetPolicy()=%+v err=%v", stored, err)
	}
}

func completeTestProvisionForAction(t *testing.T, ctx context.Context, dataStore *Store, suffix, instanceID string, now time.Time) {
	t.Helper()
	if _, err := dataStore.db.ExecContext(ctx, `UPDATE jobs SET status=?, updated_at=? WHERE id=?`,
		domain.JobSucceeded, now, "job-"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.db.ExecContext(ctx, `UPDATE operations SET status=?, updated_at=? WHERE id=?`,
		domain.OperationSucceeded, now, "operation-"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.db.ExecContext(ctx, `UPDATE instances SET status=?, updated_at=? WHERE id=?`,
		domain.InstanceStopped, now, instanceID); err != nil {
		t.Fatal(err)
	}
}

func queueTestHermesUpdate(
	t *testing.T,
	ctx context.Context,
	dataStore *Store,
	host domain.Host,
	instance domain.Instance,
	currentImage, currentImageID, targetImage, originalStatus string,
) domain.Job {
	t.Helper()
	now := time.Now().UTC()
	backupID := "recovery-" + strings.Repeat("c", 32)
	payload, err := json.Marshal(domain.HermesUpdatePayload{
		OriginalStatus: originalStatus,
		Backup: domain.RecoveryPointPayload{
			RecoveryPointID: backupID, InstanceID: instance.ID, Name: instance.Name,
			Image: currentImage, ImageID: currentImageID,
		},
		Upgrade: domain.HermesUpgradePayload{
			InstanceID: instance.ID, Name: instance.Name,
			CurrentImage: currentImage, CurrentImageID: currentImageID,
			TargetImage: targetImage, TargetVersion: "0.19.0",
			RecoveryPointID: backupID, ProjectName: "hermes-fleet-" + instance.Name,
			DataVolume: "hermes-fleet-" + instance.Name + "-data", ManagedPath: "/managed/" + instance.Name,
			Rollback: domain.RecoveryRestorePayload{
				RecoveryPointID: backupID, InstanceID: instance.ID, Name: instance.Name,
				Image: currentImage, ImageID: currentImageID, RequireImageID: true,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	operation := domain.Operation{
		ID: "operation-hermes-" + instance.ID, InstanceID: instance.ID, Type: "UPGRADE_HERMES",
		Status: domain.OperationPending, Summary: "Update Hermes", CreatedAt: now, UpdatedAt: now,
		Metadata: json.RawMessage(`{"original_status":"` + originalStatus + `","update_kind":"VERSION_UPDATE"}`),
	}
	job := domain.Job{
		ID: "job-hermes-" + instance.ID, OperationID: operation.ID, HostID: host.ID, InstanceID: instance.ID,
		Type: "instance.hermes.update", Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now,
	}
	if err := dataStore.QueueAction(ctx, domain.InstanceStopped, domain.InstanceUpdating, operation, job); err != nil {
		t.Fatal(err)
	}
	return job
}

func successfulProvisionResult(instance domain.Instance) domain.JobResult {
	project, volume, directory := domain.ManagedIdentity(instance.ID, instance.Name)
	return domain.JobResult{
		Success: true, ProjectName: project, DataVolume: volume,
		ManagedPath: filepath.Join("/managed", directory), ImageID: "sha256:" + strings.Repeat("a", 64),
	}
}
