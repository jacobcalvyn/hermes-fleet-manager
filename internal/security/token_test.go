package security

import "testing"

func TestTokenHashAndComparison(t *testing.T) {
	token, err := GenerateToken(32)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) < 32 {
		t.Fatalf("generated token is too short: %d", len(token))
	}
	if !Equal(HashToken(token), HashToken(token)) {
		t.Fatal("equal token hashes were rejected")
	}
	if Equal(HashToken(token), HashToken(token+"x")) {
		t.Fatal("different token hashes were accepted")
	}
}
