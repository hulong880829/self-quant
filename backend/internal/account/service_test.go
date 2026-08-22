package account

import (
	"context"
	"testing"
	"time"
)

type memoryAccounts struct {
	account Account
}

func (m memoryAccounts) GetByUsername(_ context.Context, username string) (Account, error) {
	if username != m.account.Username {
		return Account{}, ErrNotFound
	}
	return m.account, nil
}

func TestServiceLoginAndValidate(t *testing.T) {
	hash, err := HashPassword("admin123")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := NewTokenService("test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(memoryAccounts{account: Account{
		Username: "admin", PasswordHash: hash, Permission: "admin",
	}}, tokens)
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	if session.Username != "admin" || session.Permission != "admin" || session.Token == "" {
		t.Fatalf("session=%+v", session)
	}
	validated, err := service.ValidateSession(session.Token)
	if err != nil || validated.Username != "admin" {
		t.Fatalf("validated=%+v err=%v", validated, err)
	}
	if _, err := service.Login(context.Background(), "admin", "bad"); err != ErrInvalidCredentials {
		t.Fatalf("err=%v", err)
	}
}
