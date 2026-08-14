package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoteAccessConfigurationRoundTripAndDelete(t *testing.T) {
	dataStore, err := Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()

	ctx := context.Background()
	if _, err := dataStore.GetRemoteAccessConfig(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRemoteAccessConfig() error=%v, want ErrNotFound", err)
	}

	createdAt := time.Date(2026, 8, 4, 1, 2, 3, 0, time.UTC)
	record := RemoteAccessConfigRecord{Ciphertext: "encrypted-configuration", UpdatedAt: createdAt}
	if err := dataStore.PutRemoteAccessConfig(ctx, record); err != nil {
		t.Fatal(err)
	}
	stored, err := dataStore.GetRemoteAccessConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Ciphertext != record.Ciphertext || !stored.UpdatedAt.Equal(createdAt) {
		t.Fatalf("stored record=%+v, want %+v", stored, record)
	}

	updatedAt := createdAt.Add(time.Minute)
	updated := RemoteAccessConfigRecord{Ciphertext: "replacement-ciphertext", UpdatedAt: updatedAt}
	if err := dataStore.PutRemoteAccessConfig(ctx, updated); err != nil {
		t.Fatal(err)
	}
	stored, err = dataStore.GetRemoteAccessConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Ciphertext != updated.Ciphertext || !stored.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("updated record=%+v, want %+v", stored, updated)
	}

	if err := dataStore.DeleteRemoteAccessConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.GetRemoteAccessConfig(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRemoteAccessConfig() after delete error=%v, want ErrNotFound", err)
	}
}
