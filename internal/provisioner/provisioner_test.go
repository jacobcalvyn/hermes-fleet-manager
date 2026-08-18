package provisioner

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/compatibility"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/recovery"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/recoverycodec"
	runtimeassets "github.com/jacobcalvyn/hermes-fleet-manager/runtime"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestHermesProfileAuthenticationUsesEphemeralCookieSession(t *testing.T) {
	requests := 0
	p := &Provisioner{httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if got := request.Header.Get("X-Hermes-Session-Token"); got != "" {
			t.Fatalf("session token header=%q, want empty", got)
		}
		switch request.URL.Path {
		case "/api/auth/providers":
			if request.Method != http.MethodGet {
				t.Fatalf("provider request method=%s", request.Method)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"providers":[{"name":"basic","supports_password":true}]}`)),
				Header:     make(http.Header),
			}, nil
		case "/auth/password-login":
			if request.Method != http.MethodPost {
				t.Fatalf("login request method=%s", request.Method)
			}
			var payload map[string]string
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["provider"] != "basic" || payload["username"] != "fleet-admin" || payload["password"] != "dashboard-secret" {
				t.Fatalf("login payload=%v", payload)
			}
			header := make(http.Header)
			header.Add("Set-Cookie", "hermes_session=authenticated; Path=/; HttpOnly; SameSite=Lax")
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Header: header}, nil
		case "/api/profiles":
			cookie, err := request.Cookie("hermes_session")
			if err != nil || cookie.Value != "authenticated" {
				t.Fatalf("authenticated cookie=%v, err=%v", cookie, err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"profiles":[]}`)),
				Header:     make(http.Header),
			}, nil
		default:
			t.Fatalf("unexpected request=%s %s", request.Method, request.URL.Path)
			return nil, nil
		}
	})}}
	session, err := p.authenticateHermesProfileSession(context.Background(), 9119, "fleet-admin", "dashboard-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.hermesProfileRequest(context.Background(), 9119, session, http.MethodGet, "/api/profiles", nil); err != nil {
		t.Fatal(err)
	}
	if requests != 3 {
		t.Fatalf("requests=%d, want 3", requests)
	}
}

func TestHermesProfileAuthenticationRejectsLoginWithoutCookie(t *testing.T) {
	p := &Provisioner{httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := `{"providers":[{"name":"basic","supports_password":true}]}`
		if request.URL.Path == "/auth/password-login" {
			body = `{"ok":true}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}}
	_, err := p.authenticateHermesProfileSession(context.Background(), 9119, "fleet-admin", "dashboard-secret")
	if err == nil || !strings.Contains(err.Error(), "did not establish a session") {
		t.Fatalf("error=%v", err)
	}
}

func TestHermesPasswordProviderAcceptsWrappedAndListDocuments(t *testing.T) {
	for name, body := range map[string]string{
		"wrapped": `{"providers":[{"name":"basic"}]}`,
		"list":    `[{"id":"local","type":"basic"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			provider, err := hermesPasswordProvider([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			if provider == "" {
				t.Fatal("password provider is empty")
			}
		})
	}
}

func TestEnsureDashboardSessionTokenFilePreservesExistingToken(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte("API_SERVER_KEY=key\nHERMES_DASHBOARD_SESSION_TOKEN=existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := ensureDashboardSessionTokenFile(context.Background(), envPath)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("existing session token was unexpectedly replaced")
	}
	encoded, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "API_SERVER_KEY=key\nHERMES_DASHBOARD_SESSION_TOKEN=existing\n" {
		t.Fatalf("environment=%q", encoded)
	}
}

func TestEnsureDashboardSessionTokenFileCreatesMissingToken(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte("API_SERVER_KEY=key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := ensureDashboardSessionTokenFile(context.Background(), envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("missing session token was not created")
	}
	token, err := readManagedEnvValue(envPath, dashboardSessionTokenEnvironmentKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) < 32 {
		t.Fatalf("session token length=%d", len(token))
	}
}

func TestRotateDashboardSessionTokenFileReplacesExistingToken(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte("API_SERVER_KEY=key\nHERMES_DASHBOARD_SESSION_TOKEN=existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rotateDashboardSessionTokenFile(context.Background(), envPath); err != nil {
		t.Fatal(err)
	}
	token, err := readManagedEnvValue(envPath, dashboardSessionTokenEnvironmentKey)
	if err != nil {
		t.Fatal(err)
	}
	if token == "existing" || len(token) < 32 {
		t.Fatalf("rotated session token was not replaced: length=%d", len(token))
	}
}

func TestRenderComposeUsesIsolatedResources(t *testing.T) {
	payload := domain.ProvisionPayload{
		InstanceID: "00000000-0000-4000-8000-000000000001",
		Name:       "fleet-test-01", Image: "local/hermes-fleet-runtime:0.18.2",
		APIPort: 8650, DashboardPort: 9130,
	}
	manifest := renderCompose(payload, "hermes-fleet-fleet-test-01-00000000", "hermes-fleet-fleet-test-01-00000000-data")
	for _, expected := range []string{
		"127.0.0.1:8650:8642",
		"127.0.0.1:9130:9119",
		"container_name: \"hermes-fleet-instance-fleet-test-01-hermes\"",
		"container_name: \"hermes-fleet-instance-fleet-test-01-dashboard\"",
		"io.hermes-fleet.instance-id: \"00000000-0000-4000-8000-000000000001\"",
		"name: \"hermes-fleet-fleet-test-01-00000000-data\"",
		"io.hermes-fleet.managed: \"true\"",
		"TELEGRAM_BOT_TOKEN: ${TELEGRAM_BOT_TOKEN:-}",
		"WHATSAPP_ENABLED: ${WHATSAPP_ENABLED:-false}",
		"pids_limit: ${HERMES_FLEET_HERMES_PIDS_LIMIT:-512}",
		"mem_limit: ${HERMES_FLEET_HERMES_MEMORY_LIMIT:-4g}",
		"pids_limit: ${HERMES_FLEET_DASHBOARD_PIDS_LIMIT:-256}",
		"mem_limit: ${HERMES_FLEET_DASHBOARD_MEMORY_LIMIT:-2g}",
		`max-size: "${HERMES_FLEET_LOG_MAX_SIZE:-25m}"`,
		`max-file: "${HERMES_FLEET_LOG_MAX_FILES:-4}"`,
		"name: hermes-fleet-edge",
		"external: true",
		"HERMES_DASHBOARD_SESSION_TOKEN: ${HERMES_DASHBOARD_SESSION_TOKEN:-}",
		`curl -fsS -u \"$${HERMES_DASHBOARD_BASIC_AUTH_USERNAME}:$${HERMES_DASHBOARD_BASIC_AUTH_PASSWORD}\" http://127.0.0.1:9119/chat`,
	} {
		if !strings.Contains(manifest, expected) {
			t.Fatalf("manifest does not contain %q", expected)
		}
	}
	services := strings.SplitN(manifest, "\n  dashboard:", 2)
	if len(services) != 2 {
		t.Fatal("manifest does not contain the dashboard service boundary")
	}
	for _, key := range []string{"TELEGRAM_BOT_TOKEN", "WHATSAPP_ALLOWED_USERS"} {
		if strings.Contains(services[1], key) {
			t.Fatalf("dashboard service unexpectedly receives %s", key)
		}
	}
}

func TestMessagingEnvironmentClearsDisabledChannelCredentials(t *testing.T) {
	environment := messagingEnvironment(domain.MessagingConfiguration{
		Telegram: domain.TelegramMessagingConfiguration{
			BotToken: "telegram-secret", AllowedUsers: []string{"42"},
			GroupAllowedUsers: []string{"43"}, GroupAllowedChats: []string{"-100123"},
			ProxyURL: "socks5://127.0.0.1:9050",
		},
		WhatsApp: domain.WhatsAppMessagingConfiguration{
			Mode: "bot", AllowedUsers: []string{"628123456789"},
		},
	})
	for _, key := range []string{
		"TELEGRAM_BOT_TOKEN", "TELEGRAM_ALLOWED_USERS", "TELEGRAM_GROUP_ALLOWED_USERS",
		"TELEGRAM_GROUP_ALLOWED_CHATS", "TELEGRAM_PROXY", "WHATSAPP_ALLOWED_USERS",
	} {
		if environment[key] != "" {
			t.Fatalf("disabled channel retained %s=%q", key, environment[key])
		}
	}
	if environment["WHATSAPP_ENABLED"] != "false" || environment["WHATSAPP_MODE"] != "bot" {
		t.Fatalf("unexpected disabled WhatsApp environment: %+v", environment)
	}
}

func TestUpdateEnvContentWithKeysPreservesUnmanagedValues(t *testing.T) {
	original := []byte("API_SERVER_KEY=\"keep-me\"\nTELEGRAM_BOT_TOKEN=\"old\"\nCUSTOM_VALUE=\"preserve-me\"\n")
	updated, err := updateEnvContentWithKeys(original, map[string]string{
		"TELEGRAM_BOT_TOKEN": "new$token",
		"WHATSAPP_ENABLED":   "true",
	}, []string{"TELEGRAM_BOT_TOKEN", "WHATSAPP_ENABLED"})
	if err != nil {
		t.Fatalf("updateEnvContentWithKeys() error=%v", err)
	}
	contents := string(updated)
	for _, expected := range []string{
		"API_SERVER_KEY=\"keep-me\"",
		"CUSTOM_VALUE=\"preserve-me\"",
		"TELEGRAM_BOT_TOKEN=\"new$$token\"",
		"WHATSAPP_ENABLED=\"true\"",
	} {
		if !strings.Contains(contents, expected) {
			t.Fatalf("updated environment does not contain %q:\n%s", expected, contents)
		}
	}
	if strings.Contains(contents, `TELEGRAM_BOT_TOKEN="old"`) {
		t.Fatalf("updated environment retained the old Telegram token:\n%s", contents)
	}
}

func TestVerifyMessagingYAMLObservation(t *testing.T) {
	expected := messagingYAMLSettings{
		TelegramRequireMention:         true,
		WhatsAppUnauthorizedDMBehavior: "pair",
		WhatsAppReplyPrefix:            "Hermes",
		ConfigurationRevision:          strings.Repeat("a", 64),
	}
	output := fmt.Sprintf(
		`{"telegram_require_mention":true,"whatsapp_unauthorized_dm_behavior":"pair","whatsapp_reply_prefix":"Hermes","schema_version":1,"configuration_revision":%q}`,
		expected.ConfigurationRevision,
	)
	if err := verifyMessagingYAMLObservation(output, expected); err != nil {
		t.Fatalf("verifyMessagingYAMLObservation() error=%v", err)
	}
	if err := verifyMessagingYAMLObservation(strings.Replace(output, `"schema_version":1`, `"schema_version":2`, 1), expected); err == nil {
		t.Fatal("verifyMessagingYAMLObservation() accepted the wrong readiness schema")
	}
}

func TestMCPRuntimeConfigurationKeepsSecretsOutOfYAMLAndBlocksLocalExecution(t *testing.T) {
	for _, expected := range []string{
		`"tools": {"include": server["tools"], "resources": False, "prompts": False}`,
		`"trust": "untrusted"`,
		`"sampling": {"enabled": False}`,
		`"elicitation": {"enabled": False}`,
		`entry["headers"] = {"Authorization": "Bearer ${" + variable + "}"}`,
		`managed_env.append(variable + "=" + json.dumps(server["bearer_token"]))`,
		`raw["mcp_servers"] = mcp_servers`,
	} {
		if !strings.Contains(mcpConfigApplyScript, expected) {
			t.Fatalf("MCP apply script does not enforce %q", expected)
		}
	}
	for _, forbidden := range []string{"subprocess", "os.system", "shell=True", `"command"`, `"args"`} {
		if strings.Contains(mcpConfigApplyScript, forbidden) {
			t.Fatalf("MCP apply script contains an arbitrary execution path %q", forbidden)
		}
	}

	payload := domain.MCPApplyPayload{
		ImageID:    "sha256:" + strings.Repeat("c", 64),
		DataVolume: "hermes-fleet-instance-00000000-data",
	}
	stopped := (&Provisioner{}).mcpPythonCommand(payload, "", "print('bounded')")
	joined := strings.Join(stopped, " ")
	for _, expected := range []string{
		"run --rm -i --network none", "--cap-drop ALL", "no-new-privileges",
		payload.DataVolume + ":/data", payload.ImageID, "print('bounded')",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("stopped MCP mutation command does not contain %q: %v", expected, stopped)
		}
	}
	running := (&Provisioner{}).mcpPythonCommand(payload, "owned-hermes", "print('bounded')")
	wantRunning := []string{"exec", "-i", "owned-hermes", "python", "-c", "print('bounded')"}
	if strings.Join(running, "\x00") != strings.Join(wantRunning, "\x00") {
		t.Fatalf("running MCP mutation command=%v, want %v", running, wantRunning)
	}
}

func TestMCPConnectionTestUsesOnlyTheExactManagedRuntime(t *testing.T) {
	payload := domain.MCPApplyPayload{
		DesiredStatus: domain.InstanceStopped,
		ImageID:       "sha256:" + strings.Repeat("d", 64),
		DataVolume:    "hermes-fleet-instance-00000000-data",
		ProjectName:   "hermes-fleet-instance-00000000",
	}
	p, err := New(t.TempDir(), "docker")
	if err != nil {
		t.Fatal(err)
	}
	var command []string
	p.dockerRun = func(_ context.Context, args ...string) (string, error) {
		command = append([]string(nil), args...)
		return "connection ok", nil
	}
	if _, err := p.testMCPServer(context.Background(), payload, "/managed/instance", "knowledge"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(command, " ")
	for _, expected := range []string{
		"run --rm", "--cap-drop ALL", "no-new-privileges", payload.DataVolume + ":/data",
		"--entrypoint hermes " + payload.ImageID + " mcp test knowledge",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("MCP connection test does not contain %q: %v", expected, command)
		}
	}
	if strings.Contains(joined, " sh ") || strings.Contains(joined, " bash ") {
		t.Fatalf("MCP connection test unexpectedly invokes a shell: %v", command)
	}
}

func TestProvisionRejectsUnsafeControlPlanePayload(t *testing.T) {
	provisioner, err := New(t.TempDir(), "docker-must-not-run")
	if err != nil {
		t.Fatal(err)
	}
	payload := domain.ProvisionPayload{
		InstanceID: "00000000-0000-4000-8000-000000000001",
		Name:       "fleet-test-01", Image: "runtime:latest\n    volumes: [/tmp:/host]",
		Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
		APIPort: 8650, DashboardPort: 9130,
	}
	result := provisioner.provision(context.Background(), payload)
	if result.Success || !strings.Contains(result.Error, "unsafe Hermes image") {
		t.Fatalf("provision() result=%+v", result)
	}
}

func TestPrepareHermesImageRefusesToMoveExistingUnverifiedTag(t *testing.T) {
	p, err := New(t.TempDir(), "docker")
	if err != nil {
		t.Fatal(err)
	}
	targetSource := "8bcdef6ef2bcbaa22bd23b72efe60906123a4f66"
	payload := domain.HermesUpgradePayload{
		TargetVersion: "0.19.0",
		TargetSource:  targetSource,
		TargetImage:   runtimeassets.ImageReference("0.19.0", targetSource),
	}
	imageID := "sha256:" + strings.Repeat("a", 64)
	buildCalls := 0
	p.dockerRun = func(_ context.Context, args ...string) (string, error) {
		switch {
		case args[0] == "image" && args[1] == "inspect":
			return imageID + "\n" + payload.TargetVersion + "\n" + payload.TargetSource + "\nwrong-runtime-build\n", nil
		case args[0] == "image" && args[1] == "ls":
			return imageID + "\n", nil
		case args[0] == "build":
			buildCalls++
			return "", nil
		default:
			return "", fmt.Errorf("unexpected Docker command: %v", args)
		}
	}

	_, err = p.prepareHermesImage(context.Background(), payload)
	if err == nil || !strings.Contains(err.Error(), "already exists") ||
		!strings.Contains(err.Error(), "runtime wrapper") {
		t.Fatalf("prepareHermesImage() error=%v, want immutable existing-tag refusal", err)
	}
	if buildCalls != 0 {
		t.Fatalf("prepareHermesImage() moved an existing unverified tag with %d build calls", buildCalls)
	}
}

func TestPrepareHermesImageBuildsOnlyWhenCanonicalTagIsAbsent(t *testing.T) {
	p, err := New(t.TempDir(), "docker")
	if err != nil {
		t.Fatal(err)
	}
	targetSource := "8bcdef6ef2bcbaa22bd23b72efe60906123a4f66"
	payload := domain.HermesUpgradePayload{
		TargetVersion: "0.19.0",
		TargetSource:  targetSource,
		TargetImage:   runtimeassets.ImageReference("0.19.0", targetSource),
	}
	imageID := "sha256:" + strings.Repeat("b", 64)
	inspectCalls, buildCalls := 0, 0
	p.dockerRun = func(_ context.Context, args ...string) (string, error) {
		switch {
		case args[0] == "image" && args[1] == "inspect":
			inspectCalls++
			// Pemeriksaan pertama berjalan sebelum kunci build, yang kedua di
			// dalamnya; keduanya harus gagal sebelum Fleet membangun image.
			if inspectCalls <= 2 {
				return "No such image", errors.New("not found")
			}
			return imageID + "\n" + payload.TargetVersion + "\n" + payload.TargetSource + "\n" + runtimeassets.BuildID() + "\n", nil
		case args[0] == "image" && args[1] == "ls":
			return "", nil
		case args[0] == "build":
			buildCalls++
			for _, arg := range args[1:] {
				if arg == "--pull" || arg == "--pull=true" {
					t.Fatalf("prepareHermesImage() pulled a moving base tag: %v", args)
				}
			}
			return "built", nil
		default:
			return "", fmt.Errorf("unexpected Docker command: %v", args)
		}
	}

	actualImageID, err := p.prepareHermesImage(context.Background(), payload)
	if err != nil || actualImageID != imageID {
		t.Fatalf("prepareHermesImage() imageID=%q error=%v", actualImageID, err)
	}
	if buildCalls != 1 || inspectCalls != 3 {
		t.Fatalf("prepareHermesImage() inspectCalls=%d buildCalls=%d", inspectCalls, buildCalls)
	}
}

func TestPrepareHermesImageSkipsTheBuildLockWhenTheReleaseIsAlreadyVerified(t *testing.T) {
	p, err := New(t.TempDir(), "docker")
	if err != nil {
		t.Fatal(err)
	}
	targetSource := "8bcdef6ef2bcbaa22bd23b72efe60906123a4f66"
	payload := domain.HermesUpgradePayload{
		TargetVersion: "0.19.0",
		TargetSource:  targetSource,
		TargetImage:   runtimeassets.ImageReference("0.19.0", targetSource),
	}
	imageID := "sha256:" + strings.Repeat("c", 64)
	inspectCalls := 0
	p.dockerRun = func(_ context.Context, args ...string) (string, error) {
		switch {
		case args[0] == "image" && args[1] == "inspect":
			inspectCalls++
			return imageID + "\n" + payload.TargetVersion + "\n" + payload.TargetSource + "\n" + runtimeassets.BuildID() + "\n", nil
		default:
			return "", fmt.Errorf("unexpected Docker command: %v", args)
		}
	}

	// Kunci build ditahan seolah instance lain sedang membangun rilis berbeda.
	p.imageBuildMu.Lock()
	defer p.imageBuildMu.Unlock()

	done := make(chan struct{})
	var actualImageID string
	var prepareErr error
	go func() {
		actualImageID, prepareErr = p.prepareHermesImage(context.Background(), payload)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("prepareHermesImage() queued behind an unrelated runtime build")
	}
	if prepareErr != nil || actualImageID != imageID {
		t.Fatalf("prepareHermesImage() imageID=%q error=%v", actualImageID, prepareErr)
	}
	if inspectCalls != 1 {
		t.Fatalf("prepareHermesImage() inspectCalls=%d, want a single verification", inspectCalls)
	}
}

func TestProvisionPreparesSelectedHermesReleaseOnDemand(t *testing.T) {
	p, err := New(t.TempDir(), "docker")
	if err != nil {
		t.Fatal(err)
	}
	source := "8bcdef6ef2bcbaa22bd23b72efe60906123a4f66"
	image := runtimeassets.ImageReference("0.19.0", source)
	imageID := "sha256:" + strings.Repeat("d", 64)
	inspectCalls, buildCalls := 0, 0
	p.dockerRun = func(_ context.Context, args ...string) (string, error) {
		switch {
		case args[0] == "image" && args[1] == "inspect":
			inspectCalls++
			if inspectCalls <= 2 {
				return "not found", errors.New("not found")
			}
			return imageID + "\n0.19.0\n" + source + "\n" + runtimeassets.BuildID() + "\n", nil
		case args[0] == "image" && args[1] == "ls":
			return "", nil
		case args[0] == "build":
			buildCalls++
			return "built", nil
		default:
			return "", fmt.Errorf("unexpected Docker command: %v", args)
		}
	}
	p.portCheck = func(int) error { return nil }
	result := p.provision(context.Background(), domain.ProvisionPayload{
		InstanceID: "00000000-0000-4000-8000-000000000001", Name: "fleet-test-01",
		Image: image, HermesVersion: "0.19.0", HermesSource: source, Provider: "openai-codex",
		APIPort: 18650, DashboardPort: 19130,
	})
	if result.Success || !strings.Contains(result.Error, "Docker Compose provisioning failed") {
		t.Fatalf("provision() result=%+v", result)
	}
	if buildCalls != 1 || inspectCalls != 3 {
		t.Fatalf("on-demand preparation inspectCalls=%d buildCalls=%d", inspectCalls, buildCalls)
	}
}

func TestUpgradeHermesVerifiesPinnedTargetAndReturnsInstanceStopped(t *testing.T) {
	root := t.TempDir()
	managedPath := filepath.Join(root, "fleet-test-01-00000000")
	if err := os.MkdirAll(filepath.Join(managedPath, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"compose.yaml", ".env"} {
		if err := os.WriteFile(filepath.Join(managedPath, name), []byte("current\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	artifact := filepath.Join(root, "rollback.tar")
	artifactData := []byte("verified rollback artifact")
	if err := os.WriteFile(artifact, artifactData, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(artifactData)
	currentImageID := "sha256:" + strings.Repeat("a", 64)
	targetImageID := "sha256:" + strings.Repeat("b", 64)
	targetImage := "local/hermes-fleet-runtime:0.19.0"
	targetSource := "8bcdef6ef2bcbaa22bd23b72efe60906123a4f66"
	createdAt := time.Now().UTC()
	payload := domain.HermesUpgradePayload{
		InstanceID: "00000000-0000-4000-8000-000000000001", Name: "fleet-test-01",
		CurrentImage: "local/hermes-fleet-runtime:0.18.2", CurrentImageID: currentImageID,
		TargetImage: targetImage, TargetVersion: "0.19.0", TargetSource: targetSource,
		RecoveryPointID: "recovery-" + strings.Repeat("c", 32), Provider: "openai-codex",
		ProjectName: "hermes-fleet-fleet-test-01-00000000", DataVolume: "hermes-fleet-fleet-test-01-00000000-data",
		ManagedPath: managedPath, APIPort: 28650, DashboardPort: 29130,
	}
	payload.Rollback = domain.RecoveryRestorePayload{
		RecoveryPointID: payload.RecoveryPointID, InstanceID: payload.InstanceID, Name: payload.Name,
		Image: payload.CurrentImage, ImageID: currentImageID, RequireImageID: true, Provider: payload.Provider,
		ProjectName: payload.ProjectName, DataVolume: payload.DataVolume, ManagedPath: managedPath,
		AgentVersion: "0.10.0", CreatedAt: createdAt, RecoverySHA256: hex.EncodeToString(digest[:]),
		RecoverySizeBytes: int64(len(artifactData)), MaxBytes: 1 << 20,
	}

	provisioner, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	provisioner.httpClient = &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	running, upgraded := false, false
	var commands [][]string
	provisioner.dockerRun = func(_ context.Context, args ...string) (string, error) {
		commands = append(commands, append([]string(nil), args...))
		switch args[0] {
		case "compose":
			if argumentsContain(args, "config") && argumentsContain(args, "--images") {
				return payload.CurrentImage + "\n" + payload.CurrentImage + "\n", nil
			}
			if argumentsContain(args, "up") {
				upgraded, running = true, true
				return "", nil
			}
			if argumentsContain(args, "stop") {
				running = false
				return "", nil
			}
		case "ps":
			return "aaaaaaaaaaaa\nbbbbbbbbbbbb\n", nil
		case "inspect":
			status := "exited"
			if running {
				status = "running"
			}
			imageID := currentImageID
			if upgraded {
				imageID = targetImageID
			}
			containers := []map[string]any{
				upgradeTestContainer("aaaaaaaaaaaa", "hermes", imageID, status, payload),
				upgradeTestContainer("bbbbbbbbbbbb", "dashboard", imageID, status, payload),
			}
			encoded, marshalErr := json.Marshal(containers)
			return string(encoded), marshalErr
		case "volume":
			return payload.ProjectName + "\n", nil
		case "image":
			if args[len(args)-1] == currentImageID {
				return currentImageID + "\n", nil
			}
			if args[len(args)-1] == payload.CurrentImage {
				return currentImageID + "\n", nil
			}
			if args[len(args)-1] == targetImage {
				return targetImageID + "\n" + payload.TargetVersion + "\n" + payload.TargetSource + "\n" + runtimeassets.BuildID() + "\n", nil
			}
		}
		return "", fmt.Errorf("unexpected Docker command: %v", args)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	result := provisioner.Execute(context.Background(), domain.Job{Type: "instance.hermes.upgrade", Payload: encoded, InputArtifact: artifact})
	if !result.Success || result.ImageID != targetImageID || result.InstanceStatus != domain.InstanceStopped {
		t.Fatalf("Hermes update result=%+v", result)
	}
	if running || !upgraded || !hasComposeAction(commands, "stop") {
		t.Fatalf("Hermes update did not restore the stopped lifecycle: commands=%v", commands)
	}
	manifest, err := os.ReadFile(filepath.Join(managedPath, "compose.yaml"))
	if err != nil || !strings.Contains(string(manifest), `image: "`+targetImage+`"`) {
		t.Fatalf("updated manifest error=%v contents=%s", err, manifest)
	}
}

func TestUpgradeHermesContinuesWhenComposeAlreadyUsesTarget(t *testing.T) {
	root := t.TempDir()
	managedPath := filepath.Join(root, "fleet-test-01-00000000")
	if err := os.MkdirAll(filepath.Join(managedPath, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(root, "rollback.tar")
	artifactData := []byte("verified rollback artifact")
	if err := os.WriteFile(artifact, artifactData, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(artifactData)
	currentImageID := "sha256:" + strings.Repeat("a", 64)
	targetImageID := "sha256:" + strings.Repeat("b", 64)
	targetImage := "local/hermes-fleet-runtime:0.19.0"
	targetSource := "8bcdef6ef2bcbaa22bd23b72efe60906123a4f66"
	createdAt := time.Now().UTC()
	payload := domain.HermesUpgradePayload{
		InstanceID: "00000000-0000-4000-8000-000000000001", Name: "fleet-test-01",
		CurrentImage: "local/hermes-fleet-runtime:0.18.2", CurrentImageID: currentImageID,
		TargetImage: targetImage, TargetVersion: "0.19.0", TargetSource: targetSource,
		RecoveryPointID: "recovery-" + strings.Repeat("c", 32), Provider: "openai-codex",
		ProjectName: "hermes-fleet-fleet-test-01-00000000", DataVolume: "hermes-fleet-fleet-test-01-00000000-data",
		ManagedPath: managedPath, APIPort: 28650, DashboardPort: 29130,
	}
	payload.Rollback = domain.RecoveryRestorePayload{
		RecoveryPointID: payload.RecoveryPointID, InstanceID: payload.InstanceID, Name: payload.Name,
		Image: payload.CurrentImage, ImageID: currentImageID, RequireImageID: true, Provider: payload.Provider,
		ProjectName: payload.ProjectName, DataVolume: payload.DataVolume, ManagedPath: managedPath,
		AgentVersion: "0.10.0", CreatedAt: createdAt, RecoverySHA256: hex.EncodeToString(digest[:]),
		RecoverySizeBytes: int64(len(artifactData)), MaxBytes: 1 << 20,
	}
	targetManifest := renderCompose(domain.ProvisionPayload{
		InstanceID: payload.InstanceID, Name: payload.Name, Image: targetImage, Provider: payload.Provider,
		APIPort: payload.APIPort, DashboardPort: payload.DashboardPort,
	}, payload.ProjectName, payload.DataVolume)
	for _, name := range []string{".env"} {
		if err := os.WriteFile(filepath.Join(managedPath, name), []byte("current\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(managedPath, "compose.yaml"), []byte(targetManifest), 0o600); err != nil {
		t.Fatal(err)
	}

	provisioner, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	provisioner.httpClient = &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	running, upgraded := false, false
	var commands [][]string
	provisioner.dockerRun = func(_ context.Context, args ...string) (string, error) {
		commands = append(commands, append([]string(nil), args...))
		switch args[0] {
		case "compose":
			if argumentsContain(args, "config") && argumentsContain(args, "--images") {
				return payload.TargetImage + "\n" + payload.TargetImage + "\n", nil
			}
			if argumentsContain(args, "up") {
				upgraded, running = true, true
				return "", nil
			}
			if argumentsContain(args, "stop") {
				running = false
				return "", nil
			}
		case "ps":
			return "aaaaaaaaaaaa\nbbbbbbbbbbbb\n", nil
		case "inspect":
			status := "exited"
			if running {
				status = "running"
			}
			imageID := currentImageID
			if upgraded {
				imageID = targetImageID
			}
			containers := []map[string]any{
				upgradeTestContainer("aaaaaaaaaaaa", "hermes", imageID, status, payload),
				upgradeTestContainer("bbbbbbbbbbbb", "dashboard", imageID, status, payload),
			}
			encoded, marshalErr := json.Marshal(containers)
			return string(encoded), marshalErr
		case "volume":
			return payload.ProjectName + "\n", nil
		case "image":
			if args[len(args)-1] == currentImageID {
				return currentImageID + "\n", nil
			}
			if args[len(args)-1] == payload.CurrentImage {
				return currentImageID + "\n", nil
			}
			if args[len(args)-1] == targetImage {
				return targetImageID + "\n" + payload.TargetVersion + "\n" + payload.TargetSource + "\n" + runtimeassets.BuildID() + "\n", nil
			}
		}
		return "", fmt.Errorf("unexpected Docker command: %v", args)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	result := provisioner.Execute(context.Background(), domain.Job{Type: "instance.hermes.upgrade", Payload: encoded, InputArtifact: artifact})
	if !result.Success || result.ImageID != targetImageID || result.InstanceStatus != domain.InstanceStopped {
		t.Fatalf("partial Hermes update result=%+v", result)
	}
	if running || !upgraded || !hasComposeAction(commands, "stop") {
		t.Fatalf("partial Hermes update did not recreate the target runtime: commands=%v", commands)
	}
}

func TestUpgradeHermesRestoresVerifiedBackupAfterTargetFailure(t *testing.T) {
	root := t.TempDir()
	managedPath := filepath.Join(root, "fleet-test-01-00000000")
	if err := os.MkdirAll(filepath.Join(managedPath, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	currentImageID := "sha256:" + strings.Repeat("a", 64)
	targetImageID := "sha256:" + strings.Repeat("b", 64)
	currentImage := "local/hermes-fleet-runtime:0.18.2"
	targetImage := "local/hermes-fleet-runtime:0.19.0"
	payload := domain.HermesUpgradePayload{
		InstanceID: "00000000-0000-4000-8000-000000000001", Name: "fleet-test-01",
		CurrentImage: currentImage, CurrentImageID: currentImageID,
		TargetImage: targetImage, TargetVersion: "0.19.0", TargetSource: "8bcdef6ef2bcbaa22bd23b72efe60906123a4f66",
		RecoveryPointID: "recovery-" + strings.Repeat("c", 32), Provider: "openai-codex",
		ProjectName: "hermes-fleet-fleet-test-01-00000000", DataVolume: "hermes-fleet-fleet-test-01-00000000-data",
		ManagedPath: managedPath, APIPort: 28650, DashboardPort: 29130,
	}
	payload.Rollback = domain.RecoveryRestorePayload{
		RecoveryPointID: payload.RecoveryPointID, InstanceID: payload.InstanceID, Name: payload.Name,
		Image: currentImage, ImageID: currentImageID, RequireImageID: true, Provider: payload.Provider,
		ProjectName: payload.ProjectName, DataVolume: payload.DataVolume, ManagedPath: managedPath,
		AgentVersion: "0.10.0", CreatedAt: time.Now().UTC(), MaxBytes: 10 << 20,
	}
	currentManifest := renderCompose(domain.ProvisionPayload{
		InstanceID: payload.InstanceID, Name: payload.Name, Image: currentImage, Provider: payload.Provider,
		APIPort: payload.APIPort, DashboardPort: payload.DashboardPort,
	}, payload.ProjectName, payload.DataVolume)
	for filename, contents := range map[string]string{
		".env": "STATE=current\n", "compose.yaml": currentManifest,
	} {
		if err := os.WriteFile(filepath.Join(managedPath, filename), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archive := restoreArchiveForProvisioner(t, payload.Rollback, map[string]string{
		".env": "STATE=restored\n", "compose.yaml": currentManifest,
	}, recoveryVolumeTar(t))
	artifact := filepath.Join(root, "rollback.tar")
	if err := os.WriteFile(artifact, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(archive)
	payload.Rollback.RecoverySHA256 = hex.EncodeToString(digest[:])
	payload.Rollback.RecoverySizeBytes = int64(len(archive))

	provisioner, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	provisioner.httpClient = &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	running, upAttempts := false, 0
	provisioner.dockerRun = func(_ context.Context, args ...string) (string, error) {
		switch args[0] {
		case "compose":
			if argumentsContain(args, "config") && argumentsContain(args, "--images") {
				return currentImage + "\n" + currentImage + "\n", nil
			}
			if argumentsContain(args, "up") {
				upAttempts++
				if upAttempts == 1 {
					return "target failed", errors.New("target failed")
				}
				running = true
				return "", nil
			}
			if argumentsContain(args, "stop") {
				running = false
			}
			return "", nil
		case "ps":
			return "aaaaaaaaaaaa\nbbbbbbbbbbbb\n", nil
		case "inspect":
			status := "exited"
			if running {
				status = "running"
			}
			containers := []map[string]any{
				upgradeTestContainer("aaaaaaaaaaaa", "hermes", currentImageID, status, payload),
				upgradeTestContainer("bbbbbbbbbbbb", "dashboard", currentImageID, status, payload),
			}
			encoded, marshalErr := json.Marshal(containers)
			return string(encoded), marshalErr
		case "volume":
			if argumentsContain(args, "{{json .Labels}}") {
				return `{"io.hermes-fleet.managed":"true","io.hermes-fleet.instance-id":"` + payload.InstanceID + `"}`, nil
			}
			return payload.ProjectName + "\n", nil
		case "image":
			if argumentsContain(args, "{{json .}}") {
				return recoveryRuntimeImageInspect(t, currentImageID, "0.18.2", strings.Repeat("f", 40)), nil
			}
			switch args[len(args)-1] {
			case currentImage, currentImageID:
				return currentImageID + "\n", nil
			case targetImage:
				return targetImageID + "\n" + payload.TargetVersion + "\n" + payload.TargetSource + "\n" + runtimeassets.BuildID() + "\n", nil
			}
		case "run":
			if argumentsContain(args, "-cf") {
				return string(recoveryVolumeTar(t)), nil
			}
			return "", nil
		}
		return "", fmt.Errorf("unexpected Docker command: %v", args)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	result := provisioner.Execute(context.Background(), domain.Job{Type: "instance.hermes.upgrade", Payload: encoded, InputArtifact: artifact})
	if result.Success || result.InstanceStatus != domain.InstanceStopped ||
		!strings.Contains(result.Error, "verified backup and original runtime were restored") {
		t.Fatalf("Hermes update rollback result=%+v", result)
	}
	if running || upAttempts != 2 {
		t.Fatalf("rollback lifecycle running=%v upAttempts=%d", running, upAttempts)
	}
	restoredEnvironment, err := os.ReadFile(filepath.Join(managedPath, ".env"))
	if err != nil || string(restoredEnvironment) != "STATE=restored\n" {
		t.Fatalf("restored environment error=%v contents=%q", err, restoredEnvironment)
	}
}

func upgradeTestContainer(id, service, imageID, status string, payload domain.HermesUpgradePayload) map[string]any {
	return map[string]any{
		"Id": id, "Image": imageID,
		"Config": map[string]any{"Labels": map[string]string{
			"io.hermes-fleet.managed": "true", "io.hermes-fleet.instance-id": payload.InstanceID,
			"com.docker.compose.project": payload.ProjectName, "com.docker.compose.service": service,
		}},
		"State": map[string]any{"Status": status},
	}
}

func argumentsContain(arguments []string, expected string) bool {
	for _, argument := range arguments {
		if argument == expected {
			return true
		}
	}
	return false
}

func TestSafeManagedPathRejectsOutsideRoot(t *testing.T) {
	provisioner, err := New(t.TempDir(), "docker")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provisioner.safeManagedPath("/tmp/outside-fleet-root"); err == nil {
		t.Fatal("safeManagedPath() accepted an external path")
	}
}

func TestInterruptedRestoreQuarantinesOnlyAffectedInstance(t *testing.T) {
	root := t.TempDir()
	affectedName := "fleet-affected-00000000"
	unaffectedName := "fleet-unaffected-00000000"
	for _, name := range []string{affectedName, unaffectedName} {
		managedPath := filepath.Join(root, name)
		if err := os.MkdirAll(managedPath, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(managedPath, "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rollbackName := "." + affectedName + ".restore-backup-deadbeef"
	if err := os.Mkdir(filepath.Join(root, rollbackName), 0o700); err != nil {
		t.Fatal(err)
	}

	provisioner, err := New(root, "docker")
	if err != nil {
		t.Fatalf("New() failed because one instance has an interrupted restore: %v", err)
	}
	if _, err := provisioner.safeManagedPath(filepath.Join(root, unaffectedName)); err != nil {
		t.Fatalf("unaffected instance was quarantined: %v", err)
	}
	if _, err := provisioner.safeManagedPath(filepath.Join(root, affectedName)); err == nil ||
		!strings.Contains(err.Error(), "quarantined after an interrupted restore") {
		t.Fatalf("affected instance error=%v, want interrupted restore quarantine", err)
	}
	if _, err := os.Stat(filepath.Join(root, rollbackName)); err != nil {
		t.Fatalf("rollback workspace must be preserved for manual recovery: %v", err)
	}
}

func TestCreateRecoveryPointRequiresStoppedFleetResourcesAndEncryptsStagingFiles(t *testing.T) {
	root := t.TempDir()
	managedPath := filepath.Join(root, "fleet-test-01-00000000")
	if err := os.MkdirAll(filepath.Join(managedPath, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		".env": "DASHBOARD_PASSWORD=workspace-secret\n", "compose.yaml": "services: {}\n",
		"workspace/oauth.json": `{"token":"oauth-secret"}`,
	} {
		if err := os.WriteFile(filepath.Join(managedPath, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	projectName := "hermes-fleet-fleet-test-01-00000000"
	imageID := "sha256:" + strings.Repeat("a", 64)
	volumeTar := recoveryVolumeTar(t)
	p.volumeSize = func(context.Context, string, string, int64) (int64, error) {
		return int64(len(volumeTar)), nil
	}
	p.diskAvailable = func(string) (uint64, error) { return ^uint64(0), nil }
	p.dockerRun = func(_ context.Context, args ...string) (string, error) {
		switch args[0] {
		case "compose":
			if slicesContain(args, "--all") {
				return strings.Repeat("c", 64) + "\n", nil
			}
			return "", nil
		case "inspect":
			return imageID + "\n", nil
		case "volume":
			return projectName + "\n", nil
		case "image":
			return imageID + "\n", nil
		case "run":
			return string(volumeTar), nil
		default:
			return "", fmt.Errorf("unexpected Docker command: %v", args)
		}
	}
	payload := domain.RecoveryPointPayload{
		RecoveryPointID: "recovery-" + strings.Repeat("b", 32),
		InstanceID:      "00000000-0000-4000-8000-000000000001", Name: "fleet-test-01",
		Image: "local/hermes-fleet-runtime:0.18.2", ImageID: imageID,
		Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
		CodexConfigured: true,
		ProjectName:     projectName, DataVolume: projectName + "-data", ManagedPath: managedPath,
		AgentVersion: "0.6.4", CreatedAt: time.Now().UTC(), MaxBytes: 10 << 20,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	result := p.Execute(context.Background(), domain.Job{Type: "instance.recovery.create", Payload: encoded})
	if !result.Success {
		t.Fatalf("create recovery point result=%+v", result)
	}
	defer os.Remove(result.RecoveryArtifact)
	encrypted, err := os.ReadFile(result.RecoveryArtifact)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte("workspace-secret")) || bytes.Contains(encrypted, []byte("oauth-secret")) {
		t.Fatal("Host Agent staging artifact contains plaintext secrets")
	}
	var plaintext bytes.Buffer
	if written, err := recoverycodec.Decrypt(
		context.Background(), &plaintext, bytes.NewReader(encrypted), result.RecoveryKey, payload.RecoveryPointID+":artifact",
	); err != nil || written != result.RecoverySizeBytes {
		t.Fatalf("decrypt staging artifact written=%d err=%v", written, err)
	}
	plaintextPath := filepath.Join(t.TempDir(), "recovery.tar")
	if err := os.WriteFile(plaintextPath, plaintext.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	restorePayload := domain.RecoveryRestorePayload{
		RecoveryPointID: payload.RecoveryPointID, InstanceID: payload.InstanceID, Name: payload.Name,
		Image: payload.Image, ImageID: payload.ImageID, Provider: payload.Provider, Model: payload.Model,
		Reasoning: payload.Reasoning, ServiceTier: payload.ServiceTier, CodexConfigured: payload.CodexConfigured, ProjectName: payload.ProjectName,
		DataVolume: payload.DataVolume, ManagedPath: payload.ManagedPath, AgentVersion: payload.AgentVersion,
		CreatedAt: payload.CreatedAt, RecoverySizeBytes: result.RecoverySizeBytes, MaxBytes: payload.MaxBytes,
	}
	stagingRoot := t.TempDir()
	restoredWorkspace, restoredVolume, err := extractRestoreArchive(context.Background(), plaintextPath, stagingRoot, restorePayload)
	if err != nil {
		t.Fatalf("extractRestoreArchive() error=%v", err)
	}
	restoredEnvironment, err := os.ReadFile(filepath.Join(restoredWorkspace, ".env"))
	if err != nil || !bytes.Contains(restoredEnvironment, []byte("workspace-secret")) {
		t.Fatalf("restored environment error=%v contents=%q", err, restoredEnvironment)
	}
	if err := validateVolumeArchive(restoredVolume, restorePayload.MaxBytes); err != nil {
		t.Fatalf("restored volume validation error=%v", err)
	}
	tamperedPayload := restorePayload
	tamperedPayload.Model = "different-model"
	if _, _, err := extractRestoreArchive(context.Background(), plaintextPath, t.TempDir(), tamperedPayload); err == nil {
		t.Fatal("extractRestoreArchive() accepted a manifest for different desired state")
	}
	found := map[string]bool{}
	archive := tar.NewReader(bytes.NewReader(plaintext.Bytes()))
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found[header.Name] = true
		if _, err := io.Copy(io.Discard, archive); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"manifest.json", "workspace/.env", "workspace/compose.yaml", "workspace/workspace/oauth.json", "data-volume.tar"} {
		if !found[name] {
			t.Fatalf("recovery archive is missing %q: entries=%v", name, found)
		}
	}

	p.diskAvailable = func(string) (uint64, error) { return 0, nil }
	result = p.createRecoveryPoint(context.Background(), payload)
	if result.Success || !strings.Contains(result.Error, "insufficient disk space") {
		t.Fatalf("low disk recovery result=%+v", result)
	}
	p.diskAvailable = func(string) (uint64, error) { return ^uint64(0), nil }
	p.dockerRun = func(_ context.Context, args ...string) (string, error) {
		if args[0] == "compose" {
			return "running-container-id\n", nil
		}
		return "", fmt.Errorf("unexpected Docker command after running-state check: %v", args)
	}
	result = p.createRecoveryPoint(context.Background(), payload)
	if result.Success || !strings.Contains(result.Error, "still running") {
		t.Fatalf("running instance recovery result=%+v", result)
	}
}

func slicesContain(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func recoveryVolumeTar(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := tar.NewWriter(&buffer)
	data := []byte("persistent instance data")
	if err := archive.WriteHeader(&tar.Header{Name: "state.db", Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestValidateVolumeArchiveRejectsUnsafeHardLinksAndDuplicatePaths(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		headers []*tar.Header
	}{
		{
			name: "hard link escapes archive root",
			headers: []*tar.Header{
				{Name: "nested/link", Linkname: "../outside", Typeflag: tar.TypeLink},
			},
		},
		{
			name: "normalized duplicate path",
			headers: []*tar.Header{
				{Name: "./state.db", Mode: 0o600, Typeflag: tar.TypeReg},
				{Name: "state.db", Mode: 0o600, Typeflag: tar.TypeReg},
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "volume.tar")
			file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			archive := tar.NewWriter(file)
			for _, header := range testCase.headers {
				if err := archive.WriteHeader(header); err != nil {
					t.Fatal(err)
				}
			}
			if err := archive.Close(); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := validateVolumeArchive(filename, 1<<20); err == nil {
				t.Fatal("validateVolumeArchive() accepted an unsafe archive")
			}
		})
	}
}

func TestValidateVolumeArchiveAcceptsDockerTarDirectoryEntries(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "volume.tar")
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewWriter(file)
	for _, header := range []*tar.Header{
		{Name: "./", Mode: 0o700, Typeflag: tar.TypeDir},
		{Name: "./cache/", Mode: 0o700, Typeflag: tar.TypeDir},
		{Name: "./cache/state.db", Mode: 0o600, Typeflag: tar.TypeReg},
	} {
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := validateVolumeArchive(filename, 1<<20); err != nil {
		t.Fatalf("validateVolumeArchive() rejected Docker tar directory entries: %v", err)
	}
}

func TestCanonicalArchivePathRejectsTrailingSlash(t *testing.T) {
	if _, safe := canonicalArchivePath("state.db/"); safe {
		t.Fatal("canonicalArchivePath() accepted a trailing slash")
	}
}

func TestImportVolumeStreamsArchiveToInteractiveDocker(t *testing.T) {
	archiveBytes := recoveryVolumeTar(t)
	archivePath := filepath.Join(t.TempDir(), "volume.tar")
	if err := os.WriteFile(archivePath, archiveBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	p := &Provisioner{dockerPath: "docker"}
	p.dockerInputRun = func(_ context.Context, input io.Reader, args ...string) (string, error) {
		if len(args) == 0 || args[0] != "run" || !slicesContain(args, "-i") || !slicesContain(args, "-xf") {
			t.Fatalf("volume import command does not attach archive stdin: %v", args)
		}
		streamed, err := io.ReadAll(input)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(streamed, archiveBytes) {
			t.Fatalf("volume import streamed %d bytes, want %d", len(streamed), len(archiveBytes))
		}
		return "", nil
	}
	if err := p.importVolume(context.Background(), "fleet-test-volume", "runtime:test", archivePath); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRecoveryPointReplacesStoppedStateAndRollsBackOnImportFailure(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		failImport bool
	}{
		{name: "success"},
		{name: "rollback after import failure", failImport: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			source := strings.Repeat("c", 40)
			backupImageID := "sha256:" + strings.Repeat("a", 64)
			resolvedImageID := "sha256:" + strings.Repeat("b", 64)
			managedPath := filepath.Join(root, "fleet-test-01-00000000")
			if err := os.MkdirAll(managedPath, 0o700); err != nil {
				t.Fatal(err)
			}
			for filename, contents := range map[string]string{
				".env": "STATE=old\n", "compose.yaml": "services: {}\n", "old.txt": "old workspace",
			} {
				if err := os.WriteFile(filepath.Join(managedPath, filename), []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			payload := domain.RecoveryRestorePayload{
				RecoveryPointID: "recovery-" + strings.Repeat("c", 32),
				InstanceID:      "00000000-0000-4000-8000-000000000001", Name: "fleet-test-01",
				Image: "local/hermes-fleet-runtime:0.18.2-" + source[:12] + "-" + runtimeassets.BuildID()[:12], ImageID: backupImageID,
				Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
				ProjectName: "hermes-fleet-fleet-test-01-00000000", DataVolume: "hermes-fleet-fleet-test-01-00000000-data",
				ManagedPath: managedPath, AgentVersion: "0.10.0", CreatedAt: time.Now().UTC(), MaxBytes: 10 << 20,
			}
			archive := restoreArchiveForProvisioner(t, payload, map[string]string{
				".env": "STATE=restored\n", "compose.yaml": "services: {}\n", "restored.txt": "restored workspace",
			}, recoveryVolumeTar(t))
			artifactPath := filepath.Join(root, "restore-input.tar")
			if err := os.WriteFile(artifactPath, archive, 0o600); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(archive)
			payload.RecoverySHA256 = hex.EncodeToString(digest[:])
			payload.RecoverySizeBytes = int64(len(archive))

			p, err := New(root, "docker")
			if err != nil {
				t.Fatal(err)
			}
			p.volumeSize = func(context.Context, string, string, int64) (int64, error) {
				return int64(len(recoveryVolumeTar(t))), nil
			}
			p.diskAvailable = func(string) (uint64, error) { return ^uint64(0), nil }
			imports := 0
			p.dockerRun = func(_ context.Context, args ...string) (string, error) {
				switch args[0] {
				case "compose":
					return "", nil
				case "volume":
					return `{"io.hermes-fleet.managed":"true","io.hermes-fleet.instance-id":"00000000-0000-4000-8000-000000000001"}`, nil
				case "image":
					return recoveryRuntimeImageInspect(t, resolvedImageID, "0.18.2", source), nil
				case "run":
					if slicesContain(args, "-cf") {
						return string(recoveryVolumeTar(t)), nil
					}
					if slicesContain(args, "-xf") {
						imports++
						if testCase.failImport && imports == 1 {
							return "import failed", errors.New("import failed")
						}
					}
					return "", nil
				default:
					return "", fmt.Errorf("unexpected Docker command: %v", args)
				}
			}
			result := p.restoreRecoveryPoint(context.Background(), payload, artifactPath)
			if testCase.failImport {
				if result.Success || result.InstanceStatus != domain.InstanceStopped || !strings.Contains(result.Error, "original stopped state was restored") {
					t.Fatalf("restore rollback result=%+v", result)
				}
				contents, err := os.ReadFile(filepath.Join(managedPath, ".env"))
				if err != nil || string(contents) != "STATE=old\n" {
					t.Fatalf("rollback environment error=%v contents=%q", err, contents)
				}
				return
			}
			if !result.Success || result.InstanceStatus != domain.InstanceStopped || result.ImageID != resolvedImageID {
				t.Fatalf("restore result=%+v", result)
			}
			contents, err := os.ReadFile(filepath.Join(managedPath, ".env"))
			if err != nil || string(contents) != "STATE=restored\n" {
				t.Fatalf("restored environment error=%v contents=%q", err, contents)
			}
			if _, err := os.Stat(filepath.Join(managedPath, "old.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("old workspace file survived restore: %v", err)
			}
		})
	}
}

func TestRestoreRecoveryPointCreatesCleanHostWorkspaceAndVolume(t *testing.T) {
	root := t.TempDir()
	source := strings.Repeat("d", 40)
	backupImageID := "sha256:" + strings.Repeat("b", 64)
	resolvedImageID := "sha256:" + strings.Repeat("c", 64)
	payload := domain.RecoveryRestorePayload{
		RecoveryPointID: "recovery-" + strings.Repeat("d", 32),
		InstanceID:      "00000000-0000-4000-8000-000000000002", Name: "fleet-clean-01",
		Image: "local/hermes-fleet-runtime:0.20.0-" + source[:12] + "-" + runtimeassets.BuildID()[:12], ImageID: backupImageID,
		Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
		ProjectName: "hermes-fleet-fleet-clean-01-00000000", DataVolume: "hermes-fleet-fleet-clean-01-00000000-data",
		ManagedPath: filepath.Join(root, "fleet-clean-01-00000000"), AgentVersion: "0.12.1", CreatedAt: time.Now().UTC(), MaxBytes: 10 << 20,
	}
	archive := restoreArchiveForProvisioner(t, payload, map[string]string{
		".env": "STATE=restored\n", "compose.yaml": "services: {}\n",
	}, recoveryVolumeTar(t))
	artifactPath := filepath.Join(root, "restore-clean.tar")
	if err := os.WriteFile(artifactPath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(archive)
	payload.RecoverySHA256 = hex.EncodeToString(digest[:])
	payload.RecoverySizeBytes = int64(len(archive))
	p, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	p.diskAvailable = func(string) (uint64, error) { return ^uint64(0), nil }
	volumeCreated, volumeImported := false, false
	p.dockerRun = func(_ context.Context, args ...string) (string, error) {
		switch args[0] {
		case "volume":
			switch args[1] {
			case "inspect":
				if !volumeCreated {
					return "no such volume", errors.New("not found")
				}
				return `{"io.hermes-fleet.managed":"true","io.hermes-fleet.instance-id":"00000000-0000-4000-8000-000000000002"}`, nil
			case "ls":
				return "", nil
			case "create":
				volumeCreated = true
				return payload.DataVolume + "\n", nil
			case "rm":
				volumeCreated = false
				return payload.DataVolume, nil
			}
		case "image":
			return recoveryRuntimeImageInspect(t, resolvedImageID, "0.20.0", source), nil
		case "run":
			if slicesContain(args, "-xf") {
				if !slicesContain(args, "-i") {
					t.Fatalf("volume import command does not attach archive stdin: %v", args)
				}
				volumeImported = true
			}
			return "", nil
		case "compose":
			return "", nil
		}
		return "", fmt.Errorf("unexpected Docker command: %v", args)
	}
	result := p.restoreRecoveryPoint(context.Background(), payload, artifactPath)
	if !result.Success || result.ImageID != resolvedImageID || result.InstanceStatus != domain.InstanceStopped || !volumeCreated || !volumeImported {
		t.Fatalf("clean-host restore result=%+v volumeCreated=%t volumeImported=%t", result, volumeCreated, volumeImported)
	}
	if contents, err := os.ReadFile(filepath.Join(payload.ManagedPath, ".env")); err != nil || string(contents) != "STATE=restored\n" {
		t.Fatalf("restored workspace error=%v contents=%q", err, contents)
	}
}

func TestResolveRecoveryRuntimeImageRequiresExactIDForUpgradeRollback(t *testing.T) {
	source := strings.Repeat("e", 40)
	payload := domain.RecoveryRestorePayload{
		Image:          "local/hermes-fleet-runtime:0.20.0-" + source[:12] + "-" + runtimeassets.BuildID()[:12],
		ImageID:        "sha256:" + strings.Repeat("a", 64),
		RequireImageID: true,
	}
	p, err := New(t.TempDir(), "docker")
	if err != nil {
		t.Fatal(err)
	}
	p.dockerRun = func(_ context.Context, args ...string) (string, error) {
		if args[0] != "image" {
			return "", fmt.Errorf("unexpected Docker command: %v", args)
		}
		return recoveryRuntimeImageInspect(t, "sha256:"+strings.Repeat("b", 64), "0.20.0", source), nil
	}
	if _, err := p.resolveRecoveryRuntimeImage(context.Background(), payload); err == nil || !strings.Contains(err.Error(), "same-host rollback image") {
		t.Fatalf("resolveRecoveryRuntimeImage() error=%v", err)
	}
}

func TestResolveRecoveryRuntimeImageRejectsReleaseMismatch(t *testing.T) {
	source := strings.Repeat("e", 40)
	payload := domain.RecoveryRestorePayload{
		Image:   "local/hermes-fleet-runtime:0.20.0-" + source[:12] + "-" + runtimeassets.BuildID()[:12],
		ImageID: "sha256:" + strings.Repeat("a", 64),
	}
	p, err := New(t.TempDir(), "docker")
	if err != nil {
		t.Fatal(err)
	}
	p.dockerRun = func(_ context.Context, args ...string) (string, error) {
		if args[0] != "image" {
			return "", fmt.Errorf("unexpected Docker command: %v", args)
		}
		return recoveryRuntimeImageInspect(t, "sha256:"+strings.Repeat("b", 64), "0.19.0", source), nil
	}
	if _, err := p.resolveRecoveryRuntimeImage(context.Background(), payload); err == nil || !strings.Contains(err.Error(), "backup release identity") {
		t.Fatalf("resolveRecoveryRuntimeImage() error=%v", err)
	}
}

func recoveryRuntimeImageInspect(t *testing.T, imageID, version, source string) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"Id": imageID,
		"Config": map[string]any{"Labels": map[string]string{
			"io.hermes-fleet.hermes-version":        version,
			"io.hermes-fleet.hermes-ref":            source,
			"io.hermes-fleet.runtime-build-id":      runtimeassets.BuildID(),
			"io.hermes-fleet.runtime-config-schema": strconv.Itoa(compatibility.RuntimeSchemaCurrent),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func restoreArchiveForProvisioner(t *testing.T, payload domain.RecoveryRestorePayload, workspace map[string]string, volume []byte) []byte {
	t.Helper()
	manifest, err := json.Marshal(recovery.Manifest{
		FormatVersion: 1, RecoveryPointID: payload.RecoveryPointID, InstanceID: payload.InstanceID, InstanceName: payload.Name,
		Image: payload.Image, ImageID: payload.ImageID, Provider: payload.Provider, Model: payload.Model,
		Reasoning: payload.Reasoning, ServiceTier: payload.ServiceTier, ProjectName: payload.ProjectName,
		DataVolume: payload.DataVolume, ManagedPath: payload.ManagedPath, AgentVersion: payload.AgentVersion, CreatedAt: payload.CreatedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	archive := tar.NewWriter(&buffer)
	writeEntry := func(header *tar.Header, data []byte) {
		t.Helper()
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if len(data) != 0 {
			if _, err := archive.Write(data); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeEntry(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(manifest)), Typeflag: tar.TypeReg}, manifest)
	writeEntry(&tar.Header{Name: "workspace", Mode: 0o700, Typeflag: tar.TypeDir}, nil)
	for name, contents := range workspace {
		data := []byte(contents)
		writeEntry(&tar.Header{Name: "workspace/" + name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}, data)
	}
	writeEntry(&tar.Header{Name: "data-volume.tar", Mode: 0o600, Size: int64(len(volume)), Typeflag: tar.TypeReg}, volume)
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestInspectCredentialsReturnsOnlyAllowlistedValues(t *testing.T) {
	root := t.TempDir()
	provisioner, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	managedPath := filepath.Join(root, "fleet-test-01")
	if err := os.MkdirAll(managedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedPath, "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	contents := `HERMES_DASHBOARD_BASIC_AUTH_USERNAME=admin
HERMES_DASHBOARD_BASIC_AUTH_PASSWORD="dashboard-secret"
API_SERVER_KEY='api-secret'
SLACK_BOT_TOKEN=must-not-leak
`
	if err := os.WriteFile(filepath.Join(managedPath, ".env"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	result := provisioner.inspectCredentials(domain.ActionPayload{ManagedPath: managedPath})
	if !result.Success || result.Credentials == nil {
		t.Fatalf("inspectCredentials() result=%+v", result)
	}
	if result.Credentials.DashboardUsername != "admin" ||
		result.Credentials.DashboardPassword != "dashboard-secret" ||
		result.Credentials.APIServerKey != "api-secret" {
		t.Fatal("inspectCredentials() did not return the expected allowlisted values")
	}
	encoded := result.Credentials.DashboardUsername + result.Credentials.DashboardPassword + result.Credentials.APIServerKey
	if strings.Contains(encoded, "must-not-leak") {
		t.Fatal("inspectCredentials() leaked a non-allowlisted value")
	}
}

func TestFormatEnvAssignmentEscapesInterpolationAndQuotes(t *testing.T) {
	actual := formatEnvAssignment("OPENAI_API_KEY", `key$part"quoted`)
	if actual != `OPENAI_API_KEY="key$$part\"quoted"` {
		t.Fatalf("formatEnvAssignment() = %q", actual)
	}
}

func TestProvisionFailureRetainsManagedPathMetadata(t *testing.T) {
	root := t.TempDir()
	dockerPath := filepath.Join(root, "docker-fail")
	if err := os.WriteFile(dockerPath, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	p, err := New(root, dockerPath)
	if err != nil {
		t.Fatal(err)
	}
	p.portCheck = func(int) error { return nil }
	result := p.provision(context.Background(), domain.ProvisionPayload{
		InstanceID:    "00000000-0000-4000-8000-000000000001",
		Name:          "fleet-test-01",
		Image:         "hermes:test",
		Provider:      "openai-codex",
		Model:         "gpt-5.6-sol",
		Reasoning:     "medium",
		ServiceTier:   "normal",
		APIPort:       18650,
		DashboardPort: 19130,
	})
	if result.Success {
		t.Fatal("expected fake docker failure")
	}
	if result.ProjectName == "" || result.DataVolume == "" || result.ManagedPath == "" {
		t.Fatalf("failure lost cleanup metadata: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(result.ManagedPath, ".env")); err != nil {
		t.Fatalf("managed .env was not retained for cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(result.ManagedPath, "compose.yaml")); err != nil {
		t.Fatalf("managed compose.yaml was not retained for cleanup: %v", err)
	}
}

func TestProvisionRetryAcceptsPortsOwnedByExactFleetContainers(t *testing.T) {
	root := t.TempDir()
	payload := domain.ProvisionPayload{
		InstanceID:    "00000000-0000-4000-8000-000000000001",
		Name:          "fleet-test-01",
		Image:         "local/hermes-fleet-runtime:0.19.0",
		Provider:      "openai-codex",
		APIPort:       18650,
		DashboardPort: 19130,
	}
	projectName, dataVolume, directoryName := domain.ManagedIdentity(payload.InstanceID, payload.Name)
	managedPath := filepath.Join(root, directoryName)
	if err := os.MkdirAll(filepath.Join(managedPath, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedPath, ".env"), []byte("managed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(managedPath, "compose.yaml"),
		[]byte(renderCompose(payload, projectName, dataVolume)),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	p, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	portChecks := 0
	p.portCheck = func(int) error {
		portChecks++
		return errors.New("port is occupied")
	}
	imageID := "sha256:" + strings.Repeat("a", 64)
	p.dockerRun = func(_ context.Context, args ...string) (string, error) {
		switch args[0] {
		case "ps":
			return "aaaaaaaaaaaa\nbbbbbbbbbbbb\n", nil
		case "inspect":
			containers := []map[string]any{
				{
					"Id": "aaaaaaaaaaaa", "Image": imageID,
					"Config": map[string]any{"Labels": map[string]string{
						"io.hermes-fleet.managed": "true", "io.hermes-fleet.instance-id": payload.InstanceID,
						"com.docker.compose.project": projectName, "com.docker.compose.service": "hermes",
					}},
					"HostConfig": map[string]any{"PortBindings": map[string]any{
						"8642/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": "18650"}},
					}},
					"State": map[string]any{"Status": "running"},
				},
				{
					"Id": "bbbbbbbbbbbb", "Image": imageID,
					"Config": map[string]any{"Labels": map[string]string{
						"io.hermes-fleet.managed": "true", "io.hermes-fleet.instance-id": payload.InstanceID,
						"com.docker.compose.project": projectName, "com.docker.compose.service": "dashboard",
					}},
					"HostConfig": map[string]any{"PortBindings": map[string]any{
						"9119/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": "19130"}},
					}},
					"State": map[string]any{"Status": "running"},
				},
			}
			encoded, marshalErr := json.Marshal(containers)
			return string(encoded), marshalErr
		case "image":
			return imageID + "\n", nil
		case "compose":
			return "running", nil
		default:
			return "", fmt.Errorf("unexpected Docker command: %v", args)
		}
	}
	p.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Request:    request,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("ok")),
		}, nil
	})}

	result := p.provision(context.Background(), payload)
	if !result.Success {
		t.Fatalf("provision retry result=%+v", result)
	}
	if portChecks != 0 {
		t.Fatalf("provision retry ran %d host port checks against its own containers", portChecks)
	}
}

func TestDeleteRetiresEarlyProvisioningFailureWithoutDockerMutation(t *testing.T) {
	for _, test := range []struct {
		name          string
		emptyMetadata bool
		createPartial bool
	}{
		{name: "missing runtime with exact metadata"},
		{name: "missing runtime with legacy empty metadata", emptyMetadata: true},
		{name: "partial runtime artifacts are retained", createPartial: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			instanceID := "00000000-0000-4000-8000-000000000001"
			instanceName := "fleet-test-01"
			projectName, _, directoryName := domain.ManagedIdentity(instanceID, instanceName)
			managedPath := filepath.Join(root, directoryName)
			if test.createPartial {
				if err := os.MkdirAll(managedPath, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(managedPath, ".env"), []byte("retained-secret\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			p, err := New(root, "docker")
			if err != nil {
				t.Fatal(err)
			}
			p.dockerRun = func(_ context.Context, args ...string) (string, error) {
				return "", fmt.Errorf("Docker must not run for an early provisioning tombstone: %v", args)
			}
			payload := domain.ActionPayload{
				InstanceID:  instanceID,
				Name:        instanceName,
				ProjectName: projectName,
				ManagedPath: managedPath,
			}
			if test.emptyMetadata {
				payload.ProjectName = ""
				payload.ManagedPath = ""
			}
			result := p.lifecycle(context.Background(), "instance.delete", payload)
			if !result.Success {
				t.Fatalf("lifecycle delete result=%+v", result)
			}
			if test.createPartial {
				if _, err := os.Stat(filepath.Join(managedPath, ".env")); err != nil {
					t.Fatalf("delete changed retained early-failure artifacts: %v", err)
				}
			}
		})
	}
}

func TestDockerDoesNotStartWithCanceledLeaseContext(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "docker-started")
	t.Setenv("FLEET_TEST_DOCKER_MARKER", marker)
	dockerPath := filepath.Join(root, "fake-docker")
	script := []byte("#!/bin/sh\nprintf started > \"$FLEET_TEST_DOCKER_MARKER\"\n")
	if err := os.WriteFile(dockerPath, script, 0o700); err != nil {
		t.Fatal(err)
	}
	provisioner, err := New(filepath.Join(root, "managed"), dockerPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provisioner.docker(ctx, "compose", "stop"); !errors.Is(err, context.Canceled) {
		t.Fatalf("docker() error=%v, want context.Canceled", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("docker command started with canceled lease context: %v", err)
	}
}

func TestStartDoesNotRecreateStoppedContainersWhenImageTagMoves(t *testing.T) {
	root := t.TempDir()
	managedPath := filepath.Join(root, "fleet-test-01-00000000")
	if err := os.MkdirAll(managedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedPath, "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provisioner, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	var commands [][]string
	provisioner.dockerRun = func(_ context.Context, args ...string) (string, error) {
		commands = append(commands, append([]string(nil), args...))
		if args[0] == "image" {
			return "sha256:" + strings.Repeat("b", 64) + "\n", nil
		}
		return "", fmt.Errorf("unexpected Docker command: %v", args)
	}

	result := provisioner.lifecycle(context.Background(), "instance.start", domain.ActionPayload{
		InstanceID: "00000000-0000-4000-8000-000000000001", Name: "fleet-test-01",
		Image: "local/hermes-fleet-runtime:0.18.2", ProjectName: "hermes-fleet-fleet-test-01-00000000", ManagedPath: managedPath,
		ImageID: "sha256:" + strings.Repeat("a", 64), Provider: "openai-codex", Model: "gpt-5.6-sol",
		Reasoning: "medium", ServiceTier: "normal", APIPort: 18650, DashboardPort: 19130,
	})
	if result.Success || !strings.Contains(result.Error, "image reference changed") {
		t.Fatalf("lifecycle() result=%+v, want moved image rejection", result)
	}
	if len(commands) != 1 || commands[0][0] != "image" {
		t.Fatalf("lifecycle() Docker commands=%v, want only immutable image inspection", commands)
	}
}

func TestStartRecreatesMissingContainersWithClearNames(t *testing.T) {
	root := t.TempDir()
	managedPath := filepath.Join(root, "fleet-test-01-00000000")
	if err := os.MkdirAll(managedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	for filename, contents := range map[string]string{"compose.yaml": "services: {}\n", ".env": "API_SERVER_KEY=test\n"} {
		if err := os.WriteFile(filepath.Join(managedPath, filename), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	provisioner, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	imageID := "sha256:" + strings.Repeat("a", 64)
	var commands [][]string
	provisioner.dockerRun = func(_ context.Context, args ...string) (string, error) {
		commands = append(commands, append([]string(nil), args...))
		switch {
		case args[0] == "image" && slicesContain(args, "{{json .Config.Labels}}"):
			return runtimeImageLabelsJSON(runtimeStateSchemaVersion, testRuntimeBuildID), nil
		case args[0] == "image":
			return imageID + "\n", nil
		case args[0] == "volume":
			return `{"io.hermes-fleet.managed":"true","io.hermes-fleet.instance-id":"00000000-0000-4000-8000-000000000001"}`, nil
		case slicesContain(args, runtimeStateProbe):
			return readyRuntimeStateJSON("openai-codex", "gpt-5.6-sol", "medium", "normal"), nil
		case args[0] == "compose":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected Docker command: %v", args)
		}
	}
	provisioner.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Request: request, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}

	result := provisioner.lifecycle(context.Background(), "instance.start", domain.ActionPayload{
		InstanceID: "00000000-0000-4000-8000-000000000001", Name: "fleet-test-01",
		Image: "local/hermes-fleet-runtime:0.18.2", ProjectName: "hermes-fleet-fleet-test-01-00000000", ManagedPath: managedPath,
		ImageID: imageID, Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
		APIPort: 18650, DashboardPort: 19130,
	})
	if !result.Success {
		t.Fatalf("lifecycle() result=%+v", result)
	}
	if len(commands) != 5 {
		t.Fatalf("lifecycle() Docker commands=%v, want image inspection, volume ownership, Compose up, runtime metadata, and readiness probe", commands)
	}
	composeCommand := strings.Join(commands[2], " ")
	if !strings.HasSuffix(composeCommand, " up -d --remove-orphans") {
		t.Fatalf("lifecycle() command=%q, want safe Compose recreation", composeCommand)
	}
	manifest, err := os.ReadFile(filepath.Join(managedPath, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`container_name: "hermes-fleet-instance-fleet-test-01-hermes"`,
		`container_name: "hermes-fleet-instance-fleet-test-01-dashboard"`,
	} {
		if !strings.Contains(string(manifest), expected) {
			t.Fatalf("updated manifest does not contain %q", expected)
		}
	}
}

func TestRuntimeRepairJobUsesManagedStartValidation(t *testing.T) {
	provisioner, err := New(t.TempDir(), "docker-must-not-run")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(domain.ActionPayload{InstanceID: "invalid", Name: "fleet-test-01"})
	if err != nil {
		t.Fatal(err)
	}
	result := provisioner.Execute(context.Background(), domain.Job{
		Type: "instance.runtime.repair", Payload: payload,
	})
	if result.Success || !strings.Contains(result.Error, "invalid managed instance identity") {
		t.Fatalf("Execute() result=%+v, want managed lifecycle validation", result)
	}
}

func TestRuntimeRepairUsesBoundedEscalationCommands(t *testing.T) {
	tests := []struct {
		name     string
		phase    int
		expected []string
	}{
		{
			name:  "restart services",
			phase: 1,
			expected: []string{
				"up -d --remove-orphans",
				"restart hermes dashboard",
			},
		},
		{
			name:  "force recreate services",
			phase: 2,
			expected: []string{
				"up -d --remove-orphans --force-recreate",
			},
		},
		{
			name:  "rebuild compose project without volumes",
			phase: 3,
			expected: []string{
				"down --remove-orphans",
				"up -d --remove-orphans --force-recreate",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			managedPath := filepath.Join(root, "fleet-test-01-00000000")
			if err := os.MkdirAll(filepath.Join(managedPath, "workspace"), 0o700); err != nil {
				t.Fatal(err)
			}
			for filename, contents := range map[string]string{"compose.yaml": "services: {}\n", ".env": "API_SERVER_KEY=test\n"} {
				if err := os.WriteFile(filepath.Join(managedPath, filename), []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			provisioner, err := New(root, "docker")
			if err != nil {
				t.Fatal(err)
			}
			imageID := "sha256:" + strings.Repeat("a", 64)
			var composeCommands []string
			provisioner.dockerRun = func(_ context.Context, args ...string) (string, error) {
				switch args[0] {
				case "image":
					return imageID + "\n", nil
				case "volume":
					return `{"io.hermes-fleet.managed":"true","io.hermes-fleet.instance-id":"00000000-0000-4000-8000-000000000001"}`, nil
				case "compose":
					composeCommands = append(composeCommands, strings.Join(args[7:], " "))
					return "", nil
				default:
					return "", fmt.Errorf("unexpected Docker command: %v", args)
				}
			}
			provisioner.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK, Request: request, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader("ok")),
				}, nil
			})}
			result := provisioner.repairRuntime(context.Background(), domain.RuntimeRepairPayload{
				ActionPayload: domain.ActionPayload{
					InstanceID: "00000000-0000-4000-8000-000000000001", Name: "fleet-test-01",
					Image: "local/hermes-fleet-runtime:0.19.0", ImageID: imageID,
					Provider: "openai-codex", ProjectName: "hermes-fleet-fleet-test-01-00000000",
					ManagedPath: managedPath, APIPort: 18650, DashboardPort: 19130, PreserveData: true,
				},
				Phase: test.phase, Attempt: 1, Trigger: "automatic",
			})
			if !result.Success {
				t.Fatalf("repairRuntime() result=%+v", result)
			}
			if len(composeCommands) != len(test.expected) {
				t.Fatalf("repairRuntime() Compose commands=%v want=%v", composeCommands, test.expected)
			}
			for index, expected := range test.expected {
				if composeCommands[index] != expected {
					t.Fatalf("repairRuntime() Compose command[%d]=%q want=%q", index, composeCommands[index], expected)
				}
			}
			for _, command := range composeCommands {
				if strings.Contains(command, " -v") || strings.Contains(command, "--volumes") {
					t.Fatalf("repairRuntime() attempted to remove data volume: %q", command)
				}
			}
		})
	}
}

func TestStartRefusesToRecreateADeletedDataVolume(t *testing.T) {
	root := t.TempDir()
	managedPath := filepath.Join(root, "fleet-test-01-00000000")
	if err := os.MkdirAll(managedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	for filename, contents := range map[string]string{"compose.yaml": "services: {}\n", ".env": "API_SERVER_KEY=test\n"} {
		if err := os.WriteFile(filepath.Join(managedPath, filename), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	provisioner, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	imageID := "sha256:" + strings.Repeat("a", 64)
	var commands [][]string
	provisioner.dockerRun = func(_ context.Context, args ...string) (string, error) {
		commands = append(commands, append([]string(nil), args...))
		if args[0] == "image" {
			return imageID + "\n", nil
		}
		if args[0] == "volume" {
			return "", errors.New("no such volume")
		}
		return "", fmt.Errorf("unexpected Docker command: %v", args)
	}
	result := provisioner.lifecycle(context.Background(), "instance.start", domain.ActionPayload{
		InstanceID: "00000000-0000-4000-8000-000000000001", Name: "fleet-test-01",
		Image: "local/hermes-fleet-runtime:0.18.2", ProjectName: "hermes-fleet-fleet-test-01-00000000", ManagedPath: managedPath,
		ImageID: imageID, Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
		APIPort: 18650, DashboardPort: 19130,
	})
	if result.Success || !strings.Contains(result.Error, "data volume is unavailable") {
		t.Fatalf("lifecycle() result=%+v, want missing data volume rejection", result)
	}
	if len(commands) != 2 {
		t.Fatalf("lifecycle() Docker commands=%v, want image and volume inspection only", commands)
	}
}

func TestLifecycleRejectsPeerManagedIdentity(t *testing.T) {
	root := t.TempDir()
	peerPath := filepath.Join(root, "peer-00000000")
	if err := os.MkdirAll(peerPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(peerPath, "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provisioner, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	called := false
	provisioner.dockerRun = func(_ context.Context, _ ...string) (string, error) {
		called = true
		return "", nil
	}
	result := provisioner.lifecycle(context.Background(), "instance.stop", domain.ActionPayload{
		InstanceID: "00000000-0000-4000-8000-000000000001", Name: "fleet-test-01",
		ProjectName: "hermes-fleet-peer-00000000", ManagedPath: peerPath,
	})
	if result.Success || called {
		t.Fatalf("lifecycle() accepted peer identity: result=%+v docker_called=%v", result, called)
	}
}

func TestObserveReadsOnlyExactFleetResources(t *testing.T) {
	root := t.TempDir()
	managedPath := filepath.Join(root, "fleet-test-01-00000000")
	if err := os.MkdirAll(filepath.Join(managedPath, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"compose.yaml", ".env"} {
		if err := os.WriteFile(filepath.Join(managedPath, name), []byte("fleet\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	var mutex sync.Mutex
	var commands [][]string
	p.dockerRun = func(_ context.Context, args ...string) (string, error) {
		mutex.Lock()
		commands = append(commands, append([]string(nil), args...))
		mutex.Unlock()
		switch args[0] {
		case "version":
			return "27.0.0\n", nil
		case "volume":
			return `[{"Name":"hermes-fleet-fleet-test-01-00000000-data"}]`, nil
		case "ps":
			return "aaaaaaaaaaaa\nbbbbbbbbbbbb\n", nil
		case "inspect":
			containers := []map[string]any{
				{"Id": "aaaaaaaaaaaa", "Image": "sha256:expected", "Config": map[string]any{"Labels": map[string]string{
					"io.hermes-fleet.managed": "true", "io.hermes-fleet.instance-id": "00000000-0000-4000-8000-000000000001",
					"io.hermes-fleet.hermes-version": "0.18.2", "io.hermes-fleet.hermes-ref": "7acaff5ef2bc",
					"com.docker.compose.project": "hermes-fleet-fleet-test-01-00000000", "com.docker.compose.service": "hermes",
				}}, "State": map[string]any{"Status": "running", "Health": map[string]string{"Status": "healthy"}}},
				{"Id": "bbbbbbbbbbbb", "Image": "sha256:expected", "Config": map[string]any{"Labels": map[string]string{
					"io.hermes-fleet.managed": "true", "io.hermes-fleet.instance-id": "00000000-0000-4000-8000-000000000001",
					"com.docker.compose.project": "hermes-fleet-fleet-test-01-00000000", "com.docker.compose.service": "dashboard",
				}}, "State": map[string]any{"Status": "running", "Health": map[string]string{"Status": "healthy"}}},
			}
			encoded, marshalErr := json.Marshal(containers)
			return string(encoded), marshalErr
		case "exec":
			return readyRuntimeStateJSON("openrouter", "gpt-5.6-sol", "medium", "normal"), nil
		default:
			return "", fmt.Errorf("unexpected Docker command: %v", args)
		}
	}
	p.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Request: request, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}

	target := domain.ObservationTarget{
		InstanceID: "00000000-0000-4000-8000-000000000001", Name: "fleet-test-01",
		Provider: "openrouter", Model: "gpt-5.6-sol",
		DesiredStatus: domain.InstanceRunning, Image: "runtime:latest", ImageID: "sha256:expected",
		ProjectName: "hermes-fleet-fleet-test-01-00000000", DataVolume: "hermes-fleet-fleet-test-01-00000000-data",
		ManagedPath: managedPath, APIPort: 18650, DashboardPort: 19130, Generation: "generation-1", RefreshRequestID: "refresh-1",
	}
	observation := p.Observe(context.Background(), target)
	if observation.Status != domain.ObservationInSync || observation.TargetGeneration != target.Generation || observation.RefreshRequestID != target.RefreshRequestID || observation.HermesVersion != "0.18.2" || observation.HermesSource != "7acaff5ef2bc" {
		t.Fatalf("Observe() observation=%+v", observation)
	}

	mutating := map[string]bool{"up": true, "stop": true, "down": true, "start": true, "restart": true, "rm": true, "create": true}
	mutex.Lock()
	defer mutex.Unlock()
	if len(commands) == 0 {
		t.Fatal("Observe() did not inspect Docker")
	}
	for _, command := range commands {
		for _, argument := range command {
			if mutating[argument] {
				t.Fatalf("Observe() issued mutating Docker command: %v", command)
			}
		}
	}
}

func TestObserveReportsRuntimeDriftWhenContainersAreMissing(t *testing.T) {
	root := t.TempDir()
	managedPath := filepath.Join(root, "fleet-test-01-00000000")
	if err := os.MkdirAll(filepath.Join(managedPath, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"compose.yaml", ".env"} {
		if err := os.WriteFile(filepath.Join(managedPath, name), []byte("fleet\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	provisioner, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	provisioner.dockerRun = func(_ context.Context, args ...string) (string, error) {
		switch args[0] {
		case "version":
			return "27.0.0\n", nil
		case "volume":
			return `[{"Name":"hermes-fleet-fleet-test-01-00000000-data"}]`, nil
		case "ps":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected Docker command: %v", args)
		}
	}
	observation := provisioner.Observe(context.Background(), domain.ObservationTarget{
		InstanceID: "00000000-0000-4000-8000-000000000001", Name: "fleet-test-01",
		DesiredStatus: domain.InstanceRunning, Provider: "openai-codex",
		ProjectName: "hermes-fleet-fleet-test-01-00000000", DataVolume: "hermes-fleet-fleet-test-01-00000000-data",
		ManagedPath: managedPath, Generation: "generation-1",
	})
	if observation.Status != domain.ObservationMissing {
		t.Fatalf("Observe() status=%q checks=%+v", observation.Status, observation.Checks)
	}
	foundRuntimeDrift := false
	for _, check := range observation.Checks {
		if check.Name == "runtime" && check.Status == domain.ObservationCheckDrift {
			foundRuntimeDrift = true
		}
	}
	if !foundRuntimeDrift {
		t.Fatalf("Observe() did not report actionable runtime drift: %+v", observation.Checks)
	}
}

func TestObserveTreatsCreatedContainersAsStopped(t *testing.T) {
	services := map[string]observedContainer{"hermes": {}, "dashboard": {}}
	hermes := services["hermes"]
	hermes.State.Status = "created"
	services["hermes"] = hermes
	dashboard := services["dashboard"]
	dashboard.State.Status = "created"
	services["dashboard"] = dashboard
	builder := &observationBuilder{}
	provisioner := &Provisioner{}
	if !provisioner.observeRuntimeState(domain.InstanceStopped, services, builder) {
		t.Fatalf("created containers were not treated as stopped: %+v", builder.checks)
	}
	if len(builder.checks) != 1 || builder.checks[0].Status != domain.ObservationCheckOK {
		t.Fatalf("stopped observation checks=%+v", builder.checks)
	}
}

func TestObserveTreatsEmptyHermesModelConfigurationAsDrift(t *testing.T) {
	provisioner := &Provisioner{dockerRun: func(_ context.Context, args ...string) (string, error) {
		return `{"model":{},"state":null}`, nil
	}}
	hermes := observedContainer{ID: "aaaaaaaaaaaa"}
	builder := &observationBuilder{}
	provisioner.observeRuntimeConfiguration(context.Background(), domain.ObservationTarget{
		Provider: "openai-codex", Model: "gpt-5.6-sol",
	}, hermes, builder)
	if len(builder.checks) != 1 || builder.checks[0].Name != "runtime_configuration" || builder.checks[0].Status != domain.ObservationCheckDrift {
		t.Fatalf("empty Hermes model configuration checks=%+v", builder.checks)
	}
}

func TestObserveReportsMissingWithoutCreatingResources(t *testing.T) {
	root := t.TempDir()
	p, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	p.dockerRun = func(_ context.Context, args ...string) (string, error) {
		switch args[0] {
		case "version":
			return "27.0.0\n", nil
		case "volume":
			return "", errors.New("no such volume")
		case "ps":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected Docker command: %v", args)
		}
	}
	target := domain.ObservationTarget{
		InstanceID: "00000000-0000-4000-8000-000000000001", Name: "fleet-test-01", DesiredStatus: domain.InstanceRunning,
		Provider: "openrouter", Model: "gpt-5.6-sol",
		ProjectName: "hermes-fleet-fleet-test-01-00000000", DataVolume: "hermes-fleet-fleet-test-01-00000000-data",
		ManagedPath: filepath.Join(root, "fleet-test-01-00000000"), Generation: "generation-1",
	}
	observation := p.Observe(context.Background(), target)
	if observation.Status != domain.ObservationMissing {
		t.Fatalf("Observe() status=%q checks=%+v", observation.Status, observation.Checks)
	}
	if _, err := os.Stat(target.ManagedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Observe() created or changed a missing managed path: %v", err)
	}
}

func TestObserveReportsComposeLabelMismatchAsDegraded(t *testing.T) {
	root := t.TempDir()
	managedPath := filepath.Join(root, "fleet-test-01-00000000")
	if err := os.MkdirAll(filepath.Join(managedPath, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"compose.yaml", ".env"} {
		if err := os.WriteFile(filepath.Join(managedPath, name), []byte("fleet\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	p.dockerRun = func(_ context.Context, args ...string) (string, error) {
		switch args[0] {
		case "version":
			return "27.0.0\n", nil
		case "volume":
			return "[]", nil
		case "ps":
			return "aaaaaaaaaaaa\nbbbbbbbbbbbb\n", nil
		case "inspect":
			containers := []map[string]any{
				{"Id": "aaaaaaaaaaaa", "Image": "sha256:expected", "Config": map[string]any{"Labels": map[string]string{
					"io.hermes-fleet.managed": "true", "io.hermes-fleet.instance-id": "00000000-0000-4000-8000-000000000001",
					"com.docker.compose.project": "hermes-fleet-fleet-test-01-00000000", "com.docker.compose.service": "hermes",
				}}, "State": map[string]any{"Status": "running", "Health": map[string]string{"Status": "healthy"}}},
				{"Id": "bbbbbbbbbbbb", "Image": "sha256:expected", "Config": map[string]any{"Labels": map[string]string{
					"io.hermes-fleet.managed": "true", "io.hermes-fleet.instance-id": "00000000-0000-4000-8000-000000000001",
					"com.docker.compose.project": "external-project", "com.docker.compose.service": "dashboard",
				}}, "State": map[string]any{"Status": "running"}},
			}
			encoded, marshalErr := json.Marshal(containers)
			return string(encoded), marshalErr
		default:
			return "", fmt.Errorf("unexpected Docker command: %v", args)
		}
	}
	target := domain.ObservationTarget{
		InstanceID: "00000000-0000-4000-8000-000000000001", Name: "fleet-test-01", DesiredStatus: domain.InstanceRunning,
		Provider: "openrouter", Model: "gpt-5.6-sol",
		ImageID: "sha256:expected", ProjectName: "hermes-fleet-fleet-test-01-00000000",
		DataVolume: "hermes-fleet-fleet-test-01-00000000-data", ManagedPath: managedPath, APIPort: 18650, Generation: "generation-1",
	}
	observation := p.Observe(context.Background(), target)
	if observation.Status != domain.ObservationDegraded {
		t.Fatalf("Observe() status=%q checks=%+v", observation.Status, observation.Checks)
	}
	foundOwnershipDrift := false
	for _, check := range observation.Checks {
		if check.Name == "ownership" && check.Status == domain.ObservationCheckDrift {
			foundOwnershipDrift = true
		}
	}
	if !foundOwnershipDrift {
		t.Fatalf("Observe() did not identify ownership label drift: %+v", observation.Checks)
	}
}

func TestSendChatMessageUsesHermesAuthenticationAndStableSession(t *testing.T) {
	root := t.TempDir()
	managedPath := filepath.Join(root, "fleet-chat-00000000")
	if err := os.MkdirAll(managedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedPath, "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedPath, ".env"), []byte("API_SERVER_KEY='quoted-secret'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	requestCount := 0
	p.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestCount++
		if authorization := request.Header.Get("Authorization"); authorization != "Bearer quoted-secret" {
			t.Fatalf("Authorization=%q", authorization)
		}
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		switch requestCount {
		case 1:
			if request.Method != http.MethodPost || request.URL.String() != "http://127.0.0.1:18650/api/sessions" {
				t.Fatalf("unexpected session request %s %s", request.Method, request.URL)
			}
			var payload struct {
				ID               string            `json:"id"`
				Source           string            `json:"source"`
				Provider         string            `json:"provider"`
				Model            string            `json:"model"`
				RequireModelLock bool              `json:"require_model_lock"`
				ModelOptions     map[string]string `json:"model_options"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.ID != "fleet-session-01" || payload.Source != "api_server" ||
				payload.Provider != "openai-codex" || payload.Model != "gpt-5.6-sol" || !payload.RequireModelLock ||
				payload.ModelOptions["reasoning_effort"] != "high" || payload.ModelOptions["service_tier"] != "priority" {
				t.Fatalf("session payload=%+v", payload)
			}
			return &http.Response{
				StatusCode: http.StatusCreated,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"object":"hermes.session"}`)),
				Request:    request,
			}, nil
		case 2:
			if request.Method != http.MethodPost || request.URL.String() != "http://127.0.0.1:18650/api/sessions/fleet-session-01/model" {
				t.Fatalf("unexpected model-lock request %s %s", request.Method, request.URL)
			}
			var payload struct {
				Provider     string            `json:"provider"`
				Model        string            `json:"model"`
				ModelOptions map[string]string `json:"model_options"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Provider != "openai-codex" || payload.Model != "gpt-5.6-sol" ||
				payload.ModelOptions["reasoning_effort"] != "high" || payload.ModelOptions["service_tier"] != "priority" {
				t.Fatalf("model-lock payload=%+v", payload)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					`{"object":"hermes.session.model_lock","session_id":"fleet-session-01","runtime":{"provider":"openai-codex","model":"gpt-5.6-sol","model_lock":"accepted"}}`,
				)),
				Request: request,
			}, nil
		case 3:
			if request.Method != http.MethodPost || request.URL.String() != "http://127.0.0.1:18650/api/sessions/fleet-session-01/chat" {
				t.Fatalf("unexpected chat request %s %s", request.Method, request.URL)
			}
			var payload struct {
				Input        string            `json:"input"`
				Instructions string            `json:"instructions"`
				ModelOptions map[string]string `json:"model_options"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Input != "hello Hermes" || !strings.Contains(payload.Instructions, "FILE:/absolute/path") ||
				payload.ModelOptions["reasoning_effort"] != "high" || payload.ModelOptions["service_tier"] != "priority" {
				t.Fatalf("chat payload=%+v", payload)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					`{"object":"hermes.session.chat.completion","session_id":"fleet-session-01","message":{"role":"assistant","content":"  hello Fleet  "}}`,
				)),
				Request: request,
			}, nil
		default:
			t.Fatalf("unexpected request %d: %s %s", requestCount, request.Method, request.URL)
			return nil, errors.New("unexpected request")
		}
	})}

	result := p.sendChatMessage(context.Background(), domain.ChatSendPayload{
		InstanceID: "instance-01", SessionID: "session-01", MessageID: "message-01",
		ManagedPath: managedPath, APIPort: 18650,
		Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "high", ServiceTier: "priority",
	}, []byte("hello Hermes"))
	if !result.Success || result.ChatMessage != "hello Fleet" {
		t.Fatalf("chat result=%+v", result)
	}
	if requestCount != 3 {
		t.Fatalf("request count=%d", requestCount)
	}
}

func TestEnsureHermesChatSessionRejectsMismatchedModelLockAcknowledgement(t *testing.T) {
	requestCount := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestCount++
		if requestCount == 1 {
			return &http.Response{
				StatusCode: http.StatusConflict,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"session_exists","message":"already exists"}}`)),
				Request:    request,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"object":"hermes.session.model_lock","session_id":"fleet-session-01","runtime":{"provider":"openai-codex","model":"hermes-agent","model_lock":"accepted"}}`,
			)),
			Request: request,
		}, nil
	})}
	payload := domain.ChatSendPayload{
		Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
	}
	err := ensureHermesChatSession(context.Background(), client, 18650, "secret", "fleet-session-01", payload)
	if err == nil || !strings.Contains(err.Error(), "did not acknowledge") {
		t.Fatalf("ensureHermesChatSession() error=%v", err)
	}
	if requestCount != 2 {
		t.Fatalf("request count=%d", requestCount)
	}
}

func TestExecuteChatStreamReportsOrderedDeltas(t *testing.T) {
	root := t.TempDir()
	managedPath := filepath.Join(root, "fleet-chat-stream-00000000")
	if err := os.MkdirAll(managedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedPath, "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedPath, ".env"), []byte("API_SERVER_KEY=stream-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	requestCount := 0
	p.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestCount++
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if request.Header.Get("Authorization") != "Bearer stream-secret" {
			t.Fatalf("Authorization=%q", request.Header.Get("Authorization"))
		}
		if requestCount == 1 {
			if request.URL.String() != "http://127.0.0.1:18650/api/sessions" {
				t.Fatalf("unexpected session request URL=%s", request.URL)
			}
			return &http.Response{
				StatusCode: http.StatusConflict,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"session_exists","message":"already exists"}}`)),
				Request:    request,
			}, nil
		}
		if requestCount == 2 {
			if request.Method != http.MethodPost || request.URL.String() != "http://127.0.0.1:18650/api/sessions/fleet-session-01/model" {
				t.Fatalf("unexpected model-lock request %d URL=%s", requestCount, request.URL)
			}
			var payload struct {
				Provider     string            `json:"provider"`
				Model        string            `json:"model"`
				ModelOptions map[string]string `json:"model_options"`
			}
			if json.Unmarshal(body, &payload) != nil || payload.Provider != "openai-codex" || payload.Model != "gpt-5.6-sol" ||
				payload.ModelOptions["reasoning_effort"] != "medium" || payload.ModelOptions["service_tier"] != "normal" {
				t.Fatalf("model-lock request body=%s", body)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					`{"object":"hermes.session.model_lock","session_id":"fleet-session-01","runtime":{"provider":"openai-codex","model":"gpt-5.6-sol","model_lock":"accepted"}}`,
				)),
				Request: request,
			}, nil
		}
		if requestCount != 3 || request.URL.String() != "http://127.0.0.1:18650/api/sessions/fleet-session-01/chat/stream" {
			t.Fatalf("unexpected stream request %d URL=%s", requestCount, request.URL)
		}
		if deadline, ok := request.Context().Deadline(); ok {
			t.Fatalf("stream request has an overall deadline %s", deadline)
		}
		var payload struct {
			Input        string            `json:"input"`
			Instructions string            `json:"instructions"`
			ModelOptions map[string]string `json:"model_options"`
		}
		if json.Unmarshal(body, &payload) != nil || payload.Input != "hello Hermes" ||
			!strings.Contains(payload.Instructions, "FILE:/absolute/path") ||
			payload.ModelOptions["reasoning_effort"] != "medium" || payload.ModelOptions["service_tier"] != "normal" ||
			request.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("stream request body=%s headers=%v", body, request.Header)
		}
		stream := "event: run.started\n" +
			"data: {\"session_id\":\"fleet-session-01\",\"run_id\":\"run-1\",\"seq\":1}\n\n" +
			"event: tool.started\n" +
			"data: {\"message_id\":\"msg-1\",\"tool_name\":\"terminal\",\"preview\":\"python + 3 commands\",\"args\":{\"command\":\"first\\nsecond\\nthird\\nfourth\"},\"session_id\":\"fleet-session-01\",\"run_id\":\"run-1\",\"seq\":2}\n\n" +
			"event: assistant.delta\n" +
			"data: {\"message_id\":\"msg-1\",\"delta\":\"hello\",\"session_id\":\"fleet-session-01\",\"run_id\":\"run-1\",\"seq\":3}\n\n" +
			"event: assistant.delta\n" +
			"data: {\"message_id\":\"msg-1\",\"delta\":\" Fleet\",\"session_id\":\"fleet-session-01\",\"run_id\":\"run-1\",\"seq\":4}\n\n" +
			"event: assistant.completed\n" +
			"data: {\"message_id\":\"msg-1\",\"content\":\"hello Fleet\",\"completed\":true,\"session_id\":\"fleet-session-01\",\"run_id\":\"run-1\",\"seq\":5}\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(stream)),
			Request:    request,
		}, nil
	})}
	payload, err := json.Marshal(domain.ChatSendPayload{
		InstanceID: "instance-01", SessionID: "session-01", MessageID: "message-01",
		ManagedPath: managedPath, APIPort: 18650,
		Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
	})
	if err != nil {
		t.Fatal(err)
	}
	var events []domain.ChatStreamEvent
	result := p.ExecuteChatStream(context.Background(), domain.Job{Type: "instance.chat.send", Attempts: 2, Payload: payload,
		InputSecret: []byte("hello Hermes")}, func(_ context.Context, event domain.ChatStreamEvent) error {
		events = append(events, event)
		return nil
	})
	if !result.Success || result.ChatMessage != "hello Fleet" {
		t.Fatalf("result=%+v", result)
	}
	sequenceBase := int64(2) << 32
	if len(events) != 4 || events[0].Sequence != sequenceBase+1 || events[0].Type != domain.ChatEventStarted ||
		events[1].Sequence != sequenceBase+2 || events[1].Type != domain.ChatEventActivity ||
		events[2].Sequence != sequenceBase+3 || events[2].Type != domain.ChatEventActivity ||
		events[3].Sequence != sequenceBase+4 || events[3].Type != domain.ChatEventDelta || events[3].Content != "hello Fleet" {
		t.Fatalf("events=%+v", events)
	}
	var toolPayload domain.ChatEventPayload
	if err := json.Unmarshal([]byte(events[2].Content), &toolPayload); err != nil {
		t.Fatal(err)
	}
	if toolPayload.Event != "tool.started" || !strings.Contains(toolPayload.Data, `"args":{"command":"first\nsecond\nthird\nfourth"}`) {
		t.Fatalf("tool payload=%+v", toolPayload)
	}
}

func TestConsumeHermesChatStreamAcceptsMetadataUnknownAndFragmentedEvents(t *testing.T) {
	stream := ": keepalive\r\n\r\n" +
		"event: message\r\ndata: {\"choices\":[]}\r\n\r\n" +
		"data: {\"future_metadata\":{\"version\":2}}\r\n\r\n" +
		"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\r\n\r\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":[{\"type\":\"text\",\"text\":\"hello\"},{\"type\":\"text\",\"text\":\" Fleet\"}]}}]}\r\n\r\n" +
		"data: [DONE]\r\n\r\n"
	reader := &oneByteReader{data: []byte(stream)}
	var events []domain.ChatStreamEvent
	content, sequence, err := consumeHermesChatStream(context.Background(), reader, 9,
		func(_ context.Context, event domain.ChatStreamEvent) error {
			events = append(events, event)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if content != "hello Fleet" || sequence != 13 {
		t.Fatalf("content=%q sequence=%d", content, sequence)
	}
	if len(events) != 4 ||
		events[0].Type != domain.ChatEventActivity ||
		events[1].Type != domain.ChatEventActivity ||
		events[2].Type != domain.ChatEventActivity ||
		events[3].Type != domain.ChatEventDelta ||
		events[3].Content != "hello Fleet" {
		t.Fatalf("events=%+v", events)
	}
}

func TestConsumeHermesChatStreamAcceptsTypedDeltas(t *testing.T) {
	stream := "event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"typed response\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\"}\n\n"
	var events []domain.ChatStreamEvent
	content, _, err := consumeHermesChatStream(context.Background(), strings.NewReader(stream), 0,
		func(_ context.Context, event domain.ChatStreamEvent) error {
			events = append(events, event)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if content != "typed response" || len(events) != 1 || events[0].Content != content {
		t.Fatalf("content=%q events=%+v", content, events)
	}
}

func TestConsumeHermesChatStreamRetainsNonTextEventsWithoutFailingResponse(t *testing.T) {
	rawActivity := `{"type":"tool.started","tool":"browser"}`
	stream := "event: tool.started\n" +
		"data: " + rawActivity + "\n\n" +
		"event: future-frame\n" +
		"data: {not-json}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"finished\"}}]}\n\n" +
		"data: [DONE]\n\n"
	var events []domain.ChatStreamEvent
	content, _, err := consumeHermesChatStream(context.Background(), strings.NewReader(stream), 0,
		func(_ context.Context, event domain.ChatStreamEvent) error {
			events = append(events, event)
			return nil
		})
	if err != nil || content != "finished" {
		t.Fatalf("content=%q error=%v", content, err)
	}
	if len(events) != 3 || events[0].Type != domain.ChatEventActivity ||
		events[1].Type != domain.ChatEventActivity ||
		events[2].Type != domain.ChatEventDelta || events[2].Content != "finished" {
		t.Fatalf("events=%+v", events)
	}
	var payload domain.ChatEventPayload
	if err := json.Unmarshal([]byte(events[0].Content), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Kind != "activity" || payload.Event != "tool.started" || payload.Data != rawActivity || payload.Tool != "" || payload.Label != "" {
		t.Fatalf("activity payload=%+v", payload)
	}
}

func TestHermesActivityPayloadIsCanonicalAndRedactsRawFrame(t *testing.T) {
	raw := []byte(`{"type":"artifact.created","token":"must-not-leak","tool":"report_writer","artifact":{"id":"artifact-1","name":"/tmp/report.txt","media_type":"text/plain; charset=utf-8","size_bytes":2048,"url":"javascript:alert(1)"}}`)
	encoded := hermesActivityPayload("artifact.created", raw)
	if strings.Contains(encoded, "must-not-leak") || strings.Contains(encoded, "javascript") || strings.Contains(encoded, "/tmp/") {
		t.Fatalf("raw Hermes data leaked into activity payload: %s", encoded)
	}
	var payload domain.ChatEventPayload
	if err := json.Unmarshal([]byte(encoded), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Kind != "artifact" || payload.Artifact == nil || payload.Artifact.Name != "report.txt" ||
		payload.Artifact.ID != "artifact-1" || payload.Artifact.Kind != "file" || payload.Artifact.MediaType != "text/plain" ||
		payload.Artifact.SizeBytes != 2048 || payload.Artifact.SourceTool != "report_writer" || payload.Artifact.URL != "" {
		t.Fatalf("artifact payload=%+v", payload)
	}
}

func TestHermesActivityPayloadNormalizesTypedArtifactAliases(t *testing.T) {
	encoded := hermesActivityPayload("image_output.created", []byte(`{
		"type":"image_output.created",
		"attachment":{"filename":"chart.PNG","content_type":"image/png","download_url":"https://example.test/chart.png","size":"4096"}
	}`))
	var payload domain.ChatEventPayload
	if err := json.Unmarshal([]byte(encoded), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Artifact == nil || payload.Artifact.Name != "chart.PNG" || payload.Artifact.Kind != "image" ||
		payload.Artifact.MediaType != "image/png" || payload.Artifact.SizeBytes != 4096 ||
		payload.Artifact.URL != "https://example.test/chart.png" {
		t.Fatalf("artifact payload=%+v", payload)
	}
}

func TestDecodeHermesChatEventClassifiesNestedOutputFileAsArtifact(t *testing.T) {
	frame, err := decodeHermesChatEvent("response.item.added", []byte(`{
		"type":"response.item.added",
		"item":{"type":"output_file","id":"file-1","file":{"filename":"report.csv","mime_type":"text/csv"}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if frame.EventType != domain.ChatEventArtifact {
		t.Fatalf("frame=%+v", frame)
	}
	var payload domain.ChatEventPayload
	if err := json.Unmarshal([]byte(frame.Activity), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Artifact == nil || payload.Artifact.Name != "report.csv" || payload.Artifact.MediaType != "text/csv" {
		t.Fatalf("payload=%+v", payload)
	}
}

func TestHermesActivityPayloadPreservesToolLifecycleFramesExactly(t *testing.T) {
	startedData := `{
		"tool":"browser_snapshot","toolCallId":"call-browser-42","status":"running",
		"label":"Browser Snapshot(\"https://example.test/dashboard\")"
	}`
	completedData := `{
		"tool":"browser_snapshot","toolCallId":"call-browser-42","status":"completed",
		"duration_ms":20700
	}`
	for name, raw := range map[string]string{"started": startedData, "completed": completedData} {
		encoded := hermesActivityPayload("hermes.tool.progress", []byte(raw))
		var payload domain.ChatEventPayload
		if err := json.Unmarshal([]byte(encoded), &payload); err != nil {
			t.Fatalf("%s payload: %v", name, err)
		}
		if payload.Kind != "activity" || payload.Event != "hermes.tool.progress" || payload.Data != raw {
			t.Fatalf("%s payload=%+v", name, payload)
		}
		if payload.Tool != "" || payload.CallID != "" || payload.Status != "" || payload.Label != "" || payload.DurationMS != 0 {
			t.Fatalf("Fleet reconstructed Hermes fields: %+v", payload)
		}
	}
}

func TestHermesActivityPayloadPreservesRunPreviewAndDurationInRawData(t *testing.T) {
	raw := `{
		"type":"tool.completed",
		"tool":"skill_view",
		"preview":"Skill View(\"google-workspace\")",
		"call_id":"call-skill-1",
		"duration":0.1
	}`
	encoded := hermesActivityPayload("tool.completed", []byte(raw))
	var payload domain.ChatEventPayload
	if err := json.Unmarshal([]byte(encoded), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Event != "tool.completed" || payload.Data != raw || payload.Label != "" || payload.CallID != "" || payload.Status != "" || payload.DurationMS != 0 {
		t.Fatalf("payload=%+v", payload)
	}
}

func TestHermesActivityPayloadDoesNotPromoteArgumentFragmentsToActivities(t *testing.T) {
	raw := `{"type":"tool.delta","tool":"browser_type","arguments":{"url":"file:///root/dashboard_redirect.html","value":"upt posind3m4s e1"}}`
	encoded := hermesActivityPayload("tool.delta", []byte(raw))
	var payload domain.ChatEventPayload
	if err := json.Unmarshal([]byte(encoded), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Data != raw || payload.Event != "tool.delta" || payload.Label != "" || payload.Tool != "" {
		t.Fatalf("Fleet promoted Hermes arguments into activity fields: %+v", payload)
	}
}

func TestDiscoverChatArtifactCandidatesRemovesOnlyStandaloneSafeOutputs(t *testing.T) {
	content := "Report ready.\n\nMEDIA:/data/cache/screenshots/dashboard.png\n/root/report.xlsx\nFILE:/root/report.txt\nLokasi file: /root/legacy.txt\n\n```text\n/root/inside-code.pdf\n```\nUnsafe: /root/not-standalone.pdf"
	cleaned, candidates := discoverChatArtifactCandidates(content)
	if len(candidates) != 4 || candidates[0].SourcePath != "/data/cache/screenshots/dashboard.png" ||
		candidates[0].Kind != "image" || candidates[1].SourcePath != "/root/report.xlsx" || candidates[1].Kind != "file" ||
		candidates[2].SourcePath != "/root/report.txt" || candidates[2].MediaType != "text/plain" ||
		candidates[3].SourcePath != "/root/legacy.txt" || candidates[3].MediaType != "text/plain" {
		t.Fatalf("candidates=%+v", candidates)
	}
	if strings.Contains(cleaned, "MEDIA:") || strings.Contains(cleaned, "/root/report.xlsx") ||
		strings.Contains(cleaned, "FILE:") || strings.Contains(cleaned, "Lokasi file:") ||
		!strings.Contains(cleaned, "/root/inside-code.pdf") || !strings.Contains(cleaned, "Unsafe: /root/not-standalone.pdf") {
		t.Fatalf("cleaned=%q", cleaned)
	}
}

func TestValidateChatArtifactContentAcceptsUTF8TextAndRejectsBinaryText(t *testing.T) {
	directory := t.TempDir()
	validPath := filepath.Join(directory, "report.txt")
	if err := os.WriteFile(validPath, []byte("nomor kiriman\nJP-001\nJP-002\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateChatArtifactContent(validPath, chatArtifactCandidate{Name: "report.txt", MediaType: "text/plain"}); err != nil {
		t.Fatalf("UTF-8 text was rejected: %v", err)
	}
	invalidPath := filepath.Join(directory, "binary.txt")
	if err := os.WriteFile(invalidPath, []byte{'o', 'k', 0, 0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateChatArtifactContent(invalidPath, chatArtifactCandidate{Name: "binary.txt", MediaType: "text/plain"}); err == nil {
		t.Fatal("binary content with a .txt extension was accepted")
	}
}

func TestValidateChatArtifactContentRejectsExtensionSpoofing(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(filename, []byte("not a pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateChatArtifactContent(filename, chatArtifactCandidate{Name: "report.pdf", MediaType: "application/pdf"}); err == nil {
		t.Fatal("PDF extension spoofing was accepted")
	}
}

func TestConsumeHermesChatStreamReadsFinalTextFromCompletedResponse(t *testing.T) {
	stream := "event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"final answer\"}]}]}}\n\n"
	content, _, err := consumeHermesChatStream(context.Background(), strings.NewReader(stream), 0,
		func(context.Context, domain.ChatStreamEvent) error { return nil })
	if err != nil || content != "final answer" {
		t.Fatalf("content=%q error=%v", content, err)
	}
}

func TestHermesChatProtocolConformance(t *testing.T) {
	tests := []struct {
		name          string
		wantContent   string
		wantError     string
		wantActivity  int
		wantArtifacts int
	}{
		{name: "openai-text", wantContent: "Hello Fleet", wantActivity: 2},
		{name: "tool-then-text", wantContent: "Tool completed", wantActivity: 2},
		{name: "responses-reasoning", wantContent: "Visible answer", wantActivity: 1},
		{name: "anthropic-content-block", wantContent: "Block answer", wantActivity: 1},
		{name: "artifact-then-text", wantContent: "Report created", wantArtifacts: 1},
		{name: "future-event", wantContent: "Still compatible", wantActivity: 1},
		{name: "tool-progress-artifact-final", wantContent: "Report ready", wantActivity: 3, wantArtifacts: 1},
		{name: "session-tool-arguments", wantContent: "Session answer", wantActivity: 4},
		{name: "malformed-future-then-final", wantContent: "Recovered", wantActivity: 2},
		{name: "upstream-error", wantError: "Hermes returned an error while streaming the response"},
		{name: "finish-reason-error", wantError: "Hermes returned an error while streaming the response"},
		{name: "finish-reason-length", wantError: "Hermes returned an incomplete response because the output limit was reached"},
		{name: "activity-only-eof", wantError: "Hermes chat stream ended without a message", wantActivity: 1},
	}
	fixtureNames := map[string]bool{}
	for _, test := range tests {
		fixtureNames[test.name+".sse"] = true
	}
	fixtures, err := filepath.Glob(filepath.Join("testdata", "hermes-chat-contract", "*.sse"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != len(tests) {
		t.Fatalf("fixture count=%d test count=%d; every contract fixture must be part of the gate", len(fixtures), len(tests))
	}
	for _, fixture := range fixtures {
		if !fixtureNames[filepath.Base(fixture)] {
			t.Fatalf("contract fixture %s is not covered by the conformance gate", filepath.Base(fixture))
		}
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stream, err := os.ReadFile(filepath.Join("testdata", "hermes-chat-contract", test.name+".sse"))
			if err != nil {
				t.Fatal(err)
			}
			var events []domain.ChatStreamEvent
			const initialSequence int64 = 100
			content, sequence, err := consumeHermesChatStream(context.Background(), bytes.NewReader(stream), initialSequence,
				func(_ context.Context, event domain.ChatStreamEvent) error {
					events = append(events, event)
					return nil
				})
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("content=%q error=%v, want error containing %q", content, err, test.wantError)
				}
			} else if err != nil || content != test.wantContent {
				t.Fatalf("content=%q error=%v events=%+v", content, err, events)
			}
			if sequence != initialSequence+int64(len(events)) {
				t.Fatalf("sequence=%d event count=%d", sequence, len(events))
			}
			activityCount, artifactCount, deltaCount := 0, 0, 0
			for index, event := range events {
				wantSequence := initialSequence + int64(index) + 1
				if event.Sequence != wantSequence {
					t.Fatalf("event[%d] sequence=%d, want %d", index, event.Sequence, wantSequence)
				}
				switch event.Type {
				case domain.ChatEventDelta:
					deltaCount++
				case domain.ChatEventActivity, domain.ChatEventArtifact:
					var payload domain.ChatEventPayload
					if err := json.Unmarshal([]byte(event.Content), &payload); err != nil {
						t.Fatalf("event[%d] is not a canonical Fleet payload: %v", index, err)
					}
					if payload.Event == "" || payload.Kind == "" {
						t.Fatalf("event[%d] incomplete payload=%+v", index, payload)
					}
					if event.Type == domain.ChatEventActivity {
						if payload.Data == "" {
							t.Fatalf("event[%d] did not preserve the Hermes data field: %+v", index, payload)
						}
						activityCount++
					} else {
						if payload.Label == "" || strings.Contains(event.Content, "conformance-secret") || strings.Contains(event.Content, "/tmp/") {
							t.Fatalf("event[%d] unsafe artifact capability=%+v", index, payload)
						}
						artifactCount++
					}
				default:
					t.Fatalf("event[%d] has unsupported normalized type %q", index, event.Type)
				}
			}
			if activityCount != test.wantActivity || artifactCount != test.wantArtifacts {
				t.Fatalf("activity=%d artifacts=%d events=%+v", activityCount, artifactCount, events)
			}
			if test.wantError == "" && deltaCount == 0 {
				t.Fatalf("successful fixture did not produce a visible delta: %+v", events)
			}
		})
	}
}

func TestExecuteChatStreamFailsAfterConnectionStopsReceivingBytes(t *testing.T) {
	root := t.TempDir()
	managedPath := filepath.Join(root, "fleet-chat-idle-00000000")
	if err := os.MkdirAll(managedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedPath, "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedPath, ".env"), []byte("API_SERVER_KEY=idle-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	p.chatConnectionIdleTimeout = 20 * time.Millisecond
	p.chatSemanticIdleTimeout = time.Second
	requestCount := 0
	p.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestCount++
		if requestCount == 1 {
			return &http.Response{
				StatusCode: http.StatusCreated,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"object":"hermes.session"}`)),
				Request:    request,
			}, nil
		}
		if requestCount == 2 {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					`{"object":"hermes.session.model_lock","session_id":"fleet-session-01","runtime":{"provider":"openai-codex","model":"gpt-5.6-sol","model_lock":"accepted"}}`,
				)),
				Request: request,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(&contextBlockingReader{ctx: request.Context()}),
			Request:    request,
		}, nil
	})}
	payload, err := json.Marshal(domain.ChatSendPayload{
		InstanceID: "instance-01", SessionID: "session-01", MessageID: "message-01",
		ManagedPath: managedPath, APIPort: 18650,
		Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := p.ExecuteChatStream(context.Background(), domain.Job{
		Type: "instance.chat.send", Payload: payload, InputSecret: []byte("hello"),
	}, func(context.Context, domain.ChatStreamEvent) error { return nil })
	if result.Success || !strings.Contains(result.Error, "connection stalled") || !strings.Contains(result.Error, "20ms") {
		t.Fatalf("result=%+v", result)
	}
}

func TestExecuteChatStreamHeartbeatDoesNotMaskSemanticStall(t *testing.T) {
	root := t.TempDir()
	managedPath := filepath.Join(root, "fleet-chat-semantic-idle-00000000")
	if err := os.MkdirAll(managedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedPath, "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedPath, ".env"), []byte("API_SERVER_KEY=semantic-idle-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	p.chatConnectionIdleTimeout = 100 * time.Millisecond
	p.chatSemanticIdleTimeout = 25 * time.Millisecond
	requestCount := 0
	p.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestCount++
		if requestCount == 1 {
			return &http.Response{
				StatusCode: http.StatusCreated,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"object":"hermes.session"}`)),
				Request:    request,
			}, nil
		}
		if requestCount == 2 {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					`{"object":"hermes.session.model_lock","session_id":"fleet-session-01","runtime":{"provider":"openai-codex","model":"gpt-5.6-sol","model_lock":"accepted"}}`,
				)),
				Request: request,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(&periodicHeartbeatReader{
				ctx: request.Context(), interval: 5 * time.Millisecond,
			}),
			Request: request,
		}, nil
	})}
	payload, err := json.Marshal(domain.ChatSendPayload{
		InstanceID: "instance-01", SessionID: "session-01", MessageID: "message-01",
		ManagedPath: managedPath, APIPort: 18650,
		Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := p.ExecuteChatStream(context.Background(), domain.Job{
		Type: "instance.chat.send", Payload: payload, InputSecret: []byte("hello"),
	}, func(context.Context, domain.ChatStreamEvent) error { return nil })
	if result.Success || !strings.Contains(result.Error, "tool stalled") || !strings.Contains(result.Error, "25ms") {
		t.Fatalf("result=%+v", result)
	}
}

func TestChatSemanticProgressTrackerOnlyAcceptsToolAndOutputProgress(t *testing.T) {
	activity := func(eventName, data string) domain.ChatStreamEvent {
		content, err := json.Marshal(domain.ChatEventPayload{Kind: "activity", Event: eventName, Data: data})
		if err != nil {
			t.Fatal(err)
		}
		return domain.ChatStreamEvent{Type: domain.ChatEventActivity, Content: string(content)}
	}
	tracker := chatSemanticProgressTracker{}
	tests := []struct {
		name  string
		event domain.ChatStreamEvent
		want  bool
	}{
		{name: "connection lifecycle", event: activity("run.started", `{"type":"run.started"}`)},
		{name: "reasoning", event: activity("reasoning.delta", `{"type":"reasoning.delta","text":"still thinking"}`), want: true},
		{name: "duplicate reasoning", event: activity("reasoning.delta", `{"type":"reasoning.delta","text":"still thinking"}`)},
		{name: "tool identity without tool event name", event: activity("mcp_call", `{"type":"mcp_call","tool_name":"browser_exec","call_id":"call-mcp-1"}`), want: true},
		{name: "tool started", event: activity("tool.started", `{"type":"tool.started","tool":"browser_exec","call_id":"call-1"}`), want: true},
		{name: "duplicate tool event", event: activity("tool.started", `{"type":"tool.started","tool":"browser_exec","call_id":"call-1"}`)},
		{name: "tool progress", event: activity("tool.progress", `{"type":"tool.progress","tool":"browser_exec","call_id":"call-1","step":2}`), want: true},
		{name: "assistant output", event: domain.ChatStreamEvent{Type: domain.ChatEventDelta, Content: "answer"}, want: true},
		{name: "artifact output", event: domain.ChatStreamEvent{Type: domain.ChatEventArtifact, Content: `{"kind":"artifact"}`}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := tracker.Observe(test.event); got != test.want {
				t.Fatalf("Observe()=%v, want %v for event %+v", got, test.want, test.event)
			}
		})
	}
}

type contextBlockingReader struct {
	ctx context.Context
}

func (r *contextBlockingReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

type periodicHeartbeatReader struct {
	ctx      context.Context
	interval time.Duration
}

func (r *periodicHeartbeatReader) Read(target []byte) (int, error) {
	timer := time.NewTimer(r.interval)
	defer timer.Stop()
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-timer.C:
		return copy(target, []byte("event: heartbeat\ndata: {}\n\n")), nil
	}
}

type oneByteReader struct {
	data []byte
}

func (r *oneByteReader) Read(target []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	target[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}
