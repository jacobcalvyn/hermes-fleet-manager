package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/chatartifacts"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

const (
	defaultArtifactPageLimit  = 25
	maximumArtifactPageLimit  = 100
	maximumArtifactCursorSize = 2048
)

type artifactResponse struct {
	ID           string     `json:"id"`
	InstanceID   string     `json:"instance_id"`
	InstanceName string     `json:"instance_name,omitempty"`
	SessionID    string     `json:"session_id"`
	SessionTitle string     `json:"session_title,omitempty"`
	OperationID  string     `json:"operation_id"`
	Name         string     `json:"name"`
	Kind         string     `json:"kind"`
	MediaType    string     `json:"media_type,omitempty"`
	SizeBytes    int64      `json:"size_bytes"`
	SHA256       string     `json:"sha256,omitempty"`
	Status       string     `json:"status"`
	Error        string     `json:"error,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	DeletedAt    *time.Time `json:"deleted_at,omitempty"`
	DownloadURL  string     `json:"download_url,omitempty"`
}

type artifactPageResponse struct {
	Items      []artifactResponse `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type artifactFilters struct {
	Query        string `json:"q,omitempty"`
	InstanceID   string `json:"instance_id,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	Status       string `json:"status,omitempty"`
	Kind         string `json:"kind,omitempty"`
	CreatedAfter string `json:"created_after,omitempty"`
}

type artifactCursorPayload struct {
	Cursor  chatartifacts.Cursor `json:"cursor"`
	Filters artifactFilters      `json:"filters"`
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.config.ChatArtifacts == nil {
		writeError(w, http.StatusServiceUnavailable, "output storage is not configured")
		return
	}
	query := r.URL.Query()
	for name := range query {
		switch name {
		case "limit", "cursor", "q", "instance_id", "session_id", "status", "kind", "created_after":
		default:
			writeError(w, http.StatusBadRequest, "outputs query contains an unsupported filter")
			return
		}
	}
	filters, options, err := parseArtifactFilters(query)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := defaultArtifactPageLimit
	if raw, ok := singleQueryValue(query, "limit"); ok {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 1 || parsed > maximumArtifactPageLimit {
			writeError(w, http.StatusBadRequest, "limit must be an integer between 1 and 100")
			return
		}
		limit = parsed
	} else if _, present := query["limit"]; present {
		writeError(w, http.StatusBadRequest, "limit must be an integer between 1 and 100")
		return
	}
	options.Limit = limit
	if raw, ok := singleQueryValue(query, "cursor"); ok {
		cursor, cursorFilters, decodeErr := decodeArtifactCursor(raw)
		if decodeErr != nil || cursorFilters != filters {
			writeError(w, http.StatusBadRequest, "cursor is invalid for the selected output filters")
			return
		}
		options.Cursor = cursor
	} else if _, present := query["cursor"]; present {
		writeError(w, http.StatusBadRequest, "cursor is invalid")
		return
	}
	page, err := s.config.ChatArtifacts.List(options, time.Now().UTC())
	if errors.Is(err, chatartifacts.ErrInvalid) {
		writeError(w, http.StatusBadRequest, "output filters or cursor are invalid")
		return
	}
	if err != nil {
		s.logger.Error("list outputs", "error", err)
		writeError(w, http.StatusInternalServerError, "outputs could not be listed")
		return
	}
	sessions, err := s.store.ListChatSessions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "output origins could not be loaded")
		return
	}
	response := artifactPageResponse{Items: artifactResponses(page.Items, sessions)}
	if page.NextCursor != nil {
		response.NextCursor = encodeArtifactCursor(*page.NextCursor, filters)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) artifactUsage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if len(r.URL.Query()) != 0 {
		writeError(w, http.StatusBadRequest, "output usage does not accept query parameters")
		return
	}
	if s.config.ChatArtifacts == nil {
		writeError(w, http.StatusServiceUnavailable, "output storage is not configured")
		return
	}
	usage, err := s.config.ChatArtifacts.Usage(time.Now().UTC())
	if err != nil {
		s.logger.Error("read output usage", "error", err)
		writeError(w, http.StatusInternalServerError, "output usage could not be loaded")
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

func (s *Server) deleteArtifact(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.config.ChatArtifacts == nil {
		writeError(w, http.StatusServiceUnavailable, "output storage is not configured")
		return
	}
	metadata, err := s.config.ChatArtifacts.DeleteArtifact(r.PathValue("artifactID"), time.Now().UTC())
	if errors.Is(err, chatartifacts.ErrNotFound) {
		writeError(w, http.StatusNotFound, "output was not found")
		return
	}
	if err != nil {
		s.logger.Error("delete output", "artifact_id", r.PathValue("artifactID"), "error", err)
		writeError(w, http.StatusInternalServerError, "output could not be deleted")
		return
	}
	sessions, _ := s.store.ListChatSessions(r.Context())
	writeJSON(w, http.StatusOK, artifactResponses([]chatartifacts.Metadata{metadata}, sessions)[0])
}

func parseArtifactFilters(query url.Values) (artifactFilters, chatartifacts.ListOptions, error) {
	filters := artifactFilters{}
	for _, pair := range []struct {
		name   string
		target *string
	}{
		{"q", &filters.Query}, {"instance_id", &filters.InstanceID}, {"session_id", &filters.SessionID},
		{"status", &filters.Status}, {"kind", &filters.Kind}, {"created_after", &filters.CreatedAfter},
	} {
		if value, ok := singleQueryValue(query, pair.name); ok {
			*pair.target = strings.TrimSpace(value)
		} else if _, present := query[pair.name]; present {
			return filters, chatartifacts.ListOptions{}, errors.New("output filters must contain one value")
		}
	}
	if len(filters.Query) > 120 ||
		(filters.InstanceID != "" && !chatartifacts.ValidInstanceID(filters.InstanceID)) ||
		(filters.SessionID != "" && !chatartifacts.ValidSessionID(filters.SessionID)) ||
		(filters.Status != "" && !chatartifacts.ValidStatus(filters.Status)) ||
		(filters.Kind != "" && !chatartifacts.ValidKind(filters.Kind)) {
		return filters, chatartifacts.ListOptions{}, errors.New("one or more output filters are invalid")
	}
	options := chatartifacts.ListOptions{
		Query: filters.Query, InstanceID: filters.InstanceID, SessionID: filters.SessionID,
		Status: filters.Status, Kind: filters.Kind,
	}
	if filters.CreatedAfter != "" {
		parsed, err := time.Parse(time.RFC3339, filters.CreatedAfter)
		if err != nil {
			return filters, options, errors.New("created_after must be an RFC3339 timestamp")
		}
		normalized := parsed.UTC()
		filters.CreatedAfter = normalized.Format(time.RFC3339)
		options.CreatedAfter = &normalized
	}
	return filters, options, nil
}

func singleQueryValue(query url.Values, name string) (string, bool) {
	values, present := query[name]
	return first(values), present && len(values) == 1 && values[0] != ""
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func artifactResponses(items []chatartifacts.Metadata, sessions []domain.ChatSession) []artifactResponse {
	origins := make(map[string]domain.ChatSession, len(sessions))
	for _, session := range sessions {
		origins[session.ID] = session
	}
	result := make([]artifactResponse, 0, len(items))
	for _, item := range items {
		origin := origins[item.SessionID]
		response := artifactResponse{
			ID: item.ID, InstanceID: item.InstanceID, InstanceName: origin.InstanceName,
			SessionID: item.SessionID, SessionTitle: origin.Title, OperationID: item.OperationID,
			Name: item.Name, Kind: item.Kind, MediaType: item.MediaType, SizeBytes: item.SizeBytes,
			SHA256: item.SHA256, Status: item.Status, Error: item.Error, CreatedAt: item.CreatedAt,
			ExpiresAt: item.ExpiresAt, DeletedAt: item.DeletedAt,
		}
		if item.Status == chatartifacts.StatusReady {
			response.DownloadURL = "/api/v1/chats/" + url.PathEscape(item.SessionID) + "/artifacts/" + url.PathEscape(item.ID) + "/download"
		}
		result = append(result, response)
	}
	return result
}

func encodeArtifactCursor(cursor chatartifacts.Cursor, filters artifactFilters) string {
	payload, _ := json.Marshal(artifactCursorPayload{Cursor: cursor, Filters: filters})
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeArtifactCursor(raw string) (*chatartifacts.Cursor, artifactFilters, error) {
	if raw == "" || raw != strings.TrimSpace(raw) || len(raw) > maximumArtifactCursorSize {
		return nil, artifactFilters{}, errors.New("output cursor is invalid")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) == 0 || len(decoded) > maximumArtifactCursorSize {
		return nil, artifactFilters{}, errors.New("output cursor is invalid")
	}
	var payload artifactCursorPayload
	decoder := json.NewDecoder(strings.NewReader(string(decoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return nil, artifactFilters{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) || payload.Cursor.CreatedAt.IsZero() ||
		!chatartifacts.ValidSessionID(payload.Cursor.SessionID) || !chatartifacts.ValidArtifactID(payload.Cursor.ID) {
		return nil, artifactFilters{}, errors.New("output cursor fields are invalid")
	}
	payload.Cursor.CreatedAt = payload.Cursor.CreatedAt.UTC()
	return &payload.Cursor, payload.Filters, nil
}
