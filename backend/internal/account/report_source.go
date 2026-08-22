package account

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/account/portfolio"
	"selfquant/backend/internal/polymarket"
)

var ErrInvalidServiceToken = errors.New("invalid service token")

type ProductAccountSnapshots struct {
	Snapshots []TradingAccountSnapshot
	Errors    []string
}

type TradeFillSyncResult struct {
	TradingAccountID int64
	Exchange         string
	InsertedCount    int
	SyncedThrough    time.Time
	Error            string
}

func (s *Service) authorizeInternal(token string) error {
	expected, provided := []byte(s.internalToken), []byte(strings.TrimSpace(token))
	if len(expected) == 0 || len(expected) != len(provided) ||
		subtle.ConstantTimeCompare(expected, provided) != 1 {
		return ErrInvalidServiceToken
	}
	return nil
}

func (s *Service) GetProductAccountSnapshotsInternal(
	ctx context.Context,
	serviceToken, owner, product string,
) (ProductAccountSnapshots, error) {
	if err := s.authorizeInternal(serviceToken); err != nil {
		return ProductAccountSnapshots{}, err
	}
	owner, product = strings.TrimSpace(owner), strings.TrimSpace(product)
	if owner == "" || product == "" || s.trading == nil {
		return ProductAccountSnapshots{}, ErrInvalidTradingAccount
	}
	records, err := s.trading.ListByOwnerProduct(ctx, owner, product)
	if err != nil {
		return ProductAccountSnapshots{}, err
	}
	result := ProductAccountSnapshots{
		Snapshots: make([]TradingAccountSnapshot, 0, len(records)),
	}
	for _, record := range records {
		snapshot, snapshotErr := s.snapshotForRecord(ctx, owner, record)
		if snapshotErr != nil {
			result.Errors = append(result.Errors, record.AccountName+": snapshot unavailable")
			continue
		}
		result.Snapshots = append(result.Snapshots, snapshot)
	}
	return result, nil
}

func (s *Service) SyncProductTradeFillsInternal(
	ctx context.Context,
	serviceToken, owner, product string,
	through time.Time,
) ([]TradeFillSyncResult, error) {
	if err := s.authorizeInternal(serviceToken); err != nil {
		return nil, err
	}
	owner, product = strings.TrimSpace(owner), strings.TrimSpace(product)
	if owner == "" || product == "" || s.trading == nil || s.tradeFills == nil ||
		s.cipher == nil {
		return nil, ErrInvalidTradingAccount
	}
	if through.IsZero() || through.After(time.Now().Add(time.Minute)) {
		through = time.Now().UTC()
	} else {
		through = through.UTC()
	}
	records, err := s.trading.ListByOwnerProduct(ctx, owner, product)
	if err != nil {
		return nil, err
	}
	results := make([]TradeFillSyncResult, 0, len(records))
	for _, record := range records {
		item := TradeFillSyncResult{
			TradingAccountID: record.ID, Exchange: record.Exchange,
		}
		item.InsertedCount, err = s.syncRecordTradeFills(ctx, owner, record, through)
		if err != nil {
			item.Error = err.Error()
			_ = s.tradeFills.SaveSyncError(ctx, record.ID, item.Error)
		} else {
			item.SyncedThrough = through
		}
		results = append(results, item)
	}
	return results, nil
}

func (s *Service) syncRecordTradeFills(
	ctx context.Context,
	owner string,
	record TradingAccountRecord,
	through time.Time,
) (int, error) {
	cursor, err := s.tradeFills.LoadCursor(ctx, record.ID)
	if err != nil {
		return 0, err
	}
	if cursor.IsZero() {
		cursor = through.Add(-7 * 24 * time.Hour)
	} else {
		// Overlap catches fills that become visible shortly after execution. The
		// database uniqueness key makes this safe on every retry.
		cursor = cursor.Add(-5 * time.Minute)
	}
	if !cursor.Before(through) {
		return 0, nil
	}
	fills, err := s.loadRecordTradeFills(ctx, owner, record, cursor, through)
	if err != nil {
		return 0, err
	}
	return s.tradeFills.SaveFills(ctx, record.ID, record.Exchange, fills, through)
}

func (s *Service) loadRecordTradeFills(
	ctx context.Context,
	owner string,
	record TradingAccountRecord,
	since, until time.Time,
) ([]portfolio.TradeFill, error) {
	if strings.EqualFold(record.Exchange, "polymarket") {
		if s.polymarket == nil || s.polyCLOB == nil {
			return nil, ErrSnapshotUnavailable
		}
		stored, err := s.polymarket.GetByOwner(ctx, owner, record.ID)
		if err != nil {
			return nil, err
		}
		credentials, err := s.decryptPolymarketCredentials(stored)
		if err != nil {
			return nil, err
		}
		trades, err := s.polyCLOB.ListTrades(ctx, credentials, since, until)
		if err != nil {
			return nil, fmt.Errorf("polymarket CLOB trades: %w", err)
		}
		result := make([]portfolio.TradeFill, 0, len(trades))
		for _, trade := range trades {
			price, priceErr := decimal.NewFromString(trade.Price)
			size, sizeErr := decimal.NewFromString(trade.Size)
			if priceErr != nil || sizeErr != nil || !price.IsPositive() || size.IsZero() {
				continue
			}
			symbol := trade.Market
			if symbol == "" {
				symbol = trade.AssetID
			}
			result = append(result, portfolio.TradeFill{
				ExternalTradeID: trade.ID, OrderID: trade.OrderID, Symbol: symbol,
				Side: trade.Side, Price: price.String(), Quantity: size.Abs().String(),
				QuoteNotionalUSD: price.Mul(size.Abs()).String(), Fee: trade.Fee,
				FeeCurrency: "USDC", TradedAt: trade.MatchedAt,
			})
		}
		return result, nil
	}
	if s.portfolios == nil {
		return nil, portfolio.ErrUnsupported
	}
	apiKey, err := s.cipher.Decrypt(record.APIKeyEnc)
	if err != nil {
		return nil, err
	}
	secret, err := s.cipher.Decrypt(record.APISecretEnc)
	if err != nil {
		return nil, err
	}
	passphrase := ""
	if len(record.PassphraseEnc) > 0 {
		passphrase, err = s.cipher.Decrypt(record.PassphraseEnc)
		if err != nil {
			return nil, err
		}
	}
	return s.portfolios.TradeFills(ctx, record.Exchange, portfolio.Credentials{
		APIKey: apiKey, APISecret: secret, Passphrase: passphrase,
	}, portfolio.TradeQuery{Since: since, Until: until})
}

func (s *Service) decryptPolymarketCredentials(
	stored PolymarketCredentialRecord,
) (polymarket.Credentials, error) {
	apiKey, err := s.cipher.Decrypt(stored.APIKeyEnc)
	if err != nil {
		return polymarket.Credentials{}, err
	}
	secret, err := s.cipher.Decrypt(stored.APISecretEnc)
	if err != nil {
		return polymarket.Credentials{}, err
	}
	passphrase, err := s.cipher.Decrypt(stored.PassphraseEnc)
	if err != nil {
		return polymarket.Credentials{}, err
	}
	return polymarket.Credentials{
		SignerAddress: stored.SignerAddress, FunderAddress: stored.FunderAddress,
		APIKey: apiKey, APISecret: secret, Passphrase: passphrase,
		SignatureType: stored.SignatureType,
	}, nil
}
