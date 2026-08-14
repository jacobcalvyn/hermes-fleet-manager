package reliability

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/backup"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/recovery"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/releases"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/store"
)

func TestRecoveryKitExportsVerifiedControlPlaneBackup(t *testing.T) {
	root := t.TempDir()
	dataStore, err := store.Open(filepath.Join(root, "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	backups, err := backup.New(filepath.Join(root, "backups"), dataStore, 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backups.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	recoveryPoints, err := recovery.New(filepath.Join(root, "recovery"), strings.Repeat("01", 32), 5, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	kit, err := NewRecoveryKit(backups, recoveryPoints, func(context.Context) ([]domain.Instance, error) { return nil, nil }, "0.10.0", "build-123", releases.Catalog{})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	manifest, err := kit.Export(context.Background(), &output)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.FleetVersion != "0.10.0" || manifest.BuildID != "build-123" {
		t.Fatalf("unexpected manifest identity: %+v", manifest)
	}
	wanted := map[string]bool{"manifest.json": false, "hermes-releases.json": false}
	wanted[filepath.ToSlash(filepath.Join("control-plane", manifest.ControlPlaneBackup.Filename))] = false
	reader := tar.NewReader(bytes.NewReader(output.Bytes()))
	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		if _, ok := wanted[header.Name]; ok {
			wanted[header.Name] = true
		}
	}
	for name, found := range wanted {
		if !found {
			t.Fatalf("recovery kit is missing %s", name)
		}
	}
}

func TestRecoveryKitFailsClosedWithoutCompleteBackupSet(t *testing.T) {
	root := t.TempDir()
	dataStore, err := store.Open(filepath.Join(root, "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	backups, err := backup.New(filepath.Join(root, "backups"), dataStore, 5)
	if err != nil {
		t.Fatal(err)
	}
	recoveryPoints, err := recovery.New(filepath.Join(root, "recovery"), strings.Repeat("01", 32), 5, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	kit, err := NewRecoveryKit(backups, recoveryPoints, func(context.Context) ([]domain.Instance, error) { return nil, nil }, "0.10.0", "build", releases.Catalog{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kit.Export(context.Background(), io.Discard); !errors.Is(err, ErrRecoveryKitIncomplete) {
		t.Fatalf("Export() error = %v, want ErrRecoveryKitIncomplete", err)
	}
}

func TestImportCleanHostValidatesAndReplacesFleetData(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "source")
	if err := os.MkdirAll(sourceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	dataStore, err := store.Open(filepath.Join(sourceRoot, "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	backups, err := backup.New(filepath.Join(sourceRoot, "backups"), dataStore, 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backups.Create(ctx); err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("01", 32)
	recoveryPoints, err := recovery.New(filepath.Join(sourceRoot, "recovery-points"), key, 5, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	checkedAt := time.Date(2026, 8, 10, 1, 0, 0, 0, time.UTC)
	catalog := releases.Catalog{Source: "NousResearch/hermes-agent GitHub Releases", CheckedAt: checkedAt}
	for index, version := range []string{"0.20.0", "0.19.1", "0.19.0"} {
		commit := strings.Repeat(strconv.Itoa(index+1), 40)
		catalog.Releases = append(catalog.Releases, releases.Release{
			Version: version, Tag: "v" + version, Commit: commit,
			Image:       "local/hermes-fleet-runtime:" + version + "-" + commit[:12],
			URL:         "https://github.com/NousResearch/hermes-agent/releases/tag/v" + version,
			PublishedAt: checkedAt.Add(-time.Duration(index) * time.Hour),
		})
	}
	kit, err := NewRecoveryKit(backups, recoveryPoints, func(context.Context) ([]domain.Instance, error) { return nil, nil }, "0.12.1", "build-test", catalog)
	if err != nil {
		t.Fatal(err)
	}
	kitPath := filepath.Join(root, "recovery-kit.tar")
	kitFile, err := os.OpenFile(kitPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := kit.Export(ctx, kitFile)
	if closeErr := kitFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := dataStore.Close(); err != nil {
		t.Fatal(err)
	}
	targetRoot := filepath.Join(root, "target")
	if err := os.MkdirAll(targetRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetRoot, "obsolete"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	imported, err := ImportCleanHost(ctx, CleanHostImportOptions{
		KitPath: kitPath, DataRoot: targetRoot, Confirmation: targetRoot,
		RecoveryKey: key, ExpectedVersion: "0.12.1", MaximumPointBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if imported.BuildID != manifest.BuildID {
		t.Fatalf("imported build=%q, want %q", imported.BuildID, manifest.BuildID)
	}
	if _, err := os.Stat(filepath.Join(targetRoot, "obsolete")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("obsolete Fleet data survived import: %v", err)
	}
	if _, err := os.Stat(filepath.Join(targetRoot, "fleet.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(targetRoot, "backups", manifest.ControlPlaneBackup.ID+".sqlite")); err != nil {
		t.Fatal(err)
	}
}
