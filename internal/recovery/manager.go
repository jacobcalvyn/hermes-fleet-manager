package recovery

import (
	"archive/tar"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/providers"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/recoverycodec"
)

const recoveryKitAuthenticationDomain = "hermes-fleet/recovery-kit-manifest/v2"

const (
	StatusCreating = "CREATING"
	StatusUploaded = "UPLOADED"
	StatusReady    = "READY"
	StatusFailed   = "FAILED"
)

var (
	ErrBusy         = errors.New("an instance recovery point is already being created")
	ErrConfirmation = errors.New("recovery point deletion confirmation does not match")
	ErrIntegrity    = errors.New("recovery point integrity check failed")
	ErrLimitReached = errors.New("recovery point retention limit reached")
	ErrNotFound     = errors.New("recovery point not found")
	ErrState        = errors.New("recovery point is not in the required state")
)

var (
	recoveryIDPattern = regexp.MustCompile(`^recovery-[a-f0-9]{32}$`)
	filenamePattern   = regexp.MustCompile(`^hermes-[a-z][a-z0-9-]{2,31}-[0-9]{8}T[0-9]{6}Z-[a-f0-9]{8}\.tar$`)
	sha256Pattern     = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type Reservation struct {
	InstanceID      string
	InstanceName    string
	HostID          string
	OperationID     string
	JobID           string
	Image           string
	ImageID         string
	Provider        string
	Model           string
	Reasoning       string
	ServiceTier     string
	CodexConfigured bool
	ProjectName     string
	DataVolume      string
	ManagedPath     string
	AgentVersion    string
	Automated       bool
	WorkflowID      string
}

type Manifest struct {
	FormatVersion   int       `json:"format_version"`
	RecoveryPointID string    `json:"recovery_point_id"`
	InstanceID      string    `json:"instance_id"`
	InstanceName    string    `json:"instance_name"`
	Image           string    `json:"image"`
	ImageID         string    `json:"image_id"`
	Provider        string    `json:"provider"`
	Model           string    `json:"model"`
	Reasoning       string    `json:"reasoning"`
	ServiceTier     string    `json:"service_tier"`
	CodexConfigured bool      `json:"codex_configured"`
	ProjectName     string    `json:"project_name"`
	DataVolume      string    `json:"data_volume"`
	ManagedPath     string    `json:"managed_path"`
	AgentVersion    string    `json:"agent_version"`
	CreatedAt       time.Time `json:"created_at"`
}

type Metadata struct {
	ID                 string    `json:"id"`
	InstanceID         string    `json:"instance_id"`
	InstanceName       string    `json:"instance_name"`
	HostID             string    `json:"host_id"`
	OperationID        string    `json:"operation_id"`
	JobID              string    `json:"job_id"`
	Filename           string    `json:"filename"`
	Status             string    `json:"status"`
	SizeBytes          int64     `json:"size_bytes"`
	EncryptedSizeBytes int64     `json:"encrypted_size_bytes"`
	SHA256             string    `json:"sha256,omitempty"`
	Image              string    `json:"image"`
	ImageID            string    `json:"image_id"`
	Provider           string    `json:"provider"`
	Model              string    `json:"model"`
	Reasoning          string    `json:"reasoning"`
	ServiceTier        string    `json:"service_tier"`
	CodexConfigured    bool      `json:"codex_configured"`
	ProjectName        string    `json:"project_name"`
	DataVolume         string    `json:"data_volume"`
	ManagedPath        string    `json:"managed_path"`
	AgentVersion       string    `json:"agent_version"`
	Automated          bool      `json:"automated"`
	WorkflowID         string    `json:"workflow_id,omitempty"`
	Error              string    `json:"error,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UploadedAt         time.Time `json:"uploaded_at,omitempty"`
	VerifiedAt         time.Time `json:"verified_at,omitempty"`
}

type Manager struct {
	root           string
	key            []byte
	maxPerInstance int
	maxBytes       int64
	now            func() time.Time
	mu             sync.RWMutex
	pointLocks     [64]sync.Mutex
}

func New(root, hexadecimalKey string, maxPerHost int, maxBytes int64) (*Manager, error) {
	key, err := hex.DecodeString(hexadecimalKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("recovery encryption key must contain exactly 64 hexadecimal characters")
	}
	if maxPerHost < 1 || maxPerHost > 100 {
		return nil, errors.New("recovery point retention must be between 1 and 100")
	}
	if maxBytes < 1 {
		return nil, errors.New("maximum recovery point size must be positive")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve recovery point root: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create recovery point root: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, fmt.Errorf("inspect recovery point root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("recovery point root must be a directory")
	}
	if err := os.Chmod(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("secure recovery point root: %w", err)
	}
	if err := cleanupTemporaryFiles(absolute); err != nil {
		return nil, err
	}
	manager := &Manager{root: absolute, key: key, maxPerInstance: maxPerHost, maxBytes: maxBytes, now: time.Now}
	if err := manager.reconcileInterruptedPublications(); err != nil {
		return nil, err
	}
	return manager, nil
}

// AuthenticateRecoveryKitManifest creates a domain-separated authentication tag
// without exposing the recovery encryption key outside this package.
func (m *Manager) AuthenticateRecoveryKitManifest(payload []byte) string {
	return authenticateRecoveryKitManifest(m.key, payload)
}

// VerifyRecoveryKitManifest authenticates a recovery kit manifest using the
// separately retained recovery key required by clean-host import.
func VerifyRecoveryKitManifest(hexadecimalKey string, payload []byte, tag string) error {
	key, err := hex.DecodeString(strings.TrimSpace(hexadecimalKey))
	if err != nil || len(key) != 32 {
		return errors.New("recovery encryption key must contain exactly 64 hexadecimal characters")
	}
	expected, err := hex.DecodeString(strings.TrimSpace(tag))
	if err != nil || len(expected) != sha256.Size {
		return errors.New("recovery kit manifest authentication tag is invalid")
	}
	actual, err := hex.DecodeString(authenticateRecoveryKitManifest(key, payload))
	if err != nil || !hmac.Equal(actual, expected) {
		return errors.New("recovery kit manifest authentication failed")
	}
	return nil
}

func authenticateRecoveryKitManifest(key, payload []byte) string {
	derivation := hmac.New(sha256.New, key)
	_, _ = derivation.Write([]byte(recoveryKitAuthenticationDomain))
	authenticationKey := derivation.Sum(nil)
	mac := hmac.New(sha256.New, authenticationKey)
	_, _ = mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// ValidateMetadata verifies the portable identity and integrity fields used by
// recovery kit import before any filesystem path is derived from them.
func ValidateMetadata(metadata Metadata) error {
	return validateMetadata(metadata)
}

func (m *Manager) reconcileInterruptedPublications() error {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return fmt.Errorf("inspect recovery point storage: %w", err)
	}
	names := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			names[entry.Name()] = true
		}
	}
	for name := range names {
		if strings.HasSuffix(name, ".enc") {
			id := strings.TrimSuffix(name, ".enc")
			metadataName := id + ".json"
			if !names[metadataName] {
				if err := os.Remove(filepath.Join(m.root, name)); err != nil {
					return fmt.Errorf("remove orphan recovery artifact: %w", err)
				}
				continue
			}
			data, readErr := os.ReadFile(filepath.Join(m.root, metadataName))
			var metadata Metadata
			if readErr == nil && json.Unmarshal(data, &metadata) == nil && (metadata.Status == StatusCreating || metadata.Status == StatusFailed) {
				if err := os.Remove(filepath.Join(m.root, name)); err != nil {
					return fmt.Errorf("remove interrupted recovery artifact: %w", err)
				}
			}
		}
	}
	return syncDirectory(m.root)
}

func (m *Manager) Reserve(ctx context.Context, reservation Reservation) (Metadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	items, err := m.list(ctx, reservation.InstanceID)
	if err != nil {
		return Metadata{}, err
	}
	activeItems := items[:0]
	reclaimedStale := false
	for _, item := range items {
		if item.Status == StatusCreating && m.now().UTC().Sub(item.CreatedAt) > 24*time.Hour {
			if err := os.Remove(m.metadataPath(item.ID)); err != nil {
				return Metadata{}, fmt.Errorf("remove stale recovery reservation: %w", err)
			}
			reclaimedStale = true
			continue
		}
		activeItems = append(activeItems, item)
	}
	items = activeItems
	if reclaimedStale {
		if err := syncDirectory(m.root); err != nil {
			return Metadata{}, err
		}
	}
	if len(items) >= m.maxPerInstance {
		if !reservation.Automated {
			return Metadata{}, ErrLimitReached
		}
		rotated := false
		for index := len(items) - 1; index >= 0; index-- {
			item := items[index]
			if !item.Automated || item.Status == StatusCreating || item.Status == StatusUploaded {
				continue
			}
			if err := m.deleteLocked(item); err != nil {
				return Metadata{}, fmt.Errorf("rotate oldest automatic recovery point: %w", err)
			}
			rotated = true
			break
		}
		if !rotated {
			return Metadata{}, ErrLimitReached
		}
	}
	for _, item := range items {
		if item.Status == StatusCreating || item.Status == StatusUploaded {
			return Metadata{}, ErrBusy
		}
	}
	id, suffix, err := newID()
	if err != nil {
		return Metadata{}, err
	}
	createdAt := m.now().UTC()
	metadata := Metadata{
		ID: id, InstanceID: reservation.InstanceID, InstanceName: reservation.InstanceName,
		HostID: reservation.HostID, OperationID: reservation.OperationID, JobID: reservation.JobID,
		Filename: "hermes-" + reservation.InstanceName + "-" + createdAt.Format("20060102T150405Z") + "-" + suffix + ".tar",
		Status:   StatusCreating, Image: reservation.Image, ImageID: reservation.ImageID,
		Provider: reservation.Provider, Model: reservation.Model, Reasoning: reservation.Reasoning,
		ServiceTier: reservation.ServiceTier, CodexConfigured: reservation.CodexConfigured,
		ProjectName: reservation.ProjectName, DataVolume: reservation.DataVolume,
		ManagedPath: reservation.ManagedPath, AgentVersion: reservation.AgentVersion,
		Automated: reservation.Automated, WorkflowID: reservation.WorkflowID, CreatedAt: createdAt,
	}
	if err := validateMetadata(metadata); err != nil {
		return Metadata{}, err
	}
	if err := m.writeMetadata(metadata); err != nil {
		return Metadata{}, err
	}
	if err := syncDirectory(m.root); err != nil {
		return Metadata{}, err
	}
	return metadata, nil
}

func (m *Manager) Abort(id, jobID string) error {
	unlockPoint := m.lockPoint(id)
	defer unlockPoint()
	m.mu.Lock()
	defer m.mu.Unlock()
	metadata, err := m.get(id)
	if err != nil {
		return err
	}
	if metadata.JobID != jobID || metadata.Status != StatusCreating {
		return ErrState
	}
	if err := os.Remove(m.metadataPath(id)); err != nil {
		return err
	}
	return syncDirectory(m.root)
}

// AbortTerminal removes an unfinished recovery point whose owning job has
// reached a terminal state. The caller must establish that the job is terminal;
// the host and job identities fence cleanup to the exact reservation.
func (m *Manager) AbortTerminal(id, hostID, jobID string) error {
	unlockPoint := m.lockPoint(id)
	defer unlockPoint()
	m.mu.Lock()
	defer m.mu.Unlock()
	metadata, err := m.get(id)
	if err != nil {
		return err
	}
	if metadata.HostID != hostID || metadata.JobID != jobID {
		return ErrState
	}
	if metadata.Status != StatusCreating && metadata.Status != StatusUploaded {
		return ErrState
	}
	return m.deleteLocked(metadata)
}

func (m *Manager) Upload(
	ctx context.Context,
	id, hostID, jobID, digest string,
	expectedSize int64,
	source io.Reader,
	commitFence func(context.Context) error,
) (Metadata, error) {
	unlockPoint := m.lockPoint(id)
	defer unlockPoint()
	if !sha256Pattern.MatchString(digest) || expectedSize < 1 || expectedSize > m.maxBytes {
		return Metadata{}, ErrIntegrity
	}
	m.mu.RLock()
	metadata, err := m.get(id)
	m.mu.RUnlock()
	if err != nil {
		return Metadata{}, err
	}
	if metadata.HostID != hostID || metadata.JobID != jobID {
		return Metadata{}, ErrState
	}
	if metadata.Status == StatusUploaded || metadata.Status == StatusReady {
		if metadata.SHA256 != digest || metadata.SizeBytes != expectedSize {
			return Metadata{}, ErrState
		}
		if err := consumeExpectedUpload(ctx, source, expectedSize, digest); err != nil {
			return Metadata{}, err
		}
		return metadata, nil
	}
	if metadata.Status != StatusCreating {
		return Metadata{}, ErrState
	}
	temporaryPath := filepath.Join(m.root, "."+id+".enc.uploading")
	temporary, err := os.OpenFile(temporaryPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Metadata{}, fmt.Errorf("create encrypted recovery point: %w", err)
	}
	defer os.Remove(temporaryPath)
	hash := sha256.New()
	limited := io.LimitReader(source, expectedSize+1)
	written, encryptErr := recoverycodec.Encrypt(ctx, temporary, io.TeeReader(limited, hash), m.key, id)
	if syncErr := temporary.Sync(); encryptErr == nil && syncErr != nil {
		encryptErr = syncErr
	}
	if closeErr := temporary.Close(); encryptErr == nil && closeErr != nil {
		encryptErr = closeErr
	}
	if encryptErr != nil {
		return Metadata{}, fmt.Errorf("encrypt recovery point: %w", encryptErr)
	}
	if written != expectedSize || hex.EncodeToString(hash.Sum(nil)) != digest {
		return Metadata{}, fmt.Errorf("%w: uploaded size or checksum does not match", ErrIntegrity)
	}
	info, err := os.Lstat(temporaryPath)
	if err != nil {
		return Metadata{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return Metadata{}, fmt.Errorf("%w: encrypted artifact has unsafe permissions", ErrIntegrity)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	metadata, err = m.get(id)
	if err != nil {
		return Metadata{}, err
	}
	if metadata.HostID != hostID || metadata.JobID != jobID || metadata.Status != StatusCreating {
		return Metadata{}, ErrState
	}
	if commitFence != nil {
		if err := commitFence(ctx); err != nil {
			return Metadata{}, fmt.Errorf("recovery point commit fence rejected publication: %w", err)
		}
	}
	if err := os.Rename(temporaryPath, m.artifactPath(id)); err != nil {
		return Metadata{}, fmt.Errorf("publish encrypted recovery point: %w", err)
	}
	metadata.Status = StatusUploaded
	metadata.SizeBytes = expectedSize
	metadata.EncryptedSizeBytes = info.Size()
	metadata.SHA256 = digest
	metadata.UploadedAt = m.now().UTC()
	metadata.Error = ""
	if err := m.writeMetadata(metadata); err != nil {
		_ = os.Remove(m.artifactPath(id))
		return Metadata{}, err
	}
	if err := syncDirectory(m.root); err != nil {
		return Metadata{}, err
	}
	return metadata, nil
}

func (m *Manager) VerifyUploaded(ctx context.Context, id, hostID, jobID, digest string, size int64) (Metadata, error) {
	unlockPoint := m.lockPoint(id)
	defer unlockPoint()
	m.mu.RLock()
	metadata, err := m.get(id)
	m.mu.RUnlock()
	if err != nil {
		return Metadata{}, err
	}
	if metadata.HostID != hostID || metadata.JobID != jobID || metadata.Status != StatusUploaded || metadata.SHA256 != digest || metadata.SizeBytes != size {
		return Metadata{}, ErrState
	}
	if err := m.verify(ctx, metadata); err != nil {
		return Metadata{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	metadata, err = m.get(id)
	if err != nil {
		return Metadata{}, err
	}
	if metadata.HostID != hostID || metadata.JobID != jobID || metadata.Status != StatusUploaded || metadata.SHA256 != digest || metadata.SizeBytes != size {
		return Metadata{}, ErrState
	}
	metadata.Status = StatusReady
	metadata.VerifiedAt = m.now().UTC()
	if err := m.writeMetadata(metadata); err != nil {
		return Metadata{}, err
	}
	return metadata, syncDirectory(m.root)
}

func (m *Manager) Verify(ctx context.Context, id string) (Metadata, error) {
	unlockPoint := m.lockPoint(id)
	defer unlockPoint()
	m.mu.RLock()
	metadata, err := m.get(id)
	m.mu.RUnlock()
	if err != nil {
		return Metadata{}, err
	}
	if metadata.Status != StatusReady {
		return Metadata{}, ErrState
	}
	if err := m.verify(ctx, metadata); err != nil {
		return Metadata{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	metadata, err = m.get(id)
	if err != nil {
		return Metadata{}, err
	}
	if metadata.Status != StatusReady {
		return Metadata{}, ErrState
	}
	metadata.VerifiedAt = m.now().UTC()
	if err := m.writeMetadata(metadata); err != nil {
		return Metadata{}, err
	}
	return metadata, syncDirectory(m.root)
}

func (m *Manager) Fail(id, hostID, jobID, message string) error {
	unlockPoint := m.lockPoint(id)
	defer unlockPoint()
	m.mu.Lock()
	defer m.mu.Unlock()
	metadata, err := m.get(id)
	if err != nil {
		return err
	}
	if metadata.HostID != hostID || metadata.JobID != jobID || (metadata.Status != StatusCreating && metadata.Status != StatusUploaded) {
		return ErrState
	}
	metadata.Status = StatusFailed
	metadata.SHA256 = ""
	metadata.SizeBytes = 0
	metadata.EncryptedSizeBytes = 0
	metadata.UploadedAt = time.Time{}
	metadata.VerifiedAt = time.Time{}
	metadata.Error = strings.TrimSpace(message)
	if len(metadata.Error) > 500 {
		metadata.Error = metadata.Error[:500]
	}
	if metadata.Error == "" {
		metadata.Error = "Host Agent could not create the recovery point"
	}
	if err := m.writeMetadata(metadata); err != nil {
		return err
	}
	if err := os.Remove(m.artifactPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove failed recovery artifact: %w", err)
	}
	return syncDirectory(m.root)
}

func (m *Manager) ResetForRetry(id, hostID, jobID string) error {
	unlockPoint := m.lockPoint(id)
	defer unlockPoint()
	m.mu.Lock()
	defer m.mu.Unlock()
	metadata, err := m.get(id)
	if err != nil {
		return err
	}
	if metadata.HostID != hostID || metadata.JobID != jobID {
		return ErrState
	}
	if metadata.Status != StatusCreating && metadata.Status != StatusUploaded && metadata.Status != StatusFailed {
		return ErrState
	}
	metadata.Status = StatusCreating
	metadata.SHA256 = ""
	metadata.SizeBytes = 0
	metadata.EncryptedSizeBytes = 0
	metadata.UploadedAt = time.Time{}
	metadata.VerifiedAt = time.Time{}
	metadata.Error = ""
	if err := m.writeMetadata(metadata); err != nil {
		return err
	}
	if err := os.Remove(m.artifactPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove recovery artifact before retry: %w", err)
	}
	return syncDirectory(m.root)
}

func (m *Manager) List(ctx context.Context, instanceID string) ([]Metadata, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.list(ctx, instanceID)
}

func (m *Manager) Stream(ctx context.Context, id string, destination io.Writer) (Metadata, error) {
	unlockPoint := m.lockPoint(id)
	defer unlockPoint()
	m.mu.RLock()
	metadata, err := m.get(id)
	if err != nil {
		m.mu.RUnlock()
		return Metadata{}, err
	}
	if metadata.Status != StatusReady {
		m.mu.RUnlock()
		return Metadata{}, ErrState
	}
	file, err := os.Open(m.artifactPath(id))
	m.mu.RUnlock()
	if err != nil {
		return Metadata{}, err
	}
	defer file.Close()
	hash := sha256.New()
	written, err := recoverycodec.Decrypt(ctx, io.MultiWriter(destination, hash), file, m.key, id)
	if err != nil {
		return Metadata{}, fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	if written != metadata.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != metadata.SHA256 {
		return Metadata{}, fmt.Errorf("%w: downloaded size or checksum does not match", ErrIntegrity)
	}
	return metadata, nil
}

func (m *Manager) Get(id string) (Metadata, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.get(id)
}

// OpenEncrypted returns the published encrypted artifact without exposing the
// recovery key or decrypting instance data. It is intended for off-host
// disaster-recovery exports that keep key material on a separate boundary.
func (m *Manager) OpenEncrypted(id string) (Metadata, *os.File, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	metadata, err := m.get(id)
	if err != nil {
		return Metadata{}, nil, err
	}
	if metadata.Status != StatusReady {
		return Metadata{}, nil, ErrState
	}
	file, err := os.Open(m.artifactPath(id))
	if err != nil {
		return Metadata{}, nil, fmt.Errorf("open encrypted recovery artifact: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return Metadata{}, nil, fmt.Errorf("inspect encrypted recovery artifact: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != metadata.EncryptedSizeBytes {
		file.Close()
		return Metadata{}, nil, fmt.Errorf("%w: encrypted recovery artifact has unsafe type, permissions, or size", ErrIntegrity)
	}
	return metadata, file, nil
}

func (m *Manager) Delete(ctx context.Context, id, confirmation string) error {
	unlockPoint := m.lockPoint(id)
	defer unlockPoint()
	m.mu.Lock()
	defer m.mu.Unlock()
	metadata, err := m.get(id)
	if err != nil {
		return err
	}
	if confirmation != metadata.Filename {
		return ErrConfirmation
	}
	if metadata.Status == StatusCreating || metadata.Status == StatusUploaded {
		return ErrState
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.deleteLocked(metadata)
}

func (m *Manager) deleteLocked(metadata Metadata) error {
	id := metadata.ID
	metadataPath := m.metadataPath(id)
	metadataTrash := filepath.Join(m.root, "."+id+".json.deleting")
	if err := os.Rename(metadataPath, metadataTrash); err != nil {
		return fmt.Errorf("stage recovery point metadata deletion: %w", err)
	}
	artifactTrash := filepath.Join(m.root, "."+id+".enc.deleting")
	hasArtifact := metadata.Status == StatusUploaded || metadata.Status == StatusReady
	if hasArtifact {
		if err := os.Rename(m.artifactPath(id), artifactTrash); err != nil {
			_ = os.Rename(metadataTrash, metadataPath)
			return fmt.Errorf("stage encrypted recovery point deletion: %w", err)
		}
	}
	if hasArtifact {
		if err := os.Remove(artifactTrash); err != nil {
			return fmt.Errorf("delete encrypted recovery point: %w", err)
		}
	}
	if err := os.Remove(metadataTrash); err != nil {
		return fmt.Errorf("delete recovery point metadata: %w", err)
	}
	return syncDirectory(m.root)
}

func (m *Manager) lockPoint(id string) func() {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(id))
	mutex := &m.pointLocks[hash.Sum32()%uint32(len(m.pointLocks))]
	mutex.Lock()
	return mutex.Unlock
}

func (m *Manager) verify(ctx context.Context, metadata Metadata) error {
	file, err := os.Open(m.artifactPath(metadata.ID))
	if err != nil {
		return fmt.Errorf("%w: open encrypted artifact: %v", ErrIntegrity, err)
	}
	defer file.Close()
	reader, wait := decryptedReader(ctx, file, m.key, metadata.ID)
	hash := sha256.New()
	counter := &countWriter{}
	stream := io.TeeReader(reader, io.MultiWriter(hash, counter))
	validationErr := validateArchive(stream, metadata, m.maxBytes)
	if validationErr == nil {
		trailing, drainErr := io.Copy(io.Discard, stream)
		if drainErr != nil {
			validationErr = drainErr
		} else if trailing != 0 {
			validationErr = errors.New("archive contains trailing plaintext")
		}
	}
	if validationErr != nil {
		_ = reader.CloseWithError(validationErr)
	}
	decryptErr := wait()
	if validationErr != nil {
		return fmt.Errorf("%w: %v", ErrIntegrity, validationErr)
	}
	if decryptErr != nil {
		return fmt.Errorf("%w: %v", ErrIntegrity, decryptErr)
	}
	if counter.total != metadata.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != metadata.SHA256 {
		return fmt.Errorf("%w: plaintext size or checksum does not match metadata", ErrIntegrity)
	}
	return nil
}

func decryptedReader(ctx context.Context, source io.Reader, key []byte, associatedData string) (*io.PipeReader, func() error) {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := recoverycodec.Decrypt(ctx, writer, source, key, associatedData)
		_ = writer.CloseWithError(err)
		done <- err
	}()
	return reader, func() error { return <-done }
}

func validateArchive(reader io.Reader, metadata Metadata, maxBytes int64) error {
	archive := tar.NewReader(io.LimitReader(reader, maxBytes+1))
	manifestFound, volumeFound := false, false
	entries := 0
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		entries++
		if entries > 100000 {
			return errors.New("archive contains too many entries")
		}
		name := header.Name
		if name == "" || path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "\\") {
			return errors.New("archive contains an unsafe path")
		}
		if header.Size < 0 || header.Size > maxBytes {
			return errors.New("archive entry exceeds the size limit")
		}
		switch {
		case name == "manifest.json":
			if manifestFound || header.Typeflag != tar.TypeReg || header.Size > 1<<20 {
				return errors.New("archive manifest is invalid")
			}
			manifestFound = true
			data, err := io.ReadAll(io.LimitReader(archive, (1<<20)+1))
			if err != nil || int64(len(data)) != header.Size {
				return errors.New("archive manifest could not be read")
			}
			var manifest Manifest
			if err := json.Unmarshal(data, &manifest); err != nil || !manifestMatches(manifest, metadata) {
				return errors.New("archive manifest does not match reserved metadata")
			}
		case name == "data-volume.tar":
			if volumeFound || header.Typeflag != tar.TypeReg {
				return errors.New("data volume archive is invalid")
			}
			volumeFound = true
			if _, err := io.Copy(io.Discard, archive); err != nil {
				return err
			}
		case name == "workspace" || strings.HasPrefix(name, "workspace/"):
			if header.Typeflag != tar.TypeDir && header.Typeflag != tar.TypeReg {
				return errors.New("workspace archive contains an unsupported file type")
			}
			if _, err := io.Copy(io.Discard, archive); err != nil {
				return err
			}
		default:
			return errors.New("archive contains an unexpected entry")
		}
	}
	if !manifestFound || !volumeFound {
		return errors.New("archive is missing required entries")
	}
	return nil
}

func manifestMatches(manifest Manifest, metadata Metadata) bool {
	return manifest.FormatVersion == 1 && manifest.RecoveryPointID == metadata.ID &&
		manifest.InstanceID == metadata.InstanceID && manifest.InstanceName == metadata.InstanceName &&
		manifest.Image == metadata.Image && manifest.ImageID == metadata.ImageID && manifest.Provider == metadata.Provider &&
		manifest.Model == metadata.Model && manifest.Reasoning == metadata.Reasoning && manifest.ServiceTier == metadata.ServiceTier &&
		manifest.CodexConfigured == metadata.CodexConfigured &&
		manifest.ProjectName == metadata.ProjectName && manifest.DataVolume == metadata.DataVolume &&
		manifest.ManagedPath == metadata.ManagedPath && manifest.AgentVersion == metadata.AgentVersion &&
		manifest.CreatedAt.Equal(metadata.CreatedAt)
}

func (m *Manager) list(ctx context.Context, instanceID string) ([]Metadata, error) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return nil, fmt.Errorf("list recovery points: %w", err)
	}
	names := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".enc") {
			return nil, fmt.Errorf("%w: unexpected recovery point artifact %s", ErrIntegrity, name)
		}
		names[name] = true
	}
	for name := range names {
		if strings.HasSuffix(name, ".enc") && !names[strings.TrimSuffix(name, ".enc")+".json"] {
			return nil, fmt.Errorf("%w: encrypted artifact has no metadata", ErrIntegrity)
		}
	}
	items := make([]Metadata, 0)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		metadata, err := m.get(id)
		if err != nil {
			return nil, err
		}
		if instanceID == "" || metadata.InstanceID == instanceID {
			items = append(items, metadata)
		}
	}
	sort.Slice(items, func(left, right int) bool { return items[left].CreatedAt.After(items[right].CreatedAt) })
	return items, nil
}

func (m *Manager) get(id string) (Metadata, error) {
	if !recoveryIDPattern.MatchString(id) {
		return Metadata{}, ErrNotFound
	}
	data, err := os.ReadFile(m.metadataPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return Metadata{}, ErrNotFound
	}
	if err != nil {
		return Metadata{}, err
	}
	var metadata Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return Metadata{}, fmt.Errorf("%w: invalid metadata", ErrIntegrity)
	}
	if err := validateMetadata(metadata); err != nil {
		return Metadata{}, err
	}
	metadataInfo, err := os.Lstat(m.metadataPath(id))
	if err != nil || !metadataInfo.Mode().IsRegular() || metadataInfo.Mode().Perm() != 0o600 {
		return Metadata{}, fmt.Errorf("%w: metadata has unsafe permissions", ErrIntegrity)
	}
	if metadata.Status == StatusUploaded || metadata.Status == StatusReady {
		artifactInfo, err := os.Lstat(m.artifactPath(id))
		if err != nil || !artifactInfo.Mode().IsRegular() || artifactInfo.Mode().Perm() != 0o600 || artifactInfo.Size() != metadata.EncryptedSizeBytes {
			return Metadata{}, fmt.Errorf("%w: encrypted artifact is missing or unsafe", ErrIntegrity)
		}
	} else if _, err := os.Lstat(m.artifactPath(id)); err == nil {
		return Metadata{}, fmt.Errorf("%w: incomplete recovery point has a published artifact", ErrIntegrity)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Metadata{}, fmt.Errorf("inspect recovery point artifact: %w", err)
	}
	return metadata, nil
}

func validateMetadata(metadata Metadata) error {
	runtimeErr := providers.ValidateRuntimeOrPending(metadata.Provider, metadata.Model, metadata.Reasoning, metadata.ServiceTier)
	if metadata.CodexConfigured {
		runtimeErr = providers.ValidateRuntime(metadata.Provider, metadata.Model, metadata.Reasoning, metadata.ServiceTier)
	}
	validStatus := metadata.Status == StatusCreating || metadata.Status == StatusUploaded || metadata.Status == StatusReady || metadata.Status == StatusFailed
	if !recoveryIDPattern.MatchString(metadata.ID) || !filenamePattern.MatchString(metadata.Filename) || !validStatus ||
		metadata.InstanceID == "" || metadata.InstanceName == "" || metadata.HostID == "" || metadata.OperationID == "" || metadata.JobID == "" ||
		metadata.Image == "" || metadata.ImageID == "" || metadata.ProjectName == "" || metadata.DataVolume == "" || metadata.ManagedPath == "" ||
		runtimeErr != nil ||
		metadata.AgentVersion == "" || metadata.CreatedAt.IsZero() ||
		!strings.HasPrefix(metadata.Filename, "hermes-"+metadata.InstanceName+"-"+metadata.CreatedAt.UTC().Format("20060102T150405Z")+"-") {
		return fmt.Errorf("%w: incomplete recovery point metadata", ErrIntegrity)
	}
	if metadata.Status == StatusUploaded || metadata.Status == StatusReady {
		if metadata.SizeBytes < 1 || metadata.EncryptedSizeBytes < 1 || !sha256Pattern.MatchString(metadata.SHA256) || metadata.UploadedAt.IsZero() {
			return fmt.Errorf("%w: incomplete uploaded recovery point metadata", ErrIntegrity)
		}
	}
	if metadata.Status == StatusReady && metadata.VerifiedAt.IsZero() {
		return fmt.Errorf("%w: ready recovery point was not verified", ErrIntegrity)
	}
	return nil
}

func (m *Manager) writeMetadata(metadata Metadata) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(m.root, ".recovery-metadata-*")
	if err != nil {
		return err
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
	return os.Rename(temporaryPath, m.metadataPath(metadata.ID))
}

func (m *Manager) metadataPath(id string) string { return filepath.Join(m.root, id+".json") }
func (m *Manager) artifactPath(id string) string { return filepath.Join(m.root, id+".enc") }

func newID() (string, string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", "", err
	}
	encoded := hex.EncodeToString(buffer)
	return "recovery-" + encoded, encoded[:8], nil
}

type countWriter struct{ total int64 }

func (writer *countWriter) Write(data []byte) (int, error) {
	writer.total += int64(len(data))
	return len(data), nil
}

func syncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

func cleanupTemporaryFiles(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("inspect recovery point temporary files: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".recovery-") && strings.HasSuffix(name, ".json.deleting") {
			id := strings.TrimSuffix(strings.TrimPrefix(name, "."), ".json.deleting")
			for _, candidate := range []string{id + ".enc", "." + id + ".enc.deleting", name} {
				if err := os.Remove(filepath.Join(root, candidate)); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("finish interrupted recovery point deletion: %w", err)
				}
			}
			continue
		}
		if strings.HasPrefix(name, ".recovery-") && strings.HasSuffix(name, ".enc.deleting") {
			id := strings.TrimSuffix(strings.TrimPrefix(name, "."), ".enc.deleting")
			if _, err := os.Stat(filepath.Join(root, id+".json")); err == nil {
				if err := os.Rename(filepath.Join(root, name), filepath.Join(root, id+".enc")); err != nil {
					return fmt.Errorf("restore interrupted recovery point deletion: %w", err)
				}
			} else if errors.Is(err, os.ErrNotExist) {
				if err := os.Remove(filepath.Join(root, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("finish interrupted recovery point deletion: %w", err)
				}
			} else {
				return err
			}
			continue
		}
		if !strings.HasPrefix(name, ".recovery-metadata-") && !strings.HasSuffix(name, ".enc.uploading") {
			continue
		}
		if entry.IsDir() {
			return fmt.Errorf("%w: temporary recovery path is a directory", ErrIntegrity)
		}
		if err := os.Remove(filepath.Join(root, name)); err != nil {
			return fmt.Errorf("remove stale recovery point temporary file: %w", err)
		}
	}
	return syncDirectory(root)
}

func consumeExpectedUpload(ctx context.Context, source io.Reader, expectedSize int64, digest string) error {
	hash := sha256.New()
	buffer := make([]byte, 64<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		read, err := source.Read(buffer)
		if read > 0 {
			total += int64(read)
			if total > expectedSize {
				return fmt.Errorf("%w: repeated upload exceeds the expected size", ErrIntegrity)
			}
			_, _ = hash.Write(buffer[:read])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if total != expectedSize || hex.EncodeToString(hash.Sum(nil)) != digest {
		return fmt.Errorf("%w: repeated upload size or checksum does not match", ErrIntegrity)
	}
	return nil
}
