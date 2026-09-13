package report

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	accountv1 "selfquant/backend/gen/account/v1"
)

type Service struct {
	repository *Repository
	accounts   accountv1.AccountServiceClient
	location   *time.Location
	now        func() time.Time
}

func NewService(
	repository *Repository,
	accounts accountv1.AccountServiceClient,
	location *time.Location,
) *Service {
	return &Service{
		repository: repository,
		accounts:   accounts,
		location:   location,
		now:        time.Now,
	}
}

func (s *Service) authenticate(ctx context.Context, token string) (string, error) {
	if strings.TrimSpace(token) == "" || s.accounts == nil {
		return "", ErrUnauthenticated
	}
	response, err := s.accounts.ValidateSession(ctx, &accountv1.ValidateSessionRequest{Token: token})
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	if strings.TrimSpace(response.GetUsername()) == "" {
		return "", ErrUnauthenticated
	}
	return response.GetUsername(), nil
}

func (s *Service) ListProducts(ctx context.Context, token string) ([]Product, error) {
	owner, err := s.authenticate(ctx, token)
	if err != nil {
		return nil, err
	}
	return s.repository.ListProducts(ctx, owner)
}

func (s *Service) ProductDetail(
	ctx context.Context,
	token string,
	productID int64,
) (ProductDetail, error) {
	if productID <= 0 {
		return ProductDetail{}, ErrInvalidInput
	}
	owner, err := s.authenticate(ctx, token)
	if err != nil {
		return ProductDetail{}, err
	}
	detail, err := s.repository.ProductDetail(ctx, owner, productID)
	if err != nil {
		return ProductDetail{}, err
	}
	detail.Daily = CalculatePerformanceMetrics(detail.Daily)
	if len(detail.Daily) > 0 {
		detail.LatestSnapshot = &detail.Daily[0]
	}
	return detail, nil
}

func validateRange(value DateRange) (DateRange, error) {
	for _, item := range []string{value.From, value.To} {
		if item == "" {
			continue
		}
		if _, err := time.Parse(time.DateOnly, item); err != nil {
			return DateRange{}, ErrInvalidInput
		}
	}
	if value.From != "" && value.To != "" && value.From > value.To {
		return DateRange{}, ErrInvalidInput
	}
	return normalizeRange(value), nil
}

func (s *Service) ListDailySnapshots(
	ctx context.Context,
	token string,
	productID int64,
	dateRange DateRange,
) ([]DailySnapshot, error) {
	dateRange, err := validateRange(dateRange)
	if err != nil || productID <= 0 {
		return nil, ErrInvalidInput
	}
	owner, err := s.authenticate(ctx, token)
	if err != nil {
		return nil, err
	}
	if _, err := s.repository.GetProduct(ctx, owner, productID); err != nil {
		return nil, err
	}
	items, err := s.repository.ListDailySnapshots(ctx, owner, productID, dateRange)
	if err != nil {
		return nil, err
	}
	return CalculatePerformanceMetrics(items), nil
}

func (s *Service) ListCashFlows(
	ctx context.Context,
	token string,
	productID int64,
	dateRange DateRange,
) ([]CashFlow, error) {
	dateRange, err := validateRange(dateRange)
	if err != nil || productID <= 0 {
		return nil, ErrInvalidInput
	}
	owner, err := s.authenticate(ctx, token)
	if err != nil {
		return nil, err
	}
	if _, err := s.repository.GetProduct(ctx, owner, productID); err != nil {
		return nil, err
	}
	return s.repository.ListCashFlows(ctx, owner, productID, dateRange)
}

func (s *Service) CreateCashFlow(
	ctx context.Context,
	token string,
	productID int64,
	_ string,
	occurredAt time.Time,
	amount string,
	flowType string,
	note string,
	confirmed bool,
) (CashFlow, error) {
	if productID <= 0 || !confirmed {
		return CashFlow{}, fmt.Errorf("%w: manual cash flows must be confirmed", ErrInvalidInput)
	}
	if occurredAt.IsZero() {
		return CashFlow{}, fmt.Errorf("%w: occurredAt is required", ErrInvalidInput)
	}
	flowDate := ReportDate(occurredAt)
	value, err := decimal.NewFromString(strings.TrimSpace(amount))
	if err != nil || value.IsZero() {
		return CashFlow{}, fmt.Errorf("%w: amount must be a non-zero decimal string", ErrInvalidInput)
	}
	flowType = strings.ToLower(strings.TrimSpace(flowType))
	switch flowType {
	case "subscription", "deposit":
		value = value.Abs()
	case "redemption", "withdrawal":
		value = value.Abs().Neg()
	case "transfer", "adjustment":
	default:
		return CashFlow{}, fmt.Errorf("%w: invalid cash flow type", ErrInvalidInput)
	}
	owner, err := s.authenticate(ctx, token)
	if err != nil {
		return CashFlow{}, err
	}
	item, err := s.repository.CreateCashFlow(
		ctx, owner, productID, flowDate, occurredAt.UTC(), value.String(), flowType,
		strings.TrimSpace(note), true, DefaultIdempotencyKey(flowType, occurredAt, value.String()),
	)
	if err != nil {
		return CashFlow{}, err
	}
	exists, existsErr := s.repository.SnapshotExists(ctx, productID, item.FlowDate)
	if existsErr != nil {
		return CashFlow{}, existsErr
	}
	if exists {
		if enqueueErr := s.repository.EnqueueRecompute(ctx, productID, item.FlowDate); enqueueErr != nil {
			return CashFlow{}, enqueueErr
		}
		item.RecomputeStatus = "queued"
	} else {
		item.RecomputeStatus = "none"
	}
	return item, nil
}

func (s *Service) Recompute(
	ctx context.Context,
	token string,
	productID int64,
	from string,
	to string,
) (int, error) {
	dateRange, err := validateRange(DateRange{From: from, To: to})
	if err != nil || dateRange.From == "" || dateRange.To == "" || productID <= 0 {
		return 0, ErrInvalidInput
	}
	owner, err := s.authenticate(ctx, token)
	if err != nil {
		return 0, err
	}
	if _, err := s.repository.GetProduct(ctx, owner, productID); err != nil {
		return 0, err
	}
	start, _ := time.ParseInLocation(time.DateOnly, from, s.location)
	end, _ := time.ParseInLocation(time.DateOnly, to, s.location)
	if end.Sub(start) > 10*366*24*time.Hour {
		return 0, fmt.Errorf("%w: date range is too large", ErrInvalidInput)
	}
	written := 0
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		ok, finalizeErr := s.repository.FinalizeDate(
			ctx, productID, day, s.location, s.now().UTC(),
		)
		if finalizeErr != nil {
			return written, finalizeErr
		}
		if ok {
			written++
		}
	}
	return written, nil
}

func IsUnauthenticated(err error) bool {
	return errors.Is(err, ErrUnauthenticated)
}

func IsDuplicate(err error) bool {
	return errors.Is(err, ErrDuplicate)
}
