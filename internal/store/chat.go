package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func (s *Store) CreateChatSession(ctx context.Context, session domain.ChatSession) error {
	result, err := s.db.ExecContext(ctx, `
INSERT INTO chat_sessions (id, instance_id, title, model, reasoning, service_tier, status, last_error, created_at, updated_at)
SELECT ?, id, ?, model, reasoning, service_tier, ?, '', ?, ? FROM instances WHERE id=? AND status=?`,
		session.ID, session.Title, domain.ChatSessionActive, session.CreatedAt, session.UpdatedAt,
		session.InstanceID, domain.InstanceRunning)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateChanged
	}
	return nil
}

func (s *Store) ListChatSessions(ctx context.Context) ([]domain.ChatSession, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT s.id, s.instance_id, i.name, s.title, s.model, s.reasoning, s.service_tier, s.status, s.last_error,
       (SELECT COUNT(*) FROM chat_messages WHERE session_id=s.id),
       COALESCE((SELECT id FROM chat_messages WHERE session_id=s.id ORDER BY created_at DESC, id DESC LIMIT 1), ''),
       COALESCE((SELECT role FROM chat_messages WHERE session_id=s.id ORDER BY created_at DESC, id DESC LIMIT 1), ''),
       COALESCE((SELECT ciphertext FROM chat_messages WHERE session_id=s.id ORDER BY created_at DESC, id DESC LIMIT 1), ''),
       (SELECT created_at FROM chat_messages WHERE session_id=s.id ORDER BY created_at DESC, id DESC LIMIT 1),
       EXISTS(SELECT 1 FROM chat_messages WHERE session_id=s.id AND role='user' AND status=?),
       COALESCE((SELECT MAX(id) FROM chat_events WHERE session_id=s.id), 0),
       s.created_at, s.updated_at
FROM chat_sessions s
JOIN instances i ON i.id=s.instance_id
ORDER BY s.updated_at DESC, s.id DESC`, domain.ChatMessagePending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := []domain.ChatSession{}
	for rows.Next() {
		var session domain.ChatSession
		var lastMessageAt sql.NullTime
		var responseInProgress int
		if err := rows.Scan(&session.ID, &session.InstanceID, &session.InstanceName, &session.Title,
			&session.Model, &session.Reasoning, &session.ServiceTier, &session.Status, &session.LastError, &session.MessageCount, &session.LastMessageID,
			&session.LastMessageRole, &session.LastMessageCiphertext, &lastMessageAt, &responseInProgress,
			&session.LastEventID, &session.CreatedAt, &session.UpdatedAt); err != nil {
			return nil, err
		}
		if lastMessageAt.Valid {
			session.LastMessageAt = &lastMessageAt.Time
		}
		session.ResponseInProgress = responseInProgress != 0
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (s *Store) GetChatSession(ctx context.Context, sessionID string) (domain.ChatSession, error) {
	var session domain.ChatSession
	err := s.db.QueryRowContext(ctx, `
SELECT s.id, s.instance_id, i.name, s.title, s.model, s.reasoning, s.service_tier, s.status, s.last_error,
       (SELECT COUNT(*) FROM chat_messages WHERE session_id=s.id), s.created_at, s.updated_at
FROM chat_sessions s JOIN instances i ON i.id=s.instance_id WHERE s.id=?`, sessionID).
		Scan(&session.ID, &session.InstanceID, &session.InstanceName, &session.Title,
			&session.Model, &session.Reasoning, &session.ServiceTier, &session.Status,
			&session.LastError, &session.MessageCount, &session.CreatedAt, &session.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return session, ErrNotFound
	}
	return session, err
}

func (s *Store) UpdateChatSessionConfiguration(
	ctx context.Context,
	sessionID, model, reasoning, serviceTier string,
	updatedAt time.Time,
) (domain.ChatSession, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE chat_sessions
SET model=?, reasoning=?, service_tier=?, updated_at=?
WHERE id=? AND status=?
  AND NOT EXISTS (
    SELECT 1 FROM chat_messages
    WHERE session_id=chat_sessions.id AND role='user' AND status=?
  )`, model, reasoning, serviceTier, updatedAt, sessionID, domain.ChatSessionActive, domain.ChatMessagePending)
	if err != nil {
		return domain.ChatSession{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		if _, err := s.GetChatSession(ctx, sessionID); errors.Is(err, ErrNotFound) {
			return domain.ChatSession{}, ErrNotFound
		} else if err != nil {
			return domain.ChatSession{}, err
		}
		return domain.ChatSession{}, ErrStateChanged
	}
	return s.GetChatSession(ctx, sessionID)
}

// DeleteChatSession removes one isolated chat transcript and fences any Host
// Agent work that was still processing for that session.
func (s *Store) DeleteChatSession(ctx context.Context, sessionID string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var instanceID string
	if err := tx.QueryRowContext(ctx, `SELECT instance_id FROM chat_sessions WHERE id=?`, sessionID).Scan(&instanceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	rows, err := tx.QueryContext(ctx, `
SELECT DISTINCT operation_id FROM chat_messages
WHERE session_id=? AND operation_id IS NOT NULL`, sessionID)
	if err != nil {
		return "", err
	}
	operationIDs := []string{}
	for rows.Next() {
		var operationID string
		if err := rows.Scan(&operationID); err != nil {
			rows.Close()
			return "", err
		}
		operationIDs = append(operationIDs, operationID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	if err := rows.Close(); err != nil {
		return "", err
	}

	// Deleting the jobs first invalidates every outstanding lease before the
	// transcript disappears. Late progress or completion calls are rejected.
	for _, operationID := range operationIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE operation_id=?`, operationID); err != nil {
			return "", err
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM chat_sessions WHERE id=?`, sessionID)
	if err != nil {
		return "", err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return "", ErrNotFound
	}
	for _, operationID := range operationIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM operations WHERE id=? AND type='CHAT_MESSAGE'`, operationID); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return instanceID, nil
}

// reconcileOrphanedChatWork removes legacy chat jobs and operation rows whose
// transcript was deleted without first fencing its Host Agent work. Current
// deletion is transactional; this startup repair clears durable state created
// by older control-plane builds and invalidates any late lease callbacks.
func (s *Store) reconcileOrphanedChatWork(ctx context.Context) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
DELETE FROM jobs
WHERE type='instance.chat.send'
  AND EXISTS (
    SELECT 1 FROM operations o
    WHERE o.id=jobs.operation_id AND o.type='CHAT_MESSAGE'
  )
  AND NOT EXISTS (
    SELECT 1 FROM chat_messages m
    WHERE m.operation_id=jobs.operation_id
  )
  AND NOT EXISTS (
    SELECT 1 FROM chat_events e
    WHERE e.operation_id=jobs.operation_id
  )`); err != nil {
		return 0, fmt.Errorf("fence orphaned chat jobs: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
DELETE FROM operations
WHERE type='CHAT_MESSAGE'
  AND NOT EXISTS (
    SELECT 1 FROM chat_messages m
    WHERE m.operation_id=operations.id
  )
  AND NOT EXISTS (
    SELECT 1 FROM chat_events e
    WHERE e.operation_id=operations.id
  )
  AND NOT EXISTS (
    SELECT 1 FROM jobs j
    WHERE j.operation_id=operations.id
  )`)
	if err != nil {
		return 0, fmt.Errorf("remove orphaned chat operations: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count orphaned chat operations: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit orphaned chat reconciliation: %w", err)
	}
	return count, nil
}

// GetChatThreadSnapshot reads the transcript, active response events, and
// durable event cursor from one SQLite snapshot. A client can subscribe after
// LastCursor without a gap even when a chat completes during connection setup.
func (s *Store) GetChatThreadSnapshot(ctx context.Context, sessionID string) (domain.ChatThread, []domain.ChatEvent, error) {
	thread := domain.ChatThread{ProtocolVersion: domain.ChatProtocolVersion, Messages: []domain.ChatMessage{}, Events: []domain.ChatEvent{}}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return thread, nil, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `
SELECT s.id, s.instance_id, i.name, s.title, s.model, s.reasoning, s.service_tier, s.status, s.last_error,
       (SELECT COUNT(*) FROM chat_messages WHERE session_id=s.id), s.created_at, s.updated_at
FROM chat_sessions s JOIN instances i ON i.id=s.instance_id WHERE s.id=?`, sessionID).
		Scan(&thread.Session.ID, &thread.Session.InstanceID, &thread.Session.InstanceName, &thread.Session.Title,
			&thread.Session.Model, &thread.Session.Reasoning, &thread.Session.ServiceTier,
			&thread.Session.Status, &thread.Session.LastError, &thread.Session.MessageCount,
			&thread.Session.CreatedAt, &thread.Session.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return thread, nil, ErrNotFound
		}
		return thread, nil, err
	}
	rows, err := tx.QueryContext(ctx, `
SELECT id, session_id, COALESCE(operation_id, ''), role, ciphertext, status, error, created_at, updated_at
FROM chat_messages WHERE session_id=? ORDER BY created_at, id`, sessionID)
	if err != nil {
		return thread, nil, err
	}
	for rows.Next() {
		var message domain.ChatMessage
		if err := rows.Scan(&message.ID, &message.SessionID, &message.OperationID, &message.Role,
			&message.Ciphertext, &message.Status, &message.Error, &message.CreatedAt, &message.UpdatedAt); err != nil {
			rows.Close()
			return thread, nil, err
		}
		thread.Messages = append(thread.Messages, message)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return thread, nil, err
	}
	if err := rows.Close(); err != nil {
		return thread, nil, err
	}
	richRows, err := tx.QueryContext(ctx, `
SELECT id, session_id, operation_id, sequence, type, ciphertext, content_hash, created_at
FROM chat_events
WHERE session_id=? AND type IN (?, ?)
ORDER BY id`, sessionID, domain.ChatEventActivity, domain.ChatEventArtifact)
	if err != nil {
		return thread, nil, err
	}
	for richRows.Next() {
		var event domain.ChatEvent
		if err := richRows.Scan(&event.ID, &event.SessionID, &event.OperationID, &event.Sequence,
			&event.Type, &event.Ciphertext, &event.ContentHash, &event.CreatedAt); err != nil {
			richRows.Close()
			return thread, nil, err
		}
		event.Version = domain.ChatProtocolVersion
		thread.Events = append(thread.Events, event)
	}
	if err := richRows.Err(); err != nil {
		richRows.Close()
		return thread, nil, err
	}
	if err := richRows.Close(); err != nil {
		return thread, nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM chat_events WHERE session_id=?`, sessionID).
		Scan(&thread.LastCursor); err != nil {
		return thread, nil, err
	}
	var activeOperationID string
	err = tx.QueryRowContext(ctx, `
SELECT operation_id FROM chat_messages
WHERE session_id=? AND role='user' AND status=? AND operation_id IS NOT NULL
ORDER BY created_at, id LIMIT 1`, sessionID, domain.ChatMessagePending).Scan(&activeOperationID)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return thread, nil, err
		}
		return thread, nil, nil
	}
	if err != nil {
		return thread, nil, err
	}
	thread.ActiveResponse = &domain.ChatActiveResponse{OperationID: activeOperationID, State: domain.ChatEventQueued}
	eventRows, err := tx.QueryContext(ctx, `
SELECT id, session_id, operation_id, sequence, type, ciphertext, content_hash, created_at
FROM chat_events WHERE session_id=? AND operation_id=? AND id<=? ORDER BY sequence, id`,
		sessionID, activeOperationID, thread.LastCursor)
	if err != nil {
		return thread, nil, err
	}
	activeEvents := []domain.ChatEvent{}
	for eventRows.Next() {
		var event domain.ChatEvent
		if err := eventRows.Scan(&event.ID, &event.SessionID, &event.OperationID, &event.Sequence,
			&event.Type, &event.Ciphertext, &event.ContentHash, &event.CreatedAt); err != nil {
			eventRows.Close()
			return thread, nil, err
		}
		event.Version = domain.ChatProtocolVersion
		activeEvents = append(activeEvents, event)
		if event.Sequence > thread.ActiveResponse.LastSequence {
			thread.ActiveResponse.LastSequence = event.Sequence
		}
		if event.Type == domain.ChatEventStarted || event.Type == domain.ChatEventDelta ||
			event.Type == domain.ChatEventActivity || event.Type == domain.ChatEventArtifact {
			thread.ActiveResponse.State = domain.ChatEventStarted
		}
	}
	if err := eventRows.Err(); err != nil {
		eventRows.Close()
		return thread, nil, err
	}
	if err := eventRows.Close(); err != nil {
		return thread, nil, err
	}
	if err := tx.Commit(); err != nil {
		return thread, nil, err
	}
	return thread, activeEvents, nil
}

func (s *Store) ListChatMessages(ctx context.Context, sessionID string) ([]domain.ChatMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, session_id, COALESCE(operation_id, ''), role, ciphertext, status, error, created_at, updated_at
FROM chat_messages WHERE session_id=? ORDER BY created_at, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := []domain.ChatMessage{}
	for rows.Next() {
		var message domain.ChatMessage
		if err := rows.Scan(&message.ID, &message.SessionID, &message.OperationID, &message.Role,
			&message.Ciphertext, &message.Status, &message.Error, &message.CreatedAt, &message.UpdatedAt); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func (s *Store) QueueChatMessage(
	ctx context.Context,
	sessionID, messageID, ciphertext string,
	operation domain.Operation,
	job domain.Job,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var instanceID, instanceName, instanceStatus, hostID, projectName, managedPath, provider, model, reasoning, serviceTier string
	var apiPort int
	if err := tx.QueryRowContext(ctx, `
SELECT s.instance_id, i.name, i.status, i.host_id, i.project_name, i.managed_path, i.api_port,
	   i.provider, s.model, s.reasoning, s.service_tier
FROM chat_sessions s JOIN instances i ON i.id=s.instance_id
WHERE s.id=? AND s.status=?`, sessionID, domain.ChatSessionActive).
		Scan(&instanceID, &instanceName, &instanceStatus, &hostID, &projectName, &managedPath, &apiPort, &provider, &model, &reasoning, &serviceTier); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var payload domain.ChatSendPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil ||
		job.Type != "instance.chat.send" || job.OperationID != operation.ID ||
		instanceID != operation.InstanceID || instanceID != job.InstanceID || instanceID != payload.InstanceID ||
		job.HostID != hostID || payload.InstanceName != instanceName || payload.ProjectName != projectName ||
		payload.SessionID != sessionID || payload.MessageID != messageID ||
		payload.ManagedPath != managedPath || payload.APIPort != apiPort || payload.Provider != provider || payload.Model != model ||
		payload.Reasoning != reasoning || payload.ServiceTier != serviceTier || instanceStatus != domain.InstanceRunning {
		return ErrStateChanged
	}
	if err := insertOperationAndJob(ctx, tx, operation, job); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO chat_messages (id, session_id, operation_id, role, ciphertext, status, error, created_at, updated_at)
VALUES (?, ?, ?, 'user', ?, ?, '', ?, ?)`, messageID, sessionID, operation.ID, ciphertext,
		domain.ChatMessagePending, operation.CreatedAt, operation.UpdatedAt); err != nil {
		return fmt.Errorf("insert chat message: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_sessions SET last_error='', updated_at=? WHERE id=?`, operation.CreatedAt, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO chat_events (session_id, operation_id, sequence, type, created_at)
VALUES (?, ?, 0, ?, ?)`, sessionID, operation.ID, domain.ChatEventQueued, operation.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AppendChatEvent(
	ctx context.Context,
	hostID, jobID, leaseToken string,
	event domain.ChatEvent,
) error {
	if event.Sequence < 1 || event.OperationID == "" || event.SessionID == "" {
		return ErrStateChanged
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var operationID string
	var payloadData []byte
	if err := tx.QueryRowContext(ctx, `
SELECT operation_id, payload FROM jobs
WHERE id=? AND host_id=? AND type='instance.chat.send' AND status=?
  AND lease_token=? AND lease_expires_at>?`, jobID, hostID, domain.JobRunning, leaseToken, time.Now().UTC()).
		Scan(&operationID, &payloadData); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseLost
		}
		return err
	}
	var payload domain.ChatSendPayload
	if json.Unmarshal(payloadData, &payload) != nil || operationID != event.OperationID || payload.SessionID != event.SessionID {
		return ErrStateChanged
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO chat_events (session_id, operation_id, sequence, type, ciphertext, content_hash, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(operation_id, sequence) DO NOTHING`, event.SessionID, event.OperationID, event.Sequence,
		event.Type, event.Ciphertext, event.ContentHash, event.CreatedAt)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		var eventType, contentHash string
		if err := tx.QueryRowContext(ctx, `
SELECT type, content_hash FROM chat_events WHERE operation_id=? AND sequence=?`, event.OperationID, event.Sequence).
			Scan(&eventType, &contentHash); err != nil {
			return err
		}
		if eventType != event.Type || contentHash != event.ContentHash {
			return ErrStateChanged
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE chat_sessions SET updated_at=? WHERE id=?`, event.CreatedAt, event.SessionID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListChatEvents(ctx context.Context, sessionID string, afterID int64, limit int) ([]domain.ChatEvent, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, session_id, operation_id, sequence, type, ciphertext, content_hash, created_at
FROM chat_events WHERE session_id=? AND id>? ORDER BY id LIMIT ?`, sessionID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []domain.ChatEvent{}
	for rows.Next() {
		var event domain.ChatEvent
		if err := rows.Scan(&event.ID, &event.SessionID, &event.OperationID, &event.Sequence,
			&event.Type, &event.Ciphertext, &event.ContentHash, &event.CreatedAt); err != nil {
			return nil, err
		}
		event.Version = domain.ChatProtocolVersion
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) CancelActiveChat(ctx context.Context, sessionID, reason string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var jobID, operationID, messageID string
	if err := tx.QueryRowContext(ctx, `
SELECT j.id, j.operation_id, m.id
FROM jobs j
JOIN chat_messages m ON m.operation_id=j.operation_id AND m.role='user'
WHERE m.session_id=? AND m.status=? AND j.type='instance.chat.send' AND j.status IN (?, ?, ?)
ORDER BY CASE j.status WHEN ? THEN 0 WHEN ? THEN 1 ELSE 2 END, j.created_at, j.id LIMIT 1`,
		sessionID, domain.ChatMessagePending,
		domain.JobPending, domain.JobLeased, domain.JobRunning,
		domain.JobRunning, domain.JobLeased).Scan(&jobID, &operationID, &messageID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE jobs SET status=?, lease_token='', lease_expires_at=NULL, updated_at=?
WHERE id=? AND status IN (?, ?, ?)`, domain.JobFailed, now, jobID,
		domain.JobPending, domain.JobLeased, domain.JobRunning); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE operations SET status=?, error=?, updated_at=? WHERE id=?`,
		domain.OperationFailed, reason, now, operationID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_messages SET status=?, error=?, updated_at=? WHERE id=?`,
		domain.ChatMessageFailed, reason, now, messageID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_sessions SET last_error=?, updated_at=? WHERE id=?`, reason, now, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO chat_events (session_id, operation_id, sequence, type, created_at)
SELECT ?, ?, COALESCE(MAX(sequence), 0)+1, ?, ? FROM chat_events WHERE operation_id=?`,
		sessionID, operationID, domain.ChatEventCanceled, now, operationID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ActiveChatMessageCiphertext(
	ctx context.Context,
	hostID, jobID, leaseToken string,
) (domain.ChatSendPayload, string, error) {
	payloadData, err := s.ActiveJobPayload(ctx, hostID, jobID, leaseToken, "instance.chat.send")
	if err != nil {
		return domain.ChatSendPayload{}, "", err
	}
	var payload domain.ChatSendPayload
	if err := decodeJSONStrict(payloadData, &payload); err != nil {
		return payload, "", err
	}
	var ciphertext string
	err = s.db.QueryRowContext(ctx, `
SELECT m.ciphertext FROM chat_messages m
JOIN chat_sessions s ON s.id=m.session_id
WHERE m.id=? AND m.session_id=? AND s.instance_id=? AND m.operation_id=(SELECT operation_id FROM jobs WHERE id=?)
  AND m.role='user' AND m.status=?`, payload.MessageID, payload.SessionID, payload.InstanceID, jobID,
		domain.ChatMessagePending).Scan(&ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return payload, "", ErrStateChanged
	}
	return payload, ciphertext, err
}

func decodeJSONStrict(data []byte, target any) error {
	// Job payloads are encoded internally. Their shape is validated again at
	// the trust boundary before encrypted chat content is released.
	if len(data) == 0 {
		return errors.New("empty job payload")
	}
	return jsonUnmarshal(data, target)
}

var jsonUnmarshal = func(data []byte, target any) error {
	return json.Unmarshal(data, target)
}

func chatMessageSealContext(messageID string) string { return "chat-message:" + messageID }

// ChatMessageSealContext is shared by the API encryption boundary and store
// completion validation without exposing ciphertext through public models.
func ChatMessageSealContext(messageID string) string { return chatMessageSealContext(messageID) }

func ChatEventSealContext(operationID string, sequence int64) string {
	return fmt.Sprintf("chat-event:%s:%d", operationID, sequence)
}

func normalizeChatFailure(message string) string {
	if message == "" {
		return "Hermes did not complete this message"
	}
	return message
}

func finishChatMessage(ctx context.Context, tx *sql.Tx, operationID string, payload domain.ChatSendPayload, result domain.JobResult, now time.Time) error {
	status := domain.ChatMessageFailed
	errorText := normalizeChatFailure(result.Error)
	if result.Success {
		if result.ChatCiphertext == "" || result.ChatMessage == "" {
			return invalidJobResult("successful chat result is missing assistant content")
		}
		status, errorText = domain.ChatMessageSucceeded, ""
		assistantID := payload.MessageID + "-assistant"
		if _, err := tx.ExecContext(ctx, `
INSERT INTO chat_messages (id, session_id, operation_id, role, ciphertext, status, error, created_at, updated_at)
VALUES (?, ?, NULL, 'assistant', ?, ?, '', ?, ?)`, assistantID, payload.SessionID,
			result.ChatCiphertext, domain.ChatMessageSucceeded, now, now); err != nil {
			return err
		}
	} else if result.ChatMessage != "" || result.ChatCiphertext != "" {
		return invalidJobResult("failed chat result cannot contain assistant content")
	}
	update, err := tx.ExecContext(ctx, `
UPDATE chat_messages SET status=?, error=?, updated_at=?
WHERE id=? AND session_id=? AND operation_id=? AND role='user' AND status=?`, status, errorText, now,
		payload.MessageID, payload.SessionID, operationID, domain.ChatMessagePending)
	if err != nil {
		return err
	}
	if count, _ := update.RowsAffected(); count != 1 {
		return ErrStateChanged
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_sessions SET last_error=?, updated_at=? WHERE id=?`, errorText, now, payload.SessionID); err != nil {
		return err
	}
	eventType := domain.ChatEventFailed
	if result.Success {
		eventType = domain.ChatEventCompleted
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO chat_events (session_id, operation_id, sequence, type, created_at)
SELECT ?, ?, COALESCE(MAX(sequence), 0)+1, ?, ? FROM chat_events WHERE operation_id=?`,
		payload.SessionID, operationID, eventType, now, operationID); err != nil {
		return err
	}
	return nil
}
