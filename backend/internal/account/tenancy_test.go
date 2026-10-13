package account

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCallerTokenPermissionMatrix(t *testing.T) {
	hash, err := HashPassword("password1")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := NewTokenService("test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(memoryAccounts{account: Account{
		Username: "user", PasswordHash: hash, Permission: "user",
		BrokerID: "broker-1", Status: AccountStatusActive, AuthVersion: 1,
	}}, tokens).
		WithInternalGateway("gateway-token").
		WithInternalAdmin("admin-token").
		WithInternalReports("report-token", nil).
		WithInternalTrader("trader-token")

	if _, err := service.IssueBrokerDelegatedSessionInternal(
		context.Background(), "admin-token", "broker-1", "user", "actor",
	); err != ErrInvalidServiceToken {
		t.Fatalf("admin cannot issue delegated session: %v", err)
	}
	if _, err := service.IssueBrokerDelegatedSessionInternal(
		context.Background(), "", "broker-1", "user", "actor",
	); err != ErrInvalidServiceToken {
		t.Fatalf("empty token: %v", err)
	}
	if _, err := service.IssueBrokerDelegatedSessionInternal(
		context.Background(), "wrong-token", "broker-1", "user", "actor",
	); err != ErrInvalidServiceToken {
		t.Fatalf("wrong token: %v", err)
	}
	session, err := service.IssueBrokerDelegatedSessionInternal(
		context.Background(), "gateway-token", "broker-1", "user", "actor",
	)
	if err != nil || session.Token == "" || session.Username != "user" {
		t.Fatalf("gateway delegated session: %+v err=%v", session, err)
	}

	if _, err := service.GetAdminAccountSummaryInternal(context.Background(), "gateway-token"); err != ErrInvalidServiceToken {
		t.Fatalf("gateway cannot call admin summary: %v", err)
	}
	if _, err := service.GetAdminAccountSummaryInternal(context.Background(), "report-token"); err != ErrInvalidServiceToken {
		t.Fatalf("report cannot call admin summary: %v", err)
	}
	if _, err := service.GetAdminAccountSummaryInternal(context.Background(), "admin-token"); errors.Is(err, ErrInvalidServiceToken) {
		t.Fatalf("admin token must be accepted for summary: %v", err)
	}

	if _, err := service.GetTradingCredentialsInternal(context.Background(), "report-token", "user", 1); err != ErrInvalidServiceToken {
		t.Fatalf("report cannot read trading credentials: %v", err)
	}
	if _, err := service.CreatePlatformAccountInternal(context.Background(), "trader-token", CreatePlatformAccountInput{
		Username: "new-user", Password: "password1",
	}); err != ErrInvalidServiceToken {
		t.Fatalf("trader cannot create platform account: %v", err)
	}
	if _, err := service.DeletePlatformAccountInternal(context.Background(), "trader-token", MutatePlatformAccountInput{
		Username: "user",
	}); err != ErrInvalidServiceToken {
		t.Fatalf("trader cannot delete platform account: %v", err)
	}
	if _, err := service.GetProductAccountSnapshotsInternal(context.Background(), "trader-token", "user", "arb"); err != ErrInvalidServiceToken {
		t.Fatalf("trader cannot call report snapshots: %v", err)
	}
}

func TestDelegatedSessionRejectsNonUserAndInactive(t *testing.T) {
	hash, err := HashPassword("password1")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := NewTokenService("test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	accounts := &mutableAccounts{account: Account{
		Username: "admin", PasswordHash: hash, Permission: "admin",
		BrokerID: "broker-1", Status: AccountStatusActive, AuthVersion: 1,
	}}
	service := NewService(accounts, tokens).WithInternalGateway("gateway-token")
	if _, err := service.IssueBrokerDelegatedSessionInternal(
		context.Background(), "gateway-token", "broker-1", "admin", "actor",
	); err != ErrDelegatedForbidden {
		t.Fatalf("admin target: %v", err)
	}
	accounts.account = Account{
		Username: "user", PasswordHash: hash, Permission: "user",
		BrokerID: "broker-1", Status: AccountStatusDisabled, AuthVersion: 1,
	}
	service.invalidateAccountStatus("user")
	if _, err := service.IssueBrokerDelegatedSessionInternal(
		context.Background(), "gateway-token", "broker-1", "user", "actor",
	); err != ErrDelegatedForbidden {
		t.Fatalf("disabled target: %v", err)
	}
	deleted := time.Now()
	accounts.account.Status = AccountStatusActive
	accounts.account.DeletedAt = &deleted
	service.invalidateAccountStatus("user")
	if _, err := service.IssueBrokerDelegatedSessionInternal(
		context.Background(), "gateway-token", "broker-1", "user", "actor",
	); err != ErrDelegatedForbidden {
		t.Fatalf("deleted target: %v", err)
	}
	accounts.account.DeletedAt = nil
	accounts.account.BrokerID = "other"
	service.invalidateAccountStatus("user")
	if _, err := service.IssueBrokerDelegatedSessionInternal(
		context.Background(), "gateway-token", "broker-1", "user", "actor",
	); err != ErrDelegatedForbidden {
		t.Fatalf("cross broker: %v", err)
	}
}
