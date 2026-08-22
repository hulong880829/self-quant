package ai

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	accountv1 "selfquant/backend/gen/account/v1"
)

type accountCredentialClient interface {
	ValidateSession(
		context.Context,
		*accountv1.ValidateSessionRequest,
		...grpc.CallOption,
	) (*accountv1.ValidateSessionResponse, error)
	GetAICredential(
		context.Context,
		*accountv1.GetAICredentialRequest,
		...grpc.CallOption,
	) (*accountv1.AICredentialResponse, error)
	GetAICredentialSecret(
		context.Context,
		*accountv1.GetAICredentialRequest,
		...grpc.CallOption,
	) (*accountv1.AICredentialSecretResponse, error)
	UpdateAICredentialStatus(
		context.Context,
		*accountv1.UpdateAICredentialStatusRequest,
		...grpc.CallOption,
	) (*accountv1.AICredentialResponse, error)
}

type CredentialStatus struct {
	Provider     string
	APIKeyMasked string
	Status       string
	LastError    string
	LastTestedAt time.Time
}

type AccountCredentialProvider struct {
	client accountCredentialClient
}

func NewAccountCredentialProvider(client accountCredentialClient) *AccountCredentialProvider {
	return &AccountCredentialProvider{client: client}
}

func (p *AccountCredentialProvider) Owner(ctx context.Context, token string) (string, error) {
	response, err := p.client.ValidateSession(ctx, &accountv1.ValidateSessionRequest{Token: token})
	if err != nil {
		return "", mapAccountCredentialError(err)
	}
	if response.GetUsername() == "" {
		return "", status.Error(codes.Unauthenticated, "invalid session")
	}
	return response.GetUsername(), nil
}

func (p *AccountCredentialProvider) Status(
	ctx context.Context,
	token string,
	provider string,
) (CredentialStatus, error) {
	response, err := p.client.GetAICredential(ctx, &accountv1.GetAICredentialRequest{
		Token: token, Provider: provider,
	})
	if err != nil {
		return CredentialStatus{}, mapAccountCredentialError(err)
	}
	if response.GetCredential() == nil {
		return CredentialStatus{}, ErrCredentialMissing
	}
	return credentialStatusFromProto(response.GetCredential()), nil
}

func (p *AccountCredentialProvider) Secret(
	ctx context.Context,
	token string,
	provider string,
) (string, error) {
	response, err := p.client.GetAICredentialSecret(ctx, &accountv1.GetAICredentialRequest{
		Token: token, Provider: provider,
	})
	if err != nil {
		return "", mapAccountCredentialError(err)
	}
	if response.GetApiKey() == "" {
		return "", ErrCredentialMissing
	}
	return response.GetApiKey(), nil
}

func (p *AccountCredentialProvider) UpdateStatus(
	ctx context.Context,
	token string,
	provider string,
	credentialStatus string,
	lastError string,
) (CredentialStatus, error) {
	response, err := p.client.UpdateAICredentialStatus(
		ctx,
		&accountv1.UpdateAICredentialStatusRequest{
			Token: token, Provider: provider, Status: credentialStatus,
			ErrorMessage: lastError,
		},
	)
	if err != nil {
		return CredentialStatus{}, mapAccountCredentialError(err)
	}
	if response.GetCredential() == nil {
		return CredentialStatus{}, fmt.Errorf("account service returned empty credential")
	}
	return credentialStatusFromProto(response.GetCredential()), nil
}

func credentialStatusFromProto(item *accountv1.AICredential) CredentialStatus {
	result := CredentialStatus{
		Provider: item.GetProvider(), APIKeyMasked: item.GetApiKeyMasked(),
		Status: item.GetStatus(), LastError: item.GetLastError(),
	}
	if item.GetLastTestedAt() != nil {
		result.LastTestedAt = item.GetLastTestedAt().AsTime()
	}
	return result
}

func mapAccountCredentialError(err error) error {
	switch status.Code(err) {
	case codes.NotFound:
		return ErrCredentialMissing
	case codes.Unauthenticated:
		return status.Error(codes.Unauthenticated, "invalid session")
	case codes.InvalidArgument:
		return fmt.Errorf("%w: invalid credential request", ErrInvalidRequest)
	default:
		return fmt.Errorf("account credential request: %w", err)
	}
}

func credentialIsInvalid(err error) bool {
	return errors.Is(err, ErrCredentialInvalid)
}
