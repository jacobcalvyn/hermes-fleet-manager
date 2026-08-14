package mcpdiscovery

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/compatibility"
)

const (
	protocolVersion = "2025-03-26"
	maximumBodySize = 2 << 20
	maximumTools    = 100
)

var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

var blockedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
	netip.MustParsePrefix("2001:db8::/32"),
}

type Request struct {
	URL         string
	BearerToken string
}

type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type StageError struct {
	Stage string
	Err   error
}

func (err *StageError) Error() string { return fmt.Sprintf("MCP %s failed: %v", err.Stage, err.Err) }
func (err *StageError) Unwrap() error { return err.Err }

type HTTPStatusError struct {
	StatusCode int
}

func (err *HTTPStatusError) Error() string {
	if err.StatusCode == http.StatusUnauthorized || err.StatusCode == http.StatusForbidden {
		return "MCP endpoint rejected authentication"
	}
	return fmt.Sprintf("MCP endpoint returned HTTP %d", err.StatusCode)
}

type Client struct {
	resolver   *net.Resolver
	dialer     *net.Dialer
	httpClient *http.Client
}

func NewClient() *Client {
	return &Client{
		resolver: net.DefaultResolver,
		dialer:   &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 15 * time.Second},
	}
}

func (client *Client) Discover(ctx context.Context, request Request) ([]Tool, error) {
	endpoint, err := validateEndpoint(request.URL)
	if err != nil {
		return nil, err
	}
	httpClient := client.httpClient
	if httpClient == nil {
		httpClient, err = client.restrictedHTTPClient(ctx, endpoint)
		if err != nil {
			return nil, err
		}
	}
	protocol := &protocolClient{endpoint: endpoint, bearerToken: request.BearerToken, httpClient: httpClient}
	if err := protocol.initialize(ctx); err != nil {
		return nil, err
	}
	defer protocol.closeSession()
	return protocol.listTools(ctx)
}

func validateEndpoint(raw string) (*url.URL, error) {
	endpoint, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return nil, errors.New("MCP discovery requires an HTTPS endpoint without credentials or a fragment")
	}
	if endpoint.Port() != "" {
		port, portErr := strconv.Atoi(endpoint.Port())
		if portErr != nil || port < 1 || port > 65535 {
			return nil, errors.New("MCP discovery endpoint port is invalid")
		}
	}
	return endpoint, nil
}

func (client *Client) restrictedHTTPClient(ctx context.Context, endpoint *url.URL) (*http.Client, error) {
	resolver := client.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addresses, err := resolver.LookupIPAddr(ctx, endpoint.Hostname())
	if err != nil || len(addresses) == 0 {
		return nil, errors.New("MCP discovery endpoint could not be resolved")
	}
	allowed := make([]net.IP, 0, len(addresses))
	for _, candidate := range addresses {
		address, ok := netip.AddrFromSlice(candidate.IP)
		if !ok || blockedAddress(address.Unmap()) {
			return nil, errors.New("MCP discovery endpoint resolves to a private or non-routable address")
		}
		allowed = append(allowed, append(net.IP(nil), candidate.IP...))
	}
	port := endpoint.Port()
	if port == "" {
		port = "443"
	}
	dialer := client.dialer
	if dialer == nil {
		dialer = &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 15 * time.Second}
	}
	transport := &http.Transport{
		Proxy:                 nil,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, ServerName: endpoint.Hostname()},
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       15 * time.Second,
		DisableCompression:    true,
		DialContext: func(dialContext context.Context, network, _ string) (net.Conn, error) {
			var lastErr error
			for _, address := range allowed {
				connection, dialErr := dialer.DialContext(dialContext, network, net.JoinHostPort(address.String(), port))
				if dialErr == nil {
					return connection, nil
				}
				lastErr = dialErr
			}
			return nil, lastErr
		},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   25 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("MCP discovery does not follow redirects")
		},
	}, nil
}

func blockedAddress(address netip.Addr) bool {
	for _, network := range blockedNetworks {
		if network.Contains(address) {
			return true
		}
	}
	return false
}

type protocolClient struct {
	endpoint        *url.URL
	bearerToken     string
	httpClient      *http.Client
	sessionID       string
	protocolVersion string
	requestCounter  int
}

type rpcEnvelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code int `json:"code"`
	} `json:"error,omitempty"`
}

func (client *protocolClient) initialize(ctx context.Context) error {
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := client.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "hermes-fleet", "version": compatibility.HostAgentVersion},
	}, &result); err != nil {
		return &StageError{Stage: "initialize", Err: err}
	}
	if result.ProtocolVersion == "" {
		return errors.New("MCP initialize response did not include a protocol version")
	}
	client.protocolVersion = result.ProtocolVersion
	if err := client.notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return &StageError{Stage: "initialized notification", Err: err}
	}
	return nil
}

func (client *protocolClient) listTools(ctx context.Context) ([]Tool, error) {
	tools := make([]Tool, 0)
	cursor := ""
	for page := 0; page < 20; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := client.call(ctx, "tools/list", params, &result); err != nil {
			return nil, &StageError{Stage: "tools/list", Err: err}
		}
		for _, candidate := range result.Tools {
			name := strings.TrimSpace(candidate.Name)
			if !toolNamePattern.MatchString(name) {
				return nil, errors.New("MCP server returned an invalid tool name")
			}
			description := strings.TrimSpace(candidate.Description)
			if len(description) > 500 {
				description = description[:500]
			}
			tools = append(tools, Tool{Name: name, Description: description})
			if len(tools) > maximumTools {
				return nil, errors.New("MCP server exposes more than 100 tools")
			}
		}
		cursor = strings.TrimSpace(result.NextCursor)
		if cursor == "" {
			break
		}
	}
	if len(tools) == 0 {
		return nil, errors.New("MCP server did not expose any tools")
	}
	return tools, nil
}

func (client *protocolClient) call(ctx context.Context, method string, params any, target any) error {
	client.requestCounter++
	payload := map[string]any{"jsonrpc": "2.0", "id": client.requestCounter, "method": method, "params": params}
	envelope, err := client.send(ctx, payload, true)
	if err != nil {
		return err
	}
	if envelope.Error != nil {
		return fmt.Errorf("MCP server returned JSON-RPC error %d", envelope.Error.Code)
	}
	if len(envelope.Result) == 0 {
		return errors.New("MCP server returned an empty JSON-RPC result")
	}
	if err := json.Unmarshal(envelope.Result, target); err != nil {
		return errors.New("MCP server returned an invalid JSON-RPC result")
	}
	return nil
}

func (client *protocolClient) notify(ctx context.Context, method string, params any) error {
	_, err := client.send(ctx, map[string]any{"jsonrpc": "2.0", "method": method, "params": params}, false)
	return err
}

func (client *protocolClient) send(ctx context.Context, payload any, responseRequired bool) (rpcEnvelope, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return rpcEnvelope{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return rpcEnvelope{}, errors.New("MCP discovery request could not be created")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Origin", client.endpoint.Scheme+"://"+client.endpoint.Host)
	request.Header.Set("User-Agent", "hermes-fleet-mcp-discovery/"+compatibility.HostAgentVersion)
	if client.bearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+client.bearerToken)
	}
	if client.sessionID != "" {
		request.Header.Set("Mcp-Session-Id", client.sessionID)
		request.Header.Set("Mcp-Protocol-Version", client.negotiatedProtocolVersion())
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return rpcEnvelope{}, errors.New("MCP endpoint could not be reached")
	}
	defer response.Body.Close()
	if client.sessionID == "" {
		client.sessionID = strings.TrimSpace(response.Header.Get("Mcp-Session-Id"))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return rpcEnvelope{}, &HTTPStatusError{StatusCode: response.StatusCode}
	}
	if !responseRequired || response.StatusCode == http.StatusAccepted || response.StatusCode == http.StatusNoContent {
		return rpcEnvelope{}, nil
	}
	limited := io.LimitReader(response.Body, maximumBodySize+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil || len(responseBody) > maximumBodySize {
		return rpcEnvelope{}, errors.New("MCP response exceeded the safe size limit")
	}
	return decodeEnvelope(response.Header.Get("Content-Type"), responseBody)
}

func decodeEnvelope(contentType string, body []byte) (rpcEnvelope, error) {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		scanner := bufio.NewScanner(bytes.NewReader(body))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			candidate := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var envelope rpcEnvelope
			if json.Unmarshal([]byte(candidate), &envelope) == nil && (len(envelope.Result) > 0 || envelope.Error != nil) {
				return envelope, nil
			}
		}
		return rpcEnvelope{}, errors.New("MCP event stream did not contain a JSON-RPC response")
	}
	var envelope rpcEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return rpcEnvelope{}, errors.New("MCP endpoint returned an invalid JSON-RPC response")
	}
	return envelope, nil
}

func (client *protocolClient) closeSession() {
	if client.sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, client.endpoint.String(), nil)
	if err != nil {
		return
	}
	request.Header.Set("Mcp-Session-Id", client.sessionID)
	request.Header.Set("Mcp-Protocol-Version", client.negotiatedProtocolVersion())
	request.Header.Set("Origin", client.endpoint.Scheme+"://"+client.endpoint.Host)
	if client.bearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+client.bearerToken)
	}
	response, err := client.httpClient.Do(request)
	if err == nil {
		response.Body.Close()
	}
}

func (client *protocolClient) negotiatedProtocolVersion() string {
	if client.protocolVersion != "" {
		return client.protocolVersion
	}
	return protocolVersion
}
