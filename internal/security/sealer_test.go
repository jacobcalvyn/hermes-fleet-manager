package security

import (
	"strings"
	"testing"
)

func TestSealerRoundTripAndContextBinding(t *testing.T) {
	sealer, err := NewSealer(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := sealer.Seal([]byte("dashboard-secret"), "operation-1")
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := sealer.Open(ciphertext, "operation-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "dashboard-secret" {
		t.Fatalf("unexpected plaintext: %q", plaintext)
	}
	if _, err := sealer.Open(ciphertext, "operation-2"); err == nil {
		t.Fatal("ciphertext opened under a different operation context")
	}
}

func TestFingerprintIsDeterministicAndContextBound(t *testing.T) {
	sealer, err := NewSealer(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	first := sealer.Fingerprint([]byte("small delta"), "chat-event:one:1")
	if first == "" || first != sealer.Fingerprint([]byte("small delta"), "chat-event:one:1") {
		t.Fatal("fingerprint is not deterministic")
	}
	if first == sealer.Fingerprint([]byte("small delta"), "chat-event:two:1") ||
		first == sealer.Fingerprint([]byte("other delta"), "chat-event:one:1") {
		t.Fatal("fingerprint is not bound to both context and content")
	}
}
