package trader

import (
	"context"
	"fmt"
	"strings"

	"selfquant/backend/internal/trader/exchange"
)

type AccountProfileStepResult struct {
	Step    string
	Status  string
	Code    string
	Message string
}

type AccountProfileResult struct {
	TradingAccountID int64
	ProductName      string
	AccountName      string
	Exchange         string
	OverallStatus    string
	Steps            []AccountProfileStepResult
}

func (s *Service) ApplyAccountProfile(
	ctx context.Context,
	token string,
	accountID int64,
) (AccountProfileResult, error) {
	token = strings.TrimSpace(token)
	if token == "" || accountID <= 0 {
		return AccountProfileResult{}, ErrInvalidArgument
	}
	owner, err := s.credentials.Owner(ctx, token)
	if err != nil {
		return AccountProfileResult{}, err
	}
	account, err := s.credentials.Get(ctx, token, accountID)
	if err != nil {
		return AccountProfileResult{}, err
	}
	venue := strings.ToLower(strings.TrimSpace(account.Exchange))
	switch venue {
	case "binance", "okx", "bybit", "bitget", "gate":
	default:
		return AccountProfileResult{}, ErrUnsupportedExchange
	}
	adapter, ok := s.venues.Adapter(venue)
	if !ok {
		return AccountProfileResult{}, ErrUnsupportedExchange
	}
	manager, ok := adapter.(exchange.AccountProfileManager)
	if !ok {
		return AccountProfileResult{}, ErrUnsupportedExchange
	}
	instruments, err := s.catalog.List(ctx, venue, "perpetual")
	if err != nil {
		return AccountProfileResult{}, fmt.Errorf("%w: load account profile instruments: %v", ErrPersistence, err)
	}
	venueInstruments := make([]exchange.Instrument, 0, len(instruments))
	for _, instrument := range instruments {
		venueInstruments = append(venueInstruments, toVenueInstrument(instrument))
	}

	lock := s.accountLock(account.TradingAccountID)
	lock.Lock()
	defer lock.Unlock()
	applyCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	applied, err := manager.ApplyAccountProfile(
		applyCtx,
		exchange.Credentials{
			APIKey: account.APIKey, APISecret: account.APISecret,
			Passphrase: account.Passphrase,
		},
		exchange.AccountProfileRequest{Instruments: venueInstruments},
	)
	if err != nil {
		return AccountProfileResult{}, fmt.Errorf("%w: apply %s account profile: %v", ErrVenueUnavailable, venue, err)
	}
	result := AccountProfileResult{
		TradingAccountID: account.TradingAccountID,
		ProductName:      account.ProductName,
		AccountName:      account.AccountName,
		Exchange:         venue,
		OverallStatus:    applied.OverallStatus,
		Steps:            make([]AccountProfileStepResult, 0, len(applied.Steps)),
	}
	for _, step := range applied.Steps {
		mapped := AccountProfileStepResult{
			Step: step.Step, Status: step.Status,
			Code: step.Code, Message: step.Message,
		}
		result.Steps = append(result.Steps, mapped)
		s.logger.Info(
			"trader account profile step",
			"owner", owner,
			"account_id", account.TradingAccountID,
			"exchange", venue,
			"step", mapped.Step,
			"status", mapped.Status,
			"code", mapped.Code,
		)
	}
	return result, nil
}
