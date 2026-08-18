package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

const (
	testAdminTunnelID     = "11111111-1111-4111-8111-111111111111"
	testInstancesTunnelID = "22222222-2222-4222-8222-222222222222"
	testInstancesToken    = "eyJhIjoiYWNjb3VudCIsInQiOiIyMjIyMjIyMi0yMjIyLTQyMjItODIyMi0yMjIyMjIyMjIyMjIiLCJzIjoic2VjcmV0In0"
)

type recordedRequest struct {
	Method string
	Path   string
	Token  string
	Body   map[string]any
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type memoryOwnershipStore struct {
	resources []domain.RemoteAccessResource
}

func (store *memoryOwnershipStore) ListRemoteAccessResources(context.Context) ([]domain.RemoteAccessResource, error) {
	return append([]domain.RemoteAccessResource(nil), store.resources...), nil
}

func (store *memoryOwnershipStore) PutRemoteAccessResource(_ context.Context, resource domain.RemoteAccessResource) error {
	for index, current := range store.resources {
		if current.InstanceID == resource.InstanceID && current.Kind == resource.Kind && current.Hostname == resource.Hostname {
			store.resources[index] = resource
			return nil
		}
	}
	store.resources = append(store.resources, resource)
	return nil
}

func (store *memoryOwnershipStore) DeleteRemoteAccessResource(_ context.Context, instanceID, kind, hostname string) error {
	filtered := store.resources[:0]
	for _, resource := range store.resources {
		if resource.InstanceID == instanceID && resource.Kind == kind && resource.Hostname == hostname {
			continue
		}
		filtered = append(filtered, resource)
	}
	store.resources = filtered
	return nil
}

func TestBuildInstancePublicHostname(t *testing.T) {
	hostname, err := BuildInstancePublicHostname(" Andes ", "Test01", "EXAMPLE.COM.")
	if err != nil {
		t.Fatal(err)
	}
	if hostname != "andes-test01.example.com" {
		t.Fatalf("hostname=%q", hostname)
	}
	for _, test := range []struct {
		name      string
		namespace string
		instance  string
		zone      string
	}{
		{name: "empty namespace", instance: "test01", zone: "example.com"},
		{name: "invalid namespace", namespace: "andes_1", instance: "test01", zone: "example.com"},
		{name: "invalid instance", namespace: "andes", instance: "test_01", zone: "example.com"},
		{name: "missing zone", namespace: "andes", instance: "test01"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, buildErr := BuildInstancePublicHostname(test.namespace, test.instance, test.zone); buildErr == nil {
				t.Fatal("expected hostname validation error")
			}
		})
	}
}

func TestInstancePublishingVerifiesExplicitHostnameAndRecordsOwnership(t *testing.T) {
	var ingress []map[string]any
	var dnsRecords []dnsRecord
	version := 1
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		respond := func(status int, payload string) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload)), Request: request}, nil
		}
		if request.URL.Host == "aksa.example.com" {
			return respond(http.StatusOK, "dashboard ready")
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/zones/zone-a":
			return respond(http.StatusOK, `{"success":true,"result":{"id":"zone-a","name":"example.com","status":"active","account":{"id":"account-a"}}}`)
		case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/configurations"):
			payload, _ := json.Marshal(map[string]any{"success": true, "result": map[string]any{
				"config": map[string]any{"ingress": ingress, "originRequest": map[string]any{}}, "source": "cloudflare", "version": version,
			}})
			return respond(http.StatusOK, string(payload))
		case request.Method == http.MethodPut && strings.Contains(request.URL.Path, "/configurations"):
			var body struct {
				Config struct {
					Ingress []map[string]any `json:"ingress"`
				} `json:"config"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				return nil, err
			}
			if len(body.Config.Ingress) == 0 {
				return respond(http.StatusBadRequest, `{"success":false,"errors":[{"message":"Bad Configuration: Validation failed: The config file doesn't contain any ingress rules"}]}`)
			}
			ingress = body.Config.Ingress
			version++
			return respond(http.StatusOK, `{"success":true,"result":{}}`)
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/dns_records"):
			payload, _ := json.Marshal(map[string]any{"success": true, "result": dnsRecords})
			return respond(http.StatusOK, string(payload))
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/dns_records"):
			var record dnsRecord
			if err := json.NewDecoder(request.Body).Decode(&record); err != nil {
				return nil, err
			}
			record.ID = "dns-aksa"
			dnsRecords = append(dnsRecords, record)
			payload, _ := json.Marshal(map[string]any{"success": true, "result": record})
			return respond(http.StatusOK, string(payload))
		default:
			return respond(http.StatusNotFound, `{"success":false,"errors":[{"message":"unexpected request"}]}`)
		}
	})
	client := &http.Client{Transport: transport}
	config := Config{
		APIBaseURL: "https://api.example.test", InstancesTunnelToken: testInstancesToken,
		InstancesConnectorTokenPath: filepath.Join(t.TempDir(), "instances", "token"),
		RouteAutomation:             RouteAutomationConfig{AccountID: "account-a", ZoneID: "zone-a", APIToken: "api-token"},
	}
	manager, err := New(config, func(context.Context) ([]domain.Instance, error) {
		return []domain.Instance{{ID: "instance-aksa", Name: "aksa", PublicHostname: "aksa.example.com", Status: domain.InstanceRunning}}, nil
	}, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	ownership := &memoryOwnershipStore{}
	manager.SetOwnershipStore(ownership)
	if tunnelID, zoneName, err := manager.VerifyInstancePublishing(context.Background(), config); err != nil || tunnelID != testInstancesTunnelID || zoneName != "example.com" {
		t.Fatalf("verify tunnel=%q zone=%q err=%v", tunnelID, zoneName, err)
	}
	if len(ingress) != 1 || ingress[0]["service"] != "http_status:404" || ingress[0]["hostname"] != nil {
		t.Fatalf("empty tunnel was not initialized with a safe catch-all: %+v", ingress)
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	observation := manager.RouteObservations()["aksa.example.com"]
	if observation.ProviderState != RouteProviderPublished || observation.DNSState != ResourceReady ||
		observation.IngressState != ResourceReady || observation.EndpointState != EndpointReachable {
		t.Fatalf("publication observation=%+v", observation)
	}
	if got := manager.PublicDashboardURL("aksa.example.com"); got != "https://aksa.example.com" {
		t.Fatalf("public dashboard URL=%q", got)
	}
	if len(ownership.resources) != 2 {
		t.Fatalf("ownership resources=%+v", ownership.resources)
	}
	viewJSON, err := json.Marshal(manager.Configuration())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(viewJSON), "api-token") || strings.Contains(string(viewJSON), testInstancesToken) {
		t.Fatalf("configuration view exposed a secret: %s", viewJSON)
	}
	if manager.Configuration().RouteAutomationZoneName != "example.com" {
		t.Fatalf("verified publishing zone=%q", manager.Configuration().RouteAutomationZoneName)
	}
}

func TestInstancePublishingRetainsOwnershipUntilRemovalIsVerified(t *testing.T) {
	var mu sync.Mutex
	sticky := true
	dnsRecords := []dnsRecord{{
		ID: "dns-deleting", Name: "deleting.example.com", Type: "CNAME",
		Content: testInstancesTunnelID + ".cfargotunnel.com", Proxied: true, Comment: managedDNSComment,
	}}
	ingress := []map[string]any{
		{"hostname": "deleting.example.com", "service": "http://hermes-fleet-instance-deleting-dashboard:9119"},
		{"service": "http_status:404"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		writeResult := func(result any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result})
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/zones/zone-a":
			writeResult(map[string]any{"id": "zone-a", "name": "example.com", "status": "active", "account": map[string]any{"id": "account-a"}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/dns_records"):
			writeResult(dnsRecords)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/dns_records/"):
			if !sticky {
				dnsRecords = nil
			}
			writeResult(map[string]any{})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/configurations"):
			writeResult(map[string]any{"config": map[string]any{"ingress": ingress, "originRequest": map[string]any{}}, "source": "cloudflare", "version": 1})
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/configurations"):
			if !sticky {
				ingress = []map[string]any{{"service": "http_status:404"}}
			}
			writeResult(map[string]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"success":false,"errors":[{"message":"unexpected request"}]}`))
		}
	}))
	defer server.Close()

	config := Config{
		APIBaseURL: server.URL, InstancesTunnelToken: testInstancesToken,
		InstancesConnectorTokenPath: filepath.Join(t.TempDir(), "instances", "token"),
		RouteAutomation: RouteAutomationConfig{
			AccountID: "account-a", ZoneID: "zone-a", APIToken: "api-token", TunnelID: testInstancesTunnelID,
		},
	}
	manager, err := New(config, func(context.Context) ([]domain.Instance, error) {
		return []domain.Instance{{
			ID: "instance-deleting", Name: "deleting", PublicHostname: "deleting.example.com", Status: domain.InstanceDeleting,
		}}, nil
	}, server.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ownership := &memoryOwnershipStore{resources: []domain.RemoteAccessResource{
		{InstanceID: "instance-deleting", Kind: "dns", ResourceID: "dns-deleting", Hostname: "deleting.example.com", TunnelID: testInstancesTunnelID, ZoneID: "zone-a"},
		{InstanceID: "instance-deleting", Kind: "ingress", Hostname: "deleting.example.com", TunnelID: testInstancesTunnelID, ZoneID: "zone-a", OriginService: "http://hermes-fleet-instance-deleting-dashboard:9119"},
	}}
	manager.SetOwnershipStore(ownership)

	if _, err := manager.reconcileAutomatedInstanceRoutes(context.Background(), config, map[string]string{}); err == nil || !strings.Contains(err.Error(), "still present") {
		t.Fatalf("sticky Cloudflare removal error=%v", err)
	}
	if len(ownership.resources) != 2 {
		t.Fatalf("ownership was discarded before provider verification: %+v", ownership.resources)
	}

	mu.Lock()
	sticky = false
	mu.Unlock()
	if _, err := manager.reconcileAutomatedInstanceRoutes(context.Background(), config, map[string]string{}); err != nil {
		t.Fatalf("verified Cloudflare removal: %v", err)
	}
	if len(ownership.resources) != 0 {
		t.Fatalf("verified stale ownership remains: %+v", ownership.resources)
	}
}

func TestManagerReconcilesDedicatedTunnelBoundaries(t *testing.T) {
	var mu sync.Mutex
	requests := make([]recordedRequest, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			data, _ := io.ReadAll(r.Body)
			if len(data) > 0 {
				_ = json.Unmarshal(data, &body)
			}
		}
		mu.Lock()
		requests = append(requests, recordedRequest{Method: r.Method, Path: r.URL.RequestURI(), Token: r.Header.Get("Authorization"), Body: body})
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/configurations"):
			_, _ = w.Write([]byte(`{"success":true,"result":{"config":{"ingress":[{"service":"http_status:404"}]},"source":"cloudflare","version":7}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/dns_records"):
			_, _ = w.Write([]byte(`{"success":true,"result":[]}`))
		default:
			_, _ = w.Write([]byte(`{"success":true,"result":{}}`))
		}
	}))
	defer server.Close()

	manager, err := New(testConfig(server.URL), func(context.Context) ([]domain.Instance, error) {
		return []domain.Instance{
			{Name: "fleet-a", PublicHostname: "fleet-a.example.com", Status: domain.InstanceRunning},
			{Name: "fleet-b", PublicHostname: "fleet-b.example.com", Status: domain.InstanceProvisioning},
			{Name: "deleted", Status: domain.InstanceDeleted},
		}, nil
	}, server.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := manager.Status()
	if status.State != "synced" || !status.Admin.Synced || status.Admin.Routes != 1 || !status.Instances.Synced || status.Instances.Routes != 2 {
		t.Fatalf("status=%+v", status)
	}
	if got := manager.PublicDashboardURL("fleet-a"); got != "" {
		t.Fatalf("public URL=%q", got)
	}

	mu.Lock()
	defer mu.Unlock()
	var adminPut, instancesPut *recordedRequest
	adminDNSCreates, instanceDNSCreates := 0, 0
	for index := range requests {
		request := &requests[index]
		switch {
		case request.Method == http.MethodPut && strings.Contains(request.Path, "/"+testAdminTunnelID+"/"):
			adminPut = request
		case request.Method == http.MethodPut && strings.Contains(request.Path, "/"+testInstancesTunnelID+"/"):
			instancesPut = request
		case request.Method == http.MethodPost && request.Token == "Bearer admin-api-token":
			adminDNSCreates++
		case request.Method == http.MethodPost && request.Token == "Bearer instances-api-token":
			instanceDNSCreates++
		}
	}
	if adminPut == nil || adminPut.Token != "Bearer admin-api-token" {
		t.Fatalf("admin PUT=%+v", adminPut)
	}
	if instancesPut == nil || instancesPut.Token != "Bearer instances-api-token" {
		t.Fatalf("instances PUT=%+v", instancesPut)
	}
	if !strings.Contains(adminPut.Path, "version=7") || !strings.Contains(instancesPut.Path, "version=7") {
		t.Fatal("tunnel updates do not use optimistic configuration versions")
	}
	assertIngress(t, adminPut.Body, map[string]string{
		"admin.example.com": "http://control-plane:9180",
	})
	assertIngress(t, instancesPut.Body, map[string]string{
		"fleet-a.example.com": "http://hermes-fleet-instance-fleet-a-dashboard:9119",
		"fleet-b.example.com": "http://hermes-fleet-instance-fleet-b-dashboard:9119",
	})
	if adminDNSCreates != 1 || instanceDNSCreates != 2 {
		t.Fatalf("DNS creates admin=%d instances=%d", adminDNSCreates, instanceDNSCreates)
	}
}

func TestManagerReportsConnectorReadinessSeparatelyFromRouteSynchronization(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/configurations"):
			_, _ = w.Write([]byte(`{"success":true,"result":{"config":{"ingress":[{"service":"http_status:404"}]},"source":"cloudflare","version":1}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/dns_records"):
			_, _ = w.Write([]byte(`{"success":true,"result":[]}`))
		default:
			_, _ = w.Write([]byte(`{"success":true,"result":{}}`))
		}
	}))
	defer apiServer.Close()

	var connectorsReady atomic.Bool
	healthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !connectorsReady.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"state":"starting","ready":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"state":"running","ready":true}`))
	}))
	defer healthServer.Close()

	config := testConfig(apiServer.URL)
	config.AdminConnectorHealthURL = healthServer.URL + "/admin"
	config.InstancesConnectorHealthURL = healthServer.URL + "/instances"
	manager, err := New(config, func(context.Context) ([]domain.Instance, error) { return nil, nil }, apiServer.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := manager.Status()
	if status.State != "syncing" || !status.Admin.Synced || !status.Instances.Synced {
		t.Fatalf("route synchronization must remain distinct from connector readiness: %+v", status)
	}
	if status.Admin.ConnectorState != "starting" || status.Instances.ConnectorState != "starting" {
		t.Fatalf("connector states=%+v/%+v", status.Admin, status.Instances)
	}

	connectorsReady.Store(true)
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	status = manager.Status()
	if status.State != "synced" || status.Admin.ConnectorState != "running" || status.Instances.ConnectorState != "running" || status.LastError != "" {
		t.Fatalf("ready connector status=%+v", status)
	}
}

func TestRoutineRevalidationPreservesLastSyncedGlobalStatus(t *testing.T) {
	manager := &Manager{status: Status{
		Configured: true, State: "synced", LastSyncAt: time.Now().UTC().Format(time.RFC3339),
		Admin: BoundaryStatus{Synced: true}, Instances: BoundaryStatus{Synced: true},
	}}
	manager.setSyncing()
	if status := manager.Status(); status.State != "synced" {
		t.Fatalf("routine revalidation replaced last-known synced status: %+v", status)
	}

	manager.mu.Lock()
	manager.status.State = "pending"
	manager.mu.Unlock()
	manager.setSyncing()
	if status := manager.Status(); status.State != "syncing" {
		t.Fatalf("unverified configuration did not enter syncing: %+v", status)
	}
}

func TestAdminStatusIncludesConnectorAndPublicEndpointChecks(t *testing.T) {
	runtimeDirectory := t.TempDir()
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "connector.test":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"state":"running","ready":true}`)),
			}, nil
		case "admin.example.com":
			header := make(http.Header)
			header.Set("Location", "https://team.cloudflareaccess.com/cdn-cgi/access/login")
			return &http.Response{StatusCode: http.StatusFound, Header: header, Body: io.NopCloser(strings.NewReader(""))}, nil
		default:
			return nil, fmt.Errorf("unexpected request to %s", request.URL.String())
		}
	})}
	manager, err := New(Config{
		AdminConnectorTokenPath: filepath.Join(runtimeDirectory, "admin", "token"),
		AdminConnectorHealthURL: "http://connector.test/health",
	}, func(context.Context) ([]domain.Instance, error) { return nil, nil }, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Configure(context.Background(), Config{
		AdminTunnelToken: "eyJ-admin-boundary-token-that-is-long-enough",
		AdminHostname:    "admin.example.com",
	}); err != nil {
		t.Fatal(err)
	}
	status := manager.Status()
	if status.Admin.ConnectorState != "running" || status.Admin.EndpointState != EndpointAccessProtected || status.Admin.EndpointCheckedAt == "" {
		t.Fatalf("admin boundary status=%+v", status.Admin)
	}
	if !strings.Contains(status.Admin.EndpointDetail, "origin was not verified") {
		t.Fatalf("admin endpoint detail=%q", status.Admin.EndpointDetail)
	}
}

func TestManagerRejectsPartialOrSharedConfiguration(t *testing.T) {
	partial := Config{AccountID: "account"}
	if _, err := New(partial, nil, nil, nil); err == nil {
		t.Fatal("partial configuration was accepted")
	}
	shared := testConfig("https://api.example.test")
	shared.InstancesAPIToken = shared.AdminAPIToken
	if _, err := New(shared, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "must be different") {
		t.Fatalf("shared API token error=%v", err)
	}
	if _, err := New(testConfig("https://api.example.test"), nil, nil, nil); err == nil || !strings.Contains(err.Error(), "instance source") {
		t.Fatalf("missing instance source error=%v", err)
	}
	connectorToken := testConfig("https://api.example.test")
	connectorToken.AdminTunnelID = "eyJhIjoiZXhhbXBsZSIsInQiOiJub3QtYS10dW5uZWwtaWQifQ"
	if _, err := New(connectorToken, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "canonical UUIDs, not connector tokens") {
		t.Fatalf("connector token error=%v", err)
	}
	directTokens := Config{
		AdminTunnelToken:     "eyJ-shared-connector-token-that-is-long-enough",
		InstancesTunnelToken: "eyJ-shared-connector-token-that-is-long-enough",
		AdminHostname:        "admin.example.com",
	}
	if _, err := New(directTokens, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "must be different") {
		t.Fatalf("shared connector token error=%v", err)
	}
	partialAutomation := Config{RouteAutomation: RouteAutomationConfig{AccountID: "account"}}
	if _, err := New(partialAutomation, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "account ID, zone ID, tunnel ID, and API token") {
		t.Fatalf("partial route automation error=%v", err)
	}
	invalidAutomation := Config{
		InstancesTunnelToken: testInstancesToken,
		RouteAutomation: RouteAutomationConfig{
			AccountID: "account", ZoneID: "zone", TunnelID: "not-a-uuid", APIToken: "api-token",
		},
	}
	if manager, err := New(invalidAutomation, func(context.Context) ([]domain.Instance, error) { return nil, nil }, nil, nil); err != nil || manager.Config().RouteAutomation.TunnelID != testInstancesTunnelID {
		t.Fatalf("derived route automation tunnel error=%v config=%+v", err, manager)
	}
	automationWithoutConnector := Config{RouteAutomation: RouteAutomationConfig{
		AccountID: "account", ZoneID: "zone", TunnelID: testInstancesTunnelID, APIToken: "api-token",
	}}
	if _, err := New(automationWithoutConnector, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "tunnel-token remote access") {
		t.Fatalf("route automation without connector error=%v", err)
	}
	disabled, err := New(Config{}, nil, nil, nil)
	if err != nil || disabled.Status().Configured || disabled.Status().State != "disabled" {
		t.Fatalf("disabled status=%+v err=%v", disabled.Status(), err)
	}
}

func TestManagerConfiguresRuntimeDirectlyFromConnectorTokens(t *testing.T) {
	const (
		adminToken     = "eyJ-admin-connector-token-that-is-long-enough"
		instancesToken = testInstancesToken
	)
	runtimeDirectory := t.TempDir()
	adminTokenPath := filepath.Join(runtimeDirectory, "admin", "token")
	instancesTokenPath := filepath.Join(runtimeDirectory, "instances", "token")
	adminConfigPath := filepath.Join(runtimeDirectory, "admin", "config.yml")
	instancesConfigPath := filepath.Join(runtimeDirectory, "instances", "config.yml")
	for _, path := range []string{adminConfigPath, instancesConfigPath} {
		if err := writeConnectorRuntime(path, []byte("legacy local configuration")); err != nil {
			t.Fatal(err)
		}
	}
	var providerRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerRequests.Add(1)
		http.Error(w, "provider API must not be called", http.StatusInternalServerError)
	}))
	defer server.Close()

	manager, err := New(Config{
		APIBaseURL:               server.URL,
		AdminConnectorConfigPath: adminConfigPath, InstancesConnectorConfigPath: instancesConfigPath,
		AdminConnectorTokenPath: adminTokenPath, InstancesConnectorTokenPath: instancesTokenPath,
	}, func(context.Context) ([]domain.Instance, error) {
		return []domain.Instance{
			{Name: "alpha", PublicHostname: "alpha.example.com", Status: domain.InstanceRunning},
			{Name: "beta", PublicHostname: "beta.example.com", Status: domain.InstanceStopped},
			{Name: "deleted", Status: domain.InstanceDeleted},
		}, nil
	}, server.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{
		AdminTunnelToken: adminToken, InstancesTunnelToken: instancesToken,
		AdminHostname: "admin.example.com",
	}
	if err := manager.Configure(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if providerRequests.Load() != 0 {
		t.Fatalf("direct token configuration made %d provider API requests", providerRequests.Load())
	}
	for path, expected := range map[string]string{adminTokenPath: adminToken, instancesTokenPath: instancesToken} {
		data, readErr := os.ReadFile(path)
		if readErr != nil || strings.TrimSpace(string(data)) != expected {
			t.Fatalf("connector token %q=%q err=%v", path, data, readErr)
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("connector token %q mode=%o, want 600", path, info.Mode().Perm())
		}
	}
	for _, path := range []string{adminConfigPath, instancesConfigPath} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("legacy connector configuration remains at %q: %v", path, statErr)
		}
	}
	view := manager.Configuration()
	if !view.AdminTunnelTokenConfigured || !view.InstancesTunnelTokenConfigured || view.AdminTunnelID != "" || view.LegacyProviderManaged {
		t.Fatalf("configuration view=%+v", view)
	}
	if view.AdminTunnelTokenFingerprint != tunnelTokenFingerprint(adminToken) ||
		view.InstancesTunnelTokenFingerprint != tunnelTokenFingerprint(instancesToken) {
		t.Fatalf("configuration fingerprints=%+v", view)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), adminToken) || strings.Contains(string(encoded), instancesToken) {
		t.Fatalf("configuration view exposed a connector token: %s", encoded)
	}
	status := manager.Status()
	if status.State != "synced" || status.Instances.Routes != 2 {
		t.Fatalf("status=%+v", status)
	}
	if got := manager.PublicDashboardURL("alpha"); got != "" {
		t.Fatalf("public dashboard URL=%q", got)
	}
	observations := manager.RouteObservations()
	if len(observations) != 2 || observations["alpha.example.com"].ProviderState != RouteProviderReady || observations["beta.example.com"].ProviderState != RouteProviderReady {
		t.Fatalf("route observations=%+v", observations)
	}
	if err := manager.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{adminTokenPath, instancesTokenPath} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("connector token remains after disable at %q: %v", path, statErr)
		}
	}
}

func TestManagerConfiguresConnectorTokenBoundariesIndependently(t *testing.T) {
	const (
		adminToken     = "eyJ-admin-boundary-token-that-is-long-enough"
		instancesToken = testInstancesToken
	)
	runtimeDirectory := t.TempDir()
	adminTokenPath := filepath.Join(runtimeDirectory, "admin", "token")
	instancesTokenPath := filepath.Join(runtimeDirectory, "instances", "token")
	manager, err := New(Config{
		AdminConnectorTokenPath:     adminTokenPath,
		InstancesConnectorTokenPath: instancesTokenPath,
	}, func(context.Context) ([]domain.Instance, error) {
		return []domain.Instance{{Name: "alpha", PublicHostname: "alpha.example.com", Status: domain.InstanceRunning}}, nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	adminConfig := Config{AdminTunnelToken: adminToken, AdminHostname: "admin.example.com"}
	if err := manager.Configure(context.Background(), adminConfig); err != nil {
		t.Fatal(err)
	}
	status := manager.Status()
	if status.State != "pending" || !status.Admin.Synced || status.Instances.Synced || status.Admin.Routes != 1 || status.Instances.Routes != 0 {
		t.Fatalf("partial status=%+v", status)
	}
	if data, readErr := os.ReadFile(adminTokenPath); readErr != nil || strings.TrimSpace(string(data)) != adminToken {
		t.Fatalf("admin token=%q err=%v", data, readErr)
	}
	if _, statErr := os.Stat(instancesTokenPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("instance token unexpectedly exists: %v", statErr)
	}
	if got := manager.PublicDashboardURL("alpha"); got != "" {
		t.Fatalf("partial public URL=%q", got)
	}

	completeConfig := adminConfig
	completeConfig.InstancesTunnelToken = instancesToken
	if err := manager.Configure(context.Background(), completeConfig); err != nil {
		t.Fatal(err)
	}
	status = manager.Status()
	if status.State != "synced" || !status.Admin.Synced || !status.Instances.Synced || status.Instances.Routes != 1 {
		t.Fatalf("complete status=%+v", status)
	}
	if data, readErr := os.ReadFile(instancesTokenPath); readErr != nil || strings.TrimSpace(string(data)) != instancesToken {
		t.Fatalf("instance token=%q err=%v", data, readErr)
	}
	if got := manager.PublicDashboardURL("alpha"); got != "" {
		t.Fatalf("complete public URL=%q", got)
	}
	observations := manager.RouteObservations()
	if len(observations) != 1 || observations["alpha.example.com"].ProviderState != RouteProviderReady {
		t.Fatalf("route observations=%+v", observations)
	}
}

func TestMergeManagedIngressPreservesUnrelatedAndPrunesStaleFleetRoutes(t *testing.T) {
	current := []map[string]any{
		{"hostname": "unrelated.example.net", "service": "http://other:8080"},
		{"hostname": "docs.hermes.example.com", "service": "http://documentation:8080"},
		{"hostname": "old.hermes.example.com", "service": "http://hermes-fleet-instance-old-dashboard:9119"},
		{"hostname": "alpha.hermes.example.com", "service": "http://old-alpha:9119"},
		{"service": "http_status:404"},
	}
	desired := map[string]string{
		"alpha.hermes.example.com": "http://alpha:9119",
		"beta.hermes.example.com":  "http://beta:9119",
	}
	merged := mergeManagedIngress(current, desired)
	if !ingressContains(merged, "unrelated.example.net", "http://other:8080") {
		t.Fatalf("unrelated ingress was not preserved: %+v", merged)
	}
	if !ingressContains(merged, "docs.hermes.example.com", "http://documentation:8080") {
		t.Fatalf("same-domain non-Fleet ingress was not preserved: %+v", merged)
	}
	if !ingressContains(merged, "alpha.hermes.example.com", "http://alpha:9119") || !ingressContains(merged, "beta.hermes.example.com", "http://beta:9119") {
		t.Fatalf("desired ingress routes were not reconciled: %+v", merged)
	}
	if !ingressContains(merged, "old.hermes.example.com", "http://hermes-fleet-instance-old-dashboard:9119") {
		t.Fatalf("unowned stale ingress was removed: %+v", merged)
	}
	if service, _ := merged[len(merged)-1]["service"].(string); service != "http_status:404" {
		t.Fatalf("fallback ingress is not last: %+v", merged)
	}
}

func TestPublicEndpointFallsBackToGetWhenHeadIsNotAllowed(t *testing.T) {
	var methods []string
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		methods = append(methods, request.Method)
		status := http.StatusMethodNotAllowed
		if request.Method == http.MethodGet {
			status = http.StatusOK
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}, nil
	})}
	manager := &Manager{client: client}
	state, detail := manager.checkPublicEndpoint(context.Background(), "alpha.hermes.example.com")
	if state != EndpointReachable || !strings.Contains(detail, "HTTP 200") {
		t.Fatalf("endpoint state=%q detail=%q", state, detail)
	}
	if got := strings.Join(methods, ","); got != "HEAD,GET" {
		t.Fatalf("request methods=%q", got)
	}
}

func TestPublishedObservationRemainsAvailableWhileRevalidating(t *testing.T) {
	routes := map[string]string{
		"alpha.hermes.example.com": "http://alpha:9119",
		"beta.hermes.example.com":  "http://beta:9119",
	}
	previous := map[string]RouteObservation{
		"alpha.hermes.example.com": {
			Hostname: "alpha.hermes.example.com", OriginService: "http://alpha:9119",
			ProviderState: RouteProviderPublished, DNSState: ResourceReady,
			IngressState: ResourceReady, EndpointState: EndpointReachable,
		},
	}

	observations := revalidatingRouteObservations(routes, previous)
	alpha := observations["alpha.hermes.example.com"]
	if !alpha.Revalidating || alpha.ProviderState != RouteProviderPublishing ||
		alpha.DNSState != ResourceReady || alpha.IngressState != ResourceReady || alpha.EndpointState != EndpointReachable {
		t.Fatalf("published observation was not preserved during revalidation: %+v", alpha)
	}
	beta := observations["beta.hermes.example.com"]
	if beta.Revalidating || beta.ProviderState != RouteProviderPublishing ||
		beta.DNSState != ResourcePending || beta.IngressState != ResourcePending || beta.EndpointState != EndpointUnchecked {
		t.Fatalf("new route did not start pending: %+v", beta)
	}
}

func TestPublicEndpointRetriesTransientResolutionFailure(t *testing.T) {
	var attempts int
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("dial tcp: lookup alpha.hermes.example.com: no such host")
		}
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}, nil
	})}
	manager := &Manager{client: client}
	state, detail := manager.checkPublicEndpointWithPolicy(
		context.Background(), "alpha.hermes.example.com", 100*time.Millisecond, time.Millisecond,
	)
	if state != EndpointReachable || !strings.Contains(detail, "HTTP 302") {
		t.Fatalf("endpoint state=%q detail=%q", state, detail)
	}
	if attempts != 2 {
		t.Fatalf("endpoint attempts=%d, want 2", attempts)
	}
}

func TestPublicEndpointReportsDNSPropagationWithoutRawResolverError(t *testing.T) {
	var attempts int
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		attempts++
		return nil, &net.DNSError{Err: "no such host", Name: request.URL.Hostname(), IsNotFound: true}
	})}
	manager := &Manager{client: client}
	state, detail := manager.checkPublicEndpointWithPolicy(
		context.Background(), "alpha.hermes.example.com", 10*time.Millisecond, time.Millisecond,
	)
	if state != EndpointPropagating || detail != "Public DNS is still propagating; Fleet will verify it automatically" {
		t.Fatalf("endpoint state=%q detail=%q", state, detail)
	}
	if attempts < 2 {
		t.Fatalf("endpoint attempts=%d, want retries during propagation", attempts)
	}
}

func TestPublicEndpointUsesDedicatedResolverClient(t *testing.T) {
	apiRequests := 0
	endpointRequests := 0
	apiClient := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		apiRequests++
		return nil, errors.New("stale host resolver")
	})}
	endpointClient := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		endpointRequests++
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}, nil
	})}
	manager := &Manager{client: apiClient, endpointClient: endpointClient}
	state, detail := manager.checkPublicEndpoint(context.Background(), "alpha.hermes.example.com")
	if state != EndpointReachable || !strings.Contains(detail, "HTTP 302") {
		t.Fatalf("endpoint state=%q detail=%q", state, detail)
	}
	if apiRequests != 0 || endpointRequests != 1 {
		t.Fatalf("api requests=%d endpoint requests=%d", apiRequests, endpointRequests)
	}
}

func TestEnsureConnectorTokenRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	targetPath := filepath.Join(directory, "target")
	tokenPath := filepath.Join(directory, "token")
	if err := os.WriteFile(targetPath, []byte("unchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetPath, tokenPath); err != nil {
		t.Fatal(err)
	}
	if err := ensureConnectorToken(tokenPath, "eyJ-new-connector-token-that-is-long-enough"); err == nil || !strings.Contains(err.Error(), "not a symlink") {
		t.Fatalf("ensureConnectorToken() error=%v, want symlink rejection", err)
	}
	contents, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "unchanged\n" {
		t.Fatalf("symlink target was changed: %q", contents)
	}
}

func TestEnsureConnectorTokenRepairsPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	token := "eyJ-existing-connector-token-that-is-long-enough"
	if err := os.WriteFile(path, []byte(token+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureConnectorToken(path, token); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("connector token mode=%o, want 600", info.Mode().Perm())
	}
}

func TestManagerConfiguresRuntimeFromCloudflareWithoutExposingTokens(t *testing.T) {
	runtimeDirectory := t.TempDir()
	adminTokenPath := filepath.Join(runtimeDirectory, "admin", "token")
	instancesTokenPath := filepath.Join(runtimeDirectory, "instances", "token")
	var remoteMutations atomic.Int32
	var mutationSawPublishedToken atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/token"):
			if strings.Contains(r.URL.Path, "/"+testAdminTunnelID+"/") {
				_, _ = w.Write([]byte(`{"success":true,"result":"admin-connector-token"}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"result":"instances-connector-token"}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/configurations"):
			_, _ = w.Write([]byte(`{"success":true,"result":{"config":{"ingress":[{"service":"http_status:404"}]},"source":"cloudflare","version":1}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/dns_records"):
			_, _ = w.Write([]byte(`{"success":true,"result":[]}`))
		default:
			if r.Method == http.MethodPut || r.Method == http.MethodPost {
				remoteMutations.Add(1)
				for _, path := range []string{adminTokenPath, instancesTokenPath} {
					if _, err := os.Stat(path); err == nil {
						mutationSawPublishedToken.Store(true)
					}
				}
			}
			_, _ = w.Write([]byte(`{"success":true,"result":{}}`))
		}
	}))
	defer server.Close()

	manager, err := New(Config{
		APIBaseURL: server.URL, AdminConnectorTokenPath: adminTokenPath,
		InstancesConnectorTokenPath: instancesTokenPath,
	}, func(context.Context) ([]domain.Instance, error) { return nil, nil }, server.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	config := testConfig(server.URL)
	if err := manager.Configure(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if remoteMutations.Load() == 0 {
		t.Fatal("Configure returned before synchronizing the remote tunnel configuration")
	}
	if mutationSawPublishedToken.Load() {
		t.Fatal("connector token was published before the remote configuration was safe")
	}
	if status := manager.Status(); status.State != "synced" {
		t.Fatalf("status after Configure=%+v, want synced", status)
	}
	view := manager.Configuration()
	if !view.LegacyProviderManaged || view.AdminCredentialAvailable || view.InstancesCredentialAvailable {
		t.Fatalf("configuration view=%+v", view)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "api-token") || strings.Contains(string(encoded), "connector-token") {
		t.Fatalf("configuration view exposed a token: %s", encoded)
	}
	for path, want := range map[string]string{
		filepath.Join(runtimeDirectory, "admin", "token"):     "admin-connector-token\n",
		filepath.Join(runtimeDirectory, "instances", "token"): "instances-connector-token\n",
	} {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(contents) != want {
			t.Fatalf("connector token file %s=%q, want %q", path, contents, want)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("connector token file %s mode=%o, want 600", path, info.Mode().Perm())
		}
	}
}

func TestManagerWritesLocalIngressFromTunnelScopedCredentials(t *testing.T) {
	runtimeDirectory := t.TempDir()
	credentialsDirectory := filepath.Join(runtimeDirectory, "credentials")
	if err := os.MkdirAll(credentialsDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tunnelID := range []string{testAdminTunnelID, testInstancesTunnelID} {
		credential := []byte(`{"AccountTag":"account","TunnelSecret":"secret","TunnelID":"` + tunnelID + `"}`)
		if err := os.WriteFile(filepath.Join(credentialsDirectory, tunnelID+".json"), credential, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	adminConfigPath := filepath.Join(runtimeDirectory, "admin", "config.yml")
	instancesConfigPath := filepath.Join(runtimeDirectory, "instances", "config.yml")
	manager, err := New(Config{
		CredentialsDirectory:         credentialsDirectory,
		AdminConnectorConfigPath:     adminConfigPath,
		InstancesConnectorConfigPath: instancesConfigPath,
	}, func(context.Context) ([]domain.Instance, error) {
		return []domain.Instance{
			{Name: "beta", PublicHostname: "beta.example.com", Status: domain.InstanceRunning},
			{Name: "alpha", PublicHostname: "alpha.example.com", Status: domain.InstanceStopped},
			{Name: "deleted", Status: domain.InstanceDeleted},
		}, nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{
		AdminTunnelID: testAdminTunnelID, InstancesTunnelID: testInstancesTunnelID,
		AdminHostname: "admin.example.com",
	}
	if err := manager.Configure(context.Background(), config); err != nil {
		t.Fatal(err)
	}

	adminData, err := os.ReadFile(adminConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	instancesData, err := os.ReadFile(instancesConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	adminText := string(adminData)
	instancesText := string(instancesData)
	if !strings.Contains(adminText, `tunnel: "`+testAdminTunnelID+`"`) || !strings.Contains(adminText, `hostname: "admin.example.com"`) || !strings.Contains(adminText, `service: "http://control-plane:9180"`) {
		t.Fatalf("admin config=%s", adminText)
	}
	for _, expected := range []string{
		`tunnel: "` + testInstancesTunnelID + `"`,
		`hostname: "alpha.example.com"`,
		`hostname: "beta.example.com"`,
		`service: "http://hermes-fleet-instance-alpha-dashboard:9119"`,
	} {
		if !strings.Contains(instancesText, expected) {
			t.Fatalf("instance config missing %q: %s", expected, instancesText)
		}
	}
	if strings.Index(instancesText, "alpha.example.com") > strings.Index(instancesText, "beta.example.com") {
		t.Fatalf("instance ingress is not deterministic: %s", instancesText)
	}
	view := manager.Configuration()
	if !view.AdminCredentialAvailable || !view.InstancesCredentialAvailable || view.LegacyProviderManaged {
		t.Fatalf("configuration view=%+v", view)
	}
	if status := manager.Status(); status.State != "synced" || status.Instances.Routes != 2 {
		t.Fatalf("status=%+v", status)
	}
	if err := manager.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, configPath := range []string{adminConfigPath, instancesConfigPath} {
		if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("connector config remains after disable: %s err=%v", configPath, err)
		}
	}
	for _, tunnelID := range []string{testAdminTunnelID, testInstancesTunnelID} {
		if _, err := os.Stat(filepath.Join(credentialsDirectory, tunnelID+".json")); err != nil {
			t.Fatalf("disable removed host credential %s: %v", tunnelID, err)
		}
	}
}

func TestManagerRejectsMissingOrSymlinkedTunnelCredential(t *testing.T) {
	runtimeDirectory := t.TempDir()
	credentialsDirectory := filepath.Join(runtimeDirectory, "credentials")
	if err := os.MkdirAll(credentialsDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(runtimeDirectory, "outside.json")
	if err := os.WriteFile(outside, []byte(`{"AccountTag":"account","TunnelSecret":"secret","TunnelID":"`+testAdminTunnelID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(credentialsDirectory, testAdminTunnelID+".json")); err != nil {
		t.Fatal(err)
	}
	manager, err := New(Config{
		CredentialsDirectory:         credentialsDirectory,
		AdminConnectorConfigPath:     filepath.Join(runtimeDirectory, "admin", "config.yml"),
		InstancesConnectorConfigPath: filepath.Join(runtimeDirectory, "instances", "config.yml"),
	}, func(context.Context) ([]domain.Instance, error) { return nil, nil }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = manager.Configure(context.Background(), Config{
		AdminTunnelID: testAdminTunnelID, InstancesTunnelID: testInstancesTunnelID,
		AdminHostname: "admin.example.com",
	})
	if err == nil || !strings.Contains(err.Error(), "not a symlink") {
		t.Fatalf("Configure() error=%v, want symlink rejection", err)
	}
}

func TestManagerRequiresDisableBeforeChangingManagedBoundary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/token"):
			if strings.Contains(r.URL.Path, "/"+testAdminTunnelID+"/") {
				_, _ = w.Write([]byte(`{"success":true,"result":"admin-connector-token"}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"result":"instances-connector-token"}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/configurations"):
			_, _ = w.Write([]byte(`{"success":true,"result":{"config":{"ingress":[{"service":"http_status:404"}]},"source":"cloudflare","version":1}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/dns_records"):
			_, _ = w.Write([]byte(`{"success":true,"result":[]}`))
		default:
			_, _ = w.Write([]byte(`{"success":true,"result":{}}`))
		}
	}))
	defer server.Close()

	runtimeDirectory := t.TempDir()
	manager, err := New(Config{
		APIBaseURL: server.URL, AdminConnectorTokenPath: filepath.Join(runtimeDirectory, "admin", "token"),
		InstancesConnectorTokenPath: filepath.Join(runtimeDirectory, "instances", "token"),
	}, func(context.Context) ([]domain.Instance, error) { return nil, nil }, server.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	config := testConfig(server.URL)
	if err := manager.Configure(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	localMigration := Config{
		AdminTunnelID: config.AdminTunnelID, InstancesTunnelID: config.InstancesTunnelID,
		AdminHostname: config.AdminHostname,
	}
	if err := manager.Configure(context.Background(), localMigration); err == nil || !strings.Contains(err.Error(), "credential mode") {
		t.Fatalf("Configure() legacy migration error=%v", err)
	}
	changed := config
	changed.AdminHostname = "new-admin.example.com"
	if err := manager.Configure(context.Background(), changed); err == nil || !strings.Contains(err.Error(), "disable Cloudflare remote access before changing") {
		t.Fatalf("Configure() boundary change error=%v", err)
	}
	if view := manager.Configuration(); view.AdminHostname != config.AdminHostname {
		t.Fatalf("active boundary changed after rejection: %+v", view)
	}
}

func TestManagerRejectsSharedConnectorCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"result":"shared-connector-token"}`))
	}))
	defer server.Close()
	runtimeDirectory := t.TempDir()
	manager, err := New(Config{
		APIBaseURL: server.URL, AdminConnectorTokenPath: filepath.Join(runtimeDirectory, "admin", "token"),
		InstancesConnectorTokenPath: filepath.Join(runtimeDirectory, "instances", "token"),
	}, func(context.Context) ([]domain.Instance, error) { return nil, nil }, server.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Configure(context.Background(), testConfig(server.URL)); err == nil || !strings.Contains(err.Error(), "same connector token") {
		t.Fatalf("Configure() error=%v, want shared connector rejection", err)
	}
	if _, err := os.Stat(filepath.Join(runtimeDirectory, "admin", "token")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("admin token file exists after rejection: %v", err)
	}
}

func TestManagerDisableRevokesLocalConnectorTokensWhenCloudflareCleanupFails(t *testing.T) {
	var cleanupFails atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if cleanupFails.Load() {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"success":false,"errors":[{"message":"upstream unavailable"}]}`))
			return
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/token"):
			if strings.Contains(r.URL.Path, "/"+testAdminTunnelID+"/") {
				_, _ = w.Write([]byte(`{"success":true,"result":"admin-connector-token"}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"result":"instances-connector-token"}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/configurations"):
			_, _ = w.Write([]byte(`{"success":true,"result":{"config":{"ingress":[{"service":"http_status:404"}]},"source":"cloudflare","version":1}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/dns_records"):
			_, _ = w.Write([]byte(`{"success":true,"result":[]}`))
		default:
			_, _ = w.Write([]byte(`{"success":true,"result":{}}`))
		}
	}))
	defer server.Close()

	runtimeDirectory := t.TempDir()
	adminTokenPath := filepath.Join(runtimeDirectory, "admin", "token")
	instancesTokenPath := filepath.Join(runtimeDirectory, "instances", "token")
	manager, err := New(Config{
		APIBaseURL: server.URL, AdminConnectorTokenPath: adminTokenPath,
		InstancesConnectorTokenPath: instancesTokenPath,
	}, func(context.Context) ([]domain.Instance, error) { return nil, nil }, server.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Configure(context.Background(), testConfig(server.URL)); err != nil {
		t.Fatal(err)
	}
	cleanupFails.Store(true)
	if err := manager.Disable(context.Background()); err == nil || !strings.Contains(err.Error(), "disable admin tunnel") {
		t.Fatalf("Disable() error=%v, want remote cleanup failure", err)
	}
	if status := manager.Status(); !status.Configured || status.State != "cleanup_pending" || status.LastError == "" {
		t.Fatalf("status after failed cleanup=%+v, want retryable cleanup_pending", status)
	}
	for _, path := range []string{adminTokenPath, instancesTokenPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("connector token remains after disable at %s: %v", path, err)
		}
	}
	cleanupFails.Store(false)
	if err := manager.Disable(context.Background()); err != nil {
		t.Fatalf("Disable() retry error=%v", err)
	}
	if status := manager.Status(); status.Configured || status.State != "disabled" {
		t.Fatalf("status after successful cleanup retry=%+v", status)
	}
}

func TestManagerDoesNotOverwriteConflictingDNS(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/configurations"):
			_, _ = w.Write([]byte(`{"success":true,"result":{"config":{"ingress":[]},"source":"cloudflare","version":1}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/dns_records"):
			_, _ = w.Write([]byte(`{"success":true,"result":[{"id":"existing","name":"admin.example.com","type":"CNAME","content":"other.example.net","proxied":true}]}`))
		default:
			_, _ = w.Write([]byte(`{"success":true,"result":{}}`))
		}
	}))
	defer server.Close()
	manager, err := New(testConfig(server.URL), func(context.Context) ([]domain.Instance, error) { return nil, nil }, server.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = manager.Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "conflicts with an unmanaged record") {
		t.Fatalf("reconcile error=%v", err)
	}
	if manager.Status().State != "error" {
		t.Fatalf("status=%+v", manager.Status())
	}
}

func TestManagerRejectsSameTargetDNSWithoutFleetOwnership(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/configurations"):
			_, _ = w.Write([]byte(`{"success":true,"result":{"config":{"ingress":[]},"source":"cloudflare","version":1}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/dns_records"):
			_, _ = w.Write([]byte(`{"success":true,"result":[{"id":"existing","name":"admin.example.com","type":"CNAME","content":"` + testAdminTunnelID + `.cfargotunnel.com","proxied":true}]}`))
		default:
			_, _ = w.Write([]byte(`{"success":true,"result":{}}`))
		}
	}))
	defer server.Close()
	manager, err := New(testConfig(server.URL), func(context.Context) ([]domain.Instance, error) { return nil, nil }, server.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = manager.Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is not owned by Fleet") {
		t.Fatalf("reconcile error=%v, want ownership rejection", err)
	}
}

func testConfig(apiURL string) Config {
	return Config{
		AccountID: "account", ZoneID: "zone", AdminAPIToken: "admin-api-token",
		InstancesAPIToken: "instances-api-token", AdminTunnelID: testAdminTunnelID,
		InstancesTunnelID: testInstancesTunnelID, AdminHostname: "admin.example.com",
		AdminAccessTeam:     "admin-team",
		AdminAccessAudience: "admin-audience", InstancesAccessTeam: "instances-team",
		InstancesAccessAudience: "instances-audience", APIBaseURL: apiURL,
	}
}

func assertIngress(t *testing.T, body map[string]any, expected map[string]string) {
	t.Helper()
	config, ok := body["config"].(map[string]any)
	if !ok {
		t.Fatalf("config body=%+v", body)
	}
	ingress, ok := config["ingress"].([]any)
	if !ok || len(ingress) != len(expected)+1 {
		t.Fatalf("ingress=%+v", config["ingress"])
	}
	actual := make(map[string]string)
	for _, value := range ingress[:len(ingress)-1] {
		rule := value.(map[string]any)
		actual[rule["hostname"].(string)] = rule["service"].(string)
		origin := rule["originRequest"].(map[string]any)
		access := origin["access"].(map[string]any)
		if required, _ := access["required"].(bool); !required {
			t.Fatalf("Access is not required for %+v", rule)
		}
	}
	if fallback := ingress[len(ingress)-1].(map[string]any)["service"]; fallback != "http_status:404" {
		t.Fatalf("fallback=%v", fallback)
	}
	if len(actual) != len(expected) {
		t.Fatalf("actual=%+v expected=%+v", actual, expected)
	}
	for hostname, service := range expected {
		if actual[hostname] != service {
			t.Fatalf("route %s=%q want %q", hostname, actual[hostname], service)
		}
	}
}
