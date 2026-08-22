package account

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/account/portfolio"
	"selfquant/backend/internal/polymarket"
)

var ErrSnapshotUnavailable = errors.New("account snapshot unavailable")

type TradingAccountSnapshot struct {
	TradingAccountID                   int64
	ProductName, Exchange, AccountName string
	AccountEquityUSD                   string
	AvailableFundsUSD, RiskPercent     string
	Positions                          []portfolio.Position
	SpotBalances                       map[string]string
	SourceUpdatedAt                    time.Time
	Stale                              bool
	LastError                          string
}

type ProductGroupPosition struct {
	Symbol, Side, TotalNotionalUSD string
	SpotSize, ContractSize         string
}

type ProductGroupSnapshot struct {
	ProductName       string
	AccountCount      int
	AccountEquityUSD  string
	AvailableFundsUSD string
	Positions         []ProductGroupPosition
	SourceUpdatedAt   time.Time
	Stale, Partial    bool
	Errors            []string
}

type snapshotCacheEntry struct {
	value     TradingAccountSnapshot
	expiresAt time.Time
}

func (s *Service) WithSnapshots(
	registry *portfolio.Registry,
	data *polymarket.DataClient,
	clob *polymarket.CLOBClient,
	ttl time.Duration,
) *Service {
	if ttl <= 0 {
		ttl = 3 * time.Second
	}
	s.portfolios, s.polyData, s.polyCLOB, s.snapshotTTL = registry, data, clob, ttl
	s.snapshotCache = make(map[string]snapshotCacheEntry)
	return s
}

func (s *Service) GetTradingAccountSnapshot(ctx context.Context, token string, id int64) (TradingAccountSnapshot, error) {
	session, err := s.ValidateSession(token)
	if err != nil {
		return TradingAccountSnapshot{}, err
	}
	if id <= 0 || s.trading == nil || s.cipher == nil {
		return TradingAccountSnapshot{}, ErrInvalidTradingAccount
	}
	record, err := s.trading.GetByOwner(ctx, session.Username, id)
	if err != nil {
		return TradingAccountSnapshot{}, err
	}
	return s.snapshotForRecord(ctx, session.Username, record)
}

func (s *Service) snapshotForRecord(ctx context.Context, owner string, record TradingAccountRecord) (TradingAccountSnapshot, error) {
	key := fmt.Sprintf("%s:%d", owner, record.ID)
	now := time.Now()
	s.snapshotMu.RLock()
	cached, ok := s.snapshotCache[key]
	s.snapshotMu.RUnlock()
	if ok && now.Before(cached.expiresAt) {
		return cloneSnapshot(cached.value), nil
	}

	value, err, _ := s.snapshotGroup.Do(key, func() (any, error) {
		s.snapshotMu.RLock()
		latest, fresh := s.snapshotCache[key]
		s.snapshotMu.RUnlock()
		if fresh && time.Now().Before(latest.expiresAt) {
			return cloneSnapshot(latest.value), nil
		}
		loaded, loadErr := s.loadSnapshot(ctx, owner, record)
		if loadErr != nil {
			if fresh {
				fallback := cloneSnapshot(latest.value)
				fallback.Stale, fallback.LastError = true, "upstream snapshot refresh failed"
				return fallback, nil
			}
			return TradingAccountSnapshot{}, fmt.Errorf("%w: %v", ErrSnapshotUnavailable, loadErr)
		}
		s.snapshotMu.Lock()
		s.snapshotCache[key] = snapshotCacheEntry{value: cloneSnapshot(loaded), expiresAt: time.Now().Add(s.snapshotTTL)}
		s.snapshotMu.Unlock()
		return loaded, nil
	})
	if err != nil {
		return TradingAccountSnapshot{}, err
	}
	return cloneSnapshot(value.(TradingAccountSnapshot)), nil
}

func (s *Service) loadSnapshot(ctx context.Context, owner string, record TradingAccountRecord) (TradingAccountSnapshot, error) {
	result := TradingAccountSnapshot{
		TradingAccountID: record.ID, ProductName: record.ProductName,
		Exchange: record.Exchange, AccountName: record.AccountName,
	}
	if strings.EqualFold(record.Exchange, "polymarket") {
		if s.polymarket == nil || s.polyData == nil || s.polyCLOB == nil {
			return result, ErrSnapshotUnavailable
		}
		credentials, err := s.polymarket.GetByOwner(ctx, owner, record.ID)
		if err != nil {
			return result, err
		}
		apiKey, err := s.cipher.Decrypt(credentials.APIKeyEnc)
		if err != nil {
			return result, err
		}
		secret, err := s.cipher.Decrypt(credentials.APISecretEnc)
		if err != nil {
			return result, err
		}
		passphrase, err := s.cipher.Decrypt(credentials.PassphraseEnc)
		if err != nil {
			return result, err
		}
		balance, err := s.polyCLOB.CollateralBalance(ctx, polymarket.Credentials{
			SignerAddress: credentials.SignerAddress, FunderAddress: credentials.FunderAddress,
			APIKey: apiKey, APISecret: secret, Passphrase: passphrase, SignatureType: credentials.SignatureType,
		})
		if err != nil {
			return result, err
		}
		positions, err := s.polyData.ListPositions(ctx, credentials.FunderAddress)
		if err != nil {
			return result, err
		}
		now := time.Now().UTC()
		equity, equityErr := decimal.NewFromString(balance)
		if equityErr != nil {
			return result, fmt.Errorf("invalid polymarket collateral balance: %w", equityErr)
		}
		result.AvailableFundsUSD, result.SourceUpdatedAt = balance, now
		for _, item := range positions {
			size, sizeErr := decimal.NewFromString(item.Size)
			if sizeErr != nil || !size.IsPositive() || item.Redeemable ||
				item.EndTime.IsZero() || !item.EndTime.After(now) {
				continue
			}
			currentValue, currentValueErr := decimal.NewFromString(item.CurrentValue)
			if currentValueErr != nil {
				continue
			}
			equity = equity.Add(currentValue)
			result.Positions = append(result.Positions, portfolio.Position{
				Key: "polymarket:" + item.TokenID, Kind: "polymarket", Exchange: "Polymarket",
				Symbol: item.Market, Side: item.Outcome, NotionalUSD: item.CurrentValue,
				Size: item.Size, EntryPrice: item.AveragePrice, MarkPrice: item.CurrentPrice,
				UnrealizedPnL: item.CashPnL, MarketTitle: item.Market, Outcome: item.Outcome,
				InitialValue: item.InitialValue, CurrentValue: item.CurrentValue, CashPnL: item.CashPnL,
				ConditionID: item.ConditionID, TokenID: item.TokenID, EndTime: item.EndTime,
			})
		}
		result.AccountEquityUSD = equity.String()
		return result, nil
	}
	if s.portfolios == nil {
		return result, portfolio.ErrUnsupported
	}
	apiKey, err := s.cipher.Decrypt(record.APIKeyEnc)
	if err != nil {
		return result, err
	}
	secret, err := s.cipher.Decrypt(record.APISecretEnc)
	if err != nil {
		return result, err
	}
	passphrase := ""
	if len(record.PassphraseEnc) > 0 {
		passphrase, err = s.cipher.Decrypt(record.PassphraseEnc)
		if err != nil {
			return result, err
		}
	}
	snapshot, err := s.portfolios.Snapshot(ctx, record.Exchange, portfolio.Credentials{APIKey: apiKey, APISecret: secret, Passphrase: passphrase})
	if err != nil {
		return result, err
	}
	if normalizeErr := s.normalizeCEXSnapshot(ctx, record.Exchange, &snapshot); normalizeErr != nil {
		result.LastError = normalizeErr.Error()
	}
	result.AccountEquityUSD = snapshot.AccountEquityUSD
	result.AvailableFundsUSD, result.RiskPercent, result.Positions = snapshot.AvailableFundsUSD, snapshot.RiskPercent, snapshot.Positions
	result.SpotBalances = snapshot.SpotBalances
	result.SourceUpdatedAt = snapshot.UpdatedAt
	return result, nil
}

func (s *Service) GetProductGroupSnapshot(ctx context.Context, token, product string) (ProductGroupSnapshot, error) {
	session, err := s.ValidateSession(token)
	if err != nil {
		return ProductGroupSnapshot{}, err
	}
	product = strings.TrimSpace(product)
	if product == "" {
		return ProductGroupSnapshot{}, ErrInvalidTradingAccount
	}
	records, err := s.trading.ListByOwnerProduct(ctx, session.Username, product)
	if err != nil {
		return ProductGroupSnapshot{}, err
	}
	result := ProductGroupSnapshot{ProductName: product, AccountCount: len(records)}
	type outcome struct {
		snapshot TradingAccountSnapshot
		err      error
		account  string
	}
	outcomes := make(chan outcome, len(records))
	limit := make(chan struct{}, 4)
	var wait sync.WaitGroup
	for _, record := range records {
		record := record
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case limit <- struct{}{}:
			case <-ctx.Done():
				outcomes <- outcome{err: ctx.Err(), account: record.AccountName}
				return
			}
			defer func() { <-limit }()
			value, snapshotErr := s.snapshotForRecord(ctx, session.Username, record)
			outcomes <- outcome{snapshot: value, err: snapshotErr, account: record.AccountName}
		}()
	}
	wait.Wait()
	close(outcomes)
	type aggregate struct {
		notional, spot, contract decimal.Decimal
	}
	aggregated := make(map[string]aggregate)
	totalEquity, totalAvailable := decimal.Zero, decimal.Zero
	hasEquity, hasAvailable := false, false
	for item := range outcomes {
		if item.err != nil {
			result.Partial = true
			result.Errors = append(result.Errors, item.account+": snapshot unavailable")
			continue
		}
		if item.snapshot.Stale {
			result.Stale, result.Partial = true, true
			result.Errors = append(result.Errors, item.account+": cached snapshot")
		}
		if item.snapshot.LastError != "" {
			result.Partial = true
			result.Errors = append(result.Errors, item.account+": "+item.snapshot.LastError)
		}
		if item.snapshot.SourceUpdatedAt.After(result.SourceUpdatedAt) {
			result.SourceUpdatedAt = item.snapshot.SourceUpdatedAt
		}
		if value, parseErr := decimal.NewFromString(item.snapshot.AccountEquityUSD); parseErr == nil {
			totalEquity, hasEquity = totalEquity.Add(value), true
		}
		if value, parseErr := decimal.NewFromString(item.snapshot.AvailableFundsUSD); parseErr == nil {
			totalAvailable, hasAvailable = totalAvailable.Add(value), true
		}
		accountSymbols := make(map[string]struct{})
		for _, position := range item.snapshot.Positions {
			symbol := position.BaseAsset
			if symbol == "" {
				symbol = normalizeGroupSymbol(position.Symbol)
			}
			symbol = strings.ToUpper(strings.TrimSpace(symbol))
			if symbol == "" {
				continue
			}
			current := aggregated[symbol]
			if value, parseErr := decimal.NewFromString(position.NotionalUSD); parseErr == nil {
				current.notional = current.notional.Add(value.Abs())
			}
			if position.Kind == "cex" {
				accountSymbols[symbol] = struct{}{}
				contract := position.SignedContractSize
				if value, parseErr := decimal.NewFromString(contract); parseErr == nil {
					current.contract = current.contract.Add(value)
				}
			}
			aggregated[symbol] = current
		}
		for symbol := range accountSymbols {
			current := aggregated[symbol]
			if value, parseErr := decimal.NewFromString(item.snapshot.SpotBalances[symbol]); parseErr == nil {
				current.spot = current.spot.Add(value)
				aggregated[symbol] = current
			}
		}
	}
	if hasEquity {
		result.AccountEquityUSD = totalEquity.String()
	}
	if hasAvailable {
		result.AvailableFundsUSD = totalAvailable.String()
	}
	for symbol, total := range aggregated {
		result.Positions = append(result.Positions, ProductGroupPosition{
			Symbol: symbol, TotalNotionalUSD: total.notional.String(),
			SpotSize: total.spot.String(), ContractSize: total.contract.String(),
		})
	}
	sort.Slice(result.Positions, func(i, j int) bool {
		left, _ := decimal.NewFromString(result.Positions[i].TotalNotionalUSD)
		right, _ := decimal.NewFromString(result.Positions[j].TotalNotionalUSD)
		return left.GreaterThan(right)
	})
	return result, nil
}

func normalizeGroupSymbol(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	for _, suffix := range []string{"-PERP", "-SWAP", "USDT", "USDC", "USD"} {
		value = strings.TrimSuffix(value, suffix)
	}
	return strings.TrimRight(value, "-_")
}

func cloneSnapshot(value TradingAccountSnapshot) TradingAccountSnapshot {
	value.Positions = append([]portfolio.Position(nil), value.Positions...)
	if value.SpotBalances != nil {
		source := value.SpotBalances
		value.SpotBalances = make(map[string]string, len(source))
		for asset, balance := range source {
			value.SpotBalances[asset] = balance
		}
	}
	return value
}
