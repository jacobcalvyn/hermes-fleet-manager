package domain

import (
	"errors"
	"regexp"
)

const JobSetHermesToolset = "instance.toolsets.set"

var hermesToolsetNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

type HermesToolsetMutationPayload struct {
	HermesProfileInspectPayload
	ToolsetName string `json:"toolset_name"`
	Profile     string `json:"profile"`
	Enabled     bool   `json:"enabled"`
}

func ValidateHermesToolsetMutationPayload(payload *HermesToolsetMutationPayload) error {
	if payload == nil || payload.InstanceID == "" || payload.Name == "" || payload.ProjectName == "" ||
		payload.ManagedPath == "" || payload.DashboardPort < 1 || payload.DashboardPort > 65535 ||
		!hermesToolsetNamePattern.MatchString(payload.ToolsetName) || ValidateHermesProfileReference(payload.Profile) != nil {
		return errors.New("Hermes toolset mutation payload is invalid")
	}
	return nil
}
