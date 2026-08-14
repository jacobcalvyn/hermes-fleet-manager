package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/agent"
)

func TestWriteConfigReplacesExistingConfigWith0600File(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := agent.Config{HostID: "agent-1", HostToken: "secret", ControlPlaneURL: "http://127.0.0.1:8650"}
	if err := writeConfig(path, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %o, want 600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got agent.Config
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	if got != want {
		t.Fatalf("config = %+v, want %+v", got, want)
	}
	read, err := readConfig(path)
	if err != nil {
		t.Fatalf("readConfig() rejected valid config: %v", err)
	}
	if read != want {
		t.Fatalf("readConfig() = %+v, want %+v", read, want)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfig(path); err == nil {
		t.Fatal("readConfig() accepted an insecure config mode")
	}
}

func TestReadAdminTokenRequiresEnvironmentOrExplicitStdin(t *testing.T) {
	token, err := readAdminToken(strings.NewReader("ignored"), " environment-secret \n", false)
	if err != nil {
		t.Fatal(err)
	}
	if token != "environment-secret" {
		t.Fatalf("environment token = %q", token)
	}

	token, err = readAdminToken(strings.NewReader("stdin-secret\n"), "environment-secret", true)
	if err != nil {
		t.Fatal(err)
	}
	if token != "stdin-secret" {
		t.Fatalf("stdin token = %q", token)
	}

	if _, err := readAdminToken(strings.NewReader("not-read"), "", false); err == nil {
		t.Fatal("readAdminToken() accepted a missing admin token")
	}
	if _, err := readAdminToken(strings.NewReader(strings.Repeat("x", 4097)), "", true); err == nil {
		t.Fatal("readAdminToken() accepted an oversized stdin token")
	}
}

func TestReadEnrollmentTokenFromStdinIsBounded(t *testing.T) {
	token, err := readSecretFromStdin(strings.NewReader("enrollment-secret\n"), "enrollment token")
	if err != nil {
		t.Fatal(err)
	}
	if token != "enrollment-secret" {
		t.Fatalf("enrollment token = %q", token)
	}
	if _, err := readSecretFromStdin(strings.NewReader(""), "enrollment token"); err == nil {
		t.Fatal("readSecretFromStdin() accepted an empty enrollment token")
	}
	if _, err := readSecretFromStdin(strings.NewReader(strings.Repeat("x", 4097)), "enrollment token"); err == nil {
		t.Fatal("readSecretFromStdin() accepted an oversized enrollment token")
	}
}

func TestProbeFailureExitCodeOnlyAllowsAuthenticationRecovery(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{
			name: "unauthorized",
			err:  &agent.HTTPError{StatusCode: http.StatusUnauthorized},
			want: probeAuthenticationRejectedExitCode,
		},
		{
			name: "forbidden",
			err:  &agent.HTTPError{StatusCode: http.StatusForbidden},
			want: probeAuthenticationRejectedExitCode,
		},
		{
			name: "service unavailable",
			err:  &agent.HTTPError{StatusCode: http.StatusServiceUnavailable},
			want: 1,
		},
		{name: "network failure", err: errors.New("connection refused"), want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := probeFailureExitCode(test.err); got != test.want {
				t.Fatalf("probeFailureExitCode() = %d, want %d", got, test.want)
			}
		})
	}
}
