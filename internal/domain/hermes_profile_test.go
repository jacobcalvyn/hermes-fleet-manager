package domain

import (
	"testing"
)

func TestValidateHermesProfileNameMatchesHermesContract(t *testing.T) {
	for _, name := range []string{"research-worker", "research_worker", "1-research", "a"} {
		if err := ValidateHermesProfileName(name); err != nil {
			t.Fatalf("ValidateHermesProfileName(%q) error=%v", name, err)
		}
	}
	for _, name := range []string{"", "Research-Worker", "research/worker", "test", "root", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		if err := ValidateHermesProfileName(name); err == nil {
			t.Fatalf("ValidateHermesProfileName(%q) unexpectedly succeeded", name)
		}
	}
}

func TestValidateHermesProfileInventoryRejectsUnsafeOrDuplicateMetadata(t *testing.T) {
	inventory := &HermesProfileInventory{
		InstanceID: "instance-a",
		Profiles: []HermesProfile{
			{Name: "zeta", Provider: "openai-codex", Model: "gpt-5.6-sol"},
			{Name: "default_profile", Default: true},
		},
	}
	if err := ValidateHermesProfileInventory(inventory); err != nil {
		t.Fatal(err)
	}
	if inventory.Profiles[0].Name != "default_profile" || inventory.Profiles[1].Name != "zeta" {
		t.Fatalf("profiles=%+v, want deterministic name ordering", inventory.Profiles)
	}

	inventory.Profiles = append(inventory.Profiles, HermesProfile{Name: "zeta"})
	if err := ValidateHermesProfileInventory(inventory); err == nil {
		t.Fatal("duplicate profile inventory unexpectedly succeeded")
	}

	inventory.Profiles = []HermesProfile{{Name: "../secret"}}
	if err := ValidateHermesProfileInventory(inventory); err == nil {
		t.Fatal("unsafe profile inventory unexpectedly succeeded")
	}
}

func TestIsHermesProfileJobIncludesLifecycleMutations(t *testing.T) {
	for _, jobType := range []string{
		JobInspectHermesProfiles,
		JobCreateHermesProfile,
		JobRepairHermesProfiles,
		JobActivateHermesProfile,
		JobDeleteHermesProfile,
	} {
		if !IsHermesProfileJob(jobType) {
			t.Errorf("IsHermesProfileJob(%q) = false", jobType)
		}
	}
	if IsHermesProfileJob("instance.chat.send") {
		t.Fatal("chat job was classified as a Hermes profile job")
	}
}
