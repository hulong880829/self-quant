package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	adminv1 "selfquant/backend/gen/admin/v1"
)

const brokerAudience = "selfquant-api"

type brokerClaims struct {
	Issuer       string `json:"iss"`
	Subject      string `json:"sub"`
	Audience     string `json:"aud"`
	Role         string `json:"role"`
	BrokerID     string `json:"broker_id"`
	TokenVersion int64  `json:"token_version"`
	IssuedAt     int64  `json:"iat"`
	ExpiresAt    int64  `json:"exp"`
	JWTID        string `json:"jti"`
}

type brokerAuthCache struct {
	status       string
	tokenVersion int64
	expires      time.Time
}

type delegatedSession struct {
	token   string
	expires time.Time
}

func loadBrokerPublicKey(path string) (ed25519.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read jwt public key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("jwt public key is not PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse jwt public key: %w", err)
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("jwt public key is not Ed25519")
	}
	return key, nil
}

func parseBrokerAccessToken(token string, key ed25519.PublicKey) (brokerClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return brokerClaims{}, errors.New("invalid access token")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return brokerClaims{}, errors.New("invalid access token")
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil || header.Alg != "EdDSA" {
		return brokerClaims{}, errors.New("invalid access token")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return brokerClaims{}, errors.New("invalid access token")
	}
	if !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return brokerClaims{}, errors.New("invalid access token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return brokerClaims{}, errors.New("invalid access token")
	}
	var claims brokerClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return brokerClaims{}, errors.New("invalid access token")
	}
	if claims.Audience != brokerAudience || claims.Role != "broker_admin" ||
		strings.TrimSpace(claims.BrokerID) == "" || claims.ExpiresAt <= time.Now().Unix() {
		return brokerClaims{}, errors.New("invalid access token")
	}
	return claims, nil
}

func (h *Handler) lookupBrokerState(ctx context.Context, brokerID string) (brokerAuthCache, error) {
	now := time.Now()
	h.brokerStateMu.RLock()
	if entry, ok := h.brokerState[brokerID]; ok && now.Before(entry.expires) {
		h.brokerStateMu.RUnlock()
		return entry, nil
	}
	h.brokerStateMu.RUnlock()
	if h.admin == nil {
		return brokerAuthCache{}, errAdminUnavailable
	}
	response, err := h.admin.GetBrokerAuthStateInternal(ctx, &adminv1.GetBrokerAuthStateInternalRequest{
		ServiceToken: h.gatewayInternalToken,
		BrokerId:     brokerID,
	})
	if err != nil {
		return brokerAuthCache{}, err
	}
	entry := brokerAuthCache{
		status: response.GetStatus(), tokenVersion: response.GetTokenVersion(),
		expires: now.Add(h.brokerAuthTTL),
	}
	h.brokerStateMu.Lock()
	if h.brokerState == nil {
		h.brokerState = map[string]brokerAuthCache{}
	}
	h.brokerState[brokerID] = entry
	h.brokerStateMu.Unlock()
	return entry, nil
}

func (h *Handler) invalidateBrokerState(brokerID string) {
	h.brokerStateMu.Lock()
	delete(h.brokerState, brokerID)
	h.brokerStateMu.Unlock()
	h.delegatedMu.Lock()
	for key := range h.delegated {
		if strings.HasPrefix(key, brokerID+"\x00") {
			delete(h.delegated, key)
		}
	}
	h.delegatedMu.Unlock()
}

var errAdminUnavailable = errors.New("admin service unavailable")

type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	window time.Duration
	limit  int
}

func newRateLimiter(window time.Duration, limit int) *rateLimiter {
	return &rateLimiter{hits: map[string][]time.Time{}, window: window, limit: limit}
}

func (l *rateLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.window)
	kept := l.hits[key][:0]
	for _, at := range l.hits[key] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	if len(kept) >= l.limit {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}
