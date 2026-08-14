package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJobInputSecretIsNeverSerialized(t *testing.T) {
	const secret = "123456789:telegram-secret"
	encoded, err := json.Marshal(Job{
		ID:          "job-1",
		Type:        "instance.messaging.configure",
		InputSecret: []byte(secret),
	})
	if err != nil {
		t.Fatalf("json.Marshal(Job) error=%v", err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "input_secret") {
		t.Fatalf("serialized job exposed InputSecret: %s", encoded)
	}
}
