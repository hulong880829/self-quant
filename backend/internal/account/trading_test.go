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
	if created.Exchange != "binance" || created.APIKeyMasked != "abcd********mnop" || !created.HasPassphrase {
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
