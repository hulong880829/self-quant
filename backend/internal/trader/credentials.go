package trader

import (
	"context"

	accountv1 "selfquant/backend/gen/account/v1"
)

type credentialProvider interface {
	Owner(context.Context, string) (string, error)
	Get(context.Context, string, int64) (Credentials, error)
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
	}, nil
}
