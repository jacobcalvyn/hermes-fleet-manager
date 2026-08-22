package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func TestFleetSkillCatalogCRUD(t *testing.T) {
	dataStore, err := Open(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	skill := domain.FleetSkill{
		Name: "browser-report", Description: "Create a browser report", Category: "browser",
		Content:            "---\nname: browser-report\ndescription: Create a browser report\n---\nUse Chromium.",
		OriginType:         domain.FleetSkillOriginInstance,
		SourceInstanceID:   "instance-1",
		SourceInstanceName: "nara",
		SourceProfile:      "default",
		SourceRevision:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SourceProvenance:   "agent",
		SourceObservedAt:   now.Add(-time.Minute),
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if err := dataStore.CreateFleetSkill(ctx, skill); err != nil {
		t.Fatal(err)
	}
	stored, err := dataStore.FleetSkill(ctx, skill.Name)
	if err != nil || stored.Revision == "" || stored.Content == skill.Content ||
		stored.OriginType != domain.FleetSkillOriginInstance || stored.SourceInstanceID != "instance-1" ||
		stored.SourceInstanceName != "nara" ||
		stored.SourceProfile != "default" || stored.SourceProvenance != "agent" {
		t.Fatalf("FleetSkill()=%+v err=%v", stored, err)
	}
	stored.Description = "Updated browser report"
	stored.UpdatedAt = now.Add(time.Second)
	if err := dataStore.UpdateFleetSkill(ctx, stored); err != nil {
		t.Fatal(err)
	}
	items, err := dataStore.ListFleetSkills(ctx)
	if err != nil || len(items) != 1 || items[0].Description != stored.Description {
		t.Fatalf("ListFleetSkills()=%+v err=%v", items, err)
	}
	if err := dataStore.DeleteFleetSkill(ctx, skill.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.FleetSkill(ctx, skill.Name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FleetSkill() after delete err=%v", err)
	}
}
