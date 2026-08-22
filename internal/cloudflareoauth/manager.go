package cloudflareoauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/cloudflare"
)

const (
	defaultAuthorizationURL = "https://dash.cloudflare.com/oauth2/auth"
	defaultTokenURL         = "https://dash.cloudflare.com/oauth2/token"
	defaultAPIBaseURL       = "https://api.cloudflare.com/client/v4"
	managedDNSComment       = "Managed by Hermes Fleet"
	sessionTTL              = 10 * time.Minute
	oauthClientsURL         = "https://dash.cloudflare.com/?to=/:account/oauth-clients"
	oauthDocumentationURL   = "https://developers.cloudflare.com/fundamentals/oauth/create-an-oauth-client/"
)

var requiredScopes = []RequiredScope{
	{ID: "account-settings.read", Name: "Account Settings Read", Purpose: "List the Cloudflare accounts available to the signed-in user."},
	{ID: "zone.read", Name: "Zone Read", Purpose: "List and select the DNS zone Fleet will publish into."},
	{ID: "dns.write", Name: "DNS Write", Purpose: "Create and reconcile Fleet-owned DNS records."},
	{ID: "argotunnel.write", Name: "Cloudflare Tunnel Write", Purpose: "Create and configure Fleet-owned Cloudflare tunnels."},
	{ID: "offline_access", Name: "Offline access", Purpose: "Renew authorization without asking the user to sign in again."},
}

type RequiredScope struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
}

type ClientSetup struct {
	ClientConfigured bool            `json:"client_configured"`
	ClientID         string          `json:"client_id,omitempty"`
	RedirectURL      string          `json:"redirect_url"`
	OAuthClientsURL  string          `json:"oauth_clients_url"`
	DocumentationURL string          `json:"documentation_url"`
	Scopes           []RequiredScope `json:"scopes"`
}

func DefaultScopes() []string {
	scopes := make([]string, 0, len(requiredScopes))
	for _, scope := range requiredScopes {
		scopes = append(scopes, scope.ID)
	}
	return scopes
}

type Config struct {
	ClientID         string
	RedirectURL      string
	Scopes           []string
	AuthorizationURL string
	TokenURL         string
	APIBaseURL       string
	HTTPClient       *http.Client
	Now              func() time.Time
}

type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Zone struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	AccountID string `json:"account_id"`
}

type Session struct {
	ID        string    `json:"id"`
	Accounts  []Account `json:"accounts"`
	Zones     []Zone    `json:"zones"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ProvisionRequest struct {
	SessionID      string
	AccountID      string
	ZoneID         string
	FleetNamespace string
}

type Provisioned struct {
	Cloudflare cloudflare.Config
	Account    Account
	Zone       Zone
}

type pendingAuthorization struct {
	Verifier  string
	ExpiresAt time.Time
}

type authorizedSession struct {
	View       Session
	Credential cloudflare.OAuthCredentials
}

type Manager struct {
	config Config
	client *http.Client
	now    func() time.Time

	mu         sync.Mutex
	pending    map[string]pendingAuthorization
	authorized map[string]authorizedSession
}

func New(config Config) (*Manager, error) {
	config.ClientID = strings.TrimSpace(config.ClientID)
	config.RedirectURL = strings.TrimSpace(config.RedirectURL)
	config.AuthorizationURL = strings.TrimSpace(config.AuthorizationURL)
	config.TokenURL = strings.TrimSpace(config.TokenURL)
	config.APIBaseURL = strings.TrimRight(strings.TrimSpace(config.APIBaseURL), "/")
	if config.AuthorizationURL == "" {
		config.AuthorizationURL = defaultAuthorizationURL
	}
	if config.TokenURL == "" {
		config.TokenURL = defaultTokenURL
	}
	if config.APIBaseURL == "" {
		config.APIBaseURL = defaultAPIBaseURL
	}
	if len(normalizedScopes(config.Scopes)) == 0 {
		config.Scopes = DefaultScopes()
	}
	if config.ClientID == "" && config.RedirectURL == "" {
		return &Manager{config: config, client: normalizedClient(config.HTTPClient), now: normalizedClock(config.Now), pending: map[string]pendingAuthorization{}, authorized: map[string]authorizedSession{}}, nil
	}
	if config.RedirectURL == "" {
		return nil, errors.New("Cloudflare OAuth requires a redirect URL")
	}
	redirect, err := url.Parse(config.RedirectURL)
	if err != nil || redirect.Scheme == "" || redirect.Host == "" {
		return nil, errors.New("Cloudflare OAuth redirect URL must be absolute")
	}
	return &Manager{config: config, client: normalizedClient(config.HTTPClient), now: normalizedClock(config.Now), pending: map[string]pendingAuthorization{}, authorized: map[string]authorizedSession{}}, nil
}

func normalizedClient(client *http.Client) *http.Client {
	if client == nil {
		return &http.Client{Timeout: 20 * time.Second}
	}
	return client
}

func normalizedClock(now func() time.Time) func() time.Time {
	if now == nil {
		return time.Now
	}
	return now
}

func (manager *Manager) Enabled() bool {
	if manager == nil {
		return false
	}
	config := manager.configSnapshot()
	return config.ClientID != "" && config.RedirectURL != ""
}

func (manager *Manager) Setup() ClientSetup {
	if manager == nil {
		return ClientSetup{OAuthClientsURL: oauthClientsURL, DocumentationURL: oauthDocumentationURL, Scopes: append([]RequiredScope(nil), requiredScopes...)}
	}
	config := manager.configSnapshot()
	return ClientSetup{
		ClientConfigured: config.ClientID != "" && config.RedirectURL != "",
		ClientID:         config.ClientID,
		RedirectURL:      config.RedirectURL,
		OAuthClientsURL:  oauthClientsURL,
		DocumentationURL: oauthDocumentationURL,
		Scopes:           append([]RequiredScope(nil), requiredScopes...),
	}
}

func ValidateClientID(clientID string) error {
	clientID = strings.TrimSpace(clientID)
	if len(clientID) < 8 || len(clientID) > 256 {
		return errors.New("Cloudflare OAuth client ID must be between 8 and 256 characters")
	}
	for _, character := range clientID {
		if character <= 0x20 || character == 0x7f {
			return errors.New("Cloudflare OAuth client ID cannot contain whitespace or control characters")
		}
	}
	return nil
}

func (manager *Manager) ConfigureClient(clientID string) error {
	if manager == nil {
		return errors.New("Cloudflare OAuth runtime is unavailable")
	}
	clientID = strings.TrimSpace(clientID)
	if err := ValidateClientID(clientID); err != nil {
		return err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.config.RedirectURL == "" {
		return errors.New("Cloudflare OAuth redirect URL is unavailable")
	}
	manager.config.ClientID = clientID
	manager.pending = map[string]pendingAuthorization{}
	manager.authorized = map[string]authorizedSession{}
	return nil
}

func (manager *Manager) configSnapshot() Config {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	config := manager.config
	config.Scopes = append([]string(nil), config.Scopes...)
	return config
}

func (manager *Manager) Begin() (string, error) {
	config := manager.configSnapshot()
	if config.ClientID == "" || config.RedirectURL == "" {
		return "", errors.New("Cloudflare OAuth is not enabled for this Fleet build")
	}
	state, err := randomToken(32)
	if err != nil {
		return "", err
	}
	verifier, err := randomToken(48)
	if err != nil {
		return "", err
	}
	challenge := sha256.Sum256([]byte(verifier))
	now := manager.now().UTC()
	manager.mu.Lock()
	manager.pruneLocked(now)
	manager.pending[state] = pendingAuthorization{Verifier: verifier, ExpiresAt: now.Add(sessionTTL)}
	manager.mu.Unlock()

	parameters := url.Values{
		"response_type":         {"code"},
		"client_id":             {config.ClientID},
		"redirect_uri":          {config.RedirectURL},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	if scopes := normalizedScopes(config.Scopes); len(scopes) > 0 {
		parameters.Set("scope", strings.Join(scopes, " "))
	}
	return config.AuthorizationURL + "?" + parameters.Encode(), nil
}

func (manager *Manager) CompleteAuthorization(ctx context.Context, state, code string) (Session, error) {
	state = strings.TrimSpace(state)
	code = strings.TrimSpace(code)
	if state == "" || code == "" {
		return Session{}, errors.New("Cloudflare OAuth callback is missing state or code")
	}
	now := manager.now().UTC()
	manager.mu.Lock()
	manager.pruneLocked(now)
	pending, ok := manager.pending[state]
	delete(manager.pending, state)
	manager.mu.Unlock()
	if !ok || !pending.ExpiresAt.After(now) {
		return Session{}, errors.New("Cloudflare OAuth request expired or was already used")
	}
	credential, err := manager.exchange(ctx, code, pending.Verifier)
	if err != nil {
		return Session{}, err
	}
	accounts, zones, err := manager.resources(ctx, credential.AccessToken)
	if err != nil {
		return Session{}, err
	}
	if len(accounts) == 0 || len(zones) == 0 {
		return Session{}, errors.New("Cloudflare authorization has no accessible account with a DNS zone")
	}
	sessionID, err := randomToken(32)
	if err != nil {
		return Session{}, err
	}
	view := Session{ID: sessionID, Accounts: accounts, Zones: zones, ExpiresAt: now.Add(sessionTTL)}
	manager.mu.Lock()
	manager.authorized[sessionID] = authorizedSession{View: view, Credential: credential}
	manager.mu.Unlock()
	return view, nil
}

func (manager *Manager) Session(id string) (Session, error) {
	now := manager.now().UTC()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.pruneLocked(now)
	session, ok := manager.authorized[strings.TrimSpace(id)]
	if !ok {
		return Session{}, errors.New("Cloudflare authorization session expired or does not exist")
	}
	return session.View, nil
}

func (manager *Manager) Provision(ctx context.Context, request ProvisionRequest) (Provisioned, error) {
	request.SessionID = strings.TrimSpace(request.SessionID)
	request.AccountID = strings.TrimSpace(request.AccountID)
	request.ZoneID = strings.TrimSpace(request.ZoneID)
	namespace, err := cloudflare.NormalizeFleetNamespace(request.FleetNamespace)
	if err != nil {
		return Provisioned{}, err
	}
	now := manager.now().UTC()
	manager.mu.Lock()
	manager.pruneLocked(now)
	session, ok := manager.authorized[request.SessionID]
	manager.mu.Unlock()
	if !ok {
		return Provisioned{}, errors.New("Cloudflare authorization session expired or does not exist")
	}
	account, zone, err := selectResources(session.View, request.AccountID, request.ZoneID)
	if err != nil {
		return Provisioned{}, err
	}
	accessToken := session.Credential.AccessToken
	adminTunnel, err := manager.ensureTunnel(ctx, accessToken, account.ID, "hermes-fleet-"+namespace+"-admin", namespace, "admin")
	if err != nil {
		return Provisioned{}, fmt.Errorf("prepare Fleet Manager tunnel: %w", err)
	}
	instancesTunnel, err := manager.ensureTunnel(ctx, accessToken, account.ID, "hermes-fleet-"+namespace+"-instances", namespace, "instances")
	if err != nil {
		return Provisioned{}, fmt.Errorf("prepare instance tunnel: %w", err)
	}
	if adminTunnel.ID == instancesTunnel.ID {
		return Provisioned{}, errors.New("Cloudflare returned the same tunnel for both Fleet boundaries")
	}
	adminToken, err := manager.connectorToken(ctx, accessToken, account.ID, adminTunnel.ID)
	if err != nil {
		return Provisioned{}, fmt.Errorf("get Fleet Manager connector token: %w", err)
	}
	instancesToken, err := manager.connectorToken(ctx, accessToken, account.ID, instancesTunnel.ID)
	if err != nil {
		return Provisioned{}, fmt.Errorf("get instance connector token: %w", err)
	}
	adminHostname := namespace + "." + zone.Name
	if err := manager.configureTunnel(ctx, accessToken, account.ID, adminTunnel.ID, []map[string]any{{"hostname": adminHostname, "service": "http://control-plane:9180"}}); err != nil {
		return Provisioned{}, fmt.Errorf("configure Fleet Manager tunnel: %w", err)
	}
	if err := manager.configureTunnel(ctx, accessToken, account.ID, instancesTunnel.ID, nil); err != nil {
		return Provisioned{}, fmt.Errorf("configure instance tunnel: %w", err)
	}
	if err := manager.ensureDNS(ctx, accessToken, zone.ID, adminHostname, adminTunnel.ID); err != nil {
		return Provisioned{}, fmt.Errorf("publish Fleet Manager hostname: %w", err)
	}

	manager.mu.Lock()
	delete(manager.authorized, request.SessionID)
	manager.mu.Unlock()
	return Provisioned{
		Account: account,
		Zone:    zone,
		Cloudflare: cloudflare.Config{
			AdminTunnelToken:     adminToken,
			InstancesTunnelToken: instancesToken,
			AdminHostname:        adminHostname,
			RouteAutomation: cloudflare.RouteAutomationConfig{
				AccountID: account.ID, ZoneID: zone.ID, ZoneName: zone.Name,
				FleetNamespace: namespace, TunnelID: instancesTunnel.ID, APIToken: accessToken,
			},
			OAuth: session.Credential,
		},
	}, nil
}

func (manager *Manager) exchange(ctx context.Context, code, verifier string) (cloudflare.OAuthCredentials, error) {
	config := manager.configSnapshot()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {config.ClientID},
		"redirect_uri":  {config.RedirectURL},
		"code":          {code},
		"code_verifier": {verifier},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, config.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return cloudflare.OAuthCredentials{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := manager.client.Do(request)
	if err != nil {
		return cloudflare.OAuthCredentials{}, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return cloudflare.OAuthCredentials{}, err
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
		Description  string `json:"error_description"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		return cloudflare.OAuthCredentials{}, fmt.Errorf("Cloudflare OAuth returned HTTP %d with an invalid response", response.StatusCode)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || result.AccessToken == "" || result.RefreshToken == "" || result.ExpiresIn <= 0 {
		detail := strings.TrimSpace(result.Description)
		if detail == "" {
			detail = strings.TrimSpace(result.Error)
		}
		if detail == "" {
			detail = http.StatusText(response.StatusCode)
		}
		return cloudflare.OAuthCredentials{}, fmt.Errorf("Cloudflare OAuth HTTP %d: %s", response.StatusCode, detail)
	}
	if result.TokenType != "" && !strings.EqualFold(result.TokenType, "bearer") {
		return cloudflare.OAuthCredentials{}, fmt.Errorf("Cloudflare OAuth returned unsupported token type %q", result.TokenType)
	}
	return cloudflare.OAuthCredentials{
		ClientID: config.ClientID, AccessToken: strings.TrimSpace(result.AccessToken),
		RefreshToken: strings.TrimSpace(result.RefreshToken), Scope: strings.TrimSpace(result.Scope),
		ExpiresAt: manager.now().UTC().Add(time.Duration(result.ExpiresIn) * time.Second),
	}, nil
}

func (manager *Manager) resources(ctx context.Context, token string) ([]Account, []Zone, error) {
	var accounts []Account
	if err := manager.api(ctx, token, http.MethodGet, "/accounts?per_page=100", nil, &accounts); err != nil {
		return nil, nil, fmt.Errorf("list Cloudflare accounts: %w", err)
	}
	var rawZones []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	if err := manager.api(ctx, token, http.MethodGet, "/zones?per_page=100&status=active", nil, &rawZones); err != nil {
		return nil, nil, fmt.Errorf("list Cloudflare zones: %w", err)
	}
	knownAccounts := make(map[string]struct{}, len(accounts))
	for _, account := range accounts {
		knownAccounts[account.ID] = struct{}{}
	}
	zones := make([]Zone, 0, len(rawZones))
	for _, zone := range rawZones {
		if _, ok := knownAccounts[zone.Account.ID]; !ok || strings.TrimSpace(zone.ID) == "" || strings.TrimSpace(zone.Name) == "" {
			continue
		}
		zones = append(zones, Zone{ID: zone.ID, Name: strings.ToLower(strings.TrimSpace(zone.Name)), AccountID: zone.Account.ID})
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Name < accounts[j].Name })
	sort.Slice(zones, func(i, j int) bool { return zones[i].Name < zones[j].Name })
	return accounts, zones, nil
}

type tunnel struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Metadata map[string]string `json:"metadata"`
}

func (manager *Manager) ensureTunnel(ctx context.Context, token, accountID, name, namespace, boundary string) (tunnel, error) {
	path := "/accounts/" + url.PathEscape(accountID) + "/cfd_tunnel?is_deleted=false&name=" + url.QueryEscape(name)
	var tunnels []tunnel
	if err := manager.api(ctx, token, http.MethodGet, path, nil, &tunnels); err != nil {
		return tunnel{}, err
	}
	if len(tunnels) > 1 {
		return tunnel{}, fmt.Errorf("multiple Cloudflare tunnels are named %s", name)
	}
	if len(tunnels) == 1 {
		candidate := tunnels[0]
		if candidate.Metadata["managed_by"] != "hermes-fleet" || candidate.Metadata["fleet_namespace"] != namespace || candidate.Metadata["boundary"] != boundary {
			return tunnel{}, fmt.Errorf("tunnel %s already exists but is not owned by this Fleet namespace", name)
		}
		return candidate, nil
	}
	body := map[string]any{
		"name": name, "config_src": "cloudflare",
		"metadata": map[string]string{"managed_by": "hermes-fleet", "fleet_namespace": namespace, "boundary": boundary},
	}
	var created tunnel
	if err := manager.api(ctx, token, http.MethodPost, "/accounts/"+url.PathEscape(accountID)+"/cfd_tunnel", body, &created); err != nil {
		return tunnel{}, err
	}
	if created.ID == "" {
		return tunnel{}, errors.New("Cloudflare created a tunnel without an ID")
	}
	return created, nil
}

func (manager *Manager) connectorToken(ctx context.Context, token, accountID, tunnelID string) (string, error) {
	var connectorToken string
	path := "/accounts/" + url.PathEscape(accountID) + "/cfd_tunnel/" + url.PathEscape(tunnelID) + "/token"
	if err := manager.api(ctx, token, http.MethodGet, path, nil, &connectorToken); err != nil {
		return "", err
	}
	if _, err := cloudflare.TunnelIDFromToken(connectorToken); err != nil {
		return "", fmt.Errorf("Cloudflare returned an invalid connector token: %w", err)
	}
	return connectorToken, nil
}

func (manager *Manager) configureTunnel(ctx context.Context, token, accountID, tunnelID string, ingress []map[string]any) error {
	if ingress == nil {
		ingress = []map[string]any{}
	}
	ingress = append(ingress, map[string]any{"service": "http_status:404"})
	body := map[string]any{"config": map[string]any{"ingress": ingress, "originRequest": map[string]any{}}}
	path := "/accounts/" + url.PathEscape(accountID) + "/cfd_tunnel/" + url.PathEscape(tunnelID) + "/configurations"
	return manager.api(ctx, token, http.MethodPut, path, body, nil)
}

func (manager *Manager) ensureDNS(ctx context.Context, token, zoneID, hostname, tunnelID string) error {
	path := "/zones/" + url.PathEscape(zoneID) + "/dns_records?type=CNAME&name=" + url.QueryEscape(hostname)
	var records []struct {
		ID      string `json:"id"`
		Content string `json:"content"`
		Comment string `json:"comment"`
		Proxied bool   `json:"proxied"`
	}
	if err := manager.api(ctx, token, http.MethodGet, path, nil, &records); err != nil {
		return err
	}
	target := tunnelID + ".cfargotunnel.com"
	if len(records) > 1 {
		return fmt.Errorf("DNS hostname %s has multiple CNAME records", hostname)
	}
	if len(records) == 1 {
		record := records[0]
		if !strings.EqualFold(strings.TrimSpace(record.Content), target) || !record.Proxied || record.Comment != managedDNSComment {
			return fmt.Errorf("DNS hostname %s conflicts with an unmanaged record", hostname)
		}
		return nil
	}
	body := map[string]any{"type": "CNAME", "name": hostname, "content": target, "proxied": true, "ttl": 1, "comment": managedDNSComment}
	return manager.api(ctx, token, http.MethodPost, "/zones/"+url.PathEscape(zoneID)+"/dns_records", body, nil)
}

func (manager *Manager) api(ctx context.Context, token, method, path string, body, output any) error {
	config := manager.configSnapshot()
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, config.APIBaseURL+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := manager.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return err
	}
	var envelope struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("Cloudflare returned HTTP %d with an invalid response", response.StatusCode)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !envelope.Success {
		detail := http.StatusText(response.StatusCode)
		if len(envelope.Errors) > 0 && strings.TrimSpace(envelope.Errors[0].Message) != "" {
			detail = strings.TrimSpace(envelope.Errors[0].Message)
		}
		return fmt.Errorf("Cloudflare API HTTP %d: %s", response.StatusCode, detail)
	}
	if output != nil && len(envelope.Result) > 0 && string(envelope.Result) != "null" {
		if err := json.Unmarshal(envelope.Result, output); err != nil {
			return fmt.Errorf("decode Cloudflare response: %w", err)
		}
	}
	return nil
}

func selectResources(session Session, accountID, zoneID string) (Account, Zone, error) {
	var account Account
	for _, candidate := range session.Accounts {
		if candidate.ID == accountID {
			account = candidate
			break
		}
	}
	if account.ID == "" {
		return Account{}, Zone{}, errors.New("selected Cloudflare account is not authorized")
	}
	var zone Zone
	for _, candidate := range session.Zones {
		if candidate.ID == zoneID && candidate.AccountID == account.ID {
			zone = candidate
			break
		}
	}
	if zone.ID == "" {
		return Account{}, Zone{}, errors.New("selected Cloudflare zone is not authorized for this account")
	}
	return account, zone, nil
}

func normalizedScopes(values []string) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0, len(values))
	for _, value := range values {
		for _, scope := range strings.Fields(value) {
			if _, ok := seen[scope]; ok {
				continue
			}
			seen[scope] = struct{}{}
			result = append(result, scope)
		}
	}
	sort.Strings(result)
	return result
}

func randomToken(size int) (string, error) {
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func (manager *Manager) pruneLocked(now time.Time) {
	for state, pending := range manager.pending {
		if !pending.ExpiresAt.After(now) {
			delete(manager.pending, state)
		}
	}
	for id, session := range manager.authorized {
		if !session.View.ExpiresAt.After(now) {
			delete(manager.authorized, id)
		}
	}
}
