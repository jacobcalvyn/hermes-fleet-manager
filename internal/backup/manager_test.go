package backup_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/backup"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/store"
)

func TestManagerCreatesVerifiesListsOpensAndDeletesBackup(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	root := t.TempDir()
	manager, err := backup.New(root, database, 2)
	if err != nil {
		t.Fatal(err)
	}

	created, err := manager.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Filename == "" || created.SHA256 == "" || created.SizeBytes <= 0 || created.VerifiedAt.IsZero() {
		t.Fatalf("incomplete backup metadata: %+v", created)
	}
	for _, path := range []string{filepath.Join(root, created.ID+".sqlite"), filepath.Join(root, created.ID+".json")} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("backup artifact %s mode = %v", path, info.Mode())
		}
	}

	items, err := manager.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != created.ID {
		t.Fatalf("List() = %+v", items)
	}
	verified, err := manager.Verify(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if verified.SHA256 != created.SHA256 {
		t.Fatalf("Verify() changed digest: %q != %q", verified.SHA256, created.SHA256)
	}
	metadata, file, err := manager.Open(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	header := make([]byte, 15)
	if _, err := file.Read(header); err != nil {
		t.Fatal(err)
	}
	if string(header) != "SQLite format 3" || metadata.ID != created.ID {
		t.Fatalf("Open() returned invalid backup: header=%q metadata=%+v", header, metadata)
	}
	if _, _, err := manager.Open("../../fleet.db"); !errors.Is(err, backup.ErrNotFound) {
		t.Fatalf("Open() traversal error = %v, want ErrNotFound", err)
	}

	if err := manager.Delete(ctx, created.ID, "wrong confirmation"); !errors.Is(err, backup.ErrConfirmation) {
		t.Fatalf("Delete() error = %v, want ErrConfirmation", err)
	}
	if err := manager.Delete(ctx, created.ID, created.Filename); err != nil {
		t.Fatal(err)
	}
	items, err = manager.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("backup still listed after deletion: %+v", items)
	}
}

func TestManagerRotatesOldestBackupAtRetentionLimit(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	root := t.TempDir()
	manager, err := backup.New(root, database, 1)
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := manager.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	items, err := manager.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != rotated.ID {
		t.Fatalf("List() after rotation = %+v, want only %s", items, rotated.ID)
	}
	if _, _, err := manager.Open(created.ID); !errors.Is(err, backup.ErrNotFound) {
		t.Fatalf("Open(oldest) error = %v, want ErrNotFound", err)
	}
}

func TestManagerDetectsTampering(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	root := t.TempDir()
	manager, err := backup.New(root, database, 2)
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(root, created.ID+".sqlite")
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("tampered"), 128); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Verify(ctx, created.ID); !errors.Is(err, backup.ErrIntegrity) {
		t.Fatalf("Verify() error = %v, want ErrIntegrity", err)
	}
}

func TestManagerCleansInterruptedPublicationOnStartup(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	root := t.TempDir()
	orphan := filepath.Join(root, "backup-"+strings.Repeat("a", 32)+".sqlite")
	if err := os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := backup.New(root, database, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan backup was not reconciled: %v", err)
	}
	items, err := manager.List(context.Background())
	if err != nil || len(items) != 0 {
		t.Fatalf("List() after reconciliation items=%+v error=%v", items, err)
	}
}
