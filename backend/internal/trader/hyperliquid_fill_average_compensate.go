package trader

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
)

const (
	hyperliquidAverageCompensateLookback       = time.Minute
	hyperliquidAverageCompensateConcurrency    = 2
	hyperliquidAverageCompensateDefaultTimeout = 12 * time.Second
)

type missingHyperliquidAverageOrder struct {
	ID               string
	CombinationID    string
	OwnerUsername    string
	TradingAccountID int64
	InstrumentID     int64
	VenueOrderID     string
	FilledQuantity   string
	CreatedAt        time.Time
}

type missingHyperliquidAverageGroup struct {
	OwnerUsername    string
	TradingAccountID int64
	InstrumentID     int64
	Orders           []missingHyperliquidAverageOrder
}

type hyperliquidAverageCompensateStore interface {
	ListMissingHyperliquidArbitrageAverages(
		context.Context, []string,
	) ([]missingHyperliquidAverageOrder, error)
	TrySetHyperliquidArbitrageAverage(context.Context, string, string, string) error
}

type HyperliquidArbitrageFillAverageCompensator struct {
	store       hyperliquidAverageCompensateStore
	catalog     instrumentCatalog
	credentials internalCredentialProvider
	venues      *exchange.Registry
	token       string
	timeout     time.Duration
	logger      *slog.Logger
}

func NewHyperliquidArbitrageFillAverageCompensator(
	store hyperliquidAverageCompensateStore,
	catalog instrumentCatalog,
	credentials internalCredentialProvider,
	venues *exchange.Registry,
	token string,
	timeout time.Duration,
	logger *slog.Logger,
) *HyperliquidArbitrageFillAverageCompensator {
	if timeout <= 0 {
		timeout = hyperliquidAverageCompensateDefaultTimeout
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &HyperliquidArbitrageFillAverageCompensator{
		store: store, catalog: catalog, credentials: credentials,
		venues: venues, token: token, timeout: timeout, logger: logger,
	}
}

func (c *HyperliquidArbitrageFillAverageCompensator) Compensate(
	ctx context.Context,
	combinationIDs []string,
) {
	if c == nil || c.store == nil || len(combinationIDs) == 0 {
		return
	}
	orders, err := c.store.ListMissingHyperliquidArbitrageAverages(ctx, combinationIDs)
	if err != nil {
		if ctx.Err() == nil {
			c.logger.Error("list missing hyperliquid averages failed", "error", err)
		}
		return
	}
	ready := make([]missingHyperliquidAverageOrder, 0, len(orders))
	for _, order := range orders {
		if strings.TrimSpace(order.VenueOrderID) == "" {
			c.logger.Warn(
				"hyperliquid average compensate skipped missing venue order id",
				"order_id", order.ID,
				"combination_id", order.CombinationID,
			)
			continue
		}
		ready = append(ready, order)
	}
	groups := groupMissingHyperliquidAverageOrders(ready)
	if len(groups) == 0 {
		return
	}
	sem := make(chan struct{}, hyperliquidAverageCompensateConcurrency)
	var wg sync.WaitGroup
	for index := range groups {
		if ctx.Err() != nil {
			break
		}
		group := groups[index]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			c.compensateGroup(ctx, group)
		}()
	}
	wg.Wait()
}

func (c *HyperliquidArbitrageFillAverageCompensator) compensateGroup(
	ctx context.Context,
	group missingHyperliquidAverageGroup,
) {
	if ctx.Err() != nil || len(group.Orders) == 0 {
		return
	}
	if c.credentials == nil || c.catalog == nil {
		c.logger.Error("hyperliquid average compensate is not configured")
		return
	}
	credentials, err := c.credentials.GetInternal(
		ctx, c.token, group.OwnerUsername, group.TradingAccountID,
	)
	if err != nil {
		c.logger.Error(
			"hyperliquid average compensate credentials failed",
			"account_id", group.TradingAccountID,
			"error", err,
		)
		return
	}
	instrument, err := c.catalog.Get(ctx, group.InstrumentID)
	if err != nil {
		c.logger.Error(
			"hyperliquid average compensate instrument failed",
			"instrument_id", group.InstrumentID,
			"error", err,
		)
		return
	}
	if c.venues == nil {
		c.logger.Error("hyperliquid average compensate venues are not configured")
		return
	}
	adapter, ok := c.venues.Adapter("hyperliquid")
	if !ok {
		c.logger.Error("hyperliquid average compensate adapter unavailable")
		return
	}
	reader, ok := adapter.(exchange.FillReader)
	if !ok {
		c.logger.Error("hyperliquid average compensate fill reader unavailable")
		return
	}
	since := missingAverageSince(group.Orders)
	queryCtx, cancel := context.WithTimeout(ctx, c.timeout)
	fills, err := reader.ListFills(
		queryCtx, toVenueCredentials(credentials), toVenueInstrument(instrument), since,
	)
	cancel()
	if err != nil {
		c.logger.Error(
			"hyperliquid average compensate list fills failed",
			"account_id", group.TradingAccountID,
			"instrument_id", group.InstrumentID,
			"error", err,
		)
		return
	}
	for _, order := range group.Orders {
		if ctx.Err() != nil {
			return
		}
		quantity, notional, matched := aggregateHyperliquidFills(
			fills, order.VenueOrderID, instrument,
		)
		expected, expectedErr := decimal.NewFromString(strings.TrimSpace(order.FilledQuantity))
		if expectedErr != nil || !expected.IsPositive() || !matched || !quantity.Equal(expected) {
			c.logger.Warn(
				"hyperliquid average compensate quantity mismatch",
				"order_id", order.ID,
				"venue_order_id", order.VenueOrderID,
				"filled_quantity", order.FilledQuantity,
				"rest_quantity", quantity.String(),
			)
			continue
		}
		average := notional.Div(quantity).String()
		if setErr := c.store.TrySetHyperliquidArbitrageAverage(
			ctx, order.ID, average, order.FilledQuantity,
		); setErr != nil {
			c.logger.Error(
				"hyperliquid average compensate update failed",
				"order_id", order.ID,
				"error", setErr,
			)
		}
	}
}

func groupMissingHyperliquidAverageOrders(
	orders []missingHyperliquidAverageOrder,
) []missingHyperliquidAverageGroup {
	indexByKey := make(map[string]int)
	groups := make([]missingHyperliquidAverageGroup, 0)
	for _, order := range orders {
		key := fmt.Sprintf(
			"%s\x00%d\x00%d",
			order.OwnerUsername, order.TradingAccountID, order.InstrumentID,
		)
		if index, ok := indexByKey[key]; ok {
			groups[index].Orders = append(groups[index].Orders, order)
			continue
		}
		indexByKey[key] = len(groups)
		groups = append(groups, missingHyperliquidAverageGroup{
			OwnerUsername:    order.OwnerUsername,
			TradingAccountID: order.TradingAccountID,
			InstrumentID:     order.InstrumentID,
			Orders:           []missingHyperliquidAverageOrder{order},
		})
	}
	return groups
}

func missingAverageSince(orders []missingHyperliquidAverageOrder) time.Time {
	earliest := orders[0].CreatedAt
	for _, order := range orders[1:] {
		if order.CreatedAt.Before(earliest) {
			earliest = order.CreatedAt
		}
	}
	return earliest.Add(-hyperliquidAverageCompensateLookback)
}

func aggregateHyperliquidFills(
	fills []exchange.Fill,
	venueOrderID string,
	instrument Instrument,
) (decimal.Decimal, decimal.Decimal, bool) {
	venueOrderID = strings.TrimSpace(venueOrderID)
	if venueOrderID == "" {
		return decimal.Zero, decimal.Zero, false
	}
	seen := make(map[string]struct{})
	quantity := decimal.Zero
	notional := decimal.Zero
	venueInstrument := toVenueInstrument(instrument)
	for _, fill := range fills {
		if strings.TrimSpace(fill.VenueOrderID) != venueOrderID {
			continue
		}
		tradeID := strings.TrimSpace(fill.TradeID)
		if tradeID == "" {
			continue
		}
		if _, dup := seen[tradeID]; dup {
			continue
		}
		converted, err := exchange.FromVenueQuantity(venueInstrument, fill.Quantity)
		if err != nil {
			continue
		}
		base, err := decimal.NewFromString(converted)
		if err != nil || !base.IsPositive() {
			continue
		}
		price, err := decimal.NewFromString(strings.TrimSpace(fill.Price))
		if err != nil || !price.IsPositive() {
			continue
		}
		seen[tradeID] = struct{}{}
		quantity = quantity.Add(base)
		notional = notional.Add(base.Mul(price))
	}
	return quantity, notional, quantity.IsPositive()
}

func (r *Repository) ListMissingHyperliquidArbitrageAverages(
	ctx context.Context,
	combinationIDs []string,
) ([]missingHyperliquidAverageOrder, error) {
	if len(combinationIDs) == 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT o.id::text,e.combination_id::text,c.owner_username,
		       o.trading_account_id,o.instrument_id,COALESCE(o.venue_order_id,''),
		       o.filled_quantity::text,o.created_at
		FROM trader_orders o
		JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
		JOIN trader_arbitrage_combinations c ON c.id=e.combination_id
		WHERE e.combination_id=ANY($1::uuid[])
		  AND lower(o.exchange)='hyperliquid'
		  AND o.arbitrage_execution_id IS NOT NULL
		  AND o.status IN ('filled','canceled','rejected','expired')
		  AND o.filled_quantity>0
		  AND o.average_price<=0`, combinationIDs)
	if err != nil {
		return nil, fmt.Errorf("list missing hyperliquid averages: %w", err)
	}
	defer rows.Close()
	orders := make([]missingHyperliquidAverageOrder, 0)
	for rows.Next() {
		var order missingHyperliquidAverageOrder
		if scanErr := rows.Scan(
			&order.ID, &order.CombinationID, &order.OwnerUsername,
			&order.TradingAccountID, &order.InstrumentID, &order.VenueOrderID,
			&order.FilledQuantity, &order.CreatedAt,
		); scanErr != nil {
			return nil, fmt.Errorf("scan missing hyperliquid average: %w", scanErr)
		}
		orders = append(orders, order)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate missing hyperliquid averages: %w", err)
	}
	return orders, nil
}

func (r *Repository) TrySetHyperliquidArbitrageAverage(
	ctx context.Context,
	orderID, averagePrice, expectedFilledQuantity string,
) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE trader_orders
		SET average_price=$2::numeric, updated_at=now()
		WHERE id=$1::uuid
		  AND average_price<=0
		  AND filled_quantity=$3::numeric
		  AND status IN ('filled','canceled','rejected','expired')`,
		orderID, averagePrice, expectedFilledQuantity,
	)
	if err != nil {
		return fmt.Errorf("set hyperliquid arbitrage average: %w", err)
	}
	return nil
}
