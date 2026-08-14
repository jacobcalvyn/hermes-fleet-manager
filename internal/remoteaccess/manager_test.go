package remoteaccess

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/cloudflare"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func TestExistingEndpointsAreNormalizedAndMappedWithoutProviderMutation(t *testing.T) {
	instances := []domain.Instance{
		{ID: "instance-a", Name: "alpha", Status: domain.InstanceRunning},
		{ID: "instance-b", Name: "beta", Status: domain.InstanceStopped},
		{ID: "instance-deleted", Name: "deleted", Status: domain.InstanceDeleted},
	}
	manager := newTestManager(t, func(context.Context) ([]domain.Instance, error) { return instances, nil })
	config := Config{
		Mode: ModeExistingEndpoints,
		Existing: ExistingEndpointsConfig{
			AdminURL: "https://ADMIN.example.com/",
			InstanceDashboardURLs: map[string]string{
				"instance-a": "https://ALPHA.example.com/",
				"instance-b": "https://beta.example.com",
			},
		},
	}
	if err := manager.Configure(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	status := manager.Status()
	if status.Mode != ModeExistingEndpoints || status.State != "registered" || status.Instances.Routes != 2 || status.Admin.Synced {
		t.Fatalf("status=%+v", status)
	}
	if got := manager.PublicDashboardURL("instance-a", "alpha"); got != "https://alpha.example.com" {
		t.Fatalf("dashboard URL=%q", got)
	}
	view := manager.Configuration(context.Background())
	if view.AdminURL != "https://admin.example.com" || len(view.InstanceEndpoints) != 2 {
		t.Fatalf("configuration view=%+v", view)
	}
	if view.InstanceEndpoints[0].InstanceName != "alpha" || view.InstanceEndpoints[0].DashboardURL != "https://alpha.example.com" {
		t.Fatalf("first endpoint=%+v", view.InstanceEndpoints[0])
	}
}

func TestExistingEndpointConfigurationRetainsMappingsWhenInstanceLookupFails(t *testing.T) {
	lookupFails := false
	manager := newTestManager(t, func(context.Context) ([]domain.Instance, error) {
		if lookupFails {
			return nil, errors.New("database unavailable")
		}
		return []domain.Instance{{ID: "instance-a", Name: "alpha", Status: domain.InstanceRunning}}, nil
	})
	if err := manager.Configure(context.Background(), Config{
		Mode: ModeExistingEndpoints,
		Existing: ExistingEndpointsConfig{InstanceDashboardURLs: map[string]string{
			"instance-a": "https://alpha.example.com",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	lookupFails = true
	view := manager.Configuration(context.Background())
	if len(view.InstanceEndpoints) != 1 || view.InstanceEndpoints[0].InstanceID != "instance-a" || view.InstanceEndpoints[0].DashboardURL != "https://alpha.example.com" {
		t.Fatalf("configuration view after lookup failure=%+v", view)
	}
}

func TestExistingEndpointsRejectUnsafeOrIncompleteURLs(t *testing.T) {
	manager := newTestManager(t, func(context.Context) ([]domain.Instance, error) {
		return []domain.Instance{{ID: "instance-a", Name: "alpha", Status: domain.InstanceRunning}}, nil
	})
	tests := []struct {
		name   string
		config ExistingEndpointsConfig
		want   string
	}{
		{name: "empty", config: ExistingEndpointsConfig{}, want: "at least one"},
		{name: "http admin", config: ExistingEndpointsConfig{AdminURL: "http://admin.example.com"}, want: "absolute HTTPS"},
		{name: "local admin", config: ExistingEndpointsConfig{AdminURL: "https://localhost", InstanceDashboardURLs: map[string]string{"instance-a": "https://alpha.example.com"}}, want: "public DNS hostname"},
		{name: "invalid admin hostname", config: ExistingEndpointsConfig{AdminURL: "https://-admin.example.com"}, want: "valid public DNS hostname"},
		{name: "unknown instance", config: ExistingEndpointsConfig{AdminURL: "https://admin.example.com", InstanceDashboardURLs: map[string]string{"missing": "https://missing.example.com"}}, want: "unknown or inactive"},
		{name: "dashboard path", config: ExistingEndpointsConfig{AdminURL: "https://admin.example.com", InstanceDashboardURLs: map[string]string{"instance-a": "https://alpha.example.com/chat"}}, want: "public origin root"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := manager.Configure(context.Background(), Config{Mode: ModeExistingEndpoints, Existing: test.config})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Configure() error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestRemoteAccessModeSwitchRequiresExplicitDisable(t *testing.T) {
	manager := newTestManager(t, func(context.Context) ([]domain.Instance, error) { return nil, nil })
	if err := manager.Configure(context.Background(), Config{Mode: ModeExistingEndpoints, Existing: ExistingEndpointsConfig{AdminURL: "https://admin.example.com"}}); err != nil {
		t.Fatal(err)
	}
	err := manager.Configure(context.Background(), Config{Mode: ModeManagedCloudflare})
	if err == nil || !strings.Contains(err.Error(), "disable") {
		t.Fatalf("mode switch error=%v", err)
	}
	if err := manager.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := manager.Status(); status.Configured || status.State != "disabled" {
		t.Fatalf("status after disable=%+v", status)
	}
}

func TestManagedConfigurationDescribesFleetOwnedOriginServices(t *testing.T) {
	manager := newTestManager(t, func(context.Context) ([]domain.Instance, error) {
		return []domain.Instance{
			{ID: "instance-b", Name: "beta", Status: domain.InstanceStopped},
			{ID: "instance-a", Name: "alpha", Status: domain.InstanceRunning},
			{ID: "instance-deleted", Name: "deleted", Status: domain.InstanceDeleted},
		}, nil
	})

	view := manager.Configuration(context.Background())
	if view.AdminOriginService != AdminOriginService {
		t.Fatalf("admin origin=%q", view.AdminOriginService)
	}
	if len(view.InstanceRoutes) != 2 {
		t.Fatalf("routes=%+v", view.InstanceRoutes)
	}
	if route := view.InstanceRoutes[0]; route.InstanceID != "instance-a" || route.InstanceName != "alpha" || route.Hostname != "" || route.OriginService != "http://hermes-fleet-instance-alpha-dashboard:9119" {
		t.Fatalf("first route=%+v", route)
	}
	if route := view.InstanceRoutes[1]; route.InstanceID != "instance-b" || route.InstanceName != "beta" || route.OriginService != "http://hermes-fleet-instance-beta-dashboard:9119" {
		t.Fatalf("second route=%+v", route)
	}
}

func TestManagedPublishingZoneIsVisibleWithoutSecrets(t *testing.T) {
	const connectorToken = "eyJhIjoiYWNjb3VudCIsInQiOiIyMjIyMjIyMi0yMjIyLTQyMjItODIyMi0yMjIyMjIyMjIyMjIiLCJzIjoic2VjcmV0In0"
	managed, err := cloudflare.New(cloudflare.Config{
		InstancesTunnelToken: connectorToken,
		RouteAutomation: cloudflare.RouteAutomationConfig{
			AccountID: "account-a", ZoneID: "zone-a", ZoneName: "EXAMPLE.com", FleetNamespace: "andes", APIToken: "api-token-secret",
		},
	}, func(context.Context) ([]domain.Instance, error) { return nil, nil }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(managed, func(context.Context) ([]domain.Instance, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	view := manager.Configuration(context.Background())
	if !view.InstancePublishingConfigured || view.InstancePublishingZone != "example.com" || view.InstancePublishingFleetNamespace != "andes" {
		t.Fatalf("publishing metadata=%+v", view)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), connectorToken) || strings.Contains(string(encoded), "api-token-secret") {
		t.Fatalf("configuration view exposed secrets: %s", encoded)
	}
}

func TestDecodeConfigMigratesLegacyCloudflarePayload(t *testing.T) {
	payload, err := json.Marshal(cloudflare.Config{AccountID: "account", AdminHostname: "admin.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	config, err := DecodeConfig(payload)
	if err != nil {
		t.Fatal(err)
	}
	if config.Mode != ModeManagedCloudflare || config.Cloudflare.AccountID != "account" {
		t.Fatalf("decoded config=%+v", config)
	}
}

func newTestManager(t *testing.T, source InstanceSource) *Manager {
	t.Helper()
	managed, err := cloudflare.New(cloudflare.Config{}, cloudflare.InstanceSource(source), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(managed, source)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}
