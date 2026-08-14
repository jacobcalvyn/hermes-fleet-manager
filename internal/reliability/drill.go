package reliability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/backup"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/recovery"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/store"
)

const (
	DrillNever      = "NEVER_RUN"
	DrillRunning    = "RUNNING"
	DrillPassed     = "PASSED"
	DrillIncomplete = "INCOMPLETE"
	DrillFailed     = "FAILED"
)

var ErrDrillRunning = errors.New("a recovery drill is already running")

type State struct {
	Status                    string    `json:"status"`
	StartedAt                 time.Time `json:"started_at,omitempty"`
	CompletedAt               time.Time `json:"completed_at,omitempty"`
	ControlPlaneBackupChecked bool      `json:"control_plane_backup_checked"`
	InstanceBackupsChecked    int       `json:"instance_backups_checked"`
	InstancesWithoutBackup    int       `json:"instances_without_backup"`
	Error                     string    `json:"error,omitempty"`
}

type Manager struct {
	statePath string
	backups   *backup.Manager
	recovery  *recovery.Manager
	instances func(context.Context) ([]domain.Instance, error)
	now       func() time.Time

	mu    sync.RWMutex
	state State
}

func New(root string, backups *backup.Manager, recoveryPoints *recovery.Manager, instances func(context.Context) ([]domain.Instance, error)) (*Manager, error) {
	if backups == nil || recoveryPoints == nil || instances == nil {
		return nil, errors.New("recovery drill dependencies are required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve recovery drill state directory: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create recovery drill state directory: %w", err)
	}
	rootInfo, err := os.Lstat(absolute)
	if err != nil {
		return nil, fmt.Errorf("inspect recovery drill state directory: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("recovery drill state root must be a directory")
	}
	if err := os.Chmod(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("secure recovery drill state directory: %w", err)
	}
	manager := &Manager{
		statePath: filepath.Join(absolute, "recovery-drill.json"), backups: backups,
		recovery: recoveryPoints, instances: instances, now: time.Now,
		state: State{Status: DrillNever},
	}
	if err := removeInterruptedStateWrite(manager.statePath + ".tmp"); err != nil {
		return nil, err
	}
	interrupted := false
	if data, readErr := readSecureState(manager.statePath); readErr == nil {
		var state State
		if err := json.Unmarshal(data, &state); err != nil || !validDrillStatus(state.Status) {
			return nil, errors.New("recovery drill state is invalid")
		}
		if state.Status == DrillRunning {
			state.Status = DrillFailed
			state.Error = "the previous recovery drill was interrupted"
			state.CompletedAt = manager.now().UTC()
			interrupted = true
		}
		manager.state = state
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return nil, fmt.Errorf("read recovery drill state: %w", readErr)
	}
	if interrupted {
		if err := manager.writeStateLocked(); err != nil {
			return nil, err
		}
	}
	return manager, nil
}

func removeInterruptedStateWrite(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect interrupted recovery drill state: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("interrupted recovery drill state has an unsafe type")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove interrupted recovery drill state: %w", err)
	}
	return nil
}

func readSecureState(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("recovery drill state has unsafe type or permissions")
	}
	return os.ReadFile(path)
}

func validDrillStatus(value string) bool {
	return value == DrillNever || value == DrillRunning || value == DrillPassed || value == DrillIncomplete || value == DrillFailed
}

func (m *Manager) Status() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

func (m *Manager) Start() (State, error) {
	m.mu.Lock()
	previous := m.state
	if m.state.Status == DrillRunning {
		state := m.state
		m.mu.Unlock()
		return state, ErrDrillRunning
	}
	m.state = State{Status: DrillRunning, StartedAt: m.now().UTC()}
	state := m.state
	err := m.writeStateLocked()
	if err != nil {
		m.state = previous
	}
	m.mu.Unlock()
	if err != nil {
		return State{}, err
	}
	go m.run()
	return state, nil
}

func (m *Manager) run() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	result := m.execute(ctx)
	result.StartedAt = m.Status().StartedAt
	result.CompletedAt = m.now().UTC()
	m.mu.Lock()
	m.state = result
	if err := m.writeStateLocked(); err != nil {
		m.state.Status = DrillFailed
		m.state.Error = "persist recovery drill result: " + err.Error()
	}
	m.mu.Unlock()
}

func (m *Manager) execute(ctx context.Context) State {
	result := State{Status: DrillPassed}
	items, err := m.backups.List(ctx)
	if err != nil {
		return failedState("list control-plane backups", err)
	}
	if len(items) == 0 {
		result.Status = DrillIncomplete
		result.Error = "no control-plane backup is available"
	} else if err := m.verifyControlPlaneBackup(ctx, items[0]); err != nil {
		return failedState("verify control-plane backup in scratch storage", err)
	} else {
		result.ControlPlaneBackupChecked = true
	}
	instances, err := m.instances(ctx)
	if err != nil {
		return failedState("list managed instances", err)
	}
	for _, instance := range instances {
		points, listErr := m.recovery.List(ctx, instance.ID)
		if listErr != nil {
			return failedState("list instance backups", listErr)
		}
		verified := false
		for _, point := range points {
			if point.Status != recovery.StatusReady {
				continue
			}
			if _, verifyErr := m.recovery.Verify(ctx, point.ID); verifyErr != nil {
				return failedState("verify encrypted instance backup for "+instance.Name, verifyErr)
			}
			result.InstanceBackupsChecked++
			verified = true
			break
		}
		if !verified {
			result.InstancesWithoutBackup++
			result.Status = DrillIncomplete
		}
	}
	if result.Status == DrillIncomplete && result.Error == "" {
		result.Error = "one or more managed instances do not have a verified backup"
	}
	return result
}

func failedState(operation string, err error) State {
	return State{Status: DrillFailed, Error: operation + ": " + err.Error()}
}

func (m *Manager) verifyControlPlaneBackup(ctx context.Context, metadata backup.Metadata) error {
	if _, err := m.backups.Verify(ctx, metadata.ID); err != nil {
		return err
	}
	_, source, err := m.backups.Open(metadata.ID)
	if err != nil {
		return err
	}
	defer source.Close()
	temporaryRoot, err := os.MkdirTemp(filepath.Dir(m.statePath), ".recovery-drill-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporaryRoot)
	scratchPath := filepath.Join(temporaryRoot, "fleet.db")
	destination, err := os.OpenFile(scratchPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(destination, source); err != nil {
		destination.Close()
		return err
	}
	if err := destination.Sync(); err != nil {
		destination.Close()
		return err
	}
	if err := destination.Close(); err != nil {
		return err
	}
	scratch, err := store.Open(scratchPath)
	if err != nil {
		return err
	}
	defer scratch.Close()
	return scratch.Ready(ctx)
}

func (m *Manager) writeStateLocked() error {
	data, err := json.Marshal(m.state)
	if err != nil {
		return err
	}
	temporary := m.statePath + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("write recovery drill state: %w", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		_ = os.Remove(temporary)
		return fmt.Errorf("write recovery drill state: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		_ = os.Remove(temporary)
		return fmt.Errorf("sync recovery drill state: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("close recovery drill state: %w", err)
	}
	if err := os.Rename(temporary, m.statePath); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("publish recovery drill state: %w", err)
	}
	directory, err := os.Open(filepath.Dir(m.statePath))
	if err != nil {
		return fmt.Errorf("open recovery drill state directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync recovery drill state directory: %w", err)
	}
	return nil
}
