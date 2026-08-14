package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestFleetHealthHistoryRecordsTransitionsAndLastSuccess(t *testing.T) {
	dataStore, err := Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	ctx := context.Background()
	startedAt := time.Date(2026, 8, 6, 1, 2, 3, 0, time.UTC)
	for _, sample := range []struct {
		status string
		detail string
		at     time.Time
	}{
		{status: "healthy", detail: "queue is stable", at: startedAt},
		{status: "healthy", detail: "queue remains stable", at: startedAt.Add(time.Minute)},
		{status: "degraded", detail: "one lease expired", at: startedAt.Add(2 * time.Minute)},
	} {
		if err := dataStore.RecordFleetHealth(ctx, "host_queue", sample.status, sample.detail, sample.at); err != nil {
			t.Fatal(err)
		}
	}
	states, err := dataStore.ListFleetHealthStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].Status != "degraded" || states[0].LastSuccessAt == nil || !states[0].LastSuccessAt.Equal(startedAt.Add(time.Minute)) {
		t.Fatalf("states=%+v", states)
	}
	incidents, err := dataStore.ListFleetHealthIncidents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 2 || incidents[0].Status != "degraded" || incidents[1].Status != "healthy" {
		t.Fatalf("incidents=%+v", incidents)
	}
}
