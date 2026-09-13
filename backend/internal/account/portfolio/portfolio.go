package portfolio

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

var ErrUnsupported = errors.New("unified account snapshots are not supported for this exchange")

type Credentials struct {
	APIKey, APISecret, Passphrase string
	CredentialKind, SigningAddress string
	AccountIndex                  *int64
	APIKeyIndex                   *int32
}

type Position struct {
	Key, Kind, Exchange, Symbol, Side         string
	NotionalUSD, Size, SpotSize               string
	SignedContractSize, EntryPrice, MarkPrice string
	UnrealizedPnL                             string
	WireSymbol, ProductCategory, BaseAsset    string
	MarketTitle, Outcome                      string
	InitialValue, CurrentValue, CashPnL       string
	ConditionID, TokenID                      string
	EndTime                                   time.Time
}

type Snapshot struct {
	AccountEquityUSD  string
	AvailableFundsUSD string
	RiskPercent       string
	Positions         []Position
	SpotBalances      map[string]string
	UpdatedAt         time.Time
}

type Adapter interface {
	Snapshot(context.Context, Credentials) (Snapshot, error)
}

type TradeFill struct {
	ExternalTradeID, OrderID, Symbol, Side string
	Price, Quantity, QuoteNotionalUSD      string
	Fee, FeeCurrency                       string
	TradedAt                               time.Time
}

type TradeQuery struct {
	Since, Until time.Time
}

type TradeAdapter interface {
	TradeFills(context.Context, Credentials, TradeQuery) ([]TradeFill, error)
}

type Registry struct {
	adapters map[string]Adapter
}

func NewRegistry(client *http.Client, urls map[string]string) *Registry {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &Registry{adapters: map[string]Adapter{
		"binance":     limit(newBinance(client, urls["binance"])),
		"okx":         limit(newOKX(client, urls["okx"])),
		"bitget":      limit(newBitget(client, urls["bitget"])),
		"bybit":       limit(newBybit(client, urls["bybit"])),
		"gate":        limit(newGate(client, urls["gate"])),
		"hyperliquid": limit(newHyperliquid(client, urls["hyperliquid"])),
		"aster":       limit(newAster(client, urls["aster"])),
		"lighter":     limit(newLighter(client, urls["lighter"])),
	}}
}

type limitedAdapter struct {
	delegate Adapter
	mu       sync.Mutex
	next     time.Time
}

func limit(delegate Adapter) Adapter { return &limitedAdapter{delegate: delegate} }

func (a *limitedAdapter) Snapshot(ctx context.Context, credentials Credentials) (Snapshot, error) {
	if err := a.wait(ctx); err != nil {
		return Snapshot{}, err
	}
	return a.delegate.Snapshot(ctx, credentials)
}

func (a *limitedAdapter) TradeFills(
	ctx context.Context,
	credentials Credentials,
	query TradeQuery,
) ([]TradeFill, error) {
	delegate, ok := a.delegate.(TradeAdapter)
	if !ok {
		return nil, ErrUnsupported
	}
	if err := a.wait(ctx); err != nil {
		return nil, err
	}
	return delegate.TradeFills(ctx, credentials, query)
}

func (a *limitedAdapter) wait(ctx context.Context) error {
	a.mu.Lock()
	delay := time.Until(a.next)
	if delay < 0 {
		delay = 0
	}
	a.next = time.Now().Add(delay + 50*time.Millisecond)
	a.mu.Unlock()
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func (r *Registry) Snapshot(ctx context.Context, exchange string, credentials Credentials) (Snapshot, error) {
	adapter, ok := r.adapters[strings.ToLower(strings.TrimSpace(exchange))]
	if !ok {
		return Snapshot{}, ErrUnsupported
	}
	return adapter.Snapshot(ctx, credentials)
}

func (r *Registry) TradeFills(
	ctx context.Context,
	exchange string,
	credentials Credentials,
	query TradeQuery,
) ([]TradeFill, error) {
	adapter, ok := r.adapters[strings.ToLower(strings.TrimSpace(exchange))]
	if !ok {
		return nil, ErrUnsupported
	}
	trades, ok := adapter.(TradeAdapter)
	if !ok {
		return nil, ErrUnsupported
	}
	return trades.TradeFills(ctx, credentials, query)
}
