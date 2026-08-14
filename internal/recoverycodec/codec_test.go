package recoverycodec

import (
	"bytes"
	"context"
	"crypto/rand"
	"testing"
)

func TestRoundTripAndTamperDetection(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	plaintext := bytes.Repeat([]byte("recovery-data-"), 100000)
	var encrypted bytes.Buffer
	written, err := Encrypt(context.Background(), &encrypted, bytes.NewReader(plaintext), key, "point-1")
	if err != nil {
		t.Fatal(err)
	}
	if written != int64(len(plaintext)) {
		t.Fatalf("written=%d want=%d", written, len(plaintext))
	}
	var decrypted bytes.Buffer
	read, err := Decrypt(context.Background(), &decrypted, bytes.NewReader(encrypted.Bytes()), key, "point-1")
	if err != nil {
		t.Fatal(err)
	}
	if read != int64(len(plaintext)) || !bytes.Equal(decrypted.Bytes(), plaintext) {
		t.Fatal("decrypted recovery data does not match")
	}

	tampered := append([]byte(nil), encrypted.Bytes()...)
	tampered[len(tampered)/2] ^= 0x01
	if _, err := Decrypt(context.Background(), &bytes.Buffer{}, bytes.NewReader(tampered), key, "point-1"); err == nil {
		t.Fatal("tampered recovery stream was accepted")
	}
	if _, err := Decrypt(context.Background(), &bytes.Buffer{}, bytes.NewReader(encrypted.Bytes()), key, "point-2"); err == nil {
		t.Fatal("recovery stream was accepted with different associated data")
	}
}

func TestEncryptUsesVersionTwoStreamKeyFormat(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	var encrypted bytes.Buffer
	if _, err := Encrypt(context.Background(), &encrypted, bytes.NewReader([]byte("recovery")), key, "point-1"); err != nil {
		t.Fatal(err)
	}
	if encrypted.Len() < 24 || string(encrypted.Bytes()[:8]) != "HFRP0002" {
		t.Fatalf("Encrypt() header=%q, want version two format", encrypted.Bytes()[:8])
	}
}

func TestDecryptPreservesVersionOneCompatibility(t *testing.T) {
	key := bytes.Repeat([]byte{0x24}, 32)
	plaintext := bytes.Repeat([]byte("legacy-recovery-data"), 1000)
	var legacy bytes.Buffer
	if _, err := encryptV1(
		context.Background(), &legacy, bytes.NewReader(plaintext), key, "legacy-point", bytes.Repeat([]byte{0x11}, 8),
	); err != nil {
		t.Fatal(err)
	}
	if string(legacy.Bytes()[:8]) != "HFRP0001" {
		t.Fatalf("legacy header=%q", legacy.Bytes()[:8])
	}
	var decrypted bytes.Buffer
	if _, err := Decrypt(context.Background(), &decrypted, bytes.NewReader(legacy.Bytes()), key, "legacy-point"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted.Bytes(), plaintext) {
		t.Fatal("version one recovery stream did not remain readable")
	}
}

func TestVersionTwoUsesIndependentStreamKeys(t *testing.T) {
	key := bytes.Repeat([]byte{0x66}, 32)
	plaintext := bytes.Repeat([]byte("same-recovery-data"), 1000)
	var first, second bytes.Buffer
	for _, destination := range []*bytes.Buffer{&first, &second} {
		if _, err := Encrypt(context.Background(), destination, bytes.NewReader(plaintext), key, "same-point"); err != nil {
			t.Fatal(err)
		}
	}
	if bytes.Equal(first.Bytes()[8:24], second.Bytes()[8:24]) {
		t.Fatal("version two recovery streams reused the same salt")
	}
	if bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("version two recovery streams reused key and nonce material")
	}
	tamperedSalt := append([]byte(nil), first.Bytes()...)
	tamperedSalt[8] ^= 0x01
	if _, err := Decrypt(context.Background(), &bytes.Buffer{}, bytes.NewReader(tamperedSalt), key, "same-point"); err == nil {
		t.Fatal("version two recovery stream accepted a modified salt")
	}
}
