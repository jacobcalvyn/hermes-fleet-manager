package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestConnectorHealthStates(t *testing.T) {
	health := newConnectorHealth()
	for _, test := range []struct {
		name  string
		state string
		ready bool
		err   error
		code  int
	}{
		{name: "disabled", state: "disabled", ready: true, code: http.StatusOK},
		{name: "running", state: "running", ready: true, code: http.StatusOK},
		{name: "starting", state: "starting", ready: false, code: http.StatusServiceUnavailable},
		{name: "retrying", state: "retrying", ready: false, err: errors.New("tunnel failed"), code: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			health.set(test.state, test.ready, test.err)
			request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			response := httptest.NewRecorder()
			health.handler(response, request)
			if response.Code != test.code {
				t.Fatalf("health status=%d, want %d", response.Code, test.code)
			}
			if got := health.value(); got.State != test.state || got.Ready != test.ready {
				t.Fatalf("snapshot=%+v", got)
			}
		})
	}
}

func TestReadRuntimeSpecPrefersLegacyLocalConfigAndFallsBackToToken(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yml")
	tokenPath := filepath.Join(directory, "token")

	spec, err := readRuntimeSpec(configPath, tokenPath)
	if err != nil || spec.Mode != "disabled" {
		t.Fatalf("empty runtime spec=%+v err=%v", spec, err)
	}
	if err := os.WriteFile(tokenPath, []byte("legacy-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err = readRuntimeSpec(configPath, tokenPath)
	if err != nil || spec.Mode != "token" || spec.Path != tokenPath {
		t.Fatalf("token runtime spec=%+v err=%v", spec, err)
	}
	if err := os.WriteFile(configPath, []byte("tunnel: tunnel-id\ningress:\n  - service: http_status:404\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err = readRuntimeSpec(configPath, tokenPath)
	if err != nil || spec.Mode != "local" || spec.Path != configPath {
		t.Fatalf("local runtime spec=%+v err=%v", spec, err)
	}
	if err := os.WriteFile(configPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRuntimeSpec(configPath, tokenPath); err == nil {
		t.Fatal("empty local configuration was accepted")
	}
}
