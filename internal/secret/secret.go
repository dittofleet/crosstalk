// Package secret turns the one key a person holds into the two things
// crosstalk needs: a token that lets a device into the hub, and a key that
// seals messages between devices. Both are derived one way, so the hub,
// which only ever sees the token, cannot open a message.
package secret

import (
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	// KeyPrefix marks the key a person keeps, TokenPrefix the token the hub
	// is given, so neither is pasted where the other belongs.
	KeyPrefix   = "ctk_"
	TokenPrefix = "cth_"

	keyBytes = 32
)

var b64 = base64.RawURLEncoding

// ErrSealed is returned for a message that was not sealed with this key, was
// sealed for another device, or was altered on the way.
var ErrSealed = errors.New("message was not sealed with this key")

// Keys is what a device derives from the key.
type Keys struct {
	// HubToken is sent to the hub on connecting.
	HubToken string
	aead     cipher.AEAD
}

// NewKey makes a fresh key.
func NewKey() (string, error) {
	raw := make([]byte, keyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return KeyPrefix + b64.EncodeToString(raw), nil
}

// Derive reads a key as NewKey printed it.
func Derive(key string) (*Keys, error) {
	key = strings.TrimSpace(key)
	if strings.HasPrefix(key, TokenPrefix) {
		return nil, errors.New("that is the hub token, not the key (the key starts with " + KeyPrefix + ")")
	}
	body, ok := strings.CutPrefix(key, KeyPrefix)
	raw, err := b64.DecodeString(body)
	if !ok || err != nil || len(raw) != keyBytes {
		return nil, errors.New("not a crosstalk key (it starts with " + KeyPrefix + ")")
	}
	token, err := hkdf.Key(sha256.New, raw, nil, "crosstalk hub token v1", keyBytes)
	if err != nil {
		return nil, err
	}
	seal, err := hkdf.Key(sha256.New, raw, nil, "crosstalk message key v1", keyBytes)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(seal)
	if err != nil {
		return nil, err
	}
	return &Keys{HubToken: TokenPrefix + b64.EncodeToString(token), aead: aead}, nil
}

// Seal encrypts a message from one device to another. The names are bound
// into it, so the hub cannot hand it to a third device or pass it off as
// coming from someone else: Open fails unless both match.
func (k *Keys) Seal(plaintext []byte, from, to string) (string, error) {
	nonce := make([]byte, k.aead.NonceSize(), k.aead.NonceSize()+len(plaintext)+k.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(k.aead.Seal(nonce, nonce, plaintext, route(from, to))), nil
}

// Open reverses Seal.
func (k *Keys) Open(box, from, to string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(box)
	n := k.aead.NonceSize()
	if err != nil || len(raw) < n {
		return nil, ErrSealed
	}
	plaintext, err := k.aead.Open(nil, raw[:n], raw[n:], route(from, to))
	if err != nil {
		return nil, ErrSealed
	}
	return plaintext, nil
}

// Device names cannot contain a newline, so this cannot be read two ways.
func route(from, to string) []byte {
	return fmt.Appendf(nil, "crosstalk v1\n%s\n%s", from, to)
}
