package mcpevents

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// Seal encrypts plaintext with AES-256-GCM under key. A fresh random nonce is
// prepended to the result. The subscription id and role are bound as
// associated data, so a sealed value cannot be moved to another row or role.
func Seal(key []byte, subscriptionID, role string, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, sealAAD(subscriptionID, role)), nil
}

// Open reverses Seal. Any tampering or binding mismatch returns an error that
// carries no key or plaintext detail.
func Open(key []byte, subscriptionID, role string, sealed []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	n := gcm.NonceSize()
	if len(sealed) < n+gcm.Overhead() {
		return nil, errors.New("sealed value too short")
	}
	plain, err := gcm.Open(nil, sealed[:n], sealed[n:], sealAAD(subscriptionID, role))
	if err != nil {
		return nil, errors.New("cannot open sealed value")
	}
	return plain, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != keySize {
		return nil, errors.New("MCP Events key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealAAD length-prefixes the id so (id, role) pairs cannot collide.
func sealAAD(subscriptionID, role string) []byte {
	return []byte(fmt.Sprintf("%d:%s|%s", len(subscriptionID), subscriptionID, role))
}
