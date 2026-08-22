package cloudflare

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/netpolicy"
)

const (
	defaultAPIBaseURL    = "https://api.cloudflare.com/client/v4"
	defaultOAuthTokenURL = "https://dash.cloudflare.com/oauth2/token"
	managedDNSComment    = "Managed by Hermes Fleet"
	defaultSyncPeriod    = 30 * time.Second
	connectorSyncPeriod  = 2 * time.Second
)

var canonicalTunnelID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Config struct {
	AccountID                    string                `json:"account_id"`
	ZoneID                       string                `json:"zone_id"`
	AdminAPIToken                string                `json:"admin_api_token"`
	InstancesAPIToken            string                `json:"instances_api_token"`
	AdminTunnelID                string                `json:"admin_tunnel_id"`
	InstancesTunnelID            string                `json:"instances_tunnel_id"`
	AdminTunnelToken             string                `json:"admin_tunnel_token,omitempty"`
	InstancesTunnelToken         string                `json:"instances_tunnel_token,omitempty"`
	AdminHostname                string                `json:"admin_hostname"`
	AdminAccessTeam              string                `json:"admin_access_team"`
	AdminAccessAudience          string                `json:"admin_access_audience"`
	InstancesAccessTeam          string                `json:"instances_access_team"`
	InstancesAccessAudience      string                `json:"instances_access_audience"`
	RouteAutomation              RouteAutomationConfig `json:"route_automation,omitempty"`
	OAuth                        OAuthCredentials      `json:"oauth,omitempty"`
	APIBaseURL                   string                `json:"-"`
	SyncPeriod                   time.Duration         `json:"-"`
	CredentialsDirectory         string                `json:"-"`
	AdminConnectorConfigPath     string                `json:"-"`
	InstancesConnectorConfigPath string                `json:"-"`
	AdminConnectorTokenPath      string                `json:"-"`
	InstancesConnectorTokenPath  string                `json:"-"`
	AdminConnectorHealthURL      string                `json:"-"`
	InstancesConnectorHealthURL  string                `json:"-"`
}

// OAuthCredentials contains the renewable Cloudflare authorization used by
// Fleet-managed route automation. The containing remote-access configuration
// is encrypted as one sealed record; these values are never returned by the
// configuration API.
type OAuthCredentials struct {
	ClientID     string    `json:"client_id,omitempty"`
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

// RouteAutomationConfig authorizes Fleet to reconcile only the instance
// dashboard routes inside an existing remotely-managed Cloudflare tunnel.
// The API token is persisted only inside Fleet's sealed configuration.
type RouteAutomationConfig struct {
	AccountID      string `json:"account_id,omitempty"`
	ZoneID         string `json:"zone_id,omitempty"`
	ZoneName       string `json:"zone_name,omitempty"`
	FleetNamespace string `json:"fleet_namespace,omitempty"`
	TunnelID       string `json:"tunnel_id,omitempty"`
	APIToken       string `json:"api_token,omitempty"`
}

type ConfigurationView struct {
	AdminTunnelID                   string `json:"admin_tunnel_id"`
	InstancesTunnelID               string `json:"instances_tunnel_id"`
	AdminHostname                   string `json:"admin_hostname"`
	AdminCredentialAvailable        bool   `json:"admin_credential_available"`
	InstancesCredentialAvailable    bool   `json:"instances_credential_available"`
	AdminTunnelTokenConfigured      bool   `json:"admin_tunnel_token_configured"`
	InstancesTunnelTokenConfigured  bool   `json:"instances_tunnel_token_configured"`
	AdminTunnelTokenFingerprint     string `json:"admin_tunnel_token_fingerprint,omitempty"`
	InstancesTunnelTokenFingerprint string `json:"instances_tunnel_token_fingerprint,omitempty"`
	LegacyProviderManaged           bool   `json:"legacy_provider_managed"`
	RouteAutomationConfigured       bool   `json:"route_automation_configured"`
	RouteAutomationAccountID        string `json:"route_automation_account_id,omitempty"`
	RouteAutomationZoneID           string `json:"route_automation_zone_id,omitempty"`
	RouteAutomationZoneName         string `json:"route_automation_zone_name,omitempty"`
	RouteAutomationFleetNamespace   string `json:"route_automation_fleet_namespace,omitempty"`
	RouteAutomationTunnelID         string `json:"route_automation_tunnel_id,omitempty"`
	RouteAutomationTokenFingerprint string `json:"route_automation_token_fingerprint,omitempty"`
	OAuthConnected                  bool   `json:"oauth_connected"`
	OAuthScope                      string `json:"oauth_scope,omitempty"`
}

type RouteObservation struct {
	Hostname          string `json:"hostname"`
	OriginService     string `json:"origin_service"`
	ProviderState     string `json:"provider_state"`
	ProviderDetail    string `json:"provider_detail,omitempty"`
	ProviderCheckedAt string `json:"provider_checked_at,omitempty"`
	Revalidating      bool   `json:"revalidating,omitempty"`
	DNSState          string `json:"dns_state"`
	DNSDetail         string `json:"dns_detail,omitempty"`
	DNSCheckedAt      string `json:"dns_checked_at,omitempty"`
	IngressState      string `json:"ingress_state"`
	IngressDetail     string `json:"ingress_detail,omitempty"`
	IngressCheckedAt  string `json:"ingress_checked_at,omitempty"`
	EndpointState     string `json:"endpoint_state"`
	EndpointDetail    string `json:"endpoint_detail,omitempty"`
	EndpointCheckedAt string `json:"endpoint_checked_at,omitempty"`
}

type InstanceSource func(context.Context) ([]domain.Instance, error)

type ResourceOwnership interface {
	ListRemoteAccessResources(context.Context) ([]domain.RemoteAccessResource, error)
	PutRemoteAccessResource(context.Context, domain.RemoteAccessResource) error
	DeleteRemoteAccessResource(context.Context, string, string, string) error
}

type BoundaryStatus struct {
	TunnelID           string `json:"tunnel_id,omitempty"`
	Hostname           string `json:"hostname,omitempty"`
	Routes             int    `json:"routes"`
	Synced             bool   `json:"synced"`
	ConnectorState     string `json:"connector_state,omitempty"`
	ConnectorCheckedAt string `json:"connector_checked_at,omitempty"`
	ConnectorError     string `json:"connector_error,omitempty"`
	EndpointState      string `json:"endpoint_state,omitempty"`
	EndpointDetail     string `json:"endpoint_detail,omitempty"`
	EndpointCheckedAt  string `json:"endpoint_checked_at,omitempty"`
}

type Status struct {
	Configured bool           `json:"configured"`
	State      string         `json:"state"`
	Admin      BoundaryStatus `json:"admin"`
	Instances  BoundaryStatus `json:"instances"`
	LastSyncAt string         `json:"last_sync_at,omitempty"`
	LastError  string         `json:"last_error,omitempty"`
}

type Manager struct {
	config            Config
	source            InstanceSource
	ownership         ResourceOwnership
	client            *http.Client
	endpointClient    *http.Client
	endpointClientFor func(context.Context, string) (*http.Client, error)
	logger            *slog.Logger
	reconcile         sync.Mutex
	lifecycle         sync.Mutex
	oauthRefresh      sync.Mutex
	mu                sync.RWMutex
	status            Status
	routes            map[string]struct{}
	observations      map[string]RouteObservation
	trigger           chan struct{}
	persistConfig     func(context.Context, Config) error
}

func New(config Config, source InstanceSource, client *http.Client, logger *slog.Logger) (*Manager, error) {
	config = normalizeConfig(config)
	configured, err := validateConfig(config)
	if err != nil {
		return nil, err
	}
	if configured && source == nil {
		return nil, errors.New("Cloudflare remote access requires an instance source")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if logger == nil {
		logger = slog.Default()
	}
	manager := &Manager{
		config: config, source: source, client: client, logger: logger,
		routes: make(map[string]struct{}), observations: make(map[string]RouteObservation), trigger: make(chan struct{}, 1),
	}
	if _, standardTransport := client.Transport.(*http.Transport); client.Transport == nil || standardTransport {
		manager.endpointClientFor = newPublicEndpointClientFactory(client)
	} else {
		// Explicitly injected transports are test/caller-owned and cannot be
		// cloned into a DNS-pinned transport. Production uses http.Transport.
		manager.endpointClient = client
	}
	manager.status = Status{
		Configured: configured,
		State:      map[bool]string{true: "pending", false: "disabled"}[configured],
		Admin:      BoundaryStatus{TunnelID: config.AdminTunnelID, Hostname: config.AdminHostname},
		Instances:  BoundaryStatus{TunnelID: config.InstancesTunnelID},
	}
	return manager, nil
}

func newPublicEndpointClientFactory(base *http.Client) func(context.Context, string) (*http.Client, error) {
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			protocol := "udp"
			if strings.HasPrefix(network, "tcp") {
				protocol = "tcp"
			}
			dialer := &net.Dialer{Timeout: 3 * time.Second}
			return dialer.DialContext(ctx, protocol, "1.1.1.1:53")
		},
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, hostname string) (*http.Client, error) {
		_, client, err := netpolicy.NewPinnedHTTPSClient(ctx, "https://"+hostname, resolver, dialer, base)
		return client, err
	}
}

func normalizeConfig(config Config) Config {
	config.AccountID = strings.TrimSpace(config.AccountID)
	config.ZoneID = strings.TrimSpace(config.ZoneID)
	config.AdminAPIToken = strings.TrimSpace(config.AdminAPIToken)
	config.InstancesAPIToken = strings.TrimSpace(config.InstancesAPIToken)
	config.AdminTunnelID = strings.ToLower(strings.TrimSpace(config.AdminTunnelID))
	config.InstancesTunnelID = strings.ToLower(strings.TrimSpace(config.InstancesTunnelID))
	config.AdminTunnelToken = strings.TrimSpace(config.AdminTunnelToken)
	config.InstancesTunnelToken = strings.TrimSpace(config.InstancesTunnelToken)
	if tunnelID, err := TunnelIDFromToken(config.InstancesTunnelToken); err == nil {
		config.InstancesTunnelID = tunnelID
		config.RouteAutomation.TunnelID = tunnelID
	}
	config.AdminHostname = normalizeHostname(config.AdminHostname)
	config.AdminAccessTeam = strings.TrimSpace(config.AdminAccessTeam)
	config.AdminAccessAudience = strings.TrimSpace(config.AdminAccessAudience)
	config.InstancesAccessTeam = strings.TrimSpace(config.InstancesAccessTeam)
	config.InstancesAccessAudience = strings.TrimSpace(config.InstancesAccessAudience)
	config.RouteAutomation.AccountID = strings.TrimSpace(config.RouteAutomation.AccountID)
	config.RouteAutomation.ZoneID = strings.TrimSpace(config.RouteAutomation.ZoneID)
	config.RouteAutomation.ZoneName = normalizeHostname(config.RouteAutomation.ZoneName)
	config.RouteAutomation.FleetNamespace = strings.ToLower(strings.TrimSpace(config.RouteAutomation.FleetNamespace))
	config.RouteAutomation.TunnelID = strings.ToLower(strings.TrimSpace(config.RouteAutomation.TunnelID))
	config.RouteAutomation.APIToken = strings.TrimSpace(config.RouteAutomation.APIToken)
	config.OAuth.ClientID = strings.TrimSpace(config.OAuth.ClientID)
	config.OAuth.AccessToken = strings.TrimSpace(config.OAuth.AccessToken)
	config.OAuth.RefreshToken = strings.TrimSpace(config.OAuth.RefreshToken)
	config.OAuth.Scope = strings.TrimSpace(config.OAuth.Scope)
	config.APIBaseURL = strings.TrimRight(strings.TrimSpace(config.APIBaseURL), "/")
	config.CredentialsDirectory = strings.TrimSpace(config.CredentialsDirectory)
	config.AdminConnectorConfigPath = strings.TrimSpace(config.AdminConnectorConfigPath)
	config.InstancesConnectorConfigPath = strings.TrimSpace(config.InstancesConnectorConfigPath)
	config.AdminConnectorTokenPath = strings.TrimSpace(config.AdminConnectorTokenPath)
	config.InstancesConnectorTokenPath = strings.TrimSpace(config.InstancesConnectorTokenPath)
	config.AdminConnectorHealthURL = strings.TrimSpace(config.AdminConnectorHealthURL)
	config.InstancesConnectorHealthURL = strings.TrimSpace(config.InstancesConnectorHealthURL)
	if config.APIBaseURL == "" {
		config.APIBaseURL = defaultAPIBaseURL
	}
	if config.SyncPeriod <= 0 {
		config.SyncPeriod = defaultSyncPeriod
	}
	return config
}

func validateConfig(config Config) (bool, error) {
	oauthValues := []string{config.OAuth.ClientID, config.OAuth.AccessToken, config.OAuth.RefreshToken}
	oauthPopulated := 0
	for _, value := range oauthValues {
		if value != "" {
			oauthPopulated++
		}
	}
	if oauthPopulated != 0 && oauthPopulated != len(oauthValues) {
		return false, errors.New("Cloudflare OAuth requires the client, access, and refresh token")
	}
	automationValues := []string{
		config.RouteAutomation.AccountID,
		config.RouteAutomation.ZoneID,
		config.RouteAutomation.APIToken,
	}
	automationPopulated := 0
	for _, value := range automationValues {
		if value != "" {
			automationPopulated++
		}
	}
	if automationPopulated != 0 && automationPopulated != len(automationValues) {
		return false, errors.New("Cloudflare route automation requires the account ID, zone ID, tunnel ID, and API token")
	}
	tokenValues := []string{config.AdminTunnelToken, config.InstancesTunnelToken}
	tokensPopulated := 0
	for _, value := range tokenValues {
		if value != "" {
			tokensPopulated++
		}
	}
	boundary := []string{
		config.AdminTunnelID, config.InstancesTunnelID, config.AdminHostname,
	}
	populated := 0
	for _, value := range boundary {
		if value != "" {
			populated++
		}
	}
	legacy := []string{
		config.AccountID, config.ZoneID, config.AdminAPIToken, config.InstancesAPIToken,
		config.AdminAccessTeam, config.AdminAccessAudience,
		config.InstancesAccessTeam, config.InstancesAccessAudience,
	}
	legacyPopulated := 0
	for _, value := range legacy {
		if value != "" {
			legacyPopulated++
		}
	}
	if populated == 0 && legacyPopulated == 0 && tokensPopulated == 0 && automationPopulated == 0 {
		return false, nil
	}
	if tokensPopulated > 0 {
		if config.AdminTunnelID != "" || legacyPopulated != 0 {
			return false, errors.New("Cloudflare tunnel tokens cannot be combined with legacy tunnel ID or provider API fields")
		}
		if (config.AdminTunnelToken == "") != (config.AdminHostname == "") {
			return false, errors.New("Cloudflare admin remote access must define both the tunnel token and hostname")
		}
		if config.AdminTunnelToken != "" && !validTunnelToken(config.AdminTunnelToken) {
			return false, errors.New("Cloudflare admin tunnel token is invalid")
		}
		if config.InstancesTunnelToken != "" && !validTunnelToken(config.InstancesTunnelToken) {
			return false, errors.New("Cloudflare instance tunnel token is invalid")
		}
		if config.AdminTunnelToken != "" && config.InstancesTunnelToken != "" && subtle.ConstantTimeCompare([]byte(config.AdminTunnelToken), []byte(config.InstancesTunnelToken)) == 1 {
			return false, errors.New("Cloudflare admin and instance tunnel tokens must be different")
		}
		if err := validateHostnameBoundary(config); err != nil {
			return false, err
		}
		if automationPopulated > 0 && !instancesTokenBoundaryConfigured(config) {
			return false, errors.New("Cloudflare route automation requires the shared instance dashboard tunnel")
		}
		if config.InstancesTunnelToken != "" {
			tunnelID, err := TunnelIDFromToken(config.InstancesTunnelToken)
			if err != nil {
				return false, fmt.Errorf("Cloudflare instance tunnel token: %w", err)
			}
			if config.InstancesTunnelID != tunnelID || config.RouteAutomation.TunnelID != tunnelID {
				return false, errors.New("Cloudflare instance publishing tunnel identity is inconsistent")
			}
		}
		return true, nil
	}
	if automationPopulated > 0 {
		return false, errors.New("Cloudflare route automation is supported only with tunnel-token remote access")
	}
	if populated != len(boundary) {
		return false, errors.New("Cloudflare remote access must define both tunnel IDs and the admin hostname")
	}
	if legacyPopulated != 0 && legacyPopulated != len(legacy) {
		return false, errors.New("legacy Cloudflare provider-managed configuration is incomplete")
	}
	if config.AdminTunnelID == config.InstancesTunnelID {
		return false, errors.New("Cloudflare admin and instance tunnel IDs must be different")
	}
	if !validTunnelID(config.AdminTunnelID) || !validTunnelID(config.InstancesTunnelID) {
		return false, errors.New("Cloudflare tunnel IDs must be canonical UUIDs, not connector tokens")
	}
	if legacyPopulated > 0 && config.AdminAPIToken == config.InstancesAPIToken {
		return false, errors.New("Cloudflare admin and instance API tokens must be different")
	}
	if err := validateHostnameBoundary(config); err != nil {
		return false, err
	}
	return true, nil
}

func validateHostnameBoundary(config Config) error {
	if config.AdminHostname != "" && !validHostname(config.AdminHostname) {
		return errors.New("Cloudflare admin hostname must be a valid lowercase DNS name")
	}
	return nil
}

func directTokenManaged(config Config) bool {
	return config.AdminTunnelToken != "" || config.InstancesTunnelToken != ""
}

func adminTokenBoundaryConfigured(config Config) bool {
	return config.AdminTunnelToken != "" && config.AdminHostname != ""
}

func instancesTokenBoundaryConfigured(config Config) bool {
	return config.InstancesTunnelToken != ""
}

func validTunnelToken(value string) bool {
	if len(value) < 32 || len(value) > 8<<10 {
		return false
	}
	for _, character := range value {
		if character <= ' ' || character == 0x7f {
			return false
		}
	}
	return true
}

func TunnelIDFromToken(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !validTunnelToken(value) {
		return "", errors.New("token format is invalid")
	}
	var decoded []byte
	var decodeErr error
	for _, decoder := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
		decoded, decodeErr = decoder.DecodeString(value)
		if decodeErr == nil {
			break
		}
	}
	if decodeErr != nil || len(decoded) == 0 || len(decoded) > 16<<10 {
		return "", errors.New("token payload is not valid base64")
	}
	var payload struct {
		TunnelID string `json:"t"`
	}
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return "", errors.New("token payload is not valid JSON")
	}
	tunnelID := strings.ToLower(strings.TrimSpace(payload.TunnelID))
	if !validTunnelID(tunnelID) {
		return "", errors.New("token does not contain a canonical tunnel ID")
	}
	return tunnelID, nil
}

func (manager *Manager) SetOwnershipStore(ownership ResourceOwnership) {
	manager.mu.Lock()
	manager.ownership = ownership
	manager.mu.Unlock()
}

// SetConfigPersister installs the encrypted persistence boundary used after an
// OAuth refresh token rotation. The callback must not expose the configuration
// through an API response.
func (manager *Manager) SetConfigPersister(persist func(context.Context, Config) error) {
	manager.mu.Lock()
	manager.persistConfig = persist
	manager.mu.Unlock()
}

func legacyProviderManaged(config Config) bool {
	return config.AccountID != "" && config.ZoneID != "" &&
		config.AdminAPIToken != "" && config.InstancesAPIToken != "" &&
		config.AdminAccessTeam != "" && config.AdminAccessAudience != "" &&
		config.InstancesAccessTeam != "" && config.InstancesAccessAudience != ""
}

func validTunnelID(value string) bool {
	return canonicalTunnelID.MatchString(value)
}

func normalizeHostname(value string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
}

// NormalizePublicHostname validates an explicit per-instance public hostname.
// An empty value disables publishing for that instance.
func NormalizePublicHostname(value string) (string, error) {
	hostname := normalizeHostname(value)
	if hostname == "" {
		return "", nil
	}
	if !validHostname(hostname) {
		return "", errors.New("public hostname must be a valid lowercase DNS name")
	}
	return hostname, nil
}

// NormalizeFleetNamespace validates the stable DNS label that distinguishes one
// Fleet installation from other installations publishing into the same zone.
func NormalizeFleetNamespace(value string) (string, error) {
	namespace := strings.ToLower(strings.TrimSpace(value))
	if namespace == "" {
		return "", errors.New("Fleet namespace is required")
	}
	if len(namespace) > 32 || !validDNSLabel(namespace) {
		return "", errors.New("Fleet namespace must be 1-32 lowercase letters, numbers, or hyphens, and cannot start or end with a hyphen")
	}
	return namespace, nil
}

// BuildInstancePublicHostname returns the server-authoritative hostname for a
// Fleet-owned instance publication.
func BuildInstancePublicHostname(namespace, instanceName, zoneName string) (string, error) {
	normalizedNamespace, err := NormalizeFleetNamespace(namespace)
	if err != nil {
		return "", err
	}
	instanceLabel := strings.ToLower(strings.TrimSpace(instanceName))
	if !validDNSLabel(instanceLabel) {
		return "", errors.New("instance name cannot be used in a public hostname")
	}
	label := normalizedNamespace + "-" + instanceLabel
	if len(label) > 63 {
		return "", errors.New("Fleet namespace and instance name exceed the 63-character DNS label limit")
	}
	zone := normalizeHostname(zoneName)
	if !validHostname(zone) {
		return "", errors.New("verified Cloudflare zone is not a valid DNS name")
	}
	return NormalizePublicHostname(label + "." + zone)
}

func validDNSLabel(value string) bool {
	if value == "" || len(value) > 63 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

func validHostname(value string) bool {
	if value == "" || len(value) > 253 || strings.Contains(value, "*") || !strings.Contains(value, ".") ||
		net.ParseIP(value) != nil || value == "localhost" || strings.HasSuffix(value, ".localhost") || strings.HasSuffix(value, ".local") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	return true
}

func (manager *Manager) Start(ctx context.Context) {
	manager.mu.RLock()
	syncPeriod := manager.config.SyncPeriod
	manager.mu.RUnlock()
	if syncPeriod <= 0 {
		syncPeriod = defaultSyncPeriod
	}
	go func() {
		ticker := time.NewTicker(syncPeriod)
		defer ticker.Stop()
		connectorTicker := time.NewTicker(connectorSyncPeriod)
		defer connectorTicker.Stop()
		manager.runReconcile(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				manager.runReconcile(ctx)
			case <-connectorTicker.C:
				if manager.Status().State == "syncing" {
					manager.runReconcile(ctx)
				}
			case <-manager.trigger:
				manager.runReconcile(ctx)
			}
		}
	}()
}

func (manager *Manager) Trigger() {
	status := manager.Status()
	if !status.Configured || status.State == "cleanup_pending" {
		return
	}
	select {
	case manager.trigger <- struct{}{}:
	default:
	}
}

// Configure activates one or both pre-provisioned Cloudflare tunnels. New
// configurations use tunnel-scoped connector tokens. Provider API and local
// credential fields remain supported only for encrypted configurations written
// by older Fleet releases.
func (manager *Manager) Configure(ctx context.Context, config Config) error {
	manager.lifecycle.Lock()
	defer manager.lifecycle.Unlock()
	manager.mu.RLock()
	previousConfig := manager.config
	previousStatus := manager.status
	manager.mu.RUnlock()
	if config.APIBaseURL == "" {
		config.APIBaseURL = previousConfig.APIBaseURL
	}
	if config.CredentialsDirectory == "" {
		config.CredentialsDirectory = previousConfig.CredentialsDirectory
	}
	if config.AdminConnectorConfigPath == "" {
		config.AdminConnectorConfigPath = previousConfig.AdminConnectorConfigPath
	}
	if config.InstancesConnectorConfigPath == "" {
		config.InstancesConnectorConfigPath = previousConfig.InstancesConnectorConfigPath
	}
	if config.AdminConnectorTokenPath == "" {
		config.AdminConnectorTokenPath = previousConfig.AdminConnectorTokenPath
	}
	if config.InstancesConnectorTokenPath == "" {
		config.InstancesConnectorTokenPath = previousConfig.InstancesConnectorTokenPath
	}
	if config.AdminConnectorHealthURL == "" {
		config.AdminConnectorHealthURL = previousConfig.AdminConnectorHealthURL
	}
	if config.InstancesConnectorHealthURL == "" {
		config.InstancesConnectorHealthURL = previousConfig.InstancesConnectorHealthURL
	}
	config = normalizeConfig(config)
	configured, err := validateConfig(config)
	if err != nil {
		return err
	}
	if !configured {
		return errors.New("Cloudflare remote access configuration is empty")
	}
	if previousStatus.Configured {
		if previousStatus.State == "cleanup_pending" {
			return errors.New("Cloudflare remote cleanup is incomplete; retry Disable before applying configuration")
		}
		if managedConfigurationKind(previousConfig) != managedConfigurationKind(config) {
			return errors.New("disable Cloudflare remote access before changing the Cloudflare credential mode")
		}
		if !sameManagedBoundary(previousConfig, config) {
			return errors.New("disable Cloudflare remote access before changing tunnel or hostname boundaries")
		}
	}
	if directTokenManaged(config) {
		return manager.configureTokens(ctx, config)
	}
	if !legacyProviderManaged(config) {
		return manager.configureLocal(ctx, config)
	}
	if config.AdminConnectorTokenPath == "" || config.InstancesConnectorTokenPath == "" {
		return errors.New("legacy Cloudflare connector runtime paths are unavailable")
	}

	adminToken, err := manager.fetchConnectorToken(ctx, config, config.AdminAPIToken, config.AdminTunnelID)
	if err != nil {
		return fmt.Errorf("verify admin tunnel: %w", err)
	}
	instancesToken, err := manager.fetchConnectorToken(ctx, config, config.InstancesAPIToken, config.InstancesTunnelID)
	if err != nil {
		return fmt.Errorf("verify instance tunnel: %w", err)
	}
	if adminToken == instancesToken {
		return errors.New("Cloudflare returned the same connector token for both tunnel boundaries")
	}
	if err := manager.verifyRemoteTunnel(ctx, config, config.AdminAPIToken, config.AdminTunnelID); err != nil {
		return fmt.Errorf("verify admin tunnel configuration: %w", err)
	}
	if err := manager.verifyRemoteTunnel(ctx, config, config.InstancesAPIToken, config.InstancesTunnelID); err != nil {
		return fmt.Errorf("verify instance tunnel configuration: %w", err)
	}

	manager.reconcile.Lock()
	defer manager.reconcile.Unlock()
	routes, err := manager.reconcileConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("synchronize Cloudflare remote access: %w", err)
	}
	previousAdminToken, previousAdminErr := os.ReadFile(config.AdminConnectorTokenPath)
	if err := writeConnectorToken(config.AdminConnectorTokenPath, adminToken); err != nil {
		return fmt.Errorf("store admin connector token: %w", err)
	}
	if err := writeConnectorToken(config.InstancesConnectorTokenPath, instancesToken); err != nil {
		if previousAdminErr == nil {
			_ = writeConnectorToken(config.AdminConnectorTokenPath, strings.TrimSpace(string(previousAdminToken)))
		} else {
			_ = os.Remove(config.AdminConnectorTokenPath)
		}
		return fmt.Errorf("store instance connector token: %w", err)
	}

	manager.mu.Lock()
	manager.config = config
	manager.routes = make(map[string]struct{}, len(routes))
	for hostname := range routes {
		manager.routes[hostname] = struct{}{}
	}
	manager.status = Status{
		Configured: true,
		State:      "synced",
		Admin:      BoundaryStatus{TunnelID: config.AdminTunnelID, Hostname: config.AdminHostname, Routes: 1, Synced: true},
		Instances:  BoundaryStatus{TunnelID: config.InstancesTunnelID, Routes: len(routes), Synced: true},
		LastSyncAt: time.Now().UTC().Format(time.RFC3339),
	}
	manager.mu.Unlock()
	manager.refreshConnectorHealth(ctx, config)
	manager.refreshAdminEndpoint(ctx, config)
	return nil
}

func tokenConfigurationState(adminConfigured, instancesConfigured bool) string {
	if adminConfigured && instancesConfigured {
		return "synced"
	}
	return "pending"
}

func sameManagedBoundary(current, next Config) bool {
	if directTokenManaged(current) && directTokenManaged(next) {
		return true
	}
	if current.AdminHostname != next.AdminHostname {
		return false
	}
	return current.AdminTunnelID == next.AdminTunnelID &&
		current.InstancesTunnelID == next.InstancesTunnelID
}

func managedConfigurationKind(config Config) string {
	if directTokenManaged(config) {
		return "connector_token"
	}
	if legacyProviderManaged(config) {
		return "legacy_provider"
	}
	return "legacy_local"
}

func (manager *Manager) configureTokens(ctx context.Context, config Config) error {
	adminConfigured := adminTokenBoundaryConfigured(config)
	instancesConfigured := instancesTokenBoundaryConfigured(config)
	if adminConfigured && config.AdminConnectorTokenPath == "" {
		return errors.New("Cloudflare admin connector token runtime path is unavailable")
	}
	if instancesConfigured && config.InstancesConnectorTokenPath == "" {
		return errors.New("Cloudflare instance connector token runtime path is unavailable")
	}

	manager.reconcile.Lock()
	defer manager.reconcile.Unlock()
	routes := make(map[string]string)
	adminRoutes := 0
	if adminConfigured {
		adminRoutes = 1
	}
	if instancesConfigured {
		var err error
		routes, err = manager.instanceRoutes(ctx, config)
		if err != nil {
			return err
		}
	}
	if err := publishConnectorTokens(config); err != nil {
		return err
	}

	manager.mu.Lock()
	manager.config = config
	manager.routes = make(map[string]struct{})
	manager.observations = pendingRouteObservations(routes, routeAutomationConfigured(config))
	manager.status = Status{
		Configured: true,
		State:      tokenConfigurationState(adminConfigured, instancesConfigured),
		Admin:      BoundaryStatus{Hostname: config.AdminHostname, Routes: adminRoutes, Synced: adminConfigured},
		Instances:  BoundaryStatus{TunnelID: config.InstancesTunnelID, Routes: len(routes), Synced: instancesConfigured},
		LastSyncAt: time.Now().UTC().Format(time.RFC3339),
	}
	manager.mu.Unlock()
	manager.refreshConnectorHealth(ctx, config)
	manager.refreshAdminEndpoint(ctx, config)
	return nil
}

func publishConnectorTokens(config Config) error {
	adminConfigured := adminTokenBoundaryConfigured(config)
	instancesConfigured := instancesTokenBoundaryConfigured(config)
	var previousAdminToken []byte
	var previousAdminErr error
	if adminConfigured {
		previousAdminToken, previousAdminErr = os.ReadFile(config.AdminConnectorTokenPath)
		if err := ensureConnectorToken(config.AdminConnectorTokenPath, config.AdminTunnelToken); err != nil {
			return fmt.Errorf("store admin connector token: %w", err)
		}
	}
	if instancesConfigured {
		if err := ensureConnectorToken(config.InstancesConnectorTokenPath, config.InstancesTunnelToken); err != nil {
			if adminConfigured {
				if previousAdminErr == nil {
					_ = writeConnectorToken(config.AdminConnectorTokenPath, strings.TrimSpace(string(previousAdminToken)))
				} else {
					_ = os.Remove(config.AdminConnectorTokenPath)
				}
			}
			return fmt.Errorf("store instance connector token: %w", err)
		}
	}
	paths := make([]string, 0, 2)
	if adminConfigured {
		paths = append(paths, config.AdminConnectorConfigPath)
	}
	if instancesConfigured {
		paths = append(paths, config.InstancesConnectorConfigPath)
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove deprecated connector configuration: %w", err)
		}
	}
	return nil
}

func ensureConnectorToken(path, token string) error {
	info, statErr := os.Lstat(path)
	if statErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return errors.New("connector token path must be a regular file, not a symlink")
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	current, err := os.ReadFile(path)
	if err == nil && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(current))), []byte(token)) == 1 {
		if info.Mode().Perm() != 0o600 {
			return os.Chmod(path, 0o600)
		}
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeConnectorToken(path, token)
}

type tunnelCredential struct {
	AccountTag   string `json:"AccountTag"`
	TunnelSecret string `json:"TunnelSecret"`
	TunnelID     string `json:"TunnelID"`
}

func (manager *Manager) configureLocal(ctx context.Context, config Config) error {
	if config.CredentialsDirectory == "" {
		return errors.New("Cloudflare tunnel credential directory is unavailable")
	}
	if config.AdminConnectorConfigPath == "" || config.InstancesConnectorConfigPath == "" {
		return errors.New("Cloudflare connector configuration paths are unavailable")
	}
	if err := validateTunnelCredential(config.CredentialsDirectory, config.AdminTunnelID); err != nil {
		return fmt.Errorf("admin tunnel credential: %w", err)
	}
	if err := validateTunnelCredential(config.CredentialsDirectory, config.InstancesTunnelID); err != nil {
		return fmt.Errorf("instance tunnel credential: %w", err)
	}

	manager.reconcile.Lock()
	defer manager.reconcile.Unlock()
	routes, err := manager.writeLocalIngress(ctx, config)
	if err != nil {
		return fmt.Errorf("write Cloudflare tunnel ingress: %w", err)
	}
	for _, path := range []string{config.AdminConnectorTokenPath, config.InstancesConnectorTokenPath} {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			manager.logger.Warn("remove deprecated Cloudflare connector token", "path", path, "error", err)
		}
	}

	manager.mu.Lock()
	manager.config = config
	manager.routes = make(map[string]struct{}, len(routes))
	for hostname := range routes {
		manager.routes[hostname] = struct{}{}
	}
	manager.status = Status{
		Configured: true,
		State:      "synced",
		Admin:      BoundaryStatus{TunnelID: config.AdminTunnelID, Hostname: config.AdminHostname, Routes: 1, Synced: true},
		Instances:  BoundaryStatus{TunnelID: config.InstancesTunnelID, Routes: len(routes), Synced: true},
		LastSyncAt: time.Now().UTC().Format(time.RFC3339),
	}
	manager.mu.Unlock()
	manager.refreshConnectorHealth(ctx, config)
	return nil
}

func validateTunnelCredential(directory, tunnelID string) error {
	path := filepath.Join(directory, tunnelID+".json")
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s.json is not installed in the Fleet credential directory", tunnelID)
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("credential path must be a regular file, not a symlink")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data) > 64<<10 {
		return errors.New("credential file has an invalid size")
	}
	var credential tunnelCredential
	if err := json.Unmarshal(data, &credential); err != nil {
		return errors.New("credential file is not valid Cloudflare tunnel JSON")
	}
	if strings.TrimSpace(credential.AccountTag) == "" || strings.TrimSpace(credential.TunnelSecret) == "" {
		return errors.New("credential file is missing Cloudflare tunnel identity fields")
	}
	if !strings.EqualFold(strings.TrimSpace(credential.TunnelID), tunnelID) {
		return errors.New("credential file belongs to a different tunnel")
	}
	return nil
}

func (manager *Manager) instanceRoutes(ctx context.Context, config Config) (map[string]string, error) {
	instances, err := manager.source(ctx)
	if err != nil {
		return nil, fmt.Errorf("list managed instances: %w", err)
	}
	routes := make(map[string]string)
	for _, instance := range instances {
		if instance.Status == domain.InstanceDeleted || instance.Status == domain.InstanceDeleting {
			continue
		}
		hostname, err := NormalizePublicHostname(instance.PublicHostname)
		if err != nil {
			return nil, fmt.Errorf("instance %s: %w", instance.Name, err)
		}
		if hostname == "" {
			continue
		}
		routes[hostname] = "http://hermes-fleet-instance-" + instance.Name + "-dashboard:9119"
	}
	return routes, nil
}

func (manager *Manager) writeLocalIngress(ctx context.Context, config Config) (map[string]string, error) {
	routes, err := manager.instanceRoutes(ctx, config)
	if err != nil {
		return nil, err
	}
	admin := map[string]string{config.AdminHostname: "http://control-plane:9180"}
	adminData := renderLocalTunnelConfig(config.CredentialsDirectory, config.AdminTunnelID, admin)
	instancesData := renderLocalTunnelConfig(config.CredentialsDirectory, config.InstancesTunnelID, routes)
	previousAdmin, previousAdminErr := os.ReadFile(config.AdminConnectorConfigPath)
	if err := writeConnectorRuntime(config.AdminConnectorConfigPath, adminData); err != nil {
		return nil, fmt.Errorf("admin connector: %w", err)
	}
	if err := writeConnectorRuntime(config.InstancesConnectorConfigPath, instancesData); err != nil {
		if previousAdminErr == nil {
			_ = writeConnectorRuntime(config.AdminConnectorConfigPath, previousAdmin)
		} else {
			_ = os.Remove(config.AdminConnectorConfigPath)
		}
		return nil, fmt.Errorf("instance connector: %w", err)
	}
	return routes, nil
}

func renderLocalTunnelConfig(credentialsDirectory, tunnelID string, routes map[string]string) []byte {
	var builder strings.Builder
	builder.WriteString("tunnel: ")
	builder.WriteString(strconv.Quote(tunnelID))
	builder.WriteString("\ncredentials-file: ")
	builder.WriteString(strconv.Quote(filepath.Join(credentialsDirectory, tunnelID+".json")))
	builder.WriteString("\ningress:\n")
	hostnames := make([]string, 0, len(routes))
	for hostname := range routes {
		hostnames = append(hostnames, hostname)
	}
	sort.Strings(hostnames)
	for _, hostname := range hostnames {
		builder.WriteString("  - hostname: ")
		builder.WriteString(strconv.Quote(hostname))
		builder.WriteString("\n    service: ")
		builder.WriteString(strconv.Quote(routes[hostname]))
		builder.WriteByte('\n')
	}
	builder.WriteString("  - service: http_status:404\n")
	return []byte(builder.String())
}

func writeConnectorRuntime(path string, data []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".connector-config-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func (manager *Manager) verifyRemoteTunnel(ctx context.Context, config Config, apiToken, tunnelID string) error {
	var current tunnelConfiguration
	path := fmt.Sprintf("/accounts/%s/cfd_tunnel/%s/configurations", url.PathEscape(config.AccountID), url.PathEscape(tunnelID))
	if err := manager.apiWithConfig(ctx, config, apiToken, http.MethodGet, path, nil, &current); err != nil {
		return err
	}
	if current.Source != "cloudflare" {
		return fmt.Errorf("tunnel is %q managed; Fleet only supports remotely managed tunnels", current.Source)
	}
	return nil
}

func (manager *Manager) fetchConnectorToken(ctx context.Context, config Config, apiToken, tunnelID string) (string, error) {
	var connectorToken string
	path := fmt.Sprintf("/accounts/%s/cfd_tunnel/%s/token", url.PathEscape(config.AccountID), url.PathEscape(tunnelID))
	if err := manager.apiWithConfig(ctx, config, apiToken, http.MethodGet, path, nil, &connectorToken); err != nil {
		return "", err
	}
	connectorToken = strings.TrimSpace(connectorToken)
	if connectorToken == "" {
		return "", errors.New("Cloudflare returned an empty connector token")
	}
	return connectorToken, nil
}

func writeConnectorToken(path, token string) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".connector-token-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(token + "\n"); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func (manager *Manager) runReconcile(ctx context.Context) {
	status := manager.Status()
	if !status.Configured || status.State == "cleanup_pending" {
		return
	}
	if err := manager.Reconcile(ctx); err != nil {
		manager.logger.Error("reconcile Cloudflare remote access", "error", err)
	}
}

// RecordConfigurationFailure keeps a persisted configuration visible and
// editable after a transient startup/API failure without claiming that routes
// or connectors are active.
func (manager *Manager) RecordConfigurationFailure(config Config, failure error) {
	manager.mu.Lock()
	base := manager.config
	config.APIBaseURL = base.APIBaseURL
	config.SyncPeriod = base.SyncPeriod
	config.CredentialsDirectory = base.CredentialsDirectory
	config.AdminConnectorConfigPath = base.AdminConnectorConfigPath
	config.InstancesConnectorConfigPath = base.InstancesConnectorConfigPath
	config.AdminConnectorTokenPath = base.AdminConnectorTokenPath
	config.InstancesConnectorTokenPath = base.InstancesConnectorTokenPath
	config.AdminConnectorHealthURL = base.AdminConnectorHealthURL
	config.InstancesConnectorHealthURL = base.InstancesConnectorHealthURL
	config = normalizeConfig(config)
	manager.config = config
	manager.routes = make(map[string]struct{})
	manager.observations = make(map[string]RouteObservation)
	manager.status = Status{
		Configured: true, State: "error", LastError: failure.Error(),
		Admin:     BoundaryStatus{TunnelID: config.AdminTunnelID, Hostname: config.AdminHostname},
		Instances: BoundaryStatus{TunnelID: config.InstancesTunnelID},
	}
	manager.mu.Unlock()
}

func (manager *Manager) Status() Status {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.status
}

func (manager *Manager) Configuration() ConfigurationView {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	config := manager.config
	return ConfigurationView{
		AdminTunnelID: config.AdminTunnelID, InstancesTunnelID: config.InstancesTunnelID,
		AdminHostname:                   config.AdminHostname,
		AdminCredentialAvailable:        tunnelCredentialAvailable(config.CredentialsDirectory, config.AdminTunnelID),
		InstancesCredentialAvailable:    tunnelCredentialAvailable(config.CredentialsDirectory, config.InstancesTunnelID),
		AdminTunnelTokenConfigured:      config.AdminTunnelToken != "",
		InstancesTunnelTokenConfigured:  config.InstancesTunnelToken != "",
		AdminTunnelTokenFingerprint:     tunnelTokenFingerprint(config.AdminTunnelToken),
		InstancesTunnelTokenFingerprint: tunnelTokenFingerprint(config.InstancesTunnelToken),
		LegacyProviderManaged:           legacyProviderManaged(config),
		RouteAutomationConfigured:       routeAutomationConfigured(config),
		RouteAutomationAccountID:        config.RouteAutomation.AccountID,
		RouteAutomationZoneID:           config.RouteAutomation.ZoneID,
		RouteAutomationZoneName:         config.RouteAutomation.ZoneName,
		RouteAutomationFleetNamespace:   config.RouteAutomation.FleetNamespace,
		RouteAutomationTunnelID:         config.RouteAutomation.TunnelID,
		RouteAutomationTokenFingerprint: tunnelTokenFingerprint(config.RouteAutomation.APIToken),
		OAuthConnected:                  config.OAuth.ClientID != "" && config.OAuth.RefreshToken != "",
		OAuthScope:                      config.OAuth.Scope,
	}
}

func (manager *Manager) RouteObservations() map[string]RouteObservation {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	result := make(map[string]RouteObservation, len(manager.observations))
	for hostname, observation := range manager.observations {
		result[hostname] = observation
	}
	return result
}

func tunnelTokenFingerprint(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%X", digest[:5])
}

func tunnelCredentialAvailable(directory, tunnelID string) bool {
	return directory != "" && tunnelID != "" && validateTunnelCredential(directory, tunnelID) == nil
}

// Config returns an internal copy used only for encrypted persistence and
// transactional rollback. Callers must never serialize it to an API response.
func (manager *Manager) Config() Config {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.config
}

func (manager *Manager) PublicDashboardURL(publicHostname string) string {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if (directTokenManaged(manager.config) && !instancesTokenBoundaryConfigured(manager.config)) || !manager.status.Instances.Synced {
		return ""
	}
	if state := manager.status.Instances.ConnectorState; state != "" && state != "running" {
		return ""
	}
	hostname, err := NormalizePublicHostname(publicHostname)
	if err != nil || hostname == "" {
		return ""
	}
	if _, ok := manager.routes[hostname]; !ok {
		return ""
	}
	return "https://" + hostname
}

func (manager *Manager) Reconcile(ctx context.Context) error {
	manager.reconcile.Lock()
	defer manager.reconcile.Unlock()
	manager.mu.RLock()
	config := manager.config
	status := manager.status
	manager.mu.RUnlock()
	if !status.Configured {
		return errors.New("Cloudflare remote access is not configured")
	}
	if status.State == "cleanup_pending" {
		return errors.New("Cloudflare remote cleanup is incomplete; retry Disable")
	}
	manager.setSyncing()

	var routes map[string]string
	var err error
	if directTokenManaged(config) {
		routes = make(map[string]string)
		if instancesTokenBoundaryConfigured(config) {
			routes, err = manager.instanceRoutes(ctx, config)
		}
		if err == nil {
			err = publishConnectorTokens(config)
		}
		if err == nil && routeAutomationConfigured(config) {
			routes, err = manager.reconcileAutomatedInstanceRoutes(ctx, config, routes)
		} else if err == nil {
			manager.mu.Lock()
			manager.observations = pendingRouteObservations(routes, false)
			manager.mu.Unlock()
			routes = make(map[string]string)
		}
	} else if legacyProviderManaged(config) {
		routes, err = manager.reconcileConfig(ctx, config)
	} else {
		if credentialErr := validateTunnelCredential(config.CredentialsDirectory, config.AdminTunnelID); credentialErr != nil {
			return manager.fail(fmt.Errorf("admin tunnel credential: %w", credentialErr))
		}
		if credentialErr := validateTunnelCredential(config.CredentialsDirectory, config.InstancesTunnelID); credentialErr != nil {
			return manager.fail(fmt.Errorf("instance tunnel credential: %w", credentialErr))
		}
		routes, err = manager.writeLocalIngress(ctx, config)
	}
	if err != nil {
		return manager.fail(err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	manager.mu.Lock()
	manager.routes = make(map[string]struct{}, len(routes))
	for hostname := range routes {
		manager.routes[hostname] = struct{}{}
	}
	if directTokenManaged(config) {
		manager.status.State = tokenConfigurationState(adminTokenBoundaryConfigured(config), instancesTokenBoundaryConfigured(config))
	} else {
		manager.status.State = "synced"
	}
	manager.status.LastError = ""
	manager.status.LastSyncAt = now
	manager.status.Admin.Synced = !directTokenManaged(config) || adminTokenBoundaryConfigured(config)
	if manager.status.Admin.Synced {
		manager.status.Admin.Routes = 1
	} else {
		manager.status.Admin.Routes = 0
	}
	manager.status.Instances.Synced = !directTokenManaged(config) || instancesTokenBoundaryConfigured(config)
	manager.status.Instances.Routes = len(routes)
	manager.mu.Unlock()
	manager.refreshConnectorHealth(ctx, config)
	manager.refreshAdminEndpoint(ctx, config)
	return nil
}

type connectorHealthResponse struct {
	State     string `json:"state"`
	Ready     bool   `json:"ready"`
	LastError string `json:"last_error"`
}

func (manager *Manager) refreshConnectorHealth(ctx context.Context, config Config) {
	checkAdmin := !directTokenManaged(config) || adminTokenBoundaryConfigured(config)
	checkInstances := !directTokenManaged(config) || instancesTokenBoundaryConfigured(config)
	var admin, instances connectorHealthResponse
	var adminChecked, instancesChecked bool
	var adminErr, instancesErr error
	if checkAdmin {
		admin, adminChecked, adminErr = manager.connectorHealth(ctx, config.AdminConnectorHealthURL)
	}
	if checkInstances {
		instances, instancesChecked, instancesErr = manager.connectorHealth(ctx, config.InstancesConnectorHealthURL)
	}
	if !adminChecked && !instancesChecked {
		return
	}
	checkedAt := time.Now().UTC().Format(time.RFC3339)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	apply := func(boundary *BoundaryStatus, result connectorHealthResponse, checked bool, err error) {
		if !checked {
			return
		}
		boundary.ConnectorCheckedAt = checkedAt
		boundary.ConnectorState = result.State
		boundary.ConnectorError = result.LastError
		if err != nil {
			boundary.ConnectorState = "unreachable"
			boundary.ConnectorError = err.Error()
		}
	}
	apply(&manager.status.Admin, admin, adminChecked, adminErr)
	apply(&manager.status.Instances, instances, instancesChecked, instancesErr)
	adminTransitional := adminChecked && adminErr == nil && (admin.State == "disabled" || admin.State == "starting")
	instancesTransitional := instancesChecked && instancesErr == nil && (instances.State == "disabled" || instances.State == "starting")
	if adminTransitional || instancesTransitional {
		manager.status.State = "syncing"
		manager.status.LastError = ""
		return
	}
	adminFailed := adminChecked && (adminErr != nil || !admin.Ready || admin.State != "running")
	instancesFailed := instancesChecked && (instancesErr != nil || !instances.Ready || instances.State != "running")
	if adminFailed || instancesFailed {
		manager.status.State = "degraded"
		failures := make([]string, 0, 2)
		if adminFailed {
			failures = append(failures, "admin connector is not ready")
		}
		if instancesFailed {
			failures = append(failures, "instance connector is not ready")
		}
		manager.status.LastError = strings.Join(failures, "; ")
		return
	}
	if directTokenManaged(config) && tokenConfigurationState(adminTokenBoundaryConfigured(config), instancesTokenBoundaryConfigured(config)) == "pending" {
		manager.status.State = "pending"
		manager.status.LastError = ""
	}
}

func (manager *Manager) connectorHealth(ctx context.Context, endpoint string) (connectorHealthResponse, bool, error) {
	if endpoint == "" {
		return connectorHealthResponse{}, false, nil
	}
	requestContext, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint, nil)
	if err != nil {
		return connectorHealthResponse{}, true, err
	}
	response, err := manager.client.Do(request)
	if err != nil {
		return connectorHealthResponse{}, true, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return connectorHealthResponse{}, true, err
	}
	var result connectorHealthResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return connectorHealthResponse{}, true, fmt.Errorf("decode connector health: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !result.Ready {
		if result.State == "" {
			return result, true, fmt.Errorf("connector health returned HTTP %d", response.StatusCode)
		}
		return result, true, nil
	}
	return result, true, nil
}

func (manager *Manager) refreshAdminEndpoint(ctx context.Context, config Config) {
	manager.mu.RLock()
	connectorRunning := manager.status.Admin.ConnectorState == "running"
	manager.mu.RUnlock()
	if !directTokenManaged(config) || !adminTokenBoundaryConfigured(config) || !connectorRunning || config.AdminHostname == "" {
		return
	}
	state, detail := manager.checkPublicEndpoint(ctx, config.AdminHostname)
	manager.mu.Lock()
	manager.status.Admin.EndpointState = state
	manager.status.Admin.EndpointDetail = detail
	manager.status.Admin.EndpointCheckedAt = time.Now().UTC().Format(time.RFC3339)
	manager.mu.Unlock()
}

func (manager *Manager) VerifyInstancePublishing(ctx context.Context, candidate Config) (string, string, error) {
	manager.mu.RLock()
	runtimeConfig := manager.config
	manager.mu.RUnlock()
	candidate.APIBaseURL = runtimeConfig.APIBaseURL
	candidate = normalizeConfig(candidate)
	if candidate.InstancesTunnelToken == "" || candidate.RouteAutomation.AccountID == "" ||
		candidate.RouteAutomation.ZoneID == "" || candidate.RouteAutomation.APIToken == "" {
		return "", "", errors.New("instance publishing requires the tunnel token, account ID, zone ID, and API token")
	}
	tunnelID, err := TunnelIDFromToken(candidate.InstancesTunnelToken)
	if err != nil {
		return "", "", err
	}
	apiConfig := candidate
	apiConfig.AccountID = candidate.RouteAutomation.AccountID
	apiConfig.ZoneID = candidate.RouteAutomation.ZoneID
	var zone struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Status  string `json:"status"`
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	if err := manager.apiWithConfig(ctx, apiConfig, candidate.RouteAutomation.APIToken, http.MethodGet,
		"/zones/"+url.PathEscape(candidate.RouteAutomation.ZoneID), nil, &zone); err != nil {
		return "", "", fmt.Errorf("verify Cloudflare zone: %w", err)
	}
	zoneName := normalizeHostname(zone.Name)
	if zone.ID == "" || zoneName == "" || (zone.Account.ID != "" && zone.Account.ID != candidate.RouteAutomation.AccountID) {
		return "", "", errors.New("Cloudflare zone does not belong to the configured account")
	}
	current, err := manager.readAutomatedTunnelConfiguration(ctx, apiConfig, RouteAutomationConfig{
		AccountID: candidate.RouteAutomation.AccountID, ZoneID: candidate.RouteAutomation.ZoneID,
		TunnelID: tunnelID, APIToken: candidate.RouteAutomation.APIToken,
	})
	if err != nil {
		return "", "", fmt.Errorf("verify Cloudflare tunnel: %w", err)
	}
	if current.Source != "cloudflare" {
		return "", "", fmt.Errorf("Cloudflare tunnel is %q managed; Fleet requires a remotely managed tunnel", current.Source)
	}
	path := fmt.Sprintf("/accounts/%s/cfd_tunnel/%s/configurations", url.PathEscape(candidate.RouteAutomation.AccountID), url.PathEscape(tunnelID))
	if current.Version > 0 {
		path += "?version=" + fmt.Sprint(current.Version)
	}
	ingress := current.Config.Ingress
	if len(ingress) == 0 {
		ingress = []map[string]any{{"service": "http_status:404"}}
	}
	verifiedConfig := map[string]any{"ingress": ingress}
	if current.Config.OriginRequest != nil {
		verifiedConfig["originRequest"] = current.Config.OriginRequest
	}
	body := map[string]any{"config": verifiedConfig}
	if err := manager.apiWithConfig(ctx, apiConfig, candidate.RouteAutomation.APIToken, http.MethodPut, path, body, nil); err != nil {
		return "", "", cloudflareTunnelConfigurationWriteError(err)
	}
	return tunnelID, zoneName, nil
}

func cloudflareTunnelConfigurationWriteError(err error) error {
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "http 401") || strings.Contains(lower, "http 403") {
		return fmt.Errorf("Cloudflare API token cannot edit tunnel configuration: %w", err)
	}
	return fmt.Errorf("Cloudflare rejected tunnel configuration: %w", err)
}

func (manager *Manager) reconcileConfig(ctx context.Context, config Config) (map[string]string, error) {
	routes, err := manager.instanceRoutes(ctx, config)
	if err != nil {
		return nil, err
	}
	if err := manager.reconcileTunnel(ctx, config, tunnelPlan{
		Token: config.AdminAPIToken, TunnelID: config.AdminTunnelID,
		Routes:     map[string]string{config.AdminHostname: "http://control-plane:9180"},
		AccessTeam: config.AdminAccessTeam, AccessAudience: config.AdminAccessAudience,
	}); err != nil {
		return nil, fmt.Errorf("admin tunnel: %w", err)
	}
	if err := manager.reconcileDNS(ctx, config, config.AdminAPIToken, config.AdminTunnelID, map[string]string{config.AdminHostname: ""}, false); err != nil {
		return nil, fmt.Errorf("admin DNS: %w", err)
	}
	if err := manager.reconcileTunnel(ctx, config, tunnelPlan{
		Token: config.InstancesAPIToken, TunnelID: config.InstancesTunnelID,
		Routes: routes, AccessTeam: config.InstancesAccessTeam,
		AccessAudience: config.InstancesAccessAudience,
	}); err != nil {
		return nil, fmt.Errorf("instance tunnel: %w", err)
	}
	if err := manager.reconcileDNS(ctx, config, config.InstancesAPIToken, config.InstancesTunnelID, routes, true); err != nil {
		return nil, fmt.Errorf("instance DNS: %w", err)
	}
	return routes, nil
}

// Disable stops connector processes by removing Fleet-owned runtime files.
// Cloudflare tunnel, DNS, and Access resources remain unchanged. Legacy
// provider-managed configurations retain their historical remote-cleanup
// behavior for safe migration.
func (manager *Manager) Disable(ctx context.Context) error {
	manager.lifecycle.Lock()
	defer manager.lifecycle.Unlock()
	manager.reconcile.Lock()
	defer manager.reconcile.Unlock()
	manager.mu.RLock()
	config := manager.config
	configured := manager.status.Configured
	manager.mu.RUnlock()
	if !configured {
		return nil
	}
	if !legacyProviderManaged(config) {
		var failures []error
		for label, path := range map[string]string{
			"admin connector configuration":    config.AdminConnectorConfigPath,
			"instance connector configuration": config.InstancesConnectorConfigPath,
			"admin connector token":            config.AdminConnectorTokenPath,
			"instance connector token":         config.InstancesConnectorTokenPath,
		} {
			if path == "" {
				continue
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, fmt.Errorf("remove %s: %w", label, err))
			}
		}
		if err := errors.Join(failures...); err != nil {
			return err
		}
		manager.mu.Lock()
		manager.config = runtimeOnlyConfig(config)
		manager.routes = make(map[string]struct{})
		manager.observations = make(map[string]RouteObservation)
		manager.status = Status{State: "disabled"}
		manager.mu.Unlock()
		return nil
	}
	var failures []error
	if err := os.Remove(config.AdminConnectorTokenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		failures = append(failures, fmt.Errorf("remove admin connector token: %w", err))
	}
	if err := os.Remove(config.InstancesConnectorTokenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		failures = append(failures, fmt.Errorf("remove instance connector token: %w", err))
	}
	if err := manager.reconcileTunnel(ctx, config, tunnelPlan{Token: config.AdminAPIToken, TunnelID: config.AdminTunnelID, Routes: map[string]string{}, AccessTeam: config.AdminAccessTeam, AccessAudience: config.AdminAccessAudience}); err != nil {
		failures = append(failures, fmt.Errorf("disable admin tunnel: %w", err))
	}
	if err := manager.reconcileTunnel(ctx, config, tunnelPlan{Token: config.InstancesAPIToken, TunnelID: config.InstancesTunnelID, Routes: map[string]string{}, AccessTeam: config.InstancesAccessTeam, AccessAudience: config.InstancesAccessAudience}); err != nil {
		failures = append(failures, fmt.Errorf("disable instance tunnel: %w", err))
	}
	if err := manager.deleteManagedDNS(ctx, config, config.AdminAPIToken, config.AdminTunnelID, func(name string) bool { return name == config.AdminHostname }); err != nil {
		failures = append(failures, fmt.Errorf("remove admin DNS: %w", err))
	}
	cleanupErr := errors.Join(failures...)
	manager.mu.Lock()
	if cleanupErr != nil {
		manager.routes = make(map[string]struct{})
		manager.observations = make(map[string]RouteObservation)
		manager.status.State = "cleanup_pending"
		manager.status.LastError = cleanupErr.Error()
		manager.status.Admin.Synced = false
		manager.status.Admin.Routes = 0
		manager.status.Instances.Synced = false
		manager.status.Instances.Routes = 0
		manager.mu.Unlock()
		return cleanupErr
	}
	manager.config = runtimeOnlyConfig(config)
	manager.routes = make(map[string]struct{})
	manager.observations = make(map[string]RouteObservation)
	manager.status = Status{State: "disabled"}
	manager.mu.Unlock()
	return nil
}

func runtimeOnlyConfig(config Config) Config {
	return Config{
		APIBaseURL: config.APIBaseURL, SyncPeriod: config.SyncPeriod,
		CredentialsDirectory:         config.CredentialsDirectory,
		AdminConnectorConfigPath:     config.AdminConnectorConfigPath,
		InstancesConnectorConfigPath: config.InstancesConnectorConfigPath,
		AdminConnectorTokenPath:      config.AdminConnectorTokenPath,
		InstancesConnectorTokenPath:  config.InstancesConnectorTokenPath,
		AdminConnectorHealthURL:      config.AdminConnectorHealthURL,
		InstancesConnectorHealthURL:  config.InstancesConnectorHealthURL,
	}
}

func (manager *Manager) deleteManagedDNS(ctx context.Context, config Config, token, tunnelID string, matches func(string) bool) error {
	target := tunnelID + ".cfargotunnel.com"
	path := fmt.Sprintf("/zones/%s/dns_records?type=CNAME&per_page=5000", url.PathEscape(config.ZoneID))
	var records []dnsRecord
	if err := manager.apiWithConfig(ctx, config, token, http.MethodGet, path, nil, &records); err != nil {
		return err
	}
	for _, record := range records {
		name := normalizeHostname(record.Name)
		if !matches(name) || record.Comment != managedDNSComment || normalizeHostname(record.Content) != target {
			continue
		}
		deletePath := fmt.Sprintf("/zones/%s/dns_records/%s", url.PathEscape(config.ZoneID), url.PathEscape(record.ID))
		if err := manager.apiWithConfig(ctx, config, token, http.MethodDelete, deletePath, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

func (manager *Manager) setSyncing() {
	manager.mu.Lock()
	if manager.status.State == "synced" && manager.status.LastSyncAt != "" &&
		manager.status.Admin.Synced && manager.status.Instances.Synced {
		manager.status.LastError = ""
		manager.mu.Unlock()
		return
	}
	manager.status.State = "syncing"
	manager.status.LastError = ""
	manager.mu.Unlock()
}

func (manager *Manager) fail(err error) error {
	manager.mu.Lock()
	manager.status.State = "error"
	manager.status.LastError = err.Error()
	manager.status.Admin.Synced = false
	manager.status.Instances.Synced = false
	manager.mu.Unlock()
	return err
}

type tunnelPlan struct {
	Token          string
	TunnelID       string
	Routes         map[string]string
	AccessTeam     string
	AccessAudience string
}

type tunnelConfiguration struct {
	Config struct {
		Ingress       []map[string]any `json:"ingress"`
		OriginRequest map[string]any   `json:"originRequest,omitempty"`
	} `json:"config"`
	Source  string `json:"source"`
	Version int    `json:"version"`
}

func (manager *Manager) reconcileTunnel(ctx context.Context, config Config, plan tunnelPlan) error {
	var current tunnelConfiguration
	path := fmt.Sprintf("/accounts/%s/cfd_tunnel/%s/configurations", url.PathEscape(config.AccountID), url.PathEscape(plan.TunnelID))
	if err := manager.apiWithConfig(ctx, config, plan.Token, http.MethodGet, path, nil, &current); err != nil {
		return err
	}
	if current.Source != "cloudflare" {
		return fmt.Errorf("tunnel is %q managed; Fleet only reconciles remotely managed tunnels", current.Source)
	}
	hostnames := make([]string, 0, len(plan.Routes))
	for hostname := range plan.Routes {
		hostnames = append(hostnames, hostname)
	}
	sort.Strings(hostnames)
	ingress := make([]map[string]any, 0, len(hostnames)+1)
	for _, hostname := range hostnames {
		ingress = append(ingress, map[string]any{
			"hostname": hostname,
			"service":  plan.Routes[hostname],
			"originRequest": map[string]any{"access": map[string]any{
				"required": true, "teamName": plan.AccessTeam, "audTag": []string{plan.AccessAudience},
			}},
		})
	}
	ingress = append(ingress, map[string]any{"service": "http_status:404"})
	body := map[string]any{"config": map[string]any{"ingress": ingress, "originRequest": map[string]any{}}}
	if current.Version > 0 {
		path += "?version=" + fmt.Sprint(current.Version)
	}
	return manager.apiWithConfig(ctx, config, plan.Token, http.MethodPut, path, body, nil)
}

type dnsRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Comment string `json:"comment"`
	Proxied bool   `json:"proxied"`
}

func (manager *Manager) reconcileDNS(ctx context.Context, config Config, token, tunnelID string, desired map[string]string, prune bool) error {
	target := tunnelID + ".cfargotunnel.com"
	path := fmt.Sprintf("/zones/%s/dns_records?type=CNAME&per_page=5000", url.PathEscape(config.ZoneID))
	var records []dnsRecord
	if err := manager.apiWithConfig(ctx, config, token, http.MethodGet, path, nil, &records); err != nil {
		return err
	}
	byName := make(map[string][]dnsRecord)
	for _, record := range records {
		byName[normalizeHostname(record.Name)] = append(byName[normalizeHostname(record.Name)], record)
	}
	for hostname := range desired {
		existing := byName[hostname]
		if len(existing) > 1 {
			return fmt.Errorf("DNS hostname %s has multiple CNAME records", hostname)
		}
		if len(existing) == 1 {
			record := existing[0]
			if normalizeHostname(record.Content) != target || !record.Proxied {
				return fmt.Errorf("DNS hostname %s conflicts with an unmanaged record", hostname)
			}
			if record.Comment != managedDNSComment {
				return fmt.Errorf("DNS hostname %s points to the Fleet tunnel but is not owned by Fleet", hostname)
			}
			continue
		}
		body := map[string]any{
			"type": "CNAME", "name": hostname, "content": target, "proxied": true,
			"ttl": 1, "comment": managedDNSComment,
		}
		createPath := fmt.Sprintf("/zones/%s/dns_records", url.PathEscape(config.ZoneID))
		if err := manager.apiWithConfig(ctx, config, token, http.MethodPost, createPath, body, nil); err != nil {
			return fmt.Errorf("create %s: %w", hostname, err)
		}
	}
	if !prune {
		return nil
	}
	for _, record := range records {
		name := normalizeHostname(record.Name)
		_, wanted := desired[name]
		if wanted || record.Comment != managedDNSComment || normalizeHostname(record.Content) != target {
			continue
		}
		deletePath := fmt.Sprintf("/zones/%s/dns_records/%s", url.PathEscape(config.ZoneID), url.PathEscape(record.ID))
		if err := manager.apiWithConfig(ctx, config, token, http.MethodDelete, deletePath, nil, nil); err != nil {
			return fmt.Errorf("delete stale %s: %w", name, err)
		}
	}
	return nil
}

type apiEnvelope struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

func (manager *Manager) api(ctx context.Context, token, method, path string, body any, output any) error {
	manager.mu.RLock()
	config := manager.config
	manager.mu.RUnlock()
	return manager.apiWithConfig(ctx, config, token, method, path, body, output)
}

func (manager *Manager) apiWithConfig(ctx context.Context, config Config, token, method, path string, body any, output any) error {
	if config.OAuth.RefreshToken != "" && token == config.RouteAutomation.APIToken {
		if config.OAuth.AccessToken != "" && time.Until(config.OAuth.ExpiresAt) > 2*time.Minute {
			token = config.OAuth.AccessToken
		} else {
			resolved, err := manager.oauthAccessToken(ctx, config)
			if err != nil {
				return fmt.Errorf("refresh Cloudflare OAuth authorization: %w", err)
			}
			token = resolved
		}
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
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
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return err
	}
	var envelope apiEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("Cloudflare returned HTTP %d with an invalid response", response.StatusCode)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !envelope.Success {
		message := http.StatusText(response.StatusCode)
		if len(envelope.Errors) > 0 && envelope.Errors[0].Message != "" {
			message = envelope.Errors[0].Message
		}
		return fmt.Errorf("Cloudflare API HTTP %d: %s", response.StatusCode, message)
	}
	if output != nil && len(envelope.Result) > 0 && string(envelope.Result) != "null" {
		if err := json.Unmarshal(envelope.Result, output); err != nil {
			return fmt.Errorf("decode Cloudflare response: %w", err)
		}
	}
	return nil
}

func (manager *Manager) oauthAccessToken(ctx context.Context, fallback Config) (string, error) {
	manager.oauthRefresh.Lock()
	defer manager.oauthRefresh.Unlock()

	manager.mu.RLock()
	current := manager.config
	persist := manager.persistConfig
	manager.mu.RUnlock()
	if current.OAuth.ClientID == "" || current.OAuth.RefreshToken == "" {
		current = fallback
	}
	if current.OAuth.AccessToken != "" && time.Until(current.OAuth.ExpiresAt) > 2*time.Minute {
		return current.OAuth.AccessToken, nil
	}
	if current.OAuth.ClientID == "" || current.OAuth.RefreshToken == "" {
		return "", errors.New("OAuth refresh credentials are unavailable")
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {current.OAuth.ClientID},
		"refresh_token": {current.OAuth.RefreshToken},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, defaultOAuthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := manager.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
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
		return "", fmt.Errorf("Cloudflare OAuth returned HTTP %d with an invalid response", response.StatusCode)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || strings.TrimSpace(result.AccessToken) == "" {
		detail := strings.TrimSpace(result.Description)
		if detail == "" {
			detail = strings.TrimSpace(result.Error)
		}
		if detail == "" {
			detail = http.StatusText(response.StatusCode)
		}
		return "", fmt.Errorf("Cloudflare OAuth HTTP %d: %s", response.StatusCode, detail)
	}
	if result.TokenType != "" && !strings.EqualFold(result.TokenType, "bearer") {
		return "", fmt.Errorf("Cloudflare OAuth returned unsupported token type %q", result.TokenType)
	}
	if result.ExpiresIn <= 0 {
		return "", errors.New("Cloudflare OAuth response omitted token expiry")
	}

	next := current
	next.OAuth.AccessToken = strings.TrimSpace(result.AccessToken)
	if strings.TrimSpace(result.RefreshToken) != "" {
		next.OAuth.RefreshToken = strings.TrimSpace(result.RefreshToken)
	}
	if strings.TrimSpace(result.Scope) != "" {
		next.OAuth.Scope = strings.TrimSpace(result.Scope)
	}
	next.OAuth.ExpiresAt = time.Now().UTC().Add(time.Duration(result.ExpiresIn) * time.Second)
	next.RouteAutomation.APIToken = next.OAuth.AccessToken

	if persist != nil {
		if err := persist(ctx, next); err != nil {
			return "", fmt.Errorf("persist rotated OAuth authorization: %w", err)
		}
	}
	manager.mu.Lock()
	if manager.config.OAuth.ClientID == next.OAuth.ClientID || manager.config.OAuth.ClientID == "" {
		manager.config.OAuth = next.OAuth
		manager.config.RouteAutomation.APIToken = next.RouteAutomation.APIToken
	}
	manager.mu.Unlock()
	return next.OAuth.AccessToken, nil
}
