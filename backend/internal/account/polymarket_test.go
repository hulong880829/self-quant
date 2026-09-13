package account

import (
	"context"
	"testing"
	"time"

	"selfquant/backend/internal/polymarketauth"
)

type memoryPolymarketStore struct {
	account TradingAccountRecord
	record  PolymarketCredentialRecord
}

func (m *memoryPolymarketStore) Create(
	_ context.Context,
	account TradingAccountRecord,
	record PolymarketCredentialRecord,
) (TradingAccountRecord, PolymarketCredentialRecord, error) {
	account.ID = 7
	account.CreatedAt, account.UpdatedAt = time.Now(), time.Now()
	record.TradingAccountID = account.ID
	record.OwnerUsername = account.OwnerUsername
	record.AccountName = account.AccountName
	record.BindingStatus = "active"
	m.account, m.record = account, record
	return account, record, nil
}

func (m *memoryPolymarketStore) ListMetadataByOwner(
	_ context.Context,
	owner string,
) ([]PolymarketCredentialRecord, error) {
	if m.record.OwnerUsername != owner {
		return nil, nil
	}
	return []PolymarketCredentialRecord{m.record}, nil
}

func (m *memoryPolymarketStore) GetByOwner(
	_ context.Context,
	owner string,
	accountID int64,
) (PolymarketCredentialRecord, error) {
	if m.record.OwnerUsername != owner || m.record.TradingAccountID != accountID {
		return PolymarketCredentialRecord{}, ErrTradingAccountNotFound
	}
	return m.record, nil
}

func (m *memoryPolymarketStore) UpdateCredentialsByOwner(
	_ context.Context,
	owner string,
	accountID int64,
	apiKey, secret, passphrase []byte,
	status string,
) error {
	if m.record.OwnerUsername != owner || m.record.TradingAccountID != accountID {
		return ErrTradingAccountNotFound
	}
	m.record.APIKeyEnc, m.record.APISecretEnc, m.record.PassphraseEnc = apiKey, secret, passphrase
	m.record.BindingStatus = status
	return nil
}

func (m *memoryPolymarketStore) UpdateBindingStatusByOwner(
	_ context.Context,
	owner string,
	accountID int64,
	status string,
) error {
	if m.record.OwnerUsername != owner || m.record.TradingAccountID != accountID {
		return ErrTradingAccountNotFound
	}
	m.record.BindingStatus = status
	return nil
}

type refreshingIssuer struct{}

func (refreshingIssuer) CreateOrDerive(
	context.Context, string, uint64,
) (polymarketauth.Credentials, string, error) {
	return polymarketauth.Credentials{
		APIKey: "old-key", Secret: "old-secret", Passphrase: "old-pass",
	}, "0x0000000000000000000000000000000000000001", nil
}

func (refreshingIssuer) Derive(
	context.Context, string, uint64,
) (polymarketauth.Credentials, string, error) {
	return polymarketauth.Credentials{
		APIKey: "new-key", Secret: "new-secret", Passphrase: "new-pass",
	}, "0x0000000000000000000000000000000000000001", nil
}

func TestSignatureTypeForWallet(t *testing.T) {
	if got := signatureTypeForWallet("eoa"); got != 0 {
		t.Fatalf("EOA signature type = %d, want 0", got)
	}
	if got := signatureTypeForWallet("deposit"); got != 1 {
		t.Fatalf("Proxy Wallet signature type = %d, want 1", got)
	}
}

func TestRefreshPolymarketCredentialsIsOwnerScopedAndAtomicAtStoreBoundary(t *testing.T) {
	service, _ := newTradingTestService(t)
	store := &memoryPolymarketStore{}
	service.WithPolymarket(store, refreshingIssuer{})
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreatePolymarketTradingAccount(
		context.Background(), session.Token,
		CreatePolymarketTradingAccountInput{
			AccountName: "poly", PrivateKey: "private",
			WalletType: "eoa",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := service.RefreshPolymarketCredentials(
		context.Background(), session.Token, created.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.APIKey != "new-key" || refreshed.APISecret != "new-secret" ||
		refreshed.Passphrase != "new-pass" || store.record.BindingStatus != "active" {
		t.Fatalf("refreshed=%+v status=%q", refreshed, store.record.BindingStatus)
	}
	if string(store.record.APIKeyEnc) == "new-key" ||
		string(store.record.APISecretEnc) == "new-secret" {
		t.Fatal("refreshed credentials were stored without encryption")
	}
	if _, err := service.RefreshPolymarketCredentials(
		context.Background(), session.Token, created.ID+1,
	); err != ErrTradingAccountNotFound {
		t.Fatalf("cross-account refresh err=%v", err)
	}
}

func TestRefreshPolymarketCredentialsKeepsInvalidBindingStatus(t *testing.T) {
	service, _ := newTradingTestService(t)
	store := &memoryPolymarketStore{}
	service.WithPolymarket(store, refreshingIssuer{})
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreatePolymarketTradingAccount(
		context.Background(), session.Token,
		CreatePolymarketTradingAccountInput{
			AccountName: "poly", PrivateKey: "private",
			WalletType: "eoa",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.InvalidatePolymarketCredentials(
		context.Background(), session.Token, created.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetPolymarketCredentials(
		context.Background(), session.Token, created.ID,
	); err != ErrPolymarketCredentialsInvalid {
		t.Fatalf("get while invalid err=%v", err)
	}
	if _, err := service.RefreshPolymarketCredentials(
		context.Background(), session.Token, created.ID,
	); err != nil {
		t.Fatal(err)
	}
	if store.record.BindingStatus != "invalid" {
		t.Fatalf("status=%q", store.record.BindingStatus)
	}
	if _, err := service.GetPolymarketCredentials(
		context.Background(), session.Token, created.ID,
	); err != ErrPolymarketCredentialsInvalid {
		t.Fatalf("get after refresh err=%v", err)
	}
	if err := service.ActivatePolymarketCredentials(
		context.Background(), session.Token, created.ID,
	); err != nil {
		t.Fatal(err)
	}
	if store.record.BindingStatus != "active" {
		t.Fatalf("status=%q", store.record.BindingStatus)
	}
	if _, err := service.GetPolymarketCredentials(
		context.Background(), session.Token, created.ID,
	); err != nil {
		t.Fatalf("get after activate err=%v", err)
	}
}
