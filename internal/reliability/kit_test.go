package reliability

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
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
	if manifest.FormatVersion != 2 || len(manifest.AuthenticationTag) != 64 {
		t.Fatalf("recovery kit manifest is not authenticated: %+v", manifest)
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

func TestImportCleanHostRejectsTamperedManifest(t *testing.T) {
	archiveData, key, _ := createEmptyRecoveryKitArchive(t)
	tampered := rewriteRecoveryKitManifest(t, archiveData, func(manifest *KitManifest) {
		manifest.BuildID = "forged-build"
	}, nil)
	err := importRecoveryKitBytes(t, tampered, key)
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("ImportCleanHost() error = %v, want authentication failure", err)
	}
}

func TestImportCleanHostRejectsSignedTraversalMetadataBeforeExtraction(t *testing.T) {
	archiveData, key, recoveryPoints := createEmptyRecoveryKitArchive(t)
	forged := rewriteRecoveryKitManifest(t, archiveData, func(manifest *KitManifest) {
		manifest.ControlPlaneBackup.ID = "../outside"
	}, func(manifest *KitManifest) {
		payload, err := authenticatedManifestBytes(*manifest)
		if err != nil {
			t.Fatal(err)
		}
		manifest.AuthenticationTag = recoveryPoints.AuthenticateRecoveryKitManifest(payload)
	})
	err := importRecoveryKitBytes(t, forged, key)
	if err == nil || !strings.Contains(err.Error(), "invalid control-plane backup metadata") {
		t.Fatalf("ImportCleanHost() error = %v, want invalid metadata", err)
	}
}

func TestWriteImportedFileRejectsDestinationOutsideStage(t *testing.T) {
	parent := t.TempDir()
	stage := filepath.Join(parent, "stage")
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(parent, "outside")
	err := writeImportedFile(context.Background(), stage, destination, strings.NewReader("forged"), 6, os.Getuid(), os.Getgid())
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("writeImportedFile() error = %v", err)
	}
	if _, statErr := os.Stat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("outside destination was created: %v", statErr)
	}
}

func createEmptyRecoveryKitArchive(t *testing.T) ([]byte, string, *recovery.Manager) {
	t.Helper()
	root := t.TempDir()
	dataStore, err := store.Open(filepath.Join(root, "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	backups, err := backup.New(filepath.Join(root, "backups"), dataStore, 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backups.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("01", 32)
	recoveryPoints, err := recovery.New(filepath.Join(root, "recovery"), key, 5, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	kit, err := NewRecoveryKit(backups, recoveryPoints, func(context.Context) ([]domain.Instance, error) { return nil, nil }, "0.10.0", "build-test", releases.Catalog{})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if _, err := kit.Export(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes(), key, recoveryPoints
}

func rewriteRecoveryKitManifest(t *testing.T, source []byte, mutate func(*KitManifest), authenticate func(*KitManifest)) []byte {
	t.Helper()
	type entry struct {
		header tar.Header
		data   []byte
	}
	var entries []entry
	reader := tar.NewReader(bytes.NewReader(source))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		copied := *header
		if header.Name == "manifest.json" {
			var manifest KitManifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			mutate(&manifest)
			if authenticate != nil {
				authenticate(&manifest)
			}
			data, err = json.MarshalIndent(manifest, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			data = append(data, '\n')
			copied.Size = int64(len(data))
		}
		entries = append(entries, entry{header: copied, data: data})
	}
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	for _, item := range entries {
		if err := writer.WriteHeader(&item.header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(item.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func importRecoveryKitBytes(t *testing.T, data []byte, key string) error {
	t.Helper()
	root := t.TempDir()
	kitPath := filepath.Join(root, "kit.tar")
	if err := os.WriteFile(kitPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := ImportCleanHost(context.Background(), CleanHostImportOptions{
		KitPath: kitPath, DataRoot: target, Confirmation: target, RecoveryKey: key, MaximumPointBytes: 1 << 20,
	})
	return err
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
