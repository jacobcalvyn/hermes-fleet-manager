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
	DeviceURL       string `json:"device_url,omitempty"`
	AuthNote        string `json:"auth_note,omitempty"`
}

var catalog = []Entry{
	{Slug: "openai-codex", Label: "OpenAI Codex", AuthType: AuthInstanceOAuth, DeviceURL: "https://auth.openai.com/codex/device"},
	{
		Slug: "xai-oauth", Label: "xAI Grok", AuthType: AuthInstanceOAuth,
		AuthNote: "Grok OAuth requires SuperGrok or X Premium+. Some accounts receive HTTP 403 until xAI enables that subscription tier.",
	},
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

func IsInstanceOAuth(slug string) bool {
	entry, ok := Lookup(slug)
	return ok && entry.AuthType == AuthInstanceOAuth
}

func InstanceOAuthSlugs() []string {
	slugs := make([]string, 0, 2)
	for _, entry := range catalog {
		if entry.AuthType == AuthInstanceOAuth {
			slugs = append(slugs, entry.Slug)
		}
	}
	return slugs
}

func DeviceURL(slug string) (string, bool) {
	entry, ok := Lookup(slug)
	if !ok || entry.DeviceURL == "" {
		return "", false
	}
	return entry.DeviceURL, true
}

func AllowedDeviceURL(uri string) bool {
	for _, entry := range catalog {
		if entry.DeviceURL != "" && entry.DeviceURL == uri {
			return true
		}
	}
	parsed, err := url.ParseRequestURI(uri)
	return err == nil && parsed.Scheme == "https" && parsed.Host == "auth.x.ai" && parsed.User == nil && parsed.Path != ""
}

func ObservationAuthCheckName(slug string) string {
	if slug == "openai-codex" {
		return "codex_auth"
	}
	return "provider_auth"
}

func AuthStatusLoggedIn(slug string) string {
	return slug + ": logged in"
}

func AuthStatusLoggedOut(slug string) string {
	return slug + ": logged out"
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

// ValidateRuntimeCapabilities rejects provider/model controls that Hermes does
// not transmit to the upstream provider. This keeps Fleet's saved desired
// state aligned with the effective request instead of presenting ignored
// settings as applied.
func ValidateRuntimeCapabilities(provider, model, reasoning, serviceTier string) error {
	if provider != "xai-oauth" {
		return nil
	}
	if serviceTier == "priority" && !isGrok46Family(model) {
		return errors.New("priority service tier requires a Grok 4.6 model")
	}
	if reasoning == "none" {
		return nil
	}
	if !grokSupportsReasoningEffort(model) {
		return errors.New("this Grok model manages reasoning automatically; use reasoning none")
	}
	if reasoning != "low" && reasoning != "medium" && reasoning != "high" && !(reasoning == "xhigh" && isGrok46Family(model)) {
		return errors.New("this Grok model does not support the selected reasoning effort")
	}
	return nil
}

func grokModelName(model string) string {
	name := strings.ToLower(strings.TrimSpace(model))
	if index := strings.LastIndex(name, "/"); index >= 0 {
		name = name[index+1:]
	}
	return strings.ReplaceAll(name, "_", "-")
}

func isGrok46Family(model string) bool {
	name := grokModelName(model)
	return name == "grok-4.6" || strings.HasPrefix(name, "grok-4.6-")
}

func grokSupportsReasoningEffort(model string) bool {
	name := grokModelName(model)
	for _, prefix := range []string{"grok-3-mini", "grok-4.20-multi-agent", "grok-4.3", "grok-4.5", "grok-4.6"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// ValidateRuntimeOrPending accepts an unconfigured instance-OAuth runtime
// during the authentication-first setup flow. Any partially configured
// runtime remains invalid so Fleet never provisions an ambiguous desired state.
func ValidateRuntimeOrPending(provider, model, reasoning, serviceTier string) error {
	if IsRuntimePending(provider, model, reasoning, serviceTier) {
		return nil
	}
	return ValidateRuntime(provider, model, reasoning, serviceTier)
}

func IsRuntimePending(provider, model, reasoning, serviceTier string) bool {
	return IsInstanceOAuth(provider) && model == "" && reasoning == "" && serviceTier == ""
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
