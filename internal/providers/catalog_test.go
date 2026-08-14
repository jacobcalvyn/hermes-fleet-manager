package providers

import "testing"

func TestValidateProfileAuthRules(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		baseURL  string
		apiKey   string
		wantErr  bool
	}{
		{name: "codex per instance oauth", provider: "openai-codex"},
		{name: "codex rejects global key", provider: "openai-codex", apiKey: "secret", wantErr: true},
		{name: "openrouter requires key", provider: "openrouter", wantErr: true},
		{name: "openrouter key", provider: "openrouter", apiKey: "secret"},
		{name: "lmstudio requires url", provider: "lmstudio", apiKey: "secret", wantErr: true},
		{name: "lmstudio no auth", provider: "lmstudio", baseURL: "http://host.docker.internal:1234"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateProfile("Local profile", test.provider, test.baseURL, "model-1", "medium", "normal", test.apiKey, false)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateProfile() error=%v wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func TestValidateImageReference(t *testing.T) {
	for _, test := range []struct {
		image   string
		wantErr bool
	}{
		{image: "local/hermes-fleet-runtime:0.18.2"},
		{image: "registry.example:5000/runtime@sha256:abcdef"},
		{image: "runtime:latest\n    volumes: [/tmp:/host]", wantErr: true},
		{image: "runtime latest", wantErr: true},
	} {
		if err := ValidateImageReference(test.image); (err != nil) != test.wantErr {
			t.Errorf("ValidateImageReference(%q) error=%v wantErr=%v", test.image, err, test.wantErr)
		}
	}
}

func TestValidateRuntimeOrPending(t *testing.T) {
	for _, test := range []struct {
		name        string
		provider    string
		model       string
		reasoning   string
		serviceTier string
		wantPending bool
		wantErr     bool
	}{
		{name: "Codex pending", provider: "openai-codex", wantPending: true},
		{name: "Codex configured", provider: "openai-codex", model: "gpt-5.6-sol", reasoning: "medium", serviceTier: "normal"},
		{name: "partial configuration", provider: "openai-codex", reasoning: "medium", wantErr: true},
		{name: "other provider cannot be pending", provider: "openrouter", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateRuntimeOrPending(test.provider, test.model, test.reasoning, test.serviceTier)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateRuntimeOrPending() error=%v wantErr=%v", err, test.wantErr)
			}
			if pending := IsRuntimePending(test.provider, test.model, test.reasoning, test.serviceTier); pending != test.wantPending {
				t.Fatalf("IsRuntimePending()=%v want=%v", pending, test.wantPending)
			}
		})
	}
}
