package provisioner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func TestRepairHermesProfileDashboardRotatesTokenAndRecreatesOnlyDashboard(t *testing.T) {
	managedPath := t.TempDir()
	envPath := filepath.Join(managedPath, ".env")
	if err := os.WriteFile(envPath, []byte("API_SERVER_KEY=key\nHERMES_DASHBOARD_SESSION_TOKEN=existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var dockerArgs []string
	provisioner := &Provisioner{
		dockerRun: func(_ context.Context, args ...string) (string, error) {
			dockerArgs = append([]string(nil), args...)
			return "dashboard recreated", nil
		},
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/chat" {
				t.Fatalf("dashboard readiness path=%q, want /chat", request.URL.Path)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
		})},
	}
	if _, err := provisioner.repairHermesProfileDashboard(context.Background(), managedPath, "fleet-project", 9130); err != nil {
		t.Fatal(err)
	}
	token, err := readManagedEnvValue(envPath, dashboardSessionTokenEnvironmentKey)
	if err != nil {
		t.Fatal(err)
	}
	if token == "existing" {
		t.Fatal("profile repair did not rotate the dashboard session token")
	}
	command := strings.Join(dockerArgs, " ")
	if !strings.Contains(command, "up -d --no-deps --force-recreate dashboard") || strings.Contains(command, "restart hermes") {
		t.Fatalf("repair compose command=%q", command)
	}
}

func TestHermesProfileMutationUsesDedicatedTimeout(t *testing.T) {
	shared := &http.Client{Timeout: time.Second}
	provisioner := &Provisioner{httpClient: shared}
	mutation := provisioner.hermesProfileHTTPClient(http.MethodPost, "/api/profiles")
	if mutation == shared || mutation.Timeout != hermesProfileMutationTimeout {
		t.Fatalf("mutation client=%p timeout=%s, want a dedicated %s client", mutation, mutation.Timeout, hermesProfileMutationTimeout)
	}
	if shared.Timeout != time.Second {
		t.Fatalf("shared client timeout changed to %s", shared.Timeout)
	}
	if got := provisioner.hermesProfileHTTPClient(http.MethodGet, "/api/profiles"); got != shared {
		t.Fatal("profile inventory request should retain the shared client")
	}
	deletion := provisioner.hermesProfileHTTPClient(http.MethodDelete, "/api/profiles/research")
	if deletion == shared || deletion.Timeout != hermesProfileMutationTimeout {
		t.Fatalf("deletion client=%p timeout=%s, want a dedicated %s client", deletion, deletion.Timeout, hermesProfileMutationTimeout)
	}
	if got := provisioner.hermesProfileHTTPClient(http.MethodPost, "/api/profiles/active"); got != shared {
		t.Fatal("active profile request should retain the shared client")
	}
}

func TestHermesProfileCreateDisposition(t *testing.T) {
	profiles := []domain.HermesProfile{{Name: "research", Description: "Research worker"}}
	if got := hermesProfileCreateDisposition(profiles, "coder", "Coding worker"); got != hermesProfileNeedsCreate {
		t.Fatalf("missing profile disposition=%v", got)
	}
	if got := hermesProfileCreateDisposition(profiles, "research", "Research worker"); got != hermesProfileAlreadyManaged {
		t.Fatalf("matching profile disposition=%v", got)
	}
	if got := hermesProfileCreateDisposition(profiles, "research", "Different owner"); got != hermesProfileNameConflict {
		t.Fatalf("conflicting profile disposition=%v", got)
	}
}

func TestActivateHermesProfileVerifiesAuthoritativeInventory(t *testing.T) {
	getCount := 0
	provisioner := &Provisioner{httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/profiles":
			getCount++
			active := getCount > 1
			body, _ := json.Marshal(map[string]any{"profiles": []map[string]any{
				{"name": "default", "default": true, "active": !active, "gateway_running": false},
				{"name": "research", "active": active, "gateway_running": false},
			}})
			return profileHTTPResponse(http.StatusOK, string(body)), nil
		case request.Method == http.MethodPost && request.URL.Path == "/api/profiles/active":
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["name"] != "research" {
				t.Fatalf("activation body=%v", body)
			}
			return profileHTTPResponse(http.StatusOK, `{"ok":true}`), nil
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
			return nil, nil
		}
	})}}
	payload := domain.HermesProfileMutationPayload{
		HermesProfileInspectPayload: domain.HermesProfileInspectPayload{InstanceID: "instance-1", DashboardPort: 9130},
		ProfileName:                 "research",
	}
	result := provisioner.activateHermesProfileWithSession(context.Background(), payload, &hermesProfileSession{})
	if !result.Success || result.HermesProfiles == nil || getCount != 2 {
		t.Fatalf("result=%+v getCount=%d", result, getCount)
	}
	profile, found := hermesProfileByName(result.HermesProfiles.Profiles, "research")
	if !found || !profile.Active {
		t.Fatalf("activated profile=%+v found=%v", profile, found)
	}
}

func TestDeleteHermesProfileVerifiesAbsence(t *testing.T) {
	getCount := 0
	deleteCount := 0
	provisioner := &Provisioner{httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/profiles":
			getCount++
			profiles := []map[string]any{{"name": "default", "default": true, "active": true, "gateway_running": false}}
			if getCount == 1 {
				profiles = append(profiles, map[string]any{"name": "research", "active": false, "gateway_running": true})
			}
			body, _ := json.Marshal(map[string]any{"profiles": profiles})
			return profileHTTPResponse(http.StatusOK, string(body)), nil
		case request.Method == http.MethodDelete && request.URL.Path == "/api/profiles/research":
			deleteCount++
			return profileHTTPResponse(http.StatusOK, `{"ok":true}`), nil
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
			return nil, nil
		}
	})}}
	payload := domain.HermesProfileMutationPayload{
		HermesProfileInspectPayload: domain.HermesProfileInspectPayload{InstanceID: "instance-1", DashboardPort: 9130},
		ProfileName:                 "research",
	}
	result := provisioner.deleteHermesProfileWithSession(context.Background(), payload, &hermesProfileSession{})
	if !result.Success || result.HermesProfiles == nil || getCount != 2 || deleteCount != 1 {
		t.Fatalf("result=%+v getCount=%d deleteCount=%d", result, getCount, deleteCount)
	}
	if _, found := hermesProfileByName(result.HermesProfiles.Profiles, "research"); found {
		t.Fatal("deleted profile remains in the authoritative inventory")
	}
}

func TestDeleteHermesProfileIsIdempotentWhenAlreadyAbsent(t *testing.T) {
	deleteCount := 0
	provisioner := &Provisioner{httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodDelete {
			deleteCount++
		}
		return profileHTTPResponse(http.StatusOK, `{"profiles":[{"name":"default","default":true,"active":true,"gateway_running":false}]}`), nil
	})}}
	payload := domain.HermesProfileMutationPayload{
		HermesProfileInspectPayload: domain.HermesProfileInspectPayload{InstanceID: "instance-1", DashboardPort: 9130},
		ProfileName:                 "research",
	}
	result := provisioner.deleteHermesProfileWithSession(context.Background(), payload, &hermesProfileSession{})
	if !result.Success || deleteCount != 0 {
		t.Fatalf("result=%+v deleteCount=%d", result, deleteCount)
	}
}

func TestRecreateHermesAfterProfileDeletionWaitsForHealthyGateway(t *testing.T) {
	managedPath := t.TempDir()
	var dockerArgs []string
	provisioner := &Provisioner{dockerRun: func(_ context.Context, args ...string) (string, error) {
		dockerArgs = append([]string(nil), args...)
		return "gateway recreated", nil
	}}
	output, err := provisioner.recreateHermesAfterProfileDeletion(context.Background(), managedPath, "fleet-project")
	if err != nil || output != "gateway recreated" {
		t.Fatalf("output=%q err=%v", output, err)
	}
	command := strings.Join(dockerArgs, " ")
	if !strings.Contains(command, "up -d --no-deps --force-recreate --wait --wait-timeout 60 hermes") {
		t.Fatalf("recreate command=%q", command)
	}
}

func profileHTTPResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
