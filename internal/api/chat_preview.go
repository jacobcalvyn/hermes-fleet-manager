package api

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/chatartifacts"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/chatpreview"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/store"
)

func (s *Server) previewChatArtifact(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, private")
	if s.config.ChatArtifacts == nil {
		writeError(w, http.StatusServiceUnavailable, "chat artifact storage is not configured")
		return
	}
	query := r.URL.Query()
	for name := range query {
		if name != "sheet" {
			writeError(w, http.StatusBadRequest, "artifact preview query contains an unsupported option")
			return
		}
	}
	sheet := ""
	if values, present := query["sheet"]; present {
		if len(values) != 1 {
			writeError(w, http.StatusBadRequest, "sheet must be provided once")
			return
		}
		sheet = strings.TrimSpace(values[0])
		if sheet == "" || len(sheet) > 100 || !utf8.ValidString(sheet) {
			writeError(w, http.StatusBadRequest, "sheet must contain between 1 and 100 UTF-8 bytes")
			return
		}
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
		s.logger.Error("open chat artifact preview", "session_id", sessionID, "artifact_id", r.PathValue("artifactID"), "error", err)
		writeError(w, http.StatusInternalServerError, "chat artifact preview could not be opened")
		return
	}
	defer file.Close()
	preview, err := chatpreview.Build(metadata.Name, metadata.MediaType, file, metadata.SizeBytes, sheet)
	switch {
	case errors.Is(err, chatpreview.ErrUnsupported):
		writeError(w, http.StatusUnsupportedMediaType, "chat artifact does not support an interactive preview")
		return
	case errors.Is(err, chatpreview.ErrSheetMissing):
		writeError(w, http.StatusBadRequest, "chat artifact sheet was not found")
		return
	case errors.Is(err, chatpreview.ErrInvalid):
		s.logger.Warn("reject chat artifact preview", "session_id", sessionID, "artifact_id", metadata.ID, "error", err)
		writeError(w, http.StatusUnprocessableEntity, "chat artifact preview could not be generated")
		return
	case err != nil:
		s.logger.Error("generate chat artifact preview", "session_id", sessionID, "artifact_id", metadata.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "chat artifact preview could not be generated")
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	writeJSON(w, http.StatusOK, preview)
}
