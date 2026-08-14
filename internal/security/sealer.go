package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

type Sealer struct {
	aead           cipher.AEAD
	fingerprintKey [32]byte
}

func NewSealer(hexKey string) (*Sealer, error) {
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("decode encryption key: %w", err)
	}
	if len(key) != 32 {
		return nil, errors.New("encryption key must be exactly 32 bytes encoded as 64 hex characters")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	fingerprintKey := sha256.Sum256(append([]byte("hermes-fleet:fingerprint:"), key...))
	return &Sealer{aead: aead, fingerprintKey: fingerprintKey}, nil
}

func (s *Sealer) Seal(plaintext []byte, context string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := s.aead.Seal(nonce, nonce, plaintext, []byte(context))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Fingerprint returns a keyed, deterministic digest for idempotency checks.
// Unlike a plain hash, it does not expose small plaintext chunks to an offline
// dictionary attack when the database is copied without the encryption key.
func (s *Sealer) Fingerprint(plaintext []byte, context string) string {
	mac := hmac.New(sha256.New, s.fingerprintKey[:])
	_, _ = mac.Write([]byte(context))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(plaintext)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Sealer) Open(ciphertext, context string) ([]byte, error) {
	sealed, err := base64.RawURLEncoding.DecodeString(ciphertext)
	if err != nil {
		return nil, err
	}
	nonceSize := s.aead.NonceSize()
	if len(sealed) < nonceSize {
		return nil, errors.New("encrypted payload is truncated")
	}
	nonce, payload := sealed[:nonceSize], sealed[nonceSize:]
	return s.aead.Open(nil, nonce, payload, []byte(context))
}
