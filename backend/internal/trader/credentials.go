package trader

import (
	"context"

	accountv1 "selfquant/backend/gen/account/v1"
)

type credentialProvider interface {
	Owner(context.Context, string) (string, error)
	Meta(context.Context, string, int64) (AccountMeta, error)
	Get(context.Context, string, int64) (Credentials, error)
}

type tradingReadinessProvider interface {
	InspectTradingReadiness(context.Context, string, int64) (TradingReadiness, error)
}

type TradingReadiness struct {
	Ready                bool
	Status               string
	UnavailableCode      string
	UnavailableReason    string
	ResolvedAccountIndex *int64
	ResolvedAPIKeyIndex  *int32
}

type AccountCredentialProvider struct {
	client accountv1.AccountServiceClient
}

func NewAccountCredentialProvider(client accountv1.AccountServiceClient) *AccountCredentialProvider {
	return &AccountCredentialProvider{client: client}
}

func (p *AccountCredentialProvider) Owner(ctx context.Context, token string) (string, error) {
	response, err := p.client.ValidateSession(ctx, &accountv1.ValidateSessionRequest{Token: token})
	if err != nil {
		return "", err
	}
	return response.GetUsername(), nil
}

func (p *AccountCredentialProvider) Meta(
	ctx context.Context,
	token string,
	accountID int64,
) (AccountMeta, error) {
	response, err := p.client.GetTradingAccountMeta(ctx, &accountv1.GetTradingAccountMetaRequest{
		Token: token, TradingAccountId: accountID,
	})
	if err != nil {
		return AccountMeta{}, err
	}
	return AccountMeta{
		TradingAccountID: response.GetTradingAccountId(),
		ProductName:      response.GetProductName(),
		Exchange:         response.GetExchange(),
		AccountName:      response.GetAccountName(),
		CredentialKind:   response.GetCredentialKind(),
		AccountIndex:     response.AccountIndex,
		APIKeyIndex:      response.ApiKeyIndex,
	}, nil
}

func (p *AccountCredentialProvider) Get(
	ctx context.Context,
	token string,
	accountID int64,
) (Credentials, error) {
	response, err := p.client.GetTradingCredentials(ctx, &accountv1.GetTradingCredentialsRequest{
		Token: token, TradingAccountId: accountID,
	})
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{
		TradingAccountID: response.GetTradingAccountId(),
		ProductName:      response.GetProductName(),
		Exchange:         response.GetExchange(),
		AccountName:      response.GetAccountName(),
		APIKey:           response.GetApiKey(),
		APISecret:        response.GetApiSecret(),
		Passphrase:       response.GetPassphrase(),
		CredentialKind:   response.GetCredentialKind(),
		SigningAddress:   response.GetSigningAddress(),
		VaultAddress:     response.GetVaultAddress(),
		AccountIndex:     response.AccountIndex,
		APIKeyIndex:      response.ApiKeyIndex,
	}, nil
}

func (p *AccountCredentialProvider) InspectTradingReadiness(
	ctx context.Context,
	token string,
	accountID int64,
) (TradingReadiness, error) {
	response, err := p.client.InspectTradingReadiness(
		ctx,
		&accountv1.InspectTradingReadinessRequest{
			Token: token, TradingAccountId: accountID,
		},
	)
	if err != nil {
		return TradingReadiness{}, err
	}
	return TradingReadiness{
		Ready:                response.GetTradingReady(),
		Status:               response.GetTradingStatus(),
		UnavailableCode:      response.GetTradingUnavailableCode(),
		UnavailableReason:    response.GetTradingUnavailableReason(),
		ResolvedAccountIndex: response.ResolvedAccountIndex,
		ResolvedAPIKeyIndex:  response.ResolvedApiKeyIndex,
	}, nil
}

func (p *AccountCredentialProvider) GetInternal(
	ctx context.Context,
	serviceToken string,
	owner string,
	accountID int64,
) (Credentials, error) {
	response, err := p.client.GetTradingCredentialsInternal(
		ctx,
		&accountv1.GetTradingCredentialsInternalRequest{
			ServiceToken: serviceToken, OwnerUsername: owner, TradingAccountId: accountID,
		},
	)
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{
		TradingAccountID: response.GetTradingAccountId(),
		ProductName:      response.GetProductName(),
		Exchange:         response.GetExchange(),
		AccountName:      response.GetAccountName(),
		APIKey:           response.GetApiKey(),
		APISecret:        response.GetApiSecret(),
		Passphrase:       response.GetPassphrase(),
		CredentialKind:   response.GetCredentialKind(),
		SigningAddress:   response.GetSigningAddress(),
		VaultAddress:     response.GetVaultAddress(),
		AccountIndex:     response.AccountIndex,
		APIKeyIndex:      response.ApiKeyIndex,
	}, nil
}
