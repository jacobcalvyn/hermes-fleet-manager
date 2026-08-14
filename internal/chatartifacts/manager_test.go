package chatartifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testMetadata(content []byte) Metadata {
	digest := sha256.Sum256(content)
	return Metadata{
		ID:          "artifact-0123456789abcdef0123456789abcdef",
		InstanceID:  "33333333-3333-4333-8333-333333333333",
		SessionID:   "11111111-1111-4111-8111-111111111111",
		OperationID: "22222222-2222-4222-8222-222222222222",
		Name:        "dashboard.csv", Kind: "file", MediaType: "text/csv",
		SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:]),
	}
}

func TestManagerStoresLeaseVerifiedArtifactAndDeletesSession(t *testing.T) {
	manager, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("dashboard,ready\n")
	metadata := testMetadata(content)
	verified := false
	stored, err := manager.Put(context.Background(), metadata, bytes.NewReader(content), func() error {
		verified = true
		return nil
	})
	if err != nil || !verified || stored.ID != metadata.ID {
		t.Fatalf("stored=%+v verified=%t error=%v", stored, verified, err)
	}
	opened, file, err := manager.Open(metadata.SessionID, metadata.ID)
	if err != nil {
		t.Fatal(err)
	}
	read, _ := io.ReadAll(file)
	_ = file.Close()
	if opened.SHA256 != metadata.SHA256 || !bytes.Equal(read, content) {
		t.Fatalf("opened=%+v content=%q", opened, read)
	}
	if err := manager.DeleteSession(metadata.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Open(metadata.SessionID, metadata.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open() after delete error=%v", err)
	}
}

func TestManagerRejectsBodyMismatchBeforeLeaseVerification(t *testing.T) {
	manager, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	metadata := testMetadata([]byte("data"))
	metadata.SHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	verified := false
	if _, err := manager.Put(context.Background(), metadata, bytes.NewReader([]byte("wrong")), func() error {
		verified = true
		return nil
	}); err == nil || verified {
		t.Fatalf("Put() error=%v verified=%t", err, verified)
	}
}

func TestManagerEnforcesSessionQuotaBeforeReadingBody(t *testing.T) {
	manager, err := New(t.TempDir(), Config{
		SessionMaxBytes: MaximumBytes, InstanceMaxBytes: 2 * MaximumBytes,
		TotalMaxBytes: 3 * MaximumBytes, Retention: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("first")
	metadata := testMetadata(content)
	if _, err := manager.Put(context.Background(), metadata, bytes.NewReader(content), nil); err != nil {
		t.Fatal(err)
	}
	second := metadata
	second.ID = "artifact-fedcba9876543210fedcba9876543210"
	second.SizeBytes = MaximumBytes
	second.SHA256 = strings.Repeat("b", 64)
	read := false
	if _, err := manager.Put(context.Background(), second, readerFunc(func([]byte) (int, error) {
		read = true
		return 0, io.EOF
	}), nil); !errors.Is(err, ErrQuota) || read {
		t.Fatalf("Put() error=%v read=%t", err, read)
	}
}

func TestManagerExpiresContentAndRetainsManifest(t *testing.T) {
	root := t.TempDir()
	manager, err := New(root, Config{
		SessionMaxBytes: MaximumBytes, InstanceMaxBytes: 2 * MaximumBytes,
		TotalMaxBytes: 3 * MaximumBytes, Retention: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("expires")
	metadata := testMetadata(content)
	metadata.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)
	if _, err := manager.Put(context.Background(), metadata, bytes.NewReader(content), nil); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Get(metadata.SessionID, metadata.ID)
	if err != nil || status.Status != StatusExpired || status.Error == "" {
		t.Fatalf("status=%+v error=%v", status, err)
	}
	if _, _, err := manager.Open(metadata.SessionID, metadata.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("Open() error=%v", err)
	}
	metadataPath := filepath.Join(root, metadata.SessionID, metadata.ID+".json")
	if _, err := os.Stat(metadataPath); err != nil {
		t.Fatalf("expired manifest was removed: %v", err)
	}
}

func TestManagerReconcilesMissingAndOrphanedContent(t *testing.T) {
	root := t.TempDir()
	manager, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("stored")
	metadata := testMetadata(content)
	if _, err := manager.Put(context.Background(), metadata, bytes.NewReader(content), nil); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, metadata.SessionID)
	if err := os.Remove(filepath.Join(directory, metadata.ID+".data")); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(directory, "artifact-fedcba9876543210fedcba9876543210.data")
	if err := os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	report, err := manager.Reconcile(time.Now().UTC())
	if err != nil || report.Missing != 1 || report.RemovedOrphans != 1 || report.TotalBytes != 0 {
		t.Fatalf("report=%+v error=%v", report, err)
	}
	status, err := manager.Get(metadata.SessionID, metadata.ID)
	if err != nil || status.Status != StatusMissing {
		t.Fatalf("status=%+v error=%v", status, err)
	}
}

func TestManagerRejectsSymlinkedContentAndRetainsManifest(t *testing.T) {
	root := t.TempDir()
	manager, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("stored")
	metadata := testMetadata(content)
	if _, err := manager.Put(context.Background(), metadata, bytes.NewReader(content), nil); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, metadata.SessionID)
	dataPath := filepath.Join(directory, metadata.ID+".data")
	targetPath := filepath.Join(root, "outside.data")
	if err := os.WriteFile(targetPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dataPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetPath, dataPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Open(metadata.SessionID, metadata.ID); !errors.Is(err, ErrMissing) {
		t.Fatalf("Open() error=%v", err)
	}
	status, err := manager.Get(metadata.SessionID, metadata.ID)
	if err != nil || status.Status != StatusMissing {
		t.Fatalf("status=%+v error=%v", status, err)
	}
	if _, err := os.Stat(targetPath); err != nil {
		t.Fatalf("symlink target was modified: %v", err)
	}
}

func TestManagerRecordsLifecycleThenAcceptsMatchingUpload(t *testing.T) {
	manager, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("prepared output")
	metadata := testMetadata(content)
	metadata.Status = StatusPreparing
	metadata.CreatedAt = time.Now().UTC().Add(-time.Minute)
	prepared, err := manager.Record(metadata)
	if err != nil || prepared.Status != StatusPreparing {
		t.Fatalf("Record()=%+v error=%v", prepared, err)
	}
	metadata.Status = ""
	stored, err := manager.Put(context.Background(), metadata, bytes.NewReader(content), nil)
	if err != nil || stored.Status != StatusReady || !stored.CreatedAt.Equal(prepared.CreatedAt) {
		t.Fatalf("Put()=%+v error=%v", stored, err)
	}
	reported := metadata
	reported.Status = StatusReady
	ready, err := manager.Record(reported)
	if err != nil || ready.Status != StatusReady {
		t.Fatalf("ready Record()=%+v error=%v", ready, err)
	}
}

func TestManagerListsFiltersAndPaginatesLifecycleRecords(t *testing.T) {
	manager, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first := testMetadata([]byte("first"))
	first.Status = StatusRejected
	first.Error = "The output type is blocked."
	first.SizeBytes = 0
	first.SHA256 = ""
	first.CreatedAt = now.Add(-time.Minute)
	if _, err := manager.Record(first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID = "artifact-fedcba9876543210fedcba9876543210"
	second.Name = "latest.png"
	second.Kind = "image"
	second.MediaType = "image/png"
	second.Status = StatusFailed
	second.Error = "The renderer failed."
	second.CreatedAt = now
	if _, err := manager.Record(second); err != nil {
		t.Fatal(err)
	}
	page, err := manager.List(ListOptions{Limit: 1}, now)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != second.ID || page.NextCursor == nil {
		t.Fatalf("first page=%+v error=%v", page, err)
	}
	next, err := manager.List(ListOptions{Limit: 1, Cursor: page.NextCursor}, now)
	if err != nil || len(next.Items) != 1 || next.Items[0].ID != first.ID || next.NextCursor != nil {
		t.Fatalf("next page=%+v error=%v", next, err)
	}
	filtered, err := manager.List(ListOptions{Limit: 10, Kind: "image", Query: "renderer"}, now)
	if err != nil || len(filtered.Items) != 1 || filtered.Items[0].ID != second.ID {
		t.Fatalf("filtered=%+v error=%v", filtered, err)
	}
	usage, err := manager.Usage(now)
	if err != nil || usage.StatusCounts[StatusRejected] != 1 || usage.StatusCounts[StatusFailed] != 1 || usage.TotalBytes != 0 {
		t.Fatalf("usage=%+v error=%v", usage, err)
	}
}

func TestManagerDeletesSingleArtifactAsAuditTombstone(t *testing.T) {
	manager, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("downloadable")
	metadata := testMetadata(content)
	if _, err := manager.Put(context.Background(), metadata, bytes.NewReader(content), nil); err != nil {
		t.Fatal(err)
	}
	deletedAt := time.Now().UTC().Add(time.Minute)
	deleted, err := manager.DeleteArtifact(metadata.ID, deletedAt)
	if err != nil || deleted.Status != StatusDeleted || deleted.DeletedAt == nil || !deleted.DeletedAt.Equal(deletedAt) {
		t.Fatalf("DeleteArtifact()=%+v error=%v", deleted, err)
	}
	if _, _, err := manager.Open(metadata.SessionID, metadata.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open() after delete error=%v", err)
	}
	page, err := manager.List(ListOptions{Limit: 10, Status: StatusDeleted}, deletedAt)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != metadata.ID {
		t.Fatalf("deleted page=%+v error=%v", page, err)
	}
	usage, err := manager.Usage(deletedAt)
	if err != nil || usage.TotalBytes != 0 || usage.StatusCounts[StatusDeleted] != 1 {
		t.Fatalf("usage=%+v error=%v", usage, err)
	}
}

type readerFunc func([]byte) (int, error)

func (function readerFunc) Read(buffer []byte) (int, error) { return function(buffer) }
