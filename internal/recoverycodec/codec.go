package recoverycodec

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	chunkSize    = 1 << 20
	headerV1Size = 16
	headerV2Size = 24
)

var (
	magicV1 = [8]byte{'H', 'F', 'R', 'P', '0', '0', '0', '1'}
	magicV2 = [8]byte{'H', 'F', 'R', 'P', '0', '0', '0', '2'}
)

func Encrypt(ctx context.Context, destination io.Writer, source io.Reader, key []byte, associatedData string) (int64, error) {
	if len(key) != 32 {
		return 0, errors.New("recovery encryption key must contain 32 bytes")
	}
	header := make([]byte, headerV2Size)
	copy(header, magicV2[:])
	if _, err := rand.Read(header[8:]); err != nil {
		return 0, fmt.Errorf("generate recovery stream salt: %w", err)
	}
	streamKey := deriveStreamKey(key, header[8:], associatedData)
	defer clear(streamKey)
	aead, err := newAEAD(streamKey)
	if err != nil {
		return 0, err
	}
	if _, err := destination.Write(header); err != nil {
		return 0, fmt.Errorf("write recovery header: %w", err)
	}
	return encryptChunks(ctx, destination, source, aead, nil, func(counter, length uint32) []byte {
		return makeAADV2(header, associatedData, counter, length)
	})
}

func encryptV1(ctx context.Context, destination io.Writer, source io.Reader, key []byte, associatedData string, prefix []byte) (int64, error) {
	if len(prefix) != 8 {
		return 0, errors.New("legacy recovery nonce prefix must contain 8 bytes")
	}
	aead, err := newAEAD(key)
	if err != nil {
		return 0, err
	}
	header := make([]byte, headerV1Size)
	copy(header, magicV1[:])
	copy(header[8:], prefix)
	if _, err := destination.Write(header); err != nil {
		return 0, fmt.Errorf("write recovery header: %w", err)
	}
	return encryptChunks(ctx, destination, source, aead, prefix, func(counter, length uint32) []byte {
		return makeAAD(associatedData, counter, length)
	})
}

func encryptChunks(
	ctx context.Context,
	destination io.Writer,
	source io.Reader,
	aead cipher.AEAD,
	noncePrefix []byte,
	aad func(counter, length uint32) []byte,
) (int64, error) {
	buffer := make([]byte, chunkSize)
	var total int64
	for counter := uint32(0); ; counter++ {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		read, readErr := io.ReadFull(source, buffer)
		if errors.Is(readErr, io.ErrUnexpectedEOF) || errors.Is(readErr, io.EOF) {
			readErr = nil
		}
		if readErr != nil {
			return total, fmt.Errorf("read recovery stream: %w", readErr)
		}
		if read > 0 {
			if counter == ^uint32(0) {
				return total, errors.New("recovery stream exceeds the supported chunk count")
			}
			nonce := makeNonce(noncePrefix, counter)
			sealed := aead.Seal(nil, nonce, buffer[:read], aad(counter, uint32(read)))
			var length [4]byte
			binary.BigEndian.PutUint32(length[:], uint32(read))
			if _, err := destination.Write(length[:]); err != nil {
				return total, fmt.Errorf("write recovery chunk length: %w", err)
			}
			if _, err := destination.Write(sealed); err != nil {
				return total, fmt.Errorf("write recovery chunk: %w", err)
			}
			total += int64(read)
		}
		if read < len(buffer) {
			var terminator [4]byte
			if _, err := destination.Write(terminator[:]); err != nil {
				return total, fmt.Errorf("write recovery terminator: %w", err)
			}
			return total, nil
		}
	}
}

func Decrypt(ctx context.Context, destination io.Writer, source io.Reader, key []byte, associatedData string) (int64, error) {
	var encodedMagic [8]byte
	if _, err := io.ReadFull(source, encodedMagic[:]); err != nil {
		return 0, fmt.Errorf("read recovery header: %w", err)
	}
	var (
		aead        cipher.AEAD
		noncePrefix []byte
		aad         func(counter, length uint32) []byte
		derivedKey  []byte
	)
	switch encodedMagic {
	case magicV1:
		prefix := make([]byte, 8)
		if _, err := io.ReadFull(source, prefix); err != nil {
			return 0, fmt.Errorf("read recovery v1 header: %w", err)
		}
		var err error
		aead, err = newAEAD(key)
		if err != nil {
			return 0, err
		}
		noncePrefix = prefix
		aad = func(counter, length uint32) []byte { return makeAAD(associatedData, counter, length) }
	case magicV2:
		header := make([]byte, headerV2Size)
		copy(header, encodedMagic[:])
		if _, err := io.ReadFull(source, header[8:]); err != nil {
			return 0, fmt.Errorf("read recovery v2 header: %w", err)
		}
		if len(key) != 32 {
			return 0, errors.New("recovery encryption key must contain 32 bytes")
		}
		derivedKey = deriveStreamKey(key, header[8:], associatedData)
		defer clear(derivedKey)
		var err error
		aead, err = newAEAD(derivedKey)
		if err != nil {
			return 0, err
		}
		aad = func(counter, length uint32) []byte { return makeAADV2(header, associatedData, counter, length) }
	default:
		return 0, errors.New("invalid recovery stream header")
	}
	var total int64
	for counter := uint32(0); ; counter++ {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		var encodedLength [4]byte
		if _, err := io.ReadFull(source, encodedLength[:]); err != nil {
			return total, fmt.Errorf("read recovery chunk length: %w", err)
		}
		plainLength := binary.BigEndian.Uint32(encodedLength[:])
		if plainLength == 0 {
			var trailing [1]byte
			if read, err := source.Read(trailing[:]); read != 0 || (err != nil && !errors.Is(err, io.EOF)) {
				return total, errors.New("recovery stream contains trailing data")
			}
			return total, nil
		}
		if plainLength > chunkSize {
			return total, errors.New("invalid recovery chunk length")
		}
		sealed := make([]byte, int(plainLength)+aead.Overhead())
		if _, err := io.ReadFull(source, sealed); err != nil {
			return total, fmt.Errorf("read recovery chunk: %w", err)
		}
		plaintext, err := aead.Open(nil, makeNonce(noncePrefix, counter), sealed, aad(counter, plainLength))
		if err != nil {
			return total, errors.New("recovery stream authentication failed")
		}
		if _, err := destination.Write(plaintext); err != nil {
			return total, fmt.Errorf("write recovery plaintext: %w", err)
		}
		total += int64(len(plaintext))
	}
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("recovery encryption key must contain 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func makeNonce(prefix []byte, counter uint32) []byte {
	nonce := make([]byte, 12)
	copy(nonce, prefix)
	binary.BigEndian.PutUint32(nonce[8:], counter)
	return nonce
}

func makeAAD(context string, counter, length uint32) []byte {
	buffer := make([]byte, len(context)+8)
	copy(buffer, context)
	binary.BigEndian.PutUint32(buffer[len(context):], counter)
	binary.BigEndian.PutUint32(buffer[len(context)+4:], length)
	return buffer
}

func makeAADV2(header []byte, context string, counter, length uint32) []byte {
	buffer := make([]byte, len(header)+len(context)+8)
	copy(buffer, header)
	copy(buffer[len(header):], context)
	binary.BigEndian.PutUint32(buffer[len(header)+len(context):], counter)
	binary.BigEndian.PutUint32(buffer[len(header)+len(context)+4:], length)
	return buffer
}

func deriveStreamKey(masterKey, salt []byte, associatedData string) []byte {
	extract := hmac.New(sha256.New, salt)
	_, _ = extract.Write(masterKey)
	prk := extract.Sum(nil)
	defer clear(prk)
	expand := hmac.New(sha256.New, prk)
	_, _ = expand.Write([]byte("hermes-fleet-recovery-v2\x00"))
	_, _ = expand.Write([]byte(associatedData))
	_, _ = expand.Write([]byte{1})
	return expand.Sum(nil)
}
