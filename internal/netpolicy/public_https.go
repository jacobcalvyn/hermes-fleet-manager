package netpolicy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

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

var (
	ErrInvalidEndpoint = errors.New("public endpoint is invalid")
	ErrUnsafeAddress   = errors.New("public endpoint resolves to a private or non-routable address")
)

type Resolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type Dialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

func NewPinnedHTTPSClient(ctx context.Context, rawURL string, resolver Resolver, dialer Dialer, base *http.Client) (*url.URL, *http.Client, error) {
	endpoint, err := parsePublicHTTPSURL(rawURL)
	if err != nil {
		return nil, nil, err
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addresses, err := resolver.LookupIPAddr(ctx, endpoint.Hostname())
	if err != nil {
		return nil, nil, err
	}
	if len(addresses) == 0 {
		return nil, nil, errors.New("public endpoint did not resolve to an address")
	}
	allowed := make([]net.IP, 0, len(addresses))
	for _, candidate := range addresses {
		address, ok := netip.AddrFromSlice(candidate.IP)
		if !ok || !IsPublicAddress(address.Unmap()) {
			return nil, nil, ErrUnsafeAddress
		}
		allowed = append(allowed, append(net.IP(nil), candidate.IP...))
	}
	if dialer == nil {
		dialer = &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 15 * time.Second}
	}
	if base == nil {
		base = &http.Client{Timeout: 25 * time.Second}
	}
	var transport *http.Transport
	switch configured := base.Transport.(type) {
	case nil:
		transport = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		transport = configured.Clone()
	default:
		return nil, nil, errors.New("public endpoint client requires an HTTP transport")
	}
	port := endpoint.Port()
	if port == "" {
		port = "443"
	}
	transport.Proxy = nil
	transport.TLSClientConfig = cloneTLSConfig(transport.TLSClientConfig, endpoint.Hostname())
	transport.TLSHandshakeTimeout = 8 * time.Second
	transport.ResponseHeaderTimeout = 15 * time.Second
	transport.IdleConnTimeout = 15 * time.Second
	transport.DisableCompression = true
	transport.DialContext = func(dialContext context.Context, network, _ string) (net.Conn, error) {
		var lastErr error
		for _, address := range allowed {
			connection, dialErr := dialer.DialContext(dialContext, network, net.JoinHostPort(address.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}
	client := *base
	client.Transport = transport
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return errors.New("public endpoint verification does not follow redirects")
	}
	return endpoint, &client, nil
}

func parsePublicHTTPSURL(raw string) (*url.URL, error) {
	endpoint, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return nil, fmt.Errorf("%w: HTTPS URL without credentials or a fragment is required", ErrInvalidEndpoint)
	}
	hostname := strings.ToLower(strings.TrimSuffix(endpoint.Hostname(), "."))
	if net.ParseIP(hostname) != nil || !strings.Contains(hostname, ".") || hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") || strings.HasSuffix(hostname, ".local") {
		return nil, fmt.Errorf("%w: a public DNS hostname is required", ErrInvalidEndpoint)
	}
	if endpoint.Port() != "" {
		port, portErr := strconv.Atoi(endpoint.Port())
		if portErr != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("%w: port is invalid", ErrInvalidEndpoint)
		}
	}
	return endpoint, nil
}

// IsPublicAddress reports whether an address is globally routable under the
// Fleet outbound network policy.
func IsPublicAddress(address netip.Addr) bool {
	if !address.IsValid() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	for _, network := range blockedNetworks {
		if network.Contains(address) {
			return false
		}
	}
	return true
}

func cloneTLSConfig(config *tls.Config, serverName string) *tls.Config {
	if config == nil {
		config = &tls.Config{}
	} else {
		config = config.Clone()
	}
	if config.MinVersion < tls.VersionTLS12 {
		config.MinVersion = tls.VersionTLS12
	}
	config.ServerName = serverName
	return config
}
