package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/chatartifacts"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/chatpreview"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func TestChatArtifactPreviewRequiresAuthAndReturnsBoundedData(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, hostToken := environment.enrollHost(t)
	instance := environment.provisionRunningInstance(t, hostID, hostToken, "preview-test")
	now := time.Now().UTC()
	session := domain.ChatSession{
		ID: "11111111-1111-4111-8111-111111111111", InstanceID: instance.ID, Title: "Artifact preview",
		Status: domain.ChatSessionActive, CreatedAt: now, UpdatedAt: now,
	}
	if err := environment.dataStore.CreateChatSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}

	content := []byte("Company,Ticker\nAlphabet,GOOGL\nNvidia,NVDA\n")
	digest := sha256.Sum256(content)
	metadata := chatartifacts.Metadata{
		ID: "artifact-0123456789abcdef0123456789abcdef", InstanceID: instance.ID,
		SessionID: session.ID, OperationID: "22222222-2222-4222-8222-222222222222",
		Name: "companies.csv", Kind: "file", MediaType: "text/csv", SizeBytes: int64(len(content)),
		SHA256: hex.EncodeToString(digest[:]), CreatedAt: now,
	}
	if _, err := environment.chatArtifacts.Put(context.Background(), metadata, bytes.NewReader(content), nil); err != nil {
		t.Fatal(err)
	}
	previewURL := "/api/v1/chats/" + session.ID + "/artifacts/" + metadata.ID + "/preview"

	response := environment.request(t, http.MethodGet, previewURL, nil, "", nil)
	assertStatus(t, response, http.StatusUnauthorized)
	response.Body.Close()

	response = environment.request(t, http.MethodGet, previewURL+"?unexpected=true", nil, environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)
	response.Body.Close()

	response = environment.request(t, http.MethodGet, previewURL+"?sheet=Data", nil, environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)
	response.Body.Close()

	response = environment.request(t, http.MethodGet, previewURL, nil, environment.adminToken, nil)
	assertStatus(t, response, http.StatusOK)
	if response.Header.Get("Cache-Control") != "no-store, private" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("preview security headers cache=%q nosniff=%q", response.Header.Get("Cache-Control"), response.Header.Get("X-Content-Type-Options"))
	}
	var preview chatpreview.Preview
	if err := json.NewDecoder(response.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(preview.Columns) != 2 || preview.Columns[0] != "Company" || len(preview.Rows) != 2 || preview.Rows[1][1] != "NVDA" {
		t.Fatalf("preview=%+v", preview)
	}
}
