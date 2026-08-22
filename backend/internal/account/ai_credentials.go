package account

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	AIProviderOpenRouter = "openrouter"
	AICredentialUnknown  = "unknown"
	AICredentialValid    = "valid"
	AICredentialInvalid  = "invalid"
)

var ErrInvalidAICredential = errors.New("invalid ai credential")

type aiCredentialStore interface {
	GetByOwnerProvider(context.Context, string, string) (AICredentialRecord, error)
	Upsert(context.Context, AICredentialRecord) (AICredentialRecord, error)
	UpdateStatus(context.Context, string, string, string, string) (AICredentialRecord, error)
	DeleteByOwnerProvider(context.Context, string, string) error
}

type AICredentialView struct {
	Provider     string
	APIKeyMasked string
	Status       string
	LastError    string
	LastTestedAt time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type AICredentialSecret struct {
	Provider string
	APIKey   string
}

func (s *Service) WithAICredentials(store aiCredentialStore) *Service {
	s.aiCredentials = store
	return s
}

func (s *Service) GetAICredential(
	ctx context.Context,
	token string,
	provider string,
) (AICredentialView, error) {
	session, provider, err := s.aiCredentialSession(token, provider)
	if err != nil {
		return AICredentialView{}, err
	}
	record, err := s.aiCredentials.GetByOwnerProvider(ctx, session.Username, provider)
	if err != nil {
		return AICredentialView{}, err
	}
	return aiCredentialView(record), nil
}

func (s *Service) UpsertAICredential(
	ctx context.Context,
	token string,
	provider string,
	apiKey string,
) (AICredentialView, error) {
	session, provider, err := s.aiCredentialSession(token, provider)
	if err != nil {
		return AICredentialView{}, err
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" || len(apiKey) > 8192 || s.cipher == nil {
		return AICredentialView{}, ErrInvalidAICredential
	}
	encrypted, err := s.cipher.Encrypt(apiKey)
	if err != nil {
		return AICredentialView{}, fmt.Errorf("encrypt ai credential: %w", err)
	}
	record, err := s.aiCredentials.Upsert(ctx, AICredentialRecord{
		OwnerUsername: session.Username,
		Provider:      provider,
		APIKeyEnc:     encrypted,
		APIKeyMasked:  MaskAPIKey(apiKey),
	})
	if err != nil {
		return AICredentialView{}, err
	}
	return aiCredentialView(record), nil
}

func (s *Service) DeleteAICredential(
	ctx context.Context,
	token string,
	provider string,
) error {
	session, provider, err := s.aiCredentialSession(token, provider)
	if err != nil {
		return err
	}
	return s.aiCredentials.DeleteByOwnerProvider(ctx, session.Username, provider)
}

func (s *Service) GetAICredentialSecret(
	ctx context.Context,
	token string,
	provider string,
) (AICredentialSecret, error) {
	session, provider, err := s.aiCredentialSession(token, provider)
	if err != nil {
		return AICredentialSecret{}, err
	}
	record, err := s.aiCredentials.GetByOwnerProvider(ctx, session.Username, provider)
	if err != nil {
		return AICredentialSecret{}, err
	}
	apiKey, err := s.cipher.Decrypt(record.APIKeyEnc)
	if err != nil {
		return AICredentialSecret{}, fmt.Errorf("decrypt ai credential: %w", err)
	}
	return AICredentialSecret{Provider: provider, APIKey: apiKey}, nil
}

func (s *Service) UpdateAICredentialStatus(
	ctx context.Context,
	token string,
	provider string,
	credentialStatus string,
	lastError string,
) (AICredentialView, error) {
	session, provider, err := s.aiCredentialSession(token, provider)
	if err != nil {
		return AICredentialView{}, err
	}
	credentialStatus = strings.ToLower(strings.TrimSpace(credentialStatus))
	if credentialStatus != AICredentialUnknown &&
		credentialStatus != AICredentialValid &&
		credentialStatus != AICredentialInvalid {
		return AICredentialView{}, ErrInvalidAICredential
	}
	lastError = strings.TrimSpace(lastError)
	if len(lastError) > 500 {
		lastError = lastError[:500]
	}
	record, err := s.aiCredentials.UpdateStatus(
		ctx, session.Username, provider, credentialStatus, lastError,
	)
	if err != nil {
		return AICredentialView{}, err
	}
	return aiCredentialView(record), nil
}

func (s *Service) aiCredentialSession(
	token string,
	provider string,
) (Session, string, error) {
	session, err := s.ValidateSession(token)
	if err != nil {
		return Session{}, "", err
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != AIProviderOpenRouter || s.aiCredentials == nil || s.cipher == nil {
		return Session{}, "", ErrInvalidAICredential
	}
	return session, provider, nil
}

func aiCredentialView(record AICredentialRecord) AICredentialView {
	return AICredentialView{
		Provider:     record.Provider,
		APIKeyMasked: record.APIKeyMasked,
		Status:       record.Status,
		LastError:    record.LastError,
		LastTestedAt: record.LastTestedAt,
		CreatedAt:    record.CreatedAt,
		UpdatedAt:    record.UpdatedAt,
	}
}
