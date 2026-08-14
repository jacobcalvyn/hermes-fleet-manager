package reliability

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/backup"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/recovery"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/store"
)

func TestRecoveryDrillUsesScratchDatabaseAndReportsMissingInstanceBackups(t *testing.T) {
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
	manager, err := New(filepath.Join(root, "reliability"), backups, recoveryPoints, func(context.Context) ([]domain.Instance, error) {
		return []domain.Instance{{ID: "00000000-0000-4000-8000-000000000001", Name: "fleet-test-01"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC) }
	state := manager.execute(context.Background())
	if state.Status != DrillIncomplete || !state.ControlPlaneBackupChecked || state.InstancesWithoutBackup != 1 {
		t.Fatalf("unexpected drill state: %+v", state)
	}
}

func TestNewRejectsCorruptPersistedState(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "recovery-drill.json"), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	if _, err := New(root, backups, recoveryPoints, func(context.Context) ([]domain.Instance, error) { return nil, nil }); err == nil {
		t.Fatal("New() accepted a corrupt persisted recovery drill state")
	}
}

func TestNewPersistsInterruptedDrillAsFailed(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "recovery-drill.json")
	data, err := json.Marshal(State{Status: DrillRunning, StartedAt: time.Date(2026, 8, 5, 1, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
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
	manager, err := New(root, backups, recoveryPoints, func(context.Context) ([]domain.Instance, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	if manager.Status().Status != DrillFailed {
		t.Fatalf("interrupted drill status = %s", manager.Status().Status)
	}
	persisted, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err := json.Unmarshal(persisted, &state); err != nil {
		t.Fatal(err)
	}
	if state.Status != DrillFailed || state.CompletedAt.IsZero() {
		t.Fatalf("interrupted drill was not persisted as failed: %+v", state)
	}
}
