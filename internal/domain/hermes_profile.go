package domain

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	JobInspectHermesProfiles = "instance.profiles.inspect"
	JobCreateHermesProfile   = "instance.profiles.create"
	JobRepairHermesProfiles  = "instance.profiles.repair"
	JobActivateHermesProfile = "instance.profiles.activate"
	JobDeleteHermesProfile   = "instance.profiles.delete"
)

var (
	hermesProfileNamePattern          = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	hermesInventoryProfileNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	hermesReservedProfileNames        = map[string]struct{}{
		"hermes": {}, "root": {}, "sudo": {}, "test": {}, "tmp": {},
	}
)

type HermesProfile struct {
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	Active         bool   `json:"active"`
	Default        bool   `json:"default"`
	GatewayRunning bool   `json:"gateway_running"`
}

type HermesProfileInventory struct {
	InstanceID string          `json:"instance_id"`
	Profiles   []HermesProfile `json:"profiles"`
	ObservedAt time.Time       `json:"observed_at,omitempty"`
}

type HermesProfileInspectPayload struct {
	InstanceID    string `json:"instance_id"`
	Name          string `json:"name"`
	ProjectName   string `json:"project_name"`
	ManagedPath   string `json:"managed_path"`
	DashboardPort int    `json:"dashboard_port"`
}

type HermesProfileCreatePayload struct {
	HermesProfileInspectPayload
	ProfileName string `json:"profile_name"`
	CloneFrom   string `json:"clone_from"`
	Description string `json:"description,omitempty"`
}

type HermesProfileMutationPayload struct {
	HermesProfileInspectPayload
	ProfileName string `json:"profile_name"`
}

func IsHermesProfileJob(jobType string) bool {
	switch jobType {
	case JobInspectHermesProfiles, JobCreateHermesProfile, JobRepairHermesProfiles,
		JobActivateHermesProfile, JobDeleteHermesProfile:
		return true
	default:
		return false
	}
}

func ValidateHermesProfileName(name string) error {
	if !hermesProfileNamePattern.MatchString(name) {
		return errors.New("profile name must use 1-64 lowercase letters, numbers, underscores, or hyphens")
	}
	if _, reserved := hermesReservedProfileNames[name]; reserved {
		return errors.New("profile name is reserved by Hermes")
	}
	return nil
}

func ValidateHermesProfileReference(name string) error {
	if !hermesInventoryProfileNamePattern.MatchString(name) {
		return errors.New("profile reference is invalid")
	}
	return nil
}

func ValidateHermesProfileInventory(inventory *HermesProfileInventory) error {
	if inventory == nil || strings.TrimSpace(inventory.InstanceID) == "" || len(inventory.Profiles) > 128 {
		return errors.New("Hermes profile inventory is invalid")
	}
	seen := make(map[string]struct{}, len(inventory.Profiles))
	for index := range inventory.Profiles {
		profile := &inventory.Profiles[index]
		profile.Name = strings.TrimSpace(profile.Name)
		profile.Description = strings.TrimSpace(profile.Description)
		profile.Provider = strings.TrimSpace(profile.Provider)
		profile.Model = strings.TrimSpace(profile.Model)
		if ValidateHermesProfileReference(profile.Name) != nil || len(profile.Description) > 1000 || len(profile.Provider) > 128 || len(profile.Model) > 128 {
			return errors.New("Hermes profile inventory contains invalid metadata")
		}
		if _, exists := seen[profile.Name]; exists {
			return errors.New("Hermes profile inventory contains duplicate profiles")
		}
		seen[profile.Name] = struct{}{}
	}
	sort.Slice(inventory.Profiles, func(i, j int) bool { return inventory.Profiles[i].Name < inventory.Profiles[j].Name })
	return nil
}
