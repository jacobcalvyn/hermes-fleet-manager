package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/chatartifacts"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/providers"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/store"
)

const maximumChatMessageBytes = 64 << 10

type createChatSessionRequest struct {
	InstanceID string `json:"instance_id"`
	Title      string `json:"title"`
}

type sendChatMessageRequest struct {
	Content string `json:"content"`
}

type updateChatSessionConfigurationRequest struct {
	Model       string `json:"model"`
	Reasoning   string `json:"reasoning"`
	ServiceTier string `json:"service_tier"`
}

const maximumChatDeltaBytes = 64 << 10

func validateChatSessionRuntime(provider, model, reasoning, serviceTier string) error {
	if err := providers.ValidateRuntime(provider, model, reasoning, serviceTier); err != nil {
		return err
	}
	if reasoning == "max" {
		return errors.New("session reasoning must be none, minimal, low, medium, high, or xhigh")
	}
	return nil
}

func (s *Server) listChatSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.store.ListChatSessions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chat sessions could not be loaded")
		return
	}
	for index := range sessions {
		if sessions[index].LastMessageCiphertext == "" {
			continue
		}
		plaintext, openErr := s.config.Sealer.Open(sessions[index].LastMessageCiphertext,
			store.ChatMessageSealContext(sessions[index].ID))
		if openErr != nil {
			writeError(w, http.StatusInternalServerError, "chat session preview could not be decrypted")
			return
		}
		sessions[index].LastMessagePreview = chatMessagePreview(string(plaintext))
		clearBytes(plaintext)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, sessions)
}

func chatMessagePreview(content string) string {
	const maximumRunes = 96
	normalized := strings.Join(strings.Fields(content), " ")
	runes := []rune(normalized)
	if len(runes) <= maximumRunes {
		return normalized
	}
	return strings.TrimSpace(string(runes[:maximumRunes])) + "…"
}

func defaultChatSessionTitle(sessionID string) string {
	digest := sha256.Sum256([]byte(sessionID))
	number := 100 + (int(digest[0])<<8|int(digest[1]))%900
	return fmt.Sprintf("Chat %03d", number)
}

func (s *Server) createChatSession(w http.ResponseWriter, r *http.Request) {
	var request createChatSessionRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.InstanceID = strings.TrimSpace(request.InstanceID)
	request.Title = strings.TrimSpace(request.Title)
	if request.InstanceID == "" {
		writeError(w, http.StatusBadRequest, "select one target instance")
		return
	}
	instance, err := s.store.GetInstance(r.Context(), request.InstanceID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "target instance was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "target instance could not be loaded")
		return
	}
	if instance.Status != domain.InstanceRunning || instance.ManagedPath == "" || instance.APIPort < 1 {
		writeError(w, http.StatusConflict, "target instance is not ready for chat")
		return
	}
	if err := validateChatSessionRuntime(instance.Provider, instance.Model, instance.Reasoning, instance.ServiceTier); err != nil {
		writeError(w, http.StatusConflict, "target instance model configuration is not ready for chat")
		return
	}
	sessionID, _, err := twoIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chat session identity could not be generated")
		return
	}
	if request.Title == "" {
		request.Title = defaultChatSessionTitle(sessionID)
	}
	if utf8.RuneCountInString(request.Title) > 120 {
		writeError(w, http.StatusBadRequest, "chat title must not exceed 120 characters")
		return
	}
	now := time.Now().UTC()
	session := domain.ChatSession{
		ID: sessionID, InstanceID: instance.ID, InstanceName: instance.Name, Title: request.Title,
		Model: instance.Model, Reasoning: instance.Reasoning, ServiceTier: instance.ServiceTier,
		Status: domain.ChatSessionActive, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.CreateChatSession(r.Context(), session); err != nil {
		if errors.Is(err, store.ErrStateChanged) {
			writeError(w, http.StatusConflict, "target instance is no longer ready for chat")
			return
		}
		writeError(w, http.StatusInternalServerError, "chat session could not be created")
		return
	}
	s.events.Publish("chat.session.created", instance.ID)
	writeJSON(w, http.StatusCreated, session)
}

func (s *Server) updateChatSessionConfiguration(w http.ResponseWriter, r *http.Request) {
	var request updateChatSessionConfigurationRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.Model = strings.TrimSpace(request.Model)
	request.Reasoning = strings.TrimSpace(request.Reasoning)
	request.ServiceTier = strings.TrimSpace(request.ServiceTier)
	session, err := s.store.GetChatSession(r.Context(), r.PathValue("sessionID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "chat session was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chat session could not be loaded")
		return
	}
	instance, err := s.store.GetInstance(r.Context(), session.InstanceID)
	if err != nil || instance.Status != domain.InstanceRunning {
		writeError(w, http.StatusConflict, "target instance is not ready for chat")
		return
	}
	if err := validateChatSessionRuntime(instance.Provider, request.Model, request.Reasoning, request.ServiceTier); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	allowedModels := map[string]bool{instance.Model: true, session.Model: true}
	if instance.Observation != nil {
		for _, model := range instance.Observation.ModelCatalog {
			allowedModels[model] = true
		}
	}
	if !allowedModels[request.Model] {
		writeError(w, http.StatusBadRequest, "model is not available in the target instance catalog")
		return
	}
	updated, err := s.store.UpdateChatSessionConfiguration(r.Context(), session.ID,
		request.Model, request.Reasoning, request.ServiceTier, time.Now().UTC())
	if errors.Is(err, store.ErrStateChanged) {
		writeError(w, http.StatusConflict, "wait for the active Hermes response before changing session configuration")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chat session configuration could not be updated")
		return
	}
	s.events.Publish("chat.session.updated", session.InstanceID)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deleteChatSession(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	instanceID, err := s.store.DeleteChatSession(r.Context(), sessionID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "chat session was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chat session could not be deleted")
		return
	}
	if s.config.ChatArtifacts != nil {
		if err := s.config.ChatArtifacts.DeleteSession(sessionID); err != nil {
			s.logger.Error("delete chat artifacts", "session_id", sessionID, "error", err)
		}
	}
	s.events.Publish("chat.session.deleted", instanceID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getChatThread(w http.ResponseWriter, r *http.Request) {
	thread, activeEvents, err := s.store.GetChatThreadSnapshot(r.Context(), r.PathValue("sessionID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "chat session was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chat session could not be loaded")
		return
	}
	for index := range thread.Messages {
		plaintext, openErr := s.config.Sealer.Open(thread.Messages[index].Ciphertext, store.ChatMessageSealContext(thread.Session.ID))
		if openErr != nil {
			writeError(w, http.StatusInternalServerError, "chat transcript could not be decrypted")
			return
		}
		thread.Messages[index].Content = string(plaintext)
		clearBytes(plaintext)
	}
	for index := range thread.Events {
		if err := s.openChatEvent(&thread.Events[index]); err != nil {
			writeError(w, http.StatusInternalServerError, "chat activity could not be decrypted")
			return
		}
	}
	if thread.ActiveResponse != nil {
		var content strings.Builder
		for index := range activeEvents {
			if activeEvents[index].Type != domain.ChatEventDelta || activeEvents[index].Ciphertext == "" {
				continue
			}
			plaintext, openErr := s.config.Sealer.Open(activeEvents[index].Ciphertext,
				store.ChatEventSealContext(activeEvents[index].OperationID, activeEvents[index].Sequence))
			if openErr != nil {
				writeError(w, http.StatusInternalServerError, "active chat response could not be decrypted")
				return
			}
			content.Write(plaintext)
			clearBytes(plaintext)
		}
		thread.ActiveResponse.Content = content.String()
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, thread)
}

func (s *Server) openChatEvent(event *domain.ChatEvent) error {
	if event.Ciphertext == "" {
		return nil
	}
	plaintext, err := s.config.Sealer.Open(event.Ciphertext,
		store.ChatEventSealContext(event.OperationID, event.Sequence))
	if err != nil {
		return err
	}
	defer clearBytes(plaintext)
	if event.Type == domain.ChatEventDelta {
		event.Content = string(plaintext)
		return nil
	}
	if event.Type != domain.ChatEventActivity && event.Type != domain.ChatEventArtifact {
		return nil
	}
	var payload domain.ChatEventPayload
	if json.Unmarshal(plaintext, &payload) != nil || !validChatEventPayload(event.Type, payload) {
		// Invalid legacy or upstream-only data remains durable for audit and
		// replay cursors, but it is never turned into invented browser content.
		return nil
	}
	event.Payload = &payload
	return nil
}

func validChatEventPayload(eventType string, payload domain.ChatEventPayload) bool {
	if payload.Event == "" || len(payload.Event) > 256 || len(payload.Data) > maximumChatDeltaBytes ||
		(payload.Data != "" && !utf8.ValidString(payload.Data)) ||
		(payload.Data == "" && payload.Label == "") || len(payload.Label) > 512 ||
		payload.DurationMS < 0 || payload.DurationMS > int64((24*time.Hour)/time.Millisecond) {
		return false
	}
	if eventType == domain.ChatEventActivity {
		return payload.Kind == "activity" && payload.Artifact == nil
	}
	if eventType != domain.ChatEventArtifact || payload.Kind != "artifact" || payload.Artifact == nil {
		return false
	}
	artifact := payload.Artifact
	if artifact.Name == "" || len(artifact.Name) > 80 || artifact.SizeBytes < 0 || artifact.SizeBytes > 1<<40 {
		return false
	}
	if artifact.Kind == "" {
		artifact.Kind = "file"
	}
	if artifact.Kind != "file" && artifact.Kind != "image" && artifact.Kind != "audio" && artifact.Kind != "video" {
		return false
	}
	if len(artifact.MediaType) > 127 || len(artifact.SourceTool) > 80 || len(artifact.URL) > 2048 ||
		len(artifact.Error) > 200 || !validChatArtifactStatus(artifact.Status) {
		return false
	}
	if artifact.SHA256 != "" {
		decoded, err := hex.DecodeString(artifact.SHA256)
		if err != nil || len(decoded) != sha256.Size {
			return false
		}
	}
	unavailable := artifact.Status == "failed" || artifact.Status == "rejected" || artifact.Status == "missing" || artifact.Status == "expired"
	if unavailable && (artifact.URL != "" || artifact.Error == "") {
		return false
	}
	if !unavailable && artifact.Error != "" {
		return false
	}
	if artifact.Status == "preparing" && artifact.URL != "" {
		return false
	}
	if artifact.URL != "" {
		if !validChatArtifactURL(artifact.URL) {
			return false
		}
	}
	return true
}

func validChatArtifactStatus(status string) bool {
	switch status {
	case "", "preparing", "ready", "rejected", "missing", "expired", "failed":
		return true
	default:
		return false
	}
}

func validChatArtifactURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Fragment != "" {
		return false
	}
	if parsed.Scheme == "http" || parsed.Scheme == "https" {
		return parsed.Host != ""
	}
	return parsed.Scheme == "" && parsed.Host == "" && parsed.RawQuery == "" &&
		strings.HasPrefix(parsed.Path, "/api/v1/chats/") && strings.HasSuffix(parsed.Path, "/download") &&
		!strings.Contains(parsed.Path, "..") && !strings.Contains(parsed.Path, "//")
}

func (s *Server) sendChatMessage(w http.ResponseWriter, r *http.Request) {
	var request sendChatMessageRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.Content = strings.TrimSpace(request.Content)
	if request.Content == "" {
		writeError(w, http.StatusBadRequest, "message is required")
		return
	}
	if len(request.Content) > maximumChatMessageBytes || !utf8.ValidString(request.Content) {
		writeError(w, http.StatusBadRequest, "message must be valid UTF-8 and not exceed 64 KiB")
		return
	}
	session, err := s.store.GetChatSession(r.Context(), r.PathValue("sessionID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "chat session was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chat session could not be loaded")
		return
	}
	instance, err := s.store.GetInstance(r.Context(), session.InstanceID)
	if err != nil {
		writeError(w, http.StatusConflict, "target instance could not be loaded")
		return
	}
	if instance.Status != domain.InstanceRunning || instance.ManagedPath == "" || instance.APIPort < 1 {
		writeError(w, http.StatusConflict, "target instance is not ready for chat")
		return
	}
	if err := validateChatSessionRuntime(instance.Provider, session.Model, session.Reasoning, session.ServiceTier); err != nil {
		writeError(w, http.StatusConflict, "chat session configuration is not ready")
		return
	}
	operationID, jobID, messageID, err := threeIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chat message identity could not be generated")
		return
	}
	ciphertext, err := s.config.Sealer.Seal([]byte(request.Content), store.ChatMessageSealContext(session.ID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chat message could not be encrypted")
		return
	}
	payloadData, err := json.Marshal(domain.ChatSendPayload{
		InstanceID: instance.ID, InstanceName: instance.Name, SessionID: session.ID, MessageID: messageID,
		ProjectName: instance.ProjectName, ManagedPath: instance.ManagedPath, APIPort: instance.APIPort,
		Provider: instance.Provider,
		Model:    session.Model, Reasoning: session.Reasoning, ServiceTier: session.ServiceTier,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chat job could not be encoded")
		return
	}
	now := time.Now().UTC()
	operation := domain.Operation{
		ID: operationID, InstanceID: instance.ID, Type: "CHAT_MESSAGE", Status: domain.OperationPending,
		Summary:   "Send chat message to " + instance.Name,
		Metadata:  operationMetadata(map[string]any{"chat_session_id": session.ID, "chat_message_id": messageID}),
		CreatedAt: now, UpdatedAt: now,
	}
	job := domain.Job{
		ID: jobID, OperationID: operation.ID, HostID: instance.HostID, InstanceID: instance.ID,
		Type: "instance.chat.send", Status: domain.JobPending, Payload: payloadData, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.QueueChatMessage(r.Context(), session.ID, messageID, ciphertext, operation, job); err != nil {
		if s.writeQueueAdmissionError(w, err) {
			return
		}
		if errors.Is(err, store.ErrStateChanged) {
			writeError(w, http.StatusConflict, "target instance is no longer ready for chat")
			return
		}
		writeError(w, http.StatusInternalServerError, "chat message could not be queued")
		return
	}
	s.events.Publish("chat.message.queued", instance.ID)
	writeJSON(w, http.StatusAccepted, operation)
}

func (s *Server) downloadChatInput(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	payload, ciphertext, err := s.store.ActiveChatMessageCiphertext(
		r.Context(), r.Header.Get("X-Fleet-Host-ID"), r.PathValue("jobID"), r.Header.Get(leaseTokenHeader),
	)
	if err != nil {
		writeError(w, http.StatusConflict, "chat input lease is no longer active")
		return
	}
	plaintext, err := s.config.Sealer.Open(ciphertext, store.ChatMessageSealContext(payload.SessionID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chat input could not be decrypted")
		return
	}
	defer clearBytes(plaintext)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(plaintext)
}

func (s *Server) appendChatEvent(w http.ResponseWriter, r *http.Request) {
	var request domain.ChatStreamEvent
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.Sequence < 1 || (request.Type != domain.ChatEventStarted &&
		request.Type != domain.ChatEventDelta &&
		request.Type != domain.ChatEventActivity &&
		request.Type != domain.ChatEventArtifact) {
		writeError(w, http.StatusBadRequest, "unsupported chat stream event")
		return
	}
	if request.Type == domain.ChatEventStarted && request.Content != "" {
		writeError(w, http.StatusBadRequest, "chat start event cannot contain content")
		return
	}
	if (request.Type == domain.ChatEventDelta || request.Type == domain.ChatEventActivity || request.Type == domain.ChatEventArtifact) &&
		(request.Content == "" || len(request.Content) > maximumChatDeltaBytes || !utf8.ValidString(request.Content)) {
		writeError(w, http.StatusBadRequest, "chat event content must be valid UTF-8 and not exceed 64 KiB")
		return
	}
	var eventPayload *domain.ChatEventPayload
	if request.Type == domain.ChatEventActivity || request.Type == domain.ChatEventArtifact {
		var payload domain.ChatEventPayload
		if json.Unmarshal([]byte(request.Content), &payload) != nil || !validChatEventPayload(request.Type, payload) {
			writeError(w, http.StatusBadRequest, "chat activity does not match the Fleet capability contract")
			return
		}
		canonical, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			writeError(w, http.StatusBadRequest, "chat activity could not be normalized")
			return
		}
		request.Content = string(canonical)
		eventPayload = &payload
	}
	payloadData, err := s.store.ActiveJobPayload(r.Context(), r.Header.Get("X-Fleet-Host-ID"),
		r.PathValue("jobID"), r.Header.Get(leaseTokenHeader), "instance.chat.send")
	var payload domain.ChatSendPayload
	if err != nil || json.Unmarshal(payloadData, &payload) != nil || payload.SessionID == "" {
		writeError(w, http.StatusConflict, "chat event lease is no longer active")
		return
	}
	operationID, _, err := s.store.JobMetadata(r.Context(), r.Header.Get("X-Fleet-Host-ID"),
		r.PathValue("jobID"), r.Header.Get(leaseTokenHeader))
	if err != nil {
		writeError(w, http.StatusConflict, "chat event lease is no longer active")
		return
	}
	sealContext := store.ChatEventSealContext(operationID, request.Sequence)
	event := domain.ChatEvent{
		Version:   domain.ChatProtocolVersion,
		SessionID: payload.SessionID, OperationID: operationID, Sequence: request.Sequence,
		Type: request.Type, ContentHash: s.config.Sealer.Fingerprint([]byte(request.Content), sealContext), CreatedAt: time.Now().UTC(),
	}
	if request.Content != "" {
		event.Ciphertext, err = s.config.Sealer.Seal([]byte(request.Content), sealContext)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "chat event could not be encrypted")
			return
		}
	}
	if err := s.store.AppendChatEvent(r.Context(), r.Header.Get("X-Fleet-Host-ID"), r.PathValue("jobID"),
		r.Header.Get(leaseTokenHeader), event); err != nil {
		if errors.Is(err, store.ErrLeaseLost) || errors.Is(err, store.ErrStateChanged) {
			writeError(w, http.StatusConflict, "chat event lease is no longer active")
			return
		}
		writeError(w, http.StatusInternalServerError, "chat event could not be recorded")
		return
	}
	if request.Type == domain.ChatEventArtifact && eventPayload != nil && eventPayload.Artifact != nil &&
		s.config.ChatArtifacts != nil && chatartifacts.ValidArtifactID(eventPayload.Artifact.ID) {
		artifact := eventPayload.Artifact
		createdAt := event.CreatedAt
		if artifact.CreatedAt != nil {
			createdAt = artifact.CreatedAt.UTC()
		}
		if _, err := s.config.ChatArtifacts.Record(chatartifacts.Metadata{
			ID: artifact.ID, InstanceID: payload.InstanceID, SessionID: payload.SessionID, OperationID: operationID,
			Name: artifact.Name, Kind: artifact.Kind, MediaType: artifact.MediaType, SizeBytes: artifact.SizeBytes,
			SHA256: strings.ToLower(artifact.SHA256), Status: artifact.Status, Error: artifact.Error,
			CreatedAt: createdAt, ExpiresAt: artifact.ExpiresAt,
		}); err != nil {
			s.logger.Error("record chat artifact lifecycle", "artifact_id", artifact.ID, "session_id", payload.SessionID, "error", err)
			writeError(w, http.StatusInternalServerError, "chat artifact lifecycle could not be recorded")
			return
		}
	}
	s.events.Publish("chat.stream.changed", payload.SessionID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) uploadChatArtifact(w http.ResponseWriter, r *http.Request) {
	if s.config.ChatArtifacts == nil {
		writeError(w, http.StatusServiceUnavailable, "chat artifact storage is not configured")
		return
	}
	artifactID := r.PathValue("artifactID")
	name := strings.TrimSpace(r.Header.Get("X-Fleet-Artifact-Name"))
	kind := strings.TrimSpace(r.Header.Get("X-Fleet-Artifact-Kind"))
	digest := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Fleet-Artifact-SHA256")))
	mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	mediaType = strings.ToLower(mediaType)
	decodedDigest, digestErr := hex.DecodeString(digest)
	if !chatartifacts.ValidArtifactID(artifactID) || !validTransferredArtifactName(name) ||
		!validTransferredArtifactMedia(kind, mediaType) || digestErr != nil || len(decodedDigest) != sha256.Size ||
		r.ContentLength < 1 || r.ContentLength > chatartifacts.MaximumBytes {
		writeError(w, http.StatusBadRequest, "chat artifact metadata is invalid")
		return
	}
	if mediaErr != nil {
		writeError(w, http.StatusBadRequest, "chat artifact media type is invalid")
		return
	}
	hostID, jobID, leaseToken := r.Header.Get("X-Fleet-Host-ID"), r.PathValue("jobID"), r.Header.Get(leaseTokenHeader)
	payloadData, err := s.store.ActiveJobPayload(r.Context(), hostID, jobID, leaseToken, "instance.chat.send")
	var payload domain.ChatSendPayload
	if err != nil || json.Unmarshal(payloadData, &payload) != nil || payload.SessionID == "" {
		writeError(w, http.StatusConflict, "chat artifact lease is no longer active")
		return
	}
	operationID, _, err := s.store.JobMetadata(r.Context(), hostID, jobID, leaseToken)
	if err != nil {
		writeError(w, http.StatusConflict, "chat artifact lease is no longer active")
		return
	}
	metadata := chatartifacts.Metadata{
		ID: artifactID, InstanceID: payload.InstanceID, SessionID: payload.SessionID, OperationID: operationID,
		Name: name, Kind: kind, MediaType: mediaType, SizeBytes: r.ContentLength, SHA256: digest,
	}
	var leaseErr error
	stored, err := s.config.ChatArtifacts.Put(r.Context(), metadata, r.Body, func() error {
		if _, activeErr := s.store.ActiveJobPayload(r.Context(), hostID, jobID, leaseToken, "instance.chat.send"); activeErr != nil {
			leaseErr = activeErr
			return activeErr
		}
		activeOperationID, _, activeErr := s.store.JobMetadata(r.Context(), hostID, jobID, leaseToken)
		if activeErr != nil || activeOperationID != operationID {
			if activeErr == nil {
				activeErr = errors.New("chat artifact operation changed")
			}
			leaseErr = activeErr
			return activeErr
		}
		return nil
	})
	if leaseErr != nil {
		writeError(w, http.StatusConflict, "chat artifact lease is no longer active")
		return
	}
	if errors.Is(err, chatartifacts.ErrInvalid) {
		writeError(w, http.StatusBadRequest, "chat artifact body does not match its metadata")
		return
	}
	if errors.Is(err, chatartifacts.ErrQuota) {
		writeError(w, http.StatusInsufficientStorage, "chat artifact storage quota was exceeded")
		return
	}
	if err != nil {
		s.logger.Error("store chat artifact", "artifact_id", artifactID, "session_id", payload.SessionID, "error", err)
		writeError(w, http.StatusInternalServerError, "chat artifact could not be stored")
		return
	}
	writeJSON(w, http.StatusOK, chatArtifactResponse(stored))
}

func (s *Server) getChatArtifact(w http.ResponseWriter, r *http.Request) {
	if s.config.ChatArtifacts == nil {
		writeError(w, http.StatusServiceUnavailable, "chat artifact storage is not configured")
		return
	}
	sessionID := r.PathValue("sessionID")
	if _, err := s.store.GetChatSession(r.Context(), sessionID); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "chat artifact was not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "chat session could not be loaded")
		return
	}
	metadata, err := s.config.ChatArtifacts.Get(sessionID, r.PathValue("artifactID"))
	if errors.Is(err, chatartifacts.ErrNotFound) {
		writeError(w, http.StatusNotFound, "chat artifact was not found")
		return
	}
	if err != nil {
		s.logger.Error("read chat artifact manifest", "session_id", sessionID, "artifact_id", r.PathValue("artifactID"), "error", err)
		writeError(w, http.StatusInternalServerError, "chat artifact manifest could not be read")
		return
	}
	writeJSON(w, http.StatusOK, chatArtifactResponse(metadata))
}

func chatArtifactResponse(metadata chatartifacts.Metadata) domain.ChatArtifact {
	createdAt := metadata.CreatedAt
	artifact := domain.ChatArtifact{
		ID: metadata.ID, Name: metadata.Name, Kind: metadata.Kind, MediaType: metadata.MediaType,
		SizeBytes: metadata.SizeBytes, SHA256: metadata.SHA256, Status: metadata.Status, Error: metadata.Error,
		SourceTool: "file_output", CreatedAt: &createdAt, ExpiresAt: metadata.ExpiresAt,
	}
	if metadata.Status == chatartifacts.StatusReady {
		artifact.URL = "/api/v1/chats/" + url.PathEscape(metadata.SessionID) + "/artifacts/" + url.PathEscape(metadata.ID) + "/download"
	}
	return artifact
}

func (s *Server) downloadChatArtifact(w http.ResponseWriter, r *http.Request) {
	if s.config.ChatArtifacts == nil {
		writeError(w, http.StatusServiceUnavailable, "chat artifact storage is not configured")
		return
	}
	sessionID := r.PathValue("sessionID")
	if _, err := s.store.GetChatSession(r.Context(), sessionID); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "chat artifact was not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "chat session could not be loaded")
		return
	}
	metadata, file, err := s.config.ChatArtifacts.Open(sessionID, r.PathValue("artifactID"))
	if errors.Is(err, chatartifacts.ErrNotFound) {
		writeError(w, http.StatusNotFound, "chat artifact was not found")
		return
	}
	if errors.Is(err, chatartifacts.ErrExpired) {
		writeError(w, http.StatusGone, "chat artifact expired")
		return
	}
	if errors.Is(err, chatartifacts.ErrMissing) {
		writeError(w, http.StatusGone, "chat artifact content is missing")
		return
	}
	if err != nil {
		s.logger.Error("open chat artifact", "session_id", sessionID, "artifact_id", r.PathValue("artifactID"), "error", err)
		writeError(w, http.StatusInternalServerError, "chat artifact could not be opened")
		return
	}
	defer file.Close()
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("Content-Type", metadata.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(metadata.SizeBytes, 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": metadata.Name}))
	http.ServeContent(w, r, metadata.Name, metadata.CreatedAt, file)
}

func validTransferredArtifactName(name string) bool {
	if name == "" || len(name) > 80 || strings.ContainsAny(name, "/\\") {
		return false
	}
	for _, character := range name {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune(" ._-()", character) {
			continue
		}
		return false
	}
	return true
}

func validTransferredArtifactMedia(kind, mediaType string) bool {
	allowed := map[string]map[string]bool{
		"image": {"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true},
		"audio": {"audio/mpeg": true, "audio/wav": true},
		"video": {"video/mp4": true, "video/webm": true},
		"file": {
			"application/pdf": true, "text/csv": true, "text/plain": true,
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         true,
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   true,
			"application/vnd.openxmlformats-officedocument.presentationml.presentation": true,
		},
	}
	return allowed[kind][mediaType]
}

func (s *Server) chatEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	if _, err := s.store.GetChatSession(r.Context(), sessionID); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "chat session was not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "chat session could not be loaded")
		return
	}
	afterID := int64(0)
	cursor := strings.TrimSpace(r.URL.Query().Get("after"))
	if cursor == "" {
		cursor = strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	}
	if cursor != "" {
		parsed, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "invalid chat event cursor")
			return
		}
		afterID = parsed
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "chat streaming is unavailable")
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	updates, unsubscribe := s.events.Subscribe()
	defer unsubscribe()
	drain := func() error {
		for {
			events, err := s.store.ListChatEvents(r.Context(), sessionID, afterID, 100)
			if err != nil {
				return err
			}
			for index := range events {
				if err := s.openChatEvent(&events[index]); err != nil {
					return err
				}
				if err := writeChatEvent(w, events[index]); err != nil {
					return err
				}
				afterID = events[index].ID
			}
			if len(events) < 100 {
				return nil
			}
		}
	}
	if err := drain(); err != nil {
		return
	}
	flusher.Flush()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-updates:
			if err := drain(); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeChatEvent(w io.Writer, event domain.ChatEvent) error {
	if event.Version == 0 {
		event.Version = domain.ChatProtocolVersion
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: chat-event\ndata: %s\n\n", event.ID, payload)
	return err
}

func (s *Server) cancelChatRun(w http.ResponseWriter, r *http.Request) {
	const reason = "Canceled by Fleet operator"
	if err := s.store.CancelActiveChat(r.Context(), r.PathValue("sessionID"), reason, time.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusConflict, "this chat has no active response to cancel")
			return
		}
		writeError(w, http.StatusInternalServerError, "chat response could not be canceled")
		return
	}
	s.events.Publish("chat.stream.changed", r.PathValue("sessionID"))
	w.WriteHeader(http.StatusNoContent)
}
