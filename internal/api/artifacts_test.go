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
)

func TestOutputsAPIListsUsageAndDeletesWithBoundCursor(t *testing.T) {
	environment := newAPITestEnvironment(t)
	now := time.Now().UTC().Add(-time.Minute)
	content := []byte("fleet output")
	digest := sha256.Sum256(content)
	ready := chatartifacts.Metadata{
		ID:          "artifact-0123456789abcdef0123456789abcdef",
		InstanceID:  "33333333-3333-4333-8333-333333333333",
		SessionID:   "11111111-1111-4111-8111-111111111111",
		OperationID: "22222222-2222-4222-8222-222222222222",
		Name:        "report.txt", Kind: "file", MediaType: "text/plain", SizeBytes: int64(len(content)),
		SHA256: hex.EncodeToString(digest[:]), CreatedAt: now,
	}
	if _, err := environment.chatArtifacts.Put(context.Background(), ready, bytes.NewReader(content), nil); err != nil {
		t.Fatal(err)
	}
	failed := ready
	failed.ID = "artifact-fedcba9876543210fedcba9876543210"
	failed.Name = "preview.png"
	failed.Kind = "image"
	failed.MediaType = "image/png"
	failed.SizeBytes = 0
	failed.SHA256 = ""
	failed.Status = chatartifacts.StatusFailed
	failed.Error = "Renderer failed."
	failed.CreatedAt = now.Add(time.Second)
	if _, err := environment.chatArtifacts.Record(failed); err != nil {
		t.Fatal(err)
	}

	response := environment.request(t, http.MethodGet, "/api/v1/artifacts?limit=1", nil, environment.adminToken, nil)
	assertStatus(t, response, http.StatusOK)
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("outputs response was cacheable")
	}
	var page artifactPageResponse
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(page.Items) != 1 || page.Items[0].ID != failed.ID || page.NextCursor == "" {
		t.Fatalf("page=%+v", page)
	}

	response = environment.request(t, http.MethodGet, "/api/v1/artifacts?limit=1&status=ready&cursor="+page.NextCursor, nil, environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)
	response.Body.Close()

	response = environment.request(t, http.MethodGet, "/api/v1/artifacts/usage", nil, environment.adminToken, nil)
	assertStatus(t, response, http.StatusOK)
	var usage chatartifacts.UsageSnapshot
	if err := json.NewDecoder(response.Body).Decode(&usage); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if usage.TotalBytes != int64(len(content)) || usage.StatusCounts[chatartifacts.StatusReady] != 1 || usage.StatusCounts[chatartifacts.StatusFailed] != 1 {
		t.Fatalf("usage=%+v", usage)
	}

	response = environment.request(t, http.MethodDelete, "/api/v1/artifacts/"+ready.ID, nil, environment.adminToken, nil)
	assertStatus(t, response, http.StatusOK)
	var deleted artifactResponse
	if err := json.NewDecoder(response.Body).Decode(&deleted); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if deleted.Status != chatartifacts.StatusDeleted || deleted.DownloadURL != "" || deleted.DeletedAt == nil {
		t.Fatalf("deleted=%+v", deleted)
	}
}

func TestOutputsAPIRequiresAdminAndRejectsUnknownFilters(t *testing.T) {
	environment := newAPITestEnvironment(t)
	response := environment.request(t, http.MethodGet, "/api/v1/artifacts", nil, "", nil)
	assertStatus(t, response, http.StatusUnauthorized)
	response.Body.Close()
	response = environment.request(t, http.MethodGet, "/api/v1/artifacts?unexpected=true", nil, environment.adminToken, nil)
	assertStatus(t, response, http.StatusBadRequest)
	response.Body.Close()
}
