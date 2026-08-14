package remoteaccess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/cloudflare"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

const (
	ModeManagedCloudflare = "managed_cloudflare"
	ModeExistingEndpoints = "existing_endpoints"
	AdminOriginService    = "http://control-plane:9180"
)

type ExistingEndpointsConfig struct {
	AdminURL              string            `json:"admin_url"`
	InstanceDashboardURLs map[string]string `json:"instance_dashboard_urls"`
}

type Config struct {
	Mode       string                  `json:"mode"`
	Cloudflare cloudflare.Config       `json:"cloudflare,omitempty"`
	Existing   ExistingEndpointsConfig `json:"existing,omitempty"`
}

type InstanceEndpoint struct {
	InstanceID   string `json:"instance_id"`
	InstanceName string `json:"instance_name"`
	DashboardURL string `json:"dashboard_url"`
}

type PublishedRoute struct {
	InstanceID        string `json:"instance_id"`
	InstanceName      string `json:"instance_name"`
	Hostname          string `json:"hostname,omitempty"`
	OriginService     string `json:"origin_service"`
	ProviderState     string `json:"provider_state"`
	ProviderDetail    string `json:"provider_detail,omitempty"`
	ProviderCheckedAt string `json:"provider_checked_at,omitempty"`
	DNSState          string `json:"dns_state"`
	DNSDetail         string `json:"dns_detail,omitempty"`
	DNSCheckedAt      string `json:"dns_checked_at,omitempty"`
	RouteState        string `json:"route_state"`
	RouteDetail       string `json:"route_detail,omitempty"`
	RouteCheckedAt    string `json:"route_checked_at,omitempty"`
	EndpointState     string `json:"endpoint_state"`
	EndpointDetail    string `json:"endpoint_detail,omitempty"`
	EndpointCheckedAt string `json:"endpoint_checked_at,omitempty"`
	Published         bool   `json:"published"`
	Revalidating      bool   `json:"revalidating,omitempty"`
}

type ConfigurationView struct {
	Mode                               string             `json:"mode"`
	AdminTunnelID                      string             `json:"admin_tunnel_id"`
	InstancesTunnelID                  string             `json:"instances_tunnel_id"`
	AdminHostname                      string             `json:"admin_hostname"`
	AdminCredentialAvailable           bool               `json:"admin_credential_available"`
	InstancesCredentialAvailable       bool               `json:"instances_credential_available"`
	AdminTunnelTokenConfigured         bool               `json:"admin_tunnel_token_configured"`
	InstancesTunnelTokenConfigured     bool               `json:"instances_tunnel_token_configured"`
	AdminTunnelTokenFingerprint        string             `json:"admin_tunnel_token_fingerprint,omitempty"`
	InstancesTunnelTokenFingerprint    string             `json:"instances_tunnel_token_fingerprint,omitempty"`
	LegacyProviderManaged              bool               `json:"legacy_provider_managed"`
	InstancePublishingConfigured       bool               `json:"instance_publishing_configured"`
	InstancePublishingAccountID        string             `json:"instance_publishing_account_id,omitempty"`
	InstancePublishingZoneID           string             `json:"instance_publishing_zone_id,omitempty"`
	InstancePublishingZone             string             `json:"instance_publishing_zone,omitempty"`
	InstancePublishingFleetNamespace   string             `json:"instance_publishing_fleet_namespace,omitempty"`
	InstancePublishingTunnelID         string             `json:"instance_publishing_tunnel_id,omitempty"`
	InstancePublishingTokenFingerprint string             `json:"instance_publishing_token_fingerprint,omitempty"`
	AdminURL                           string             `json:"admin_url"`
	InstanceEndpoints                  []InstanceEndpoint `json:"instance_endpoints"`
	AdminOriginService                 string             `json:"admin_origin_service"`
	InstanceRoutes                     []PublishedRoute   `json:"instance_routes"`
}

type BoundaryStatus struct {
	TunnelID           string `json:"tunnel_id,omitempty"`
	Hostname           string `json:"hostname,omitempty"`
	URL                string `json:"url,omitempty"`
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
	Mode       string         `json:"mode,omitempty"`
	State      string         `json:"state"`
	Admin      BoundaryStatus `json:"admin"`
	Instances  BoundaryStatus `json:"instances"`
	LastSyncAt string         `json:"last_sync_at,omitempty"`
	LastError  string         `json:"last_error,omitempty"`
}

type InstanceSource func(context.Context) ([]domain.Instance, error)

type Manager struct {
	managed *cloudflare.Manager
	source  InstanceSource

	lifecycle sync.Mutex
	mu        sync.RWMutex
	mode      string
	existing  ExistingEndpointsConfig
	status    Status
}

func New(managed *cloudflare.Manager, source InstanceSource) (*Manager, error) {
	if managed == nil {
		return nil, errors.New("managed Cloudflare runtime is required")
	}
	if source == nil {
		return nil, errors.New("remote access requires an instance source")
	}
	return &Manager{managed: managed, source: source, status: Status{State: "disabled"}}, nil
}

func DecodeConfig(payload []byte) (Config, error) {
	var probe struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(probe.Mode) != "" {
		var config Config
		if err := json.Unmarshal(payload, &config); err != nil {
			return Config{}, err
		}
		return config, nil
	}

	// Versions before 0.11 stored a flat Cloudflare configuration. Keep it
	// readable so upgrades never strand an encrypted, active remote boundary.
	var legacy cloudflare.Config
	if err := json.Unmarshal(payload, &legacy); err != nil {
		return Config{}, err
	}
	return Config{Mode: ModeManagedCloudflare, Cloudflare: legacy}, nil
}

func (manager *Manager) Start(ctx context.Context) {
	manager.managed.Start(ctx)
}

func (manager *Manager) Trigger() {
	manager.mu.RLock()
	mode := manager.mode
	manager.mu.RUnlock()
	if mode == ModeManagedCloudflare {
		manager.managed.Trigger()
		return
	}
	if mode == ModeExistingEndpoints {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = manager.Reconcile(ctx)
		}()
	}
}

func (manager *Manager) Configure(ctx context.Context, config Config) error {
	manager.lifecycle.Lock()
	defer manager.lifecycle.Unlock()

	config.Mode = strings.TrimSpace(config.Mode)
	if config.Mode == "" {
		config.Mode = ModeManagedCloudflare
	}
	current := manager.Status()
	if current.Configured && current.Mode != config.Mode {
		return errors.New("disable the current remote access mode before selecting another mode")
	}

	switch config.Mode {
	case ModeManagedCloudflare:
		if err := manager.managed.Configure(ctx, config.Cloudflare); err != nil {
			return err
		}
		manager.mu.Lock()
		manager.mode = ModeManagedCloudflare
		manager.existing = ExistingEndpointsConfig{}
		manager.mu.Unlock()
		return nil
	case ModeExistingEndpoints:
		normalized, status, err := manager.validateExisting(ctx, config.Existing)
		if err != nil {
			return err
		}
		manager.mu.Lock()
		manager.mode = ModeExistingEndpoints
		manager.existing = normalized
		manager.status = status
		manager.mu.Unlock()
		return nil
	default:
		return errors.New("remote access mode must be managed_cloudflare or existing_endpoints")
	}
}

func (manager *Manager) RecordConfigurationFailure(config Config, failure error) {
	if config.Mode == "" || config.Mode == ModeManagedCloudflare {
		manager.managed.RecordConfigurationFailure(config.Cloudflare, failure)
		manager.mu.Lock()
		manager.mode = ModeManagedCloudflare
		manager.status = Status{}
		manager.mu.Unlock()
		return
	}
	manager.mu.Lock()
	manager.mode = config.Mode
	manager.existing = config.Existing
	manager.status = Status{Configured: true, Mode: config.Mode, State: "error", LastError: failure.Error()}
	manager.mu.Unlock()
}

func (manager *Manager) Status() Status {
	manager.mu.RLock()
	mode := manager.mode
	status := manager.status
	manager.mu.RUnlock()
	if mode != ModeManagedCloudflare {
		return status
	}
	managed := manager.managed.Status()
	return Status{
		Configured: managed.Configured, Mode: ModeManagedCloudflare, State: managed.State,
		Admin: BoundaryStatus{
			TunnelID: managed.Admin.TunnelID, Hostname: managed.Admin.Hostname, Routes: managed.Admin.Routes, Synced: managed.Admin.Synced,
			ConnectorState: managed.Admin.ConnectorState, ConnectorCheckedAt: managed.Admin.ConnectorCheckedAt, ConnectorError: managed.Admin.ConnectorError,
			EndpointState: managed.Admin.EndpointState, EndpointDetail: managed.Admin.EndpointDetail, EndpointCheckedAt: managed.Admin.EndpointCheckedAt,
		},
		Instances: BoundaryStatus{
			TunnelID: managed.Instances.TunnelID, Routes: managed.Instances.Routes, Synced: managed.Instances.Synced,
			ConnectorState: managed.Instances.ConnectorState, ConnectorCheckedAt: managed.Instances.ConnectorCheckedAt, ConnectorError: managed.Instances.ConnectorError,
		},
		LastSyncAt: managed.LastSyncAt, LastError: managed.LastError,
	}
}

func (manager *Manager) Configuration(ctx context.Context) ConfigurationView {
	manager.mu.RLock()
	mode := manager.mode
	existing := cloneExisting(manager.existing)
	manager.mu.RUnlock()
	if mode == ModeManagedCloudflare || mode == "" {
		view := manager.managed.Configuration()
		routes := manager.publishedInstanceRoutes(ctx)
		return ConfigurationView{
			Mode:          mode,
			AdminTunnelID: view.AdminTunnelID, InstancesTunnelID: view.InstancesTunnelID,
			AdminHostname:                      view.AdminHostname,
			AdminCredentialAvailable:           view.AdminCredentialAvailable,
			InstancesCredentialAvailable:       view.InstancesCredentialAvailable,
			AdminTunnelTokenConfigured:         view.AdminTunnelTokenConfigured,
			InstancesTunnelTokenConfigured:     view.InstancesTunnelTokenConfigured,
			AdminTunnelTokenFingerprint:        view.AdminTunnelTokenFingerprint,
			InstancesTunnelTokenFingerprint:    view.InstancesTunnelTokenFingerprint,
			LegacyProviderManaged:              view.LegacyProviderManaged,
			InstancePublishingConfigured:       view.RouteAutomationConfigured && view.RouteAutomationFleetNamespace != "",
			InstancePublishingAccountID:        view.RouteAutomationAccountID,
			InstancePublishingZoneID:           view.RouteAutomationZoneID,
			InstancePublishingZone:             view.RouteAutomationZoneName,
			InstancePublishingFleetNamespace:   view.RouteAutomationFleetNamespace,
			InstancePublishingTunnelID:         view.RouteAutomationTunnelID,
			InstancePublishingTokenFingerprint: view.RouteAutomationTokenFingerprint,
			AdminOriginService:                 AdminOriginService,
			InstanceRoutes:                     routes,
		}
	}
	view := ConfigurationView{Mode: mode, AdminURL: existing.AdminURL}
	instances, err := manager.source(ctx)
	if err != nil {
		for instanceID, dashboardURL := range existing.InstanceDashboardURLs {
			view.InstanceEndpoints = append(view.InstanceEndpoints, InstanceEndpoint{
				InstanceID: instanceID, InstanceName: instanceID, DashboardURL: dashboardURL,
			})
		}
		sort.Slice(view.InstanceEndpoints, func(i, j int) bool {
			return view.InstanceEndpoints[i].InstanceID < view.InstanceEndpoints[j].InstanceID
		})
		return view
	}
	for _, instance := range instances {
		if instance.Status == domain.InstanceDeleted || instance.Status == domain.InstanceDeleting {
			continue
		}
		view.InstanceEndpoints = append(view.InstanceEndpoints, InstanceEndpoint{
			InstanceID: instance.ID, InstanceName: instance.Name,
			DashboardURL: existing.InstanceDashboardURLs[instance.ID],
		})
	}
	sort.Slice(view.InstanceEndpoints, func(i, j int) bool {
		return view.InstanceEndpoints[i].InstanceName < view.InstanceEndpoints[j].InstanceName
	})
	return view
}

func (manager *Manager) publishedInstanceRoutes(ctx context.Context) []PublishedRoute {
	instances, err := manager.source(ctx)
	if err != nil {
		return nil
	}
	observations := manager.managed.RouteObservations()
	routes := make([]PublishedRoute, 0, len(instances))
	for _, instance := range instances {
		if instance.Status == domain.InstanceDeleted || instance.Status == domain.InstanceDeleting {
			continue
		}
		hostname, hostnameErr := cloudflare.NormalizePublicHostname(instance.PublicHostname)
		if hostnameErr != nil {
			hostname = ""
		}
		route := PublishedRoute{
			InstanceID: instance.ID, InstanceName: instance.Name, Hostname: hostname,
			OriginService: "http://hermes-fleet-instance-" + instance.Name + "-dashboard:9119",
			ProviderState: cloudflare.RouteProviderReady,
			DNSState:      cloudflare.ResourcePending, RouteState: cloudflare.ResourcePending,
			EndpointState: cloudflare.EndpointUnchecked,
		}
		if observation, exists := observations[hostname]; exists {
			route.ProviderState = observation.ProviderState
			route.ProviderDetail = observation.ProviderDetail
			route.ProviderCheckedAt = observation.ProviderCheckedAt
			route.DNSState = observation.DNSState
			route.DNSDetail = observation.DNSDetail
			route.DNSCheckedAt = observation.DNSCheckedAt
			route.RouteState = observation.IngressState
			route.RouteDetail = observation.IngressDetail
			route.RouteCheckedAt = observation.IngressCheckedAt
			route.EndpointState = observation.EndpointState
			route.EndpointDetail = observation.EndpointDetail
			route.EndpointCheckedAt = observation.EndpointCheckedAt
			route.Revalidating = observation.Revalidating
			route.Published = observation.Revalidating ||
				(observation.ProviderState == cloudflare.RouteProviderPublished && observation.EndpointState == cloudflare.EndpointReachable)
		}
		routes = append(routes, route)
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].InstanceName < routes[j].InstanceName })
	return routes
}

func (manager *Manager) VerifyInstancePublishing(ctx context.Context, config Config) (string, string, error) {
	if strings.TrimSpace(config.Mode) == "" {
		config.Mode = ModeManagedCloudflare
	}
	if config.Mode != ModeManagedCloudflare {
		return "", "", errors.New("instance publishing requires managed Cloudflare remote access")
	}
	return manager.managed.VerifyInstancePublishing(ctx, config.Cloudflare)
}

func (manager *Manager) Config() Config {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if manager.mode == ModeExistingEndpoints {
		return Config{Mode: manager.mode, Existing: cloneExisting(manager.existing)}
	}
	return Config{Mode: ModeManagedCloudflare, Cloudflare: manager.managed.Config()}
}

func (manager *Manager) PublicDashboardURL(instanceID, publicHostname string) string {
	manager.mu.RLock()
	mode := manager.mode
	urlValue := manager.existing.InstanceDashboardURLs[instanceID]
	manager.mu.RUnlock()
	if mode == ModeExistingEndpoints {
		return urlValue
	}
	if mode == ModeManagedCloudflare {
		return manager.managed.PublicDashboardURL(publicHostname)
	}
	return ""
}

func (manager *Manager) Reconcile(ctx context.Context) error {
	manager.mu.RLock()
	mode := manager.mode
	existing := cloneExisting(manager.existing)
	manager.mu.RUnlock()
	if mode == ModeManagedCloudflare {
		return manager.managed.Reconcile(ctx)
	}
	if mode != ModeExistingEndpoints {
		return errors.New("remote access is not configured")
	}
	normalized, status, err := manager.validateExisting(ctx, existing)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if err != nil {
		manager.status.State = "error"
		manager.status.LastError = err.Error()
		return err
	}
	manager.existing = normalized
	manager.status = status
	return nil
}

func (manager *Manager) Disable(ctx context.Context) error {
	manager.lifecycle.Lock()
	defer manager.lifecycle.Unlock()
	manager.mu.RLock()
	mode := manager.mode
	manager.mu.RUnlock()
	if mode == ModeManagedCloudflare {
		if err := manager.managed.Disable(ctx); err != nil {
			return err
		}
	}
	manager.mu.Lock()
	manager.mode = ""
	manager.existing = ExistingEndpointsConfig{}
	manager.status = Status{State: "disabled"}
	manager.mu.Unlock()
	return nil
}

func (manager *Manager) validateExisting(ctx context.Context, config ExistingEndpointsConfig) (ExistingEndpointsConfig, Status, error) {
	adminURL := ""
	var err error
	if strings.TrimSpace(config.AdminURL) != "" {
		adminURL, err = normalizePublicURL(config.AdminURL)
		if err != nil {
			return ExistingEndpointsConfig{}, Status{}, fmt.Errorf("Fleet Manager public URL: %w", err)
		}
	}
	instances, err := manager.source(ctx)
	if err != nil {
		return ExistingEndpointsConfig{}, Status{}, fmt.Errorf("list managed instances: %w", err)
	}
	normalized := ExistingEndpointsConfig{AdminURL: adminURL, InstanceDashboardURLs: make(map[string]string)}
	seenURLs := make(map[string]string)
	if adminURL != "" {
		seenURLs[adminURL] = "Fleet Manager"
	}
	knownInstances := make(map[string]domain.Instance, len(instances))
	for _, instance := range instances {
		if instance.Status == domain.InstanceDeleted || instance.Status == domain.InstanceDeleting {
			continue
		}
		knownInstances[instance.ID] = instance
	}
	for instanceID, value := range config.InstanceDashboardURLs {
		instance, exists := knownInstances[instanceID]
		if !exists {
			return ExistingEndpointsConfig{}, Status{}, fmt.Errorf("dashboard URL references an unknown or inactive instance: %s", instanceID)
		}
		if strings.TrimSpace(value) == "" {
			continue
		}
		dashboardURL, normalizeErr := normalizePublicURL(value)
		if normalizeErr != nil {
			return ExistingEndpointsConfig{}, Status{}, fmt.Errorf("dashboard URL for %s: %w", instance.Name, normalizeErr)
		}
		if owner, duplicate := seenURLs[dashboardURL]; duplicate {
			return ExistingEndpointsConfig{}, Status{}, fmt.Errorf("dashboard URL for %s duplicates %s", instance.Name, owner)
		}
		seenURLs[dashboardURL] = instance.Name
		normalized.InstanceDashboardURLs[instanceID] = dashboardURL
	}
	if adminURL == "" && len(normalized.InstanceDashboardURLs) == 0 {
		return ExistingEndpointsConfig{}, Status{}, errors.New("register at least one Fleet Manager or instance dashboard HTTPS URL")
	}
	hostname := ""
	if parsed, parseErr := url.Parse(adminURL); parseErr == nil {
		hostname = parsed.Hostname()
	}
	adminRoutes := 0
	if adminURL != "" {
		adminRoutes = 1
	}
	status := Status{
		Configured: true, Mode: ModeExistingEndpoints, State: "registered",
		Admin:     BoundaryStatus{Hostname: hostname, URL: adminURL, Routes: adminRoutes},
		Instances: BoundaryStatus{Routes: len(normalized.InstanceDashboardURLs)},
	}
	return normalized, status, nil
}

func normalizePublicURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", errors.New("must be an absolute HTTPS URL")
	}
	if parsed.User != nil {
		return "", errors.New("must not contain embedded credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("must not contain a query string or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", errors.New("must point to the public origin root")
	}
	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") || strings.HasSuffix(hostname, ".local") {
		return "", errors.New("must use a public DNS hostname")
	}
	if ip := net.ParseIP(hostname); ip != nil {
		return "", errors.New("must use a public DNS hostname instead of an IP address")
	}
	if !validPublicHostname(hostname) {
		return "", errors.New("must use a valid public DNS hostname")
	}
	parsed.Scheme = "https"
	if parsed.Port() == "443" {
		parsed.Host = hostname
	} else {
		parsed.Host = strings.ToLower(parsed.Host)
	}
	parsed.Path = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func validPublicHostname(hostname string) bool {
	if len(hostname) > 253 || !strings.Contains(hostname, ".") || strings.Contains(hostname, "*") {
		return false
	}
	for _, label := range strings.Split(hostname, ".") {
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

func cloneExisting(config ExistingEndpointsConfig) ExistingEndpointsConfig {
	clone := ExistingEndpointsConfig{AdminURL: config.AdminURL, InstanceDashboardURLs: make(map[string]string, len(config.InstanceDashboardURLs))}
	for instanceID, dashboardURL := range config.InstanceDashboardURLs {
		clone.InstanceDashboardURLs[instanceID] = dashboardURL
	}
	return clone
}
