package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrConfirmation = errors.New("backup deletion confirmation does not match")
	ErrIntegrity    = errors.New("backup integrity check failed")
	ErrNotFound     = errors.New("backup not found")
)

var (
	backupIDPattern       = regexp.MustCompile(`^backup-[a-f0-9]{32}$`)
	backupFilenamePattern = regexp.MustCompile(`^hermes-fleet-[0-9]{8}T[0-9]{6}Z-[a-f0-9]{8}\.sqlite$`)
	sha256Pattern         = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type SnapshotStore interface {
	CreateBackup(context.Context, string) error
	VerifyBackup(context.Context, string) error
}

type Metadata struct {
	ID         string    `json:"id"`
	Filename   string    `json:"filename"`
	SizeBytes  int64     `json:"size_bytes"`
	SHA256     string    `json:"sha256"`
	CreatedAt  time.Time `json:"created_at"`
	VerifiedAt time.Time `json:"verified_at"`
}

type Manager struct {
	root       string
	store      SnapshotStore
	maxBackups int
	now        func() time.Time
	mu         sync.RWMutex
}

func New(root string, store SnapshotStore, maxBackups int) (*Manager, error) {
	if store == nil {
		return nil, errors.New("backup snapshot store is required")
	}
	if maxBackups < 1 {
		return nil, errors.New("backup retention limit must be positive")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve backup root: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create backup root: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, fmt.Errorf("inspect backup root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("backup root must be a directory")
	}
	if err := os.Chmod(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("secure backup root: %w", err)
	}
	if err := cleanupInterruptedBackups(absolute); err != nil {
		return nil, err
	}
	return &Manager{root: absolute, store: store, maxBackups: maxBackups, now: time.Now}, nil
}

func cleanupInterruptedBackups(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("inspect backup storage: %w", err)
	}
	names := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			return fmt.Errorf("%w: backup storage contains a directory", ErrIntegrity)
		}
		names[entry.Name()] = true
	}
	for name := range names {
		if strings.HasPrefix(name, ".backup-") && strings.HasSuffix(name, ".json.deleting") {
			id := strings.TrimSuffix(strings.TrimPrefix(name, "."), ".json.deleting")
			for _, candidate := range []string{id + ".sqlite", "." + id + ".sqlite.deleting", name} {
				if err := os.Remove(filepath.Join(root, candidate)); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("finish interrupted backup deletion: %w", err)
				}
			}
			continue
		}
		if strings.HasPrefix(name, ".backup-") && strings.HasSuffix(name, ".sqlite.deleting") {
			id := strings.TrimSuffix(strings.TrimPrefix(name, "."), ".sqlite.deleting")
			if names[id+".json"] {
				if err := os.Rename(filepath.Join(root, name), filepath.Join(root, id+".sqlite")); err != nil {
					return fmt.Errorf("restore interrupted backup deletion: %w", err)
				}
			} else if err := os.Remove(filepath.Join(root, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("finish interrupted backup deletion: %w", err)
			}
			continue
		}
		remove := strings.HasPrefix(name, ".backup-metadata-") || strings.HasSuffix(name, ".sqlite.tmp") || strings.HasSuffix(name, ".deleting")
		if strings.HasSuffix(name, ".sqlite") && !strings.HasPrefix(name, ".") {
			remove = !names[strings.TrimSuffix(name, ".sqlite")+".json"]
		}
		if strings.HasSuffix(name, ".json") && !strings.HasPrefix(name, ".") {
			remove = !names[strings.TrimSuffix(name, ".json")+".sqlite"]
		}
		if remove {
			if err := os.Remove(filepath.Join(root, name)); err != nil {
				return fmt.Errorf("remove interrupted backup artifact: %w", err)
			}
		}
	}
	return syncDirectory(root)
}

func (m *Manager) Create(ctx context.Context) (Metadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	items, err := m.list(ctx)
	if err != nil {
		return Metadata{}, err
	}
	id, suffix, err := newBackupID()
	if err != nil {
		return Metadata{}, err
	}
	createdAt := m.now().UTC()
	metadata := Metadata{
		ID: id, Filename: "hermes-fleet-" + createdAt.Format("20060102T150405Z") + "-" + suffix + ".sqlite",
		CreatedAt: createdAt,
	}
	temporaryPath := filepath.Join(m.root, "."+id+".sqlite.tmp")
	finalPath := m.databasePath(id)
	defer os.Remove(temporaryPath)
	if err := m.store.CreateBackup(ctx, temporaryPath); err != nil {
		return Metadata{}, err
	}
	if err := secureAndSyncFile(temporaryPath); err != nil {
		return Metadata{}, err
	}
	if err := m.store.VerifyBackup(ctx, temporaryPath); err != nil {
		return Metadata{}, fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	digest, size, err := hashFile(temporaryPath)
	if err != nil {
		return Metadata{}, err
	}
	metadata.SHA256 = digest
	metadata.SizeBytes = size
	metadata.VerifiedAt = m.now().UTC()
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return Metadata{}, fmt.Errorf("publish backup database: %w", err)
	}
	if err := m.writeMetadata(metadata); err != nil {
		_ = os.Remove(finalPath)
		return Metadata{}, err
	}
	if err := syncDirectory(m.root); err != nil {
		return Metadata{}, err
	}
	// Retention is a rolling window. Publish and verify the replacement first,
	// then remove the oldest retained backup so a failed snapshot never destroys
	// the last known-good recovery point.
	if err := m.pruneOldestLocked(items); err != nil {
		rollbackErr := m.deleteLocked(metadata)
		if rollbackErr != nil {
			return Metadata{}, fmt.Errorf("rotate backup retention: %v; remove unrotated backup: %w", err, rollbackErr)
		}
		return Metadata{}, err
	}
	return metadata, nil
}

func (m *Manager) pruneOldestLocked(previous []Metadata) error {
	excess := len(previous) + 1 - m.maxBackups
	for index := len(previous) - 1; index >= 0 && excess > 0; index-- {
		if err := m.deleteLocked(previous[index]); err != nil {
			return fmt.Errorf("prune oldest backup %s: %w", previous[index].Filename, err)
		}
		excess--
	}
	return nil
}

func (m *Manager) List(ctx context.Context) ([]Metadata, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.list(ctx)
}

func (m *Manager) list(ctx context.Context) ([]Metadata, error) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	names := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		names[entry.Name()] = true
	}
	for name := range names {
		var id, counterpart string
		switch {
		case strings.HasSuffix(name, ".json"):
			id = strings.TrimSuffix(name, ".json")
			counterpart = id + ".sqlite"
		case strings.HasSuffix(name, ".sqlite"):
			id = strings.TrimSuffix(name, ".sqlite")
			counterpart = id + ".json"
		default:
			return nil, fmt.Errorf("%w: unexpected backup artifact %s", ErrIntegrity, name)
		}
		if !backupIDPattern.MatchString(id) || !names[counterpart] {
			return nil, fmt.Errorf("%w: incomplete backup artifact pair", ErrIntegrity)
		}
	}
	items := make([]Metadata, 0)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		metadata, err := m.get(id)
		if err != nil {
			return nil, err
		}
		items = append(items, metadata)
	}
	sort.Slice(items, func(left, right int) bool { return items[left].CreatedAt.After(items[right].CreatedAt) })
	return items, nil
}

func (m *Manager) Verify(ctx context.Context, id string) (Metadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	metadata, err := m.get(id)
	if err != nil {
		return Metadata{}, err
	}
	path := m.databasePath(id)
	digest, size, err := hashFile(path)
	if err != nil {
		return Metadata{}, err
	}
	if digest != metadata.SHA256 || size != metadata.SizeBytes {
		return Metadata{}, fmt.Errorf("%w: checksum or size does not match metadata", ErrIntegrity)
	}
	if err := m.store.VerifyBackup(ctx, path); err != nil {
		return Metadata{}, fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	metadata.VerifiedAt = m.now().UTC()
	if err := m.writeMetadata(metadata); err != nil {
		return Metadata{}, err
	}
	if err := syncDirectory(m.root); err != nil {
		return Metadata{}, err
	}
	return metadata, nil
}

func (m *Manager) Open(id string) (Metadata, *os.File, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	metadata, err := m.get(id)
	if err != nil {
		return Metadata{}, nil, err
	}
	file, err := os.Open(m.databasePath(id))
	if err != nil {
		return Metadata{}, nil, fmt.Errorf("open backup: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return Metadata{}, nil, fmt.Errorf("inspect opened backup: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		file.Close()
		return Metadata{}, nil, fmt.Errorf("%w: backup artifact has unsafe type or permissions", ErrIntegrity)
	}
	return metadata, file, nil
}

func (m *Manager) Delete(ctx context.Context, id, confirmation string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	metadata, err := m.get(id)
	if err != nil {
		return err
	}
	if confirmation != metadata.Filename {
		return ErrConfirmation
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.deleteLocked(metadata)
}

func (m *Manager) deleteLocked(metadata Metadata) error {
	id := metadata.ID
	databasePath := m.databasePath(id)
	metadataPath := m.metadataPath(id)
	databaseTrash := filepath.Join(m.root, "."+id+".sqlite.deleting")
	metadataTrash := filepath.Join(m.root, "."+id+".json.deleting")
	if err := os.Rename(metadataPath, metadataTrash); err != nil {
		return fmt.Errorf("stage backup metadata deletion: %w", err)
	}
	if err := os.Rename(databasePath, databaseTrash); err != nil {
		_ = os.Rename(metadataTrash, metadataPath)
		return fmt.Errorf("stage backup database deletion: %w", err)
	}
	if err := os.Remove(databaseTrash); err != nil {
		_ = os.Rename(metadataTrash, metadataPath)
		_ = os.Rename(databaseTrash, databasePath)
		return fmt.Errorf("delete backup database: %w", err)
	}
	if err := os.Remove(metadataTrash); err != nil {
		return fmt.Errorf("delete backup metadata: %w", err)
	}
	return syncDirectory(m.root)
}

func (m *Manager) get(id string) (Metadata, error) {
	if !backupIDPattern.MatchString(id) {
		return Metadata{}, ErrNotFound
	}
	data, err := os.ReadFile(m.metadataPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return Metadata{}, ErrNotFound
	}
	if err != nil {
		return Metadata{}, fmt.Errorf("read backup metadata: %w", err)
	}
	var metadata Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return Metadata{}, fmt.Errorf("%w: invalid backup metadata", ErrIntegrity)
	}
	if metadata.ID != id || !backupFilenamePattern.MatchString(metadata.Filename) || metadata.SizeBytes <= 0 ||
		!sha256Pattern.MatchString(metadata.SHA256) || metadata.CreatedAt.IsZero() || metadata.VerifiedAt.IsZero() {
		return Metadata{}, fmt.Errorf("%w: incomplete backup metadata", ErrIntegrity)
	}
	for _, path := range []string{m.metadataPath(id), m.databasePath(id)} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return Metadata{}, ErrNotFound
		}
		if err != nil {
			return Metadata{}, fmt.Errorf("inspect backup artifact: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return Metadata{}, fmt.Errorf("%w: backup artifact has unsafe type or permissions", ErrIntegrity)
		}
	}
	return metadata, nil
}

func (m *Manager) writeMetadata(metadata Metadata) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("encode backup metadata: %w", err)
	}
	temporary, err := os.CreateTemp(m.root, ".backup-metadata-*")
	if err != nil {
		return fmt.Errorf("create temporary backup metadata: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, m.metadataPath(metadata.ID)); err != nil {
		return fmt.Errorf("publish backup metadata: %w", err)
	}
	return nil
}

func (m *Manager) databasePath(id string) string { return filepath.Join(m.root, id+".sqlite") }
func (m *Manager) metadataPath(id string) string { return filepath.Join(m.root, id+".json") }

func newBackupID() (string, string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", "", fmt.Errorf("generate backup identity: %w", err)
	}
	encoded := hex.EncodeToString(buffer)
	return "backup-" + encoded, encoded[:8], nil
}

func secureAndSyncFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open backup database: %w", err)
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("secure backup database: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync backup database: %w", err)
	}
	return nil
}

func hashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("open backup for hashing: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, fmt.Errorf("hash backup: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open backup directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync backup directory: %w", err)
	}
	return nil
}
