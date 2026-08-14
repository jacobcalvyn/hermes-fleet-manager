package messaging

import (
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

var (
	telegramIdentityPattern = regexp.MustCompile(`^-?[0-9]{1,20}$`)
	whatsAppNumberPattern   = regexp.MustCompile(`^[1-9][0-9]{6,14}$`)
)

func NormalizeAndValidate(config domain.MessagingConfiguration) (domain.MessagingConfiguration, error) {
	config.Telegram.BotToken = strings.TrimSpace(config.Telegram.BotToken)
	config.Telegram.AllowedUsers = normalizeList(config.Telegram.AllowedUsers)
	config.Telegram.GroupAllowedUsers = normalizeList(config.Telegram.GroupAllowedUsers)
	config.Telegram.GroupAllowedChats = normalizeList(config.Telegram.GroupAllowedChats)
	config.Telegram.ProxyURL = strings.TrimSpace(config.Telegram.ProxyURL)
	if config.Telegram.Enabled {
		if config.Telegram.BotToken == "" {
			return config, errors.New("Telegram bot token is required when Telegram is enabled")
		}
		if len(config.Telegram.AllowedUsers) == 0 {
			return config, errors.New("at least one Telegram user must be allowed")
		}
	}
	for _, value := range append(append(slices.Clone(config.Telegram.AllowedUsers), config.Telegram.GroupAllowedUsers...), config.Telegram.GroupAllowedChats...) {
		if !telegramIdentityPattern.MatchString(value) {
			return config, errors.New("Telegram user and chat IDs must be numeric")
		}
	}
	if config.Telegram.ProxyURL != "" {
		proxy, err := url.Parse(config.Telegram.ProxyURL)
		if err != nil || proxy.Host == "" || (proxy.Scheme != "http" && proxy.Scheme != "https" && proxy.Scheme != "socks5") {
			return config, errors.New("Telegram proxy must be an http, https, or socks5 URL")
		}
		if proxy.User != nil {
			return config, errors.New("Telegram proxy credentials are not accepted in the URL")
		}
	}

	config.WhatsApp.Mode = strings.ToLower(strings.TrimSpace(config.WhatsApp.Mode))
	if config.WhatsApp.Mode == "" {
		config.WhatsApp.Mode = "bot"
	}
	config.WhatsApp.AllowedUsers = normalizeList(config.WhatsApp.AllowedUsers)
	config.WhatsApp.UnauthorizedDMBehavior = strings.ToLower(strings.TrimSpace(config.WhatsApp.UnauthorizedDMBehavior))
	if config.WhatsApp.UnauthorizedDMBehavior == "" {
		config.WhatsApp.UnauthorizedDMBehavior = "ignore"
	}
	config.WhatsApp.ReplyPrefix = strings.TrimSpace(config.WhatsApp.ReplyPrefix)
	if config.WhatsApp.Mode != "bot" && config.WhatsApp.Mode != "self-chat" {
		return config, errors.New("WhatsApp mode must be bot or self-chat")
	}
	if config.WhatsApp.UnauthorizedDMBehavior != "ignore" && config.WhatsApp.UnauthorizedDMBehavior != "pair" {
		return config, errors.New("WhatsApp unauthorized DM behavior must be ignore or pair")
	}
	if len(config.WhatsApp.ReplyPrefix) > 240 {
		return config, errors.New("WhatsApp reply prefix must be at most 240 characters")
	}
	if config.WhatsApp.Enabled && config.WhatsApp.Mode == "bot" && len(config.WhatsApp.AllowedUsers) == 0 {
		return config, errors.New("at least one WhatsApp number must be allowed in bot mode")
	}
	for _, value := range config.WhatsApp.AllowedUsers {
		if !whatsAppNumberPattern.MatchString(value) {
			return config, errors.New("WhatsApp numbers must use international format")
		}
	}
	return config, nil
}

func normalizeList(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	normalized := make([]string, 0, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	slices.Sort(normalized)
	return normalized
}
