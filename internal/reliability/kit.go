package reliability

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/backup"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/recovery"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/releases"
)

var ErrRecoveryKitIncomplete = errors.New("recovery kit prerequisites are incomplete")

type KitManifest struct {
	FormatVersion       int                 `json:"format_version"`
	CreatedAt           time.Time           `json:"created_at"`
	FleetVersion        string              `json:"fleet_version"`
	BuildID             string              `json:"build_id"`
	ControlPlaneBackup  backup.Metadata     `json:"control_plane_backup"`
	InstanceBackups     []recovery.Metadata `json:"instance_backups"`
	ReleaseCatalog      releases.Catalog    `json:"release_catalog"`
	EncryptionKeyPolicy string              `json:"encryption_key_policy"`
}

type RecoveryKit struct {
	backups      *backup.Manager
	recovery     *recovery.Manager
	instances    func(context.Context) ([]domain.Instance, error)
	fleetVersion string
	buildID      string
	catalog      releases.Catalog
	now          func() time.Time
}

func NewRecoveryKit(backups *backup.Manager, recoveryPoints *recovery.Manager, instances func(context.Context) ([]domain.Instance, error), fleetVersion, buildID string, catalog releases.Catalog) (*RecoveryKit, error) {
	if backups == nil || recoveryPoints == nil || instances == nil {
		return nil, errors.New("recovery kit dependencies are required")
	}
	return &RecoveryKit{
		backups: backups, recovery: recoveryPoints, instances: instances,
		fleetVersion: fleetVersion, buildID: buildID, catalog: catalog, now: time.Now,
	}, nil
}

func (k *RecoveryKit) Export(ctx context.Context, destination io.Writer) (KitManifest, error) {
	controlPlaneBackups, err := k.backups.List(ctx)
	if err != nil {
		return KitManifest{}, fmt.Errorf("list control-plane backups: %w", err)
	}
	if len(controlPlaneBackups) == 0 {
		return KitManifest{}, fmt.Errorf("%w: create a verified control-plane backup first", ErrRecoveryKitIncomplete)
	}
	controlPlaneBackup, err := k.backups.Verify(ctx, controlPlaneBackups[0].ID)
	if err != nil {
		return KitManifest{}, fmt.Errorf("verify control-plane backup: %w", err)
	}
	instances, err := k.instances(ctx)
	if err != nil {
		return KitManifest{}, fmt.Errorf("list managed instances: %w", err)
	}
	instanceBackups := make([]recovery.Metadata, 0, len(instances))
	for _, instance := range instances {
		points, listErr := k.recovery.List(ctx, instance.ID)
		if listErr != nil {
			return KitManifest{}, fmt.Errorf("list backups for %s: %w", instance.Name, listErr)
		}
		var selected *recovery.Metadata
		for _, point := range points {
			if point.Status != recovery.StatusReady {
				continue
			}
			verified, verifyErr := k.recovery.Verify(ctx, point.ID)
			if verifyErr != nil {
				return KitManifest{}, fmt.Errorf("verify backup for %s: %w", instance.Name, verifyErr)
			}
			selected = &verified
			break
		}
		if selected == nil {
			return KitManifest{}, fmt.Errorf("%w: instance %s has no verified backup", ErrRecoveryKitIncomplete, instance.Name)
		}
		instanceBackups = append(instanceBackups, *selected)
	}
	manifest := KitManifest{
		FormatVersion: 1, CreatedAt: k.now().UTC(), FleetVersion: k.fleetVersion, BuildID: k.buildID,
		ControlPlaneBackup: controlPlaneBackup, InstanceBackups: instanceBackups, ReleaseCatalog: k.catalog,
		EncryptionKeyPolicy: "FLEET_RECOVERY_ENCRYPTION_KEY is intentionally excluded and must be retained separately",
	}
	archive := tar.NewWriter(destination)
	if err := writeJSONEntry(archive, "manifest.json", manifest); err != nil {
		_ = archive.Close()
		return KitManifest{}, err
	}
	if err := writeJSONEntry(archive, "hermes-releases.json", k.catalog); err != nil {
		_ = archive.Close()
		return KitManifest{}, err
	}
	_, controlPlaneFile, err := k.backups.Open(controlPlaneBackup.ID)
	if err != nil {
		_ = archive.Close()
		return KitManifest{}, err
	}
	if err := writeFileEntry(ctx, archive, filepath.Join("control-plane", controlPlaneBackup.Filename), controlPlaneFile, controlPlaneBackup.SizeBytes); err != nil {
		controlPlaneFile.Close()
		_ = archive.Close()
		return KitManifest{}, err
	}
	if err := controlPlaneFile.Close(); err != nil {
		_ = archive.Close()
		return KitManifest{}, err
	}
	for _, point := range instanceBackups {
		_, encrypted, openErr := k.recovery.OpenEncrypted(point.ID)
		if openErr != nil {
			_ = archive.Close()
			return KitManifest{}, openErr
		}
		base := filepath.Join("instances", safeArchiveName(point.InstanceName), point.ID)
		if err := writeJSONEntry(archive, base+".json", point); err != nil {
			encrypted.Close()
			_ = archive.Close()
			return KitManifest{}, err
		}
		if err := writeFileEntry(ctx, archive, base+".enc", encrypted, point.EncryptedSizeBytes); err != nil {
			encrypted.Close()
			_ = archive.Close()
			return KitManifest{}, err
		}
		if err := encrypted.Close(); err != nil {
			_ = archive.Close()
			return KitManifest{}, err
		}
	}
	if err := archive.Close(); err != nil {
		return KitManifest{}, fmt.Errorf("finish recovery kit: %w", err)
	}
	return manifest, nil
}

func writeJSONEntry(archive *tar.Writer, name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := archive.WriteHeader(&tar.Header{Name: filepath.ToSlash(name), Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = archive.Write(data)
	return err
}

func writeFileEntry(ctx context.Context, archive *tar.Writer, name string, source io.Reader, size int64) error {
	if size < 1 {
		return errors.New("recovery kit artifact size is invalid")
	}
	if err := archive.WriteHeader(&tar.Header{Name: filepath.ToSlash(name), Mode: 0o600, Size: size, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	written, err := io.Copy(archive, &contextReader{ctx: ctx, reader: source})
	if err != nil {
		return err
	}
	if written != size {
		return errors.New("recovery kit artifact size changed during export")
	}
	return nil
}

func safeArchiveName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." || filepath.Base(value) != value {
		return "instance"
	}
	return value
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
