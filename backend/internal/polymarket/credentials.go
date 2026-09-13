package polymarket

import (
	"context"

	accountv1 "selfquant/backend/gen/account/v1"
)

type AccountCredentialProvider struct {
	client accountv1.AccountServiceClient
}

func NewAccountCredentialProvider(client accountv1.AccountServiceClient) *AccountCredentialProvider {
	return &AccountCredentialProvider{client: client}
}

func (p *AccountCredentialProvider) Get(
	ctx context.Context,
	token string,
	accountID int64,
) (Credentials, string, error) {
	response, err := p.client.GetPolymarketCredentials(
		ctx,
		&accountv1.GetPolymarketCredentialsRequest{
			Token: token, TradingAccountId: accountID,
		},
	)
	if err != nil {
		return Credentials{}, "", err
	}
	return credentialsFromResponse(response), response.GetAccountName(), nil
}

func (p *AccountCredentialProvider) Refresh(
	ctx context.Context,
	token string,
	accountID int64,
) (Credentials, string, error) {
	response, err := p.client.RefreshPolymarketCredentials(
		ctx,
		&accountv1.GetPolymarketCredentialsRequest{
			Token: token, TradingAccountId: accountID,
		},
	)
	if err != nil {
		return Credentials{}, "", err
	}
	return credentialsFromResponse(response), response.GetAccountName(), nil
}

func (p *AccountCredentialProvider) Invalidate(
	ctx context.Context,
	token string,
	accountID int64,
) error {
	_, err := p.client.InvalidatePolymarketCredentials(
		ctx,
		&accountv1.GetPolymarketCredentialsRequest{
			Token: token, TradingAccountId: accountID,
		},
	)
	return err
}

func (p *AccountCredentialProvider) Activate(
	ctx context.Context,
	token string,
	accountID int64,
) error {
	_, err := p.client.ActivatePolymarketCredentials(
		ctx,
		&accountv1.GetPolymarketCredentialsRequest{
			Token: token, TradingAccountId: accountID,
		},
	)
	return err
}

func (p *AccountCredentialProvider) Owner(
	ctx context.Context,
	token string,
) (string, error) {
	response, err := p.client.ValidateSession(
		ctx, &accountv1.ValidateSessionRequest{Token: token},
	)
	if err != nil {
		return "", err
	}
	return response.GetUsername(), nil
}

func credentialsFromResponse(response *accountv1.GetPolymarketCredentialsResponse) Credentials {
	return Credentials{
		SignerAddress: response.GetSignerAddress(),
		FunderAddress: response.GetFunderAddress(),
		PrivateKey:    response.GetPrivateKey(),
		APIKey:        response.GetApiKey(),
		APISecret:     response.GetApiSecret(),
		Passphrase:    response.GetPassphrase(),
		SignatureType: response.GetSignatureType(),
	}
}
