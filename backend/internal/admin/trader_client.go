package admin

import (
	"context"

	traderv1 "selfquant/backend/gen/trader/v1"
)

type TraderCountClient struct {
	client traderv1.TraderServiceClient
}

func NewTraderCountClient(client traderv1.TraderServiceClient) *TraderCountClient {
	return &TraderCountClient{client: client}
}

func (c *TraderCountClient) GetArbitrageCountsByOwnerInternal(
	ctx context.Context, serviceToken string,
) ([]ArbitrageCount, error) {
	response, err := c.client.GetArbitrageCountsByOwnerInternal(ctx, &traderv1.GetArbitrageCountsByOwnerInternalRequest{
		ServiceToken: serviceToken,
	})
	if err != nil {
		return nil, err
	}
	rows := make([]ArbitrageCount, 0, len(response.GetItems()))
	for _, item := range response.GetItems() {
		rows = append(rows, ArbitrageCount{
			OwnerUsername: item.GetOwnerUsername(),
			Running:       item.GetRunning(),
			Closing:       item.GetClosing(),
			Closed:        item.GetClosed(),
			Failed:        item.GetFailed(),
		})
	}
	return rows, nil
}
