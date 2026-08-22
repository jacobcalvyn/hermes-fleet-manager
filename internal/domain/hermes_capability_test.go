package domain

import "testing"

func TestValidateHermesCapabilityInventoryNormalizesDeterministicOrder(t *testing.T) {
	inventory := &HermesCapabilityInventory{
		InstanceID: "instance-1",
		Features:   map[string]bool{"skills_api": true},
		Skills: []HermesSkillCapability{
			{Name: "zeta", Description: " Zeta skill "},
			{Name: "alpha", Category: " custom "},
		},
		Toolsets: []HermesToolsetCapability{
			{Name: "browser", Tools: []string{"browser_click", "browser_exec"}},
		},
	}
	if err := ValidateHermesCapabilityInventory(inventory); err != nil {
		t.Fatal(err)
	}
	if inventory.Skills[0].Name != "alpha" || inventory.Skills[0].Category != "custom" ||
		inventory.Skills[1].Description != "Zeta skill" {
		t.Fatalf("inventory was not normalized: %+v", inventory.Skills)
	}
}

func TestValidateHermesCapabilityInventoryRejectsDuplicates(t *testing.T) {
	inventory := &HermesCapabilityInventory{
		InstanceID: "instance-1", Features: map[string]bool{},
		Skills: []HermesSkillCapability{{Name: "duplicate"}, {Name: "duplicate"}},
	}
	if err := ValidateHermesCapabilityInventory(inventory); err == nil {
		t.Fatal("duplicate skill inventory was accepted")
	}
}
