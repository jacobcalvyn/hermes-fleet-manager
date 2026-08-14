package main

import (
	"strings"
	"testing"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func TestCompareStableInstanceStates(t *testing.T) {
	before := []domain.Instance{
		{ID: "running", Name: "running", Status: domain.InstanceRunning},
		{ID: "stopped", Name: "stopped", Status: domain.InstanceStopped},
		{ID: "updating", Name: "updating", Status: domain.InstanceUpdating},
	}
	after := []domain.Instance{
		{ID: "running", Name: "running", Status: domain.InstanceRunning},
		{ID: "stopped", Name: "stopped", Status: domain.InstanceStopped},
	}
	if err := compareStableInstanceStates(before, after); err != nil {
		t.Fatal(err)
	}
	after[0].Status = domain.InstanceFailed
	if err := compareStableInstanceStates(before, after); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("compareStableInstanceStates() error = %v", err)
	}
}
