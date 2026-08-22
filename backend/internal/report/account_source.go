package report

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	accountv1 "selfquant/backend/gen/account/v1"
)

type AccountSource interface {
	SampleProduct(context.Context, Product) ([]AccountEquity, error)
	SyncProductTradeFills(context.Context, Product, time.Time) error
}

type GRPCAccountSource struct {
	client accountv1.AccountServiceClient
	token  string
}

func NewGRPCAccountSource(
	client accountv1.AccountServiceClient,
	token string,
) (*GRPCAccountSource, error) {
	token = strings.TrimSpace(token)
	if client == nil || token == "" {
		return nil, ErrSourceDisabled
	}
	return &GRPCAccountSource{
		client: client,
		token:  token,
	}, nil
}

func (s *GRPCAccountSource) SampleProduct(
	ctx context.Context,
	product Product,
) ([]AccountEquity, error) {
	response, err := s.client.GetProductAccountSnapshotsInternal(
		ctx,
		&accountv1.GetProductAccountSnapshotsInternalRequest{
			ServiceToken:  s.token,
			OwnerUsername: product.OwnerUsername,
			ProductName:   product.Name,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("load internal product snapshots: %w", err)
	}
	items := make([]AccountEquity, 0, len(response.GetSnapshots()))
	for _, value := range response.GetSnapshots() {
		sourceUpdatedAt := time.Now().UTC()
		if value.GetSourceUpdatedAt() != nil && value.GetSourceUpdatedAt().IsValid() {
			sourceUpdatedAt = value.GetSourceUpdatedAt().AsTime().UTC()
		}
		items = append(items, AccountEquity{
			TradingAccountID:  value.GetTradingAccountId(),
			EquityUSD:         value.GetAccountEquityUsd(),
			AvailableFundsUSD: value.GetAvailableFundsUsd(),
			SourceUpdatedAt:   sourceUpdatedAt,
		})
	}
	if len(items) == 0 && len(response.GetErrors()) > 0 {
		return items, fmt.Errorf("partial product snapshots: %s", strings.Join(response.GetErrors(), "; "))
	}
	return items, nil
}

func (s *GRPCAccountSource) SyncProductTradeFills(
	ctx context.Context,
	product Product,
	through time.Time,
) error {
	response, err := s.client.SyncProductTradeFillsInternal(
		ctx,
		&accountv1.SyncProductTradeFillsInternalRequest{
			ServiceToken:  s.token,
			OwnerUsername: product.OwnerUsername,
			ProductName:   product.Name,
			ThroughTime:   timestamppb.New(through.UTC()),
		},
	)
	if err != nil {
		return fmt.Errorf("sync internal product trade fills: %w", err)
	}
	var syncErrors []string
	for _, result := range response.GetResults() {
		if result.GetError() != "" {
			syncErrors = append(syncErrors, fmt.Sprintf(
				"%s account %d: %s",
				result.GetExchange(), result.GetTradingAccountId(), result.GetError(),
			))
		}
	}
	if len(syncErrors) > 0 {
		return fmt.Errorf("partial trade fill sync: %s", strings.Join(syncErrors, "; "))
	}
	return nil
}
