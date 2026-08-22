package mcpdiscovery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

type staticResolver struct {
	addresses []net.IPAddr
}

func (resolver staticResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return resolver.addresses, nil
}

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestDiscoverInitializesAndListsToolsWithoutExposingBearerToken(t *testing.T) {
	t.Helper()
	requests := 0
	client := NewClient()
	client.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Header.Get("Authorization") != "Bearer discovery-secret" {
			t.Fatal("discovery request did not carry the bearer token")
		}
		if request.Method == http.MethodDelete {
			return response(request, http.StatusNoContent, "", ""), nil
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		switch payload["method"] {
		case "initialize":
			result := response(request, http.StatusOK, "application/json", `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"test","version":"1"}}}`)
			result.Header.Set("Mcp-Session-Id", "session-1")
			return result, nil
		case "notifications/initialized":
			if request.Header.Get("Mcp-Session-Id") != "session-1" {
				t.Fatal("initialized notification did not retain the MCP session")
			}
			return response(request, http.StatusAccepted, "", ""), nil
		case "tools/list":
			return response(request, http.StatusOK, "text/event-stream", "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"tools\":[{\"name\":\"search\",\"description\":\"Search records\"},{\"name\":\"fetch\"}]}}\n\n"), nil
		default:
			t.Fatalf("unexpected MCP method %v", payload["method"])
			return nil, nil
		}
	})}
	tools, err := client.Discover(context.Background(), Request{URL: "https://mcp.example.com/mcp", BearerToken: "discovery-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 4 || len(tools) != 2 || tools[0].Name != "search" || tools[0].Description != "Search records" || tools[1].Name != "fetch" {
		t.Fatalf("discovery requests=%d tools=%+v", requests, tools)
	}
}

func TestDiscoverRejectsPrivateAndUnsafeEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"http://mcp.example.com/mcp",
		"https://user:password@mcp.example.com/mcp",
		"https://mcp.example.com/mcp#fragment",
	} {
		if _, err := validateEndpoint(endpoint); err == nil {
			t.Fatalf("validateEndpoint(%q) unexpectedly succeeded", endpoint)
		}
	}
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "::1", "fc00::1"} {
		address, err := netipParse(raw)
		if err != nil || !blockedAddress(address) {
			t.Fatalf("address %q blocked=%v err=%v", raw, blockedAddress(address), err)
		}
	}
}

func TestRestrictedDiscoveryRejectsMixedPublicAndPrivateDNSAnswers(t *testing.T) {
	endpoint, err := validateEndpoint("https://mcp.example.com/v1")
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient()
	client.resolver = staticResolver{addresses: []net.IPAddr{
		{IP: net.ParseIP("1.1.1.1")},
		{IP: net.ParseIP("127.0.0.1")},
	}}
	if _, err := client.restrictedHTTPClient(context.Background(), endpoint); err == nil || !strings.Contains(err.Error(), "private or non-routable") {
		t.Fatalf("restrictedHTTPClient() error = %v", err)
	}
}

func TestDiscoverRejectsAuthenticationFailureWithoutReturningRemoteBody(t *testing.T) {
	client := NewClient()
	client.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return response(request, http.StatusUnauthorized, "application/json", `{"error":"leak discovery-secret"}`), nil
	})}
	_, err := client.Discover(context.Background(), Request{URL: "https://mcp.example.com/mcp", BearerToken: "discovery-secret"})
	if err == nil || !strings.Contains(err.Error(), "rejected authentication") || strings.Contains(err.Error(), "discovery-secret") {
		t.Fatalf("Discover() error=%v", err)
	}
}

func TestDiscoverReturnsTypedInitializeHTTPFailure(t *testing.T) {
	client := NewClient()
	client.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return response(request, http.StatusNotFound, "text/plain", "not found"), nil
	})}
	_, err := client.Discover(context.Background(), Request{URL: "https://mcp.example.com/mcp", BearerToken: "discovery-secret"})
	if err == nil {
		t.Fatal("Discover() unexpectedly succeeded")
	}
	var stageErr *StageError
	if !errors.As(err, &stageErr) || stageErr.Stage != "initialize" {
		t.Fatalf("Discover() stage error=%v", err)
	}
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusNotFound {
		t.Fatalf("Discover() status error=%v", err)
	}
	if strings.Contains(err.Error(), "discovery-secret") || strings.Contains(err.Error(), "not found") {
		t.Fatalf("Discover() leaked sensitive remote content: %v", err)
	}
}

func response(request *http.Request, status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func netipParse(raw string) (address netip.Addr, err error) {
	return netip.ParseAddr(raw)
}
