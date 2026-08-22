package provisioner

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func TestSetHermesToolsetIsIdempotent(t *testing.T) {
	p, err := New(t.TempDir(), "docker")
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	p.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Method != http.MethodGet || request.URL.Path != "/api/tools/toolsets" || request.URL.Query().Get("profile") != "default" {
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.String())
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"toolsets":[{"name":"browser","enabled":true}]}`)), Header: make(http.Header)}, nil
	})}
	result := p.setHermesToolsetWithSession(context.Background(), validToolsetPayload(), &hermesProfileSession{})
	if !result.Success || requests != 1 {
		t.Fatalf("result=%+v requests=%d", result, requests)
	}
}

func TestSetHermesToolsetMutatesAndVerifies(t *testing.T) {
	p, err := New(t.TempDir(), "docker")
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	p.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		enabled := requests > 1
		if request.Method == http.MethodPut {
			if request.URL.Path != "/api/tools/toolsets/browser" {
				t.Fatalf("unexpected mutation path %q", request.URL.Path)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
		}
		body := `{"toolsets":[{"name":"browser","enabled":false}]}`
		if enabled {
			body = `{"toolsets":[{"name":"browser","enabled":true}]}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	result := p.setHermesToolsetWithSession(context.Background(), validToolsetPayload(), &hermesProfileSession{})
	if !result.Success || requests != 3 {
		t.Fatalf("result=%+v requests=%d", result, requests)
	}
}

func validToolsetPayload() domain.HermesToolsetMutationPayload {
	return domain.HermesToolsetMutationPayload{
		HermesProfileInspectPayload: domain.HermesProfileInspectPayload{
			InstanceID: "instance-1", Name: "nara", ProjectName: "fleet-nara",
			ManagedPath: "/managed/nara", DashboardPort: 19130,
		},
		ToolsetName: "browser", Profile: "default", Enabled: true,
	}
}
