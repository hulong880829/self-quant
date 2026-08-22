package account

import (
	"context"
	"testing"
	"time"
)

type memoryAICredentialStore struct {
	records map[string]AICredentialRecord
}

func (m *memoryAICredentialStore) key(owner, provider string) string {
	return owner + "\x00" + provider
}

func (m *memoryAICredentialStore) GetByOwnerProvider(
	_ context.Context,
	owner string,
	provider string,
) (AICredentialRecord, error) {
	record, ok := m.records[m.key(owner, provider)]
	if !ok {
		return AICredentialRecord{}, ErrAICredentialNotFound
	}
	return record, nil
}

func (m *memoryAICredentialStore) Upsert(
	_ context.Context,
	record AICredentialRecord,
) (AICredentialRecord, error) {
	now := time.Now().UTC()
	key := m.key(record.OwnerUsername, record.Provider)
	if old, ok := m.records[key]; ok {
		record.ID = old.ID
		record.CreatedAt = old.CreatedAt
	} else {
		record.ID = int64(len(m.records) + 1)
		record.CreatedAt = now
	}
	record.Status = AICredentialUnknown
	record.UpdatedAt = now
	m.records[key] = record
	return record, nil
}

func (m *memoryAICredentialStore) UpdateStatus(
	_ context.Context,
	owner string,
	provider string,
	credentialStatus string,
	lastError string,
) (AICredentialRecord, error) {
	key := m.key(owner, provider)
	record, ok := m.records[key]
	if !ok {
		return AICredentialRecord{}, ErrAICredentialNotFound
	}
	record.Status = credentialStatus
	record.LastError = lastError
	record.LastTestedAt = time.Now().UTC()
	record.UpdatedAt = record.LastTestedAt
	m.records[key] = record
	return record, nil
}

func (m *memoryAICredentialStore) DeleteByOwnerProvider(
	_ context.Context,
	owner string,
	provider string,
) error {
	key := m.key(owner, provider)
	if _, ok := m.records[key]; !ok {
		return ErrAICredentialNotFound
	}
	delete(m.records, key)
	return nil
}

func newAICredentialTestService(t *testing.T) (*Service, *memoryAICredentialStore, string) {
	t.Helper()
	service, _ := newTradingTestService(t)
	store := &memoryAICredentialStore{records: make(map[string]AICredentialRecord)}
	service.WithAICredentials(store)
	session, err := service.Login(context.Background(), "admin", "admin123")
	if err != nil {
		t.Fatal(err)
	}
	return service, store, session.Token
}

func TestAICredentialLifecycleEncryptsAndScopes(t *testing.T) {
	service, store, token := newAICredentialTestService(t)
	view, err := service.UpsertAICredential(
		context.Background(), token, AIProviderOpenRouter, "sk-or-secret-value",
	)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != AICredentialUnknown || view.APIKeyMasked != "sk-o**********alue" {
		t.Fatalf("view=%+v", view)
	}
	record := store.records[store.key("admin", AIProviderOpenRouter)]
	if string(record.APIKeyEnc) == "sk-or-secret-value" {
		t.Fatal("api key stored in plaintext")
	}
	secret, err := service.GetAICredentialSecret(
		context.Background(), token, AIProviderOpenRouter,
	)
	if err != nil || secret.APIKey != "sk-or-secret-value" {
		t.Fatalf("secret=%+v err=%v", secret, err)
	}

	view, err = service.UpdateAICredentialStatus(
		context.Background(), token, AIProviderOpenRouter, AICredentialValid, "",
	)
	if err != nil || view.Status != AICredentialValid || view.LastTestedAt.IsZero() {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	if err := service.DeleteAICredential(
		context.Background(), token, AIProviderOpenRouter,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetAICredential(
		context.Background(), token, AIProviderOpenRouter,
	); err != ErrAICredentialNotFound {
		t.Fatalf("err=%v", err)
	}
}

func TestAICredentialRotationResetsStatus(t *testing.T) {
	service, _, token := newAICredentialTestService(t)
	if _, err := service.UpsertAICredential(
		context.Background(), token, AIProviderOpenRouter, "first-secret-key",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateAICredentialStatus(
		context.Background(), token, AIProviderOpenRouter, AICredentialInvalid, "bad key",
	); err != nil {
		t.Fatal(err)
	}
	rotated, err := service.UpsertAICredential(
		context.Background(), token, AIProviderOpenRouter, "second-secret-key",
	)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Status != AICredentialUnknown || rotated.LastError != "" ||
		!rotated.LastTestedAt.IsZero() {
		t.Fatalf("rotated=%+v", rotated)
	}
}
