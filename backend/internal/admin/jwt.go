package admin

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"
)

const brokerAudience = "selfquant-api"

type BrokerClaims struct {
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

type Signer struct {
	key    ed25519.PrivateKey
	kid    string
	ttl    time.Duration
	issuer string
	now    func() time.Time
}

func LoadSigner(path string, ttl time.Duration) (*Signer, error) {
	key, err := loadPrivateKey(path)
	if err != nil {
		return nil, err
	}
	kid := base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)[:8])
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Signer{key: key, kid: kid, ttl: ttl, issuer: "selfquant-admin", now: time.Now}, nil
}

func (s *Signer) Issue(brokerID string, tokenVersion int64) (string, int64, error) {
	now := s.now().UTC()
	exp := now.Add(s.ttl)
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", 0, err
	}
	header, err := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": s.kid})
	if err != nil {
		return "", 0, err
	}
	claims, err := json.Marshal(BrokerClaims{
		Issuer: s.issuer, Subject: brokerID, Audience: brokerAudience,
		Role: "broker_admin", BrokerID: brokerID, TokenVersion: tokenVersion,
		IssuedAt: now.Unix(), ExpiresAt: exp.Unix(),
		JWTID: base64.RawURLEncoding.EncodeToString(jti),
	})
	if err != nil {
		return "", 0, err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	sig := ed25519.Sign(s.key, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), int64(s.ttl.Seconds()), nil
}

func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read jwt private key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("jwt private key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse jwt private key: %w", err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("jwt private key is not Ed25519")
	}
	return key, nil
}
