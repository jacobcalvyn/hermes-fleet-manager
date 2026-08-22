package domain

import (
	"errors"
	"sort"
	"strings"
	"time"
)

const JobInspectHermesCapabilities = "instance.capabilities.inspect"

type HermesSkillCapability struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
}

type HermesToolsetCapability struct {
	Name        string   `json:"name"`
	Label       string   `json:"label,omitempty"`
	Description string   `json:"description,omitempty"`
	Enabled     bool     `json:"enabled"`
	Configured  bool     `json:"configured"`
	Tools       []string `json:"tools"`
}

type HermesBrowserCapability struct {
	Available      bool   `json:"available"`
	Implementation string `json:"implementation,omitempty"`
}

type HermesCapabilityInventory struct {
	InstanceID    string                    `json:"instance_id"`
	Platform      string                    `json:"platform,omitempty"`
	Model         string                    `json:"model,omitempty"`
	RuntimeMode   string                    `json:"runtime_mode,omitempty"`
	ToolExecution string                    `json:"tool_execution,omitempty"`
	SplitRuntime  bool                      `json:"split_runtime"`
	Features      map[string]bool           `json:"features"`
	Skills        []HermesSkillCapability   `json:"skills"`
	Toolsets      []HermesToolsetCapability `json:"toolsets"`
	Browser       HermesBrowserCapability   `json:"browser"`
	ObservedAt    time.Time                 `json:"observed_at,omitempty"`
}

type HermesCapabilityInspectPayload struct {
	InstanceID  string `json:"instance_id"`
	Name        string `json:"name"`
	ProjectName string `json:"project_name"`
	ManagedPath string `json:"managed_path"`
	APIPort     int    `json:"api_port"`
}

func ValidateHermesCapabilityInventory(inventory *HermesCapabilityInventory) error {
	if inventory == nil || strings.TrimSpace(inventory.InstanceID) == "" ||
		len(inventory.Platform) > 128 || len(inventory.Model) > 256 ||
		len(inventory.RuntimeMode) > 128 || len(inventory.ToolExecution) > 128 ||
		len(inventory.Features) > 128 || len(inventory.Skills) > 1024 || len(inventory.Toolsets) > 256 ||
		len(inventory.Browser.Implementation) > 128 {
		return errors.New("Hermes capability inventory is invalid")
	}
	for name := range inventory.Features {
		if strings.TrimSpace(name) == "" || len(name) > 128 {
			return errors.New("Hermes capability inventory contains an invalid feature")
		}
	}
	seenSkills := make(map[string]struct{}, len(inventory.Skills))
	for index := range inventory.Skills {
		skill := &inventory.Skills[index]
		skill.Name = strings.TrimSpace(skill.Name)
		skill.Description = strings.TrimSpace(skill.Description)
		skill.Category = strings.TrimSpace(skill.Category)
		if skill.Name == "" || len(skill.Name) > 128 || len(skill.Description) > 4096 || len(skill.Category) > 128 {
			return errors.New("Hermes capability inventory contains invalid skill metadata")
		}
		if _, exists := seenSkills[skill.Name]; exists {
			return errors.New("Hermes capability inventory contains duplicate skills")
		}
		seenSkills[skill.Name] = struct{}{}
	}
	seenToolsets := make(map[string]struct{}, len(inventory.Toolsets))
	for index := range inventory.Toolsets {
		toolset := &inventory.Toolsets[index]
		toolset.Name = strings.TrimSpace(toolset.Name)
		toolset.Label = strings.TrimSpace(toolset.Label)
		toolset.Description = strings.TrimSpace(toolset.Description)
		if toolset.Name == "" || len(toolset.Name) > 128 || len(toolset.Label) > 256 ||
			len(toolset.Description) > 4096 || len(toolset.Tools) > 512 {
			return errors.New("Hermes capability inventory contains invalid toolset metadata")
		}
		if _, exists := seenToolsets[toolset.Name]; exists {
			return errors.New("Hermes capability inventory contains duplicate toolsets")
		}
		seenToolsets[toolset.Name] = struct{}{}
		seenTools := make(map[string]struct{}, len(toolset.Tools))
		for toolIndex, tool := range toolset.Tools {
			tool = strings.TrimSpace(tool)
			if tool == "" || len(tool) > 128 {
				return errors.New("Hermes capability inventory contains an invalid tool")
			}
			if _, exists := seenTools[tool]; exists {
				return errors.New("Hermes capability inventory contains duplicate tools")
			}
			seenTools[tool] = struct{}{}
			toolset.Tools[toolIndex] = tool
		}
		sort.Strings(toolset.Tools)
	}
	sort.Slice(inventory.Skills, func(i, j int) bool { return inventory.Skills[i].Name < inventory.Skills[j].Name })
	sort.Slice(inventory.Toolsets, func(i, j int) bool { return inventory.Toolsets[i].Name < inventory.Toolsets[j].Name })
	return nil
}
