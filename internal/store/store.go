package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/providers"
)

var (
	ErrNotFound             = errors.New("resource not found")
	ErrLeaseLost            = errors.New("job lease is no longer active")
	ErrInvalidJobResult     = errors.New("job completion result is invalid")
	ErrStateChanged         = errors.New("resource state changed before the operation was queued")
	ErrInstanceBusy         = errors.New("instance already has an active operation")
	ErrHostBusy             = errors.New("host has a nonterminal job")
	ErrHostIdentityMismatch = errors.New("host identity confirmation does not match")
	ErrObservationOwnership = errors.New("observation target does not belong to this host")
	ErrObservationNotReady  = errors.New("instance is not ready for observation")
	ErrObservationBusy      = errors.New("instance already has a pending observation request")
	ErrQueueCapacity        = errors.New("host job queue has reached its admission limit")
	immutableImageIDPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

const JobQueueMaxPerHost = 100

type Store struct {
	db *sql.DB
}

type PendingInstanceDeletion struct {
	InstanceID  string
	OperationID string
}

const (
	codexConfigurationStateMigration = "20260727_codex_configuration_state_v1"
	controlPlaneOperationMigration   = "20260808_control_plane_operations_v1"
	chatEventPayloadMigration        = "20260812_chat_event_payloads_v1"
)

func Open(path string) (*Store, error) {
	dataStore, err := open(path)
	if err == nil {
		return dataStore, nil
	}
	if !isSQLiteSHMSizeError(err) {
		return nil, err
	}
	quarantined, quarantineErr := quarantineInvalidSQLiteSHM(path)
	if quarantineErr != nil {
		return nil, fmt.Errorf("%w; recover invalid sqlite shared-memory file: %v", err, quarantineErr)
	}
	dataStore, retryErr := open(path)
	if retryErr != nil {
		return nil, fmt.Errorf("retry sqlite after quarantining %s: %w", filepath.Base(quarantined), retryErr)
	}
	return dataStore, nil
}

func open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", sqliteDataSourceName(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure sqlite: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := s.reconcileOrphanedChatWork(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func sqliteDataSourceName(path string) string {
	dsn := url.URL{Scheme: "file", Path: path}
	query := dsn.Query()
	// These connection-local pragmas must be applied every time database/sql
	// opens a replacement physical connection. Applying them through a one-off
	// Exec only configures the first connection and silently loses foreign-key
	// enforcement and the busy timeout after a bad connection is discarded.
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "foreign_keys(ON)")
	query.Add("_pragma", "busy_timeout(5000)")
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

func isSQLiteSHMSizeError(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqlite3.SQLITE_IOERR_SHMSIZE
}

func quarantineInvalidSQLiteSHM(path string) (string, error) {
	sharedMemoryPath := path + "-shm"
	info, err := os.Lstat(sharedMemoryPath)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", filepath.Base(sharedMemoryPath), err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("refusing to quarantine non-regular %s", filepath.Base(sharedMemoryPath))
	}
	const sqliteWALIndexRegionSize = 32 * 1024
	if info.Size() >= sqliteWALIndexRegionSize && info.Size()%sqliteWALIndexRegionSize == 0 {
		return "", fmt.Errorf("refusing to quarantine plausibly valid %s (%d bytes)", filepath.Base(sharedMemoryPath), info.Size())
	}
	quarantined := fmt.Sprintf("%s.invalid-%d", sharedMemoryPath, time.Now().UTC().UnixNano())
	if err := os.Rename(sharedMemoryPath, quarantined); err != nil {
		return "", fmt.Errorf("quarantine %s: %w", filepath.Base(sharedMemoryPath), err)
	}
	return quarantined, nil
}

func (s *Store) Close() error { return s.db.Close() }

// PrepareCleanHostRecovery converts a restored control-plane snapshot into a
// safe offline desired state. Instance data is restored separately by the Host
// Agent, so no recovered instance may be considered running before that step.
func (s *Store) PrepareCleanHostRecovery(ctx context.Context, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin clean-host recovery preparation: %w", err)
	}
	defer tx.Rollback()
	reason := "Interrupted by clean-host recovery import"
	if _, err := tx.ExecContext(ctx, `
UPDATE jobs
SET status=?, lease_token='', lease_expires_at=NULL, updated_at=?
WHERE status IN (?, ?, ?)`, domain.JobFailed, at, domain.JobPending, domain.JobLeased, domain.JobRunning); err != nil {
		return fmt.Errorf("fence restored jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE operations
SET status=?, error=CASE WHEN error='' THEN ? ELSE error END, updated_at=?
WHERE status IN (?, ?)`, domain.OperationFailed, reason, at, domain.OperationPending, domain.OperationRunning); err != nil {
		return fmt.Errorf("fence restored operations: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO chat_events (session_id, operation_id, sequence, type, created_at)
SELECT m.session_id, m.operation_id,
       COALESCE((SELECT MAX(e.sequence) FROM chat_events e WHERE e.operation_id=m.operation_id), 0)+1,
       ?, ?
FROM chat_messages m
WHERE m.role='user' AND m.status=? AND m.operation_id IS NOT NULL`,
		domain.ChatEventFailed, at, domain.ChatMessagePending); err != nil {
		return fmt.Errorf("terminate restored chat streams: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE chat_messages SET status=?, error=?, updated_at=? WHERE role='user' AND status=?`,
		domain.ChatMessageFailed, reason, at, domain.ChatMessagePending); err != nil {
		return fmt.Errorf("fail restored chat messages: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE chat_sessions SET last_error=?, updated_at=?
WHERE EXISTS (SELECT 1 FROM chat_messages m WHERE m.session_id=chat_sessions.id AND m.error=?)`,
		reason, at, reason); err != nil {
		return fmt.Errorf("fail restored chat sessions: %w", err)
	}
	for _, table := range []string{"instance_observations", "observation_requests", "runtime_reconcile_state", "runtime_remediation_state"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("clear restored %s: %w", table, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE instances
SET status=?, last_error='', updated_at=?
WHERE status<>?`, domain.InstanceStopped, at, domain.InstanceDeleted); err != nil {
		return fmt.Errorf("stop restored instances: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit clean-host recovery preparation: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint clean-host recovery database: %w", err)
	}
	return nil
}

// Ready verifies that SQLite is reachable and can acquire a write reservation
// without committing any application data.
func (s *Store) Ready(ctx context.Context) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		retry, err := s.readyOnce(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry || ctx.Err() != nil {
			return err
		}
	}
	return lastErr
}

func (s *Store) readyOnce(ctx context.Context) (bool, error) {
	connection, err := s.db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire sqlite connection: %w", err)
	}
	defer connection.Close()
	if _, err := connection.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return discardInterruptedSQLiteConnection(connection, "acquire sqlite write reservation", err)
	}
	rollbackContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := connection.ExecContext(rollbackContext, `ROLLBACK`); err != nil {
		return discardInterruptedSQLiteConnection(connection, "release sqlite write reservation", err)
	}
	return false, nil
}

func discardInterruptedSQLiteConnection(connection *sql.Conn, operation string, err error) (bool, error) {
	wrapped := fmt.Errorf("%s: %w", operation, err)
	if !isSQLiteInterrupt(err) {
		return false, wrapped
	}
	if discardErr := connection.Raw(func(any) error { return driver.ErrBadConn }); discardErr != nil &&
		!errors.Is(discardErr, driver.ErrBadConn) {
		return false, fmt.Errorf("%w; discard interrupted sqlite connection: %v", wrapped, discardErr)
	}
	return true, wrapped
}

func isSQLiteInterrupt(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqlite3.SQLITE_INTERRUPT
}

func (s *Store) CreateBackup(ctx context.Context, destination string) error {
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("resolve backup destination: %w", err)
	}
	if _, err := os.Lstat(absolute); err == nil {
		return errors.New("backup destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect backup destination: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM credential_reveals WHERE expires_at <= ?`, time.Now().UTC()); err != nil {
		return fmt.Errorf("purge expired credential reveals before backup: %w", err)
	}
	connection, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire sqlite backup connection: %w", err)
	}
	defer connection.Close()
	if err := connection.Raw(func(driverConnection any) error {
		backuper, ok := driverConnection.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("sqlite driver does not support online backups")
		}
		operation, err := backuper.NewBackup(absolute)
		if err != nil {
			return err
		}
		_, stepErr := operation.Step(-1)
		finishErr := operation.Finish()
		if stepErr != nil {
			return stepErr
		}
		return finishErr
	}); err != nil {
		_ = os.Remove(absolute)
		return fmt.Errorf("create sqlite backup: %w", err)
	}
	return nil
}

func (s *Store) VerifyBackup(ctx context.Context, path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve backup path: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return fmt.Errorf("inspect backup: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("backup is not a regular file")
	}
	dsn := url.URL{Scheme: "file", Path: absolute}
	query := dsn.Query()
	query.Set("mode", "ro")
	// Backups are complete, immutable snapshots. Opening them as immutable keeps
	// verification read-only and prevents SQLite from creating WAL/SHM sidecars
	// next to the artifact managed by the backup catalog.
	query.Set("immutable", "1")
	dsn.RawQuery = query.Encode()
	database, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return fmt.Errorf("open backup read-only: %w", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	var result string
	if err := database.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&result); err != nil {
		return fmt.Errorf("run sqlite quick check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("sqlite quick check failed: %s", result)
	}
	rows, err := database.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("run sqlite foreign key check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("sqlite foreign key check failed")
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read sqlite foreign key check: %w", err)
	}
	return nil
}

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS hosts (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  hostname TEXT NOT NULL,
  os TEXT NOT NULL,
  arch TEXT NOT NULL,
  agent_version TEXT NOT NULL,
  token_hash TEXT NOT NULL,
  last_seen_at DATETIME NOT NULL,
  created_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS instances (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  host_id TEXT NOT NULL REFERENCES hosts(id),
  status TEXT NOT NULL,
  image TEXT NOT NULL,
  image_id TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL,
  model TEXT NOT NULL,
  reasoning TEXT NOT NULL,
  service_tier TEXT NOT NULL,
  codex_configured INTEGER NOT NULL DEFAULT 0,
  api_port INTEGER NOT NULL,
  dashboard_port INTEGER NOT NULL,
  public_hostname TEXT NOT NULL DEFAULT '',
  project_name TEXT NOT NULL DEFAULT '',
  data_volume TEXT NOT NULL DEFAULT '',
  managed_path TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS operations (
  id TEXT PRIMARY KEY,
  instance_id TEXT REFERENCES instances(id),
  workflow_id TEXT NOT NULL DEFAULT '',
  actor TEXT NOT NULL DEFAULT 'FLEET_ADMIN',
  type TEXT NOT NULL,
  status TEXT NOT NULL,
  summary TEXT NOT NULL,
  metadata BLOB NOT NULL DEFAULT '{}',
  progress BLOB NOT NULL DEFAULT '{}',
  error TEXT NOT NULL DEFAULT '',
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS schema_migrations (
  name TEXT PRIMARY KEY,
  applied_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS jobs (
  id TEXT PRIMARY KEY,
  operation_id TEXT NOT NULL REFERENCES operations(id),
  host_id TEXT NOT NULL REFERENCES hosts(id),
  instance_id TEXT NOT NULL REFERENCES instances(id),
  type TEXT NOT NULL,
  status TEXT NOT NULL,
  payload BLOB NOT NULL,
  progress BLOB NOT NULL DEFAULT '{}',
  attempts INTEGER NOT NULL DEFAULT 0,
  lease_token TEXT NOT NULL DEFAULT '',
  lease_expires_at DATETIME,
  completion_hash TEXT NOT NULL DEFAULT '',
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_claim ON jobs(host_id, status, created_at);
CREATE INDEX IF NOT EXISTS idx_operations_created ON operations(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_jobs_operation_progress
  ON jobs(operation_id, updated_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_operations_cursor
  ON operations(created_at DESC, id DESC);
CREATE TABLE IF NOT EXISTS credential_reveals (
  operation_id TEXT PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
  ciphertext TEXT NOT NULL,
  expires_at DATETIME NOT NULL,
  created_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS instance_observations (
  instance_id TEXT PRIMARY KEY REFERENCES instances(id) ON DELETE CASCADE,
  host_id TEXT NOT NULL REFERENCES hosts(id),
  target_generation TEXT NOT NULL,
  hermes_version TEXT NOT NULL DEFAULT '',
  hermes_source TEXT NOT NULL DEFAULT '',
  model_catalog BLOB NOT NULL DEFAULT '[]',
  recommended_model TEXT NOT NULL DEFAULT '',
  provider_model_catalogs BLOB NOT NULL DEFAULT '{}',
  status TEXT NOT NULL,
  summary TEXT NOT NULL,
  checks BLOB NOT NULL,
  observed_at DATETIME NOT NULL,
  received_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS observation_requests (
  instance_id TEXT PRIMARY KEY REFERENCES instances(id) ON DELETE CASCADE,
  host_id TEXT NOT NULL REFERENCES hosts(id),
  request_id TEXT NOT NULL,
  requested_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS runtime_reconcile_state (
  instance_id TEXT PRIMARY KEY REFERENCES instances(id) ON DELETE CASCADE,
  desired_revision TEXT NOT NULL,
  consecutive_drift INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_observed_at DATETIME NOT NULL,
  last_attempt_at DATETIME
);
CREATE TABLE IF NOT EXISTS runtime_remediation_state (
  instance_id TEXT PRIMARY KEY REFERENCES instances(id) ON DELETE CASCADE,
  desired_revision TEXT NOT NULL,
  workflow_id TEXT NOT NULL,
  status TEXT NOT NULL,
  phase INTEGER NOT NULL DEFAULT 1,
  attempt_in_phase INTEGER NOT NULL DEFAULT 0,
  total_attempts INTEGER NOT NULL DEFAULT 0,
  consecutive_drift INTEGER NOT NULL DEFAULT 0,
  last_observed_at DATETIME NOT NULL,
  last_attempt_at DATETIME,
  next_attempt_at DATETIME,
  last_error TEXT NOT NULL DEFAULT '',
  updated_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS instance_messaging_configs (
  instance_id TEXT PRIMARY KEY REFERENCES instances(id) ON DELETE CASCADE,
  ciphertext TEXT NOT NULL,
  desired_revision TEXT NOT NULL,
  applied_revision TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  updated_at DATETIME NOT NULL,
  applied_at DATETIME
);
CREATE TABLE IF NOT EXISTS instance_mcp_configs (
  instance_id TEXT PRIMARY KEY REFERENCES instances(id) ON DELETE CASCADE,
  ciphertext TEXT NOT NULL,
  desired_revision TEXT NOT NULL,
  applied_revision TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  updated_at DATETIME NOT NULL,
  applied_at DATETIME
);
CREATE TABLE IF NOT EXISTS system_remote_access_config (
  id INTEGER PRIMARY KEY CHECK(id = 1),
  ciphertext TEXT NOT NULL,
  updated_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS remote_access_resources (
  instance_id TEXT NOT NULL REFERENCES instances(id),
  kind TEXT NOT NULL CHECK(kind IN ('dns', 'ingress')),
  resource_id TEXT NOT NULL DEFAULT '',
  hostname TEXT NOT NULL,
  tunnel_id TEXT NOT NULL,
  zone_id TEXT NOT NULL,
  origin_service TEXT NOT NULL DEFAULT '',
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL,
  PRIMARY KEY(instance_id, kind, hostname)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_remote_access_resource_hostname_kind
  ON remote_access_resources(hostname, kind);
CREATE TABLE IF NOT EXISTS fleet_health_state (
  component TEXT PRIMARY KEY,
  status TEXT NOT NULL,
  detail TEXT NOT NULL,
  updated_at DATETIME NOT NULL,
  last_success_at DATETIME
);
CREATE TABLE IF NOT EXISTS fleet_health_incidents (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  component TEXT NOT NULL,
  previous_status TEXT NOT NULL,
  status TEXT NOT NULL,
  detail TEXT NOT NULL,
  occurred_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS fleet_policies (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL COLLATE NOCASE UNIQUE,
  description TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK(status IN ('ENABLED', 'DISABLED')),
  desired_hermes TEXT NOT NULL CHECK(desired_hermes = 'LATEST_STABLE'),
  strategy TEXT NOT NULL CHECK(strategy IN ('ONE_AT_A_TIME', 'ALL_AT_ONCE')),
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS fleet_policy_scope (
  policy_id TEXT NOT NULL REFERENCES fleet_policies(id) ON DELETE CASCADE,
  instance_id TEXT NOT NULL REFERENCES instances(id),
  PRIMARY KEY(policy_id, instance_id)
);
CREATE TABLE IF NOT EXISTS policy_rollout_targets (
  rollout_id TEXT NOT NULL REFERENCES operations(id) ON DELETE CASCADE,
  policy_id TEXT NOT NULL,
  instance_id TEXT NOT NULL REFERENCES instances(id),
  child_operation_id TEXT REFERENCES operations(id),
  status TEXT NOT NULL CHECK(status IN ('PENDING', 'RUNNING', 'SUCCEEDED', 'FAILED', 'BLOCKED')),
  detail TEXT NOT NULL DEFAULT '',
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL,
  PRIMARY KEY(rollout_id, instance_id)
);
CREATE INDEX IF NOT EXISTS idx_policy_rollout_targets_status
  ON policy_rollout_targets(rollout_id, status, created_at);
CREATE TABLE IF NOT EXISTS campaign_targets (
  campaign_id TEXT NOT NULL REFERENCES operations(id) ON DELETE CASCADE,
  instance_id TEXT NOT NULL REFERENCES instances(id),
  request_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK(status IN ('PENDING', 'RUNNING', 'SUCCEEDED', 'FAILED', 'BLOCKED')),
  detail TEXT NOT NULL DEFAULT '',
  requested_at DATETIME,
  completed_at DATETIME,
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL,
  PRIMARY KEY(campaign_id, instance_id)
);
CREATE INDEX IF NOT EXISTS idx_campaign_targets_status
  ON campaign_targets(campaign_id, status, created_at);
CREATE INDEX IF NOT EXISTS idx_campaign_targets_instance
  ON campaign_targets(instance_id, status, updated_at DESC);
CREATE TABLE IF NOT EXISTS chat_sessions (
  id TEXT PRIMARY KEY,
  instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
  title TEXT NOT NULL,
	model TEXT NOT NULL DEFAULT '',
	reasoning TEXT NOT NULL DEFAULT '',
	service_tier TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK(status IN ('ACTIVE')),
  last_error TEXT NOT NULL DEFAULT '',
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chat_sessions_updated
  ON chat_sessions(updated_at DESC, id DESC);
CREATE TABLE IF NOT EXISTS chat_messages (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
  operation_id TEXT REFERENCES operations(id),
  role TEXT NOT NULL CHECK(role IN ('user', 'assistant')),
  ciphertext TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('PENDING', 'SUCCEEDED', 'FAILED')),
  error TEXT NOT NULL DEFAULT '',
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_messages_operation
  ON chat_messages(operation_id) WHERE operation_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_chat_messages_session
  ON chat_messages(session_id, created_at, id);
CREATE TABLE IF NOT EXISTS chat_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
  operation_id TEXT NOT NULL REFERENCES operations(id) ON DELETE CASCADE,
  sequence INTEGER NOT NULL CHECK(sequence >= 0),
  type TEXT NOT NULL CHECK(type IN ('RUN_QUEUED', 'RUN_STARTED', 'ASSISTANT_DELTA', 'ASSISTANT_ACTIVITY', 'ASSISTANT_ARTIFACT', 'RUN_COMPLETED', 'RUN_FAILED', 'RUN_CANCELED')),
  ciphertext TEXT NOT NULL DEFAULT '',
  content_hash TEXT NOT NULL DEFAULT '',
  created_at DATETIME NOT NULL,
  UNIQUE(operation_id, sequence)
);
CREATE INDEX IF NOT EXISTS idx_chat_events_session_cursor
  ON chat_events(session_id, id);
CREATE TABLE IF NOT EXISTS hermes_profile_inventories (
  instance_id TEXT PRIMARY KEY REFERENCES instances(id) ON DELETE CASCADE,
  profiles BLOB NOT NULL DEFAULT '[]',
  observed_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_fleet_health_incidents_occurred
  ON fleet_health_incidents(occurred_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_instance_observations_host ON instance_observations(host_id, received_at DESC);
CREATE INDEX IF NOT EXISTS idx_observation_requests_host ON observation_requests(host_id, requested_at);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate sqlite: %w", err)
	}
	if err := s.ensureColumn("jobs", "lease_token", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("jobs", "completion_hash", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("jobs", "progress", "BLOB NOT NULL DEFAULT '{}'"); err != nil {
		return err
	}
	if err := s.ensureColumn("instance_observations", "hermes_version", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("instance_observations", "hermes_source", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("instance_observations", "model_catalog", "BLOB NOT NULL DEFAULT '[]'"); err != nil {
		return err
	}
	if err := s.ensureColumn("instance_observations", "recommended_model", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("instance_observations", "provider_model_catalogs", "BLOB NOT NULL DEFAULT '{}'"); err != nil {
		return err
	}
	if err := s.migrateCodexConfigurationState(); err != nil {
		return err
	}
	if err := s.ensureColumn("operations", "workflow_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("operations", "actor", "TEXT NOT NULL DEFAULT 'FLEET_ADMIN'"); err != nil {
		return err
	}
	if err := s.ensureColumn("operations", "metadata", "BLOB NOT NULL DEFAULT '{}'"); err != nil {
		return err
	}
	if err := s.ensureColumn("operations", "progress", "BLOB NOT NULL DEFAULT '{}'"); err != nil {
		return err
	}
	if err := s.migrateControlPlaneOperations(); err != nil {
		return err
	}
	if err := s.migrateChatEventPayloads(); err != nil {
		return err
	}
	if err := s.ensureColumn("chat_sessions", "model", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("chat_sessions", "reasoning", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("chat_sessions", "service_tier", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if _, err := s.db.Exec(`
UPDATE chat_sessions
SET model=(SELECT model FROM instances WHERE instances.id=chat_sessions.instance_id),
    reasoning=(SELECT reasoning FROM instances WHERE instances.id=chat_sessions.instance_id),
    service_tier=(SELECT service_tier FROM instances WHERE instances.id=chat_sessions.instance_id)
WHERE model='' AND reasoning='' AND service_tier=''`); err != nil {
		return fmt.Errorf("backfill chat session configuration: %w", err)
	}
	if err := s.ensureColumn("instances", "public_hostname", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.migrateLegacyInstanceConstraints(); err != nil {
		return err
	}
	if err := s.migrateRemovedProviderProfiles(); err != nil {
		return err
	}
	return s.createAllocationConstraints()
}

func (s *Store) migrateChatEventPayloads() (returnErr error) {
	ctx := context.Background()
	var applied bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=?)`, chatEventPayloadMigration).Scan(&applied); err != nil {
		return fmt.Errorf("inspect chat event payload migration: %w", err)
	}
	if applied {
		return nil
	}
	var ddl string
	if err := s.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='chat_events'`).Scan(&ddl); err != nil {
		return fmt.Errorf("inspect chat events schema: %w", err)
	}
	normalized := strings.ToUpper(ddl)
	if strings.Contains(normalized, "ASSISTANT_ACTIVITY") && strings.Contains(normalized, "ASSISTANT_ARTIFACT") {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`, chatEventPayloadMigration, time.Now().UTC()); err != nil {
			return fmt.Errorf("record chat event payload migration: %w", err)
		}
		return nil
	}

	connection, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve chat event migration connection: %w", err)
	}
	defer connection.Close()
	if _, err := connection.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for chat event migration: %w", err)
	}
	defer func() {
		if _, err := connection.ExecContext(ctx, `PRAGMA foreign_keys=ON`); returnErr == nil && err != nil {
			returnErr = fmt.Errorf("restore foreign keys after chat event migration: %w", err)
		}
	}()
	tx, err := connection.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin chat event payload migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
DROP TABLE IF EXISTS chat_events_v2;
CREATE TABLE chat_events_v2 (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
  operation_id TEXT NOT NULL REFERENCES operations(id) ON DELETE CASCADE,
  sequence INTEGER NOT NULL CHECK(sequence >= 0),
  type TEXT NOT NULL CHECK(type IN ('RUN_QUEUED', 'RUN_STARTED', 'ASSISTANT_DELTA', 'ASSISTANT_ACTIVITY', 'ASSISTANT_ARTIFACT', 'RUN_COMPLETED', 'RUN_FAILED', 'RUN_CANCELED')),
  ciphertext TEXT NOT NULL DEFAULT '',
  content_hash TEXT NOT NULL DEFAULT '',
  created_at DATETIME NOT NULL,
  UNIQUE(operation_id, sequence)
);
INSERT INTO chat_events_v2
  (id, session_id, operation_id, sequence, type, ciphertext, content_hash, created_at)
SELECT id, session_id, operation_id, sequence, type, ciphertext, content_hash, created_at
FROM chat_events;
DROP TABLE chat_events;
ALTER TABLE chat_events_v2 RENAME TO chat_events;
CREATE INDEX idx_chat_events_session_cursor ON chat_events(session_id, id);
INSERT INTO schema_migrations (name, applied_at) VALUES ('20260812_chat_event_payloads_v1', CURRENT_TIMESTAMP);
`); err != nil {
		return fmt.Errorf("migrate chat event payload schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit chat event payload migration: %w", err)
	}
	var violationTable string
	if err := connection.QueryRowContext(ctx, `SELECT "table" FROM pragma_foreign_key_check LIMIT 1`).Scan(&violationTable); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("verify chat event payload migration foreign keys: %w", err)
	} else if err == nil {
		return fmt.Errorf("chat event payload migration left a foreign key violation in %s", violationTable)
	}
	return nil
}

func (s *Store) migrateControlPlaneOperations() (returnErr error) {
	ctx := context.Background()
	var applied bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=?)`, controlPlaneOperationMigration).Scan(&applied); err != nil {
		return fmt.Errorf("inspect control-plane operation migration: %w", err)
	}
	if applied {
		return nil
	}
	var notNull int
	if err := s.db.QueryRowContext(ctx, `SELECT "notnull" FROM pragma_table_info('operations') WHERE name='instance_id'`).Scan(&notNull); err != nil {
		return fmt.Errorf("inspect operations.instance_id: %w", err)
	}
	if notNull == 0 {
		_, err := s.db.ExecContext(ctx, `INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`, controlPlaneOperationMigration, time.Now().UTC())
		return err
	}
	connection, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve operation migration connection: %w", err)
	}
	defer connection.Close()
	if _, err := connection.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for operation migration: %w", err)
	}
	defer func() {
		if _, err := connection.ExecContext(ctx, `PRAGMA foreign_keys=ON`); returnErr == nil && err != nil {
			returnErr = fmt.Errorf("restore foreign keys after operation migration: %w", err)
		}
	}()
	tx, err := connection.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
CREATE TABLE operations_v2 (
  id TEXT PRIMARY KEY,
  instance_id TEXT REFERENCES instances(id),
  workflow_id TEXT NOT NULL DEFAULT '',
  actor TEXT NOT NULL DEFAULT 'FLEET_ADMIN',
  type TEXT NOT NULL,
  status TEXT NOT NULL,
  summary TEXT NOT NULL,
  metadata BLOB NOT NULL DEFAULT '{}',
  progress BLOB NOT NULL DEFAULT '{}',
  error TEXT NOT NULL DEFAULT '',
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);
INSERT INTO operations_v2
  (id, instance_id, workflow_id, actor, type, status, summary, metadata, progress, error, created_at, updated_at)
SELECT id, instance_id, workflow_id, actor, type, status, summary, metadata, progress, error, created_at, updated_at
FROM operations;
DROP TABLE operations;
ALTER TABLE operations_v2 RENAME TO operations;
CREATE INDEX idx_operations_created ON operations(created_at DESC);
CREATE INDEX idx_operations_cursor ON operations(created_at DESC, id DESC);
INSERT INTO schema_migrations (name, applied_at) VALUES ('20260808_control_plane_operations_v1', CURRENT_TIMESTAMP);
`); err != nil {
		return fmt.Errorf("migrate control-plane operations: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit control-plane operation migration: %w", err)
	}
	return nil
}

func (s *Store) migrateCodexConfigurationState() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var applied bool
	if err := tx.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=?)`,
		codexConfigurationStateMigration,
	).Scan(&applied); err != nil {
		return fmt.Errorf("inspect Codex configuration state migration: %w", err)
	}
	if applied {
		return nil
	}
	var columnExists bool
	if err := tx.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM pragma_table_info('instances') WHERE name='codex_configured')`,
	).Scan(&columnExists); err != nil {
		return fmt.Errorf("inspect instances.codex_configured: %w", err)
	}
	if !columnExists {
		if _, err := tx.Exec(`ALTER TABLE instances ADD COLUMN codex_configured INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add instances.codex_configured: %w", err)
		}
	}
	// A database that already has the column was opened by a release that ran
	// the legacy backfill. Re-running it would overwrite an explicit false state
	// restored from an older instance backup.
	if !columnExists {
		if _, err := tx.Exec(`
UPDATE instances
SET codex_configured=1
WHERE codex_configured=0
  AND EXISTS (
    SELECT 1 FROM operations
    WHERE operations.instance_id=instances.id
      AND operations.type='CONFIGURE_CODEX'
      AND operations.status='SUCCEEDED'
  )`); err != nil {
			return fmt.Errorf("backfill explicit Codex configuration state: %w", err)
		}
		if _, err := tx.Exec(`
UPDATE instances
SET model='', reasoning='', service_tier=''
WHERE provider='openai-codex' AND codex_configured=0
  AND (model<>'' OR reasoning<>'' OR service_tier<>'')`); err != nil {
			return fmt.Errorf("remove implicit legacy Codex defaults: %w", err)
		}
		if _, err := tx.Exec(`
UPDATE instances
SET last_error=''
WHERE provider='openai-codex' AND codex_configured=0
  AND last_error='runtime synchronization refused: model contains unsupported characters or length'`); err != nil {
			return fmt.Errorf("clear invalid legacy Codex auto-sync error: %w", err)
		}
		if _, err := tx.Exec(`
DELETE FROM runtime_reconcile_state
WHERE instance_id IN (
  SELECT id FROM instances
  WHERE provider='openai-codex' AND codex_configured=0
)`); err != nil {
			return fmt.Errorf("clear pending Codex auto-sync state: %w", err)
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
		codexConfigurationStateMigration, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("record Codex configuration state migration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Codex configuration state migration: %w", err)
	}
	return nil
}

func (s *Store) migrateLegacyInstanceConstraints() (returnErr error) {
	var ddl string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='instances'`).Scan(&ddl); err != nil {
		return fmt.Errorf("inspect instances schema: %w", err)
	}
	normalized := strings.ToLower(strings.Join(strings.Fields(ddl), ""))
	if !strings.Contains(normalized, "nametextnotnullunique") &&
		!strings.Contains(normalized, "unique(host_id,api_port)") &&
		!strings.Contains(normalized, "unique(host_id,dashboard_port)") {
		return nil
	}

	if _, err := s.db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for instances migration: %w", err)
	}
	defer func() {
		if _, err := s.db.Exec(`PRAGMA foreign_keys=ON`); returnErr == nil && err != nil {
			returnErr = fmt.Errorf("restore foreign keys after instances migration: %w", err)
		}
	}()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
DROP TABLE IF EXISTS instances_v2;
CREATE TABLE instances_v2 (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  host_id TEXT NOT NULL REFERENCES hosts(id),
  status TEXT NOT NULL,
  image TEXT NOT NULL,
  image_id TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL,
  model TEXT NOT NULL,
  reasoning TEXT NOT NULL,
  service_tier TEXT NOT NULL,
  codex_configured INTEGER NOT NULL DEFAULT 0,
  api_port INTEGER NOT NULL,
  dashboard_port INTEGER NOT NULL,
  public_hostname TEXT NOT NULL DEFAULT '',
  project_name TEXT NOT NULL DEFAULT '',
  data_volume TEXT NOT NULL DEFAULT '',
  managed_path TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);
INSERT INTO instances_v2 (
  id, name, host_id, status, image, image_id, provider, model, reasoning, service_tier, codex_configured,
  api_port, dashboard_port, public_hostname, project_name, data_volume, managed_path,
  last_error, created_at, updated_at
)
SELECT id, name, host_id, status, image, image_id, provider, model, reasoning, service_tier, codex_configured,
  api_port, dashboard_port, public_hostname, project_name, data_volume, managed_path,
  last_error, created_at, updated_at
FROM instances;
DROP TABLE instances;
ALTER TABLE instances_v2 RENAME TO instances;
`); err != nil {
		return fmt.Errorf("rebuild instances table: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit instances migration: %w", err)
	}
	return nil
}

func (s *Store) migrateRemovedProviderProfiles() error {
	var activeBindings int
	if err := s.db.QueryRow(`
SELECT COUNT(*) FROM jobs
WHERE type='instance.provider.bind' AND status IN (?, ?, ?)`,
		domain.JobPending, domain.JobLeased, domain.JobRunning).Scan(&activeBindings); err != nil {
		return fmt.Errorf("inspect legacy provider binding jobs: %w", err)
	}
	if activeBindings != 0 {
		return errors.New("cannot remove provider profiles while a provider binding job is active")
	}

	instanceColumn, err := s.hasColumn("instances", "provider_profile_id")
	if err != nil {
		return err
	}
	jobColumn, err := s.hasColumn("jobs", "provider_profile_id")
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DROP INDEX IF EXISTS idx_jobs_provider_profile`); err != nil {
		return fmt.Errorf("drop legacy provider profile index: %w", err)
	}
	if instanceColumn {
		if _, err := tx.Exec(`ALTER TABLE instances DROP COLUMN provider_profile_id`); err != nil {
			return fmt.Errorf("drop instances provider profile column: %w", err)
		}
	}
	if jobColumn {
		if _, err := tx.Exec(`ALTER TABLE jobs DROP COLUMN provider_profile_id`); err != nil {
			return fmt.Errorf("drop jobs provider profile column: %w", err)
		}
	}
	if _, err := tx.Exec(`DROP TABLE IF EXISTS provider_profiles`); err != nil {
		return fmt.Errorf("drop legacy provider profiles table: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit provider profile removal: %w", err)
	}
	return nil
}

func (s *Store) createAllocationConstraints() error {
	const constraints = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_instances_active_name
  ON instances(name) WHERE status <> 'DELETED';
CREATE UNIQUE INDEX IF NOT EXISTS idx_instances_active_api_port
  ON instances(host_id, api_port) WHERE status <> 'DELETED';
CREATE UNIQUE INDEX IF NOT EXISTS idx_instances_active_dashboard_port
  ON instances(host_id, dashboard_port) WHERE status <> 'DELETED';
CREATE UNIQUE INDEX IF NOT EXISTS idx_instances_active_public_hostname
  ON instances(public_hostname) WHERE status <> 'DELETED' AND public_hostname <> '';
CREATE TRIGGER IF NOT EXISTS trg_instances_ports_insert
BEFORE INSERT ON instances
WHEN NEW.status <> 'DELETED'
BEGIN
  SELECT CASE WHEN EXISTS (
    SELECT 1 FROM instances current
    WHERE current.host_id = NEW.host_id
      AND current.status <> 'DELETED'
      AND (
        current.api_port = NEW.api_port OR current.dashboard_port = NEW.api_port OR
        current.api_port = NEW.dashboard_port OR current.dashboard_port = NEW.dashboard_port
      )
  ) THEN RAISE(ABORT, 'host port is already allocated') END;
END;
CREATE TRIGGER IF NOT EXISTS trg_instances_ports_update
BEFORE UPDATE OF host_id, api_port, dashboard_port, status ON instances
WHEN NEW.status <> 'DELETED'
BEGIN
  SELECT CASE WHEN EXISTS (
    SELECT 1 FROM instances current
    WHERE current.id <> NEW.id
      AND current.host_id = NEW.host_id
      AND current.status <> 'DELETED'
      AND (
        current.api_port = NEW.api_port OR current.dashboard_port = NEW.api_port OR
        current.api_port = NEW.dashboard_port OR current.dashboard_port = NEW.dashboard_port
      )
  ) THEN RAISE(ABORT, 'host port is already allocated') END;
END;
`
	if _, err := s.db.Exec(constraints); err != nil {
		return fmt.Errorf("create instance allocation constraints: %w", err)
	}
	return nil
}

func (s *Store) ensureColumn(table, column, definition string) error {
	exists, err := s.hasColumn(table, column)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

func (s *Store) hasColumn(table, column string) (bool, error) {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func (s *Store) EnrollHost(ctx context.Context, host domain.Host, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO hosts (id, name, hostname, os, arch, agent_version, token_hash, last_seen_at, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		host.ID, host.Name, host.Hostname, host.OS, host.Arch, host.AgentVersion, tokenHash, host.LastSeenAt, host.CreatedAt)
	if err != nil {
		return fmt.Errorf("enroll host: %w", err)
	}
	return nil
}

func (s *Store) RotateHostCredential(
	ctx context.Context,
	hostID, confirmName, hostname, osName, arch, tokenHash string,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var storedName, storedHostname, storedOS, storedArch string
	err = tx.QueryRowContext(ctx, `
SELECT name, hostname, os, arch
FROM hosts
WHERE id=?`,
		hostID,
	).Scan(&storedName, &storedHostname, &storedOS, &storedArch)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if confirmName != storedName || hostname != storedHostname || osName != storedOS || arch != storedArch {
		return ErrHostIdentityMismatch
	}

	var activeJobs int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM jobs
WHERE host_id=? AND status IN (?, ?, ?)`,
		hostID, domain.JobPending, domain.JobLeased, domain.JobRunning,
	).Scan(&activeJobs); err != nil {
		return err
	}
	if activeJobs != 0 {
		return ErrHostBusy
	}

	result, err := tx.ExecContext(ctx, `
UPDATE hosts
SET token_hash=?
WHERE id=? AND name=? AND hostname=? AND os=? AND arch=?
  AND NOT EXISTS (
    SELECT 1
    FROM jobs
    WHERE host_id=? AND status IN (?, ?, ?)
  )`,
		tokenHash, hostID, confirmName, hostname, osName, arch,
		hostID, domain.JobPending, domain.JobLeased, domain.JobRunning,
	)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read control-plane operation update count: %w", err)
	}
	if count != 1 {
		return ErrStateChanged
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (s *Store) HostTokenHash(ctx context.Context, hostID string) (string, error) {
	var hash string
	if err := s.db.QueryRowContext(ctx, `SELECT token_hash FROM hosts WHERE id=?`, hostID).Scan(&hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	return hash, nil
}

func (s *Store) Heartbeat(ctx context.Context, hostID, hostname, osName, arch, version string, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE hosts SET hostname=?, os=?, arch=?, agent_version=?, last_seen_at=? WHERE id=?`,
		hostname, osName, arch, version, at, hostID)
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListHosts(ctx context.Context, offlineAfter time.Duration) ([]domain.Host, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, hostname, os, arch, agent_version, last_seen_at, created_at
FROM hosts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hosts []domain.Host
	for rows.Next() {
		var h domain.Host
		if err := rows.Scan(&h.ID, &h.Name, &h.Hostname, &h.OS, &h.Arch, &h.AgentVersion, &h.LastSeenAt, &h.CreatedAt); err != nil {
			return nil, err
		}
		if time.Since(h.LastSeenAt) > offlineAfter {
			h.Status = domain.HostOffline
		} else {
			h.Status = domain.HostOnline
		}
		hosts = append(hosts, h)
	}
	return hosts, rows.Err()
}

func (s *Store) GetHost(ctx context.Context, id string) (domain.Host, error) {
	var h domain.Host
	err := s.db.QueryRowContext(ctx, `
SELECT id, name, hostname, os, arch, agent_version, last_seen_at, created_at FROM hosts WHERE id=?`, id).
		Scan(&h.ID, &h.Name, &h.Hostname, &h.OS, &h.Arch, &h.AgentVersion, &h.LastSeenAt, &h.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	return h, err
}

func (s *Store) CreateInstance(ctx context.Context, instance domain.Instance, operation domain.Operation, job domain.Job) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
INSERT INTO instances (id, name, host_id, status, image, provider, model, reasoning, service_tier, codex_configured, api_port, dashboard_port, public_hostname, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		instance.ID, instance.Name, instance.HostID, instance.Status, instance.Image, instance.Provider, instance.Model,
		instance.Reasoning, instance.ServiceTier, instance.CodexConfigured, instance.APIPort, instance.DashboardPort, instance.PublicHostname, instance.CreatedAt, instance.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert instance: %w", err)
	}
	if err := insertOperationAndJob(ctx, tx, operation, job); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) NextAvailablePorts(ctx context.Context, hostID string) (int, int, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT api_port, dashboard_port FROM instances WHERE host_id=? AND status <> ?`, hostID, domain.InstanceDeleted)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	used := make(map[int]bool)
	for rows.Next() {
		var apiPort, dashboardPort int
		if err := rows.Scan(&apiPort, &dashboardPort); err != nil {
			return 0, 0, err
		}
		used[apiPort], used[dashboardPort] = true, true
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	apiPort, dashboardPort := 8650, 9130
	for used[apiPort] && apiPort <= 65535 {
		apiPort++
	}
	for (used[dashboardPort] || dashboardPort == apiPort) && dashboardPort <= 65535 {
		dashboardPort++
	}
	if apiPort > 65535 || dashboardPort > 65535 {
		return 0, 0, errors.New("no unallocated Fleet ports remain on the selected host")
	}
	return apiPort, dashboardPort, nil
}

func (s *Store) QueueAction(ctx context.Context, expectedStatus, status string, operation domain.Operation, job domain.Job) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
UPDATE instances SET status=?, last_error='', updated_at=? WHERE id=? AND status=?`,
		status, operation.CreatedAt, operation.InstanceID, expectedStatus)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateChanged
	}
	if status != domain.InstanceRestarting {
		if _, err := tx.ExecContext(ctx, `DELETE FROM runtime_remediation_state WHERE instance_id=?`, operation.InstanceID); err != nil {
			return err
		}
	}
	busy, err := activeInstanceJobs(ctx, tx, operation.InstanceID)
	if err != nil {
		return err
	}
	if busy != 0 {
		return ErrInstanceBusy
	}
	if err := insertOperationAndJob(ctx, tx, operation, job); err != nil {
		return err
	}
	return tx.Commit()
}

// HasActiveRecoveryPointReference checks durable job state without relying on
// SQLite JSON extensions so deletion remains fenced after a control-plane restart.
func (s *Store) HasActiveRecoveryPointReference(ctx context.Context, recoveryPointID string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT type, payload
FROM jobs
WHERE status IN (?, ?, ?)
  AND type IN ('instance.recovery.restore', 'instance.hermes.update', 'instance.hermes.upgrade')`,
		domain.JobPending, domain.JobLeased, domain.JobRunning)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var jobType string
		var payload []byte
		if err := rows.Scan(&jobType, &payload); err != nil {
			return false, err
		}
		referenced, err := recoveryPointReferencedByJob(jobType, payload, recoveryPointID)
		if err != nil {
			return false, fmt.Errorf("decode active %s job payload: %w", jobType, err)
		}
		if referenced {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func recoveryPointReferencedByJob(jobType string, payload []byte, recoveryPointID string) (bool, error) {
	switch jobType {
	case "instance.recovery.restore":
		var restore domain.RecoveryRestorePayload
		if err := json.Unmarshal(payload, &restore); err != nil {
			return false, err
		}
		return restore.RecoveryPointID == recoveryPointID, nil
	case "instance.hermes.upgrade":
		var upgrade domain.HermesUpgradePayload
		if err := json.Unmarshal(payload, &upgrade); err != nil {
			return false, err
		}
		return upgrade.RecoveryPointID == recoveryPointID ||
			upgrade.Rollback.RecoveryPointID == recoveryPointID, nil
	case "instance.hermes.update":
		var update domain.HermesUpdatePayload
		if err := json.Unmarshal(payload, &update); err != nil {
			return false, err
		}
		return update.Backup.RecoveryPointID == recoveryPointID ||
			update.Upgrade.RecoveryPointID == recoveryPointID ||
			update.Upgrade.Rollback.RecoveryPointID == recoveryPointID, nil
	default:
		return false, nil
	}
}

type MessagingConfigRecord struct {
	InstanceID      string
	Ciphertext      string
	DesiredRevision string
	AppliedRevision string
	Status          string
	LastError       string
	UpdatedAt       time.Time
	AppliedAt       *time.Time
}

type MCPConfigRecord struct {
	InstanceID      string
	Ciphertext      string
	DesiredRevision string
	AppliedRevision string
	Status          string
	LastError       string
	UpdatedAt       time.Time
	AppliedAt       *time.Time
}

type RemoteAccessConfigRecord struct {
	Ciphertext string
	UpdatedAt  time.Time
}

func (s *Store) GetRemoteAccessConfig(ctx context.Context) (RemoteAccessConfigRecord, error) {
	var record RemoteAccessConfigRecord
	err := s.db.QueryRowContext(ctx, `
SELECT ciphertext, updated_at FROM system_remote_access_config WHERE id=1`).
		Scan(&record.Ciphertext, &record.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return record, ErrNotFound
	}
	return record, err
}

func (s *Store) PutRemoteAccessConfig(ctx context.Context, record RemoteAccessConfigRecord) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO system_remote_access_config (id, ciphertext, updated_at)
VALUES (1, ?, ?)
ON CONFLICT(id) DO UPDATE SET ciphertext=excluded.ciphertext, updated_at=excluded.updated_at`,
		record.Ciphertext, record.UpdatedAt)
	return err
}

func (s *Store) DeleteRemoteAccessConfig(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM system_remote_access_config WHERE id=1`)
	return err
}

func (s *Store) GetMessagingConfig(ctx context.Context, instanceID string) (MessagingConfigRecord, error) {
	var record MessagingConfigRecord
	err := s.db.QueryRowContext(ctx, `
SELECT instance_id, ciphertext, desired_revision, applied_revision, status, last_error, updated_at, applied_at
FROM instance_messaging_configs WHERE instance_id=?`, instanceID).
		Scan(&record.InstanceID, &record.Ciphertext, &record.DesiredRevision, &record.AppliedRevision,
			&record.Status, &record.LastError, &record.UpdatedAt, &record.AppliedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return record, ErrNotFound
	}
	return record, err
}

func (s *Store) QueueMessagingConfiguration(
	ctx context.Context,
	expectedStatus string,
	record MessagingConfigRecord,
	operation domain.Operation,
	job domain.Job,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
UPDATE instances SET status=?, last_error='', updated_at=? WHERE id=? AND status=?`,
		domain.InstanceUpdating, operation.CreatedAt, operation.InstanceID, expectedStatus)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateChanged
	}
	busy, err := activeInstanceJobs(ctx, tx, operation.InstanceID)
	if err != nil {
		return err
	}
	if busy != 0 {
		return ErrInstanceBusy
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO instance_messaging_configs
  (instance_id, ciphertext, desired_revision, applied_revision, status, last_error, updated_at, applied_at)
VALUES (?, ?, ?, '', 'PENDING', '', ?, NULL)
ON CONFLICT(instance_id) DO UPDATE SET
  ciphertext=excluded.ciphertext,
  desired_revision=excluded.desired_revision,
  status='PENDING',
  last_error='',
  updated_at=excluded.updated_at`,
		record.InstanceID, record.Ciphertext, record.DesiredRevision, record.UpdatedAt); err != nil {
		return err
	}
	if err := insertOperationAndJob(ctx, tx, operation, job); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetMCPConfig(ctx context.Context, instanceID string) (MCPConfigRecord, error) {
	var record MCPConfigRecord
	err := s.db.QueryRowContext(ctx, `
SELECT instance_id, ciphertext, desired_revision, applied_revision, status, last_error, updated_at, applied_at
FROM instance_mcp_configs WHERE instance_id=?`, instanceID).
		Scan(&record.InstanceID, &record.Ciphertext, &record.DesiredRevision, &record.AppliedRevision,
			&record.Status, &record.LastError, &record.UpdatedAt, &record.AppliedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return record, ErrNotFound
	}
	return record, err
}

func (s *Store) QueueMCPConfiguration(
	ctx context.Context,
	expectedStatus string,
	record MCPConfigRecord,
	operation domain.Operation,
	job domain.Job,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
UPDATE instances SET status=?, last_error='', updated_at=? WHERE id=? AND status=?`,
		domain.InstanceUpdating, operation.CreatedAt, operation.InstanceID, expectedStatus)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateChanged
	}
	busy, err := activeInstanceJobs(ctx, tx, operation.InstanceID)
	if err != nil {
		return err
	}
	if busy != 0 {
		return ErrInstanceBusy
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO instance_mcp_configs
  (instance_id, ciphertext, desired_revision, applied_revision, status, last_error, updated_at, applied_at)
VALUES (?, ?, ?, '', 'PENDING', '', ?, NULL)
ON CONFLICT(instance_id) DO UPDATE SET
  ciphertext=excluded.ciphertext,
  desired_revision=excluded.desired_revision,
  status='PENDING',
  last_error='',
  updated_at=excluded.updated_at`,
		record.InstanceID, record.Ciphertext, record.DesiredRevision, record.UpdatedAt); err != nil {
		return err
	}
	if err := insertOperationAndJob(ctx, tx, operation, job); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) QueueRuntimeRepair(
	ctx context.Context,
	expectedStatus string,
	operation domain.Operation,
	job domain.Job,
	remediation domain.RuntimeRemediation,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
UPDATE instances SET status=?, last_error='', updated_at=? WHERE id=? AND status=?`,
		domain.InstanceRestarting, operation.CreatedAt, operation.InstanceID, expectedStatus)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateChanged
	}
	busy, err := activeInstanceJobs(ctx, tx, operation.InstanceID)
	if err != nil {
		return err
	}
	if busy != 0 {
		return ErrInstanceBusy
	}
	result, err = tx.ExecContext(ctx, `
UPDATE runtime_remediation_state
SET status='QUEUED', phase=?, attempt_in_phase=?, total_attempts=?,
    last_attempt_at=?, next_attempt_at=?, last_error='', updated_at=?
WHERE instance_id=? AND workflow_id=? AND status='READY'`,
		remediation.Phase, remediation.AttemptInPhase, remediation.TotalAttempts,
		remediation.LastAttemptAt, remediation.NextAttemptAt, operation.CreatedAt,
		operation.InstanceID, remediation.WorkflowID,
	)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateChanged
	}
	if err := insertOperationAndJob(ctx, tx, operation, job); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) QueueInspection(ctx context.Context, operation domain.Operation, job domain.Job) error {
	return s.queueInspection(ctx, operation, job, "")
}

func (s *Store) QueueRunningInspection(ctx context.Context, operation domain.Operation, job domain.Job) error {
	return s.queueInspection(ctx, operation, job, domain.InstanceRunning)
}

func (s *Store) queueInspection(ctx context.Context, operation domain.Operation, job domain.Job, requiredStatus string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if requiredStatus != "" {
		var instanceStatus string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM instances WHERE id=?`, job.InstanceID).Scan(&instanceStatus); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if instanceStatus != requiredStatus {
			return ErrStateChanged
		}
	}
	active, err := activeInstanceJobs(ctx, tx, job.InstanceID)
	if err != nil {
		return err
	}
	if active != 0 {
		return ErrInstanceBusy
	}
	if err := insertOperationAndJob(ctx, tx, operation, job); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) QueueCodexAuth(ctx context.Context, operation domain.Operation, job domain.Job) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var instanceStatus string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM instances WHERE id=?`, job.InstanceID).Scan(&instanceStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if instanceStatus != domain.InstanceRunning {
		return ErrStateChanged
	}
	active, err := activeInstanceJobs(ctx, tx, job.InstanceID)
	if err != nil {
		return err
	}
	if active != 0 {
		return ErrInstanceBusy
	}
	if err := insertOperationAndJob(ctx, tx, operation, job); err != nil {
		return err
	}
	return tx.Commit()
}

func activeInstanceJobs(ctx context.Context, tx *sql.Tx, instanceID string) (int, error) {
	var active int
	err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM jobs
WHERE instance_id=? AND status IN (?, ?, ?)`,
		instanceID, domain.JobPending, domain.JobLeased, domain.JobRunning).Scan(&active)
	return active, err
}

func insertOperationAndJob(ctx context.Context, tx *sql.Tx, operation domain.Operation, job domain.Job) error {
	var active int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM jobs
WHERE host_id=? AND status IN (?, ?, ?)`,
		job.HostID, domain.JobPending, domain.JobLeased, domain.JobRunning,
	).Scan(&active); err != nil {
		return fmt.Errorf("inspect host queue capacity: %w", err)
	}
	if active >= JobQueueMaxPerHost {
		return ErrQueueCapacity
	}
	actor := operation.Actor
	if actor == "" {
		actor = "FLEET_ADMIN"
	}
	metadata := operation.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO operations (id, instance_id, workflow_id, actor, type, status, summary, metadata, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, operation.ID, operation.InstanceID, operation.WorkflowID, actor,
		operation.Type, operation.Status, operation.Summary, []byte(metadata), operation.CreatedAt, operation.UpdatedAt); err != nil {
		return fmt.Errorf("insert operation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO jobs (id, operation_id, host_id, instance_id, type, status, payload, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, job.ID, job.OperationID, job.HostID, job.InstanceID,
		job.Type, job.Status, []byte(job.Payload), job.CreatedAt, job.UpdatedAt); err != nil {
		return fmt.Errorf("insert job: %w", err)
	}
	return nil
}

func (s *Store) ListInstances(ctx context.Context) ([]domain.Instance, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT i.id, i.name, i.host_id, h.name, i.status, i.image, i.image_id, i.provider, i.model, i.reasoning,
       i.service_tier, i.codex_configured, i.api_port, i.dashboard_port, i.public_hostname, i.project_name, i.data_volume, i.managed_path,
       i.last_error, i.created_at, i.updated_at,
	       o.host_id, o.target_generation, o.hermes_version, o.hermes_source, o.model_catalog, o.recommended_model,
	       o.provider_model_catalogs, o.status, o.summary, o.checks, o.observed_at, o.received_at,
       r.request_id, r.requested_at
FROM instances i
JOIN hosts h ON h.id=i.host_id
LEFT JOIN instance_observations o ON o.instance_id=i.id
LEFT JOIN observation_requests r ON r.instance_id=i.id
WHERE i.status <> 'DELETED'
ORDER BY i.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []domain.Instance
	for rows.Next() {
		var i domain.Instance
		var observationHostID, targetGeneration, hermesVersion, hermesSource, recommendedModel, observationStatus, observationSummary sql.NullString
		var observationChecks, modelCatalog, providerCatalogs []byte
		var observedAt, receivedAt sql.NullTime
		var requestID sql.NullString
		var requestedAt sql.NullTime
		if err := rows.Scan(&i.ID, &i.Name, &i.HostID, &i.HostName, &i.Status, &i.Image, &i.ImageID, &i.Provider,
			&i.Model, &i.Reasoning, &i.ServiceTier, &i.CodexConfigured, &i.APIPort, &i.DashboardPort, &i.PublicHostname, &i.ProjectName, &i.DataVolume,
			&i.ManagedPath, &i.LastError, &i.CreatedAt, &i.UpdatedAt,
			&observationHostID, &targetGeneration, &hermesVersion, &hermesSource, &modelCatalog, &recommendedModel, &providerCatalogs,
			&observationStatus, &observationSummary, &observationChecks, &observedAt, &receivedAt,
			&requestID, &requestedAt); err != nil {
			return nil, err
		}
		if observationStatus.Valid {
			observation, err := decodeStoredObservation(
				i.ID, observationHostID.String, targetGeneration.String, hermesVersion.String, hermesSource.String,
				recommendedModel.String, observationStatus.String, observationSummary.String,
				observationChecks, modelCatalog, providerCatalogs, observedAt.Time, receivedAt.Time,
			)
			if err != nil {
				return nil, err
			}
			i.Observation = observation
		}
		if requestID.Valid {
			i.ObservationRequest = &domain.ObservationRequest{ID: requestID.String, InstanceID: i.ID, RequestedAt: requestedAt.Time}
		}
		items = append(items, i)
	}
	return items, rows.Err()
}

func (s *Store) ListObservationTargets(ctx context.Context, hostID string) ([]domain.ObservationTarget, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT i.id, i.name, i.provider, i.model, i.status, i.image, i.image_id, i.project_name, i.data_volume, i.managed_path,
	   i.codex_configured, i.api_port, i.dashboard_port, i.updated_at, COALESCE(r.request_id, '')
FROM instances i
LEFT JOIN observation_requests r ON r.instance_id=i.id
WHERE i.host_id=?
  AND i.status IN (?, ?, ?)
  AND i.project_name<>'' AND i.data_volume<>'' AND i.managed_path<>''
ORDER BY i.name`, hostID, domain.InstanceRunning, domain.InstanceStopped, domain.InstanceFailed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []domain.ObservationTarget
	for rows.Next() {
		var target domain.ObservationTarget
		var updatedAt time.Time
		if err := rows.Scan(
			&target.InstanceID, &target.Name, &target.Provider, &target.Model, &target.DesiredStatus, &target.Image, &target.ImageID,
			&target.ProjectName, &target.DataVolume, &target.ManagedPath, &target.CodexConfigured, &target.APIPort, &target.DashboardPort,
			&updatedAt, &target.RefreshRequestID,
		); err != nil {
			return nil, err
		}
		target.Generation = observationGeneration(updatedAt)
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

func (s *Store) RecordObservations(ctx context.Context, hostID string, observations []domain.InstanceObservation, receivedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, observation := range observations {
		var targetHostID, status string
		var updatedAt time.Time
		err := tx.QueryRowContext(ctx, `SELECT host_id, status, updated_at FROM instances WHERE id=?`, observation.InstanceID).
			Scan(&targetHostID, &status, &updatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrObservationOwnership
		}
		if err != nil {
			return err
		}
		if targetHostID != hostID {
			return ErrObservationOwnership
		}
		if !isObservableInstanceStatus(status) || observation.TargetGeneration != observationGeneration(updatedAt) {
			continue
		}

		var existingGeneration string
		var existingObservedAt, existingReceivedAt time.Time
		err = tx.QueryRowContext(ctx, `SELECT target_generation, observed_at, received_at FROM instance_observations WHERE instance_id=?`, observation.InstanceID).
			Scan(&existingGeneration, &existingObservedAt, &existingReceivedAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		effectiveReceivedAt := receivedAt
		if err == nil && existingGeneration == observation.TargetGeneration {
			// The API sequence check is intentionally advisory: concurrent
			// reports can both pass it before either write commits. Fence the
			// observation atomically here so an equal or older host report can
			// never overwrite current state or count as another remediation
			// signal.
			if !observation.ObservedAt.After(existingObservedAt) {
				continue
			}
			// Server receipt time drives freshness and retry timing. Preserve
			// its monotonicity if concurrent requests commit out of arrival
			// order while still accepting the newer host observation.
			if existingReceivedAt.After(effectiveReceivedAt) {
				effectiveReceivedAt = existingReceivedAt
			}
		}
		checks, err := json.Marshal(observation.Checks)
		if err != nil {
			return fmt.Errorf("encode observation checks: %w", err)
		}
		modelCatalog, err := json.Marshal(observation.ModelCatalog)
		if err != nil {
			return fmt.Errorf("encode observation model catalog: %w", err)
		}
		providerCatalogs := observation.ProviderModelCatalogs
		if providerCatalogs == nil {
			providerCatalogs = map[string]domain.ProviderModelCatalog{}
		}
		encodedProviderCatalogs, err := json.Marshal(providerCatalogs)
		if err != nil {
			return fmt.Errorf("encode observation provider model catalogs: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO instance_observations (
  instance_id, host_id, target_generation, hermes_version, hermes_source, model_catalog, recommended_model,
  provider_model_catalogs, status, summary, checks, observed_at, received_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(instance_id) DO UPDATE SET
  host_id=excluded.host_id,
  target_generation=excluded.target_generation,
  hermes_version=excluded.hermes_version,
  hermes_source=excluded.hermes_source,
  model_catalog=excluded.model_catalog,
  recommended_model=excluded.recommended_model,
  provider_model_catalogs=excluded.provider_model_catalogs,
  status=excluded.status,
  summary=excluded.summary,
  checks=excluded.checks,
  observed_at=excluded.observed_at,
  received_at=excluded.received_at`,
			observation.InstanceID, hostID, observation.TargetGeneration, observation.HermesVersion, observation.HermesSource,
			modelCatalog, observation.RecommendedModel, encodedProviderCatalogs, observation.Status, observation.Summary, checks, observation.ObservedAt, effectiveReceivedAt,
		); err != nil {
			return fmt.Errorf("record instance observation: %w", err)
		}
		if observation.RefreshRequestID != "" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM observation_requests WHERE instance_id=? AND host_id=? AND request_id=?`,
				observation.InstanceID, hostID, observation.RefreshRequestID); err != nil {
				return fmt.Errorf("clear observation request: %w", err)
			}
		}
	}
	return tx.Commit()
}

const (
	runtimeAutoSyncDriftThreshold = 2
	runtimeAutoSyncMaxAttempts    = 3
	runtimeAutoSyncCooldown       = 5 * time.Minute

	runtimeRemediationDriftThreshold   = 2
	runtimeRemediationAttemptsPerPhase = 3
	runtimeRemediationMaxPhases        = 3
	runtimeRemediationRetryDelay       = 30 * time.Second
	runtimeRemediationCooldown         = 5 * time.Minute
)

func runtimeConfigurationCheckStatus(observation domain.InstanceObservation) string {
	for _, check := range observation.Checks {
		if check.Name == "runtime_configuration" {
			return check.Status
		}
	}
	return ""
}

func (s *Store) TrackRuntimeConfigurationObservation(
	ctx context.Context, observation domain.InstanceObservation, attemptedAt time.Time,
) (bool, error) {
	status := runtimeConfigurationCheckStatus(observation)
	if status == "" {
		return false, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var storedGeneration, provider, model, reasoning, serviceTier string
	var codexConfigured bool
	var storedObservedAt, storedReceivedAt time.Time
	if err := tx.QueryRowContext(ctx, `
SELECT o.target_generation, o.observed_at, o.received_at, i.provider, i.model, i.reasoning, i.service_tier, i.codex_configured
FROM instance_observations o
JOIN instances i ON i.id=o.instance_id
WHERE o.instance_id=?`, observation.InstanceID).Scan(
		&storedGeneration, &storedObservedAt, &storedReceivedAt, &provider, &model, &reasoning, &serviceTier, &codexConfigured,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, tx.Commit()
		}
		return false, err
	}
	if storedGeneration != observation.TargetGeneration || !storedObservedAt.Equal(observation.ObservedAt) {
		return false, tx.Commit()
	}
	if !codexConfigured {
		if _, err := tx.ExecContext(ctx, `DELETE FROM runtime_reconcile_state WHERE instance_id=?`, observation.InstanceID); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if status != domain.ObservationCheckDrift {
		if _, err := tx.ExecContext(ctx, `DELETE FROM runtime_reconcile_state WHERE instance_id=?`, observation.InstanceID); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}

	revisionBytes := sha256.Sum256([]byte(provider + "\x00" + model + "\x00" + reasoning + "\x00" + serviceTier))
	revision := hex.EncodeToString(revisionBytes[:])
	var storedRevision string
	var consecutive, attempts int
	var lastObservedAt time.Time
	var lastAttemptAt sql.NullTime
	err = tx.QueryRowContext(ctx, `
SELECT desired_revision, consecutive_drift, attempts, last_observed_at, last_attempt_at
FROM runtime_reconcile_state WHERE instance_id=?`, observation.InstanceID).Scan(
		&storedRevision, &consecutive, &attempts, &lastObservedAt, &lastAttemptAt,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if errors.Is(err, sql.ErrNoRows) || storedRevision != revision {
		consecutive, attempts, lastAttemptAt = 0, 0, sql.NullTime{}
	} else if !storedReceivedAt.After(lastObservedAt) {
		return false, tx.Commit()
	}
	consecutive++
	queue := consecutive >= runtimeAutoSyncDriftThreshold && attempts < runtimeAutoSyncMaxAttempts &&
		(!lastAttemptAt.Valid || attemptedAt.Sub(lastAttemptAt.Time) >= runtimeAutoSyncCooldown)
	if queue {
		attempts++
		lastAttemptAt = sql.NullTime{Time: attemptedAt, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO runtime_reconcile_state (
  instance_id, desired_revision, consecutive_drift, attempts, last_observed_at, last_attempt_at
) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(instance_id) DO UPDATE SET
  desired_revision=excluded.desired_revision,
  consecutive_drift=excluded.consecutive_drift,
  attempts=excluded.attempts,
  last_observed_at=excluded.last_observed_at,
  last_attempt_at=excluded.last_attempt_at`, observation.InstanceID, revision, consecutive, attempts,
		storedReceivedAt, lastAttemptAt); err != nil {
		return false, err
	}
	return queue, tx.Commit()
}

func (s *Store) RecordRuntimeConfigurationQueueFailure(
	ctx context.Context, instanceID string, attemptedAt time.Time,
) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE runtime_reconcile_state
SET attempts=CASE WHEN attempts > 0 THEN attempts - 1 ELSE 0 END,
    last_attempt_at=NULL
WHERE instance_id=? AND last_attempt_at=?`,
		instanceID, attemptedAt,
	)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateChanged
	}
	return nil
}

type RuntimeRemediationDecision struct {
	Queue bool
	State domain.RuntimeRemediation
}

func runtimeHealthCheckStatus(observation domain.InstanceObservation) string {
	status := ""
	for _, check := range observation.Checks {
		if check.Name != "runtime" && check.Name != "health_endpoint" && check.Name != "dashboard_endpoint" {
			continue
		}
		switch check.Status {
		case domain.ObservationCheckDrift, domain.ObservationCheckMissing:
			return domain.ObservationCheckDrift
		case domain.ObservationCheckUnknown:
			status = domain.ObservationCheckUnknown
		case domain.ObservationCheckOK:
			if status == "" {
				status = domain.ObservationCheckOK
			}
		}
	}
	return status
}

func runtimeRepairPrerequisitesOK(observation domain.InstanceObservation) bool {
	required := map[string]bool{
		"managed_path":  false,
		"manifest":      false,
		"environment":   false,
		"workspace":     false,
		"docker_daemon": false,
		"data_volume":   false,
	}
	for _, check := range observation.Checks {
		if _, requiredCheck := required[check.Name]; requiredCheck && check.Status == domain.ObservationCheckOK {
			required[check.Name] = true
		}
	}
	for _, ok := range required {
		if !ok {
			return false
		}
	}
	return true
}

func runtimeRemediationRevision(values ...string) string {
	revisionBytes := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(revisionBytes[:])
}

func remediationFromColumns(
	instanceID, workflowID, status string,
	phase, attemptInPhase, totalAttempts, consecutiveDrift int,
	lastAttemptAt, nextAttemptAt sql.NullTime,
	lastError string, updatedAt time.Time,
) domain.RuntimeRemediation {
	state := domain.RuntimeRemediation{
		InstanceID: instanceID, WorkflowID: workflowID, Status: status,
		Phase: phase, AttemptInPhase: attemptInPhase, TotalAttempts: totalAttempts,
		MaxPhases:        runtimeRemediationMaxPhases,
		MaxAttempts:      runtimeRemediationAttemptsPerPhase * runtimeRemediationMaxPhases,
		ConsecutiveDrift: consecutiveDrift, LastError: lastError, UpdatedAt: updatedAt,
	}
	if lastAttemptAt.Valid {
		value := lastAttemptAt.Time.UTC()
		state.LastAttemptAt = &value
	}
	if nextAttemptAt.Valid {
		value := nextAttemptAt.Time.UTC()
		state.NextAttemptAt = &value
	}
	return state
}

func (s *Store) TrackRuntimeHealthObservation(
	ctx context.Context,
	observation domain.InstanceObservation,
	attemptedAt time.Time,
	workflowCandidate string,
) (RuntimeRemediationDecision, error) {
	var decision RuntimeRemediationDecision
	checkStatus := runtimeHealthCheckStatus(observation)
	if checkStatus == "" {
		return decision, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return decision, err
	}
	defer tx.Rollback()

	var (
		storedGeneration, instanceStatus, image, imageID, projectName, dataVolume, managedPath string
		provider, model, reasoning, serviceTier                                                string
		apiPort, dashboardPort                                                                 int
		storedObservedAt, storedReceivedAt                                                     time.Time
	)
	if err := tx.QueryRowContext(ctx, `
SELECT o.target_generation, o.observed_at, o.received_at, i.status, i.image, i.image_id, i.project_name,
       i.data_volume, i.managed_path, i.provider, i.model, i.reasoning, i.service_tier,
       i.api_port, i.dashboard_port
FROM instance_observations o
JOIN instances i ON i.id=o.instance_id
WHERE o.instance_id=?`, observation.InstanceID).Scan(
		&storedGeneration, &storedObservedAt, &storedReceivedAt, &instanceStatus, &image, &imageID, &projectName,
		&dataVolume, &managedPath, &provider, &model, &reasoning, &serviceTier,
		&apiPort, &dashboardPort,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return decision, tx.Commit()
		}
		return decision, err
	}
	if storedGeneration != observation.TargetGeneration || !storedObservedAt.Equal(observation.ObservedAt) {
		return decision, tx.Commit()
	}
	if checkStatus == domain.ObservationCheckDrift && !runtimeRepairPrerequisitesOK(observation) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM runtime_remediation_state WHERE instance_id=?`, observation.InstanceID); err != nil {
			return decision, err
		}
		return decision, tx.Commit()
	}

	revision := runtimeRemediationRevision(
		domain.InstanceRunning, image, imageID, projectName, dataVolume, managedPath,
		provider, model, reasoning, serviceTier, strconv.Itoa(apiPort), strconv.Itoa(dashboardPort),
	)
	if instanceStatus != domain.InstanceRunning || checkStatus != domain.ObservationCheckDrift {
		if _, err := tx.ExecContext(ctx, `DELETE FROM runtime_remediation_state WHERE instance_id=?`, observation.InstanceID); err != nil {
			return decision, err
		}
		return decision, tx.Commit()
	}

	var (
		storedRevision, workflowID, stateStatus, lastError string
		phase, attemptInPhase, totalAttempts, consecutive  int
		lastObservedAt, updatedAt                          time.Time
		lastAttemptAt, nextAttemptAt                       sql.NullTime
	)
	err = tx.QueryRowContext(ctx, `
SELECT desired_revision, workflow_id, status, phase, attempt_in_phase, total_attempts,
       consecutive_drift, last_observed_at, last_attempt_at, next_attempt_at, last_error, updated_at
FROM runtime_remediation_state WHERE instance_id=?`, observation.InstanceID).Scan(
		&storedRevision, &workflowID, &stateStatus, &phase, &attemptInPhase, &totalAttempts,
		&consecutive, &lastObservedAt, &lastAttemptAt, &nextAttemptAt, &lastError, &updatedAt,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return decision, err
	}
	if errors.Is(err, sql.ErrNoRows) || storedRevision != revision {
		workflowID = workflowCandidate
		stateStatus = "MONITORING"
		phase, attemptInPhase, totalAttempts, consecutive = 1, 0, 0, 0
		lastAttemptAt, nextAttemptAt = sql.NullTime{}, sql.NullTime{}
		lastError = ""
	} else if !storedReceivedAt.After(lastObservedAt) {
		decision.State = remediationFromColumns(
			observation.InstanceID, workflowID, stateStatus, phase, attemptInPhase, totalAttempts,
			consecutive, lastAttemptAt, nextAttemptAt, lastError, updatedAt,
		)
		return decision, tx.Commit()
	}
	if workflowID == "" {
		workflowID = workflowCandidate
	}
	consecutive++
	if totalAttempts >= runtimeRemediationAttemptsPerPhase*runtimeRemediationMaxPhases && stateStatus != "CANCELED" {
		stateStatus = "EXHAUSTED"
		nextAttemptAt = sql.NullTime{}
	}

	if stateStatus != "CANCELED" && stateStatus != "EXHAUSTED" &&
		consecutive >= runtimeRemediationDriftThreshold &&
		(!nextAttemptAt.Valid || !attemptedAt.Before(nextAttemptAt.Time)) {
		nextPhase, nextAttemptInPhase, nextTotalAttempts := phase, attemptInPhase, totalAttempts
		if nextAttemptInPhase >= runtimeRemediationAttemptsPerPhase && nextPhase < runtimeRemediationMaxPhases {
			nextPhase++
			nextAttemptInPhase = 0
		}
		if nextTotalAttempts < runtimeRemediationAttemptsPerPhase*runtimeRemediationMaxPhases {
			nextAttemptInPhase++
			nextTotalAttempts++
			predictedLastAttemptAt := sql.NullTime{Time: attemptedAt, Valid: true}
			var predictedNextAttemptAt sql.NullTime
			switch {
			case nextTotalAttempts >= runtimeRemediationAttemptsPerPhase*runtimeRemediationMaxPhases:
				predictedNextAttemptAt = sql.NullTime{}
			case nextAttemptInPhase < runtimeRemediationAttemptsPerPhase:
				delay := runtimeRemediationRetryDelay
				if nextAttemptInPhase == 2 {
					delay = 2 * runtimeRemediationRetryDelay
				}
				predictedNextAttemptAt = sql.NullTime{Time: attemptedAt.Add(delay), Valid: true}
			default:
				predictedNextAttemptAt = sql.NullTime{Time: attemptedAt.Add(runtimeRemediationCooldown), Valid: true}
			}
			stateStatus = "READY"
			lastError = ""
			decision.Queue = true
			decision.State = remediationFromColumns(
				observation.InstanceID, workflowID, "QUEUED", nextPhase, nextAttemptInPhase, nextTotalAttempts,
				consecutive, predictedLastAttemptAt, predictedNextAttemptAt, "", attemptedAt,
			)
		}
	}
	updatedAt = attemptedAt
	if _, err := tx.ExecContext(ctx, `
INSERT INTO runtime_remediation_state (
  instance_id, desired_revision, workflow_id, status, phase, attempt_in_phase, total_attempts,
  consecutive_drift, last_observed_at, last_attempt_at, next_attempt_at, last_error, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(instance_id) DO UPDATE SET
  desired_revision=excluded.desired_revision,
  workflow_id=excluded.workflow_id,
  status=excluded.status,
  phase=excluded.phase,
  attempt_in_phase=excluded.attempt_in_phase,
  total_attempts=excluded.total_attempts,
  consecutive_drift=excluded.consecutive_drift,
  last_observed_at=excluded.last_observed_at,
  last_attempt_at=excluded.last_attempt_at,
  next_attempt_at=excluded.next_attempt_at,
  last_error=excluded.last_error,
  updated_at=excluded.updated_at`,
		observation.InstanceID, revision, workflowID, stateStatus, phase, attemptInPhase, totalAttempts,
		consecutive, storedReceivedAt, lastAttemptAt, nextAttemptAt, lastError, updatedAt,
	); err != nil {
		return decision, err
	}
	if !decision.Queue {
		decision.State = remediationFromColumns(
			observation.InstanceID, workflowID, stateStatus, phase, attemptInPhase, totalAttempts,
			consecutive, lastAttemptAt, nextAttemptAt, lastError, updatedAt,
		)
	}
	return decision, tx.Commit()
}

func (s *Store) GetRuntimeRemediation(ctx context.Context, instanceID string) (*domain.RuntimeRemediation, error) {
	var (
		workflowID, status, lastError               string
		phase, attemptInPhase, totalAttempts, drift int
		lastAttemptAt, nextAttemptAt                sql.NullTime
		updatedAt                                   time.Time
	)
	err := s.db.QueryRowContext(ctx, `
SELECT workflow_id, status, phase, attempt_in_phase, total_attempts, consecutive_drift,
       last_attempt_at, next_attempt_at, last_error, updated_at
FROM runtime_remediation_state WHERE instance_id=?`, instanceID).Scan(
		&workflowID, &status, &phase, &attemptInPhase, &totalAttempts, &drift,
		&lastAttemptAt, &nextAttemptAt, &lastError, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	state := remediationFromColumns(
		instanceID, workflowID, status, phase, attemptInPhase, totalAttempts,
		drift, lastAttemptAt, nextAttemptAt, lastError, updatedAt,
	)
	return &state, nil
}

func (s *Store) RecordRuntimeRemediationQueueFailure(ctx context.Context, instanceID, detail string, failedAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE runtime_remediation_state
SET status='WAITING', next_attempt_at=?, last_error=?, updated_at=?
WHERE instance_id=? AND status='READY'`,
		failedAt.Add(runtimeRemediationRetryDelay), detail, failedAt, instanceID,
	)
	return err
}

func (s *Store) CancelRuntimeRemediation(ctx context.Context, instanceID string, canceledAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE runtime_remediation_state
SET status='CANCELED', next_attempt_at=NULL, last_error='', updated_at=?
WHERE instance_id=? AND status NOT IN ('CANCELED', 'EXHAUSTED')`, canceledAt, instanceID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateChanged
	}
	return nil
}

func (s *Store) HasFreshObservationCheck(ctx context.Context, instanceID, checkName, checkStatus string, freshAfter time.Time) (bool, error) {
	var targetGeneration string
	var checksJSON []byte
	var receivedAt, instanceUpdatedAt time.Time
	err := s.db.QueryRowContext(ctx, `
SELECT o.target_generation, o.checks, o.received_at, i.updated_at
FROM instance_observations o
JOIN instances i ON i.id=o.instance_id
WHERE o.instance_id=?`, instanceID).Scan(&targetGeneration, &checksJSON, &receivedAt, &instanceUpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if targetGeneration != observationGeneration(instanceUpdatedAt) || receivedAt.Before(freshAfter) {
		return false, nil
	}
	var checks []domain.ObservationCheck
	if err := json.Unmarshal(checksJSON, &checks); err != nil {
		return false, fmt.Errorf("decode observation checks: %w", err)
	}
	for _, check := range checks {
		if check.Name == checkName && check.Status == checkStatus {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) RequestObservation(ctx context.Context, instanceID, requestID string, requestedAt time.Time) (domain.ObservationRequest, error) {
	var hostID, status, projectName, dataVolume, managedPath string
	err := s.db.QueryRowContext(ctx, `
SELECT host_id, status, project_name, data_volume, managed_path FROM instances WHERE id=?`, instanceID).
		Scan(&hostID, &status, &projectName, &dataVolume, &managedPath)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ObservationRequest{}, ErrNotFound
	}
	if err != nil {
		return domain.ObservationRequest{}, err
	}
	if !isObservableInstanceStatus(status) || projectName == "" || dataVolume == "" || managedPath == "" {
		return domain.ObservationRequest{}, ErrObservationNotReady
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO observation_requests (instance_id, host_id, request_id, requested_at)
		VALUES (?, ?, ?, ?)`, instanceID, hostID, requestID, requestedAt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return domain.ObservationRequest{}, ErrObservationBusy
		}
		return domain.ObservationRequest{}, fmt.Errorf("request instance observation: %w", err)
	}
	return domain.ObservationRequest{ID: requestID, InstanceID: instanceID, RequestedAt: requestedAt}, nil
}

func isObservableInstanceStatus(status string) bool {
	return status == domain.InstanceRunning || status == domain.InstanceStopped || status == domain.InstanceFailed
}

func observationGeneration(updatedAt time.Time) string {
	return updatedAt.UTC().Format(time.RFC3339Nano)
}

func (s *Store) GetInstance(ctx context.Context, id string) (domain.Instance, error) {
	var i domain.Instance
	var observationHostID, targetGeneration, hermesVersion, hermesSource, recommendedModel, observationStatus, observationSummary sql.NullString
	var observationChecks, modelCatalog, providerCatalogs []byte
	var observedAt, receivedAt sql.NullTime
	var requestID sql.NullString
	var requestedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
SELECT i.id, i.name, i.host_id, i.status, i.image, i.image_id, i.provider, i.model, i.reasoning,
       i.service_tier, i.codex_configured, i.api_port, i.dashboard_port, i.public_hostname, i.project_name, i.data_volume,
       i.managed_path, i.last_error, i.created_at, i.updated_at,
       o.host_id, o.target_generation, o.hermes_version, o.hermes_source, o.model_catalog, o.recommended_model,
       o.provider_model_catalogs, o.status, o.summary, o.checks, o.observed_at, o.received_at,
       r.request_id, r.requested_at
FROM instances i
LEFT JOIN instance_observations o ON o.instance_id=i.id
LEFT JOIN observation_requests r ON r.instance_id=i.id
WHERE i.id=?`, id).Scan(&i.ID, &i.Name, &i.HostID, &i.Status, &i.Image, &i.ImageID,
		&i.Provider, &i.Model, &i.Reasoning, &i.ServiceTier, &i.CodexConfigured, &i.APIPort, &i.DashboardPort,
		&i.PublicHostname, &i.ProjectName, &i.DataVolume, &i.ManagedPath, &i.LastError, &i.CreatedAt, &i.UpdatedAt,
		&observationHostID, &targetGeneration, &hermesVersion, &hermesSource, &modelCatalog, &recommendedModel, &providerCatalogs,
		&observationStatus, &observationSummary, &observationChecks, &observedAt, &receivedAt,
		&requestID, &requestedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return i, ErrNotFound
	}
	if err != nil {
		return i, err
	}
	if observationStatus.Valid {
		observation, decodeErr := decodeStoredObservation(
			i.ID, observationHostID.String, targetGeneration.String, hermesVersion.String, hermesSource.String,
			recommendedModel.String, observationStatus.String, observationSummary.String,
			observationChecks, modelCatalog, providerCatalogs, observedAt.Time, receivedAt.Time,
		)
		if decodeErr != nil {
			return i, decodeErr
		}
		i.Observation = observation
	}
	if requestID.Valid {
		i.ObservationRequest = &domain.ObservationRequest{ID: requestID.String, InstanceID: i.ID, RequestedAt: requestedAt.Time}
	}
	return i, nil
}

// UpdateInstancePublicHostname stores publishing metadata without advancing the
// instance runtime generation. Cloudflare routing is owned by the control plane
// and must not cause the Host Agent to recreate an otherwise unchanged runtime.
func (s *Store) UpdateInstancePublicHostname(ctx context.Context, id, hostname string) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE instances
SET public_hostname=?
WHERE id=? AND status <> ?`, hostname, id, domain.InstanceDeleted)
	if err != nil {
		return fmt.Errorf("update instance public hostname: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read instance public hostname update count: %w", err)
	}
	if count != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) StartInstancePublishing(ctx context.Context, id, hostname string, operation domain.Operation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE instances SET public_hostname=? WHERE id=? AND status NOT IN (?, ?)`,
		hostname, id, domain.InstanceDeleted, domain.InstanceDeleting)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return errors.New("public hostname is already assigned to another instance")
		}
		return fmt.Errorf("update instance public hostname: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrNotFound
	}
	if err := insertControlPlaneOperation(ctx, tx, operation); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateControlPlaneOperation(ctx context.Context, operation domain.Operation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertControlPlaneOperation(ctx, tx, operation); err != nil {
		return err
	}
	return tx.Commit()
}

func insertControlPlaneOperation(ctx context.Context, tx *sql.Tx, operation domain.Operation) error {
	actor := operation.Actor
	if actor == "" {
		actor = "FLEET_ADMIN"
	}
	metadata := operation.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	progress := []byte(`{}`)
	if operation.Progress != nil {
		var err error
		progress, err = json.Marshal(operation.Progress)
		if err != nil {
			return fmt.Errorf("encode operation progress: %w", err)
		}
	}
	var instanceID any
	if operation.InstanceID != "" {
		instanceID = operation.InstanceID
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO operations (id, instance_id, workflow_id, actor, type, status, summary, metadata, progress, error, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, operation.ID, instanceID, operation.WorkflowID, actor,
		operation.Type, operation.Status, operation.Summary, []byte(metadata), progress, operation.Error,
		operation.CreatedAt, operation.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert control-plane operation: %w", err)
	}
	return nil
}

func (s *Store) UpdateControlPlaneOperation(ctx context.Context, id, status string, progress domain.JobProgress, operationErr string, at time.Time) error {
	encoded, err := json.Marshal(progress)
	if err != nil {
		return fmt.Errorf("encode operation progress: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE operations SET status=?, progress=?, error=?, updated_at=?
WHERE id=? AND (status IN (?, ?) OR status=?)`, status, encoded, operationErr, at, id,
		domain.OperationPending, domain.OperationRunning, status)
	if err != nil {
		return fmt.Errorf("update control-plane operation: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		var exists int
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE id=?)`, id).Scan(&exists); err != nil {
			return fmt.Errorf("check control-plane operation: %w", err)
		}
		if exists == 0 {
			return ErrNotFound
		}
		return ErrStateChanged
	}
	return nil
}

// ListStalePublishingOperations returns only control-plane publication work.
// Job-backed operations have independent lease fencing and must never be
// finalized by this recovery path.
func (s *Store) ListStalePublishingOperations(ctx context.Context, updatedBefore time.Time, limit int) ([]domain.Operation, error) {
	if limit <= 0 {
		return []domain.Operation{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT o.id, COALESCE(o.instance_id, ''), o.workflow_id, o.actor, o.type, o.status, o.summary,
       o.metadata, o.error, o.created_at, o.updated_at, o.progress
FROM operations o
WHERE o.type='PUBLISH_DASHBOARD'
  AND o.status IN (?, ?)
  AND o.updated_at <= ?
  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.operation_id=o.id)
ORDER BY o.updated_at, o.id
LIMIT ?`, domain.OperationPending, domain.OperationRunning, updatedBefore, limit)
	if err != nil {
		return nil, fmt.Errorf("list stale publishing operations: %w", err)
	}
	defer rows.Close()
	operations := make([]domain.Operation, 0)
	for rows.Next() {
		var operation domain.Operation
		var metadata, progress []byte
		if err := rows.Scan(&operation.ID, &operation.InstanceID, &operation.WorkflowID, &operation.Actor,
			&operation.Type, &operation.Status, &operation.Summary, &metadata, &operation.Error,
			&operation.CreatedAt, &operation.UpdatedAt, &progress); err != nil {
			return nil, err
		}
		operation.Metadata = json.RawMessage(metadata)
		if len(progress) > 0 && string(progress) != "{}" {
			var decoded domain.JobProgress
			if err := json.Unmarshal(progress, &decoded); err != nil {
				return nil, fmt.Errorf("decode stale publishing operation progress: %w", err)
			}
			operation.Progress = &decoded
		}
		operations = append(operations, operation)
	}
	return operations, rows.Err()
}

// FinalizeStalePublishingOperation uses the stale timestamp as a fencing
// token. If the original goroutine made progress after the candidate was read,
// this update is rejected instead of overwriting live work.
func (s *Store) FinalizeStalePublishingOperation(
	ctx context.Context,
	id string,
	updatedBefore time.Time,
	status string,
	progress domain.JobProgress,
	operationErr string,
	at time.Time,
) (bool, error) {
	if status != domain.OperationSucceeded && status != domain.OperationFailed {
		return false, errors.New("stale publishing operation requires a terminal status")
	}
	encoded, err := json.Marshal(progress)
	if err != nil {
		return false, fmt.Errorf("encode stale publishing progress: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE operations
SET status=?, progress=?, error=?, updated_at=?
WHERE id=?
  AND type='PUBLISH_DASHBOARD'
  AND status IN (?, ?)
  AND updated_at <= ?
  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.operation_id=operations.id)`,
		status, encoded, operationErr, at, id, domain.OperationPending, domain.OperationRunning, updatedBefore)
	if err != nil {
		return false, fmt.Errorf("finalize stale publishing operation: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read stale publishing finalize count: %w", err)
	}
	return count == 1, nil
}

func (s *Store) ListRemoteAccessResources(ctx context.Context) ([]domain.RemoteAccessResource, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT instance_id, kind, resource_id, hostname, tunnel_id, zone_id, origin_service, created_at, updated_at
FROM remote_access_resources ORDER BY instance_id, kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var resources []domain.RemoteAccessResource
	for rows.Next() {
		var resource domain.RemoteAccessResource
		if err := rows.Scan(&resource.InstanceID, &resource.Kind, &resource.ResourceID, &resource.Hostname,
			&resource.TunnelID, &resource.ZoneID, &resource.OriginService, &resource.CreatedAt, &resource.UpdatedAt); err != nil {
			return nil, err
		}
		resources = append(resources, resource)
	}
	return resources, rows.Err()
}

func (s *Store) PutRemoteAccessResource(ctx context.Context, resource domain.RemoteAccessResource) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO remote_access_resources
  (instance_id, kind, resource_id, hostname, tunnel_id, zone_id, origin_service, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(instance_id, kind, hostname) DO UPDATE SET
  resource_id=excluded.resource_id, hostname=excluded.hostname, tunnel_id=excluded.tunnel_id,
  zone_id=excluded.zone_id, origin_service=excluded.origin_service, updated_at=excluded.updated_at`,
		resource.InstanceID, resource.Kind, resource.ResourceID, resource.Hostname, resource.TunnelID,
		resource.ZoneID, resource.OriginService, resource.CreatedAt, resource.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store remote access resource ownership: %w", err)
	}
	return nil
}

func (s *Store) DeleteRemoteAccessResource(ctx context.Context, instanceID, kind, hostname string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM remote_access_resources WHERE instance_id=? AND kind=? AND hostname=?`, instanceID, kind, hostname)
	return err
}

func (s *Store) GetInstanceModelCatalog(ctx context.Context, instanceID string) ([]string, string, error) {
	return s.GetInstanceProviderModelCatalog(ctx, instanceID, "")
}

func (s *Store) GetInstanceProviderModelCatalog(ctx context.Context, instanceID, provider string) ([]string, string, error) {
	var encoded, providerCatalogs []byte
	var recommended, activeProvider string
	err := s.db.QueryRowContext(ctx, `
SELECT o.model_catalog, o.recommended_model, o.provider_model_catalogs, i.provider
FROM instance_observations o
JOIN instances i ON i.id=o.instance_id
WHERE o.instance_id=?`, instanceID).Scan(&encoded, &recommended, &providerCatalogs, &activeProvider)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	if provider != "" {
		catalogs, decodeErr := decodeProviderModelCatalogs(providerCatalogs)
		if decodeErr != nil {
			return nil, "", fmt.Errorf("decode instance provider model catalogs: %w", decodeErr)
		}
		if catalog, ok := catalogs[provider]; ok && len(catalog.Models) > 0 {
			return catalog.Models, catalog.Recommended, nil
		}
		if provider != activeProvider {
			return nil, "", ErrNotFound
		}
	}
	var models []string
	if err := json.Unmarshal(encoded, &models); err != nil {
		return nil, "", fmt.Errorf("decode instance model catalog: %w", err)
	}
	return models, recommended, nil
}

func decodeStoredObservation(
	instanceID, hostID, targetGeneration, hermesVersion, hermesSource, recommendedModel, status, summary string,
	checksJSON, modelCatalogJSON, providerCatalogsJSON []byte,
	observedAt, receivedAt time.Time,
) (*domain.InstanceObservation, error) {
	var checks []domain.ObservationCheck
	if err := json.Unmarshal(checksJSON, &checks); err != nil {
		return nil, fmt.Errorf("decode observation checks for %s: %w", instanceID, err)
	}
	var models []string
	if err := json.Unmarshal(modelCatalogJSON, &models); err != nil {
		return nil, fmt.Errorf("decode model catalog for %s: %w", instanceID, err)
	}
	catalogs, err := decodeProviderModelCatalogs(providerCatalogsJSON)
	if err != nil {
		return nil, fmt.Errorf("decode provider model catalogs for %s: %w", instanceID, err)
	}
	return &domain.InstanceObservation{
		InstanceID: instanceID, HostID: hostID, TargetGeneration: targetGeneration,
		HermesVersion: hermesVersion, HermesSource: hermesSource,
		ModelCatalog: models, RecommendedModel: recommendedModel, ProviderModelCatalogs: catalogs,
		Status: status, Summary: summary, Checks: checks,
		ObservedAt: observedAt, ReceivedAt: receivedAt,
	}, nil
}

func decodeProviderModelCatalogs(encoded []byte) (map[string]domain.ProviderModelCatalog, error) {
	if len(encoded) == 0 {
		return nil, nil
	}
	var catalogs map[string]domain.ProviderModelCatalog
	if err := json.Unmarshal(encoded, &catalogs); err != nil {
		return nil, err
	}
	if len(catalogs) == 0 {
		return nil, nil
	}
	return catalogs, nil
}

const jobLeaseMaxClaims = 3

func (s *Store) ClaimJob(ctx context.Context, hostID string, lease time.Duration) (*domain.Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	for {
		var job domain.Job
		var payload, operationMetadata []byte
		var leaseAt sql.NullTime
		err = tx.QueryRowContext(ctx, `
SELECT candidate.id, candidate.operation_id, candidate.host_id, candidate.instance_id, candidate.type, candidate.status,
       candidate.payload, candidate.attempts, candidate.lease_expires_at, candidate.created_at, candidate.updated_at,
       op.metadata
FROM jobs candidate
JOIN operations op ON op.id=candidate.operation_id
WHERE candidate.host_id=?
  AND (candidate.status=? OR ((candidate.status=? OR candidate.status=?) AND candidate.lease_expires_at < ?))
  AND (
    (candidate.type='instance.chat.send' AND NOT EXISTS (
      SELECT 1
      FROM jobs earlier
      JOIN chat_messages candidate_message
        ON candidate_message.operation_id=candidate.operation_id AND candidate_message.role='user'
      JOIN chat_messages earlier_message
        ON earlier_message.operation_id=earlier.operation_id AND earlier_message.role='user'
      WHERE earlier.id<>candidate.id
        AND earlier.type='instance.chat.send'
        AND earlier_message.session_id=candidate_message.session_id
        AND (
          (earlier.status IN (?, ?) AND earlier.lease_expires_at >= ?)
          OR (earlier.status=? AND (earlier.created_at<candidate.created_at OR
              (earlier.created_at=candidate.created_at AND earlier.id<candidate.id)))
        )
    ))
    OR
    (candidate.type<>'instance.chat.send' AND NOT EXISTS (
      SELECT 1 FROM jobs active
      WHERE active.host_id=candidate.host_id AND active.instance_id=candidate.instance_id AND active.id<>candidate.id
        AND active.status IN (?, ?) AND active.lease_expires_at >= ?
    ))
  )
ORDER BY
  CASE candidate.type
    WHEN 'instance.stop' THEN 0
    WHEN 'instance.runtime.repair' THEN 0
    WHEN 'instance.recovery.restore' THEN 0
    WHEN 'instance.image.repair' THEN 0
    WHEN 'instance.credentials.inspect' THEN 20
    ELSE 10
  END,
  candidate.created_at,
  candidate.id
LIMIT 1`, hostID, domain.JobPending, domain.JobLeased, domain.JobRunning, now,
			domain.JobLeased, domain.JobRunning, now, domain.JobPending,
			domain.JobLeased, domain.JobRunning, now).
			Scan(&job.ID, &job.OperationID, &job.HostID, &job.InstanceID, &job.Type, &job.Status,
				&payload, &job.Attempts, &leaseAt, &job.CreatedAt, &job.UpdatedAt, &operationMetadata)
		if errors.Is(err, sql.ErrNoRows) {
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		job.Payload = json.RawMessage(payload)
		if (job.Status == domain.JobLeased || job.Status == domain.JobRunning) &&
			leaseAt.Valid && leaseAt.Time.Before(now) &&
			(job.Type == "instance.chat.send" || job.Attempts >= jobLeaseMaxClaims) {
			if err := failExpiredJobLease(ctx, tx, job, operationMetadata, now); err != nil {
				return nil, err
			}
			continue
		}
		leaseToken, err := newLeaseToken()
		if err != nil {
			return nil, err
		}
		expires := now.Add(lease)
		result, err := tx.ExecContext(ctx, `
UPDATE jobs SET status=?, attempts=attempts+1, lease_token=?, lease_expires_at=?, progress='{}', updated_at=?
WHERE id=? AND (status=? OR ((status=? OR status=?) AND lease_expires_at < ?))`,
			domain.JobLeased, leaseToken, expires, now, job.ID, domain.JobPending, domain.JobLeased, domain.JobRunning, now)
		if err != nil {
			return nil, err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return nil, nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE operations SET status=?, updated_at=? WHERE id=?`, domain.OperationRunning, now, job.OperationID); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		job.Status = domain.JobLeased
		job.Payload = json.RawMessage(payload)
		job.Attempts++
		job.LeaseToken = leaseToken
		job.LeaseExpiresAt = &expires
		job.UpdatedAt = now
		return &job, nil
	}
}

type HostQueueHealth struct {
	HostID          string     `json:"host_id"`
	HostName        string     `json:"host_name"`
	Pending         int        `json:"pending"`
	Active          int        `json:"active"`
	ExpiredLeases   int        `json:"expired_leases"`
	OldestPendingAt *time.Time `json:"oldest_pending_at,omitempty"`
	AdmissionOpen   bool       `json:"admission_open"`
}

type QueueHealth struct {
	Pending           int               `json:"pending"`
	Active            int               `json:"active"`
	ExpiredLeases     int               `json:"expired_leases"`
	AdmissionRejected bool              `json:"admission_rejected"`
	MaxPerHost        int               `json:"max_per_host"`
	Hosts             []HostQueueHealth `json:"hosts"`
}

type nullableSQLiteTime struct {
	Time  time.Time
	Valid bool
}

func (value *nullableSQLiteTime) Scan(source any) error {
	if source == nil {
		value.Time = time.Time{}
		value.Valid = false
		return nil
	}
	if timestamp, ok := source.(time.Time); ok {
		value.Time = timestamp
		value.Valid = true
		return nil
	}

	var encoded string
	switch source := source.(type) {
	case string:
		encoded = source
	case []byte:
		encoded = string(source)
	default:
		return fmt.Errorf("scan SQLite time from %T", source)
	}
	if monotonic := strings.Index(encoded, " m="); monotonic >= 0 {
		encoded = encoded[:monotonic]
	}
	encoded = strings.TrimSpace(encoded)
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04",
		"2006-01-02",
	} {
		timestamp, err := time.Parse(layout, encoded)
		if err == nil {
			value.Time = timestamp
			value.Valid = true
			return nil
		}
	}
	return fmt.Errorf("scan SQLite time %q: unsupported format", encoded)
}

func (s *Store) QueueHealth(ctx context.Context, now time.Time) (QueueHealth, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT h.id, h.name,
       COALESCE(SUM(CASE WHEN j.status=? THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN j.status IN (?, ?, ?) THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN j.status IN (?, ?) AND j.lease_expires_at < ? THEN 1 ELSE 0 END), 0),
       MIN(CASE WHEN j.status=? THEN j.created_at END)
FROM hosts h
LEFT JOIN jobs j ON j.host_id=h.id
GROUP BY h.id, h.name
ORDER BY h.name`,
		domain.JobPending,
		domain.JobPending, domain.JobLeased, domain.JobRunning,
		domain.JobLeased, domain.JobRunning, now,
		domain.JobPending,
	)
	if err != nil {
		return QueueHealth{}, err
	}
	defer rows.Close()
	health := QueueHealth{MaxPerHost: JobQueueMaxPerHost, Hosts: []HostQueueHealth{}}
	for rows.Next() {
		var host HostQueueHealth
		var oldest nullableSQLiteTime
		if err := rows.Scan(&host.HostID, &host.HostName, &host.Pending, &host.Active, &host.ExpiredLeases, &oldest); err != nil {
			return QueueHealth{}, err
		}
		if oldest.Valid {
			value := oldest.Time
			host.OldestPendingAt = &value
		}
		host.AdmissionOpen = host.Active < JobQueueMaxPerHost
		health.Pending += host.Pending
		health.Active += host.Active
		health.ExpiredLeases += host.ExpiredLeases
		if !host.AdmissionOpen {
			health.AdmissionRejected = true
		}
		health.Hosts = append(health.Hosts, host)
	}
	return health, rows.Err()
}

// ReconcileExpiredJobs terminally resolves leases that have exhausted their
// retry fence. Chat work is fail-closed after its first lease because replaying
// an unknown upstream state could execute Hermes tools more than once.
func (s *Store) ReconcileExpiredJobs(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
SELECT j.id, j.operation_id, j.host_id, j.instance_id, j.type, j.status,
       j.payload, j.attempts, j.lease_expires_at, j.created_at, j.updated_at, o.metadata
FROM jobs j
JOIN operations o ON o.id=j.operation_id
WHERE j.status IN (?, ?) AND j.lease_expires_at < ?
  AND (j.attempts >= ? OR (j.type='instance.chat.send' AND j.attempts >= 1))
ORDER BY j.lease_expires_at, j.id`, domain.JobLeased, domain.JobRunning, now, jobLeaseMaxClaims)
	if err != nil {
		return 0, err
	}
	type expiredJob struct {
		job      domain.Job
		metadata []byte
	}
	expired := []expiredJob{}
	for rows.Next() {
		var item expiredJob
		var payload []byte
		var leaseAt sql.NullTime
		if err := rows.Scan(
			&item.job.ID, &item.job.OperationID, &item.job.HostID, &item.job.InstanceID,
			&item.job.Type, &item.job.Status, &payload, &item.job.Attempts, &leaseAt,
			&item.job.CreatedAt, &item.job.UpdatedAt, &item.metadata,
		); err != nil {
			rows.Close()
			return 0, err
		}
		item.job.Payload = json.RawMessage(payload)
		if leaseAt.Valid {
			value := leaseAt.Time
			item.job.LeaseExpiresAt = &value
		}
		expired = append(expired, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, item := range expired {
		if err := failExpiredJobLease(ctx, tx, item.job, item.metadata, now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(expired), nil
}

// hermesUpdateOriginalStatus membaca status runtime sebelum update agar lease
// yang habis tidak memaksa instance ke FAILED.
func hermesUpdateOriginalStatus(job domain.Job, metadata map[string]any) string {
	if job.Type != "instance.hermes.update" {
		return ""
	}
	var payload domain.HermesUpdatePayload
	if err := json.Unmarshal(job.Payload, &payload); err == nil {
		switch payload.OriginalStatus {
		case domain.InstanceRunning, domain.InstanceStopped:
			return payload.OriginalStatus
		}
	}
	if metadata != nil {
		if status, ok := metadata["original_status"].(string); ok {
			switch status {
			case domain.InstanceRunning, domain.InstanceStopped:
				return status
			}
		}
	}
	return ""
}

func failExpiredJobLease(
	ctx context.Context,
	tx *sql.Tx,
	job domain.Job,
	operationMetadata []byte,
	failedAt time.Time,
) error {
	reason := fmt.Sprintf("Host Agent lost this job lease %d times; manual retry is required", jobLeaseMaxClaims)
	failure := "lease-retry-exhausted"
	if job.Type == "instance.chat.send" {
		reason = "Host Agent lost the chat job lease; automatic retry is disabled to prevent duplicate Hermes tool execution"
		failure = "chat-lease-lost"
	}
	result, err := tx.ExecContext(ctx, `
UPDATE jobs
SET status=?, lease_token='', lease_expires_at=NULL, updated_at=?
WHERE id=? AND status IN (?, ?) AND lease_expires_at < ?`,
		domain.JobFailed, failedAt, job.ID, domain.JobLeased, domain.JobRunning, failedAt,
	)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateChanged
	}
	metadata := map[string]any{}
	if len(operationMetadata) > 0 {
		if err := json.Unmarshal(operationMetadata, &metadata); err != nil {
			return fmt.Errorf("decode exhausted job operation metadata: %w", err)
		}
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["failure"] = failure
	metadata["lease_claims"] = job.Attempts
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode exhausted job operation metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE operations SET status=?, error=?, metadata=?, updated_at=? WHERE id=?`,
		domain.OperationFailed, reason, encodedMetadata, failedAt, job.OperationID,
	); err != nil {
		return err
	}
	if job.Type != "instance.credentials.inspect" && job.Type != "instance.auth.codex" {
		instanceStatus := domain.InstanceFailed
		switch job.Type {
		case "instance.runtime.repair":
			instanceStatus = domain.InstanceRunning
		case "instance.recovery.create":
			instanceStatus = domain.InstanceStopped
		case "instance.hermes.update":
			if status := hermesUpdateOriginalStatus(job, metadata); status != "" {
				instanceStatus = status
			}
		case "instance.chat.send":
			instanceStatus = ""
		}
		if job.Type != "instance.chat.send" {
			if _, err := tx.ExecContext(ctx, `
UPDATE instances SET status=?, last_error=?, updated_at=? WHERE id=?`,
				instanceStatus, reason, failedAt, job.InstanceID,
			); err != nil {
				return err
			}
		} else {
			var payload domain.ChatSendPayload
			if err := json.Unmarshal(job.Payload, &payload); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE chat_messages SET status=?, error=?, updated_at=? WHERE id=? AND operation_id=?`,
				domain.ChatMessageFailed, reason, failedAt, payload.MessageID, job.OperationID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE chat_sessions SET last_error=?, updated_at=? WHERE id=?`,
				reason, failedAt, payload.SessionID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO chat_events (session_id, operation_id, sequence, type, created_at)
SELECT ?, ?, COALESCE(MAX(sequence), 0)+1, ?, ? FROM chat_events WHERE operation_id=?`,
				payload.SessionID, job.OperationID, domain.ChatEventFailed, failedAt, job.OperationID); err != nil {
				return err
			}
		}
	}
	if job.Type == "instance.runtime.repair" {
		if _, err := tx.ExecContext(ctx, `
UPDATE runtime_remediation_state
SET status=CASE
      WHEN total_attempts >= ? THEN 'EXHAUSTED'
      WHEN attempt_in_phase >= ? THEN 'COOLDOWN'
      ELSE 'WAITING'
    END,
    last_error=?, updated_at=?
WHERE instance_id=? AND status='QUEUED'`,
			runtimeRemediationAttemptsPerPhase*runtimeRemediationMaxPhases,
			runtimeRemediationAttemptsPerPhase,
			reason, failedAt, job.InstanceID,
		); err != nil {
			return err
		}
	}
	if job.Type == "instance.messaging.configure" {
		if _, err := tx.ExecContext(ctx, `
UPDATE instance_messaging_configs
SET status='FAILED', last_error=?, updated_at=?
WHERE instance_id=? AND status='PENDING'`,
			reason, failedAt, job.InstanceID,
		); err != nil {
			return err
		}
	}
	if job.Type == "instance.mcp.configure" {
		if _, err := tx.ExecContext(ctx, `
UPDATE instance_mcp_configs
SET status='FAILED', last_error=?, updated_at=?
WHERE instance_id=? AND status='PENDING'`,
			reason, failedAt, job.InstanceID,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) AcknowledgeJob(ctx context.Context, hostID, jobID, leaseToken string, lease time.Duration) error {
	if leaseToken == "" {
		return ErrLeaseLost
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE jobs SET status=?, lease_expires_at=?, updated_at=?
WHERE id=? AND host_id=? AND status=? AND lease_token=? AND lease_expires_at>?`,
		domain.JobRunning, now.Add(lease), now, jobID, hostID, domain.JobLeased, leaseToken, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) RenewJob(ctx context.Context, hostID, jobID, leaseToken string, lease time.Duration) error {
	if leaseToken == "" {
		return ErrLeaseLost
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE jobs SET lease_expires_at=?, updated_at=?
WHERE id=? AND host_id=? AND status=? AND lease_token=? AND lease_expires_at>?`,
		now.Add(lease), now, jobID, hostID, domain.JobRunning, leaseToken, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		var terminalMatch bool
		if err := s.db.QueryRowContext(ctx, `
SELECT EXISTS(
  SELECT 1 FROM jobs
  WHERE id=? AND host_id=? AND status IN (?, ?) AND lease_token=?
)`,
			jobID, hostID, domain.JobSucceeded, domain.JobFailed, leaseToken,
		).Scan(&terminalMatch); err != nil {
			return err
		}
		if !terminalMatch {
			return ErrLeaseLost
		}
	}
	return nil
}

func (s *Store) UpdateJobProgress(ctx context.Context, hostID, jobID, leaseToken string, progress domain.JobProgress) error {
	if leaseToken == "" {
		return ErrLeaseLost
	}
	encoded, err := json.Marshal(progress)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE jobs SET progress=?, updated_at=?
WHERE id=? AND host_id=? AND type IN ('instance.auth.codex', 'instance.hermes.update', 'instance.mcp.configure') AND status=?
  AND lease_token=? AND lease_expires_at>?`,
		encoded, now, jobID, hostID, domain.JobRunning, leaseToken, now)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) GetCodexAuthSession(ctx context.Context, instanceID, operationID string) (domain.CodexAuthSession, error) {
	return s.getCodexAuthSession(ctx, instanceID, "o.id=?", operationID)
}

func (s *Store) GetActiveCodexAuthSession(ctx context.Context, instanceID, provider string) (domain.CodexAuthSession, error) {
	if provider == "" {
		provider = "openai-codex"
	}
	return s.getCodexAuthSession(
		ctx,
		instanceID,
		"j.status IN ('PENDING','LEASED','RUNNING') AND COALESCE(NULLIF(json_extract(j.payload, '$.provider'), ''), 'openai-codex')=?",
		provider,
	)
}

func (s *Store) CancelCodexAuth(ctx context.Context, instanceID, operationID, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM operations o
JOIN jobs j ON j.operation_id=o.id
WHERE o.id=? AND o.instance_id=? AND o.type='CODEX_AUTH' AND j.type='instance.auth.codex'`,
		operationID, instanceID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrNotFound
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
UPDATE jobs SET status=?, lease_token='', lease_expires_at=NULL, updated_at=?
WHERE operation_id=? AND instance_id=? AND type='instance.auth.codex'
  AND status IN (?, ?, ?)`,
		domain.JobFailed, now, operationID, instanceID,
		domain.JobPending, domain.JobLeased, domain.JobRunning)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateChanged
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE operations SET status=?, error=?, updated_at=?
WHERE id=? AND instance_id=? AND type='CODEX_AUTH'`,
		domain.OperationFailed, reason, now, operationID, instanceID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) getCodexAuthSession(ctx context.Context, instanceID, predicate string, argument any) (domain.CodexAuthSession, error) {
	var session domain.CodexAuthSession
	var payload, progress []byte
	query := `
SELECT o.id, o.instance_id, o.status, o.error, o.created_at, o.updated_at, j.payload, j.progress
FROM operations o
JOIN jobs j ON j.operation_id=o.id
WHERE o.instance_id=? AND o.type='CODEX_AUTH' AND j.type='instance.auth.codex' AND ` + predicate + `
ORDER BY o.created_at DESC LIMIT 1`
	args := []any{instanceID}
	if argument != nil {
		args = append(args, argument)
	}
	err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&session.OperationID, &session.InstanceID, &session.Status, &session.Error,
		&session.CreatedAt, &session.UpdatedAt, &payload, &progress,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return session, ErrNotFound
	}
	if err != nil {
		return session, err
	}
	var authPayload domain.CodexAuthPayload
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &authPayload); err != nil {
			return session, fmt.Errorf("decode provider authentication payload: %w", err)
		}
	}
	session.Provider = strings.TrimSpace(authPayload.Provider)
	if session.Provider == "" {
		session.Provider = "openai-codex"
	}
	var decoded domain.JobProgress
	if len(progress) > 0 {
		if err := json.Unmarshal(progress, &decoded); err != nil {
			return session, fmt.Errorf("decode Codex authentication progress: %w", err)
		}
	}
	session.Stage = decoded.Stage
	session.VerificationURI = decoded.VerificationURI
	session.UserCode = decoded.UserCode
	if !decoded.ExpiresAt.IsZero() {
		expiresAt := decoded.ExpiresAt
		session.ExpiresAt = &expiresAt
	}
	if session.Status == domain.OperationSucceeded {
		session.Stage = "COMPLETED"
		session.VerificationURI = ""
		session.UserCode = ""
		session.ExpiresAt = nil
	} else if session.Status == domain.OperationFailed {
		session.Stage = ""
		session.VerificationURI = ""
		session.UserCode = ""
		session.ExpiresAt = nil
	}
	return session, nil
}

func (s *Store) JobMetadata(ctx context.Context, hostID, jobID, leaseToken string) (string, string, error) {
	var operationID, jobType string
	err := s.db.QueryRowContext(ctx, `
SELECT operation_id, type FROM jobs
WHERE id=? AND host_id=? AND status=? AND lease_token=? AND lease_expires_at>?`,
		jobID, hostID, domain.JobRunning, leaseToken, time.Now().UTC()).Scan(&operationID, &jobType)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrLeaseLost
	}
	return operationID, jobType, err
}

// CompletionJobMetadata returns the metadata needed to validate a completion
// request. A terminal job remains addressable by its original lease token so a
// Host Agent can safely retry after losing the first HTTP response, including
// after that lease's deadline. The recorded result hash still fences the retry.
func (s *Store) CompletionJobMetadata(ctx context.Context, hostID, jobID, leaseToken string) (string, string, string, error) {
	if leaseToken == "" {
		return "", "", "", ErrLeaseLost
	}
	var operationID, jobType, status string
	err := s.db.QueryRowContext(ctx, `
SELECT operation_id, type, status FROM jobs
WHERE id=? AND host_id=? AND lease_token=?
  AND ((status=? AND lease_expires_at>?) OR status IN (?, ?))`,
		jobID, hostID, leaseToken, domain.JobRunning, time.Now().UTC(), domain.JobSucceeded, domain.JobFailed).
		Scan(&operationID, &jobType, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", ErrLeaseLost
	}
	return operationID, jobType, status, err
}

func (s *Store) ActiveJobPayload(ctx context.Context, hostID, jobID, leaseToken, expectedType string) (json.RawMessage, error) {
	if leaseToken == "" {
		return nil, ErrLeaseLost
	}
	var payload []byte
	err := s.db.QueryRowContext(ctx, `
SELECT payload FROM jobs
WHERE id=? AND host_id=? AND type=? AND status=? AND lease_token=? AND lease_expires_at>?`,
		jobID, hostID, expectedType, domain.JobRunning, leaseToken, time.Now().UTC()).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrLeaseLost
	}
	return json.RawMessage(payload), err
}

type EncryptedReveal struct {
	Ciphertext string
	ExpiresAt  time.Time
}

// JobResultCompletionHash returns a deterministic digest of the Host Agent
// result. Secret values may contribute to the digest but are never persisted
// in plaintext.
func JobResultCompletionHash(result domain.JobResult) (string, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("encode job completion result: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func invalidJobResult(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidJobResult, message)
}

func (s *Store) CompleteJob(ctx context.Context, hostID, jobID, leaseToken string, result domain.JobResult, reveal *EncryptedReveal) error {
	completionHash, err := JobResultCompletionHash(result)
	if err != nil {
		return err
	}
	return s.CompleteJobWithHash(ctx, hostID, jobID, leaseToken, completionHash, result, reveal)
}

func (s *Store) CompleteJobWithHash(
	ctx context.Context,
	hostID, jobID, leaseToken, completionHash string,
	result domain.JobResult,
	reveal *EncryptedReveal,
) error {
	if leaseToken == "" {
		return ErrLeaseLost
	}
	decodedHash, err := hex.DecodeString(completionHash)
	if err != nil || len(decodedHash) != sha256.Size {
		return invalidJobResult("job completion hash is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	var operationID, instanceID, jobType, currentJobStatus string
	var jobPayload, storedOperationMetadata []byte
	var storedCompletionHash string
	var jobAttempts int
	if err := tx.QueryRowContext(ctx, `
SELECT j.operation_id, j.instance_id, j.type, j.status, j.payload, j.attempts, j.completion_hash, o.metadata
FROM jobs j JOIN operations o ON o.id=j.operation_id
WHERE j.id=? AND j.host_id=? AND j.lease_token=?
  AND ((j.status=? AND j.lease_expires_at>?) OR j.status IN (?, ?))`,
		jobID, hostID, leaseToken, domain.JobRunning, now, domain.JobSucceeded, domain.JobFailed).
		Scan(&operationID, &instanceID, &jobType, &currentJobStatus, &jobPayload, &jobAttempts, &storedCompletionHash, &storedOperationMetadata); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseLost
		}
		return err
	}
	if currentJobStatus == domain.JobSucceeded || currentJobStatus == domain.JobFailed {
		if len(storedCompletionHash) != sha256.Size*2 ||
			subtle.ConstantTimeCompare([]byte(storedCompletionHash), []byte(completionHash)) != 1 {
			return ErrStateChanged
		}
		// The original transaction already applied every operation and instance
		// side effect. Acknowledge only the identical result submitted under the
		// original lease token and do not replay it.
		return nil
	}
	if !domain.IsHermesProfileJob(jobType) && result.HermesProfiles != nil {
		return invalidJobResult("Hermes profile result does not match the active job")
	}
	jobStatus, operationStatus, instanceStatus := domain.JobSucceeded, domain.OperationSucceeded, statusAfterSuccess(jobType)
	errText := ""
	if !result.Success {
		jobStatus, operationStatus, instanceStatus = domain.JobFailed, domain.OperationFailed, domain.InstanceFailed
		errText = result.Error
	}
	if jobType == "instance.chat.send" {
		instanceStatus = ""
	}
	if jobType == "instance.delete" && result.Success {
		// Removing the managed runtime is only the first half of deletion. Keep
		// the durable operation and instance nonterminal until Fleet-owned
		// Cloudflare resources have been removed and verified absent.
		operationStatus = domain.OperationRunning
		instanceStatus = domain.InstanceDeleting
	}
	if jobType == "instance.runtime.repair" && !result.Success {
		// A failed bounded repair attempt must return to the desired RUNNING state
		// so a fresh observation can decide whether another attempt is safe.
		instanceStatus = domain.InstanceRunning
	}
	if jobType == "instance.recovery.create" || jobType == "instance.hermes.update" {
		if jobType == "instance.recovery.create" {
			instanceStatus = domain.InstanceStopped
		}
		if result.RecoveryPointID == "" {
			return invalidJobResult("recovery point result is missing its identity")
		}
		if result.Success && (len(result.RecoverySHA256) != 64 || result.RecoverySizeBytes < 1) {
			return invalidJobResult("successful recovery point result is missing artifact metadata")
		}
	} else if result.RecoveryPointID != "" || result.RecoverySHA256 != "" || result.RecoverySizeBytes != 0 {
		return invalidJobResult("recovery point result does not match the active job")
	}
	projectName, dataVolume, managedPath, imageID := "", "", "", ""
	var restorePayload *domain.RecoveryRestorePayload
	var upgradePayload *domain.HermesUpgradePayload
	var updatePayload *domain.HermesUpdatePayload
	var hermesProfileInventory *domain.HermesProfileInventory
	switch jobType {
	case domain.JobInspectHermesProfiles, domain.JobCreateHermesProfile, domain.JobRepairHermesProfiles,
		domain.JobActivateHermesProfile, domain.JobDeleteHermesProfile:
		instanceStatus = ""
		if result.ProjectName != "" || result.DataVolume != "" || result.ManagedPath != "" || result.ImageID != "" ||
			result.InstanceStatus != "" || result.Credentials != nil || result.ChatMessage != "" ||
			result.ChatCiphertext != "" || len(result.ChatArtifacts) != 0 || reveal != nil {
			return invalidJobResult("Hermes profile result returned unrelated metadata")
		}
		var payload domain.HermesProfileInspectPayload
		if err := json.Unmarshal(jobPayload, &payload); err != nil || payload.InstanceID != instanceID ||
			payload.Name == "" || payload.ProjectName == "" || payload.ManagedPath == "" ||
			payload.DashboardPort < 1 || payload.DashboardPort > 65535 {
			return errors.New("Hermes profile job payload is invalid")
		}
		if result.Success {
			if result.HermesProfiles == nil || result.HermesProfiles.InstanceID != payload.InstanceID ||
				!result.HermesProfiles.ObservedAt.IsZero() || domain.ValidateHermesProfileInventory(result.HermesProfiles) != nil {
				return invalidJobResult("successful Hermes profile result is incomplete or mismatched")
			}
			copy := *result.HermesProfiles
			copy.ObservedAt = now
			hermesProfileInventory = &copy
		} else if result.HermesProfiles != nil {
			return invalidJobResult("failed Hermes profile result returned an inventory")
		}
	case "instance.chat.send":
		if result.ProjectName != "" || result.DataVolume != "" || result.ManagedPath != "" || result.ImageID != "" ||
			result.InstanceStatus != "" || result.Credentials != nil {
			return invalidJobResult("chat result returned unrelated runtime metadata")
		}
		var payload domain.ChatSendPayload
		if err := json.Unmarshal(jobPayload, &payload); err != nil || payload.InstanceID != instanceID ||
			payload.SessionID == "" || payload.MessageID == "" || payload.ManagedPath == "" || payload.APIPort < 1 {
			return errors.New("chat job payload is invalid")
		}
		if result.Success && (result.ChatMessage == "" || result.ChatCiphertext == "") {
			return invalidJobResult("successful chat result is missing assistant content")
		}
		if !result.Success && (result.ChatMessage != "" || result.ChatCiphertext != "") {
			return invalidJobResult("failed chat result returned assistant content")
		}
	case "instance.provision":
		var payload domain.ProvisionPayload
		if err := json.Unmarshal(jobPayload, &payload); err != nil || payload.InstanceID != instanceID || payload.Name == "" {
			return errors.New("provision job payload is invalid")
		}
		expectedProject, expectedVolume, expectedDirectory := domain.ManagedIdentity(payload.InstanceID, payload.Name)
		hasRuntimeMetadata := result.ProjectName != "" || result.DataVolume != "" || result.ManagedPath != "" || result.ImageID != ""
		if result.Success || hasRuntimeMetadata {
			if result.ProjectName != expectedProject || result.DataVolume != expectedVolume ||
				!filepath.IsAbs(result.ManagedPath) || filepath.Clean(result.ManagedPath) != result.ManagedPath ||
				filepath.Base(result.ManagedPath) != expectedDirectory {
				return invalidJobResult("provision result does not match the reserved managed identity")
			}
		}
		if result.Success && !immutableImageIDPattern.MatchString(result.ImageID) {
			return invalidJobResult("provision result returned an invalid immutable image ID")
		}
		if !result.Success && result.ImageID != "" {
			return invalidJobResult("failed provision result returned image metadata")
		}
		projectName, dataVolume, managedPath, imageID = result.ProjectName, result.DataVolume, result.ManagedPath, result.ImageID
	case "instance.image.reconcile":
		instanceStatus = domain.InstanceStopped
		if result.ProjectName != "" || result.DataVolume != "" || result.ManagedPath != "" {
			return invalidJobResult("image reconciliation returned unrelated runtime metadata")
		}
		if result.InstanceStatus != domain.InstanceStopped {
			return invalidJobResult("image reconciliation did not preserve the stopped instance state")
		}
		if result.Success {
			if !immutableImageIDPattern.MatchString(result.ImageID) {
				return invalidJobResult("image reconciliation returned an invalid immutable image ID")
			}
			imageID = result.ImageID
		} else if result.ImageID != "" {
			return invalidJobResult("failed image reconciliation returned image metadata")
		}
	case "instance.image.repair":
		if result.ProjectName != "" || result.DataVolume != "" || result.ManagedPath != "" {
			return invalidJobResult("image repair returned unrelated runtime metadata")
		}
		var payload domain.ImageRepairPayload
		if err := json.Unmarshal(jobPayload, &payload); err != nil || payload.InstanceID != instanceID {
			return errors.New("image repair job payload is invalid")
		}
		if result.InstanceStatus != domain.InstanceRunning && result.InstanceStatus != domain.InstanceStopped && result.InstanceStatus != domain.InstanceFailed {
			return invalidJobResult("image repair returned an invalid instance status")
		}
		instanceStatus = result.InstanceStatus
		if result.Success {
			expectedStatus := domain.InstanceStopped
			if payload.Restart {
				expectedStatus = domain.InstanceRunning
			}
			if result.InstanceStatus != expectedStatus {
				return invalidJobResult("image repair did not restore the requested runtime state")
			}
			if !immutableImageIDPattern.MatchString(result.ImageID) {
				return invalidJobResult("image repair returned an invalid immutable image ID")
			}
			imageID = result.ImageID
		} else if result.ImageID != "" {
			return invalidJobResult("failed image repair returned image metadata")
		}
	case "instance.runtime.sync", "instance.runtime.configure":
		if result.ProjectName != "" || result.DataVolume != "" || result.ManagedPath != "" || result.ImageID != "" {
			return invalidJobResult("runtime synchronization returned unrelated runtime metadata")
		}
		var payload domain.RuntimeSyncPayload
		if err := json.Unmarshal(jobPayload, &payload); err != nil || payload.InstanceID != instanceID {
			return errors.New("runtime synchronization job payload is invalid")
		}
		if result.Success && result.InstanceStatus != payload.DesiredStatus {
			return invalidJobResult("runtime synchronization did not preserve the requested runtime state")
		}
		if !result.Success && result.InstanceStatus != "" &&
			result.InstanceStatus != payload.DesiredStatus && result.InstanceStatus != domain.InstanceFailed {
			return invalidJobResult("failed runtime synchronization returned an invalid instance status")
		}
		if result.InstanceStatus != "" && result.InstanceStatus != domain.InstanceRunning &&
			result.InstanceStatus != domain.InstanceStopped && result.InstanceStatus != domain.InstanceFailed {
			return invalidJobResult("runtime synchronization returned an invalid instance status")
		}
		if result.InstanceStatus != "" {
			instanceStatus = result.InstanceStatus
		}
	case "instance.messaging.configure":
		if result.ProjectName != "" || result.DataVolume != "" || result.ManagedPath != "" || result.ImageID != "" {
			return invalidJobResult("messaging configuration returned unrelated runtime metadata")
		}
		var payload domain.MessagingApplyPayload
		if err := json.Unmarshal(jobPayload, &payload); err != nil || payload.InstanceID != instanceID || payload.Revision == "" {
			return errors.New("messaging configuration job payload is invalid")
		}
		if result.Success && result.InstanceStatus != payload.DesiredStatus {
			return invalidJobResult("messaging configuration did not preserve the requested runtime state")
		}
		if result.Success &&
			result.InstanceStatus != domain.InstanceRunning && result.InstanceStatus != domain.InstanceStopped {
			return invalidJobResult("messaging configuration returned an invalid instance status")
		}
		if !result.Success &&
			result.InstanceStatus != payload.DesiredStatus && result.InstanceStatus != domain.InstanceFailed {
			return invalidJobResult("failed messaging configuration returned an invalid instance status")
		}
		instanceStatus = result.InstanceStatus
	case "instance.mcp.configure":
		if result.ProjectName != "" || result.DataVolume != "" || result.ManagedPath != "" || result.ImageID != "" {
			return invalidJobResult("MCP configuration returned unrelated runtime metadata")
		}
		var payload domain.MCPApplyPayload
		if err := json.Unmarshal(jobPayload, &payload); err != nil || payload.InstanceID != instanceID || payload.Revision == "" {
			return errors.New("MCP configuration job payload is invalid")
		}
		if result.Success && result.InstanceStatus != payload.DesiredStatus {
			return invalidJobResult("MCP configuration did not preserve the requested runtime state")
		}
		if result.Success && result.InstanceStatus != domain.InstanceRunning && result.InstanceStatus != domain.InstanceStopped {
			return invalidJobResult("MCP configuration returned an invalid instance status")
		}
		if !result.Success && result.InstanceStatus != payload.DesiredStatus && result.InstanceStatus != domain.InstanceFailed {
			return invalidJobResult("failed MCP configuration returned an invalid instance status")
		}
		instanceStatus = result.InstanceStatus
	case "instance.hermes.upgrade":
		if result.ProjectName != "" || result.DataVolume != "" || result.ManagedPath != "" {
			return invalidJobResult("Hermes update returned unrelated runtime metadata")
		}
		var payload domain.HermesUpgradePayload
		if err := json.Unmarshal(jobPayload, &payload); err != nil || payload.InstanceID != instanceID ||
			payload.TargetImage == "" || providers.ValidateImageReference(payload.TargetImage) != nil ||
			payload.Rollback.InstanceID != instanceID || payload.Rollback.Image != payload.CurrentImage ||
			payload.Rollback.ImageID != payload.CurrentImageID || !payload.Rollback.RequireImageID {
			return errors.New("Hermes update job payload is invalid")
		}
		if result.InstanceStatus != domain.InstanceStopped && result.InstanceStatus != domain.InstanceFailed {
			return invalidJobResult("Hermes update returned an invalid instance status")
		}
		instanceStatus = result.InstanceStatus
		if result.Success {
			if result.InstanceStatus != domain.InstanceStopped || !immutableImageIDPattern.MatchString(result.ImageID) {
				return invalidJobResult("Hermes update did not return a verified stopped target image")
			}
			imageID = result.ImageID
		} else if result.ImageID != "" {
			return invalidJobResult("failed Hermes update returned target image metadata")
		}
		upgradePayload = &payload
	case "instance.hermes.update":
		if result.ProjectName != "" || result.DataVolume != "" || result.ManagedPath != "" {
			return invalidJobResult("Hermes update workflow returned unrelated runtime metadata")
		}
		var payload domain.HermesUpdatePayload
		if err := json.Unmarshal(jobPayload, &payload); err != nil {
			return errors.New("Hermes update workflow payload is invalid")
		}
		upgrade := payload.Upgrade
		if upgrade.InstanceID != instanceID || payload.Backup.InstanceID != instanceID ||
			upgrade.TargetImage == "" || providers.ValidateImageReference(upgrade.TargetImage) != nil ||
			upgrade.Rollback.InstanceID != instanceID || upgrade.Rollback.Image != upgrade.CurrentImage ||
			upgrade.Rollback.ImageID != upgrade.CurrentImageID || !upgrade.Rollback.RequireImageID ||
			payload.OriginalStatus != domain.InstanceRunning && payload.OriginalStatus != domain.InstanceStopped {
			return errors.New("Hermes update workflow payload is invalid")
		}
		if result.InstanceStatus != domain.InstanceRunning && result.InstanceStatus != domain.InstanceStopped &&
			result.InstanceStatus != domain.InstanceFailed {
			return invalidJobResult("Hermes update workflow returned an invalid instance status")
		}
		instanceStatus = result.InstanceStatus
		if result.Success {
			if result.InstanceStatus != payload.OriginalStatus || !immutableImageIDPattern.MatchString(result.ImageID) {
				return invalidJobResult("Hermes update workflow did not restore the requested runtime state")
			}
			imageID = result.ImageID
		} else if result.ImageID != "" {
			if !immutableImageIDPattern.MatchString(result.ImageID) {
				return invalidJobResult("failed Hermes update workflow returned invalid target image metadata")
			}
			if result.InstanceStatus != domain.InstanceStopped && result.InstanceStatus != domain.InstanceFailed {
				return invalidJobResult("failed Hermes update workflow with target image returned an invalid instance status")
			}
			imageID = result.ImageID
		}
		updatePayload = &payload
	case "instance.recovery.restore":
		if result.ProjectName != "" || result.DataVolume != "" || result.ManagedPath != "" {
			return invalidJobResult("recovery restore returned unrelated runtime metadata")
		}
		if result.InstanceStatus != domain.InstanceStopped && result.InstanceStatus != domain.InstanceFailed {
			return invalidJobResult("recovery restore returned an invalid instance status")
		}
		if result.Success && result.InstanceStatus != domain.InstanceStopped {
			return invalidJobResult("successful recovery restore did not preserve the stopped state")
		}
		if result.Success && !immutableImageIDPattern.MatchString(result.ImageID) {
			return invalidJobResult("successful recovery restore did not return the resolved runtime image")
		}
		if !result.Success && result.ImageID != "" {
			return invalidJobResult("failed recovery restore returned runtime image metadata")
		}
		var payload domain.RecoveryRestorePayload
		if err := json.Unmarshal(jobPayload, &payload); err != nil || payload.InstanceID != instanceID ||
			!immutableImageIDPattern.MatchString(payload.ImageID) || payload.Image == "" ||
			providers.ValidateRuntimeOrPending(payload.Provider, payload.Model, payload.Reasoning, payload.ServiceTier) != nil {
			return errors.New("recovery restore job payload is invalid")
		}
		restorePayload = &payload
		instanceStatus = result.InstanceStatus
		imageID = result.ImageID
	default:
		if result.ProjectName != "" || result.DataVolume != "" || result.ManagedPath != "" || result.ImageID != "" {
			return invalidJobResult("runtime metadata result does not match the active job")
		}
	}
	if jobType == "instance.credentials.inspect" || jobType == "instance.auth.codex" || jobType == "instance.chat.send" ||
		domain.IsHermesProfileJob(jobType) {
		instanceStatus = ""
		if jobType == "instance.credentials.inspect" && reveal != nil && !result.Success {
			return invalidJobResult("failed credential inspection cannot contain a reveal")
		}
		if jobType == "instance.auth.codex" && (result.Credentials != nil ||
			result.InstanceStatus != "" || result.ProjectName != "" || result.DataVolume != "" || result.ManagedPath != "" || result.ImageID != "") {
			return invalidJobResult("Codex authentication returned unrelated instance metadata")
		}
	} else if reveal != nil {
		return invalidJobResult("credential reveal is only valid for an inspection job")
	}
	if jobType != "instance.image.reconcile" && jobType != "instance.image.repair" && jobType != "instance.runtime.sync" &&
		jobType != "instance.runtime.configure" && jobType != "instance.messaging.configure" && jobType != "instance.mcp.configure" &&
		jobType != "instance.recovery.restore" && jobType != "instance.hermes.upgrade" &&
		jobType != "instance.hermes.update" && result.InstanceStatus != "" {
		return invalidJobResult("instance status result does not match the active job")
	}
	jobUpdate, err := tx.ExecContext(ctx, `
UPDATE jobs SET status=?, completion_hash=?, updated_at=?
WHERE id=? AND host_id=? AND status=? AND lease_token=? AND lease_expires_at>?`,
		jobStatus, completionHash, now, jobID, hostID, domain.JobRunning, leaseToken, now)
	if err != nil {
		return err
	}
	if count, _ := jobUpdate.RowsAffected(); count != 1 {
		return ErrLeaseLost
	}
	if hermesProfileInventory != nil {
		encodedProfiles, err := json.Marshal(hermesProfileInventory.Profiles)
		if err != nil {
			return fmt.Errorf("encode Hermes profile inventory: %w", err)
		}
		inventoryUpdate, err := tx.ExecContext(ctx, `
	INSERT INTO hermes_profile_inventories (instance_id, profiles, observed_at)
	SELECT id, ?, ? FROM instances WHERE id=?
	ON CONFLICT(instance_id) DO UPDATE SET profiles=excluded.profiles, observed_at=excluded.observed_at`,
			encodedProfiles, hermesProfileInventory.ObservedAt, hermesProfileInventory.InstanceID)
		if err != nil {
			return fmt.Errorf("record Hermes profile inventory: %w", err)
		}
		if count, _ := inventoryUpdate.RowsAffected(); count != 1 {
			return ErrStateChanged
		}
	}
	metadata := make(map[string]any)
	if len(storedOperationMetadata) > 0 {
		_ = json.Unmarshal(storedOperationMetadata, &metadata)
	}
	metadata["attempt"] = jobAttempts
	if result.ImageID != "" {
		metadata["image_digest"] = result.ImageID
	}
	if result.RecoveryPointID != "" {
		metadata["backup_id"] = result.RecoveryPointID
	}
	if result.RecoverySHA256 != "" {
		metadata["backup_sha256"] = result.RecoverySHA256
	}
	if hermesProfileInventory != nil {
		metadata["profile_count"] = len(hermesProfileInventory.Profiles)
	}
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode operation result metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE operations SET status=?, error=?, metadata=?, updated_at=? WHERE id=?`, operationStatus, errText, encodedMetadata, now, operationID); err != nil {
		return err
	}
	if jobType != "instance.credentials.inspect" && jobType != "instance.auth.codex" &&
		jobType != "instance.chat.send" && !domain.IsHermesProfileJob(jobType) {
		if _, err := tx.ExecContext(ctx, `
UPDATE instances SET status=COALESCE(NULLIF(?, ''), status), project_name=COALESCE(NULLIF(?, ''), project_name),
  data_volume=COALESCE(NULLIF(?, ''), data_volume), managed_path=COALESCE(NULLIF(?, ''), managed_path),
  image_id=COALESCE(NULLIF(?, ''), image_id), last_error=?, updated_at=? WHERE id=?`,
			instanceStatus, projectName, dataVolume, managedPath, imageID, errText, now, instanceID); err != nil {
			return err
		}
	}
	if jobType == "instance.chat.send" {
		var payload domain.ChatSendPayload
		if err := json.Unmarshal(jobPayload, &payload); err != nil {
			return errors.New("chat job payload is invalid")
		}
		if err := finishChatMessage(ctx, tx, operationID, payload, result, now); err != nil {
			return err
		}
	}
	if jobType == "instance.runtime.repair" {
		if result.Success {
			if _, err := tx.ExecContext(ctx, `
UPDATE runtime_remediation_state
SET status='VERIFYING', last_error='', updated_at=?
WHERE instance_id=? AND status='QUEUED'`, now, instanceID); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `
UPDATE runtime_remediation_state
SET status=CASE
      WHEN total_attempts >= ? THEN 'EXHAUSTED'
      WHEN attempt_in_phase >= ? THEN 'COOLDOWN'
      ELSE 'WAITING'
    END,
    last_error=?, updated_at=?
WHERE instance_id=? AND status='QUEUED'`,
				runtimeRemediationAttemptsPerPhase*runtimeRemediationMaxPhases,
				runtimeRemediationAttemptsPerPhase,
				errText, now, instanceID,
			); err != nil {
				return err
			}
		}
	}
	if jobType == "instance.recovery.restore" && result.Success && restorePayload != nil {
		model, reasoning, serviceTier := restorePayload.Model, restorePayload.Reasoning, restorePayload.ServiceTier
		if !restorePayload.CodexConfigured {
			model, reasoning, serviceTier = "", "", ""
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE instances SET image=?, image_id=?, provider=?, model=?, reasoning=?, service_tier=?, codex_configured=?, updated_at=?
		WHERE id=?`, restorePayload.Image, result.ImageID, restorePayload.Provider, model,
			reasoning, serviceTier, restorePayload.CodexConfigured, now, instanceID); err != nil {
			return err
		}
	}
	if jobType == "instance.hermes.upgrade" && result.Success && upgradePayload != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE instances SET image=?, updated_at=? WHERE id=?`, upgradePayload.TargetImage, now, instanceID); err != nil {
			return err
		}
	}
	if jobType == "instance.hermes.update" && updatePayload != nil && result.ImageID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE instances SET image=?, updated_at=? WHERE id=?`, updatePayload.Upgrade.TargetImage, now, instanceID); err != nil {
			return err
		}
	}
	if jobType == "instance.runtime.configure" && result.Success {
		var payload domain.RuntimeSyncPayload
		if err := json.Unmarshal(jobPayload, &payload); err != nil || payload.InstanceID != instanceID {
			return errors.New("Codex configuration job payload is invalid")
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE instances SET provider=?, model=?, reasoning=?, service_tier=?, codex_configured=1, updated_at=? WHERE id=?`,
			payload.Provider, payload.Model, payload.Reasoning, payload.ServiceTier, now, instanceID); err != nil {
			return err
		}
	}
	if jobType == "instance.messaging.configure" {
		var payload domain.MessagingApplyPayload
		if err := json.Unmarshal(jobPayload, &payload); err != nil || payload.InstanceID != instanceID {
			return errors.New("messaging configuration job payload is invalid")
		}
		status := "FAILED"
		appliedRevision := ""
		var appliedAt any
		if result.Success {
			status = "APPLIED"
			appliedRevision = payload.Revision
			appliedAt = now
		}
		update, err := tx.ExecContext(ctx, `
UPDATE instance_messaging_configs
SET status=?, applied_revision=CASE WHEN ?<>'' THEN ? ELSE applied_revision END,
    last_error=?, updated_at=?, applied_at=CASE WHEN ? IS NOT NULL THEN ? ELSE applied_at END
WHERE instance_id=? AND desired_revision=?`,
			status, appliedRevision, appliedRevision, errText, now, appliedAt, appliedAt, instanceID, payload.Revision)
		if err != nil {
			return err
		}
		if count, _ := update.RowsAffected(); count != 1 {
			return ErrStateChanged
		}
	}
	if jobType == "instance.mcp.configure" {
		var payload domain.MCPApplyPayload
		if err := json.Unmarshal(jobPayload, &payload); err != nil || payload.InstanceID != instanceID {
			return errors.New("MCP configuration job payload is invalid")
		}
		status := "FAILED"
		appliedRevision := ""
		var appliedAt any
		if result.Success {
			status = "APPLIED"
			appliedRevision = payload.Revision
			appliedAt = now
		}
		update, err := tx.ExecContext(ctx, `
UPDATE instance_mcp_configs
SET status=?, applied_revision=CASE WHEN ?<>'' THEN ? ELSE applied_revision END,
    last_error=?, updated_at=?, applied_at=CASE WHEN ? IS NOT NULL THEN ? ELSE applied_at END
WHERE instance_id=? AND desired_revision=?`,
			status, appliedRevision, appliedRevision, errText, now, appliedAt, appliedAt, instanceID, payload.Revision)
		if err != nil {
			return err
		}
		if count, _ := update.RowsAffected(); count != 1 {
			return ErrStateChanged
		}
	}
	if jobType == "instance.delete" && result.Success {
		if _, err := tx.ExecContext(ctx, `DELETE FROM observation_requests WHERE instance_id=?`, instanceID); err != nil {
			return err
		}
	}
	if reveal != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM credential_reveals WHERE expires_at <= ?`, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO credential_reveals (operation_id, ciphertext, expires_at, created_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(operation_id) DO UPDATE SET ciphertext=excluded.ciphertext, expires_at=excluded.expires_at`,
			operationID, reveal.Ciphertext, reveal.ExpiresAt, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func newLeaseToken() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate job lease token: %w", err)
	}
	return hex.EncodeToString(token[:]), nil
}

func (s *Store) GetCredentialReveal(ctx context.Context, operationID string, now time.Time) (string, time.Time, error) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM credential_reveals WHERE expires_at <= ?`, now); err != nil {
		return "", time.Time{}, err
	}
	var ciphertext string
	var expiresAt time.Time
	err := s.db.QueryRowContext(ctx, `
SELECT ciphertext, expires_at FROM credential_reveals WHERE operation_id=? AND expires_at>?`, operationID, now).
		Scan(&ciphertext, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, ErrNotFound
	}
	return ciphertext, expiresAt, err
}

func statusAfterSuccess(jobType string) string {
	switch jobType {
	case "instance.stop":
		return domain.InstanceStopped
	case "instance.recovery.create":
		return domain.InstanceStopped
	case "instance.recovery.restore":
		return domain.InstanceStopped
	case "instance.image.reconcile":
		return domain.InstanceStopped
	case "instance.hermes.upgrade":
		return domain.InstanceStopped
	case "instance.image.repair":
		return domain.InstanceReconciling
	default:
		return domain.InstanceRunning
	}
}

func (s *Store) ListPendingInstanceDeletions(ctx context.Context, limit int) ([]PendingInstanceDeletion, error) {
	if limit <= 0 {
		return []PendingInstanceDeletion{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT i.id, o.id
FROM instances i
JOIN operations o ON o.instance_id=i.id
JOIN jobs j ON j.operation_id=o.id
WHERE i.status=?
  AND o.type='DELETE'
  AND o.status IN (?, ?, ?)
  AND j.type='instance.delete'
  AND j.status=?
ORDER BY o.created_at, o.id
LIMIT ?`, domain.InstanceDeleting, domain.OperationPending, domain.OperationRunning, domain.OperationFailed, domain.JobSucceeded, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending instance deletion cleanups: %w", err)
	}
	defer rows.Close()
	items := make([]PendingInstanceDeletion, 0)
	for rows.Next() {
		var item PendingInstanceDeletion
		if err := rows.Scan(&item.InstanceID, &item.OperationID); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) PendingInstanceDeletion(ctx context.Context, instanceID string) (PendingInstanceDeletion, error) {
	var item PendingInstanceDeletion
	err := s.db.QueryRowContext(ctx, `
SELECT i.id, o.id
FROM instances i
JOIN operations o ON o.instance_id=i.id
JOIN jobs j ON j.operation_id=o.id
WHERE i.id=?
  AND i.status=?
  AND o.type='DELETE'
	  AND o.status IN (?, ?, ?)
  AND j.type='instance.delete'
  AND j.status=?
ORDER BY o.created_at DESC, o.id DESC
LIMIT 1`, instanceID, domain.InstanceDeleting, domain.OperationPending, domain.OperationRunning, domain.OperationFailed, domain.JobSucceeded).
		Scan(&item.InstanceID, &item.OperationID)
	if errors.Is(err, sql.ErrNoRows) {
		return item, ErrNotFound
	}
	return item, err
}

func (s *Store) UpdateInstanceDeletionCleanup(
	ctx context.Context,
	item PendingInstanceDeletion,
	progress domain.JobProgress,
	cleanupErr string,
	completed bool,
	at time.Time,
) error {
	encoded, err := json.Marshal(progress)
	if err != nil {
		return fmt.Errorf("encode instance deletion cleanup progress: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	operationStatus := domain.OperationRunning
	instanceStatus := domain.InstanceDeleting
	if completed {
		operationStatus = domain.OperationSucceeded
		instanceStatus = domain.InstanceDeleted
		cleanupErr = ""
	} else if cleanupErr != "" {
		operationStatus = domain.OperationFailed
	}
	operationUpdate, err := tx.ExecContext(ctx, `
UPDATE operations
SET status=?, progress=?, error=?, updated_at=?
WHERE id=? AND instance_id=? AND type='DELETE' AND status IN (?, ?, ?)`,
		operationStatus, encoded, cleanupErr, at, item.OperationID, item.InstanceID,
		domain.OperationPending, domain.OperationRunning, domain.OperationFailed)
	if err != nil {
		return fmt.Errorf("update instance deletion operation: %w", err)
	}
	if count, _ := operationUpdate.RowsAffected(); count != 1 {
		return ErrStateChanged
	}
	instanceUpdate, err := tx.ExecContext(ctx, `
UPDATE instances
SET status=?, last_error=?, updated_at=?
WHERE id=? AND status=?`, instanceStatus, cleanupErr, at, item.InstanceID, domain.InstanceDeleting)
	if err != nil {
		return fmt.Errorf("update instance deletion state: %w", err)
	}
	if count, _ := instanceUpdate.RowsAffected(); count != 1 {
		return ErrStateChanged
	}
	return tx.Commit()
}

type OperationCursor struct {
	CreatedAt time.Time
	ID        string
}

type OperationPage struct {
	Items      []domain.Operation
	NextCursor *OperationCursor
}

func (s *Store) ListOperations(ctx context.Context, limit int) ([]domain.Operation, error) {
	page, err := s.ListOperationsPage(ctx, limit, nil)
	return page.Items, err
}

func (s *Store) ListOperationsPage(ctx context.Context, limit int, cursor *OperationCursor) (OperationPage, error) {
	if limit <= 0 {
		return OperationPage{Items: []domain.Operation{}}, nil
	}
	query := `
SELECT o.id, COALESCE(o.instance_id, ''), o.workflow_id, o.actor, o.type, o.status, o.summary, o.metadata,
       o.error, o.created_at, o.updated_at,
       CASE WHEN CAST(o.progress AS TEXT) <> '{}' THEN o.progress ELSE COALESCE((
         SELECT j.progress FROM jobs j WHERE j.operation_id=o.id
         ORDER BY j.updated_at DESC, j.id DESC LIMIT 1
       ), '{}') END
FROM operations o`
	args := make([]any, 0, 4)
	if cursor != nil {
		if cursor.CreatedAt.IsZero() || cursor.ID == "" {
			return OperationPage{}, errors.New("operation cursor is incomplete")
		}
		query += `
WHERE o.created_at < ? OR (o.created_at = ? AND o.id < ?)`
		args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	query += `
ORDER BY o.created_at DESC, o.id DESC LIMIT ?`
	fetchLimit := limit + 1
	if fetchLimit <= limit {
		fetchLimit = limit
	}
	args = append(args, fetchLimit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return OperationPage{}, err
	}
	defer rows.Close()
	items := make([]domain.Operation, 0, limit)
	for rows.Next() {
		var op domain.Operation
		var metadata, progress []byte
		if err := rows.Scan(&op.ID, &op.InstanceID, &op.WorkflowID, &op.Actor, &op.Type, &op.Status, &op.Summary, &metadata, &op.Error, &op.CreatedAt, &op.UpdatedAt, &progress); err != nil {
			return OperationPage{}, err
		}
		op.Metadata = json.RawMessage(metadata)
		if len(progress) > 0 && string(progress) != "{}" {
			var decoded domain.JobProgress
			if err := json.Unmarshal(progress, &decoded); err != nil {
				return OperationPage{}, fmt.Errorf("decode operation progress: %w", err)
			}
			op.Progress = &decoded
		}
		items = append(items, op)
	}
	if err := rows.Err(); err != nil {
		return OperationPage{}, err
	}
	page := OperationPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = &OperationCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

func (s *Store) GetOperation(ctx context.Context, id string) (domain.Operation, error) {
	var operation domain.Operation
	var metadata, progress []byte
	err := s.db.QueryRowContext(ctx, `
SELECT o.id, COALESCE(o.instance_id, ''), o.workflow_id, o.actor, o.type, o.status, o.summary, o.metadata,
       o.error, o.created_at, o.updated_at,
       CASE WHEN CAST(o.progress AS TEXT) <> '{}' THEN o.progress ELSE COALESCE((
         SELECT j.progress FROM jobs j WHERE j.operation_id=o.id
         ORDER BY j.updated_at DESC, j.id DESC LIMIT 1
       ), '{}') END
FROM operations o
WHERE o.id=?`, id).Scan(&operation.ID, &operation.InstanceID, &operation.WorkflowID, &operation.Actor,
		&operation.Type, &operation.Status, &operation.Summary, &metadata, &operation.Error,
		&operation.CreatedAt, &operation.UpdatedAt, &progress)
	if errors.Is(err, sql.ErrNoRows) {
		return operation, ErrNotFound
	}
	operation.Metadata = json.RawMessage(metadata)
	if len(progress) > 0 && string(progress) != "{}" {
		var decoded domain.JobProgress
		if err := json.Unmarshal(progress, &decoded); err != nil {
			return operation, fmt.Errorf("decode operation progress: %w", err)
		}
		operation.Progress = &decoded
	}
	return operation, err
}
