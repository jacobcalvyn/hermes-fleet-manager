package reliability

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/recovery"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/releases"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/store"
)

type CleanHostImportOptions struct {
	KitPath           string
	DataRoot          string
	Confirmation      string
	RecoveryKey       string
	ExpectedVersion   string
	ExpectedBuildID   string
	MaximumKitBytes   int64
	MaximumPointBytes int64
}

type cleanHostEntry struct {
	destination string
	exactSize   int64
	maximumSize int64
}

func ImportCleanHost(ctx context.Context, options CleanHostImportOptions) (KitManifest, error) {
	kitPath, err := filepath.Abs(strings.TrimSpace(options.KitPath))
	if err != nil {
		return KitManifest{}, fmt.Errorf("resolve recovery kit: %w", err)
	}
	dataRoot, err := filepath.Abs(strings.TrimSpace(options.DataRoot))
	if err != nil {
		return KitManifest{}, fmt.Errorf("resolve Fleet data root: %w", err)
	}
	if options.Confirmation != dataRoot {
		return KitManifest{}, errors.New("clean-host import confirmation must exactly match the Fleet data root")
	}
	rootInfo, err := os.Lstat(dataRoot)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return KitManifest{}, errors.New("Fleet data root must be an existing real directory")
	}
	stat, ok := rootInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return KitManifest{}, errors.New("Fleet data root ownership is unavailable")
	}
	maximumKitBytes := options.MaximumKitBytes
	if maximumKitBytes < 1 {
		maximumKitBytes = 100 << 30
	}
	kitInfo, err := os.Lstat(kitPath)
	if err != nil || !kitInfo.Mode().IsRegular() || kitInfo.Size() < 1 || kitInfo.Size() > maximumKitBytes {
		return KitManifest{}, errors.New("recovery kit has an unsafe type or size")
	}
	kit, err := os.Open(kitPath)
	if err != nil {
		return KitManifest{}, fmt.Errorf("open recovery kit: %w", err)
	}
	defer kit.Close()
	archive := tar.NewReader(&contextReader{ctx: ctx, reader: kit})
	header, err := archive.Next()
	if err != nil || header.Name != "manifest.json" || header.Typeflag != tar.TypeReg || header.Mode&0o777 != 0o600 || header.Size < 1 || header.Size > 16<<20 {
		return KitManifest{}, errors.New("recovery kit must begin with a secure manifest.json")
	}
	var manifest KitManifest
	if err := json.NewDecoder(io.LimitReader(archive, header.Size)).Decode(&manifest); err != nil {
		return KitManifest{}, fmt.Errorf("decode recovery kit manifest: %w", err)
	}
	if manifest.FormatVersion != 1 || manifest.ControlPlaneBackup.ID == "" || manifest.ControlPlaneBackup.Filename == "" || manifest.ControlPlaneBackup.SizeBytes < 1 {
		return KitManifest{}, errors.New("recovery kit manifest is unsupported or incomplete")
	}
	if options.ExpectedVersion != "" && manifest.FleetVersion != options.ExpectedVersion {
		return KitManifest{}, fmt.Errorf("recovery kit Fleet version %q does not match expected %q", manifest.FleetVersion, options.ExpectedVersion)
	}
	if options.ExpectedBuildID != "" && manifest.BuildID != options.ExpectedBuildID {
		return KitManifest{}, fmt.Errorf("recovery kit build %q does not match expected %q", manifest.BuildID, options.ExpectedBuildID)
	}
	stage, err := os.MkdirTemp(dataRoot, ".clean-host-import-")
	if err != nil {
		return KitManifest{}, fmt.Errorf("create clean-host import staging directory: %w", err)
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0o700); err != nil {
		return KitManifest{}, err
	}
	if err := os.Chown(stage, int(stat.Uid), int(stat.Gid)); err != nil {
		return KitManifest{}, err
	}
	expected := map[string]cleanHostEntry{
		"hermes-releases.json": {
			destination: filepath.Join(stage, "hermes-releases.json"), maximumSize: 16 << 20,
		},
		filepath.ToSlash(filepath.Join("control-plane", manifest.ControlPlaneBackup.Filename)): {
			destination: filepath.Join(stage, "fleet.db"), exactSize: manifest.ControlPlaneBackup.SizeBytes,
		},
	}
	maximumPointBytes := options.MaximumPointBytes
	if maximumPointBytes < 1 {
		maximumPointBytes = 50 << 30
	}
	for _, point := range manifest.InstanceBackups {
		if point.ID == "" || point.InstanceName == "" || point.EncryptedSizeBytes < 1 || point.EncryptedSizeBytes > maximumPointBytes {
			return KitManifest{}, errors.New("recovery kit contains invalid instance backup metadata")
		}
		base := filepath.ToSlash(filepath.Join("instances", safeArchiveName(point.InstanceName), point.ID))
		expected[base+".json"] = cleanHostEntry{destination: filepath.Join(stage, "recovery-points", point.ID+".json"), maximumSize: 1 << 20}
		expected[base+".enc"] = cleanHostEntry{destination: filepath.Join(stage, "recovery-points", point.ID+".enc"), exactSize: point.EncryptedSizeBytes}
	}
	seen := map[string]bool{"manifest.json": true}
	for {
		header, nextErr := archive.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return KitManifest{}, fmt.Errorf("read recovery kit: %w", nextErr)
		}
		entry, allowed := expected[header.Name]
		if !allowed || seen[header.Name] || header.Typeflag != tar.TypeReg || header.Mode&0o777 != 0o600 {
			return KitManifest{}, fmt.Errorf("recovery kit contains unexpected or unsafe entry %q", header.Name)
		}
		if (entry.exactSize > 0 && header.Size != entry.exactSize) || (entry.maximumSize > 0 && (header.Size < 1 || header.Size > entry.maximumSize)) {
			return KitManifest{}, fmt.Errorf("recovery kit entry %q has an invalid size", header.Name)
		}
		if err := writeImportedFile(ctx, entry.destination, archive, header.Size, int(stat.Uid), int(stat.Gid)); err != nil {
			return KitManifest{}, fmt.Errorf("extract recovery kit entry %q: %w", header.Name, err)
		}
		seen[header.Name] = true
	}
	for name := range expected {
		if !seen[name] {
			return KitManifest{}, fmt.Errorf("recovery kit is missing %q", name)
		}
	}
	if err := validateImportedCatalog(filepath.Join(stage, "hermes-releases.json"), manifest.ReleaseCatalog); err != nil {
		return KitManifest{}, err
	}
	if err := validateFileDigest(filepath.Join(stage, "fleet.db"), manifest.ControlPlaneBackup.SizeBytes, manifest.ControlPlaneBackup.SHA256); err != nil {
		return KitManifest{}, fmt.Errorf("verify imported control-plane database: %w", err)
	}
	backupRoot := filepath.Join(stage, "backups")
	if err := os.MkdirAll(backupRoot, 0o700); err != nil {
		return KitManifest{}, err
	}
	if err := os.Chown(backupRoot, int(stat.Uid), int(stat.Gid)); err != nil {
		return KitManifest{}, err
	}
	backupPath := filepath.Join(backupRoot, manifest.ControlPlaneBackup.ID+".sqlite")
	if err := copyImportedFile(filepath.Join(stage, "fleet.db"), backupPath, int(stat.Uid), int(stat.Gid)); err != nil {
		return KitManifest{}, fmt.Errorf("preserve imported control-plane backup: %w", err)
	}
	metadataData, err := json.MarshalIndent(manifest.ControlPlaneBackup, "", "  ")
	if err != nil {
		return KitManifest{}, err
	}
	metadataData = append(metadataData, '\n')
	if err := writeBytes(filepath.Join(backupRoot, manifest.ControlPlaneBackup.ID+".json"), metadataData, int(stat.Uid), int(stat.Gid)); err != nil {
		return KitManifest{}, err
	}
	dataStore, err := store.Open(filepath.Join(stage, "fleet.db"))
	if err != nil {
		return KitManifest{}, fmt.Errorf("open imported control-plane database: %w", err)
	}
	if err := dataStore.VerifyBackup(ctx, backupPath); err != nil {
		dataStore.Close()
		return KitManifest{}, fmt.Errorf("verify preserved control-plane backup: %w", err)
	}
	if err := dataStore.PrepareCleanHostRecovery(ctx, time.Now().UTC()); err != nil {
		dataStore.Close()
		return KitManifest{}, err
	}
	if err := dataStore.Close(); err != nil {
		return KitManifest{}, fmt.Errorf("close imported control-plane database: %w", err)
	}
	for _, sidecar := range []string{"fleet.db-wal", "fleet.db-shm"} {
		if err := os.Remove(filepath.Join(stage, sidecar)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return KitManifest{}, err
		}
	}
	recoveryRoot := filepath.Join(stage, "recovery-points")
	manager, err := recovery.New(recoveryRoot, options.RecoveryKey, 100, maximumPointBytes)
	if err != nil {
		return KitManifest{}, fmt.Errorf("open imported instance backups: %w", err)
	}
	for _, expectedPoint := range manifest.InstanceBackups {
		data, err := os.ReadFile(filepath.Join(recoveryRoot, expectedPoint.ID+".json"))
		if err != nil {
			return KitManifest{}, err
		}
		var actualPoint recovery.Metadata
		if err := json.Unmarshal(data, &actualPoint); err != nil || !reflect.DeepEqual(actualPoint, expectedPoint) {
			return KitManifest{}, fmt.Errorf("instance backup metadata for %s does not match the kit manifest", expectedPoint.InstanceName)
		}
		if _, err := manager.Verify(ctx, expectedPoint.ID); err != nil {
			return KitManifest{}, fmt.Errorf("decrypt and verify instance backup for %s: %w", expectedPoint.InstanceName, err)
		}
	}
	if err := secureImportedTree(stage, int(stat.Uid), int(stat.Gid)); err != nil {
		return KitManifest{}, fmt.Errorf("normalize imported Fleet data ownership: %w", err)
	}
	if err := promoteCleanHostStage(dataRoot, stage); err != nil {
		return KitManifest{}, err
	}
	return manifest, nil
}

func secureImportedTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("imported path %q has an unsafe type", filepath.Base(path))
		}
		mode := os.FileMode(0o600)
		if info.IsDir() {
			mode = 0o700
		}
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
		return os.Chown(path, uid, gid)
	})
}

func writeImportedFile(ctx context.Context, destination string, source io.Reader, size int64, uid, gid int) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	if err := os.Chown(filepath.Dir(destination), uid, gid); err != nil {
		return err
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(&contextReader{ctx: ctx, reader: source}, size+1))
	if copyErr == nil && written != size {
		copyErr = errors.New("recovery kit entry size changed during extraction")
	}
	if copyErr == nil {
		copyErr = file.Sync()
	}
	if closeErr := file.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.Remove(destination)
		return copyErr
	}
	return os.Chown(destination, uid, gid)
}

func writeBytes(destination string, data []byte, uid, gid int) error {
	return writeImportedFile(context.Background(), destination, strings.NewReader(string(data)), int64(len(data)), uid, gid)
}

func copyImportedFile(source, destination string, uid, gid int) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	return writeImportedFile(context.Background(), destination, file, info.Size(), uid, gid)
}

func validateImportedCatalog(filename string, expected releases.Catalog) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("read imported release catalog: %w", err)
	}
	var actual releases.Catalog
	if err := json.Unmarshal(data, &actual); err != nil {
		return fmt.Errorf("decode imported release catalog: %w", err)
	}
	if err := releases.ValidateCatalog(actual, len(expected.Releases)); err != nil {
		return fmt.Errorf("validate imported release catalog: %w", err)
	}
	if !reflect.DeepEqual(actual, expected) {
		return errors.New("imported release catalog does not match the recovery kit manifest")
	}
	return nil
}

func validateFileDigest(filename string, expectedSize int64, expectedDigest string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	digest := sha256.New()
	written, err := io.Copy(digest, file)
	if err != nil || written != expectedSize || hex.EncodeToString(digest.Sum(nil)) != expectedDigest {
		return errors.New("file size or SHA-256 does not match")
	}
	return nil
}

func promoteCleanHostStage(dataRoot, stage string) error {
	rollback, err := os.MkdirTemp(dataRoot, ".clean-host-rollback-")
	if err != nil {
		return err
	}
	stageName, rollbackName := filepath.Base(stage), filepath.Base(rollback)
	rollbackCurrent := func() error {
		entries, readErr := os.ReadDir(rollback)
		if readErr != nil {
			return readErr
		}
		for _, entry := range entries {
			if err := os.Rename(filepath.Join(rollback, entry.Name()), filepath.Join(dataRoot, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	entries, err := os.ReadDir(dataRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == stageName || entry.Name() == rollbackName {
			continue
		}
		if err := os.Rename(filepath.Join(dataRoot, entry.Name()), filepath.Join(rollback, entry.Name())); err != nil {
			_ = rollbackCurrent()
			return fmt.Errorf("stage previous Fleet data for rollback: %w", err)
		}
	}
	stagedEntries, err := os.ReadDir(stage)
	if err != nil {
		_ = rollbackCurrent()
		return err
	}
	promoted := make([]string, 0, len(stagedEntries))
	for _, entry := range stagedEntries {
		if err := os.Rename(filepath.Join(stage, entry.Name()), filepath.Join(dataRoot, entry.Name())); err != nil {
			for _, name := range promoted {
				_ = os.RemoveAll(filepath.Join(dataRoot, name))
			}
			_ = rollbackCurrent()
			return fmt.Errorf("publish imported Fleet data: %w", err)
		}
		promoted = append(promoted, entry.Name())
	}
	if err := os.Remove(stage); err != nil {
		return fmt.Errorf("remove import staging directory: %w", err)
	}
	if err := os.RemoveAll(rollback); err != nil {
		return fmt.Errorf("clean previous Fleet data after successful import: %w", err)
	}
	return nil
}
