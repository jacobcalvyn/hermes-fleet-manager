package domain

import (
	"strings"
	"testing"
	"time"
)

func TestValidateFleetSkillAddsOwnershipMarkerAndRevision(t *testing.T) {
	skill := FleetSkill{
		Name:        "browser-report",
		Description: "Create a browser report",
		Content:     "---\nname: browser-report\ndescription: Create a browser report\n---\n\n# Instructions\nUse Chromium.",
	}
	if err := ValidateFleetSkill(&skill); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(skill.Content, FleetSkillOwnershipMark) || len(skill.Revision) != 64 {
		t.Fatalf("validated skill=%+v", skill)
	}
	firstRevision := skill.Revision
	if err := ValidateFleetSkill(&skill); err != nil || skill.Revision != firstRevision || strings.Count(skill.Content, FleetSkillOwnershipMark) != 1 {
		t.Fatalf("validation is not idempotent: skill=%+v err=%v", skill, err)
	}
}

func TestValidateFleetSkillRequiresCompleteInstanceProvenance(t *testing.T) {
	skill := FleetSkill{
		Name: "browser-report", Description: "Create a browser report",
		Content:            "---\nname: browser-report\ndescription: Create a browser report\n---\nUse Chromium.",
		OriginType:         FleetSkillOriginInstance,
		SourceInstanceID:   "instance-1",
		SourceInstanceName: "nara",
		SourceProfile:      "default",
		SourceRevision:     strings.Repeat("a", 64),
		SourceProvenance:   "agent",
		SourceObservedAt:   time.Now().UTC(),
	}
	if err := ValidateFleetSkill(&skill); err != nil {
		t.Fatal(err)
	}
	if skill.OriginType != FleetSkillOriginInstance || skill.SourceInstanceID != "instance-1" || skill.SourceInstanceName != "nara" {
		t.Fatalf("skill=%+v", skill)
	}
	skill.SourceInstanceID = ""
	if err := ValidateFleetSkill(&skill); err == nil {
		t.Fatal("copied skill without a source instance was accepted")
	}
}

func TestValidateHermesSkillRemovePayload(t *testing.T) {
	payload := HermesSkillRemovePayload{
		HermesProfileInspectPayload: HermesProfileInspectPayload{
			InstanceID: "instance-1", Name: "nara", ProjectName: "fleet-nara",
			ManagedPath: "/managed/nara", DashboardPort: 19130,
		},
		SkillName: "browser-report", Profile: "default",
	}
	if err := ValidateHermesSkillRemovePayload(&payload); err != nil {
		t.Fatal(err)
	}
	payload.SkillName = "../browser-report"
	if err := ValidateHermesSkillRemovePayload(&payload); err == nil {
		t.Fatal("unsafe skill name was accepted")
	}
}

func TestValidateFleetSkillRejectsMismatchedFrontmatter(t *testing.T) {
	skill := FleetSkill{
		Name: "browser-report", Description: "Create a browser report",
		Content: "---\nname: another-skill\ndescription: Create a browser report\n---\nUse Chromium.",
	}
	if err := ValidateFleetSkill(&skill); err == nil {
		t.Fatal("mismatched frontmatter name was accepted")
	}
}

func TestFleetSkillFromContentBuildsManagedCatalogEntry(t *testing.T) {
	skill, err := FleetSkillFromContent("browser-report", "---\nname: browser-report\ndescription: Browser report\ncategory: browser\n---\nUse Chromium.")
	if err != nil {
		t.Fatal(err)
	}
	if skill.Description != "Browser report" || skill.Category != "browser" ||
		!strings.Contains(skill.Content, FleetSkillOwnershipMark) || len(skill.Revision) != 64 {
		t.Fatalf("skill=%+v", skill)
	}
}
