package messaging

import (
	"testing"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

func TestNormalizeAndValidate(t *testing.T) {
	config, err := NormalizeAndValidate(domain.MessagingConfiguration{
		Telegram: domain.TelegramMessagingConfiguration{
			Enabled: true, BotToken: " token ", AllowedUsers: []string{"42", "42"},
			GroupAllowedChats: []string{"-100123"},
		},
		WhatsApp: domain.WhatsAppMessagingConfiguration{
			Enabled: true, Mode: "bot", AllowedUsers: []string{"628123456789"},
		},
	})
	if err != nil {
		t.Fatalf("NormalizeAndValidate() error=%v", err)
	}
	if config.Telegram.BotToken != "token" || len(config.Telegram.AllowedUsers) != 1 {
		t.Fatalf("unexpected normalized Telegram config: %+v", config.Telegram)
	}
	if config.WhatsApp.UnauthorizedDMBehavior != "ignore" {
		t.Fatalf("unexpected WhatsApp defaults: %+v", config.WhatsApp)
	}
}

func TestNormalizeAndValidateRejectsUnsafeValues(t *testing.T) {
	tests := []domain.MessagingConfiguration{
		{Telegram: domain.TelegramMessagingConfiguration{Enabled: true, BotToken: "token", AllowedUsers: []string{"all"}}},
		{Telegram: domain.TelegramMessagingConfiguration{ProxyURL: "http://user:pass@example.com"}},
		{WhatsApp: domain.WhatsAppMessagingConfiguration{Enabled: true, Mode: "bot", AllowedUsers: []string{"+628123456789"}}},
	}
	for _, config := range tests {
		if _, err := NormalizeAndValidate(config); err == nil {
			t.Fatalf("NormalizeAndValidate(%+v) accepted an unsafe value", config)
		}
	}
}
