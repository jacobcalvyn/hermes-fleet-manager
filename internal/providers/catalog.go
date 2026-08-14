package providers

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

const (
	AuthInstanceOAuth  = "instance-oauth"
	AuthAPIKey         = "api-key"
	AuthAPIKeyOptional = "api-key-optional"
)

var (
	profileNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{1,47}$`)
	modelPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+@-]{0,127}$`)
	imagePattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,254}$`)
)

type Entry struct {
	Slug            string `json:"slug"`
	Label           string `json:"label"`
	AuthType        string `json:"auth_type"`
	CredentialEnv   string `json:"credential_env,omitempty"`
	BaseURLEnv      string `json:"base_url_env,omitempty"`
	BaseURLRequired bool   `json:"base_url_required"`
}

var catalog = []Entry{
	{Slug: "openai-codex", Label: "OpenAI Codex", AuthType: AuthInstanceOAuth},
	{Slug: "openrouter", Label: "OpenRouter", AuthType: AuthAPIKey, CredentialEnv: "OPENROUTER_API_KEY", BaseURLEnv: "OPENROUTER_BASE_URL"},
	{Slug: "openai-api", Label: "OpenAI API", AuthType: AuthAPIKey, CredentialEnv: "OPENAI_API_KEY", BaseURLEnv: "OPENAI_BASE_URL"},
	{Slug: "lmstudio", Label: "LM Studio", AuthType: AuthAPIKeyOptional, CredentialEnv: "LM_API_KEY", BaseURLEnv: "LM_BASE_URL", BaseURLRequired: true},
	{Slug: "gemini", Label: "Google AI Studio", AuthType: AuthAPIKey, CredentialEnv: "GOOGLE_API_KEY", BaseURLEnv: "GEMINI_BASE_URL"},
	{Slug: "deepseek", Label: "DeepSeek", AuthType: AuthAPIKey, CredentialEnv: "DEEPSEEK_API_KEY", BaseURLEnv: "DEEPSEEK_BASE_URL"},
}

func Catalog() []Entry {
	items := make([]Entry, len(catalog))
	copy(items, catalog)
	return items
}

func Lookup(slug string) (Entry, bool) {
	for _, entry := range catalog {
		if entry.Slug == slug {
			return entry, true
		}
	}
	return Entry{}, false
}

func ManagedEnvironmentKeys() []string {
	keys := []string{"HERMES_INFERENCE_PROVIDER", "HERMES_INFERENCE_MODEL", "HERMES_REASONING_EFFORT", "HERMES_SERVICE_TIER"}
	seen := map[string]bool{}
	for _, key := range keys {
		seen[key] = true
	}
	for _, entry := range catalog {
		for _, key := range []string{entry.CredentialEnv, entry.BaseURLEnv} {
			if key != "" && !seen[key] {
				keys = append(keys, key)
				seen[key] = true
			}
		}
	}
	return keys
}

func ValidateProfile(name, provider, baseURL, model, reasoning, serviceTier, apiKey string, existingSecret bool) error {
	if !profileNamePattern.MatchString(name) {
		return errors.New("profile name must be 2-48 safe characters")
	}
	entry, ok := Lookup(provider)
	if !ok {
		return errors.New("provider is not supported by this Fleet version")
	}
	if !modelPattern.MatchString(model) {
		return errors.New("model contains unsupported characters or length")
	}
	if !validReasoning(reasoning) {
		return errors.New("reasoning must be none, minimal, low, medium, high, xhigh, or max")
	}
	if serviceTier != "normal" && serviceTier != "priority" {
		return errors.New("service_tier must be normal or priority")
	}
	if baseURL != "" {
		parsed, err := url.ParseRequestURI(baseURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return errors.New("base_url must be an absolute HTTP or HTTPS URL")
		}
	}
	if entry.BaseURLRequired && baseURL == "" {
		return errors.New("base_url is required for this provider")
	}
	if entry.AuthType == AuthAPIKey && apiKey == "" && !existingSecret {
		return errors.New("api_key is required for this provider")
	}
	if entry.AuthType == AuthInstanceOAuth && apiKey != "" {
		return errors.New("this provider uses per-instance OAuth and does not accept a global api_key")
	}
	if strings.ContainsAny(apiKey, "\r\n") {
		return errors.New("api_key contains an invalid line break")
	}
	return nil
}

func ValidateRuntime(provider, model, reasoning, serviceTier string) error {
	if _, ok := Lookup(provider); !ok {
		return errors.New("provider is not supported by this Fleet version")
	}
	if !modelPattern.MatchString(model) {
		return errors.New("model contains unsupported characters or length")
	}
	if !validReasoning(reasoning) {
		return errors.New("invalid reasoning effort")
	}
	if serviceTier != "normal" && serviceTier != "priority" {
		return errors.New("invalid service tier")
	}
	return nil
}

// ValidateRuntimeOrPending accepts an unconfigured Codex runtime during the
// authentication-first setup flow. Any partially configured runtime remains
// invalid so Fleet never provisions an ambiguous desired state.
func ValidateRuntimeOrPending(provider, model, reasoning, serviceTier string) error {
	if provider == "openai-codex" && model == "" && reasoning == "" && serviceTier == "" {
		return nil
	}
	return ValidateRuntime(provider, model, reasoning, serviceTier)
}

func IsRuntimePending(provider, model, reasoning, serviceTier string) bool {
	return provider == "openai-codex" && model == "" && reasoning == "" && serviceTier == ""
}

func ValidateImageReference(image string) error {
	if !imagePattern.MatchString(image) {
		return errors.New("image must be a single safe Docker image reference of at most 255 characters")
	}
	return nil
}

func validReasoning(value string) bool {
	switch value {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}
