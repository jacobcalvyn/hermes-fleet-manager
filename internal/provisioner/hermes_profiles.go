package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

const (
	maximumHermesProfileResponseBytes  = 1 << 20
	hermesProfileMutationTimeout       = 2 * time.Minute
	hermesProfileGatewayRestartTimeout = 60 * time.Second
)

const (
	hermesDashboardUsernameEnvironmentKey = "HERMES_DASHBOARD_BASIC_AUTH_USERNAME"
	hermesDashboardPasswordEnvironmentKey = "HERMES_DASHBOARD_BASIC_AUTH_PASSWORD"
)

type hermesProfileSession struct {
	cookies []*http.Cookie
}

type hermesProfileHTTPResponse struct {
	body       []byte
	cookies    []*http.Cookie
	statusCode int
}

type hermesAuthProviderDocument struct {
	Name             string `json:"name"`
	ID               string `json:"id"`
	Type             string `json:"type"`
	SupportsPassword bool   `json:"supports_password"`
}

type hermesAuthProvidersDocument struct {
	Providers []hermesAuthProviderDocument `json:"providers"`
}

type hermesProfileDocument struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Active         bool   `json:"active"`
	Default        bool   `json:"default"`
	IsDefault      bool   `json:"is_default"`
	GatewayRunning bool   `json:"gateway_running"`
}

type hermesProfileListDocument struct {
	Profiles []hermesProfileDocument `json:"profiles"`
}

func (p *Provisioner) inspectHermesProfiles(ctx context.Context, payload domain.HermesProfileInspectPayload) domain.JobResult {
	profiles, err := p.hermesProfiles(ctx, payload)
	if err != nil {
		return domain.JobResult{Success: false, Error: err.Error()}
	}
	return domain.JobResult{
		Success:        true,
		Message:        "Hermes profiles refreshed",
		HermesProfiles: &domain.HermesProfileInventory{InstanceID: payload.InstanceID, Profiles: profiles},
	}
}

func (p *Provisioner) createHermesProfile(ctx context.Context, payload domain.HermesProfileCreatePayload) domain.JobResult {
	if err := domain.ValidateHermesProfileName(payload.ProfileName); err != nil ||
		domain.ValidateHermesProfileReference(payload.CloneFrom) != nil ||
		payload.ProfileName == payload.CloneFrom || len(payload.Description) > 1000 {
		return domain.JobResult{Success: false, Error: "invalid Hermes profile creation request"}
	}
	session, err := p.hermesProfileSession(ctx, payload.HermesProfileInspectPayload)
	if err != nil {
		return domain.JobResult{Success: false, Error: err.Error()}
	}
	profiles, err := p.hermesProfilesWithSession(ctx, payload.HermesProfileInspectPayload, session)
	if err != nil {
		return domain.JobResult{Success: false, Error: "Hermes profile creation preflight failed: " + err.Error()}
	}
	description := strings.TrimSpace(payload.Description)
	switch hermesProfileCreateDisposition(profiles, payload.ProfileName, description) {
	case hermesProfileAlreadyManaged:
		return domain.JobResult{
			Success: true, Message: "Hermes profile already exists",
			HermesProfiles: &domain.HermesProfileInventory{InstanceID: payload.InstanceID, Profiles: profiles},
		}
	case hermesProfileNameConflict:
		return domain.JobResult{Success: false, Error: "Hermes profile name is already owned by a different configuration"}
	}
	body, err := json.Marshal(map[string]any{
		"name": payload.ProfileName, "clone_from": payload.CloneFrom, "clone_all": false,
		"description": description,
	})
	if err != nil {
		return domain.JobResult{Success: false, Error: "encode Hermes profile creation request"}
	}
	if _, err := p.hermesProfileRequest(ctx, payload.DashboardPort, session, http.MethodPost, "/api/profiles", body); err != nil {
		return domain.JobResult{Success: false, Error: "Hermes profile creation failed: " + err.Error()}
	}
	profiles, err = p.hermesProfilesWithSession(ctx, payload.HermesProfileInspectPayload, session)
	if err != nil {
		return domain.JobResult{Success: false, Error: "Hermes profile was created but inventory refresh failed"}
	}
	return domain.JobResult{
		Success:        true,
		Message:        "Hermes profile created",
		HermesProfiles: &domain.HermesProfileInventory{InstanceID: payload.InstanceID, Profiles: profiles},
	}
}

func (p *Provisioner) activateHermesProfile(ctx context.Context, payload domain.HermesProfileMutationPayload) domain.JobResult {
	if domain.ValidateHermesProfileReference(payload.ProfileName) != nil {
		return domain.JobResult{Success: false, Error: "invalid Hermes profile activation request"}
	}
	session, err := p.hermesProfileSession(ctx, payload.HermesProfileInspectPayload)
	if err != nil {
		return domain.JobResult{Success: false, Error: err.Error()}
	}
	return p.activateHermesProfileWithSession(ctx, payload, session)
}

func (p *Provisioner) activateHermesProfileWithSession(ctx context.Context, payload domain.HermesProfileMutationPayload, session *hermesProfileSession) domain.JobResult {
	profiles, err := p.hermesProfilesWithSession(ctx, payload.HermesProfileInspectPayload, session)
	if err != nil {
		return domain.JobResult{Success: false, Error: "Hermes profile activation preflight failed: " + err.Error()}
	}
	profile, found := hermesProfileByName(profiles, payload.ProfileName)
	if !found {
		return domain.JobResult{Success: false, Error: "Hermes profile does not exist"}
	}
	if profile.Active {
		return domain.JobResult{
			Success: true, Message: "Hermes profile is already active",
			HermesProfiles: &domain.HermesProfileInventory{InstanceID: payload.InstanceID, Profiles: profiles},
		}
	}
	body, err := json.Marshal(map[string]string{"name": payload.ProfileName})
	if err != nil {
		return domain.JobResult{Success: false, Error: "encode Hermes profile activation request"}
	}
	response, requestErr := p.hermesProfileHTTPRequest(ctx, payload.DashboardPort, session, http.MethodPost, "/api/profiles/active", body)
	profiles, refreshErr := p.hermesProfilesWithSession(ctx, payload.HermesProfileInspectPayload, session)
	if refreshErr == nil {
		if activated, exists := hermesProfileByName(profiles, payload.ProfileName); exists && activated.Active {
			return domain.JobResult{
				Success: true, Message: "Hermes active profile updated",
				HermesProfiles: &domain.HermesProfileInventory{InstanceID: payload.InstanceID, Profiles: profiles},
			}
		}
	}
	if requestErr != nil {
		return domain.JobResult{Success: false, Error: "Hermes profile activation failed: " + requestErr.Error()}
	}
	if response.statusCode < 200 || response.statusCode > 299 {
		return domain.JobResult{Success: false, Error: fmt.Sprintf("Hermes profile activation failed: Hermes dashboard returned HTTP %d", response.statusCode)}
	}
	if refreshErr != nil {
		return domain.JobResult{Success: false, Error: "Hermes active profile changed but inventory refresh failed"}
	}
	return domain.JobResult{Success: false, Error: "Hermes did not activate the requested profile"}
}

func (p *Provisioner) deleteHermesProfile(ctx context.Context, payload domain.HermesProfileMutationPayload) domain.JobResult {
	if domain.ValidateHermesProfileReference(payload.ProfileName) != nil || payload.ProfileName == "default" {
		return domain.JobResult{Success: false, Error: "invalid Hermes profile deletion request"}
	}
	session, err := p.hermesProfileSession(ctx, payload.HermesProfileInspectPayload)
	if err != nil {
		return domain.JobResult{Success: false, Error: err.Error()}
	}
	result := p.deleteHermesProfileWithSession(ctx, payload, session)
	if !result.Success {
		return result
	}
	managedPath, err := p.safeManagedPath(payload.ManagedPath)
	if err != nil {
		return domain.JobResult{Success: false, Error: "Hermes profile was deleted but the managed runtime could not be restarted"}
	}
	if output, restartErr := p.recreateHermesAfterProfileDeletion(ctx, managedPath, payload.ProjectName); restartErr != nil {
		return failureWithOutput("Hermes profile was deleted but the Hermes gateway could not be restarted", restartErr, output)
	}
	profiles, err := p.hermesProfilesWithSession(ctx, payload.HermesProfileInspectPayload, session)
	if err != nil {
		return domain.JobResult{Success: false, Error: "Hermes profile was deleted but final inventory verification failed"}
	}
	if _, exists := hermesProfileByName(profiles, payload.ProfileName); exists {
		return domain.JobResult{Success: false, Error: "Hermes profile reappeared after the gateway restart"}
	}
	return domain.JobResult{
		Success: true, Message: result.Message,
		HermesProfiles: &domain.HermesProfileInventory{InstanceID: payload.InstanceID, Profiles: profiles},
	}
}

func (p *Provisioner) recreateHermesAfterProfileDeletion(ctx context.Context, managedPath, projectName string) (string, error) {
	return p.compose(
		ctx, managedPath, projectName,
		"up", "-d", "--no-deps", "--force-recreate", "--wait", "--wait-timeout",
		fmt.Sprintf("%d", int(hermesProfileGatewayRestartTimeout.Seconds())), "hermes",
	)
}

func (p *Provisioner) deleteHermesProfileWithSession(ctx context.Context, payload domain.HermesProfileMutationPayload, session *hermesProfileSession) domain.JobResult {
	profiles, err := p.hermesProfilesWithSession(ctx, payload.HermesProfileInspectPayload, session)
	if err != nil {
		return domain.JobResult{Success: false, Error: "Hermes profile deletion preflight failed: " + err.Error()}
	}
	if _, found := hermesProfileByName(profiles, payload.ProfileName); !found {
		return domain.JobResult{
			Success: true, Message: "Hermes profile is already absent",
			HermesProfiles: &domain.HermesProfileInventory{InstanceID: payload.InstanceID, Profiles: profiles},
		}
	}
	response, requestErr := p.hermesProfileHTTPRequest(ctx, payload.DashboardPort, session, http.MethodDelete, "/api/profiles/"+payload.ProfileName, nil)
	profiles, refreshErr := p.hermesProfilesWithSession(ctx, payload.HermesProfileInspectPayload, session)
	if refreshErr == nil {
		if _, exists := hermesProfileByName(profiles, payload.ProfileName); !exists {
			return domain.JobResult{
				Success: true, Message: "Hermes profile deleted",
				HermesProfiles: &domain.HermesProfileInventory{InstanceID: payload.InstanceID, Profiles: profiles},
			}
		}
	}
	if requestErr != nil {
		return domain.JobResult{Success: false, Error: "Hermes profile deletion failed: " + requestErr.Error()}
	}
	if response.statusCode < 200 || response.statusCode > 299 {
		return domain.JobResult{Success: false, Error: fmt.Sprintf("Hermes profile deletion failed: Hermes dashboard returned HTTP %d", response.statusCode)}
	}
	if refreshErr != nil {
		return domain.JobResult{Success: false, Error: "Hermes profile was deleted but inventory refresh failed"}
	}
	return domain.JobResult{Success: false, Error: "Hermes profile deletion was not confirmed"}
}

func hermesProfileByName(profiles []domain.HermesProfile, name string) (domain.HermesProfile, bool) {
	for _, profile := range profiles {
		if profile.Name == name {
			return profile, true
		}
	}
	return domain.HermesProfile{}, false
}

type hermesProfileCreateState uint8

const (
	hermesProfileNeedsCreate hermesProfileCreateState = iota
	hermesProfileAlreadyManaged
	hermesProfileNameConflict
)

func hermesProfileCreateDisposition(profiles []domain.HermesProfile, name, description string) hermesProfileCreateState {
	for _, profile := range profiles {
		if profile.Name != name {
			continue
		}
		if strings.TrimSpace(profile.Description) == description {
			return hermesProfileAlreadyManaged
		}
		return hermesProfileNameConflict
	}
	return hermesProfileNeedsCreate
}

func (p *Provisioner) repairHermesProfileAccess(ctx context.Context, payload domain.HermesProfileInspectPayload) domain.JobResult {
	managedPath, err := p.validateHermesProfileTarget(ctx, payload)
	if err != nil {
		return domain.JobResult{Success: false, Error: "Hermes profile access repair failed: " + err.Error()}
	}
	if output, err := p.repairHermesProfileDashboard(ctx, managedPath, payload.ProjectName, payload.DashboardPort); err != nil {
		return failureWithOutput("Hermes profile access repair could not restore the dashboard", err, output)
	}
	session, err := p.hermesProfileSession(ctx, payload)
	if err != nil {
		return domain.JobResult{Success: false, Error: "Hermes profile access repair failed: " + err.Error()}
	}
	profiles, err := p.hermesProfilesWithSession(ctx, payload, session)
	if err != nil {
		return domain.JobResult{Success: false, Error: "Hermes profile access repair verification failed: " + err.Error()}
	}
	return domain.JobResult{
		Success:        true,
		Message:        "Hermes profile access repaired and verified",
		HermesProfiles: &domain.HermesProfileInventory{InstanceID: payload.InstanceID, Profiles: profiles},
	}
}

func (p *Provisioner) repairHermesProfileDashboard(ctx context.Context, managedPath, projectName string, dashboardPort int) (string, error) {
	if err := rotateDashboardSessionTokenFile(ctx, filepath.Join(managedPath, ".env")); err != nil {
		return "", fmt.Errorf("rotate dashboard session token: %w", err)
	}
	output, err := p.compose(ctx, managedPath, projectName, "up", "-d", "--no-deps", "--force-recreate", "dashboard")
	if err != nil {
		return output, err
	}
	if err := p.waitForDashboard(ctx, dashboardPort, 60*time.Second); err != nil {
		return output, err
	}
	return output, nil
}

func (p *Provisioner) hermesProfiles(ctx context.Context, payload domain.HermesProfileInspectPayload) ([]domain.HermesProfile, error) {
	session, err := p.hermesProfileSession(ctx, payload)
	if err != nil {
		return nil, err
	}
	return p.hermesProfilesWithSession(ctx, payload, session)
}

func (p *Provisioner) hermesProfilesWithSession(ctx context.Context, payload domain.HermesProfileInspectPayload, session *hermesProfileSession) ([]domain.HermesProfile, error) {
	body, err := p.hermesProfileRequest(ctx, payload.DashboardPort, session, http.MethodGet, "/api/profiles", nil)
	if err != nil {
		return nil, fmt.Errorf("Hermes profile inventory failed: %w", err)
	}
	var wrapped hermesProfileListDocument
	if err := json.Unmarshal(body, &wrapped); err != nil || wrapped.Profiles == nil {
		var profiles []hermesProfileDocument
		if listErr := json.Unmarshal(body, &profiles); listErr != nil {
			return nil, fmt.Errorf("Hermes returned an invalid profile inventory")
		}
		wrapped.Profiles = profiles
	}
	profiles := make([]domain.HermesProfile, 0, len(wrapped.Profiles))
	for _, profile := range wrapped.Profiles {
		profiles = append(profiles, domain.HermesProfile{
			Name: profile.Name, Description: profile.Description, Provider: profile.Provider, Model: profile.Model,
			Active: profile.Active, Default: profile.Default || profile.IsDefault, GatewayRunning: profile.GatewayRunning,
		})
	}
	inventory := &domain.HermesProfileInventory{InstanceID: payload.InstanceID, Profiles: profiles}
	if err := domain.ValidateHermesProfileInventory(inventory); err != nil {
		return nil, fmt.Errorf("Hermes returned unsafe profile metadata")
	}
	return inventory.Profiles, nil
}

func (p *Provisioner) validateHermesProfileTarget(ctx context.Context, payload domain.HermesProfileInspectPayload) (string, error) {
	if !instanceIDPattern.MatchString(payload.InstanceID) || !safeNamePattern.MatchString(payload.Name) ||
		payload.ProjectName == "" || payload.DashboardPort < 1 || payload.DashboardPort > 65535 {
		return "", fmt.Errorf("invalid Hermes profile target")
	}
	managedPath, err := p.safeManagedPath(payload.ManagedPath)
	if err != nil {
		return "", fmt.Errorf("Hermes profile target is not managed by Fleet")
	}
	containers, err := p.inspectOwnedFleetContainers(ctx, payload.InstanceID, payload.ProjectName)
	if err != nil {
		return "", fmt.Errorf("Hermes profile target ownership could not be verified")
	}
	dashboardReady := false
	for _, container := range containers {
		if container.Config.Labels["com.docker.compose.service"] == "dashboard" &&
			container.State.Status == "running" &&
			(container.State.Health == nil || container.State.Health.Status == "healthy") {
			dashboardReady = true
		}
	}
	if !dashboardReady {
		return "", fmt.Errorf("Hermes dashboard is not ready")
	}
	return managedPath, nil
}

func (p *Provisioner) hermesProfileSession(ctx context.Context, payload domain.HermesProfileInspectPayload) (*hermesProfileSession, error) {
	managedPath, err := p.validateHermesProfileTarget(ctx, payload)
	if err != nil {
		return nil, err
	}
	envPath := filepath.Join(managedPath, ".env")
	if err := requireRegularFile(envPath); err != nil {
		return nil, fmt.Errorf("Hermes dashboard credentials are unavailable in the managed environment")
	}
	username, err := readManagedEnvValue(envPath, hermesDashboardUsernameEnvironmentKey)
	if err != nil {
		return nil, fmt.Errorf("Hermes dashboard username is unavailable in the managed environment")
	}
	password, err := readManagedEnvValue(envPath, hermesDashboardPasswordEnvironmentKey)
	if err != nil {
		return nil, fmt.Errorf("Hermes dashboard password is unavailable in the managed environment")
	}
	return p.authenticateHermesProfileSession(ctx, payload.DashboardPort, username, password)
}

func (p *Provisioner) authenticateHermesProfileSession(ctx context.Context, port int, username, password string) (*hermesProfileSession, error) {
	providersResponse, err := p.hermesProfileHTTPRequest(ctx, port, nil, http.MethodGet, "/api/auth/providers", nil)
	if err != nil {
		return nil, fmt.Errorf("Hermes authentication provider probe failed: %w", err)
	}
	if providersResponse.statusCode < 200 || providersResponse.statusCode > 299 {
		return nil, fmt.Errorf("Hermes authentication provider probe returned HTTP %d", providersResponse.statusCode)
	}
	provider, err := hermesPasswordProvider(providersResponse.body)
	if err != nil {
		return nil, err
	}
	loginBody, err := json.Marshal(map[string]string{"provider": provider, "username": username, "password": password})
	if err != nil {
		return nil, fmt.Errorf("encode Hermes dashboard profile login")
	}
	loginResponse, err := p.hermesProfileHTTPRequest(ctx, port, nil, http.MethodPost, "/auth/password-login", loginBody)
	if err != nil {
		return nil, fmt.Errorf("Hermes dashboard profile login failed: %w", err)
	}
	if loginResponse.statusCode < 200 || loginResponse.statusCode > 299 {
		return nil, fmt.Errorf("Hermes dashboard profile login was rejected with HTTP %d", loginResponse.statusCode)
	}
	if len(loginResponse.cookies) == 0 {
		return nil, fmt.Errorf("Hermes dashboard profile login did not establish a session")
	}
	return &hermesProfileSession{cookies: loginResponse.cookies}, nil
}

func hermesPasswordProvider(body []byte) (string, error) {
	var wrapped hermesAuthProvidersDocument
	if err := json.Unmarshal(body, &wrapped); err != nil || wrapped.Providers == nil {
		var providers []hermesAuthProviderDocument
		if listErr := json.Unmarshal(body, &providers); listErr != nil {
			return "", fmt.Errorf("Hermes returned an invalid authentication provider document")
		}
		wrapped.Providers = providers
	}
	for _, provider := range wrapped.Providers {
		name := strings.TrimSpace(provider.Name)
		if name == "" {
			name = strings.TrimSpace(provider.ID)
		}
		providerType := strings.TrimSpace(provider.Type)
		if name == "basic" || providerType == "basic" || provider.SupportsPassword {
			if name == "" {
				name = "basic"
			}
			return name, nil
		}
	}
	return "", fmt.Errorf("Hermes dashboard does not expose a password authentication provider")
}

func (p *Provisioner) hermesProfileRequest(ctx context.Context, port int, session *hermesProfileSession, method, requestPath string, body []byte) ([]byte, error) {
	response, err := p.hermesProfileHTTPRequest(ctx, port, session, method, requestPath, body)
	if err != nil {
		return nil, err
	}
	if response.statusCode < 200 || response.statusCode > 299 {
		return nil, fmt.Errorf("Hermes dashboard returned HTTP %d", response.statusCode)
	}
	return response.body, nil
}

func (p *Provisioner) hermesProfileHTTPRequest(ctx context.Context, port int, session *hermesProfileSession, method, requestPath string, body []byte) (hermesProfileHTTPResponse, error) {
	request, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", port, requestPath), bytes.NewReader(body))
	if err != nil {
		return hermesProfileHTTPResponse{}, err
	}
	request.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	if session != nil {
		for _, cookie := range session.cookies {
			request.AddCookie(cookie)
		}
	}
	client := p.hermesProfileHTTPClient(method, requestPath)
	response, err := client.Do(request)
	if err != nil {
		return hermesProfileHTTPResponse{}, err
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maximumHermesProfileResponseBytes+1))
	if err != nil || len(encoded) > maximumHermesProfileResponseBytes {
		return hermesProfileHTTPResponse{}, fmt.Errorf("Hermes profile response is invalid")
	}
	return hermesProfileHTTPResponse{body: encoded, cookies: response.Cookies(), statusCode: response.StatusCode}, nil
}

func (p *Provisioner) hermesProfileHTTPClient(method, requestPath string) *http.Client {
	profileMutation := method == http.MethodPost && requestPath == "/api/profiles"
	profileDeletion := method == http.MethodDelete && strings.HasPrefix(requestPath, "/api/profiles/")
	if !profileMutation && !profileDeletion {
		return p.httpClient
	}
	mutationClient := *p.httpClient
	mutationClient.Timeout = hermesProfileMutationTimeout
	return &mutationClient
}
