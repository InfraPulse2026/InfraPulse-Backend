package datasources

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
)

// Vault encrypts credentials at rest with a deployment-owned 256-bit key.
// Source ID is authenticated data: copying ciphertext to another source fails.
type Vault struct{ aead cipher.AEAD }

func NewVault(encoded string) (*Vault, error) {
	if encoded == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return nil, errors.New("DATA_SOURCE_ENCRYPTION_KEY must be base64 of exactly 32 random bytes")
	}
	block, err := aes.NewCipher(key)
	clear(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Vault{aead: aead}, nil
}

func (v *Vault) Seal(sourceID string, secret string) ([]byte, error) {
	if v == nil {
		return nil, ErrKeyUnavailable
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return v.aead.Seal(nonce, nonce, []byte(secret), []byte("infrapulse:data-source:v1:"+sourceID)), nil
}

func (v *Vault) Open(sourceID string, ciphertext []byte) ([]byte, error) {
	if v == nil {
		return nil, ErrKeyUnavailable
	}
	if len(ciphertext) < v.aead.NonceSize()+v.aead.Overhead() {
		return nil, errors.New("invalid encrypted credential")
	}
	return v.aead.Open(nil, ciphertext[:v.aead.NonceSize()], ciphertext[v.aead.NonceSize():], []byte("infrapulse:data-source:v1:"+sourceID))
}
