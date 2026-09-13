package rpc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	accountv1 "selfquant/backend/gen/account/v1"
	"selfquant/backend/internal/account"
	"selfquant/backend/internal/polymarketauth"
)

type fakeAccountService struct {
	session account.Session
	err     error
	items   []account.TradingAccountView
	created account.TradingAccountView
}

func (f fakeAccountService) Login(context.Context, string, string) (account.Session, error) {
	return f.session, f.err
}

func (f fakeAccountService) ValidateSession(string) (account.Session, error) {
	return f.session, f.err
}

func (f fakeAccountService) ListTradingAccounts(context.Context, string) ([]account.TradingAccountView, error) {
	return f.items, f.err
}

func (f fakeAccountService) CreateTradingAccount(
	context.Context,
	string,
	account.CreateTradingAccountInput,
) (account.TradingAccountView, error) {
	return f.created, f.err
}

func (f fakeAccountService) CreatePolymarketTradingAccount(
	context.Context,
	string,
	account.CreatePolymarketTradingAccountInput,
) (account.TradingAccountView, error) {
	return f.created, f.err
}

func (f fakeAccountService) GetPolymarketCredentials(
	context.Context,
	string,
	int64,
) (account.PolymarketCredentials, error) {
	return account.PolymarketCredentials{}, f.err
}

func (f fakeAccountService) RefreshPolymarketCredentials(
	context.Context,
	string,
	int64,
) (account.PolymarketCredentials, error) {
	return account.PolymarketCredentials{}, f.err
}

func (f fakeAccountService) InvalidatePolymarketCredentials(
	context.Context,
	string,
	int64,
) error {
	return f.err
}

func (f fakeAccountService) ActivatePolymarketCredentials(
	context.Context,
	string,
	int64,
) error {
	return f.err
}

func (f fakeAccountService) DeleteTradingAccount(context.Context, string, int64) error {
	return f.err
}

func (f fakeAccountService) GetTradingAccountSnapshot(context.Context, string, int64) (account.TradingAccountSnapshot, error) {
	return account.TradingAccountSnapshot{}, f.err
}

func (f fakeAccountService) GetCachedTradingAccountSnapshot(context.Context, string, int64) (account.TradingAccountSnapshot, error) {
	return account.TradingAccountSnapshot{}, f.err
}

func (f fakeAccountService) GetProductGroupSnapshot(context.Context, string, string) (account.ProductGroupSnapshot, error) {
	return account.ProductGroupSnapshot{}, f.err
}

func (f fakeAccountService) GetProductAccountSnapshotsInternal(
	context.Context, string, string, string,
) (account.ProductAccountSnapshots, error) {
	return account.ProductAccountSnapshots{}, f.err
}

func (f fakeAccountService) SyncProductTradeFillsInternal(
	context.Context, string, string, string, time.Time,
) ([]account.TradeFillSyncResult, error) {
	return nil, f.err
}

func (f fakeAccountService) GetTradingCredentials(
	context.Context, string, int64,
) (account.TradingCredentials, error) {
	return account.TradingCredentials{}, f.err
}

func (f fakeAccountService) GetTradingCredentialsInternal(
	context.Context, string, string, int64,
) (account.TradingCredentials, error) {
	return account.TradingCredentials{}, f.err
}

func (f fakeAccountService) GetTradingAccountMeta(
	context.Context, string, int64,
) (account.TradingAccountMeta, error) {
	return account.TradingAccountMeta{}, f.err
}

func (f fakeAccountService) InspectTradingReadiness(
	context.Context, string, int64,
) (account.TradingReadiness, error) {
	return account.TradingReadiness{}, f.err
}

func (f fakeAccountService) GetTradingAccountFeeRates(
	context.Context, string, int64,
) (account.TradingAccountFees, error) {
	return account.TradingAccountFees{}, f.err
}

func (f fakeAccountService) SyncTradingAccountFeeRates(
	context.Context, string, int64,
) (account.TradingAccountFees, error) {
	return account.TradingAccountFees{}, f.err
}

func (f fakeAccountService) GetAICredential(
	context.Context, string, string,
) (account.AICredentialView, error) {
	return account.AICredentialView{}, f.err
}

func (f fakeAccountService) UpsertAICredential(
	context.Context, string, string, string,
) (account.AICredentialView, error) {
	return account.AICredentialView{}, f.err
}

func (f fakeAccountService) DeleteAICredential(context.Context, string, string) error {
	return f.err
}

func (f fakeAccountService) GetAICredentialSecret(
	context.Context, string, string,
) (account.AICredentialSecret, error) {
	return account.AICredentialSecret{}, f.err
}

func (f fakeAccountService) UpdateAICredentialStatus(
	context.Context, string, string, string, string,
) (account.AICredentialView, error) {
	return account.AICredentialView{}, f.err
}

func TestAccountServerLoginMapsInvalidCredentials(t *testing.T) {
	server := NewAccountServer(fakeAccountService{err: account.ErrInvalidCredentials})
	_, err := server.Login(context.Background(), &accountv1.LoginRequest{
		Username: "admin", Password: "bad",
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("err=%v", err)
	}
}

func TestAccountServerLoginSuccess(t *testing.T) {
	server := NewAccountServer(fakeAccountService{session: account.Session{
		Token: "tok", Username: "admin", Permission: "admin",
	}})
	response, err := server.Login(context.Background(), &accountv1.LoginRequest{
		Username: "admin", Password: "admin123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Token != "tok" || response.Username != "admin" {
		t.Fatalf("response=%+v", response)
	}
}

func TestAccountServerValidateSessionMapsExpired(t *testing.T) {
	server := NewAccountServer(fakeAccountService{err: account.ErrExpiredToken})
	_, err := server.ValidateSession(context.Background(), &accountv1.ValidateSessionRequest{
		Token: "expired",
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("err=%v", err)
	}
	if !errors.Is(status.Convert(err).Err(), status.Error(codes.Unauthenticated, "invalid session")) &&
		status.Convert(err).Message() != "invalid session" {
		t.Fatalf("message=%q", status.Convert(err).Message())
	}
}

func TestAccountServerCreateTradingAccountMapsDuplicate(t *testing.T) {
	server := NewAccountServer(fakeAccountService{err: account.ErrDuplicateTradingAccount})
	_, err := server.CreateTradingAccount(context.Background(), &accountv1.CreateTradingAccountRequest{
		Token: "tok", ProductName: "p", Exchange: "binance", AccountName: "a",
		ApiKey: "k", ApiSecret: "s",
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("err=%v", err)
	}
}

func TestAccountServerListTradingAccountsSuccess(t *testing.T) {
	now := time.Now().UTC()
	server := NewAccountServer(fakeAccountService{items: []account.TradingAccountView{{
		ID: 1, ProductName: "Funding Arb", Exchange: "binance", AccountName: "main",
		APIKeyMasked: "abcd****mnop", HasPassphrase: true, CreatedAt: now, UpdatedAt: now,
	}}})
	response, err := server.ListTradingAccounts(context.Background(), &accountv1.ListTradingAccountsRequest{
		Token: "tok",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 1 || response.Items[0].GetAccountName() != "main" {
		t.Fatalf("response=%+v", response)
	}
	if response.Items[0].GetApiKeyMasked() == "" {
		t.Fatalf("expected masked api key: %+v", response.Items[0])
	}
}

func TestAccountServerDeleteTradingAccountMapsNotFound(t *testing.T) {
	server := NewAccountServer(fakeAccountService{err: account.ErrTradingAccountNotFound})
	_, err := server.DeleteTradingAccount(context.Background(), &accountv1.DeleteTradingAccountRequest{
		Token: "tok", Id: 9,
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("err=%v", err)
	}
}

func TestAccountServerDeleteTradingAccountMapsActiveJobs(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "arbitrage", err: account.ErrTradingAccountHasActiveArbitrage},
		{name: "twap", err: account.ErrTradingAccountHasActiveTWAP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := NewAccountServer(fakeAccountService{err: tc.err})
			_, err := server.DeleteTradingAccount(context.Background(), &accountv1.DeleteTradingAccountRequest{
				Token: "tok", Id: 9,
			})
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("err=%v", err)
			}
			if status.Convert(err).Message() != tc.err.Error() {
				t.Fatalf("message=%q", status.Convert(err).Message())
			}
		})
	}
}

func TestAccountServerRefreshMapsAuthUnavailable(t *testing.T) {
	server := NewAccountServer(fakeAccountService{err: fmt.Errorf(
		"refresh polymarket credentials: %w",
		&polymarketauth.AuthError{StatusCode: 503, Message: "maintenance"},
	)})
	_, err := server.RefreshPolymarketCredentials(
		context.Background(),
		&accountv1.GetPolymarketCredentialsRequest{Token: "tok", TradingAccountId: 7},
	)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("err=%v", err)
	}
}

func TestAccountServerCacheOnlyMissIsNotNotFound(t *testing.T) {
	server := NewAccountServer(fakeAccountService{err: account.ErrSnapshotCacheMiss})
	_, err := server.GetTradingAccountSnapshot(context.Background(), &accountv1.GetTradingAccountSnapshotRequest{
		Token: "tok", TradingAccountId: 7, CacheOnly: true,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err=%v", err)
	}
	if status.Convert(err).Message() != "snapshot_cache_miss" {
		t.Fatalf("message=%q", status.Convert(err).Message())
	}
}

func TestAccountServerLiveSnapshotNotFoundUnchanged(t *testing.T) {
	server := NewAccountServer(fakeAccountService{err: account.ErrTradingAccountNotFound})
	_, err := server.GetTradingAccountSnapshot(context.Background(), &accountv1.GetTradingAccountSnapshotRequest{
		Token: "tok", TradingAccountId: 7,
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("err=%v", err)
	}
}
