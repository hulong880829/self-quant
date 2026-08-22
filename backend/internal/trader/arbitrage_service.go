package trader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func (s *Service) CreateArbitrageCombination(
	ctx context.Context,
	input CreateArbitrageInput,
) (ArbitrageCombination, error) {
	if s.arbitrage == nil {
		return ArbitrageCombination{}, ErrNotFound
	}
	normalized, err := normalizeArbitrageInput(input)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	owner, err := s.credentials.Owner(ctx, normalized.Token)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	accountA, err := s.credentials.Get(ctx, normalized.Token, normalized.LegATradingAccountID)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	accountB, err := s.credentials.Get(ctx, normalized.Token, normalized.LegBTradingAccountID)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	if accountA.TradingAccountID == accountB.TradingAccountID ||
		accountA.Exchange == accountB.Exchange ||
		accountA.ProductName != accountB.ProductName {
		return ArbitrageCombination{}, ErrInvalidArgument
	}
	instrumentA, err := s.catalog.Get(ctx, normalized.LegAInstrumentID)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	instrumentB, err := s.catalog.Get(ctx, normalized.LegBInstrumentID)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	if instrumentA.Exchange != accountA.Exchange || instrumentB.Exchange != accountB.Exchange ||
		instrumentA.BaseAsset != instrumentB.BaseAsset ||
		instrumentA.QuoteAsset != instrumentB.QuoteAsset {
		return ArbitrageCombination{}, ErrInvalidArgument
	}
	if _, ok := s.venues.Adapter(accountA.Exchange); !ok {
		return ArbitrageCombination{}, ErrUnsupportedExchange
	}
	if _, ok := s.venues.Adapter(accountB.Exchange); !ok {
		return ArbitrageCombination{}, ErrUnsupportedExchange
	}
	item := ArbitrageCombination{
		ID: uuid.NewString(), IdempotencyKey: normalized.IdempotencyKey,
		OwnerUsername: owner, AskThresholdBps: normalized.AskThresholdBps,
		BidThresholdBps: normalized.BidThresholdBps,
		TargetNotional:  normalized.TargetNotional, OrderNotional: normalized.OrderNotional,
		MaxDeltaNotional: normalized.MaxDeltaNotional, ExecutionMode: normalized.ExecutionMode,
		MakerLeg: normalized.MakerLeg, Status: "running", CompletedNotional: "0",
		MarketDataStale: true,
		LegA:            arbitrageLeg(accountA, instrumentA),
		LegB:            arbitrageLeg(accountB, instrumentB),
	}
	item.RequestFingerprint = arbitrageFingerprint(item)
	created, inserted, err := s.arbitrage.CreateArbitrageCombination(ctx, item)
	if err != nil {
		return ArbitrageCombination{}, fmt.Errorf("%w: create arbitrage combination: %w", ErrPersistence, err)
	}
	if !inserted && created.RequestFingerprint != item.RequestFingerprint {
		return ArbitrageCombination{}, ErrIdempotencyConflict
	}
	return created, nil
}

func (s *Service) GetArbitrageCombination(
	ctx context.Context,
	token, id string,
) (ArbitrageCombination, []Order, []ArbitrageExecution, []ArbitrageEvent, error) {
	if s.arbitrage == nil || strings.TrimSpace(token) == "" || !validUUID(id) {
		return ArbitrageCombination{}, nil, nil, nil, ErrInvalidArgument
	}
	owner, err := s.credentials.Owner(ctx, token)
	if err != nil {
		return ArbitrageCombination{}, nil, nil, nil, err
	}
	item, err := s.arbitrage.GetArbitrageCombinationByOwner(ctx, owner, id)
	if err != nil {
		return ArbitrageCombination{}, nil, nil, nil, err
	}
	orders, err := s.arbitrage.ListArbitrageOrders(ctx, owner, id)
	if err != nil {
		return ArbitrageCombination{}, nil, nil, nil, err
	}
	executions, err := s.arbitrage.ListArbitrageExecutions(ctx, id, 20)
	if err != nil {
		return ArbitrageCombination{}, nil, nil, nil, err
	}
	events, err := s.arbitrage.ListArbitrageEvents(ctx, id, 50)
	if err != nil {
		return ArbitrageCombination{}, nil, nil, nil, err
	}
	return item, orders, executions, events, nil
}

func (s *Service) ListArbitrageCombinations(
	ctx context.Context,
	token, view string,
	limit int,
	cursor string,
) ([]ArbitrageCombination, string, int64, error) {
	if s.arbitrage == nil || strings.TrimSpace(token) == "" {
		return nil, "", 0, ErrInvalidArgument
	}
	view = strings.ToLower(strings.TrimSpace(view))
	if view == "" {
		view = "running"
	}
	if view != "running" && view != "closed" {
		return nil, "", 0, ErrInvalidArgument
	}
	if cursor != "" && !validUUID(cursor) {
		return nil, "", 0, ErrInvalidArgument
	}
	owner, err := s.credentials.Owner(ctx, token)
	if err != nil {
		return nil, "", 0, err
	}
	items, next, err := s.arbitrage.ListArbitrageCombinations(ctx, owner, view, limit, cursor)
	if err != nil {
		return nil, "", 0, err
	}
	total, err := s.arbitrage.CountArbitrageCombinations(ctx, owner, view)
	if err != nil {
		return nil, "", 0, err
	}
	return items, next, total, nil
}

func (s *Service) CloseArbitrageCombination(
	ctx context.Context,
	token, id string,
) (ArbitrageCombination, error) {
	if s.arbitrage == nil || strings.TrimSpace(token) == "" || !validUUID(id) {
		return ArbitrageCombination{}, ErrInvalidArgument
	}
	owner, err := s.credentials.Owner(ctx, token)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	item, err := s.arbitrage.GetArbitrageCombinationByOwner(ctx, owner, id)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	if item.Status == "closed" {
		return item, nil
	}
	if item.Status != "running" && item.Status != "failed" && item.Status != "closing" {
		return ArbitrageCombination{}, ErrArbitrageNotClosable
	}
	if item.Status == "closing" {
		return item, nil
	}
	return s.arbitrage.MarkArbitrageClosing(ctx, owner, id)
}

func normalizeArbitrageInput(input CreateArbitrageInput) (CreateArbitrageInput, error) {
	input.Token = strings.TrimSpace(input.Token)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	input.ExecutionMode = strings.ToLower(strings.TrimSpace(input.ExecutionMode))
	input.MakerLeg = strings.ToLower(strings.TrimSpace(input.MakerLeg))
	if input.Token == "" || len(input.IdempotencyKey) < 8 ||
		input.LegATradingAccountID <= 0 || input.LegBTradingAccountID <= 0 ||
		input.LegAInstrumentID <= 0 || input.LegBInstrumentID <= 0 {
		return CreateArbitrageInput{}, ErrInvalidArgument
	}
	if input.ExecutionMode != "maker_then_hedge" && input.ExecutionMode != "simultaneous_market" {
		return CreateArbitrageInput{}, ErrInvalidArgument
	}
	if input.MakerLeg == "" {
		input.MakerLeg = "a"
	}
	if input.MakerLeg != "a" && input.MakerLeg != "b" {
		return CreateArbitrageInput{}, ErrInvalidArgument
	}
	ask, askErr := decimal.NewFromString(strings.TrimSpace(input.AskThresholdBps))
	bid, bidErr := decimal.NewFromString(strings.TrimSpace(input.BidThresholdBps))
	target, targetErr := decimal.NewFromString(strings.TrimSpace(input.TargetNotional))
	order, orderErr := decimal.NewFromString(strings.TrimSpace(input.OrderNotional))
	if strings.TrimSpace(input.MaxDeltaNotional) == "" {
		input.MaxDeltaNotional = strings.TrimSpace(input.OrderNotional)
	}
	maxDelta, deltaErr := decimal.NewFromString(strings.TrimSpace(input.MaxDeltaNotional))
	if askErr != nil || bidErr != nil || targetErr != nil || orderErr != nil || deltaErr != nil ||
		!ask.IsPositive() || !bid.IsNegative() || !target.IsPositive() || !order.IsPositive() ||
		!maxDelta.IsPositive() || order.GreaterThan(target) || maxDelta.GreaterThan(order) {
		return CreateArbitrageInput{}, ErrInvalidArgument
	}
	input.AskThresholdBps = ask.String()
	input.BidThresholdBps = bid.String()
	input.TargetNotional = target.String()
	input.OrderNotional = order.String()
	input.MaxDeltaNotional = maxDelta.String()
	return input, nil
}

func arbitrageLeg(account Credentials, instrument Instrument) ArbitrageLeg {
	return ArbitrageLeg{
		TradingAccountID: account.TradingAccountID, InstrumentID: instrument.ID,
		ProductName: account.ProductName, AccountName: account.AccountName,
		Exchange: account.Exchange, ContractType: instrument.ContractType,
		ExchangeSymbol: instrument.ExchangeSymbol, BaseAsset: instrument.BaseAsset,
		QuoteAsset: instrument.QuoteAsset,
	}
}

func arbitrageFingerprint(item ArbitrageCombination) string {
	values := []string{
		strconv.FormatInt(item.LegA.TradingAccountID, 10),
		strconv.FormatInt(item.LegA.InstrumentID, 10),
		strconv.FormatInt(item.LegB.TradingAccountID, 10),
		strconv.FormatInt(item.LegB.InstrumentID, 10),
		item.AskThresholdBps, item.BidThresholdBps, item.TargetNotional,
		item.OrderNotional, item.MaxDeltaNotional, item.ExecutionMode, item.MakerLeg,
	}
	sum := sha256.Sum256([]byte(strings.Join(values, "|")))
	return hex.EncodeToString(sum[:])
}
