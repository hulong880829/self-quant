package account

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrInvalidToken = errors.New("invalid session token")
	ErrExpiredToken = errors.New("session token expired")
)

type TokenClaims struct {
	Username   string `json:"u"`
	Permission string `json:"p"`
	ExpiresAt  int64  `json:"exp"`
}

type TokenService struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

func NewTokenService(secret string, ttl time.Duration) (*TokenService, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, errors.New("session token secret is required")
	}
	if ttl <= 0 {
		return nil, errors.New("session token ttl must be positive")
	}
	return &TokenService{
		secret: []byte(secret),
		ttl:    ttl,
		now:    time.Now,
	}, nil
}

func (s *TokenService) Issue(username, permission string) (string, error) {
	claims := TokenClaims{
		Username:   username,
		Permission: permission,
		ExpiresAt:  s.now().UTC().Add(s.ttl).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal token claims: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	signature := s.sign(encoded)
	return encoded + "." + signature, nil
}

func (s *TokenService) Validate(token string) (TokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return TokenClaims{}, ErrInvalidToken
	}
	expected := s.sign(parts[0])
	if !hmac.Equal([]byte(parts[1]), []byte(expected)) {
		return TokenClaims{}, ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return TokenClaims{}, ErrInvalidToken
	}
	var claims TokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return TokenClaims{}, ErrInvalidToken
	}
	if claims.Username == "" || claims.ExpiresAt <= 0 {
		return TokenClaims{}, ErrInvalidToken
	}
	if s.now().UTC().Unix() >= claims.ExpiresAt {
		return TokenClaims{}, ErrExpiredToken
	}
	return claims, nil
}

func (s *TokenService) sign(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
