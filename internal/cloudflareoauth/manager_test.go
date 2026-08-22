package cloudflareoauth

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testAdminTunnelID     = "11111111-1111-4111-8111-111111111111"
	testInstancesTunnelID = "22222222-2222-4222-8222-222222222222"
)

func TestAuthorizationUsesPKCEAndReturnsAuthorizedResources(t *testing.T) {
	var mu sync.Mutex
	verifier := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			verifier = r.Form.Get("code_verifier")
			mu.Unlock()
			writeTestJSON(w, `{"access_token":"access","refresh_token":"refresh","token_type":"Bearer","scope":"tunnel.write dns.write","expires_in":3600}`)
		case "/client/v4/accounts":
			writeTestJSON(w, `{"success":true,"result":[{"id":"account-a","name":"Primary"}]}`)
		case "/client/v4/zones":
			writeTestJSON(w, `{"success":true,"result":[{"id":"zone-a","name":"example.com","account":{"id":"account-a"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager, err := New(Config{
		ClientID: "client-a", RedirectURL: "https://fleet.example.com/oauth/callback",
		AuthorizationURL: server.URL + "/oauth/authorize", TokenURL: server.URL + "/oauth/token",
		APIBaseURL: server.URL + "/client/v4", HTTPClient: server.Client(), Scopes: []string{"dns.write", "tunnel.write"},
	})
	if err != nil {
		t.Fatal(err)
	}
	authorizationURL, err := manager.Begin()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("code_challenge_method") != "S256" || parsed.Query().Get("code_challenge") == "" || parsed.Query().Get("state") == "" {
		t.Fatalf("authorization query=%v", parsed.Query())
	}
	if parsed.Query().Get("scope") != "dns.write tunnel.write" {
		t.Fatalf("scope=%q", parsed.Query().Get("scope"))
	}
	session, err := manager.CompleteAuthorization(context.Background(), parsed.Query().Get("state"), "authorization-code")
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	usedVerifier := verifier
	mu.Unlock()
	if usedVerifier == "" {
		t.Fatal("token exchange did not send the PKCE verifier")
	}
	if len(session.Accounts) != 1 || session.Accounts[0].ID != "account-a" || len(session.Zones) != 1 || session.Zones[0].ID != "zone-a" {
		t.Fatalf("session=%+v", session)
	}
	if _, err := manager.CompleteAuthorization(context.Background(), parsed.Query().Get("state"), "reused-code"); err == nil {
		t.Fatal("OAuth state was reusable")
	}
}

func TestClientCanBeConfiguredWithoutRestart(t *testing.T) {
	manager, err := New(Config{RedirectURL: "https://fleet.example.com/api/v1/system/remote-access/cloudflare/oauth/callback"})
	if err != nil {
		t.Fatal(err)
	}
	if manager.Enabled() {
		t.Fatal("manager unexpectedly enabled before a client ID was configured")
	}
	setup := manager.Setup()
	if setup.ClientConfigured || setup.RedirectURL == "" || len(setup.Scopes) != 5 {
		t.Fatalf("setup=%+v", setup)
	}
	if err := manager.ConfigureClient("cloudflare-client-id"); err != nil {
		t.Fatal(err)
	}
	if !manager.Enabled() {
		t.Fatal("manager was not enabled after configuring a client ID")
	}
	authorizationURL, err := manager.Begin()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("client_id") != "cloudflare-client-id" {
		t.Fatalf("client_id=%q", parsed.Query().Get("client_id"))
	}
	if parsed.Query().Get("scope") != "account-settings.read argotunnel.write dns.write offline_access zone.read" {
		t.Fatalf("scope=%q", parsed.Query().Get("scope"))
	}
}

func TestValidateClientIDRejectsWhitespace(t *testing.T) {
	for _, clientID := range []string{"", "short", "client id with spaces", "client\nid"} {
		if err := ValidateClientID(clientID); err == nil {
			t.Fatalf("ValidateClientID(%q) returned nil", clientID)
		}
	}
}

func TestProvisionCreatesOwnedTunnelBoundariesAndDNS(t *testing.T) {
	requests := make([]string, 0)
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" && r.Header.Get("Authorization") != "Bearer access" {
			t.Errorf("authorization header for %s=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		switch {
		case r.URL.Path == "/oauth/token":
			writeTestJSON(w, `{"access_token":"access","refresh_token":"refresh","token_type":"Bearer","expires_in":3600}`)
		case r.URL.Path == "/client/v4/accounts":
			writeTestJSON(w, `{"success":true,"result":[{"id":"account-a","name":"Primary"}]}`)
		case r.URL.Path == "/client/v4/zones":
			writeTestJSON(w, `{"success":true,"result":[{"id":"zone-a","name":"example.com","account":{"id":"account-a"}}]}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/cfd_tunnel"):
			writeTestJSON(w, `{"success":true,"result":[]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cfd_tunnel"):
			name := ""
			if strings.Contains(readTestBody(t, r), "-instances") {
				name = "instances"
			}
			id := testAdminTunnelID
			if name == "instances" {
				id = testInstancesTunnelID
			}
			writeTestJSON(w, fmt.Sprintf(`{"success":true,"result":{"id":%q,"name":%q,"metadata":{"managed_by":"hermes-fleet","fleet_namespace":"fleet","boundary":%q}}}`, id, "hermes-fleet-fleet-"+map[bool]string{true: "instances", false: "admin"}[name == "instances"], map[bool]string{true: "instances", false: "admin"}[name == "instances"]))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/token"):
			id := testAdminTunnelID
			if strings.Contains(r.URL.Path, testInstancesTunnelID) {
				id = testInstancesTunnelID
			}
			writeTestJSON(w, fmt.Sprintf(`{"success":true,"result":%q}`, connectorToken(id)))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/configurations"):
			writeTestJSON(w, `{"success":true,"result":{}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/dns_records"):
			writeTestJSON(w, `{"success":true,"result":[]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/dns_records"):
			writeTestJSON(w, `{"success":true,"result":{"id":"dns-a"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager, err := New(Config{
		ClientID: "client-a", RedirectURL: "https://fleet.example.com/oauth/callback",
		AuthorizationURL: server.URL + "/oauth/authorize", TokenURL: server.URL + "/oauth/token",
		APIBaseURL: server.URL + "/client/v4", HTTPClient: server.Client(), Now: func() time.Time { return time.Unix(1_800_000_000, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	authorizationURL, err := manager.Begin()
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(authorizationURL)
	session, err := manager.CompleteAuthorization(context.Background(), parsed.Query().Get("state"), "code")
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Provision(context.Background(), ProvisionRequest{SessionID: session.ID, AccountID: "account-a", ZoneID: "zone-a", FleetNamespace: "fleet"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Cloudflare.AdminHostname != "fleet.example.com" || result.Cloudflare.RouteAutomation.TunnelID != testInstancesTunnelID || result.Cloudflare.OAuth.RefreshToken != "refresh" {
		t.Fatalf("provisioned=%+v", result.Cloudflare)
	}
	mu.Lock()
	joined := strings.Join(requests, "\n")
	mu.Unlock()
	for _, expected := range []string{"POST /client/v4/accounts/account-a/cfd_tunnel", "PUT /client/v4/accounts/account-a/cfd_tunnel/" + testAdminTunnelID + "/configurations", "POST /client/v4/zones/zone-a/dns_records"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("missing request %q in:\n%s", expected, joined)
		}
	}
}

func connectorToken(tunnelID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(`{"t":"` + tunnelID + `","padding":"012345678901234567890123456789"}`))
}

func writeTestJSON(w http.ResponseWriter, payload string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(payload))
}

func readTestBody(t *testing.T, r *http.Request) string {
	t.Helper()
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}
