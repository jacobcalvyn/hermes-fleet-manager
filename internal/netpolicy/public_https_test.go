package netpolicy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
)

type staticResolver struct {
	addresses []net.IPAddr
	err       error
}

type recordingDialer struct {
	address string
}

func (dialer *recordingDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	dialer.address = address
	return nil, errors.New("dial stopped by test")
}

func (resolver staticResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return resolver.addresses, resolver.err
}

func TestPinnedHTTPSClientRejectsPrivateAndMixedResolution(t *testing.T) {
	tests := []struct {
		name      string
		addresses []net.IPAddr
	}{
		{name: "loopback", addresses: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}},
		{name: "private", addresses: []net.IPAddr{{IP: net.ParseIP("10.0.0.8")}}},
		{name: "mixed", addresses: []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}, {IP: net.ParseIP("192.168.1.5")}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := NewPinnedHTTPSClient(context.Background(), "https://mcp.example.com/v1", staticResolver{addresses: test.addresses}, nil, nil)
			if !errors.Is(err, ErrUnsafeAddress) {
				t.Fatalf("NewPinnedHTTPSClient() error = %v, want ErrUnsafeAddress", err)
			}
		})
	}
}

func TestPinnedHTTPSClientRejectsLocalHostnameBeforeResolution(t *testing.T) {
	_, _, err := NewPinnedHTTPSClient(context.Background(), "https://localhost/api", staticResolver{}, nil, nil)
	if !errors.Is(err, ErrInvalidEndpoint) {
		t.Fatalf("NewPinnedHTTPSClient() error = %v, want ErrInvalidEndpoint", err)
	}
}

func TestPinnedHTTPSClientDisablesProxyAndPreservesTLSHostname(t *testing.T) {
	dialer := &recordingDialer{}
	endpoint, client, err := NewPinnedHTTPSClient(
		context.Background(),
		"https://mcp.example.com/v1",
		staticResolver{addresses: []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}},
		dialer,
		&http.Client{},
	)
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || transport.TLSClientConfig.ServerName != endpoint.Hostname() {
		t.Fatalf("pinned transport = %#v", client.Transport)
	}
	if _, err := transport.DialContext(context.Background(), "tcp", "mcp.example.com:443"); err == nil {
		t.Fatal("pinned transport unexpectedly connected")
	}
	if dialer.address != "1.1.1.1:443" {
		t.Fatalf("pinned transport dialed %q", dialer.address)
	}
}
