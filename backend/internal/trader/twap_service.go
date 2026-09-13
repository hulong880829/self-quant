package trader

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (s *Service) CreateTwap(ctx context.Context, input CreateTwapInput) (TwapJob, error) {
	if s.twaps == nil {
		return TwapJob{}, ErrNotFound
	}
	normalized, err := normalizeTwapInput(input, time.Now().UTC())
	if err != nil {
		return TwapJob{}, err
	}
	owner, err := s.credentials.Owner(ctx, normalized.Token)
	if err != nil {
		return TwapJob{}, err
	}
	account, err := s.credentials.Get(ctx, normalized.Token, normalized.TradingAccountID)
	if err != nil {
		return TwapJob{}, err
	}
	instrument, err := s.catalog.Get(ctx, normalized.InstrumentID)
	if err != nil {
		return TwapJob{}, err
	}
	if instrument.Exchange != account.Exchange {
		return TwapJob{}, ErrInvalidArgument
	}
	adapter, ok := s.venues.Adapter(account.Exchange)
	if !ok {
		return TwapJob{}, ErrUnsupportedExchange
	}
	capabilities, err := venueCapabilities(ctx, adapter, account)
	if err != nil {
		return TwapJob{}, err
	}
	if !supportsCapability(capabilities.Products, instrument.ContractType) ||
		(len(capabilities.QuoteAssets) > 0 &&
			!supportsCapability(capabilities.QuoteAssets, instrument.QuoteAsset)) ||
		(normalized.ExecutionType == "maker" && !capabilities.MakerTwap) {
		return TwapJob{}, ErrInvalidArgument
	}
	if err := validateTwapInstrument(instrument, normalized); err != nil {
		return TwapJob{}, err
	}
	jobID := uuid.NewString()
	job := TwapJob{
		ID: jobID, IdempotencyKey: normalized.IdempotencyKey,
		RequestFingerprint: twapFingerprint(normalized), OwnerUsername: owner,
		TradingAccountID: account.TradingAccountID, ProductName: account.ProductName,
		Exchange: account.Exchange, InstrumentID: instrument.ID,
		ContractType: instrument.ContractType, ExchangeSymbol: instrument.ExchangeSymbol,
		BaseAsset: instrument.BaseAsset, QuoteAsset: instrument.QuoteAsset,
		Side: normalized.Side, TotalQuantity: normalized.TotalQuantity,
		StartAt: normalized.StartAt.UTC(), EndAt: normalized.EndAt.UTC(),
		IntervalSeconds: normalized.IntervalSeconds, LimitPrice: normalized.LimitPrice,
		MaxQuantity: normalized.MaxQuantity, ExecutionType: normalized.ExecutionType,
		OrderTimeoutSeconds: normalized.OrderTimeoutSeconds, Status: "pending",
	}
	job.NextActionAt = job.StartAt
	created, inserted, err := s.twaps.CreateTwap(ctx, job)
	if err != nil {
		return TwapJob{}, err
	}
	if !inserted && created.RequestFingerprint != job.RequestFingerprint {
		return TwapJob{}, ErrIdempotencyConflict
	}
	return created, nil
}

func (s *Service) GetTwap(ctx context.Context, token, id string) (TwapJob, error) {
	if s.twaps == nil || strings.TrimSpace(token) == "" || !validUUID(id) {
		return TwapJob{}, ErrInvalidArgument
	}
	owner, err := s.credentials.Owner(ctx, token)
	if err != nil {
		return TwapJob{}, err
	}
	job, err := s.twaps.GetTwapByOwner(ctx, owner, id)
	if err != nil {
		return TwapJob{}, err
	}
	if !terminalTwapStatus(job.Status) {
		if refreshed, refreshErr := s.twaps.RefreshTwapProgress(ctx, job.ID); refreshErr == nil {
			job = refreshed
		}
	}
	return job, nil
}

func (s *Service) ListTwaps(
	ctx context.Context,
	token string,
	accountID int64,
	view string,
	status string,
	limit int,
	cursor string,
) ([]TwapJob, string, error) {
	if s.twaps == nil || strings.TrimSpace(token) == "" || accountID < 0 {
		return nil, "", ErrInvalidArgument
	}
	view = strings.ToLower(strings.TrimSpace(view))
	if view == "" {
		view = "running"
	}
	if view != "running" && view != "closed" {
		return nil, "", ErrInvalidArgument
	}
	status = strings.ToLower(strings.TrimSpace(status))
	if status != "" {
		if view == "running" && status != "pending" && status != "running" {
			return nil, "", ErrInvalidArgument
		}
		if view == "closed" && !terminalTwapStatus(status) {
			return nil, "", ErrInvalidArgument
		}
	}
	if cursor != "" && !validUUID(cursor) {
		return nil, "", ErrInvalidArgument
	}
	owner, err := s.credentials.Owner(ctx, token)
	if err != nil {
		return nil, "", err
	}
	if accountID > 0 {
		if _, err := s.credentials.Meta(ctx, token, accountID); err != nil {
			return nil, "", err
		}
	}
	return s.twaps.ListTwaps(ctx, owner, accountID, view, status, limit, cursor)
}

func (s *Service) ListTwapOrders(ctx context.Context, token, id string) ([]Order, error) {
	if s.twaps == nil || strings.TrimSpace(token) == "" || !validUUID(id) {
		return nil, ErrInvalidArgument
	}
	owner, err := s.credentials.Owner(ctx, token)
	if err != nil {
		return nil, err
	}
	if _, err := s.twaps.GetTwapByOwner(ctx, owner, id); err != nil {
		return nil, err
	}
	return s.twaps.ListTwapOrders(ctx, owner, id)
}

func (s *Service) CancelTwap(ctx context.Context, token, id string) (TwapJob, error) {
	if s.twaps == nil || strings.TrimSpace(token) == "" || !validUUID(id) {
		return TwapJob{}, ErrInvalidArgument
	}
	owner, err := s.credentials.Owner(ctx, token)
	if err != nil {
		return TwapJob{}, err
	}
	job, err := s.twaps.GetTwapByOwner(ctx, owner, id)
	if err != nil {
		return TwapJob{}, err
	}
	if terminalTwapStatus(job.Status) {
		return TwapJob{}, ErrTwapNotCancelable
	}
	closed, err := s.twaps.CloseTwap(ctx, job.ID, "canceled", "canceled by user")
	if err != nil {
		return TwapJob{}, err
	}
	_ = s.twaps.AppendTwapEvent(ctx, job.ID, "canceled", map[string]any{"by": "user"})
	if job.ActiveOrderID != "" {
		_, _ = s.CancelOrder(ctx, token, job.ActiveOrderID)
	}
	if refreshed, refreshErr := s.twaps.RefreshTwapProgress(ctx, closed.ID); refreshErr == nil {
		closed = refreshed
	}
	return closed, nil
}

func twapSliceFireAt(jobID string, slice int, start, end time.Time, intervalSeconds int) time.Time {
	sliceStart := start.Add(time.Duration(slice*intervalSeconds) * time.Second)
	if !sliceStart.Before(end) {
		return end
	}
	sliceEnd := sliceStart.Add(time.Duration(intervalSeconds) * time.Second)
	if sliceEnd.After(end) {
		sliceEnd = end
	}
	window := sliceEnd.Sub(sliceStart)
	if window <= 0 {
		return sliceStart
	}
	sum := sha256.Sum256([]byte(jobID + ":" + itoa(int64(slice))))
	offset := time.Duration(binary.BigEndian.Uint64(sum[:8]) % uint64(window))
	return sliceStart.Add(offset)
}

func validUUID(value string) bool {
	_, err := uuid.Parse(strings.TrimSpace(value))
	return err == nil
}
