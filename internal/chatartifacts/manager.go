package chatartifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const MaximumBytes int64 = 25 << 20

const (
	StatusPreparing = "preparing"
	StatusReady     = "ready"
	StatusRejected  = "rejected"
	StatusMissing   = "missing"
	StatusExpired   = "expired"
	StatusFailed    = "failed"
	StatusDeleted   = "deleted"

	DefaultSessionMaxBytes  int64 = 256 << 20
	DefaultInstanceMaxBytes int64 = 1 << 30
	DefaultTotalMaxBytes    int64 = 2 << 30
	DefaultRetention              = 30 * 24 * time.Hour
	orphanGracePeriod             = time.Hour
)

var (
	ErrNotFound        = errors.New("chat artifact not found")
	ErrInvalid         = errors.New("invalid chat artifact")
	ErrQuota           = errors.New("chat artifact quota exceeded")
	ErrMissing         = errors.New("chat artifact content is missing")
	ErrExpired         = errors.New("chat artifact expired")
	sessionIDPattern   = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$`)
	operationIDPattern = sessionIDPattern
	instanceIDPattern  = sessionIDPattern
	artifactIDPattern  = regexp.MustCompile(`^artifact-[a-f0-9]{32}$`)
	sha256Pattern      = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type Config struct {
	SessionMaxBytes  int64
	InstanceMaxBytes int64
	TotalMaxBytes    int64
	Retention        time.Duration
}

func DefaultConfig() Config {
	return Config{
		SessionMaxBytes: DefaultSessionMaxBytes, InstanceMaxBytes: DefaultInstanceMaxBytes,
		TotalMaxBytes: DefaultTotalMaxBytes, Retention: DefaultRetention,
	}
}

type Metadata struct {
	ID          string     `json:"id"`
	InstanceID  string     `json:"instance_id,omitempty"`
	SessionID   string     `json:"session_id"`
	OperationID string     `json:"operation_id"`
	Name        string     `json:"name"`
	Kind        string     `json:"kind"`
	MediaType   string     `json:"media_type"`
	SizeBytes   int64      `json:"size_bytes"`
	SHA256      string     `json:"sha256"`
	Status      string     `json:"status"`
	Error       string     `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
}

type ReconcileReport struct {
	Ready          int
	Expired        int
	Missing        int
	Failed         int
	Invalid        int
	RemovedOrphans int
	RemovedStaging int
	TotalBytes     int64
}

type usage struct {
	total     int64
	sessions  map[string]int64
	instances map[string]int64
}

type Manager struct {
	root   string
	config Config
	mu     sync.Mutex
	usage  usage
}

func New(root string, configurations ...Config) (*Manager, error) {
	configuration := DefaultConfig()
	if len(configurations) > 1 {
		return nil, errors.New("chat artifact manager accepts at most one configuration")
	}
	if len(configurations) == 1 {
		configuration = configurations[0]
	}
	if err := validateConfig(configuration); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve chat artifact directory: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create chat artifact directory: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("chat artifact directory must be a real directory")
	}
	manager := &Manager{root: absolute, config: configuration}
	if _, err := manager.Reconcile(time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("reconcile chat artifact storage: %w", err)
	}
	return manager, nil
}

func validateConfig(configuration Config) error {
	if configuration.SessionMaxBytes < MaximumBytes || configuration.InstanceMaxBytes < configuration.SessionMaxBytes ||
		configuration.TotalMaxBytes < configuration.InstanceMaxBytes || configuration.Retention < time.Hour {
		return errors.New("chat artifact limits must allow one artifact, increase from session to total, and retain content for at least one hour")
	}
	return nil
}

func ValidArtifactID(value string) bool { return artifactIDPattern.MatchString(value) }

func ValidSessionID(value string) bool { return sessionIDPattern.MatchString(value) }

func ValidInstanceID(value string) bool { return instanceIDPattern.MatchString(value) }

func ValidStatus(value string) bool {
	switch value {
	case StatusPreparing, StatusReady, StatusRejected, StatusMissing, StatusExpired, StatusFailed, StatusDeleted:
		return true
	default:
		return false
	}
}

func ValidKind(value string) bool {
	return value == "file" || value == "image" || value == "audio" || value == "video"
}

func (m *Manager) Put(
	ctx context.Context,
	metadata Metadata,
	source io.Reader,
	verifyLease func() error,
) (Metadata, error) {
	if err := validatePutMetadata(metadata); err != nil {
		return Metadata{}, fmt.Errorf("%w: metadata is invalid", ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	var existing *Metadata
	if current, _, err := m.getLocked(metadata.SessionID, metadata.ID, time.Now().UTC(), true); err == nil {
		if compatibleMetadata(current, metadata) && current.Status == StatusReady {
			return current, nil
		}
		if !compatibleMetadata(current, metadata) || current.Status != StatusPreparing {
			return Metadata{}, fmt.Errorf("%w: identity already contains different content", ErrInvalid)
		}
		existing = &current
	} else if !errors.Is(err, ErrNotFound) {
		return Metadata{}, err
	}
	if err := m.checkQuotaLocked(metadata); err != nil {
		return Metadata{}, err
	}
	directory, err := m.sessionDirectory(metadata.SessionID, true)
	if err != nil {
		return Metadata{}, err
	}
	stage, err := os.CreateTemp(directory, ".artifact-upload-*")
	if err != nil {
		return Metadata{}, err
	}
	stageName := stage.Name()
	defer os.Remove(stageName)
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(stage, hasher), io.LimitReader(source, MaximumBytes+1))
	if copyErr == nil && written > MaximumBytes {
		copyErr = fmt.Errorf("%w: content exceeds %d bytes", ErrInvalid, MaximumBytes)
	}
	if syncErr := stage.Sync(); copyErr == nil && syncErr != nil {
		copyErr = syncErr
	}
	if closeErr := stage.Close(); copyErr == nil && closeErr != nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return Metadata{}, copyErr
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	if written != metadata.SizeBytes || digest != metadata.SHA256 {
		return Metadata{}, fmt.Errorf("%w: body does not match its declared size or digest", ErrInvalid)
	}
	if verifyLease != nil {
		if err := verifyLease(); err != nil {
			return Metadata{}, err
		}
	}
	if existing != nil {
		metadata.CreatedAt = existing.CreatedAt
	} else {
		metadata.CreatedAt = metadata.CreatedAt.UTC()
	}
	if metadata.CreatedAt.IsZero() {
		metadata.CreatedAt = time.Now().UTC()
	}
	expiresAt := metadata.CreatedAt.Add(m.config.Retention)
	metadata.ExpiresAt = &expiresAt
	metadata.Status = StatusReady
	metadata.Error = ""
	dataPath, metadataPath := m.paths(directory, metadata.ID)
	if err := os.Rename(stageName, dataPath); err != nil {
		return Metadata{}, err
	}
	if err := writeMetadata(metadataPath, metadata); err != nil {
		_ = os.Remove(dataPath)
		return Metadata{}, err
	}
	m.addUsageLocked(metadata)
	return metadata, nil
}

func (m *Manager) checkQuotaLocked(metadata Metadata) error {
	if m.usage.sessions[metadata.SessionID]+metadata.SizeBytes > m.config.SessionMaxBytes {
		return fmt.Errorf("%w: session limit is %d bytes", ErrQuota, m.config.SessionMaxBytes)
	}
	if m.usage.instances[metadata.InstanceID]+metadata.SizeBytes > m.config.InstanceMaxBytes {
		return fmt.Errorf("%w: instance limit is %d bytes", ErrQuota, m.config.InstanceMaxBytes)
	}
	if m.usage.total+metadata.SizeBytes > m.config.TotalMaxBytes {
		return fmt.Errorf("%w: Fleet limit is %d bytes", ErrQuota, m.config.TotalMaxBytes)
	}
	return nil
}

func (m *Manager) Get(sessionID, artifactID string) (Metadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	metadata, _, err := m.getLocked(sessionID, artifactID, time.Now().UTC(), true)
	return metadata, err
}

func (m *Manager) Open(sessionID, artifactID string) (Metadata, *os.File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	metadata, dataPath, err := m.getLocked(sessionID, artifactID, time.Now().UTC(), true)
	if err != nil {
		return Metadata{}, nil, err
	}
	if metadata.Status == StatusExpired {
		return metadata, nil, ErrExpired
	}
	if metadata.Status == StatusMissing {
		return metadata, nil, ErrMissing
	}
	if metadata.Status != StatusReady {
		return metadata, nil, ErrNotFound
	}
	file, err := os.Open(dataPath)
	if errors.Is(err, os.ErrNotExist) {
		return metadata, nil, ErrMissing
	}
	if err != nil {
		return Metadata{}, nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != metadata.SizeBytes {
		_ = file.Close()
		return metadata, nil, ErrMissing
	}
	return metadata, file, nil
}

func (m *Manager) getLocked(sessionID, artifactID string, now time.Time, persist bool) (Metadata, string, error) {
	directory, err := m.sessionDirectory(sessionID, false)
	if err != nil {
		return Metadata{}, "", err
	}
	if !ValidArtifactID(artifactID) {
		return Metadata{}, "", ErrNotFound
	}
	dataPath, metadataPath := m.paths(directory, artifactID)
	metadata, changed, err := m.readMetadata(metadataPath)
	if errors.Is(err, os.ErrNotExist) {
		return Metadata{}, "", ErrNotFound
	}
	if err != nil || metadata.SessionID != sessionID || metadata.ID != artifactID {
		return Metadata{}, "", errors.New("chat artifact metadata is invalid")
	}
	if metadata.Status == StatusReady && metadata.ExpiresAt != nil && !now.Before(*metadata.ExpiresAt) {
		_ = os.Remove(dataPath)
		m.removeUsageLocked(metadata)
		metadata.Status = StatusExpired
		metadata.Error = "This output expired under the Fleet artifact retention policy."
		changed = true
	}
	if metadata.Status == StatusReady {
		if !validDataFile(dataPath, metadata.SizeBytes) {
			_ = os.Remove(dataPath)
			m.removeUsageLocked(metadata)
			metadata.Status = StatusMissing
			metadata.Error = "The stored output is missing or no longer matches its manifest."
			changed = true
		}
	}
	if changed && persist {
		if err := writeMetadata(metadataPath, metadata); err != nil {
			return Metadata{}, "", err
		}
	}
	return metadata, dataPath, nil
}

func (m *Manager) DeleteSession(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	directory, err := m.sessionDirectory(sessionID, false)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	_, err = m.reconcileLocked(time.Now().UTC())
	return err
}

func (m *Manager) Reconcile(now time.Time) (ReconcileReport, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reconcileLocked(now.UTC())
}

func (m *Manager) reconcileLocked(now time.Time) (ReconcileReport, error) {
	report := ReconcileReport{}
	m.usage = usage{sessions: map[string]int64{}, instances: map[string]int64{}}
	sessions, err := os.ReadDir(m.root)
	if err != nil {
		return report, err
	}
	for _, sessionEntry := range sessions {
		if !sessionEntry.IsDir() || !sessionIDPattern.MatchString(sessionEntry.Name()) {
			continue
		}
		directory := filepath.Join(m.root, sessionEntry.Name())
		entries, readErr := os.ReadDir(directory)
		if readErr != nil {
			return report, readErr
		}
		metadataIDs := map[string]bool{}
		for _, entry := range entries {
			name := entry.Name()
			path := filepath.Join(directory, name)
			if strings.HasPrefix(name, ".artifact-") {
				if oldEnough(entry, now, orphanGracePeriod) && os.Remove(path) == nil {
					report.RemovedStaging++
				}
				continue
			}
			if entry.IsDir() || !strings.HasSuffix(name, ".json") {
				continue
			}
			artifactID := strings.TrimSuffix(name, ".json")
			if !ValidArtifactID(artifactID) {
				report.Invalid++
				continue
			}
			metadataIDs[artifactID] = true
			metadata, changed, metadataErr := m.readMetadata(path)
			if metadataErr != nil || metadata.ID != artifactID || metadata.SessionID != sessionEntry.Name() {
				report.Invalid++
				continue
			}
			dataPath, _ := m.paths(directory, artifactID)
			if metadata.Status == StatusPreparing && !metadata.CreatedAt.After(now.Add(-orphanGracePeriod)) {
				_ = os.Remove(dataPath)
				metadata.Status = StatusFailed
				metadata.Error = "The output upload did not complete before the Fleet staging deadline."
				changed = true
				report.Failed++
			}
			if metadata.Status == StatusReady && metadata.ExpiresAt != nil && !now.Before(*metadata.ExpiresAt) {
				_ = os.Remove(dataPath)
				metadata.Status = StatusExpired
				metadata.Error = "This output expired under the Fleet artifact retention policy."
				changed = true
				report.Expired++
			}
			if metadata.Status == StatusReady {
				if !validDataFile(dataPath, metadata.SizeBytes) {
					_ = os.Remove(dataPath)
					metadata.Status = StatusMissing
					metadata.Error = "The stored output is missing or no longer matches its manifest."
					changed = true
					report.Missing++
				} else {
					m.addUsageLocked(metadata)
					report.Ready++
				}
			} else {
				_ = os.Remove(dataPath)
			}
			if changed {
				if err := writeMetadata(path, metadata); err != nil {
					return report, err
				}
			}
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".data") {
				continue
			}
			artifactID := strings.TrimSuffix(name, ".data")
			if !metadataIDs[artifactID] && oldEnough(entry, now, orphanGracePeriod) {
				if os.Remove(filepath.Join(directory, name)) == nil {
					report.RemovedOrphans++
				}
			}
		}
	}
	report.TotalBytes = m.usage.total
	return report, nil
}

func oldEnough(entry os.DirEntry, now time.Time, age time.Duration) bool {
	info, err := entry.Info()
	return err == nil && !info.ModTime().After(now.Add(-age))
}

func (m *Manager) readMetadata(path string) (Metadata, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Metadata{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Metadata{}, false, errors.New("chat artifact metadata is invalid")
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return Metadata{}, false, err
	}
	var metadata Metadata
	if json.Unmarshal(encoded, &metadata) != nil {
		return Metadata{}, false, errors.New("chat artifact metadata is invalid")
	}
	changed := false
	if metadata.Status == "" {
		metadata.Status = StatusReady
		changed = true
	}
	if metadata.ExpiresAt == nil && metadata.Status == StatusReady && !metadata.CreatedAt.IsZero() {
		expiresAt := metadata.CreatedAt.UTC().Add(m.config.Retention)
		metadata.ExpiresAt = &expiresAt
		changed = true
	}
	if err := validateStoredMetadata(metadata); err != nil {
		return Metadata{}, false, err
	}
	return metadata, changed, nil
}

func validDataFile(path string, expectedSize int64) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Size() == expectedSize
}

func writeMetadata(path string, metadata Metadata) error {
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	stage, err := os.CreateTemp(directory, ".artifact-metadata-*")
	if err != nil {
		return err
	}
	stageName := stage.Name()
	defer os.Remove(stageName)
	if err := stage.Chmod(0o600); err == nil {
		_, err = stage.Write(encoded)
	}
	if err == nil {
		err = stage.Sync()
	}
	if closeErr := stage.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(stageName, path)
	}
	return err
}

func (m *Manager) addUsageLocked(metadata Metadata) {
	if m.usage.sessions == nil {
		m.usage.sessions = map[string]int64{}
	}
	if m.usage.instances == nil {
		m.usage.instances = map[string]int64{}
	}
	m.usage.total += metadata.SizeBytes
	m.usage.sessions[metadata.SessionID] += metadata.SizeBytes
	m.usage.instances[metadata.InstanceID] += metadata.SizeBytes
}

func (m *Manager) removeUsageLocked(metadata Metadata) {
	m.usage.total = max(0, m.usage.total-metadata.SizeBytes)
	m.usage.sessions[metadata.SessionID] = max(0, m.usage.sessions[metadata.SessionID]-metadata.SizeBytes)
	m.usage.instances[metadata.InstanceID] = max(0, m.usage.instances[metadata.InstanceID]-metadata.SizeBytes)
}

func validatePutMetadata(metadata Metadata) error {
	if err := validateIdentityAndDescription(metadata); err != nil {
		return err
	}
	if !instanceIDPattern.MatchString(metadata.InstanceID) || metadata.SizeBytes < 1 || metadata.SizeBytes > MaximumBytes ||
		!sha256Pattern.MatchString(metadata.SHA256) || metadata.MediaType == "" ||
		(metadata.Status != "" && metadata.Status != StatusPreparing) {
		return errors.New("chat artifact upload metadata is incomplete")
	}
	return nil
}

func validateStoredMetadata(metadata Metadata) error {
	if err := validateIdentityAndDescription(metadata); err != nil {
		return err
	}
	if !instanceIDPattern.MatchString(metadata.InstanceID) || !ValidStatus(metadata.Status) || metadata.CreatedAt.IsZero() {
		return errors.New("chat artifact registry metadata is incomplete")
	}
	if metadata.SizeBytes < 0 || metadata.SizeBytes > MaximumBytes || (metadata.SizeBytes > 0 && !sha256Pattern.MatchString(metadata.SHA256)) ||
		(metadata.SHA256 != "" && !sha256Pattern.MatchString(metadata.SHA256)) {
		return errors.New("chat artifact content metadata is invalid")
	}
	if metadata.Status == StatusReady && (metadata.SizeBytes < 1 || metadata.SHA256 == "" || metadata.MediaType == "") {
		return errors.New("ready chat artifact metadata is incomplete")
	}
	if (metadata.Status == StatusRejected || metadata.Status == StatusMissing || metadata.Status == StatusExpired || metadata.Status == StatusFailed) && metadata.Error == "" {
		return errors.New("unavailable chat artifact must include an error")
	}
	return nil
}

func validateIdentityAndDescription(metadata Metadata) error {
	if !sessionIDPattern.MatchString(metadata.SessionID) || !operationIDPattern.MatchString(metadata.OperationID) || !ValidArtifactID(metadata.ID) {
		return errors.New("chat artifact identity is invalid")
	}
	if metadata.InstanceID != "" && !instanceIDPattern.MatchString(metadata.InstanceID) {
		return errors.New("chat artifact instance identity is invalid")
	}
	if metadata.Name == "" || len(metadata.Name) > 80 {
		return errors.New("chat artifact metadata is incomplete")
	}
	if !ValidKind(metadata.Kind) {
		return errors.New("chat artifact kind is invalid")
	}
	if len(metadata.MediaType) > 127 {
		return errors.New("chat artifact media type is invalid")
	}
	if len(metadata.Error) > 200 {
		return errors.New("chat artifact error is invalid")
	}
	return nil
}

func (m *Manager) sessionDirectory(sessionID string, create bool) (string, error) {
	if !sessionIDPattern.MatchString(sessionID) {
		return "", ErrNotFound
	}
	directory := filepath.Join(m.root, sessionID)
	if create {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return "", err
		}
		return directory, nil
	}
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("chat artifact session directory is invalid")
	}
	return directory, nil
}

func (m *Manager) paths(directory, artifactID string) (string, string) {
	return filepath.Join(directory, artifactID+".data"), filepath.Join(directory, artifactID+".json")
}

func compatibleMetadata(left, right Metadata) bool {
	return left.ID == right.ID && left.InstanceID == right.InstanceID && left.SessionID == right.SessionID &&
		left.OperationID == right.OperationID && left.Name == right.Name && left.Kind == right.Kind &&
		(left.MediaType == "" || right.MediaType == "" || left.MediaType == right.MediaType) &&
		(left.SizeBytes == 0 || right.SizeBytes == 0 || left.SizeBytes == right.SizeBytes) &&
		(left.SHA256 == "" || right.SHA256 == "" || left.SHA256 == right.SHA256)
}
