package account

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

var (
	ErrInvalidCredentialsKey = errors.New("invalid credentials encryption key")
	ErrInvalidCiphertext     = errors.New("invalid credentials ciphertext")
)

type CredentialCipher struct {
	gcm cipher.AEAD
}

func NewCredentialCipher(rawKey string) (*CredentialCipher, error) {
	key, err := parseCredentialsKey(rawKey)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create gcm: %w", err)
	}
	return &CredentialCipher{gcm: gcm}, nil
}

func parseCredentialsKey(rawKey string) ([]byte, error) {
	value := strings.TrimSpace(rawKey)
	if value == "" {
		return nil, ErrInvalidCredentialsKey
	}
	if decoded, err := hex.DecodeString(value); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(value); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if len(value) == 32 {
		return []byte(value), nil
	}
	return nil, ErrInvalidCredentialsKey
}

func (c *CredentialCipher) Encrypt(plaintext string) ([]byte, error) {
	if c == nil || c.gcm == nil {
		return nil, ErrInvalidCredentialsKey
	}
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return c.gcm.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

func (c *CredentialCipher) Decrypt(ciphertext []byte) (string, error) {
	if c == nil || c.gcm == nil {
		return "", ErrInvalidCredentialsKey
	}
	nonceSize := c.gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", ErrInvalidCiphertext
	}
	nonce, payload := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plain, err := c.gcm.Open(nil, nonce, payload, nil)
	if err != nil {
		return "", ErrInvalidCiphertext
	}
	return string(plain), nil
}

func MaskAPIKey(apiKey string) string {
	trimmed := strings.TrimSpace(apiKey)
	if trimmed == "" {
		return ""
	}
	runes := []rune(trimmed)
	if len(runes) <= 8 {
		return strings.Repeat("*", len(runes))
	}
	return string(runes[:4]) + strings.Repeat("*", len(runes)-8) + string(runes[len(runes)-4:])
}
