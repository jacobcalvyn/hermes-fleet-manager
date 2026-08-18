package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/recoverycodec"
)

func TestRecoverUsesExactHostNameAndAdminAuthenticatedRotation(t *testing.T) {
	const adminToken = "admin-token-that-never-enters-an-argument"
	var rotateRequests int
	client := testHTTPClient(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+adminToken {
			t.Errorf("Authorization header was not set")
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/hosts":
			_ = json.NewEncoder(response).Encode([]map[string]string{
				{"id": "host-near-match", "name": "local-mac-old"},
				{"id": "host-exact", "name": "local-mac"},
			})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/hosts/host-exact/credentials/rotate":
			rotateRequests++
			var payload map[string]string
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Errorf("decode recovery request: %v", err)
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			want := map[string]string{
				"confirm_name":  "local-mac",
				"hostname":      "machine.local",
				"os":            runtime.GOOS,
				"arch":          runtime.GOARCH,
				"agent_version": Version,
			}
			if !reflect.DeepEqual(payload, want) {
				t.Errorf("recovery payload = %#v, want %#v", payload, want)
			}
			response.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(response).Encode(Enrollment{HostID: "host-exact", HostToken: "rotated-secret"})
		default:
			t.Errorf("unexpected recovery request %s %s", request.Method, request.URL.Path)
			response.WriteHeader(http.StatusNotFound)
		}
	}))

	enrollment, err := recoverWithClient(
		context.Background(), client, "http://fleet.test", adminToken, "local-mac", "machine.local",
	)
	if err != nil {
		t.Fatal(err)
	}
	if enrollment != (Enrollment{HostID: "host-exact", HostToken: "rotated-secret"}) {
		t.Fatalf("recovery response = %+v", enrollment)
	}
	if rotateRequests != 1 {
		t.Fatalf("rotation requests = %d, want 1", rotateRequests)
	}
}

func TestRecoverDoesNotRotateWithoutAnExactHostMatch(t *testing.T) {
	var rotateRequests int
	client := testHTTPClient(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/api/v1/hosts" {
			_ = json.NewEncoder(response).Encode([]map[string]string{{"id": "host-1", "name": "local-mac-old"}})
			return
		}
		rotateRequests++
		response.WriteHeader(http.StatusInternalServerError)
	}))

	_, err := recoverWithClient(
		context.Background(), client, "http://fleet.test", "admin-token", "local-mac", "machine.local",
	)
	if !IsHTTPStatus(err, http.StatusNotFound) {
		t.Fatalf("Recover() error = %v, want 404", err)
	}
	if rotateRequests != 0 {
		t.Fatalf("rotation requests = %d, want 0", rotateRequests)
	}
}

func TestRecoverPreservesConflictStatus(t *testing.T) {
	client := testHTTPClient(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			_ = json.NewEncoder(response).Encode([]map[string]string{{"id": "host-1", "name": "local-mac"}})
		case http.MethodPost:
			response.WriteHeader(http.StatusConflict)
			_, _ = response.Write([]byte(`{"error":"host has active jobs"}`))
		}
	}))

	_, err := recoverWithClient(
		context.Background(), client, "http://fleet.test", "admin-token", "local-mac", "machine.local",
	)
	if !IsHTTPStatus(err, http.StatusConflict) {
		t.Fatalf("Recover() error = %v, want 409", err)
	}
}

func TestProbePreservesHTTPFailureClassification(t *testing.T) {
	for _, statusCode := range []int{http.StatusUnauthorized, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			client := New(Config{
				ControlPlaneURL: "http://fleet.test",
				HostID:          "host-1",
				HostToken:       "host-token",
				Name:            "local-mac",
				Hostname:        "machine.local",
			}, nil)
			client.httpClient = testHTTPClient(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.WriteHeader(statusCode)
				_, _ = response.Write([]byte("probe rejected"))
			}))

			err := client.Probe(context.Background())
			if !IsHTTPStatus(err, statusCode) {
				t.Fatalf("Probe() error = %v, want HTTP status %d", err, statusCode)
			}
			want := fmt.Sprintf("control plane returned %d: probe rejected", statusCode)
			if err.Error() != want {
				t.Fatalf("Probe() error text = %q, want %q", err.Error(), want)
			}
		})
	}
}

func TestRecoveryPointUploadDecryptsStagingArtifactAndCarriesLeaseMetadata(t *testing.T) {
	plaintext := bytes.Repeat([]byte("instance-recovery-data"), 1000)
	key := bytes.Repeat([]byte{0x42}, 32)
	pointID := "recovery-" + strings.Repeat("a", 32)
	artifactPath := filepath.Join(t.TempDir(), "point.enc")
	artifact, err := os.OpenFile(artifactPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recoverycodec.Encrypt(context.Background(), artifact, bytes.NewReader(plaintext), key, pointID+":artifact"); err != nil {
		t.Fatal(err)
	}
	if err := artifact.Close(); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(plaintext)
	result := domain.JobResult{
		Success: true, RecoveryPointID: pointID, RecoverySHA256: hex.EncodeToString(digest[:]),
		RecoverySizeBytes: int64(len(plaintext)), RecoveryArtifact: artifactPath, RecoveryKey: key,
	}
	client := New(Config{ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "host-token"}, &observingExecutor{})
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/v1/agent/jobs/job-1/recovery-point" || request.Method != http.MethodPut {
			t.Fatalf("unexpected upload request %s %s", request.Method, request.URL.Path)
		}
		assertLeaseHeader(t, request, "lease-1")
		if request.Header.Get("X-Fleet-Recovery-Point-ID") != pointID || request.Header.Get("X-Fleet-Recovery-SHA256") != result.RecoverySHA256 {
			t.Fatalf("recovery headers=%v", request.Header)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(body, plaintext) {
			t.Fatal("uploaded recovery body does not match decrypted staging artifact")
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})
	job := domain.Job{ID: "job-1", LeaseToken: "lease-1"}
	if err := client.uploadRecoveryPoint(context.Background(), job, result); err != nil {
		t.Fatal(err)
	}
}

func TestChatArtifactUploadCarriesLeaseAndReturnsFleetCapability(t *testing.T) {
	content := []byte("dashboard,ready\n")
	digest := sha256.Sum256(content)
	localPath := filepath.Join(t.TempDir(), "dashboard.csv")
	if err := os.WriteFile(localPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	upload := domain.ChatArtifactUpload{
		LocalPath: localPath,
		Artifact: domain.ChatArtifact{
			ID: "artifact-0123456789abcdef0123456789abcdef", Name: "dashboard.csv", Kind: "file",
			MediaType: "text/csv", SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:]), Status: "ready",
		},
	}
	client := New(Config{ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "host-token"}, &observingExecutor{})
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPut || request.URL.Path != "/api/v1/agent/jobs/job-1/chat-artifacts/"+upload.Artifact.ID {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		assertLeaseHeader(t, request, "lease-1")
		body, err := io.ReadAll(request.Body)
		if err != nil || !bytes.Equal(body, content) {
			t.Fatalf("body=%q error=%v", body, err)
		}
		if request.Header.Get("X-Fleet-Artifact-SHA256") != upload.Artifact.SHA256 || request.Header.Get("Content-Type") != "text/csv" {
			t.Fatalf("headers=%v", request.Header)
		}
		ready := upload.Artifact
		ready.URL = "/api/v1/chats/session/artifacts/" + ready.ID + "/download"
		encoded, _ := json.Marshal(ready)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(encoded)), Request: request}, nil
	})
	ready, err := client.uploadChatArtifact(context.Background(), domain.Job{ID: "job-1", LeaseToken: "lease-1"}, upload)
	if err != nil || ready.URL == "" || ready.ID != upload.Artifact.ID {
		t.Fatalf("ready=%+v error=%v", ready, err)
	}
}

func TestChatArtifactUploadClassifiesFleetQuotaRejection(t *testing.T) {
	content := []byte("quota")
	digest := sha256.Sum256(content)
	localPath := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(localPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	upload := domain.ChatArtifactUpload{
		LocalPath: localPath,
		Artifact: domain.ChatArtifact{
			ID: "artifact-0123456789abcdef0123456789abcdef", Name: "report.txt", Kind: "file",
			MediaType: "text/plain", SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:]), Status: "preparing",
		},
	}
	client := New(Config{ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "host-token"}, &observingExecutor{})
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInsufficientStorage, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"error":"chat artifact storage quota was exceeded"}`)), Request: request,
		}, nil
	})
	if _, err := client.uploadChatArtifact(context.Background(), domain.Job{ID: "job-1", LeaseToken: "lease-1"}, upload); !errors.Is(err, errChatArtifactRejected) {
		t.Fatalf("upload error=%v", err)
	}
}

func TestHermesUpdateWorkflowPersistsProgressAndVerifiesBackupBeforeInstall(t *testing.T) {
	plaintext := bytes.Repeat([]byte("verified-update-backup"), 500)
	pointID := "recovery-" + strings.Repeat("c", 32)
	executor := &hermesUpdateTestExecutor{
		t: t, root: t.TempDir(), plaintext: plaintext, pointID: pointID,
		targetImageID: "sha256:" + strings.Repeat("d", 64),
	}
	progressStages := make([]string, 0, 6)
	backupVerified := false
	client := New(Config{ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "host-token"}, executor)
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		assertLeaseHeader(t, request, "lease-update")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/agent/jobs/job-update/recovery-point":
			return &http.Response{
				StatusCode: http.StatusNotFound, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{"error":"not ready"}`)), Request: request,
			}, nil
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/agent/jobs/job-update/progress":
			var progress domain.JobProgress
			if err := json.NewDecoder(request.Body).Decode(&progress); err != nil {
				t.Fatal(err)
			}
			progressStages = append(progressStages, progress.Stage)
		case request.Method == http.MethodPut && request.URL.Path == "/api/v1/agent/jobs/job-update/recovery-point":
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(body, plaintext) {
				t.Fatal("automatic update backup upload does not match the decrypted artifact")
			}
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/agent/jobs/job-update/recovery-point/verify":
			var payload struct {
				RecoveryPointID string `json:"recovery_point_id"`
				SHA256          string `json:"sha256"`
				SizeBytes       int64  `json:"size_bytes"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(plaintext)
			if payload.RecoveryPointID != pointID || payload.SHA256 != hex.EncodeToString(digest[:]) || payload.SizeBytes != int64(len(plaintext)) {
				t.Fatalf("backup verification payload=%+v", payload)
			}
			backupVerified = true
		default:
			t.Fatalf("unexpected Hermes update request %s %s", request.Method, request.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusNoContent, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("")), Request: request,
		}, nil
	})
	update := domain.HermesUpdatePayload{
		OriginalStatus: domain.InstanceRunning,
		Backup: domain.RecoveryPointPayload{
			RecoveryPointID: pointID, InstanceID: "instance-1", Name: "fleet-test-01",
			MaxBytes: 1 << 20,
		},
		Upgrade: domain.HermesUpgradePayload{
			InstanceID: "instance-1", Name: "fleet-test-01",
			CurrentImage: "runtime:0.18.2", CurrentImageID: "sha256:" + strings.Repeat("a", 64),
			TargetImage: "runtime:0.19.0", TargetVersion: "0.19.0",
			ProjectName: "hermes-fleet-test", ManagedPath: "/managed/fleet-test",
			APIPort: 8650, DashboardPort: 9130,
			Rollback: domain.RecoveryRestorePayload{RecoveryPointID: pointID, RequireImageID: true},
		},
	}
	encoded, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	result := client.executeHermesUpdate(context.Background(), domain.Job{
		ID: "job-update", Type: "instance.hermes.update", Payload: encoded, LeaseToken: "lease-update",
	})
	if !result.Success || result.InstanceStatus != domain.InstanceRunning || result.ImageID != executor.targetImageID {
		t.Fatalf("Hermes update result=%+v", result)
	}
	if !backupVerified {
		t.Fatal("Hermes was installed before the control plane verified the automatic backup")
	}
	expectedStages := []string{"PREPARING_RELEASE", "STOPPING", "BACKING_UP", "INSTALLING", "RESTORING_STATE", "VERIFYING_VERSION"}
	if strings.Join(progressStages, ",") != strings.Join(expectedStages, ",") {
		t.Fatalf("progress stages=%v want=%v", progressStages, expectedStages)
	}
	expectedExecutions := []string{"instance.hermes.prepare", "instance.stop", "instance.recovery.create", "instance.hermes.upgrade", "instance.start"}
	if strings.Join(executor.executions, ",") != strings.Join(expectedExecutions, ",") {
		t.Fatalf("executor calls=%v want=%v", executor.executions, expectedExecutions)
	}
}

func TestHermesUpdateRetryReusesVerifiedAutomaticBackup(t *testing.T) {
	plaintext := bytes.Repeat([]byte("verified-retry-backup"), 500)
	digest := sha256.Sum256(plaintext)
	pointID := "recovery-" + strings.Repeat("e", 32)
	executor := &hermesUpdateTestExecutor{
		t: t, root: t.TempDir(), plaintext: plaintext, pointID: pointID,
		targetImageID: "sha256:" + strings.Repeat("f", 64),
	}
	client := New(Config{ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "host-token"}, executor)
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		assertLeaseHeader(t, request, "lease-retry")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/agent/jobs/job-retry/recovery-point":
			header := make(http.Header)
			header.Set("Content-Length", strconv.Itoa(len(plaintext)))
			header.Set("X-Fleet-Recovery-SHA256", hex.EncodeToString(digest[:]))
			return &http.Response{
				StatusCode: http.StatusOK, Header: header,
				Body: io.NopCloser(bytes.NewReader(plaintext)), Request: request,
			}, nil
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/agent/jobs/job-retry/progress":
			return &http.Response{
				StatusCode: http.StatusNoContent, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader("")), Request: request,
			}, nil
		default:
			t.Fatalf("unexpected Hermes update retry request %s %s", request.Method, request.URL.Path)
			return nil, nil
		}
	})
	update := domain.HermesUpdatePayload{
		OriginalStatus: domain.InstanceRunning,
		Backup: domain.RecoveryPointPayload{
			RecoveryPointID: pointID, InstanceID: "instance-1", Name: "fleet-test-01", MaxBytes: 1 << 20,
		},
		Upgrade: domain.HermesUpgradePayload{
			InstanceID: "instance-1", Name: "fleet-test-01",
			CurrentImage: "runtime:0.18.2", CurrentImageID: "sha256:" + strings.Repeat("a", 64),
			TargetImage: "runtime:0.19.0", TargetVersion: "0.19.0",
			ProjectName: "hermes-fleet-test", ManagedPath: "/managed/fleet-test",
			APIPort: 8650, DashboardPort: 9130,
			Rollback: domain.RecoveryRestorePayload{RecoveryPointID: pointID, RequireImageID: true},
		},
	}
	encoded, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	result := client.executeHermesUpdate(context.Background(), domain.Job{
		ID: "job-retry", Type: "instance.hermes.update", Payload: encoded, LeaseToken: "lease-retry",
	})
	if !result.Success || result.RecoverySHA256 != hex.EncodeToString(digest[:]) ||
		result.RecoverySizeBytes != int64(len(plaintext)) {
		t.Fatalf("Hermes update retry result=%+v", result)
	}
	for _, execution := range executor.executions {
		if execution == "instance.recovery.create" {
			t.Fatal("Hermes update retry created a second backup instead of reusing the verified one")
		}
	}
}

func TestHermesUpdateRestoresVerifiedBackupWhenTargetRestartFails(t *testing.T) {
	plaintext := bytes.Repeat([]byte("verified-rollback-backup"), 500)
	digest := sha256.Sum256(plaintext)
	pointID := "recovery-" + strings.Repeat("7", 32)
	currentImage := "runtime:0.18.2"
	targetImage := "runtime:0.19.0"
	executor := &hermesUpdateTestExecutor{
		t: t, root: t.TempDir(), plaintext: plaintext, pointID: pointID,
		currentImage: currentImage, targetImage: targetImage, failTargetStart: true,
		targetImageID: "sha256:" + strings.Repeat("8", 64),
	}
	client := New(Config{ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "host-token"}, executor)
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		assertLeaseHeader(t, request, "lease-rollback")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/agent/jobs/job-rollback/recovery-point":
			header := make(http.Header)
			header.Set("Content-Length", strconv.Itoa(len(plaintext)))
			header.Set("X-Fleet-Recovery-SHA256", hex.EncodeToString(digest[:]))
			return &http.Response{
				StatusCode: http.StatusOK, Header: header,
				Body: io.NopCloser(bytes.NewReader(plaintext)), Request: request,
			}, nil
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/agent/jobs/job-rollback/progress":
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		default:
			t.Fatalf("unexpected Hermes rollback request %s %s", request.Method, request.URL.Path)
			return nil, nil
		}
	})
	update := domain.HermesUpdatePayload{
		OriginalStatus: domain.InstanceRunning,
		Backup: domain.RecoveryPointPayload{
			RecoveryPointID: pointID, InstanceID: "instance-1", Name: "fleet-test-01", MaxBytes: 1 << 20,
		},
		Upgrade: domain.HermesUpgradePayload{
			InstanceID: "instance-1", Name: "fleet-test-01",
			CurrentImage: currentImage, CurrentImageID: "sha256:" + strings.Repeat("9", 64),
			TargetImage: targetImage, TargetVersion: "0.19.0",
			ProjectName: "hermes-fleet-test", ManagedPath: "/managed/fleet-test",
			APIPort: 8650, DashboardPort: 9130,
			Rollback: domain.RecoveryRestorePayload{RecoveryPointID: pointID, RequireImageID: true},
		},
	}
	encoded, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	result := client.executeHermesUpdate(context.Background(), domain.Job{
		ID: "job-rollback", Type: "instance.hermes.update", Payload: encoded, LeaseToken: "lease-rollback",
	})
	if result.Success || result.InstanceStatus != domain.InstanceRunning || result.ImageID != "" {
		t.Fatalf("Hermes rollback result=%+v", result)
	}
	if !strings.Contains(result.Error, "verified backup and original runtime were restored") {
		t.Fatalf("Hermes rollback error=%q", result.Error)
	}
	expectedExecutions := []string{
		"instance.hermes.prepare", "instance.stop", "instance.hermes.upgrade",
		"instance.start", "instance.stop", "instance.recovery.restore", "instance.start",
	}
	if strings.Join(executor.executions, ",") != strings.Join(expectedExecutions, ",") {
		t.Fatalf("executor calls=%v want=%v", executor.executions, expectedExecutions)
	}
}

func TestHermesUpdateDoesNotRestoreBackupWhileTargetRuntimeIsStillRunning(t *testing.T) {
	plaintext := bytes.Repeat([]byte("verified-stop-guard-backup"), 500)
	digest := sha256.Sum256(plaintext)
	pointID := "recovery-" + strings.Repeat("6", 32)
	currentImage := "runtime:0.18.2"
	targetImage := "runtime:0.19.0"
	executor := &hermesUpdateTestExecutor{
		t: t, root: t.TempDir(), plaintext: plaintext, pointID: pointID,
		currentImage: currentImage, targetImage: targetImage,
		failTargetStart: true, failTargetStop: true,
		targetImageID: "sha256:" + strings.Repeat("5", 64),
	}
	client := New(Config{ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "host-token"}, executor)
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		assertLeaseHeader(t, request, "lease-stop-guard")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/agent/jobs/job-stop-guard/recovery-point":
			header := make(http.Header)
			header.Set("Content-Length", strconv.Itoa(len(plaintext)))
			header.Set("X-Fleet-Recovery-SHA256", hex.EncodeToString(digest[:]))
			return &http.Response{
				StatusCode: http.StatusOK, Header: header,
				Body: io.NopCloser(bytes.NewReader(plaintext)), Request: request,
			}, nil
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/agent/jobs/job-stop-guard/progress":
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		default:
			t.Fatalf("unexpected guarded Hermes rollback request %s %s", request.Method, request.URL.Path)
			return nil, nil
		}
	})
	update := domain.HermesUpdatePayload{
		OriginalStatus: domain.InstanceRunning,
		Backup: domain.RecoveryPointPayload{
			RecoveryPointID: pointID, InstanceID: "instance-1", Name: "fleet-test-01", MaxBytes: 1 << 20,
		},
		Upgrade: domain.HermesUpgradePayload{
			InstanceID: "instance-1", Name: "fleet-test-01",
			CurrentImage: currentImage, CurrentImageID: "sha256:" + strings.Repeat("4", 64),
			TargetImage: targetImage, TargetVersion: "0.19.0",
			ProjectName: "hermes-fleet-test", ManagedPath: "/managed/fleet-test",
			APIPort: 8650, DashboardPort: 9130,
			Rollback: domain.RecoveryRestorePayload{RecoveryPointID: pointID, RequireImageID: true},
		},
	}
	encoded, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	result := client.executeHermesUpdate(context.Background(), domain.Job{
		ID: "job-stop-guard", Type: "instance.hermes.update", Payload: encoded, LeaseToken: "lease-stop-guard",
	})
	if result.Success || result.InstanceStatus != domain.InstanceFailed || result.ImageID != "" {
		t.Fatalf("guarded Hermes rollback result=%+v", result)
	}
	if !strings.Contains(result.Error, "automatic backup restore was not attempted") {
		t.Fatalf("guarded Hermes rollback error=%q", result.Error)
	}
	expectedExecutions := []string{
		"instance.hermes.prepare", "instance.stop", "instance.hermes.upgrade",
		"instance.start", "instance.stop",
	}
	if strings.Join(executor.executions, ",") != strings.Join(expectedExecutions, ",") {
		t.Fatalf("executor calls=%v want=%v", executor.executions, expectedExecutions)
	}
}

func TestHermesUpdateStopsEvenWhenOriginalStatusIsStopped(t *testing.T) {
	plaintext := bytes.Repeat([]byte("verified-stopped-update-backup"), 500)
	pointID := "recovery-" + strings.Repeat("1", 32)
	executor := &hermesUpdateTestExecutor{
		t: t, root: t.TempDir(), plaintext: plaintext, pointID: pointID,
		targetImageID: "sha256:" + strings.Repeat("2", 64),
	}
	client := New(Config{ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "host-token"}, executor)
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		assertLeaseHeader(t, request, "lease-stopped")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/agent/jobs/job-stopped/recovery-point":
			return testHTTPResponse(request, http.StatusNotFound, `{"error":"not ready"}`), nil
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/agent/jobs/job-stopped/progress":
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		case request.Method == http.MethodPut && request.URL.Path == "/api/v1/agent/jobs/job-stopped/recovery-point":
			if _, err := io.Copy(io.Discard, request.Body); err != nil {
				t.Fatal(err)
			}
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/agent/jobs/job-stopped/recovery-point/verify":
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		default:
			t.Fatalf("unexpected stopped Hermes update request %s %s", request.Method, request.URL.Path)
			return nil, nil
		}
	})
	update := domain.HermesUpdatePayload{
		OriginalStatus: domain.InstanceStopped,
		Backup: domain.RecoveryPointPayload{
			RecoveryPointID: pointID, InstanceID: "instance-1", Name: "fleet-test-01", MaxBytes: 1 << 20,
		},
		Upgrade: domain.HermesUpgradePayload{
			InstanceID: "instance-1", Name: "fleet-test-01",
			CurrentImage: "runtime:0.18.2", CurrentImageID: "sha256:" + strings.Repeat("a", 64),
			TargetImage: "runtime:0.19.0", TargetVersion: "0.19.0",
			ProjectName: "hermes-fleet-test", ManagedPath: "/managed/fleet-test",
			APIPort: 8650, DashboardPort: 9130,
			Rollback: domain.RecoveryRestorePayload{RecoveryPointID: pointID, RequireImageID: true},
		},
	}
	encoded, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	result := client.executeHermesUpdate(context.Background(), domain.Job{
		ID: "job-stopped", Type: "instance.hermes.update", Payload: encoded, LeaseToken: "lease-stopped",
	})
	if !result.Success || result.InstanceStatus != domain.InstanceStopped || result.ImageID != executor.targetImageID {
		t.Fatalf("stopped Hermes update result=%+v", result)
	}
	expectedExecutions := []string{"instance.hermes.prepare", "instance.stop", "instance.recovery.create", "instance.hermes.upgrade"}
	if strings.Join(executor.executions, ",") != strings.Join(expectedExecutions, ",") {
		t.Fatalf("executor calls=%v want=%v", executor.executions, expectedExecutions)
	}
}

func TestHermesUpdateCompletesAfterProgressFailureOnceInstallIsVerified(t *testing.T) {
	plaintext := bytes.Repeat([]byte("verified-progress-backup"), 500)
	pointID := "recovery-" + strings.Repeat("3", 32)
	executor := &hermesUpdateTestExecutor{
		t: t, root: t.TempDir(), plaintext: plaintext, pointID: pointID,
		targetImageID: "sha256:" + strings.Repeat("4", 64),
	}
	client := New(Config{ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "host-token"}, executor)
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		assertLeaseHeader(t, request, "lease-progress")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/agent/jobs/job-progress/recovery-point":
			return testHTTPResponse(request, http.StatusNotFound, `{"error":"not ready"}`), nil
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/agent/jobs/job-progress/progress":
			var progress domain.JobProgress
			if err := json.NewDecoder(request.Body).Decode(&progress); err != nil {
				t.Fatal(err)
			}
			if progress.Stage == "RESTORING_STATE" || progress.Stage == "VERIFYING_VERSION" {
				return testHTTPResponse(request, http.StatusInternalServerError, `{"error":"progress unavailable"}`), nil
			}
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		case request.Method == http.MethodPut && request.URL.Path == "/api/v1/agent/jobs/job-progress/recovery-point":
			if _, err := io.Copy(io.Discard, request.Body); err != nil {
				t.Fatal(err)
			}
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/agent/jobs/job-progress/recovery-point/verify":
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		default:
			t.Fatalf("unexpected progress Hermes update request %s %s", request.Method, request.URL.Path)
			return nil, nil
		}
	})
	update := domain.HermesUpdatePayload{
		OriginalStatus: domain.InstanceRunning,
		Backup: domain.RecoveryPointPayload{
			RecoveryPointID: pointID, InstanceID: "instance-1", Name: "fleet-test-01", MaxBytes: 1 << 20,
		},
		Upgrade: domain.HermesUpgradePayload{
			InstanceID: "instance-1", Name: "fleet-test-01",
			CurrentImage: "runtime:0.18.2", CurrentImageID: "sha256:" + strings.Repeat("a", 64),
			TargetImage: "runtime:0.19.0", TargetVersion: "0.19.0",
			ProjectName: "hermes-fleet-test", ManagedPath: "/managed/fleet-test",
			APIPort: 8650, DashboardPort: 9130,
			Rollback: domain.RecoveryRestorePayload{RecoveryPointID: pointID, RequireImageID: true},
		},
	}
	encoded, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	result := client.executeHermesUpdate(context.Background(), domain.Job{
		ID: "job-progress", Type: "instance.hermes.update", Payload: encoded, LeaseToken: "lease-progress",
	})
	if !result.Success || result.InstanceStatus != domain.InstanceRunning || result.ImageID != executor.targetImageID {
		t.Fatalf("progress-failed Hermes update result=%+v", result)
	}
	if !strings.Contains(result.Message, "progress could not be confirmed") {
		t.Fatalf("progress-failed Hermes update message=%q", result.Message)
	}
	expectedExecutions := []string{"instance.hermes.prepare", "instance.stop", "instance.recovery.create", "instance.hermes.upgrade", "instance.start"}
	if strings.Join(executor.executions, ",") != strings.Join(expectedExecutions, ",") {
		t.Fatalf("executor calls=%v want=%v", executor.executions, expectedExecutions)
	}
}

func TestHermesUpdateReportsInstalledImageWhenLeaseIsLostAfterInstall(t *testing.T) {
	plaintext := bytes.Repeat([]byte("verified-lease-backup"), 500)
	pointID := "recovery-" + strings.Repeat("5", 32)
	executor := &hermesUpdateTestExecutor{
		t: t, root: t.TempDir(), plaintext: plaintext, pointID: pointID,
		targetImageID: "sha256:" + strings.Repeat("6", 64),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := New(Config{ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "host-token"}, executor)
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		assertLeaseHeader(t, request, "lease-lost")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/agent/jobs/job-lost/recovery-point":
			return testHTTPResponse(request, http.StatusNotFound, `{"error":"not ready"}`), nil
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/agent/jobs/job-lost/progress":
			var progress domain.JobProgress
			if err := json.NewDecoder(request.Body).Decode(&progress); err != nil {
				t.Fatal(err)
			}
			if progress.Stage == "RESTORING_STATE" {
				cancel()
				return testHTTPResponse(request, http.StatusConflict, `{"error":"job lease is no longer active"}`), nil
			}
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		case request.Method == http.MethodPut && request.URL.Path == "/api/v1/agent/jobs/job-lost/recovery-point":
			if _, err := io.Copy(io.Discard, request.Body); err != nil {
				t.Fatal(err)
			}
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/agent/jobs/job-lost/recovery-point/verify":
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		default:
			t.Fatalf("unexpected lease-lost Hermes update request %s %s", request.Method, request.URL.Path)
			return nil, nil
		}
	})
	update := domain.HermesUpdatePayload{
		OriginalStatus: domain.InstanceRunning,
		Backup: domain.RecoveryPointPayload{
			RecoveryPointID: pointID, InstanceID: "instance-1", Name: "fleet-test-01", MaxBytes: 1 << 20,
		},
		Upgrade: domain.HermesUpgradePayload{
			InstanceID: "instance-1", Name: "fleet-test-01",
			CurrentImage: "runtime:0.18.2", CurrentImageID: "sha256:" + strings.Repeat("a", 64),
			TargetImage: "runtime:0.19.0", TargetVersion: "0.19.0",
			ProjectName: "hermes-fleet-test", ManagedPath: "/managed/fleet-test",
			APIPort: 8650, DashboardPort: 9130,
			Rollback: domain.RecoveryRestorePayload{RecoveryPointID: pointID, RequireImageID: true},
		},
	}
	encoded, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	result := client.executeHermesUpdate(ctx, domain.Job{
		ID: "job-lost", Type: "instance.hermes.update", Payload: encoded, LeaseToken: "lease-lost",
	})
	if result.Success || result.InstanceStatus != domain.InstanceStopped || result.ImageID != executor.targetImageID {
		t.Fatalf("lease-lost Hermes update result=%+v", result)
	}
	if !strings.Contains(result.Error, "installed but the job lease was lost") {
		t.Fatalf("lease-lost Hermes update error=%q", result.Error)
	}
	expectedExecutions := []string{"instance.hermes.prepare", "instance.stop", "instance.recovery.create", "instance.hermes.upgrade"}
	if strings.Join(executor.executions, ",") != strings.Join(expectedExecutions, ",") {
		t.Fatalf("executor calls=%v want=%v", executor.executions, expectedExecutions)
	}
}

func TestHermesUpdateStopsDockerWorkWhenTheControlPlaneRejectsTheLease(t *testing.T) {
	plaintext := bytes.Repeat([]byte("fenced-progress-backup"), 500)
	pointID := "recovery-" + strings.Repeat("7", 32)
	executor := &hermesUpdateTestExecutor{
		t: t, root: t.TempDir(), plaintext: plaintext, pointID: pointID,
		targetImageID: "sha256:" + strings.Repeat("8", 64),
	}
	client := New(Config{ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "host-token"}, executor)
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		assertLeaseHeader(t, request, "lease-fenced")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/agent/jobs/job-fenced/recovery-point":
			return testHTTPResponse(request, http.StatusNotFound, `{"error":"not ready"}`), nil
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/agent/jobs/job-fenced/progress":
			var progress domain.JobProgress
			if err := json.NewDecoder(request.Body).Decode(&progress); err != nil {
				t.Fatal(err)
			}
			// Lease ditolak tanpa membatalkan context eksekusi, meniru jendela
			// sebelum goroutine perpanjangan lease sempat bereaksi.
			if progress.Stage == "RESTORING_STATE" {
				return testHTTPResponse(request, http.StatusConflict, `{"error":"job lease is no longer active"}`), nil
			}
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		case request.Method == http.MethodPut && request.URL.Path == "/api/v1/agent/jobs/job-fenced/recovery-point":
			if _, err := io.Copy(io.Discard, request.Body); err != nil {
				t.Fatal(err)
			}
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/agent/jobs/job-fenced/recovery-point/verify":
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		default:
			t.Fatalf("unexpected fenced Hermes update request %s %s", request.Method, request.URL.Path)
			return nil, nil
		}
	})
	update := domain.HermesUpdatePayload{
		OriginalStatus: domain.InstanceRunning,
		Backup: domain.RecoveryPointPayload{
			RecoveryPointID: pointID, InstanceID: "instance-1", Name: "fleet-test-01", MaxBytes: 1 << 20,
		},
		Upgrade: domain.HermesUpgradePayload{
			InstanceID: "instance-1", Name: "fleet-test-01",
			CurrentImage: "runtime:0.18.2", CurrentImageID: "sha256:" + strings.Repeat("a", 64),
			TargetImage: "runtime:0.19.0", TargetVersion: "0.19.0",
			ProjectName: "hermes-fleet-test", ManagedPath: "/managed/fleet-test",
			APIPort: 8650, DashboardPort: 9130,
			Rollback: domain.RecoveryRestorePayload{RecoveryPointID: pointID, RequireImageID: true},
		},
	}
	encoded, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	result := client.executeHermesUpdate(context.Background(), domain.Job{
		ID: "job-fenced", Type: "instance.hermes.update", Payload: encoded, LeaseToken: "lease-fenced",
	})
	if result.Success || result.InstanceStatus != domain.InstanceStopped || result.ImageID != executor.targetImageID {
		t.Fatalf("fenced Hermes update result=%+v", result)
	}
	if !strings.Contains(result.Error, "installed but the job lease was lost") {
		t.Fatalf("fenced Hermes update error=%q", result.Error)
	}
	expectedExecutions := []string{"instance.hermes.prepare", "instance.stop", "instance.recovery.create", "instance.hermes.upgrade"}
	if strings.Join(executor.executions, ",") != strings.Join(expectedExecutions, ",") {
		t.Fatalf("fenced executor calls=%v want=%v", executor.executions, expectedExecutions)
	}
}

func TestHermesUpdateDoesNotRestartTheInstanceAfterTheLeaseIsRejectedEarly(t *testing.T) {
	plaintext := bytes.Repeat([]byte("fenced-backup-stage"), 500)
	pointID := "recovery-" + strings.Repeat("9", 32)
	executor := &hermesUpdateTestExecutor{
		t: t, root: t.TempDir(), plaintext: plaintext, pointID: pointID,
		targetImageID: "sha256:" + strings.Repeat("b", 64),
	}
	client := New(Config{ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "host-token"}, executor)
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/agent/jobs/job-early/progress":
			var progress domain.JobProgress
			if err := json.NewDecoder(request.Body).Decode(&progress); err != nil {
				t.Fatal(err)
			}
			if progress.Stage == "BACKING_UP" {
				return testHTTPResponse(request, http.StatusConflict, `{"error":"job lease is no longer active"}`), nil
			}
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		default:
			t.Fatalf("unexpected early fenced request %s %s", request.Method, request.URL.Path)
			return nil, nil
		}
	})
	update := domain.HermesUpdatePayload{
		OriginalStatus: domain.InstanceRunning,
		Backup: domain.RecoveryPointPayload{
			RecoveryPointID: pointID, InstanceID: "instance-1", Name: "fleet-test-01", MaxBytes: 1 << 20,
		},
		Upgrade: domain.HermesUpgradePayload{
			InstanceID: "instance-1", Name: "fleet-test-01",
			CurrentImage: "runtime:0.18.2", CurrentImageID: "sha256:" + strings.Repeat("a", 64),
			TargetImage: "runtime:0.19.0", TargetVersion: "0.19.0",
			ProjectName: "hermes-fleet-test", ManagedPath: "/managed/fleet-test",
			APIPort: 8650, DashboardPort: 9130,
			Rollback: domain.RecoveryRestorePayload{RecoveryPointID: pointID, RequireImageID: true},
		},
	}
	encoded, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	result := client.executeHermesUpdate(context.Background(), domain.Job{
		ID: "job-early", Type: "instance.hermes.update", Payload: encoded, LeaseToken: "lease-early",
	})
	if result.Success || result.InstanceStatus != domain.InstanceStopped {
		t.Fatalf("early fenced Hermes update result=%+v", result)
	}
	// Restart tidak boleh dijalankan: pekerja lain berhak atas instance ini.
	expectedExecutions := []string{"instance.hermes.prepare", "instance.stop"}
	if strings.Join(executor.executions, ",") != strings.Join(expectedExecutions, ",") {
		t.Fatalf("early fenced executor calls=%v want=%v", executor.executions, expectedExecutions)
	}
}

func TestRecoveryPointDownloadIsLeaseFencedAndVerified(t *testing.T) {
	plaintext := bytes.Repeat([]byte("verified-recovery-data"), 1000)
	digest := sha256.Sum256(plaintext)
	payload, err := json.Marshal(domain.RecoveryRestorePayload{
		RecoveryPointID: "recovery-" + strings.Repeat("b", 32),
		RecoverySHA256:  hex.EncodeToString(digest[:]), RecoverySizeBytes: int64(len(plaintext)), MaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	client := New(Config{ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "host-token"}, &observingExecutor{})
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/v1/agent/jobs/job-restore/recovery-point" || request.Method != http.MethodGet {
			t.Fatalf("unexpected download request %s %s", request.Method, request.URL.Path)
		}
		assertLeaseHeader(t, request, "lease-restore")
		if request.Header.Get("X-Fleet-Host-ID") != "host-1" || request.Header.Get("Authorization") != "Bearer host-token" {
			t.Fatalf("restore request headers=%v", request.Header)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(plaintext)), Request: request,
		}, nil
	})
	artifactPath, err := client.downloadRecoveryPoint(context.Background(), domain.Job{
		ID: "job-restore", LeaseToken: "lease-restore", Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(artifactPath)
	info, err := os.Stat(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || !bytes.Equal(contents, plaintext) {
		t.Fatalf("downloaded artifact mode=%o size=%d", info.Mode().Perm(), len(contents))
	}
}

func TestInitialHeartbeatRecoversAndLeaseRenewalContinuesDuringJobExecution(t *testing.T) {
	var heartbeatCount atomic.Int32
	var renewalCount atomic.Int32
	var claimed atomic.Bool
	completed := make(chan struct{}, 1)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/heartbeat":
			if heartbeatCount.Add(1) <= 2 {
				http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/agent/jobs/claim":
			if !claimed.CompareAndSwap(false, true) {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = json.NewEncoder(w).Encode(domain.Job{
				ID: "job-1", HostID: "host-1", InstanceID: "instance-1", Type: "instance.provision",
				Status: domain.JobLeased, LeaseToken: "lease-current", Payload: json.RawMessage(`{}`),
			})
		case "/api/v1/agent/jobs/job-1/ack":
			assertLeaseHeader(t, r, "lease-current")
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/agent/jobs/job-1/renew":
			assertLeaseHeader(t, r, "lease-current")
			renewalCount.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/agent/jobs/job-1/complete":
			assertLeaseHeader(t, r, "lease-current")
			completed <- struct{}{}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})

	executor := &blockingExecutor{started: make(chan struct{}), release: make(chan struct{})}
	client := New(Config{
		ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "token", Hostname: "host",
		PollInterval: 5 * time.Millisecond, HeartbeatInterval: 10 * time.Millisecond,
		LeaseRenewInterval: 10 * time.Millisecond, InitialRetryDelay: 5 * time.Millisecond,
	}, executor)
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := &responseRecorder{header: make(http.Header)}
		handler.ServeHTTP(recorder, request)
		return recorder.result(request), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()

	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("job execution did not start")
	}
	heartbeatsAtStart := heartbeatCount.Load()
	waitFor(t, time.Second, func() bool {
		return heartbeatCount.Load() >= heartbeatsAtStart+2 && renewalCount.Load() >= 1
	})
	close(executor.release)
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("job completion was not sent")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("client did not stop after cancellation")
	}
}

func TestRunDrainsActiveJobAfterShutdown(t *testing.T) {
	var claimCount atomic.Int32
	var renewalCount atomic.Int32
	var completionCount atomic.Int32
	job := domain.Job{
		ID: "job-drain", HostID: "host-1", InstanceID: "instance-1", Type: "instance.start",
		Status: domain.JobLeased, LeaseToken: "lease-drain", Payload: json.RawMessage(`{}`),
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/agent/heartbeat":
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/v1/agent/jobs/claim":
			if claimCount.Add(1) != 1 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = json.NewEncoder(w).Encode(job)
		case r.URL.Path == "/api/v1/agent/jobs/job-drain/ack":
			assertLeaseHeader(t, r, job.LeaseToken)
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/v1/agent/jobs/job-drain/renew":
			assertLeaseHeader(t, r, job.LeaseToken)
			renewalCount.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/v1/agent/jobs/job-drain/complete":
			assertLeaseHeader(t, r, job.LeaseToken)
			completionCount.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	executor := &blockingExecutor{started: make(chan struct{}), release: make(chan struct{})}
	client := New(Config{
		ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "token", Hostname: "host",
		PollInterval: 2 * time.Millisecond, HeartbeatInterval: time.Hour,
		LeaseRenewInterval: 5 * time.Millisecond, LeaseDuration: 200 * time.Millisecond,
		InitialRetryDelay: 2 * time.Millisecond, ShutdownGracePeriod: 500 * time.Millisecond,
		JobConcurrency: 1,
	}, executor)
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := &responseRecorder{header: make(http.Header)}
		handler.ServeHTTP(recorder, request)
		return recorder.result(request), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("job execution did not start")
	}

	cancel()
	select {
	case err := <-done:
		t.Fatalf("Run() returned before the active job drained: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	if renewalCount.Load() == 0 {
		t.Fatal("lease renewal stopped while the active job was draining")
	}
	close(executor.release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error=%v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not return after the active job drained")
	}
	if completionCount.Load() != 1 {
		t.Fatalf("completion requests=%d, want 1", completionCount.Load())
	}
}

func TestProcessNextDoesNotAcknowledgeJobAfterClaimCancellation(t *testing.T) {
	var acknowledgementCount atomic.Int32
	executor := &countingExecutor{}
	client := New(Config{
		ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "token", Hostname: "host",
	}, executor)
	claimContext, cancelClaim := context.WithCancel(context.Background())
	job, err := json.Marshal(domain.Job{
		ID: "job-canceled-before-ack", HostID: "host-1", InstanceID: "instance-1", Type: "instance.start",
		Status: domain.JobLeased, LeaseToken: "lease-canceled-before-ack", Payload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/v1/agent/jobs/claim":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(&cancelOnReadReader{
					reader: bytes.NewReader(job),
					cancel: cancelClaim,
				}),
				Request: request,
			}, nil
		case "/api/v1/agent/jobs/job-canceled-before-ack/ack":
			acknowledgementCount.Add(1)
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
			return nil, errors.New("unexpected request")
		}
	})

	err = client.processNextWithExecutionContext(claimContext, context.Background())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("processNextWithExecutionContext() error=%v, want context canceled", err)
	}
	if acknowledgementCount.Load() != 0 {
		t.Fatalf("acknowledgement requests=%d, want 0", acknowledgementCount.Load())
	}
	if executor.calls.Load() != 0 {
		t.Fatalf("executor calls=%d, want 0", executor.calls.Load())
	}
}

func TestRunCancelsActiveJobAfterShutdownGracePeriod(t *testing.T) {
	var claimCount atomic.Int32
	var completionCount atomic.Int32
	job := domain.Job{
		ID: "job-grace-timeout", HostID: "host-1", InstanceID: "instance-1", Type: "instance.start",
		Status: domain.JobLeased, LeaseToken: "lease-grace-timeout", Payload: json.RawMessage(`{}`),
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/agent/heartbeat":
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/v1/agent/jobs/claim":
			if claimCount.Add(1) != 1 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = json.NewEncoder(w).Encode(job)
		case r.URL.Path == "/api/v1/agent/jobs/job-grace-timeout/ack":
			assertLeaseHeader(t, r, job.LeaseToken)
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/v1/agent/jobs/job-grace-timeout/renew":
			assertLeaseHeader(t, r, job.LeaseToken)
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/v1/agent/jobs/job-grace-timeout/complete":
			completionCount.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	executor := &blockingExecutor{started: make(chan struct{}), release: make(chan struct{})}
	const gracePeriod = 40 * time.Millisecond
	client := New(Config{
		ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "token", Hostname: "host",
		PollInterval: 2 * time.Millisecond, HeartbeatInterval: time.Hour,
		LeaseRenewInterval: 5 * time.Millisecond, LeaseDuration: 200 * time.Millisecond,
		InitialRetryDelay: 2 * time.Millisecond, ShutdownGracePeriod: gracePeriod,
		JobConcurrency: 1,
	}, executor)
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := &responseRecorder{header: make(http.Header)}
		handler.ServeHTTP(recorder, request)
		return recorder.result(request), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("job execution did not start")
	}

	startedAt := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error=%v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not cancel the active job after the shutdown grace period")
	}
	if elapsed := time.Since(startedAt); elapsed < gracePeriod {
		t.Fatalf("Run() canceled the active job after %v, before grace period %v", elapsed, gracePeriod)
	}
	if completionCount.Load() != 0 {
		t.Fatalf("completion requests=%d, want 0 after forced shutdown cancellation", completionCount.Load())
	}
}

func TestRunExecutesDifferentInstanceJobsConcurrently(t *testing.T) {
	var claimCount atomic.Int32
	var completionCount atomic.Int32
	jobs := []domain.Job{
		{ID: "job-a", HostID: "host-1", InstanceID: "instance-a", Type: "instance.start", Status: domain.JobLeased, LeaseToken: "lease-a", Payload: json.RawMessage(`{}`)},
		{ID: "job-b", HostID: "host-1", InstanceID: "instance-b", Type: "instance.start", Status: domain.JobLeased, LeaseToken: "lease-b", Payload: json.RawMessage(`{}`)},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/agent/heartbeat":
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/v1/agent/jobs/claim":
			index := int(claimCount.Add(1)) - 1
			if index >= len(jobs) {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = json.NewEncoder(w).Encode(jobs[index])
		case strings.HasSuffix(r.URL.Path, "/ack"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/complete"):
			completionCount.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	executor := &concurrentExecutor{started: make(chan string, 2), release: make(chan struct{})}
	client := New(Config{
		ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "token", Hostname: "host",
		PollInterval: 5 * time.Millisecond, HeartbeatInterval: time.Hour, LeaseRenewInterval: time.Hour,
		InitialRetryDelay: 5 * time.Millisecond, JobConcurrency: 2,
	}, executor)
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := &responseRecorder{header: make(http.Header)}
		handler.ServeHTTP(recorder, request)
		return recorder.result(request), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	started := map[string]bool{}
	for len(started) < 2 {
		select {
		case instanceID := <-executor.started:
			started[instanceID] = true
		case <-time.After(time.Second):
			cancel()
			<-done
			t.Fatal("different-instance jobs did not start concurrently")
		}
	}
	if !started["instance-a"] || !started["instance-b"] {
		t.Fatalf("started instances=%v", started)
	}
	close(executor.release)
	waitFor(t, time.Second, func() bool { return completionCount.Load() == 2 })
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("concurrent client did not stop")
	}
}

func TestLeaseRenewalLossCancelsExecutorAndSuppressesCompletion(t *testing.T) {
	var completionCount atomic.Int32
	renewalAttempted := make(chan struct{}, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/jobs/claim":
			_ = json.NewEncoder(w).Encode(domain.Job{
				ID: "job-lease-loss", HostID: "host-1", InstanceID: "instance-1", Type: "instance.stop",
				Status: domain.JobLeased, LeaseToken: "lease-stale", Payload: json.RawMessage(`{}`),
			})
		case "/api/v1/agent/jobs/job-lease-loss/ack":
			assertLeaseHeader(t, r, "lease-stale")
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/agent/jobs/job-lease-loss/renew":
			assertLeaseHeader(t, r, "lease-stale")
			renewalAttempted <- struct{}{}
			http.Error(w, "job lease is no longer active", http.StatusConflict)
		case "/api/v1/agent/jobs/job-lease-loss/complete":
			completionCount.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})

	executor := &blockingExecutor{started: make(chan struct{}), release: make(chan struct{})}
	client := New(Config{
		ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "token", Hostname: "host",
		LeaseRenewInterval: 5 * time.Millisecond,
	}, executor)
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := &responseRecorder{header: make(http.Header)}
		handler.ServeHTTP(recorder, request)
		return recorder.result(request), nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.processNext(ctx) }()

	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("job execution did not start")
	}
	select {
	case <-renewalAttempted:
	case <-time.After(time.Second):
		t.Fatal("lease renewal was not attempted")
	}
	var processErr error
	select {
	case processErr = <-done:
	case <-time.After(250 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("terminal lease rejection did not cancel the executor")
	}
	if processErr == nil || !strings.Contains(processErr.Error(), "job lease lost") {
		t.Fatalf("processNext() error=%v, want job lease lost", processErr)
	}
	if completionCount.Load() != 0 {
		t.Fatalf("completion requests=%d, want 0 after lease loss", completionCount.Load())
	}
}

func TestTransientLeaseRenewalFailureRetriesWithoutCancelingExecution(t *testing.T) {
	var renewalCount atomic.Int32
	var completionCount atomic.Int32
	executor := &blockingExecutor{started: make(chan struct{}), release: make(chan struct{})}
	client := New(Config{
		ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "token",
		LeaseRenewInterval: 5 * time.Millisecond, LeaseDuration: 200 * time.Millisecond,
	}, executor)
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.URL.Path == "/api/v1/agent/jobs/claim":
			body, _ := json.Marshal(domain.Job{
				ID: "job-transient", HostID: "host-1", InstanceID: "instance-1", Type: "instance.start",
				Status: domain.JobLeased, LeaseToken: "lease-current", Payload: json.RawMessage(`{}`),
			})
			return testHTTPResponse(request, http.StatusOK, string(body)), nil
		case strings.HasSuffix(request.URL.Path, "/ack"):
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		case strings.HasSuffix(request.URL.Path, "/renew"):
			if renewalCount.Add(1) == 1 {
				return nil, errors.New("temporary connection failure")
			}
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		case strings.HasSuffix(request.URL.Path, "/complete"):
			completionCount.Add(1)
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		default:
			return testHTTPResponse(request, http.StatusNotFound, ""), nil
		}
	})

	done := make(chan error, 1)
	go func() { done <- client.processNext(context.Background()) }()
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("job execution did not start")
	}
	waitFor(t, time.Second, func() bool { return renewalCount.Load() >= 2 })
	select {
	case err := <-done:
		t.Fatalf("transient renewal failure canceled execution: %v", err)
	default:
	}
	close(executor.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("processNext() error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("job did not complete after lease renewal recovered")
	}
	if completionCount.Load() != 1 {
		t.Fatalf("completion requests=%d, want 1", completionCount.Load())
	}
}

func TestCompletionRetriesTransportAndServerFailuresWithoutReexecutingJob(t *testing.T) {
	var completionAttempts atomic.Int32
	var completionStarted atomic.Bool
	var renewalsDuringCompletion atomic.Int32
	executor := &countingExecutor{}
	client := New(Config{
		ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "token",
		LeaseRenewInterval: 2 * time.Millisecond, LeaseDuration: 200 * time.Millisecond,
		InitialRetryDelay: 5 * time.Millisecond,
	}, executor)
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.URL.Path == "/api/v1/agent/jobs/claim":
			body, _ := json.Marshal(domain.Job{
				ID: "job-completion-retry", HostID: "host-1", InstanceID: "instance-1", Type: "instance.start",
				Status: domain.JobLeased, LeaseToken: "lease-current", Payload: json.RawMessage(`{}`),
			})
			return testHTTPResponse(request, http.StatusOK, string(body)), nil
		case strings.HasSuffix(request.URL.Path, "/ack"):
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		case strings.HasSuffix(request.URL.Path, "/renew"):
			if completionStarted.Load() {
				renewalsDuringCompletion.Add(1)
			}
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		case strings.HasSuffix(request.URL.Path, "/complete"):
			completionStarted.Store(true)
			switch completionAttempts.Add(1) {
			case 1:
				return testHTTPResponse(request, http.StatusInternalServerError, "temporary completion failure"), nil
			case 2:
				return nil, errors.New("temporary completion transport failure")
			default:
				return testHTTPResponse(request, http.StatusNoContent, ""), nil
			}
		default:
			return testHTTPResponse(request, http.StatusNotFound, ""), nil
		}
	})

	if err := client.processNext(context.Background()); err != nil {
		t.Fatalf("processNext() error=%v", err)
	}
	if executor.calls.Load() != 1 {
		t.Fatalf("executor calls=%d, want 1", executor.calls.Load())
	}
	if completionAttempts.Load() != 3 {
		t.Fatalf("completion attempts=%d, want 3", completionAttempts.Load())
	}
	if renewalsDuringCompletion.Load() == 0 {
		t.Fatal("lease renewal stopped while completion was being retried")
	}
}

func TestCompletionConflictStopsRetrying(t *testing.T) {
	var completionAttempts atomic.Int32
	client := New(Config{
		ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "token",
		InitialRetryDelay: time.Millisecond,
	}, &observingExecutor{})
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		completionAttempts.Add(1)
		return testHTTPResponse(request, http.StatusConflict, "job lease is no longer active"), nil
	})

	job := domain.Job{ID: "job-conflict", LeaseToken: "lease-stale"}
	err := client.completeJob(context.Background(), job, domain.JobResult{Success: true})
	if !errors.Is(err, errJobLeaseLost) {
		t.Fatalf("completeJob() error=%v, want %v", err, errJobLeaseLost)
	}
	if completionAttempts.Load() != 1 {
		t.Fatalf("completion attempts=%d, want 1", completionAttempts.Load())
	}
}

func TestRecoveryCompletionUsesExtendedRequestTimeout(t *testing.T) {
	client := New(Config{
		ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "token",
	}, &observingExecutor{})
	client.httpClient.Timeout = 5 * time.Millisecond
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		deadline, ok := request.Context().Deadline()
		if !ok {
			t.Fatal("recovery completion request has no timeout")
		}
		if remaining := time.Until(deadline); remaining < 2*time.Hour {
			t.Fatalf("recovery completion timeout=%v, want at least 2h", remaining)
		}
		return testHTTPResponse(request, http.StatusNoContent, ""), nil
	})
	job := domain.Job{
		ID: "job-recovery-complete", Type: "instance.recovery.create", LeaseToken: "lease-recovery",
	}
	if err := client.completeJob(context.Background(), job, domain.JobResult{Success: true}); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentTransientLeaseRenewalFailureCancelsBeforeLeaseExpiry(t *testing.T) {
	executor := &blockingExecutor{started: make(chan struct{}), release: make(chan struct{})}
	client := New(Config{
		ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "token",
		LeaseRenewInterval: 5 * time.Millisecond, LeaseDuration: 40 * time.Millisecond,
	}, executor)
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.URL.Path == "/api/v1/agent/jobs/claim":
			body, _ := json.Marshal(domain.Job{
				ID: "job-persistent", HostID: "host-1", InstanceID: "instance-1", Type: "instance.start",
				Status: domain.JobLeased, LeaseToken: "lease-current", Payload: json.RawMessage(`{}`),
			})
			return testHTTPResponse(request, http.StatusOK, string(body)), nil
		case strings.HasSuffix(request.URL.Path, "/ack"):
			return testHTTPResponse(request, http.StatusNoContent, ""), nil
		case strings.HasSuffix(request.URL.Path, "/renew"):
			return nil, errors.New("persistent connection failure")
		default:
			return testHTTPResponse(request, http.StatusNotFound, ""), nil
		}
	})

	startedAt := time.Now()
	done := make(chan error, 1)
	go func() { done <- client.processNext(context.Background()) }()
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("job execution did not start")
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "job lease lost") {
			t.Fatalf("processNext() error=%v, want job lease lost", err)
		}
		if elapsed := time.Since(startedAt); elapsed >= client.config.LeaseDuration {
			t.Fatalf("lease cancellation took %v, want before %v", elapsed, client.config.LeaseDuration)
		}
	case <-time.After(time.Second):
		t.Fatal("persistent renewal failures did not cancel execution")
	}
}

func TestObservationLoopReportsOnlyControlPlaneTargets(t *testing.T) {
	target := domain.ObservationTarget{
		InstanceID: "00000000-0000-4000-8000-000000000001", Name: "fleet-test-01",
		DesiredStatus: domain.InstanceRunning, ProjectName: "hermes-fleet-fleet-test-01-00000000",
		DataVolume: "hermes-fleet-fleet-test-01-00000000-data", ManagedPath: "/managed/fleet-test-01-00000000",
		Generation: "generation-1", RefreshRequestID: "refresh-1",
	}
	reported := make(chan domain.InstanceObservation, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/heartbeat":
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/agent/jobs/claim":
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/agent/observations/targets":
			_ = json.NewEncoder(w).Encode(map[string]any{"targets": []domain.ObservationTarget{target}})
		case "/api/v1/agent/observations":
			var payload struct {
				Observations []domain.InstanceObservation `json:"observations"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if len(payload.Observations) != 1 {
				http.Error(w, "expected one observation", http.StatusBadRequest)
				return
			}
			select {
			case reported <- payload.Observations[0]:
			default:
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	executor := &observingExecutor{}
	client := New(Config{
		ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "token", Hostname: "host",
		PollInterval: 50 * time.Millisecond, HeartbeatInterval: 50 * time.Millisecond,
		ObservationInterval: 5 * time.Millisecond, InitialRetryDelay: 5 * time.Millisecond,
	}, executor)
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := &responseRecorder{header: make(http.Header)}
		handler.ServeHTTP(recorder, request)
		return recorder.result(request), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	select {
	case observation := <-reported:
		if observation.InstanceID != target.InstanceID || observation.TargetGeneration != target.Generation || observation.RefreshRequestID != target.RefreshRequestID {
			t.Fatalf("reported observation=%+v target=%+v", observation, target)
		}
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("observation was not reported")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("client did not stop after cancellation")
	}
}

func TestObserveTargetsRunsWithBoundedConcurrencyAndIsolatesReportFailures(t *testing.T) {
	targets := []domain.ObservationTarget{
		{InstanceID: "instance-1", Generation: "generation-1"},
		{InstanceID: "instance-2", Generation: "generation-2"},
		{InstanceID: "instance-3", Generation: "generation-3"},
	}
	reported := make(chan string, len(targets))
	var reportRequests atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/observations/targets":
			_ = json.NewEncoder(w).Encode(map[string]any{"targets": targets})
		case "/api/v1/agent/observations":
			reportRequests.Add(1)
			var payload struct {
				Observations []domain.InstanceObservation `json:"observations"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if len(payload.Observations) != 1 {
				http.Error(w, "expected one observation per report", http.StatusBadRequest)
				return
			}
			instanceID := payload.Observations[0].InstanceID
			if instanceID == "instance-2" {
				http.Error(w, "rejected test observation", http.StatusServiceUnavailable)
				return
			}
			reported <- instanceID
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	executor := &concurrentObservingExecutor{
		started: make(chan string, len(targets)),
		release: make(chan struct{}),
	}
	client := New(Config{
		ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "token",
		ObservationConcurrency: 2,
	}, executor)
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := &responseRecorder{header: make(http.Header)}
		handler.ServeHTTP(recorder, request)
		return recorder.result(request), nil
	})

	done := make(chan error, 1)
	go func() { done <- client.observeTargets(context.Background()) }()
	for range 2 {
		select {
		case <-executor.started:
		case <-time.After(time.Second):
			t.Fatal("observation workers did not run concurrently")
		}
	}
	select {
	case third := <-executor.started:
		t.Fatalf("third observation %q started before a worker was released", third)
	default:
	}
	close(executor.release)

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "instance-2") {
			t.Fatalf("observeTargets() error = %v, want isolated instance-2 report failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("observations did not finish")
	}
	if reportRequests.Load() != int32(len(targets)) {
		t.Fatalf("report requests = %d, want %d", reportRequests.Load(), len(targets))
	}
	close(reported)
	got := map[string]bool{}
	for instanceID := range reported {
		got[instanceID] = true
	}
	if !got["instance-1"] || !got["instance-3"] || got["instance-2"] {
		t.Fatalf("successfully reported instances = %v, want instance-1 and instance-3", got)
	}
}

func TestRuntimeMutationsRequestAnImmediateObservation(t *testing.T) {
	for _, jobType := range []string{
		"instance.start",
		"instance.stop",
		"instance.restart",
		"instance.reconcile",
		"instance.hermes.update",
		"instance.runtime.configure",
		"instance.messaging.configure",
		"instance.mcp.configure",
		"instance.recovery.restore",
		domain.JobRepairHermesProfiles,
	} {
		if !jobNeedsImmediateObservation(jobType) {
			t.Errorf("jobNeedsImmediateObservation(%q) = false", jobType)
		}
	}
	if jobNeedsImmediateObservation("instance.credentials.reveal") {
		t.Fatal("credential reveal must not trigger a runtime observation")
	}

	client := New(Config{}, &observingExecutor{})
	client.requestImmediateObservation()
	select {
	case <-client.observationWake:
	default:
		t.Fatal("immediate observation wake was not queued")
	}
}

func TestProcessNextReportsProgressWithTheActiveLease(t *testing.T) {
	job := domain.Job{ID: "job-auth", OperationID: "operation-auth", HostID: "host-1", InstanceID: "instance-1", Type: "instance.auth.codex", Status: domain.JobLeased, LeaseToken: "lease-1", Payload: json.RawMessage(`{}`)}
	progressReceived := make(chan domain.JobProgress, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/jobs/claim":
			_ = json.NewEncoder(w).Encode(job)
		case "/api/v1/agent/jobs/job-auth/ack":
			if r.Header.Get(leaseTokenHeader) != job.LeaseToken {
				http.Error(w, "missing lease", http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/agent/jobs/job-auth/progress":
			if r.Header.Get(leaseTokenHeader) != job.LeaseToken {
				http.Error(w, "missing lease", http.StatusConflict)
				return
			}
			var progress domain.JobProgress
			if err := json.NewDecoder(r.Body).Decode(&progress); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			progressReceived <- progress
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/agent/jobs/job-auth/complete":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := New(Config{ControlPlaneURL: server.URL, HostID: "host-1", HostToken: "token", LeaseRenewInterval: time.Hour}, progressTestExecutor{})
	if err := client.processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case progress := <-progressReceived:
		if progress.Stage != "STARTING" {
			t.Fatalf("reported progress=%+v", progress)
		}
	default:
		t.Fatal("progress-capable executor did not report progress")
	}
}

func TestProcessNextDownloadsChatInputOnlyWithTheActiveLease(t *testing.T) {
	job := domain.Job{
		ID: "job-chat", OperationID: "operation-chat", HostID: "host-1", InstanceID: "instance-1",
		Type: "instance.chat.send", Status: domain.JobLeased, LeaseToken: "lease-chat", Payload: json.RawMessage(`{}`),
	}
	executor := &chatInputExecutor{}
	client := New(Config{
		ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "host-token", LeaseRenewInterval: time.Hour,
	}, executor)
	var completed domain.JobResult
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer host-token" || request.Header.Get("X-Fleet-Host-ID") != "host-1" {
			t.Fatalf("missing Host Agent authentication headers: %v", request.Header)
		}
		response := &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}
		switch request.URL.Path {
		case "/api/v1/agent/jobs/claim":
			response.StatusCode = http.StatusOK
			encoded, err := json.Marshal(job)
			if err != nil {
				t.Fatal(err)
			}
			response.Body = io.NopCloser(bytes.NewReader(encoded))
		case "/api/v1/agent/jobs/job-chat/ack":
			assertLeaseHeader(t, request, job.LeaseToken)
		case "/api/v1/agent/jobs/job-chat/chat-input":
			assertLeaseHeader(t, request, job.LeaseToken)
			response.StatusCode = http.StatusOK
			response.Body = io.NopCloser(strings.NewReader("private chat prompt"))
		case "/api/v1/agent/jobs/job-chat/complete":
			assertLeaseHeader(t, request, job.LeaseToken)
			if err := json.NewDecoder(request.Body).Decode(&completed); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		return response, nil
	})
	if err := client.processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if executor.input != "private chat prompt" {
		t.Fatalf("executor input=%q", executor.input)
	}
	if !completed.Success || completed.ChatMessage != "assistant response" {
		t.Fatalf("completed result=%+v", completed)
	}
}

func TestReportChatEventUsesLeaseAndTreatsReplayAsSuccess(t *testing.T) {
	client := New(Config{ControlPlaneURL: "http://fleet.test", HostID: "host-1", HostToken: "host-token"}, &chatInputExecutor{})
	job := domain.Job{ID: "job-chat-event", LeaseToken: "lease-chat-event"}
	var received domain.ChatStreamEvent
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/v1/agent/jobs/job-chat-event/chat-events" || request.Header.Get(leaseTokenHeader) != job.LeaseToken {
			t.Fatalf("request=%s headers=%v", request.URL.Path, request.Header)
		}
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})
	event := domain.ChatStreamEvent{Sequence: 2, Type: domain.ChatEventDelta, Content: "hello"}
	if err := client.reportChatEvent(context.Background(), job, event); err != nil {
		t.Fatal(err)
	}
	if received != event {
		t.Fatalf("received=%+v want=%+v", received, event)
	}
}

type chatInputExecutor struct {
	input string
}

func (executor *chatInputExecutor) Execute(_ context.Context, job domain.Job) domain.JobResult {
	executor.input = string(job.InputSecret)
	return domain.JobResult{Success: true, ChatMessage: "assistant response"}
}

type hermesUpdateTestExecutor struct {
	t               *testing.T
	root            string
	plaintext       []byte
	pointID         string
	targetImageID   string
	currentImage    string
	targetImage     string
	failTargetStart bool
	failTargetStop  bool
	executions      []string
}

func (executor *hermesUpdateTestExecutor) Execute(_ context.Context, job domain.Job) domain.JobResult {
	executor.executions = append(executor.executions, job.Type)
	switch job.Type {
	case "instance.hermes.prepare":
		return domain.JobResult{Success: true}
	case "instance.stop":
		var payload domain.ActionPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			executor.t.Fatal(err)
		}
		if executor.failTargetStop && payload.Image == executor.targetImage {
			return domain.JobResult{Success: false, Error: "target runtime containers could not be stopped"}
		}
		return domain.JobResult{Success: true}
	case "instance.start":
		var payload domain.ActionPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			executor.t.Fatal(err)
		}
		if executor.failTargetStart && payload.Image == executor.targetImage {
			return domain.JobResult{Success: false, Error: "target runtime health check failed"}
		}
		if executor.failTargetStart && payload.Image != executor.currentImage {
			executor.t.Fatalf("unexpected rollback start image %q", payload.Image)
		}
		return domain.JobResult{Success: true}
	case "instance.recovery.create":
		key := bytes.Repeat([]byte{0x55}, 32)
		artifact, err := os.CreateTemp(executor.root, "update-backup-")
		if err != nil {
			executor.t.Fatal(err)
		}
		if err := artifact.Chmod(0o600); err != nil {
			executor.t.Fatal(err)
		}
		if _, err := recoverycodec.Encrypt(
			context.Background(), artifact, bytes.NewReader(executor.plaintext), key, executor.pointID+":artifact",
		); err != nil {
			executor.t.Fatal(err)
		}
		if err := artifact.Close(); err != nil {
			executor.t.Fatal(err)
		}
		digest := sha256.Sum256(executor.plaintext)
		return domain.JobResult{
			Success: true, RecoveryPointID: executor.pointID,
			RecoverySHA256: hex.EncodeToString(digest[:]), RecoverySizeBytes: int64(len(executor.plaintext)),
			RecoveryArtifact: artifact.Name(), RecoveryKey: key,
		}
	case "instance.hermes.upgrade":
		backup, err := os.ReadFile(job.InputArtifact)
		if err != nil {
			executor.t.Fatal(err)
		}
		if !bytes.Equal(backup, executor.plaintext) {
			executor.t.Fatal("Hermes install did not receive the verified backup")
		}
		return domain.JobResult{Success: true, ImageID: executor.targetImageID, InstanceStatus: domain.InstanceStopped}
	case "instance.recovery.restore":
		backup, err := os.ReadFile(job.InputArtifact)
		if err != nil {
			executor.t.Fatal(err)
		}
		if !bytes.Equal(backup, executor.plaintext) {
			executor.t.Fatal("Hermes rollback did not receive the verified backup")
		}
		var payload domain.RecoveryRestorePayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			executor.t.Fatal(err)
		}
		if !payload.RequireImageID {
			executor.t.Fatal("Hermes update rollback did not require the recorded image identity")
		}
		return domain.JobResult{Success: true, ImageID: payload.ImageID, InstanceStatus: domain.InstanceStopped}
	default:
		return domain.JobResult{Success: false, Error: "unexpected executor job " + job.Type}
	}
}

type progressTestExecutor struct{}

func (progressTestExecutor) Execute(context.Context, domain.Job) domain.JobResult {
	return domain.JobResult{Success: false, Error: "unexpected legacy execution"}
}

func (progressTestExecutor) ExecuteWithProgress(ctx context.Context, _ domain.Job, report func(context.Context, domain.JobProgress) error) domain.JobResult {
	if err := report(ctx, domain.JobProgress{Stage: "STARTING"}); err != nil {
		return domain.JobResult{Success: false, Error: err.Error()}
	}
	return domain.JobResult{Success: true}
}

type blockingExecutor struct {
	started chan struct{}
	release chan struct{}
}

type concurrentExecutor struct {
	started chan string
	release chan struct{}
}

func (e *concurrentExecutor) Execute(ctx context.Context, job domain.Job) domain.JobResult {
	e.started <- job.InstanceID
	select {
	case <-e.release:
		return domain.JobResult{Success: true}
	case <-ctx.Done():
		return domain.JobResult{Success: false, Error: ctx.Err().Error()}
	}
}

type observingExecutor struct{}

type concurrentObservingExecutor struct {
	started chan string
	release chan struct{}
}

func (executor *concurrentObservingExecutor) Execute(context.Context, domain.Job) domain.JobResult {
	return domain.JobResult{Success: true}
}

func (executor *concurrentObservingExecutor) Observe(ctx context.Context, target domain.ObservationTarget) domain.InstanceObservation {
	executor.started <- target.InstanceID
	select {
	case <-executor.release:
	case <-ctx.Done():
	}
	return domain.InstanceObservation{
		Status:     domain.ObservationInSync,
		Checks:     []domain.ObservationCheck{{Name: "runtime", Status: domain.ObservationCheckOK}},
		ObservedAt: time.Now().UTC(),
	}
}

type countingExecutor struct {
	calls atomic.Int32
}

func (executor *countingExecutor) Execute(context.Context, domain.Job) domain.JobResult {
	executor.calls.Add(1)
	return domain.JobResult{Success: true}
}

func (executor *observingExecutor) Execute(context.Context, domain.Job) domain.JobResult {
	return domain.JobResult{Success: true}
}

func (executor *observingExecutor) Observe(_ context.Context, target domain.ObservationTarget) domain.InstanceObservation {
	return domain.InstanceObservation{
		InstanceID: target.InstanceID, TargetGeneration: target.Generation, RefreshRequestID: target.RefreshRequestID,
		Status: domain.ObservationInSync, Summary: "Runtime matches desired state",
		Checks: []domain.ObservationCheck{{Name: "runtime", Status: domain.ObservationCheckOK, Detail: "Running"}}, ObservedAt: time.Now().UTC(),
	}
}

func (e *blockingExecutor) Execute(ctx context.Context, _ domain.Job) domain.JobResult {
	close(e.started)
	select {
	case <-e.release:
		return domain.JobResult{Success: true}
	case <-ctx.Done():
		return domain.JobResult{Success: false, Error: ctx.Err().Error()}
	}
}

func assertLeaseHeader(t *testing.T, request *http.Request, expected string) {
	t.Helper()
	if actual := request.Header.Get(leaseTokenHeader); actual != expected {
		t.Errorf("lease token header = %q, want %q", actual, expected)
	}
}

func testHTTPResponse(request *http.Request, statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type cancelOnReadReader struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (reader *cancelOnReadReader) Read(buffer []byte) (int, error) {
	reader.cancel()
	return reader.reader.Read(buffer)
}

func testHTTPClient(handler http.Handler) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := &responseRecorder{header: make(http.Header)}
		handler.ServeHTTP(recorder, request)
		return recorder.result(request), nil
	})}
}

type responseRecorder struct {
	header http.Header
	status int
	body   []byte
}

func (r *responseRecorder) Header() http.Header { return r.header }

func (r *responseRecorder) WriteHeader(status int) { r.status = status }

func (r *responseRecorder) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.body = append(r.body, body...)
	return len(body), nil
}

func (r *responseRecorder) result(request *http.Request) *http.Response {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return &http.Response{
		StatusCode: r.status,
		Header:     r.header,
		Body:       io.NopCloser(bytes.NewReader(r.body)),
		Request:    request,
	}
}
