package admin

import (
	"context"

	accountv1 "selfquant/backend/gen/account/v1"
	"selfquant/backend/internal/account"
)

type AccountSummaryClient struct {
	client accountv1.AccountServiceClient
}

func NewAccountSummaryClient(client accountv1.AccountServiceClient) *AccountSummaryClient {
	return &AccountSummaryClient{client: client}
}

func (c *AccountSummaryClient) GetAdminAccountSummaryInternal(
	ctx context.Context, serviceToken string,
) ([]account.OwnerAccountRow, error) {
	response, err := c.client.GetAdminAccountSummaryInternal(ctx, &accountv1.GetAdminAccountSummaryInternalRequest{
		ServiceToken: serviceToken,
	})
	if err != nil {
		return nil, err
	}
	rows := make([]account.OwnerAccountRow, 0, len(response.GetOwners()))
	for _, owner := range response.GetOwners() {
		rows = append(rows, account.OwnerAccountRow{
			Username: owner.GetUsername(), BrokerID: owner.GetBrokerId(),
			Status: owner.GetStatus(), Permission: owner.GetPermission(),
		})
	}
	return rows, nil
}
