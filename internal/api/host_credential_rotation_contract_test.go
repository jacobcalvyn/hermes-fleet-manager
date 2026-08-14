package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/security"
)

func TestHostCredentialRotationRejectsInvalidRequestsWithoutMutation(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, oldToken := environment.enrollHost(t)
	path := "/api/v1/hosts/" + hostID + "/credentials/rotate"
	payload := map[string]string{
		"confirm_name":  "local-test",
		"hostname":      "host",
		"os":            "darwin",
		"arch":          "arm64",
		"agent_version": agentVersion,
	}
	before, err := environment.dataStore.GetHost(context.Background(), hostID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHash, err := environment.dataStore.HostTokenHash(context.Background(), hostID)
	if err != nil {
		t.Fatal(err)
	}

	assertUnchanged := func(t *testing.T) {
		t.Helper()
		after, err := environment.dataStore.GetHost(context.Background(), hostID)
		if err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Fatalf("rejected rotation changed host metadata: before=%+v after=%+v", before, after)
		}
		afterHash, err := environment.dataStore.HostTokenHash(context.Background(), hostID)
		if err != nil {
			t.Fatal(err)
		}
		if afterHash != beforeHash || afterHash != security.HashToken(oldToken) {
			t.Fatalf("rejected rotation changed token hash from %q to %q", beforeHash, afterHash)
		}
	}

	t.Run("malformed JSON", func(t *testing.T) {
		response := environment.rawRequest(
			t,
			http.MethodPost,
			path,
			strings.NewReader("{"),
			environment.adminToken,
			map[string]string{"Content-Type": "application/json"},
		)
		assertStatus(t, response, http.StatusBadRequest)
		assertUnchanged(t)
	})

	t.Run("unknown field", func(t *testing.T) {
		request := map[string]any{}
		for key, value := range payload {
			request[key] = value
		}
		request["unexpected"] = true
		response := environment.request(t, http.MethodPost, path, request, environment.adminToken, nil)
		assertStatus(t, response, http.StatusBadRequest)
		assertUnchanged(t)
	})

	t.Run("unknown host", func(t *testing.T) {
		response := environment.request(
			t,
			http.MethodPost,
			"/api/v1/hosts/00000000-0000-4000-8000-000000000099/credentials/rotate",
			payload,
			environment.adminToken,
			nil,
		)
		assertStatus(t, response, http.StatusNotFound)
		assertUnchanged(t)
	})
}

func TestHostCredentialRotationRejectsActiveJobWithoutMutation(t *testing.T) {
	environment := newAPITestEnvironment(t)
	hostID, oldToken := environment.enrollHost(t)
	response := environment.request(t, http.MethodPost, "/api/v1/instances", createInstanceRequest{
		Name: "credential-busy", HostID: hostID, HermesVersion: "0.19.0",
	}, environment.adminToken, nil)
	assertStatus(t, response, http.StatusAccepted)

	before, err := environment.dataStore.GetHost(context.Background(), hostID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHash, err := environment.dataStore.HostTokenHash(context.Background(), hostID)
	if err != nil {
		t.Fatal(err)
	}
	response = environment.request(
		t,
		http.MethodPost,
		"/api/v1/hosts/"+hostID+"/credentials/rotate",
		map[string]string{
			"confirm_name":  "local-test",
			"hostname":      "host",
			"os":            "darwin",
			"arch":          "arm64",
			"agent_version": agentVersion,
		},
		environment.adminToken,
		nil,
	)
	assertStatus(t, response, http.StatusConflict)

	after, err := environment.dataStore.GetHost(context.Background(), hostID)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("busy rotation changed host metadata: before=%+v after=%+v", before, after)
	}
	afterHash, err := environment.dataStore.HostTokenHash(context.Background(), hostID)
	if err != nil {
		t.Fatal(err)
	}
	if afterHash != beforeHash || afterHash != security.HashToken(oldToken) {
		t.Fatalf("busy rotation changed token hash from %q to %q", beforeHash, afterHash)
	}

	response = environment.request(t, http.MethodPost, "/api/v1/agent/heartbeat", map[string]string{
		"hostname": "host", "os": "darwin", "arch": "arm64", "agent_version": agentVersion,
	}, oldToken, map[string]string{"X-Fleet-Host-ID": hostID})
	assertStatus(t, response, http.StatusNoContent)
}
