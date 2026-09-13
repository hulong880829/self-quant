package account

import (
	"context"
	"errors"
	"testing"
	"time"
)

type memoryTradingStore struct {
	nextID  int64
	records []TradingAccountRecord
}

func (m *memoryTradingStore) ListByOwner(_ context.Context, owner string) ([]TradingAccountRecord, error) {
	result := make([]TradingAccountRecord, 0)
	for _, record := range m.records {
		if record.OwnerUsername == owner {
			result = append(result, record)
		}
	}
	return result, nil
}

func (m *memoryTradingStore) GetByOwner(_ context.Context, owner string, id int64) (TradingAccountRecord, error) {
	for _, record := range m.records {
		if record.OwnerUsername == owner && record.ID == id {
			return record, nil
		}
	}
	return TradingAccountRecord{}, ErrTradingAccountNotFound
}

func (m *memoryTradingStore) ListByOwnerProduct(_ context.Context, owner, product string) ([]TradingAccountRecord, error) {
	result := make([]TradingAccountRecord, 0)
	for _, record := range m.records {
		if record.OwnerUsername == owner && record.ProductName == product {
			result = append(result, record)
		}
	}
	return result, nil
}

func (m *memoryTradingStore) Create(_ context.Context, record TradingAccountRecord) (TradingAccountRecord, error) {
	for _, existing := range m.records {
		if existing.OwnerUsername == record.OwnerUsername &&
			existing.ProductName == record.ProductName &&
			existing.Exchange == record.Exchange &&
			existing.AccountName == record.AccountName {
			return TradingAccountRecord{}, ErrDuplicateTradingAccount
		}
	}
	m.nextID++
	record.ID = m.nextID
	now := time.Now().UTC()
	record.CreatedAt = now
	record.UpdatedAt = now
	m.records = append(m.records, record)
	return record, nil
}

func (m *memoryTradingStore) UpdateIndexes(_ context.Context, owner string, id int64, accountIndex *int64, apiKeyIndex *int16) error {
	for index, record := range m.records {
		if record.OwnerUsername == owner && record.ID == id {
			record.AccountIndex = accountIndex
			record.APIKeyIndex = apiKeyIndex
			record.UpdatedAt = time.Now().UTC()
			m.records[index] = record
			return nil
		}
	}
	return ErrTradingAccountNotFound
}

func (m *memoryTradingStore) DeleteByOwner(_ context.Context, owner string, id int64) error {
	for index, record := range m.records {
		if record.OwnerUsername == owner && record.ID == id {
			m.records = append(m.records[:index], m.records[index+1:]...)
			return nil
		}
	}
	return ErrTradingAccountNotFound
}

func newTradingTestService(t *testing.T) (*Service, *memoryTradingStore) {
	t.Helper()
	hash, err := HashPassword("admin123")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := NewTokenService("test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := NewCredentialCipher("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryTradingStore{}
	service := NewService(memoryAccounts{account: Account{
		Username: "admin", PasswordHash: hash, Permission: "admin",
	}}, tokens).WithTrading(store, cipher)
	return service, store
}

func TestTradingAccountCreateListDelete(t *testing.T) {
	service, store := newTradingTestService(t)
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateTradingAccount(context.Background(), session.Token, CreateTradingAccountInput{
		ProductName: "Funding Arb",
		Exchange:    "Binance",
		AccountName: "main",
		APIKey:      "abcdefghijklmnop",
		APISecret:   "secret-value",
		Passphrase:  "phrase",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Exchange != "binance" || created.APIKeyMasked != "" || !created.HasPassphrase {
		t.Fatalf("created=%+v", created)
	}
	if len(store.records) != 1 || string(store.records[0].APISecretEnc) == "secret-value" {
		t.Fatalf("credentials were not encrypted: %+v", store.records[0])
	}

	listed, err := service.ListTradingAccounts(context.Background(), session.Token)
	if err != nil || len(listed) != 1 || listed[0].AccountName != "main" {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	if err := service.DeleteTradingAccount(context.Background(), session.Token, created.ID); err != nil {
		t.Fatal(err)
	}
	listed, err = service.ListTradingAccounts(context.Background(), session.Token)
	if err != nil || len(listed) != 0 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
}

func TestInternalTradingCredentialsRequireTokenAndOwner(t *testing.T) {
	service, _ := newTradingTestService(t)
	service.WithInternalTrader("trader-secret")
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateTradingAccount(context.Background(), session.Token, CreateTradingAccountInput{
		ProductName: "Core", Exchange: "binance", AccountName: "main",
		APIKey: "key", APISecret: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetTradingCredentialsInternal(
		context.Background(), "wrong", "admin", created.ID,
	); !errors.Is(err, ErrInvalidServiceToken) {
		t.Fatalf("wrong token err=%v", err)
	}
	if _, err := service.GetTradingCredentialsInternal(
		context.Background(), "trader-secret", "other", created.ID,
	); !errors.Is(err, ErrTradingAccountNotFound) {
		t.Fatalf("wrong owner err=%v", err)
	}
	credentials, err := service.GetTradingCredentialsInternal(
		context.Background(), "trader-secret", "admin", created.ID,
	)
	if err != nil || credentials.APISecret != "secret" {
		t.Fatalf("credentials=%+v err=%v", credentials, err)
	}
}

func TestTradingAccountRejectsUnsupportedExchange(t *testing.T) {
	service, _ := newTradingTestService(t)
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.CreateTradingAccount(context.Background(), session.Token, CreateTradingAccountInput{
		ProductName: "Demo",
		Exchange:    "unknown",
		AccountName: "main",
		APIKey:      "abcdefghijklmnop",
		APISecret:   "secret",
	})
	if err != ErrUnsupportedExchange {
		t.Fatalf("err=%v", err)
	}
}

func TestTradingAccountRequiresPassphraseForOKX(t *testing.T) {
	service, _ := newTradingTestService(t)
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.CreateTradingAccount(context.Background(), session.Token, CreateTradingAccountInput{
		ProductName: "Demo",
		Exchange:    "okx",
		AccountName: "main",
		APIKey:      "abcdefghijklmnop",
		APISecret:   "secret",
	})
	if err != ErrPassphraseRequired {
		t.Fatalf("err=%v", err)
	}
}

func TestGetTradingCredentialsOwnerScoped(t *testing.T) {
	service, _ := newTradingTestService(t)
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateTradingAccount(context.Background(), session.Token, CreateTradingAccountInput{
		ProductName: "Funding Arb",
		Exchange:    "binance",
		AccountName: "main",
		APIKey:      "abcdefghijklmnop",
		APISecret:   "secret-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := service.GetTradingCredentials(context.Background(), session.Token, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.APIKey != "abcdefghijklmnop" || credentials.APISecret != "secret-value" {
		t.Fatalf("credentials=%+v", credentials)
	}
	if _, err := service.GetTradingCredentials(context.Background(), "bad-token", created.ID); err != ErrInvalidToken {
		t.Fatalf("err=%v", err)
	}
	if _, err := service.GetTradingCredentials(context.Background(), session.Token, 999); err != ErrTradingAccountNotFound {
		t.Fatalf("err=%v", err)
	}
}

func TestWalletDEXTradingAccountBindAndList(t *testing.T) {
	const (
		walletKey    = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
		otherAddress = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
	)
	service, _ := newTradingTestService(t)
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	for _, exchange := range []string{"hyperliquid", "aster", "lighter"} {
		created, createErr := service.CreateTradingAccount(context.Background(), session.Token, CreateTradingAccountInput{
			ProductName: "DEX", Exchange: exchange, AccountName: exchange,
			APIKey: otherAddress, APISecret: walletKey,
		})
		if createErr != nil {
			t.Fatalf("%s create: %v", exchange, createErr)
		}
		if created.WalletAddress != otherAddress || created.APIKeyMasked == walletKey {
			t.Fatalf("%s view=%+v", exchange, created)
		}
		if !created.CredentialsPresent || created.TradingReady || created.TradingStatus != TradingStatusChecking {
			t.Fatalf("%s list view leaked readiness: %+v", exchange, created)
		}
		credentials, credErr := service.GetTradingCredentials(context.Background(), session.Token, created.ID)
		if credErr != nil {
			t.Fatalf("%s credentials err=%v", exchange, credErr)
		}
		if credentials.CredentialKind != defaultAPIWalletKind(exchange) ||
			credentials.APIKey != otherAddress || credentials.APISecret != walletKey {
			t.Fatalf("%s credentials=%+v", exchange, credentials)
		}
		meta, metaErr := service.GetTradingAccountMeta(context.Background(), session.Token, created.ID)
		if metaErr != nil || meta.Exchange != exchange || meta.CredentialKind != defaultAPIWalletKind(exchange) {
			t.Fatalf("%s meta=%+v err=%v", exchange, meta, metaErr)
		}
	}
	listed, err := service.ListTradingAccounts(context.Background(), session.Token)
	if err != nil || len(listed) != 3 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	for _, item := range listed {
		if item.WalletAddress != otherAddress {
			t.Fatalf("walletAddress=%q item=%+v", item.WalletAddress, item)
		}
	}
}

func TestWalletDEXTradingCredentialsDecryptVenueFields(t *testing.T) {
	const (
		walletKey      = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
		walletAddress  = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
		signingAddress = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
	)
	accountIndex, apiKeyIndex := int64(17), int32(3)
	inputs := []CreateTradingAccountInput{
		{ProductName: "DEX", Exchange: "aster", AccountName: "aster", APIKey: walletAddress,
			APISecret: walletKey, TradingAPIKey: "aster-key", TradingAPISecret: "aster-secret"},
		{ProductName: "DEX", Exchange: "hyperliquid", AccountName: "hyperliquid", APIKey: walletAddress,
			APISecret: walletKey, TradingAPISecret: walletKey, SigningAddress: signingAddress},
		{ProductName: "DEX", Exchange: "lighter", AccountName: "lighter", APIKey: walletAddress,
			APISecret: walletKey, TradingAPISecret: walletKey, AccountIndex: &accountIndex,
			APIKeyIndex: &apiKeyIndex},
	}
	service, _ := newTradingTestService(t)
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range inputs {
		created, err := service.CreateTradingAccount(context.Background(), session.Token, input)
		if err != nil {
			t.Fatalf("%s create: %v", input.Exchange, err)
		}
		credentials, err := service.GetTradingCredentials(context.Background(), session.Token, created.ID)
		if err != nil {
			t.Fatalf("%s credentials: %v", input.Exchange, err)
		}
		if credentials.CredentialKind != credentialKindForCreate(input) ||
			credentials.APISecret == "" {
			t.Fatalf("%s credentials=%+v", input.Exchange, credentials)
		}
	}
}

func TestWalletDEXTradingAccountRejectsInvalidCredentials(t *testing.T) {
	service, _ := newTradingTestService(t)
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []CreateTradingAccountInput{
		{ProductName: "DEX", Exchange: "hyperliquid", AccountName: "bad-addr",
			APIKey: "not-an-address", APISecret: "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"},
		{ProductName: "DEX", Exchange: "aster", AccountName: "bad-key",
			APIKey: "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266", APISecret: "0x1234"},
	} {
		if _, createErr := service.CreateTradingAccount(context.Background(), session.Token, input); createErr != ErrInvalidTradingAccount {
			t.Fatalf("input=%+v err=%v", input, createErr)
		}
	}
}

func TestGetTradingCredentialsRejectsPolymarket(t *testing.T) {
	service, store := newTradingTestService(t)
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateTradingAccount(context.Background(), session.Token, CreateTradingAccountInput{
		ProductName: "PM",
		Exchange:    "polymarket",
		AccountName: "wallet",
		APIKey:      "abcdefghijklmnop",
		APISecret:   "secret-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.records[0].Exchange != "polymarket" {
		t.Fatalf("exchange=%s", store.records[0].Exchange)
	}
	if _, err := service.GetTradingCredentials(context.Background(), session.Token, created.ID); err != ErrUnsupportedExchange {
		t.Fatalf("err=%v", err)
	}
}

func TestTradingAccountRequiresValidSession(t *testing.T) {
	service, _ := newTradingTestService(t)
	_, err := service.ListTradingAccounts(context.Background(), "bad-token")
	if err != ErrInvalidToken {
		t.Fatalf("err=%v", err)
	}
}

func TestIncompleteExtensionGroupIsNotMixed(t *testing.T) {
	const (
		walletKey     = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
		walletAddress = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	)
	service, store := newTradingTestService(t)
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateTradingAccount(context.Background(), session.Token, CreateTradingAccountInput{
		ProductName: "DEX", Exchange: "aster", AccountName: "partial",
		APIKey: walletAddress, APISecret: walletKey, TradingAPIKey: "only-key",
	}); err != ErrInvalidTradingAccount {
		t.Fatalf("create incomplete extension err=%v", err)
	}
	created, err := service.CreateTradingAccount(context.Background(), session.Token, CreateTradingAccountInput{
		ProductName: "DEX", Exchange: "aster", AccountName: "wallet",
		APIKey: walletAddress, APISecret: walletKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	store.records[0].CredentialKind = CredentialKindAsterHMAC
	if _, err := service.GetTradingCredentials(context.Background(), session.Token, created.ID); err != ErrInvalidTradingAccount {
		t.Fatalf("mixed decrypt err=%v", err)
	}
	meta, err := service.GetTradingAccountMeta(context.Background(), session.Token, created.ID)
	if err != nil || meta.Exchange != "aster" {
		t.Fatalf("meta should not load secrets: %+v err=%v", meta, err)
	}
}

func TestCredentialsPresentDoesNotImplyTradingReady(t *testing.T) {
	ready := TradingReadiness{CredentialsPresent: true, TradingReady: false, TradingStatus: TradingStatusChecking}
	if ready.CredentialsPresent && ready.TradingReady {
		t.Fatal("credentialsPresent must not imply tradingReady")
	}
	aster := asterReadinessFromAccountOK(false)
	if !aster.CredentialsVerified || aster.TradingReady {
		t.Fatalf("aster read-only readiness=%+v", aster)
	}
}
