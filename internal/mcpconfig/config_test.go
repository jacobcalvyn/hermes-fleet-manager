package mcpconfig

import (
	"testing"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func TestNormalizeAndValidateRemoteConfiguration(t *testing.T) {
	config, err := NormalizeAndValidate(domain.MCPConfiguration{Servers: []domain.MCPServerConfiguration{{
		Name: " Issues ", Source: "remote", URL: "https://mcp.example.test/mcp", AuthType: "bearer",
		BearerToken: "secret", Enabled: true, Tools: []string{"write_issue", "read_issue", "read_issue"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	server := config.Servers[0]
	if server.Name != "issues" || len(server.Tools) != 2 || server.Tools[0] != "read_issue" {
		t.Fatalf("unexpected normalized server: %+v", server)
	}
}

func TestNormalizeAndValidateRejectsExecutableAndUnsafeURL(t *testing.T) {
	tests := []domain.MCPServerConfiguration{
		{Name: "shell", Source: "stdio", URL: "https://example.test", Enabled: true},
		{Name: "plain", Source: "remote", URL: "http://example.test/mcp", Enabled: true},
		{Name: "credential", Source: "remote", URL: "https://user:pass@example.test/mcp", Enabled: true},
	}
	for _, server := range tests {
		if _, err := NormalizeAndValidate(domain.MCPConfiguration{Servers: []domain.MCPServerConfiguration{server}}); err == nil {
			t.Fatalf("expected validation failure for %+v", server)
		}
	}
}
