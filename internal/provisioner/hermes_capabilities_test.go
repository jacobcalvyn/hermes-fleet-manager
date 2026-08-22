package provisioner

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func TestInspectHermesCapabilitiesUsesAuthenticatedReadOnlyAPIAndBrowserProof(t *testing.T) {
	root := t.TempDir()
	managedPath := filepath.Join(root, "fleet-capabilities-01-00000000")
	if err := os.Mkdir(managedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedPath, "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedPath, ".env"), []byte("API_SERVER_KEY=capability-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provisioner, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	provisioner.dockerRun = func(_ context.Context, args ...string) (string, error) {
		if !strings.Contains(strings.Join(args, " "), "exec -T hermes sh -lc") {
			t.Fatalf("unexpected browser proof command: %v", args)
		}
		return "", nil
	}
	provisioner.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer capability-secret" {
			t.Fatalf("unexpected capability request: %s %s", request.Method, request.Header.Get("Authorization"))
		}
		body := ""
		switch request.URL.Path {
		case "/v1/capabilities":
			body = `{"platform":"hermes-agent","model":"gpt-test","runtime":{"mode":"server_agent","tool_execution":"server","split_runtime":false},"features":{"skills_api":true,"session_key_header":"X-Key"}}`
		case "/v1/skills":
			body = `{"object":"list","data":[{"name":"browser-session","description":"Use Chromium","category":"browser"}]}`
		case "/v1/toolsets":
			body = `{"object":"list","data":[{"name":"browser","label":"Browser","enabled":true,"configured":true,"tools":["browser_exec"]}]}`
		default:
			t.Fatalf("unexpected capability path %q", request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	result := provisioner.inspectHermesCapabilities(context.Background(), domain.HermesCapabilityInspectPayload{
		InstanceID: "instance-1", Name: "capabilities", ProjectName: "fleet-capabilities",
		ManagedPath: managedPath, APIPort: 18650,
	})
	if !result.Success || result.HermesCapabilities == nil {
		t.Fatalf("capability result=%+v", result)
	}
	inventory := result.HermesCapabilities
	if !inventory.Browser.Available || inventory.Browser.Implementation != "playwright-chromium.v1" ||
		!inventory.Features["skills_api"] || len(inventory.Features) != 1 ||
		len(inventory.Skills) != 1 || len(inventory.Toolsets) != 1 {
		t.Fatalf("capability inventory=%+v", inventory)
	}
}
