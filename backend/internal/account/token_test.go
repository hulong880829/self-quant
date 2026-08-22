package account

import (
	"strings"
	"testing"
	"time"
)

func TestTokenServiceRoundTrip(t *testing.T) {
	service, err := NewTokenService("test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.Issue("admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := service.Validate(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Username != "admin" || claims.Permission != "admin" {
		t.Fatalf("claims=%+v", claims)
	}
}

func TestTokenServiceRejectsTamperedToken(t *testing.T) {
	service, err := NewTokenService("test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.Issue("admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	parts[1] = parts[1] + "x"
	if _, err := service.Validate(strings.Join(parts, ".")); err != ErrInvalidToken {
		t.Fatalf("err=%v", err)
	}
}

func TestTokenServiceRejectsExpiredToken(t *testing.T) {
	service, err := NewTokenService("test-secret", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	service.now = func() time.Time { return now }
	token, err := service.Issue("admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now.Add(time.Second) }
	if _, err := service.Validate(token); err != ErrExpiredToken {
		t.Fatalf("err=%v", err)
	}
}
