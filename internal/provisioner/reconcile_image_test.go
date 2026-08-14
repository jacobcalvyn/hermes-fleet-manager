package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

const (
	reconcileInstanceID = "00000000-0000-4000-8000-000000000001"
	reconcileName       = "fleet-test-01"
	reconcileProject    = "hermes-fleet-fleet-test-01-00000000"
	reconcileImage      = "local/hermes-fleet-runtime:0.18.2"
)

type reconcileFixture struct {
	containers      []map[string]any
	resolvedImageID string
	composeImages   string
	volumeProject   string
}

func TestExecuteReconcileImageReturnsOnlyVerifiedStoppedDigest(t *testing.T) {
	result, commands := executeReconcileFixture(t, nil)
	newImageID := "sha256:" + strings.Repeat("b", 64)
	if !result.Success || result.ImageID != newImageID || result.InstanceStatus != domain.InstanceStopped {
		t.Fatalf("Execute() result=%+v", result)
	}
	mutating := map[string]bool{"up": true, "stop": true, "down": true, "start": true, "restart": true, "rm": true, "create": true}
	for _, command := range commands {
		for _, argument := range command {
			if mutating[argument] {
				t.Fatalf("image reconciliation issued mutating Docker command: %v", command)
			}
		}
	}
}

func TestExecuteReconcileImageRejectsUntrustedRuntimeState(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*reconcileFixture)
		wantError string
	}{
		{
			name: "running container",
			mutate: func(fixture *reconcileFixture) {
				fixture.containers[0]["State"] = map[string]any{"Status": "running"}
			},
			wantError: "stopped",
		},
		{
			name: "ownership drift",
			mutate: func(fixture *reconcileFixture) {
				config := fixture.containers[0]["Config"].(map[string]any)
				labels := config["Labels"].(map[string]string)
				labels["com.docker.compose.project"] = "not-fleet-owned"
			},
			wantError: "ownership",
		},
		{
			name: "mixed container images",
			mutate: func(fixture *reconcileFixture) {
				fixture.containers[1]["Image"] = "sha256:" + strings.Repeat("c", 64)
			},
			wantError: "same immutable image",
		},
		{
			name: "moved desired tag",
			mutate: func(fixture *reconcileFixture) {
				fixture.resolvedImageID = "sha256:" + strings.Repeat("c", 64)
			},
			wantError: "desired image",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, _ := executeReconcileFixture(t, test.mutate)
			if result.Success || result.ImageID != "" || !strings.Contains(strings.ToLower(result.Error), test.wantError) {
				t.Fatalf("Execute() result=%+v, want error containing %q", result, test.wantError)
			}
		})
	}
}

func TestExecuteRepairImageRestartsAndHealthChecksRunningInstance(t *testing.T) {
	result, commands := executeRepairFixture(t, true, false)
	newImageID := "sha256:" + strings.Repeat("b", 64)
	if !result.Success || result.ImageID != newImageID || result.InstanceStatus != domain.InstanceRunning {
		t.Fatalf("Execute() result=%+v", result)
	}
	if !hasComposeAction(commands, "stop") || !hasComposeAction(commands, "start") {
		t.Fatalf("automatic repair did not restore the running lifecycle: commands=%v", commands)
	}
}

func TestExecuteRepairImageLeavesStoppedInstanceStopped(t *testing.T) {
	result, commands := executeRepairFixture(t, false, false)
	if !result.Success || result.InstanceStatus != domain.InstanceStopped {
		t.Fatalf("Execute() result=%+v", result)
	}
	if hasComposeAction(commands, "stop") || hasComposeAction(commands, "start") {
		t.Fatalf("stopped image repair mutated lifecycle: commands=%v", commands)
	}
}

func TestExecuteRepairImageRestoresRunningStateWhenPostStopVerificationFails(t *testing.T) {
	result, commands := executeRepairFixture(t, true, true)
	if result.Success || result.ImageID != "" || result.InstanceStatus != domain.InstanceRunning || !strings.Contains(result.Error, "original running state was restored") {
		t.Fatalf("Execute() result=%+v", result)
	}
	if !hasComposeAction(commands, "stop") || !hasComposeAction(commands, "start") {
		t.Fatalf("failed repair did not attempt restoration: commands=%v", commands)
	}
}

func executeRepairFixture(t *testing.T, restart, failPostStopVerification bool) (domain.JobResult, [][]string) {
	t.Helper()
	root := t.TempDir()
	managedPath := filepath.Join(root, reconcileName+"-00000000")
	if err := os.MkdirAll(filepath.Join(managedPath, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"compose.yaml", ".env"} {
		if err := os.WriteFile(filepath.Join(managedPath, name), []byte("fleet\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	newImageID := "sha256:" + strings.Repeat("b", 64)
	stopped := !restart
	inspectCount := 0
	containerData := func() []map[string]any {
		status := "running"
		if stopped {
			status = "exited"
		}
		imageIDs := []string{newImageID, newImageID}
		if failPostStopVerification && inspectCount > 1 {
			imageIDs[1] = "sha256:" + strings.Repeat("c", 64)
		}
		services := []string{"hermes", "dashboard"}
		ids := []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb"}
		containers := make([]map[string]any, 0, 2)
		for index := range services {
			containers = append(containers, map[string]any{
				"Id": ids[index], "Image": imageIDs[index],
				"Config": map[string]any{"Labels": map[string]string{
					"io.hermes-fleet.managed": "true", "io.hermes-fleet.instance-id": reconcileInstanceID,
					"com.docker.compose.project": reconcileProject, "com.docker.compose.service": services[index],
				}},
				"State": map[string]any{"Status": status},
			})
		}
		return containers
	}

	provisioner, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	provisioner.httpClient = &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	var commands [][]string
	provisioner.dockerRun = func(_ context.Context, args ...string) (string, error) {
		commands = append(commands, append([]string(nil), args...))
		switch args[0] {
		case "compose":
			action := args[len(args)-1]
			switch action {
			case "--images":
				return reconcileImage + "\n" + reconcileImage + "\n", nil
			case "stop":
				stopped = true
				return "", nil
			case "start":
				stopped = false
				return "", nil
			}
		case "ps":
			return "aaaaaaaaaaaa\nbbbbbbbbbbbb\n", nil
		case "inspect":
			inspectCount++
			encoded, marshalErr := json.Marshal(containerData())
			return string(encoded), marshalErr
		case "volume":
			return reconcileProject + "\n", nil
		case "image":
			return newImageID + "\n", nil
		}
		return "", fmt.Errorf("unexpected Docker command: %v", args)
	}
	payload, err := json.Marshal(domain.ImageRepairPayload{
		InstanceID: reconcileInstanceID, Name: reconcileName, Image: reconcileImage,
		PreviousImageID: "sha256:" + strings.Repeat("a", 64), ProjectName: reconcileProject,
		DataVolume: reconcileProject + "-data", ManagedPath: managedPath, APIPort: 8650, Restart: restart,
	})
	if err != nil {
		t.Fatal(err)
	}
	return provisioner.Execute(context.Background(), domain.Job{Type: "instance.image.repair", Payload: payload}), commands
}

func hasComposeAction(commands [][]string, action string) bool {
	for _, command := range commands {
		if len(command) > 0 && command[0] == "compose" && command[len(command)-1] == action {
			return true
		}
	}
	return false
}

func executeReconcileFixture(t *testing.T, mutate func(*reconcileFixture)) (domain.JobResult, [][]string) {
	t.Helper()
	root := t.TempDir()
	managedPath := filepath.Join(root, reconcileName+"-00000000")
	if err := os.MkdirAll(filepath.Join(managedPath, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"compose.yaml", ".env"} {
		if err := os.WriteFile(filepath.Join(managedPath, name), []byte("fleet\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	newImageID := "sha256:" + strings.Repeat("b", 64)
	container := func(id, service string) map[string]any {
		return map[string]any{
			"Id": id, "Image": newImageID,
			"Config": map[string]any{"Labels": map[string]string{
				"io.hermes-fleet.managed": "true", "io.hermes-fleet.instance-id": reconcileInstanceID,
				"com.docker.compose.project": reconcileProject, "com.docker.compose.service": service,
			}},
			"State": map[string]any{"Status": "exited"},
		}
	}
	fixture := reconcileFixture{
		containers: []map[string]any{
			container("aaaaaaaaaaaa", "hermes"), container("bbbbbbbbbbbb", "dashboard"),
		},
		resolvedImageID: newImageID,
		composeImages:   reconcileImage + "\n" + reconcileImage + "\n",
		volumeProject:   reconcileProject,
	}
	if mutate != nil {
		mutate(&fixture)
	}

	provisioner, err := New(root, "docker")
	if err != nil {
		t.Fatal(err)
	}
	var commands [][]string
	provisioner.dockerRun = func(_ context.Context, args ...string) (string, error) {
		commands = append(commands, append([]string(nil), args...))
		switch args[0] {
		case "compose":
			if len(args) >= 2 && args[len(args)-2] == "config" && args[len(args)-1] == "--images" {
				return fixture.composeImages, nil
			}
		case "ps":
			return "aaaaaaaaaaaa\nbbbbbbbbbbbb\n", nil
		case "inspect":
			encoded, marshalErr := json.Marshal(fixture.containers)
			return string(encoded), marshalErr
		case "volume":
			return fixture.volumeProject + "\n", nil
		case "image":
			return fixture.resolvedImageID + "\n", nil
		}
		return "", fmt.Errorf("unexpected Docker command: %v", args)
	}
	payload, err := json.Marshal(map[string]string{
		"instance_id": reconcileInstanceID, "name": reconcileName, "image": reconcileImage,
		"previous_image_id": "sha256:" + strings.Repeat("a", 64), "project_name": reconcileProject,
		"data_volume": reconcileProject + "-data", "managed_path": managedPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := provisioner.Execute(context.Background(), domain.Job{Type: "instance.image.reconcile", Payload: payload})
	return result, commands
}
