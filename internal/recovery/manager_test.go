package recovery

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestManagerEncryptsVerifiesStreamsAndDeletesRecoveryPoint(t *testing.T) {
	root := t.TempDir()
	manager, err := New(root, strings.Repeat("01", 32), 3, 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	reservation := testReservation()
	metadata, err := manager.Reserve(context.Background(), reservation)
	if err != nil {
		t.Fatal(err)
	}
	archive := testArchive(t, metadata, "dashboard-password=secret-value")
	digest := sha256.Sum256(archive)
	encodedDigest := hex.EncodeToString(digest[:])
	uploaded, err := manager.Upload(
		context.Background(), metadata.ID, reservation.HostID, reservation.JobID,
		encodedDigest, int64(len(archive)), bytes.NewReader(archive), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if uploaded.Status != StatusUploaded {
		t.Fatalf("uploaded status=%q", uploaded.Status)
	}
	encrypted, err := os.ReadFile(manager.artifactPath(metadata.ID))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte("secret-value")) {
		t.Fatal("encrypted artifact contains plaintext instance secret")
	}
	ready, err := manager.VerifyUploaded(
		context.Background(), metadata.ID, reservation.HostID, reservation.JobID, encodedDigest, int64(len(archive)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Status != StatusReady || ready.VerifiedAt.IsZero() {
		t.Fatalf("ready metadata=%+v", ready)
	}
	repeated, err := manager.Upload(
		context.Background(), metadata.ID, reservation.HostID, reservation.JobID,
		encodedDigest, int64(len(archive)), bytes.NewReader(archive), nil,
	)
	if err != nil || repeated.Status != StatusReady {
		t.Fatalf("idempotent upload metadata=%+v err=%v", repeated, err)
	}
	var downloaded bytes.Buffer
	if _, err := manager.Stream(context.Background(), metadata.ID, &downloaded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(downloaded.Bytes(), archive) {
		t.Fatal("downloaded recovery archive does not match upload")
	}
	if err := manager.Delete(context.Background(), metadata.ID, "wrong"); err != ErrConfirmation {
		t.Fatalf("delete error=%v want=%v", err, ErrConfirmation)
	}
	if err := manager.Delete(context.Background(), metadata.ID, metadata.Filename); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Get(metadata.ID); err != ErrNotFound {
		t.Fatalf("get deleted recovery point error=%v", err)
	}
}

func TestManagerRejectsArchiveThatDoesNotMatchReservation(t *testing.T) {
	manager, err := New(t.TempDir(), strings.Repeat("02", 32), 3, 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	reservation := testReservation()
	metadata, err := manager.Reserve(context.Background(), reservation)
	if err != nil {
		t.Fatal(err)
	}
	archive := testArchive(t, metadata, "secret")
	archive[0] ^= 0x01
	digest := sha256.Sum256(archive)
	encodedDigest := hex.EncodeToString(digest[:])
	if _, err := manager.Upload(context.Background(), metadata.ID, reservation.HostID, reservation.JobID, encodedDigest, int64(len(archive)), bytes.NewReader(archive), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.VerifyUploaded(context.Background(), metadata.ID, reservation.HostID, reservation.JobID, encodedDigest, int64(len(archive))); err == nil {
		t.Fatal("invalid recovery archive passed verification")
	}
}

func TestLongUploadDoesNotBlockRecoveryPointListing(t *testing.T) {
	manager, err := New(t.TempDir(), strings.Repeat("03", 32), 3, 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	reservation := testReservation()
	metadata, err := manager.Reserve(context.Background(), reservation)
	if err != nil {
		t.Fatal(err)
	}
	archive := testArchive(t, metadata, "secret")
	digest := sha256.Sum256(archive)
	reader := &gatedReader{
		reader: bytes.NewReader(archive), started: make(chan struct{}), release: make(chan struct{}),
	}
	uploadDone := make(chan error, 1)
	go func() {
		_, uploadErr := manager.Upload(
			context.Background(), metadata.ID, reservation.HostID, reservation.JobID,
			hex.EncodeToString(digest[:]), int64(len(archive)), reader, nil,
		)
		uploadDone <- uploadErr
	}()
	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatal("upload did not start")
	}
	listed := make(chan error, 1)
	go func() {
		_, listErr := manager.List(context.Background(), reservation.InstanceID)
		listed <- listErr
	}()
	select {
	case err := <-listed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("recovery point listing was blocked by a streaming upload")
	}
	close(reader.release)
	if err := <-uploadDone; err != nil {
		t.Fatal(err)
	}
}

func TestUploadRechecksLeaseBeforePublishingArtifact(t *testing.T) {
	manager, err := New(t.TempDir(), strings.Repeat("04", 32), 3, 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	reservation := testReservation()
	metadata, err := manager.Reserve(context.Background(), reservation)
	if err != nil {
		t.Fatal(err)
	}
	archive := testArchive(t, metadata, "secret")
	digest := sha256.Sum256(archive)
	leaseLost := errors.New("lease lost during upload")
	_, err = manager.Upload(
		context.Background(), metadata.ID, reservation.HostID, reservation.JobID,
		hex.EncodeToString(digest[:]), int64(len(archive)), bytes.NewReader(archive),
		func(context.Context) error { return leaseLost },
	)
	if !errors.Is(err, leaseLost) {
		t.Fatalf("Upload() error=%v, want commit fence failure", err)
	}
	if _, err := os.Stat(manager.artifactPath(metadata.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact was published after lease loss: %v", err)
	}
	stored, err := manager.Get(metadata.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusCreating {
		t.Fatalf("metadata status=%q, want %q", stored.Status, StatusCreating)
	}
}

func TestManagerReconcilesArtifactPublishedBeforeMetadata(t *testing.T) {
	root := t.TempDir()
	key := strings.Repeat("05", 32)
	manager, err := New(root, key, 3, 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := manager.Reserve(context.Background(), testReservation())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.artifactPath(metadata.ID), []byte("interrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(root, key, 3, 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(restarted.artifactPath(metadata.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted artifact was not removed: %v", err)
	}
	stored, err := restarted.Get(metadata.ID)
	if err != nil || stored.Status != StatusCreating {
		t.Fatalf("reservation was not preserved for a safe retry: metadata=%+v error=%v", stored, err)
	}
}

func TestReserveReclaimsStaleCreatingReservation(t *testing.T) {
	manager, err := New(t.TempDir(), strings.Repeat("06", 32), 1, 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC()
	manager.now = func() time.Time { return started }
	stale, err := manager.Reserve(context.Background(), testReservation())
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return started.Add(25 * time.Hour) }
	replacement, err := manager.Reserve(context.Background(), testReservation())
	if err != nil {
		t.Fatalf("Reserve() did not reclaim stale reservation: %v", err)
	}
	if replacement.ID == stale.ID {
		t.Fatal("Reserve() reused a stale recovery identity")
	}
	if _, err := manager.Get(stale.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale reservation still exists: %v", err)
	}
}

func TestResetForRetryPreservesReadyRecoveryPoint(t *testing.T) {
	manager, err := New(t.TempDir(), strings.Repeat("07", 32), 1, 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	reservation := testReservation()
	ready := uploadAndVerifyRecoveryPoint(t, manager, reservation)
	metadataBefore, err := os.ReadFile(manager.metadataPath(ready.ID))
	if err != nil {
		t.Fatal(err)
	}
	artifactBefore, err := os.ReadFile(manager.artifactPath(ready.ID))
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.ResetForRetry(ready.ID, reservation.HostID, reservation.JobID); !errors.Is(err, ErrState) {
		t.Fatalf("ResetForRetry(READY) error=%v, want %v", err, ErrState)
	}
	if err := manager.AbortTerminal(ready.ID, reservation.HostID, reservation.JobID); !errors.Is(err, ErrState) {
		t.Fatalf("AbortTerminal(READY) error=%v, want %v", err, ErrState)
	}

	metadataAfter, err := os.ReadFile(manager.metadataPath(ready.ID))
	if err != nil {
		t.Fatal(err)
	}
	artifactAfter, err := os.ReadFile(manager.artifactPath(ready.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(metadataAfter, metadataBefore) {
		t.Fatal("READY metadata changed after rejected retry or terminal abort")
	}
	if !bytes.Equal(artifactAfter, artifactBefore) {
		t.Fatal("READY artifact changed after rejected retry or terminal abort")
	}
	stored, err := manager.Get(ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusReady || stored.SHA256 != ready.SHA256 || stored.VerifiedAt != ready.VerifiedAt {
		t.Fatalf("READY recovery point was mutated: before=%+v after=%+v", ready, stored)
	}
}

func TestAbortTerminalCleansOnlyMatchingOrphanReservation(t *testing.T) {
	for _, test := range []struct {
		name   string
		upload bool
	}{
		{name: "creating"},
		{name: "uploaded", upload: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, err := New(t.TempDir(), strings.Repeat("08", 32), 1, 10<<20)
			if err != nil {
				t.Fatal(err)
			}
			reservation := testReservation()
			orphan, err := manager.Reserve(context.Background(), reservation)
			if err != nil {
				t.Fatal(err)
			}
			if test.upload {
				archive := testArchive(t, orphan, "secret")
				digest := sha256.Sum256(archive)
				orphan, err = manager.Upload(
					context.Background(), orphan.ID, reservation.HostID, reservation.JobID,
					hex.EncodeToString(digest[:]), int64(len(archive)), bytes.NewReader(archive), nil,
				)
				if err != nil {
					t.Fatal(err)
				}
				if orphan.Status != StatusUploaded {
					t.Fatalf("orphan status=%q, want %q", orphan.Status, StatusUploaded)
				}
			}

			if err := manager.AbortTerminal(orphan.ID, "wrong-host", reservation.JobID); !errors.Is(err, ErrState) {
				t.Fatalf("AbortTerminal(wrong host) error=%v, want %v", err, ErrState)
			}
			if err := manager.AbortTerminal(orphan.ID, reservation.HostID, "wrong-job"); !errors.Is(err, ErrState) {
				t.Fatalf("AbortTerminal(wrong job) error=%v, want %v", err, ErrState)
			}
			if _, err := manager.Get(orphan.ID); err != nil {
				t.Fatalf("identity mismatch removed recovery point: %v", err)
			}

			if err := manager.AbortTerminal(orphan.ID, reservation.HostID, reservation.JobID); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Get(orphan.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("terminal orphan still exists: %v", err)
			}
			if _, err := os.Stat(manager.artifactPath(orphan.ID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("terminal orphan artifact still exists: %v", err)
			}

			replacement := testReservation()
			replacement.JobID = "job-replacement"
			if _, err := manager.Reserve(context.Background(), replacement); err != nil {
				t.Fatalf("Reserve() remained blocked after terminal orphan cleanup: %v", err)
			}
		})
	}
}

func TestAutomatedReservationRotatesOnlyTheOldestAutomatedBackup(t *testing.T) {
	manager, err := New(t.TempDir(), strings.Repeat("09", 32), 2, 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	current := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return current }
	finalize := func(reservation Reservation) Metadata {
		metadata, reserveErr := manager.Reserve(context.Background(), reservation)
		if reserveErr != nil {
			t.Fatal(reserveErr)
		}
		archive := testArchive(t, metadata, "secret")
		digest := sha256.Sum256(archive)
		encodedDigest := hex.EncodeToString(digest[:])
		if _, uploadErr := manager.Upload(
			context.Background(), metadata.ID, reservation.HostID, reservation.JobID,
			encodedDigest, int64(len(archive)), bytes.NewReader(archive), nil,
		); uploadErr != nil {
			t.Fatal(uploadErr)
		}
		if _, verifyErr := manager.VerifyUploaded(
			context.Background(), metadata.ID, reservation.HostID, reservation.JobID,
			encodedDigest, int64(len(archive)),
		); verifyErr != nil {
			t.Fatal(verifyErr)
		}
		return metadata
	}

	manualReservation := testReservation()
	manualReservation.JobID = "job-manual"
	manual := finalize(manualReservation)
	current = current.Add(time.Minute)
	automatedReservation := testReservation()
	automatedReservation.JobID = "job-automatic-1"
	automatedReservation.Automated = true
	automated := finalize(automatedReservation)

	current = current.Add(time.Minute)
	replacementReservation := testReservation()
	replacementReservation.JobID = "job-automatic-2"
	replacementReservation.Automated = true
	replacement, err := manager.Reserve(context.Background(), replacementReservation)
	if err != nil {
		t.Fatalf("Reserve(automated) error=%v", err)
	}
	if !replacement.Automated {
		t.Fatal("replacement reservation lost its automated classification")
	}
	if _, err := manager.Get(manual.ID); err != nil {
		t.Fatalf("manual backup was rotated: %v", err)
	}
	if _, err := manager.Get(automated.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old automated backup was not rotated: %v", err)
	}
}

func uploadAndVerifyRecoveryPoint(t *testing.T, manager *Manager, reservation Reservation) Metadata {
	t.Helper()
	metadata, err := manager.Reserve(context.Background(), reservation)
	if err != nil {
		t.Fatal(err)
	}
	archive := testArchive(t, metadata, "secret")
	digest := sha256.Sum256(archive)
	encodedDigest := hex.EncodeToString(digest[:])
	if _, err := manager.Upload(
		context.Background(), metadata.ID, reservation.HostID, reservation.JobID,
		encodedDigest, int64(len(archive)), bytes.NewReader(archive), nil,
	); err != nil {
		t.Fatal(err)
	}
	ready, err := manager.VerifyUploaded(
		context.Background(), metadata.ID, reservation.HostID, reservation.JobID,
		encodedDigest, int64(len(archive)),
	)
	if err != nil {
		t.Fatal(err)
	}
	return ready
}

func testReservation() Reservation {
	return Reservation{
		InstanceID: "00000000-0000-4000-8000-000000000001", InstanceName: "fleet-test-01",
		HostID: "host-1", OperationID: "operation-1", JobID: "job-1",
		Image: "runtime:0.18.2", ImageID: "sha256:" + strings.Repeat("a", 64),
		Provider: "openai-codex", Model: "gpt-5.6-sol", Reasoning: "medium", ServiceTier: "normal",
		CodexConfigured: true,
		ProjectName:     "hermes-fleet-fleet-test-01-00000000", DataVolume: "hermes-fleet-fleet-test-01-00000000-data",
		ManagedPath: "/managed/fleet-test-01-00000000", AgentVersion: "0.6.4",
	}
}

func testArchive(t *testing.T, metadata Metadata, workspaceSecret string) []byte {
	t.Helper()
	manifest := Manifest{
		FormatVersion: 1, RecoveryPointID: metadata.ID, InstanceID: metadata.InstanceID, InstanceName: metadata.InstanceName,
		Image: metadata.Image, ImageID: metadata.ImageID, Provider: metadata.Provider, Model: metadata.Model,
		Reasoning: metadata.Reasoning, ServiceTier: metadata.ServiceTier, CodexConfigured: metadata.CodexConfigured, ProjectName: metadata.ProjectName,
		DataVolume: metadata.DataVolume, ManagedPath: metadata.ManagedPath, AgentVersion: metadata.AgentVersion,
		CreatedAt: metadata.CreatedAt,
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	archive := tar.NewWriter(&buffer)
	entries := []struct {
		name string
		data []byte
	}{
		{name: "manifest.json", data: manifestData},
		{name: "workspace/.env", data: []byte(workspaceSecret)},
		{name: "data-volume.tar", data: []byte("volume-data")},
	}
	for _, entry := range entries {
		if err := archive.WriteHeader(&tar.Header{Name: entry.name, Mode: 0o600, Size: int64(len(entry.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type gatedReader struct {
	reader  io.Reader
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (reader *gatedReader) Read(data []byte) (int, error) {
	reader.once.Do(func() { close(reader.started) })
	<-reader.release
	return reader.reader.Read(data)
}
