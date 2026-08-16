package api

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func TestDefaultChatSessionTitleUsesThreeDigitSuffix(t *testing.T) {
	title := defaultChatSessionTitle("11111111-1111-4111-8111-111111111111")
	if !regexp.MustCompile(`^Chat [0-9]{3}$`).MatchString(title) {
		t.Fatalf("default title=%q", title)
	}
	if title != defaultChatSessionTitle("11111111-1111-4111-8111-111111111111") {
		t.Fatal("default title changed for the same session identity")
	}
}

func TestChatMessagePreviewNormalizesAndBoundsContent(t *testing.T) {
	content := "  first line\n\nsecond\tline  " + strings.Repeat("界", 120)
	preview := chatMessagePreview(content)
	if strings.ContainsAny(preview, "\n\t") || !strings.HasPrefix(preview, "first line second line ") || !strings.HasSuffix(preview, "…") {
		t.Fatalf("preview=%q", preview)
	}
	if utf8.RuneCountInString(strings.TrimSuffix(preview, "…")) != 96 {
		t.Fatalf("preview rune count=%d", utf8.RuneCountInString(preview))
	}
}

func TestValidChatEventPayloadEnforcesArtifactCapabilityBoundary(t *testing.T) {
	payload := domain.ChatEventPayload{
		Kind: "artifact", Event: "artifact.created", Label: "Created report.csv",
		Artifact: &domain.ChatArtifact{
			Name: "report.csv", Kind: "file", MediaType: "text/csv", SizeBytes: 2048,
			URL: "https://example.test/report.csv", SourceTool: "report_writer",
		},
	}
	if !validChatEventPayload(domain.ChatEventArtifact, payload) {
		t.Fatal("valid canonical artifact was rejected")
	}
	payload.Artifact.URL = "javascript:alert(1)"
	if validChatEventPayload(domain.ChatEventArtifact, payload) {
		t.Fatal("unsafe artifact URL was accepted")
	}
	payload.Artifact.URL = ""
	payload.Kind = "activity"
	if validChatEventPayload(domain.ChatEventArtifact, payload) {
		t.Fatal("artifact event accepted activity payload")
	}
	payload.Kind = "artifact"
	payload.Artifact.URL = "/api/v1/chats/11111111-1111-4111-8111-111111111111/artifacts/artifact-0123456789abcdef0123456789abcdef/download"
	payload.Artifact.Status = "ready"
	payload.Artifact.SHA256 = strings.Repeat("a", 64)
	if !validChatEventPayload(domain.ChatEventArtifact, payload) {
		t.Fatal("authenticated Fleet artifact URL was rejected")
	}
	payload.Artifact.Status = "failed"
	payload.Artifact.URL = ""
	payload.Artifact.Error = "The output is unavailable."
	if !validChatEventPayload(domain.ChatEventArtifact, payload) {
		t.Fatal("failed artifact state was rejected")
	}
}

func TestValidTransferredArtifactMediaAcceptsPlainTextFile(t *testing.T) {
	if !validTransferredArtifactMedia("file", "text/plain") {
		t.Fatal("plain-text chat artifact was rejected")
	}
	if validTransferredArtifactMedia("image", "text/plain") {
		t.Fatal("plain-text chat artifact was accepted as an image")
	}
}

func TestValidChatEventPayloadAcceptsArtifactLifecycleStatuses(t *testing.T) {
	for _, status := range []string{"preparing", "ready", "rejected", "missing", "expired", "failed"} {
		artifact := &domain.ChatArtifact{
			ID: "artifact-0123456789abcdef0123456789abcdef", Name: "report.txt", Kind: "file",
			MediaType: "text/plain", SizeBytes: 12, SHA256: strings.Repeat("a", 64), Status: status,
		}
		if status == "ready" {
			artifact.URL = "/api/v1/chats/11111111-1111-4111-8111-111111111111/artifacts/artifact-0123456789abcdef0123456789abcdef/download"
		} else if status != "preparing" {
			artifact.Error = "The output is unavailable."
		}
		payload := domain.ChatEventPayload{
			Kind: "artifact", Event: "fleet.artifact." + status, Label: "Artifact " + status,
			Status: status, Artifact: artifact,
		}
		if !validChatEventPayload(domain.ChatEventArtifact, payload) {
			t.Fatalf("artifact status %q was rejected", status)
		}
	}
}

func TestValidChatEventPayloadAcceptsExactHermesDataWithoutNormalizedLabel(t *testing.T) {
	payload := domain.ChatEventPayload{
		Kind:  "activity",
		Event: "tool.started",
		Data:  `{"type":"tool.started","tool":{"name":"Browser Navigate","input":{"url":"https://example.test"}}}`,
	}
	if !validChatEventPayload(domain.ChatEventActivity, payload) {
		t.Fatal("exact Hermes activity data was rejected")
	}
	payload.Data = string([]byte{0xff})
	if validChatEventPayload(domain.ChatEventActivity, payload) {
		t.Fatal("invalid UTF-8 Hermes data was accepted")
	}
}
