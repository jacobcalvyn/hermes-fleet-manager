package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

const maximumHermesCapabilityResponseBytes = 1 << 20

type hermesCapabilityDocument struct {
	Platform string `json:"platform"`
	Model    string `json:"model"`
	Runtime  struct {
		Mode          string `json:"mode"`
		ToolExecution string `json:"tool_execution"`
		SplitRuntime  bool   `json:"split_runtime"`
	} `json:"runtime"`
	Features map[string]json.RawMessage `json:"features"`
}

type hermesSkillListDocument struct {
	Data []domain.HermesSkillCapability `json:"data"`
}

type hermesToolsetListDocument struct {
	Data []domain.HermesToolsetCapability `json:"data"`
}

func (p *Provisioner) inspectHermesCapabilities(ctx context.Context, payload domain.HermesCapabilityInspectPayload) domain.JobResult {
	if payload.InstanceID == "" || payload.Name == "" || payload.ProjectName == "" ||
		payload.APIPort < 1 || payload.APIPort > 65535 {
		return domain.JobResult{Success: false, Error: "Hermes capability payload is incomplete"}
	}
	managedPath, err := p.safeManagedPath(payload.ManagedPath)
	if err != nil {
		return failure("unsafe managed path", err)
	}
	apiKey, err := readManagedEnvValue(filepath.Join(managedPath, ".env"), "API_SERVER_KEY")
	if err != nil {
		return failure("read Hermes API credential", err)
	}
	defer func() { apiKey = "" }()

	var capabilities hermesCapabilityDocument
	if err := p.readHermesCapabilityEndpoint(ctx, payload.APIPort, apiKey, "/v1/capabilities", &capabilities); err != nil {
		return domain.JobResult{Success: false, Error: "Hermes capability inventory failed: " + err.Error()}
	}
	var skills hermesSkillListDocument
	if err := p.readHermesCapabilityEndpoint(ctx, payload.APIPort, apiKey, "/v1/skills", &skills); err != nil {
		return domain.JobResult{Success: false, Error: "Hermes skill inventory failed: " + err.Error()}
	}
	var toolsets hermesToolsetListDocument
	if err := p.readHermesCapabilityEndpoint(ctx, payload.APIPort, apiKey, "/v1/toolsets", &toolsets); err != nil {
		return domain.JobResult{Success: false, Error: "Hermes toolset inventory failed: " + err.Error()}
	}

	features := make(map[string]bool)
	for name, encoded := range capabilities.Features {
		var enabled bool
		if json.Unmarshal(encoded, &enabled) == nil {
			features[name] = enabled
		}
	}
	_, browserErr := p.compose(
		ctx, managedPath, payload.ProjectName,
		"exec", "-T", "hermes", "sh", "-lc",
		`test -n "${AGENT_BROWSER_EXECUTABLE_PATH:-}" && test -x "$AGENT_BROWSER_EXECUTABLE_PATH"`,
	)
	browser := domain.HermesBrowserCapability{Available: browserErr == nil}
	if browser.Available {
		browser.Implementation = "playwright-chromium.v1"
	}
	inventory := &domain.HermesCapabilityInventory{
		InstanceID: payload.InstanceID,
		Platform:   strings.TrimSpace(capabilities.Platform), Model: strings.TrimSpace(capabilities.Model),
		RuntimeMode:   strings.TrimSpace(capabilities.Runtime.Mode),
		ToolExecution: strings.TrimSpace(capabilities.Runtime.ToolExecution),
		SplitRuntime:  capabilities.Runtime.SplitRuntime,
		Features:      features, Skills: skills.Data, Toolsets: toolsets.Data, Browser: browser,
	}
	if err := domain.ValidateHermesCapabilityInventory(inventory); err != nil {
		return domain.JobResult{Success: false, Error: "Hermes returned an invalid capability inventory"}
	}
	return domain.JobResult{
		Success: true, Message: "Hermes capabilities refreshed", HermesCapabilities: inventory,
	}
}

func (p *Provisioner) readHermesCapabilityEndpoint(ctx context.Context, apiPort int, apiKey, endpoint string, target any) error {
	if !strings.HasPrefix(endpoint, "/v1/") {
		return errors.New("Hermes capability endpoint is invalid")
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", apiPort, endpoint), nil,
	)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Accept", "application/json")
	response, err := p.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Hermes API returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumHermesCapabilityResponseBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maximumHermesCapabilityResponseBytes {
		return errors.New("Hermes capability response exceeded 1 MiB")
	}
	if err := json.Unmarshal(body, target); err != nil {
		return errors.New("Hermes API returned invalid capability JSON")
	}
	return nil
}
