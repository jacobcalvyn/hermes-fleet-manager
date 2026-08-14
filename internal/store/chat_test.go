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

func TestChatSessionKeepsItsInstanceAndJobPayloadExcludesPrompt(t *testing.T) {
	ctx, dataStore, host, instance := newChatFixture(t, "chat-payload")
	now := time.Now().UTC()
	session := domain.ChatSession{
		ID: "chat-session-payload", InstanceID: instance.ID, Title: "Payload boundary",
		Status: domain.ChatSessionActive, CreatedAt: now, UpdatedAt: now,
	}
	if err := dataStore.CreateChatSession(ctx, session); err != nil {
		t.Fatal(err)
	}

	const prompt = "private prompt that must stay encrypted"
	payload, err := json.Marshal(domain.ChatSendPayload{
		InstanceID: instance.ID, InstanceName: instance.Name, SessionID: session.ID, MessageID: "chat-message-payload",
		ProjectName: instance.ProjectName, ManagedPath: instance.ManagedPath, APIPort: instance.APIPort,
		Provider: instance.Provider,
		Model:    instance.Model, Reasoning: instance.Reasoning, ServiceTier: instance.ServiceTier,
	})
	if err != nil {
		t.Fatal(err)
	}
	operation := domain.Operation{
		ID: "chat-operation-payload", InstanceID: instance.ID, Type: "CHAT_MESSAGE",
		Status: domain.OperationPending, Summary: "Send chat message", Metadata: json.RawMessage(`{"chat_session_id":"chat-session-payload"}`),
		CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: "chat-job-payload", OperationID: operation.ID, HostID: host.ID, InstanceID: instance.ID,
		Type: "instance.chat.send", Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now,
	}
	badJob := job
	badJob.ID = "chat-job-cross-session"
	badJob.Payload, err = json.Marshal(domain.ChatSendPayload{
		InstanceID: instance.ID, InstanceName: instance.Name, SessionID: "another-session", MessageID: "chat-message-payload",
		ProjectName: instance.ProjectName, ManagedPath: instance.ManagedPath, APIPort: instance.APIPort,
		Provider: instance.Provider,
		Model:    instance.Model, Reasoning: instance.Reasoning, ServiceTier: instance.ServiceTier,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dataStore.QueueChatMessage(ctx, session.ID, "chat-message-payload", "encrypted-user-message", operation, badJob); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("cross-session payload error=%v, want %v", err, ErrStateChanged)
	}
	if err := dataStore.QueueChatMessage(ctx, session.ID, "chat-message-payload", "encrypted-user-message", operation, job); err != nil {
		t.Fatal(err)
	}

	var storedPayload, storedMetadata string
	if err := dataStore.db.QueryRowContext(ctx, `
SELECT j.payload, o.metadata FROM jobs j JOIN operations o ON o.id=j.operation_id WHERE j.id=?`, job.ID).
		Scan(&storedPayload, &storedMetadata); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(storedPayload, prompt) || strings.Contains(storedMetadata, prompt) {
		t.Fatalf("plaintext prompt leaked into job or operation: payload=%q metadata=%q", storedPayload, storedMetadata)
	}
	storedSession, err := dataStore.GetChatSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedSession.InstanceID != instance.ID || storedSession.MessageCount != 1 {
		t.Fatalf("stored session=%+v", storedSession)
	}
}

func TestChatSessionConfigurationIsScopedAndFencedDuringActiveResponse(t *testing.T) {
	ctx, dataStore, host, instance := newChatFixture(t, "session-configuration")
	now := time.Now().UTC()
	session := domain.ChatSession{
		ID: "chat-session-configuration", InstanceID: instance.ID, Title: "Configuration",
		Status: domain.ChatSessionActive, CreatedAt: now, UpdatedAt: now,
	}
	if err := dataStore.CreateChatSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	stored, err := dataStore.GetChatSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Model != instance.Model || stored.Reasoning != instance.Reasoning || stored.ServiceTier != instance.ServiceTier {
		t.Fatalf("session defaults=%+v instance=%+v", stored, instance)
	}
	updated, err := dataStore.UpdateChatSessionConfiguration(ctx, session.ID, "gpt-session", "high", "priority", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if updated.Model != "gpt-session" || updated.Reasoning != "high" || updated.ServiceTier != "priority" {
		t.Fatalf("updated session=%+v", updated)
	}
	unchangedInstance, err := dataStore.GetInstance(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchangedInstance.Model != instance.Model || unchangedInstance.Reasoning != instance.Reasoning || unchangedInstance.ServiceTier != instance.ServiceTier {
		t.Fatalf("session update mutated instance=%+v", unchangedInstance)
	}

	updated.Model = "gpt-session"
	updated.Reasoning = "high"
	updated.ServiceTier = "priority"
	queueTestChatMessage(t, ctx, dataStore, host, instance, updated, "session-configuration", now.Add(2*time.Second))
	if _, err := dataStore.UpdateChatSessionConfiguration(ctx, session.ID, "gpt-other", "low", "normal", now.Add(3*time.Second)); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("active response update error=%v, want %v", err, ErrStateChanged)
	}
}

func TestChatInputRequiresActiveLeaseAndCompletionPreservesInstanceHealth(t *testing.T) {
	ctx, dataStore, host, instance := newChatFixture(t, "chat-complete")
	session, job := queueTestChat(t, ctx, dataStore, host, instance, "complete")
	sessions, err := dataStore.ListChatSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || !sessions[0].ResponseInProgress || sessions[0].LastMessageID != "chat-message-complete" ||
		sessions[0].LastMessageRole != "user" || sessions[0].LastMessageCiphertext != "encrypted-user-complete" ||
		sessions[0].LastMessageAt == nil || sessions[0].LastEventID == 0 {
		t.Fatalf("queued session summary=%+v", sessions)
	}

	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.ID != job.ID {
		t.Fatalf("claimed job=%+v, want %s", claimed, job.ID)
	}
	if _, _, err := dataStore.ActiveChatMessageCiphertext(ctx, host.ID, job.ID, "wrong-lease"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong lease error=%v, want %v", err, ErrLeaseLost)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, job.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	payload, ciphertext, err := dataStore.ActiveChatMessageCiphertext(ctx, host.ID, job.ID, claimed.LeaseToken)
	if err != nil {
		t.Fatal(err)
	}
	if payload.SessionID != session.ID || ciphertext != "encrypted-user-complete" {
		t.Fatalf("leased input payload=%+v ciphertext=%q", payload, ciphertext)
	}
	activityAt := time.Now().UTC().Add(time.Second)
	if err := dataStore.AppendChatEvent(ctx, host.ID, job.ID, claimed.LeaseToken, domain.ChatEvent{
		SessionID: session.ID, OperationID: job.OperationID, Sequence: 1, Type: domain.ChatEventActivity,
		Ciphertext: "encrypted-activity", ContentHash: "activity-hash", CreatedAt: activityAt,
	}); err != nil {
		t.Fatal(err)
	}
	sessions, err = dataStore.ListChatSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || !sessions[0].UpdatedAt.Equal(activityAt) || !sessions[0].ResponseInProgress {
		t.Fatalf("active session summary=%+v", sessions)
	}
	result := domain.JobResult{
		Success: true, Message: "Hermes chat completed", ChatMessage: "assistant reply",
		ChatCiphertext: "encrypted-assistant-complete",
	}
	if err := dataStore.CompleteJob(ctx, host.ID, job.ID, claimed.LeaseToken, result, nil); err != nil {
		t.Fatal(err)
	}
	sessions, err = dataStore.ListChatSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ResponseInProgress || sessions[0].LastMessageRole != "assistant" ||
		sessions[0].LastMessageID != "chat-message-complete-assistant" ||
		sessions[0].LastMessageCiphertext != "encrypted-assistant-complete" || sessions[0].MessageCount != 2 {
		t.Fatalf("completed session summary=%+v", sessions)
	}

	messages, err := dataStore.ListChatMessages(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Role != "user" || messages[0].Status != domain.ChatMessageSucceeded ||
		messages[1].Role != "assistant" || messages[1].Ciphertext != "encrypted-assistant-complete" {
		t.Fatalf("completed messages=%+v", messages)
	}
	thread, activeEvents, err := dataStore.GetChatThreadSnapshot(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if thread.ActiveResponse != nil || len(activeEvents) != 0 || len(thread.Messages) != 2 ||
		len(thread.Events) != 1 || thread.Events[0].Type != domain.ChatEventActivity || thread.LastCursor == 0 {
		t.Fatalf("completed snapshot thread=%+v active_events=%+v", thread, activeEvents)
	}
	storedInstance, err := dataStore.GetInstance(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedInstance.Status != domain.InstanceRunning || storedInstance.LastError != "" {
		t.Fatalf("chat completion mutated instance health: %+v", storedInstance)
	}
}

func TestFailedChatMessageDoesNotFailInstance(t *testing.T) {
	ctx, dataStore, host, instance := newChatFixture(t, "chat-failure")
	session, job := queueTestChat(t, ctx, dataStore, host, instance, "failure")
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim job=%+v err=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, job.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, job.ID, claimed.LeaseToken, domain.JobResult{
		Success: false, Error: "Hermes unavailable",
	}, nil); err != nil {
		t.Fatal(err)
	}
	messages, err := dataStore.ListChatMessages(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Status != domain.ChatMessageFailed || messages[0].Error != "Hermes unavailable" {
		t.Fatalf("failed messages=%+v", messages)
	}
	storedInstance, err := dataStore.GetInstance(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedInstance.Status != domain.InstanceRunning || storedInstance.LastError != "" {
		t.Fatalf("failed chat mutated instance health: %+v", storedInstance)
	}
}

func TestExpiredChatLeaseFailsClosedWithoutReclaim(t *testing.T) {
	for _, reconcile := range []bool{false, true} {
		name := "claim"
		if reconcile {
			name = "reconcile"
		}
		t.Run(name, func(t *testing.T) {
			ctx, dataStore, host, instance := newChatFixture(t, "chat-lease-lost-"+name)
			session, job := queueTestChat(t, ctx, dataStore, host, instance, "chat-lease-lost-"+name)
			claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
			if err != nil || claimed == nil || claimed.ID != job.ID || claimed.Attempts != 1 {
				t.Fatalf("first ClaimJob() job=%+v error=%v", claimed, err)
			}
			now := time.Now().UTC()
			if _, err := dataStore.db.ExecContext(ctx, `
UPDATE jobs SET lease_expires_at=? WHERE id=?`, now.Add(-time.Minute), job.ID); err != nil {
				t.Fatal(err)
			}
			if reconcile {
				count, err := dataStore.ReconcileExpiredJobs(ctx, now)
				if err != nil || count != 1 {
					t.Fatalf("ReconcileExpiredJobs() count=%d error=%v", count, err)
				}
			} else {
				next, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
				if err != nil || next != nil {
					t.Fatalf("second ClaimJob() job=%+v error=%v, want no replay", next, err)
				}
			}

			var status string
			var attempts int
			if err := dataStore.db.QueryRowContext(ctx, `SELECT status, attempts FROM jobs WHERE id=?`, job.ID).
				Scan(&status, &attempts); err != nil {
				t.Fatal(err)
			}
			if status != domain.JobFailed || attempts != 1 {
				t.Fatalf("chat job status=%q attempts=%d", status, attempts)
			}
			operation, err := dataStore.GetOperation(ctx, job.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			var metadata map[string]any
			if err := json.Unmarshal(operation.Metadata, &metadata); err != nil {
				t.Fatal(err)
			}
			if operation.Status != domain.OperationFailed ||
				!strings.Contains(operation.Error, "prevent duplicate Hermes tool execution") ||
				metadata["failure"] != "chat-lease-lost" || metadata["lease_claims"] != float64(1) {
				t.Fatalf("operation=%+v metadata=%v", operation, metadata)
			}
			messages, err := dataStore.ListChatMessages(ctx, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(messages) != 1 || messages[0].Status != domain.ChatMessageFailed ||
				!strings.Contains(messages[0].Error, "prevent duplicate Hermes tool execution") {
				t.Fatalf("messages=%+v", messages)
			}
			events, err := dataStore.ListChatEvents(ctx, session.ID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 2 || events[1].Type != domain.ChatEventFailed {
				t.Fatalf("events=%+v", events)
			}
			storedInstance, err := dataStore.GetInstance(ctx, instance.ID)
			if err != nil || storedInstance.Status != domain.InstanceRunning || storedInstance.LastError != "" {
				t.Fatalf("instance=%+v error=%v", storedInstance, err)
			}
		})
	}
}

func TestChatEventsAreLeaseFencedIdempotentAndReplayable(t *testing.T) {
	ctx, dataStore, host, instance := newChatFixture(t, "chat-events")
	session, job := queueTestChat(t, ctx, dataStore, host, instance, "events")
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim job=%+v err=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, job.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	event := domain.ChatEvent{
		SessionID: session.ID, OperationID: job.OperationID, Sequence: 1, Type: domain.ChatEventStarted,
		ContentHash: "started-hash", CreatedAt: time.Now().UTC(),
	}
	if err := dataStore.AppendChatEvent(ctx, host.ID, job.ID, "wrong-lease", event); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong lease error=%v, want %v", err, ErrLeaseLost)
	}
	if err := dataStore.AppendChatEvent(ctx, host.ID, job.ID, claimed.LeaseToken, event); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.AppendChatEvent(ctx, host.ID, job.ID, claimed.LeaseToken, event); err != nil {
		t.Fatalf("idempotent append error=%v", err)
	}
	conflict := event
	conflict.ContentHash = "different-hash"
	if err := dataStore.AppendChatEvent(ctx, host.ID, job.ID, claimed.LeaseToken, conflict); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("conflicting replay error=%v, want %v", err, ErrStateChanged)
	}
	events, err := dataStore.ListChatEvents(ctx, session.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != domain.ChatEventQueued || events[1].Type != domain.ChatEventStarted {
		t.Fatalf("events=%+v", events)
	}
	replayed, err := dataStore.ListChatEvents(ctx, session.ID, events[0].ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 1 || replayed[0].ID != events[1].ID {
		t.Fatalf("replayed events=%+v", replayed)
	}
}

func TestChatThreadSnapshotIncludesActiveResponseAndResumeCursor(t *testing.T) {
	ctx, dataStore, host, instance := newChatFixture(t, "chat-snapshot")
	session, job := queueTestChat(t, ctx, dataStore, host, instance, "snapshot")
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim job=%+v err=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, job.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	started := domain.ChatEvent{
		SessionID: session.ID, OperationID: job.OperationID, Sequence: 1, Type: domain.ChatEventStarted,
		ContentHash: "started", CreatedAt: time.Now().UTC(),
	}
	if err := dataStore.AppendChatEvent(ctx, host.ID, job.ID, claimed.LeaseToken, started); err != nil {
		t.Fatal(err)
	}
	delta := domain.ChatEvent{
		SessionID: session.ID, OperationID: job.OperationID, Sequence: 2, Type: domain.ChatEventDelta,
		Ciphertext: "encrypted-delta", ContentHash: "delta", CreatedAt: time.Now().UTC(),
	}
	if err := dataStore.AppendChatEvent(ctx, host.ID, job.ID, claimed.LeaseToken, delta); err != nil {
		t.Fatal(err)
	}
	thread, activeEvents, err := dataStore.GetChatThreadSnapshot(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if thread.ProtocolVersion != domain.ChatProtocolVersion || thread.LastCursor < 3 {
		t.Fatalf("snapshot protocol=%d cursor=%d", thread.ProtocolVersion, thread.LastCursor)
	}
	if thread.ActiveResponse == nil || thread.ActiveResponse.OperationID != job.OperationID ||
		thread.ActiveResponse.State != domain.ChatEventStarted || thread.ActiveResponse.LastSequence != 2 {
		t.Fatalf("active response=%+v", thread.ActiveResponse)
	}
	if len(activeEvents) != 3 || activeEvents[2].Ciphertext != "encrypted-delta" {
		t.Fatalf("active events=%+v", activeEvents)
	}
	replayed, err := dataStore.ListChatEvents(ctx, session.ID, thread.LastCursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 0 {
		t.Fatalf("events replayed after snapshot cursor=%+v", replayed)
	}
}

func TestChatProtocolReplayConformance(t *testing.T) {
	ctx, dataStore, host, instance := newChatFixture(t, "chat-protocol-replay")
	session, job := queueTestChat(t, ctx, dataStore, host, instance, "protocol-replay")
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim job=%+v err=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, job.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	events := []domain.ChatEvent{
		{SessionID: session.ID, OperationID: job.OperationID, Sequence: 1, Type: domain.ChatEventStarted, ContentHash: "started", CreatedAt: now},
		{SessionID: session.ID, OperationID: job.OperationID, Sequence: 2, Type: domain.ChatEventActivity, Ciphertext: "encrypted-activity", ContentHash: "activity", CreatedAt: now.Add(time.Millisecond)},
		{SessionID: session.ID, OperationID: job.OperationID, Sequence: 3, Type: domain.ChatEventArtifact, Ciphertext: "encrypted-artifact", ContentHash: "artifact", CreatedAt: now.Add(2 * time.Millisecond)},
		{SessionID: session.ID, OperationID: job.OperationID, Sequence: 4, Type: domain.ChatEventDelta, Ciphertext: "encrypted-delta-a", ContentHash: "delta-a", CreatedAt: now.Add(3 * time.Millisecond)},
	}
	for _, event := range events {
		if err := dataStore.AppendChatEvent(ctx, host.ID, job.ID, claimed.LeaseToken, event); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, activeEvents, err := dataStore.GetChatThreadSnapshot(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ProtocolVersion != domain.ChatProtocolVersion || snapshot.LastCursor == 0 ||
		snapshot.ActiveResponse == nil || snapshot.ActiveResponse.LastSequence != 4 ||
		snapshot.ActiveResponse.State != domain.ChatEventStarted {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if len(activeEvents) != 5 || len(snapshot.Events) != 2 ||
		activeEvents[0].Type != domain.ChatEventQueued || activeEvents[4].Type != domain.ChatEventDelta {
		t.Fatalf("active events=%+v rich events=%+v", activeEvents, snapshot.Events)
	}

	// Replaying an already persisted frame is a no-op; reconnects must not
	// duplicate visible content or advance the durable cursor.
	if err := dataStore.AppendChatEvent(ctx, host.ID, job.ID, claimed.LeaseToken, events[3]); err != nil {
		t.Fatalf("idempotent replay error=%v", err)
	}
	next := domain.ChatEvent{
		SessionID: session.ID, OperationID: job.OperationID, Sequence: 5, Type: domain.ChatEventDelta,
		Ciphertext: "encrypted-delta-b", ContentHash: "delta-b", CreatedAt: now.Add(4 * time.Millisecond),
	}
	if err := dataStore.AppendChatEvent(ctx, host.ID, job.ID, claimed.LeaseToken, next); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CompleteJob(ctx, host.ID, job.ID, claimed.LeaseToken, domain.JobResult{
		Success: true, Message: "Hermes chat completed", ChatMessage: "assistant reply",
		ChatCiphertext: "encrypted-assistant-reply",
	}, nil); err != nil {
		t.Fatal(err)
	}

	tail, err := dataStore.ListChatEvents(ctx, session.ID, snapshot.LastCursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 2 || tail[0].Sequence != 5 || tail[0].Type != domain.ChatEventDelta ||
		tail[1].Sequence != 6 || tail[1].Type != domain.ChatEventCompleted || tail[0].ID >= tail[1].ID {
		t.Fatalf("tail after cursor %d=%+v", snapshot.LastCursor, tail)
	}
	finalSnapshot, finalActiveEvents, err := dataStore.GetChatThreadSnapshot(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finalSnapshot.ActiveResponse != nil || len(finalActiveEvents) != 0 || len(finalSnapshot.Messages) != 2 ||
		len(finalSnapshot.Events) != 2 || finalSnapshot.LastCursor != tail[1].ID {
		t.Fatalf("final snapshot=%+v active events=%+v", finalSnapshot, finalActiveEvents)
	}
	remaining, err := dataStore.ListChatEvents(ctx, session.ID, finalSnapshot.LastCursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("events replayed after terminal cursor=%+v", remaining)
	}
}

func TestChatProtocolTerminalConformance(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		ctx, dataStore, host, instance := newChatFixture(t, "chat-protocol-failure")
		session, job := queueTestChat(t, ctx, dataStore, host, instance, "protocol-failure")
		claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
		if err != nil || claimed == nil {
			t.Fatalf("claim job=%+v err=%v", claimed, err)
		}
		if err := dataStore.AcknowledgeJob(ctx, host.ID, job.ID, claimed.LeaseToken, time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := dataStore.CompleteJob(ctx, host.ID, job.ID, claimed.LeaseToken, domain.JobResult{
			Success: false, Error: "upstream unavailable",
		}, nil); err != nil {
			t.Fatal(err)
		}
		events, err := dataStore.ListChatEvents(ctx, session.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 2 || events[1].Sequence != 1 || events[1].Type != domain.ChatEventFailed {
			t.Fatalf("failure events=%+v", events)
		}
	})

	t.Run("cancel fences lease", func(t *testing.T) {
		ctx, dataStore, host, instance := newChatFixture(t, "chat-protocol-cancel")
		session, job := queueTestChat(t, ctx, dataStore, host, instance, "protocol-cancel")
		claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
		if err != nil || claimed == nil {
			t.Fatalf("claim job=%+v err=%v", claimed, err)
		}
		if err := dataStore.AcknowledgeJob(ctx, host.ID, job.ID, claimed.LeaseToken, time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := dataStore.CancelActiveChat(ctx, session.ID, "Canceled by conformance test", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err := dataStore.RenewJob(ctx, host.ID, job.ID, claimed.LeaseToken, time.Minute); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("renew after cancel error=%v, want %v", err, ErrLeaseLost)
		}
		events, err := dataStore.ListChatEvents(ctx, session.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 2 || events[1].Sequence != 1 || events[1].Type != domain.ChatEventCanceled {
			t.Fatalf("cancel events=%+v", events)
		}
	})
}

func TestCancelActiveChatFencesJobAndRecordsTerminalEvent(t *testing.T) {
	ctx, dataStore, host, instance := newChatFixture(t, "chat-cancel")
	session, job := queueTestChat(t, ctx, dataStore, host, instance, "cancel")
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim job=%+v err=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, job.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CancelActiveChat(ctx, session.ID, "Canceled by test", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.RenewJob(ctx, host.ID, job.ID, claimed.LeaseToken, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("renew after cancel error=%v, want %v", err, ErrLeaseLost)
	}
	messages, err := dataStore.ListChatMessages(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Status != domain.ChatMessageFailed || messages[0].Error != "Canceled by test" {
		t.Fatalf("messages=%+v", messages)
	}
	events, err := dataStore.ListChatEvents(ctx, session.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Type != domain.ChatEventCanceled {
		t.Fatalf("events=%+v", events)
	}
}

func TestDeleteChatSessionIsScopedAndFencesActiveWork(t *testing.T) {
	ctx, dataStore, host, instance := newChatFixture(t, "chat-delete")
	deletedSession, deletedJob := queueTestChat(t, ctx, dataStore, host, instance, "delete-target")
	preservedSession := domain.ChatSession{
		ID: "chat-session-preserved", InstanceID: instance.ID, Title: "Preserved chat",
		Status: domain.ChatSessionActive, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := dataStore.CreateChatSession(ctx, preservedSession); err != nil {
		t.Fatal(err)
	}
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil || claimed.ID != deletedJob.ID {
		t.Fatalf("claim job=%+v err=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, deletedJob.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}

	instanceID, err := dataStore.DeleteChatSession(ctx, deletedSession.ID)
	if err != nil || instanceID != instance.ID {
		t.Fatalf("DeleteChatSession() instance=%q error=%v", instanceID, err)
	}
	if _, err := dataStore.GetChatSession(ctx, deletedSession.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session error=%v, want %v", err, ErrNotFound)
	}
	if _, err := dataStore.GetChatSession(ctx, preservedSession.ID); err != nil {
		t.Fatalf("preserved session error=%v", err)
	}
	if err := dataStore.RenewJob(ctx, host.ID, deletedJob.ID, claimed.LeaseToken, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("renew deleted chat job error=%v, want %v", err, ErrLeaseLost)
	}
	for table, query := range map[string]string{
		"messages":   `SELECT COUNT(*) FROM chat_messages WHERE session_id=?`,
		"events":     `SELECT COUNT(*) FROM chat_events WHERE session_id=?`,
		"jobs":       `SELECT COUNT(*) FROM jobs WHERE operation_id=?`,
		"operations": `SELECT COUNT(*) FROM operations WHERE id=?`,
	} {
		argument := deletedSession.ID
		if table == "jobs" || table == "operations" {
			argument = deletedJob.OperationID
		}
		var count int
		if err := dataStore.db.QueryRowContext(ctx, query, argument).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s count=%d error=%v, want 0", table, count, err)
		}
	}
	storedInstance, err := dataStore.GetInstance(ctx, instance.ID)
	if err != nil || storedInstance.Status != domain.InstanceRunning {
		t.Fatalf("instance after chat deletion=%+v error=%v", storedInstance, err)
	}
	if _, err := dataStore.DeleteChatSession(ctx, deletedSession.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing session error=%v, want %v", err, ErrNotFound)
	}
}

func TestReconcileOrphanedChatWorkFencesLegacyDeletedSession(t *testing.T) {
	ctx, dataStore, host, instance := newChatFixture(t, "chat-orphan-reconcile")
	session, job := queueTestChat(t, ctx, dataStore, host, instance, "legacy-delete")
	claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || claimed == nil || claimed.ID != job.ID {
		t.Fatalf("claim job=%+v err=%v", claimed, err)
	}
	if err := dataStore.AcknowledgeJob(ctx, host.ID, job.ID, claimed.LeaseToken, time.Minute); err != nil {
		t.Fatal(err)
	}

	// Reproduce the legacy bug: the transcript is cascaded away while the job
	// and operation remain live.
	if _, err := dataStore.db.ExecContext(ctx, `DELETE FROM chat_sessions WHERE id=?`, session.ID); err != nil {
		t.Fatal(err)
	}
	removed, err := dataStore.reconcileOrphanedChatWork(ctx)
	if err != nil || removed != 1 {
		t.Fatalf("reconcile removed=%d error=%v, want 1", removed, err)
	}
	if err := dataStore.RenewJob(ctx, host.ID, job.ID, claimed.LeaseToken, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("renew reconciled job error=%v, want %v", err, ErrLeaseLost)
	}
	for table, query := range map[string]string{
		"jobs":       `SELECT COUNT(*) FROM jobs WHERE id=?`,
		"operations": `SELECT COUNT(*) FROM operations WHERE id=?`,
	} {
		argument := job.ID
		if table == "operations" {
			argument = job.OperationID
		}
		var count int
		if err := dataStore.db.QueryRowContext(ctx, query, argument).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s count=%d error=%v, want 0", table, count, err)
		}
	}
}

func TestChatClaimsAreConcurrentAcrossSessionsOnTheSameInstance(t *testing.T) {
	ctx, dataStore, host, instance := newChatFixture(t, "chat-session-concurrency")
	_, firstJob := queueTestChat(t, ctx, dataStore, host, instance, "session-concurrency-first")
	_, secondJob := queueTestChat(t, ctx, dataStore, host, instance, "session-concurrency-second")

	claimedIDs := map[string]bool{}
	for range 2 {
		claimed, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
		if err != nil || claimed == nil {
			t.Fatalf("ClaimJob() job=%+v error=%v", claimed, err)
		}
		claimedIDs[claimed.ID] = true
	}
	if !claimedIDs[firstJob.ID] || !claimedIDs[secondJob.ID] {
		t.Fatalf("claimed jobs=%v, want %s and %s", claimedIDs, firstJob.ID, secondJob.ID)
	}
}

func TestChatMessagesQueueFIFOWithinOneSession(t *testing.T) {
	ctx, dataStore, host, instance := newChatFixture(t, "chat-session-fifo")
	session, firstJob := queueTestChat(t, ctx, dataStore, host, instance, "session-fifo-first")
	secondJob := queueTestChatMessage(
		t, ctx, dataStore, host, instance, session, "session-fifo-second", time.Now().UTC().Add(time.Second),
	)

	firstClaim, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || firstClaim == nil || firstClaim.ID != firstJob.ID {
		t.Fatalf("first ClaimJob() job=%+v error=%v, want %s", firstClaim, err, firstJob.ID)
	}
	blockedClaim, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || blockedClaim != nil {
		t.Fatalf("concurrent same-session ClaimJob() job=%+v error=%v, want nil", blockedClaim, err)
	}
	thread, _, err := dataStore.GetChatThreadSnapshot(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if thread.ActiveResponse == nil || thread.ActiveResponse.OperationID != firstJob.OperationID {
		t.Fatalf("active response=%+v, want operation %s", thread.ActiveResponse, firstJob.OperationID)
	}
	if err := dataStore.CancelActiveChat(ctx, session.ID, "Canceled by test", time.Now().UTC().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	secondClaim, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || secondClaim == nil || secondClaim.ID != secondJob.ID {
		t.Fatalf("next ClaimJob() job=%+v error=%v, want %s", secondClaim, err, secondJob.ID)
	}
}

func TestChatBypassesAnActiveAdministrativeJobOnTheSameInstance(t *testing.T) {
	ctx, dataStore, host, instance := newChatFixture(t, "chat-admin-concurrency")
	now := time.Now().UTC()
	tx, err := dataStore.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	adminOperation := domain.Operation{
		ID: "chat-admin-operation", InstanceID: instance.ID, Type: "CREDENTIAL_REVEAL",
		Status: domain.OperationPending, Summary: "Administrative work", CreatedAt: now, UpdatedAt: now,
	}
	adminJob := domain.Job{
		ID: "chat-admin-job", OperationID: adminOperation.ID, HostID: host.ID, InstanceID: instance.ID,
		Type: "instance.credentials.inspect", Status: domain.JobPending, Payload: json.RawMessage(`{}`),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := insertOperationAndJob(ctx, tx, adminOperation, adminJob); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	adminClaim, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || adminClaim == nil || adminClaim.ID != adminJob.ID {
		t.Fatalf("administrative ClaimJob() job=%+v error=%v", adminClaim, err)
	}

	_, chatJob := queueTestChat(t, ctx, dataStore, host, instance, "admin-concurrency")
	chatClaim, err := dataStore.ClaimJob(ctx, host.ID, time.Minute)
	if err != nil || chatClaim == nil || chatClaim.ID != chatJob.ID {
		t.Fatalf("chat ClaimJob() job=%+v error=%v, want %s", chatClaim, err, chatJob.ID)
	}
}

func newChatFixture(t *testing.T, suffix string) (context.Context, *Store, domain.Host, domain.Instance) {
	t.Helper()
	ctx, dataStore, host, instance := newFleetFixture(t, suffix)
	instance.Status = domain.InstanceRunning
	instance.ManagedPath = "/var/lib/hermes-fleet/instances/" + suffix
	instance.ProjectName = "project-" + suffix
	tx, err := dataStore.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status=? WHERE id=?`, domain.JobSucceeded, "job-"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE operations SET status=? WHERE id=?`, domain.OperationSucceeded, "operation-"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE instances SET status=?, managed_path=?, project_name=?, data_volume=?, last_error='' WHERE id=?`,
		instance.Status, instance.ManagedPath, instance.ProjectName, "volume-"+suffix, instance.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return ctx, dataStore, host, instance
}

func queueTestChat(
	t *testing.T,
	ctx context.Context,
	dataStore *Store,
	host domain.Host,
	instance domain.Instance,
	suffix string,
) (domain.ChatSession, domain.Job) {
	t.Helper()
	now := time.Now().UTC()
	session := domain.ChatSession{
		ID: "chat-session-" + suffix, InstanceID: instance.ID, Title: "Chat " + suffix,
		Status: domain.ChatSessionActive, CreatedAt: now, UpdatedAt: now,
	}
	if err := dataStore.CreateChatSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	return session, queueTestChatMessage(t, ctx, dataStore, host, instance, session, suffix, now)
}

func queueTestChatMessage(
	t *testing.T,
	ctx context.Context,
	dataStore *Store,
	host domain.Host,
	instance domain.Instance,
	session domain.ChatSession,
	suffix string,
	now time.Time,
) domain.Job {
	t.Helper()
	storedSession, err := dataStore.GetChatSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(domain.ChatSendPayload{
		InstanceID: instance.ID, InstanceName: instance.Name, SessionID: session.ID, MessageID: "chat-message-" + suffix,
		ProjectName: instance.ProjectName, ManagedPath: instance.ManagedPath, APIPort: instance.APIPort,
		Provider: instance.Provider,
		Model:    storedSession.Model, Reasoning: storedSession.Reasoning, ServiceTier: storedSession.ServiceTier,
	})
	if err != nil {
		t.Fatal(err)
	}
	operation := domain.Operation{
		ID: "chat-operation-" + suffix, InstanceID: instance.ID, Type: "CHAT_MESSAGE",
		Status: domain.OperationPending, Summary: "Send chat message", CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: "chat-job-" + suffix, OperationID: operation.ID, HostID: host.ID, InstanceID: instance.ID,
		Type: "instance.chat.send", Status: domain.JobPending, Payload: payload, CreatedAt: now, UpdatedAt: now,
	}
	if err := dataStore.QueueChatMessage(ctx, session.ID, "chat-message-"+suffix, "encrypted-user-"+suffix, operation, job); err != nil {
		t.Fatal(err)
	}
	return job
}
