package domain

import "testing"

func TestValidateHermesToolsetMutationPayload(t *testing.T) {
	payload := HermesToolsetMutationPayload{
		HermesProfileInspectPayload: HermesProfileInspectPayload{
			InstanceID: "instance-1", Name: "nara", ProjectName: "fleet-nara",
			ManagedPath: "/managed/nara", DashboardPort: 19130,
		},
		ToolsetName: "browser_tools", Profile: "default", Enabled: true,
	}
	if err := ValidateHermesToolsetMutationPayload(&payload); err != nil {
		t.Fatal(err)
	}
	payload.ToolsetName = "../browser"
	if err := ValidateHermesToolsetMutationPayload(&payload); err == nil {
		t.Fatal("unsafe toolset name was accepted")
	}
}
