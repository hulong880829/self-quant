package trader

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/orderstream"
)

const fillAverageAccountConcurrency = 2

type missingArbitrageAverageOrder struct {
	ID               string
	CombinationID    string
	OwnerUsername    string
	TradingAccountID int64
	Exchange         string
	ContractType     string
	InstrumentID     int64
	ClientOrderID    string
	VenueOrderID     string
	FilledQuantity   string
	CreatedAt        time.Time
}

type missingAverageStore interface {
	ListMissingArbitrageOrderAverages(context.Context, []string) ([]missingArbitrageAverageOrder, error)
	TrySetArbitrageOrderAverage(context.Context, string, string, string) (bool, error)
}

type recentAverageSource interface {
	LookupRecentFillAverage(orderstream.Key, string, string) (orderstream.RecentFillAverage, bool)
}

type fillAverageAccountGroup struct {
	OwnerUsername    string
	Exchange         string
	TradingAccountID int64
	Orders           []missingArbitrageAverageOrder
}

type fillAverageStats struct {
	mu                     sync.Mutex
	candidates             int
	memoryHits             int
	memoryQuantityMismatch int
	restQueries            int
	restResolved           int
	venueFallbackResolved  int
	updated                int
	noops                  int
	unresolved             int
}

type ArbitrageFillAverageCompensator struct {
	store       missingAverageStore
	streams     recentAverageSource
	catalog     instrumentCatalog
	credentials internalCredentialProvider
	venues      *exchange.Registry
	hyperliquid *HyperliquidArbitrageFillAverageCompensator
	token       string
	timeout     time.Duration
	logger      *slog.Logger
}

func NewArbitrageFillAverageCompensator(
	store missingAverageStore,
	streams recentAverageSource,
	catalog instrumentCatalog,
	credentials internalCredentialProvider,
	venues *exchange.Registry,
	hyperliquid *HyperliquidArbitrageFillAverageCompensator,
	token string,
	timeout time.Duration,
	logger *slog.Logger,
) *ArbitrageFillAverageCompensator {
	if timeout <= 0 {
		timeout = hyperliquidAverageCompensateDefaultTimeout
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ArbitrageFillAverageCompensator{
		store: store, streams: derefNilFillAverageStreams(streams), catalog: catalog, credentials: credentials,
		venues: venues, hyperliquid: hyperliquid, token: token, timeout: timeout, logger: logger,
	}
}

func derefNilFillAverageStreams(streams recentAverageSource) recentAverageSource {
	if streams == nil {
		return nil
	}
	manager, ok := streams.(*orderstream.Manager)
	if ok && manager == nil {
		return nil
	}
	return streams
}

func (c *ArbitrageFillAverageCompensator) Compensate(ctx context.Context, combinationIDs []string) {
	if c == nil || c.store == nil || len(combinationIDs) == 0 {
		return
	}
	orders, err := c.store.ListMissingArbitrageOrderAverages(ctx, combinationIDs)
	if err != nil {
		if ctx.Err() == nil {
			c.logger.Error("list missing arbitrage averages failed", "error", err)
		}
		return
	}
	stats := fillAverageStats{candidates: len(orders)}
	rest := make([]missingArbitrageAverageOrder, 0)
	hl := make([]missingHyperliquidAverageOrder, 0)
	for _, order := range orders {
		if ctx.Err() != nil {
			return
		}
		switch c.applyMemory(ctx, order, &stats) {
		case fillAverageHandled:
			continue
		case fillAverageMemoryMismatch:
			stats.memoryQuantityMismatch++
		}
		if strings.EqualFold(strings.TrimSpace(order.Exchange), "hyperliquid") {
			hl = append(hl, missingHyperliquidAverageOrder{
				ID: order.ID, CombinationID: order.CombinationID, OwnerUsername: order.OwnerUsername,
				TradingAccountID: order.TradingAccountID, InstrumentID: order.InstrumentID,
				VenueOrderID: order.VenueOrderID, FilledQuantity: order.FilledQuantity,
				CreatedAt: order.CreatedAt,
			})
			continue
		}
		rest = append(rest, order)
	}
	if c.hyperliquid != nil && len(hl) > 0 {
		for _, item := range c.hyperliquid.CompensateListed(ctx, hl) {
			if c.commitAverage(
				ctx, item.OrderID, item.CombinationID, "hyperliquid",
				item.Average, item.ExpectedFilled, &stats,
			) {
				stats.addVenueFallback()
			}
		}
	}
	c.compensateREST(ctx, rest, &stats)
	stats.mu.Lock()
	stats.unresolved = stats.candidates - stats.updated - stats.noops
	if stats.unresolved < 0 {
		stats.unresolved = 0
	}
	candidates, memoryHits, mismatch := stats.candidates, stats.memoryHits, stats.memoryQuantityMismatch
	restQueries, restResolved := stats.restQueries, stats.restResolved
	fallback, updated, unresolved := stats.venueFallbackResolved, stats.updated, stats.unresolved
	stats.mu.Unlock()
	c.logger.Info(
		"arbitrage fill average compensate",
		"candidates", candidates,
		"memory_hits", memoryHits,
		"memory_quantity_mismatch", mismatch,
		"rest_queries", restQueries,
		"rest_resolved", restResolved,
		"venue_fallback_resolved", fallback,
		"updated", updated,
		"unresolved", unresolved,
	)
}

type fillAverageMemoryResult int

const (
	fillAverageMiss fillAverageMemoryResult = iota
	fillAverageHandled
	fillAverageMemoryMismatch
)

func (s *fillAverageStats) addNoop() {
	s.mu.Lock()
	s.noops++
	s.mu.Unlock()
}

func (s *fillAverageStats) addUpdated() {
	s.mu.Lock()
	s.updated++
	s.mu.Unlock()
}

func (s *fillAverageStats) addRestQuery() {
	s.mu.Lock()
	s.restQueries++
	s.mu.Unlock()
}

func (s *fillAverageStats) addRestResolved() {
	s.mu.Lock()
	s.restResolved++
	s.mu.Unlock()
}

func (s *fillAverageStats) addVenueFallback() {
	s.mu.Lock()
	s.venueFallbackResolved++
	s.mu.Unlock()
}

func (c *ArbitrageFillAverageCompensator) applyMemory(
	ctx context.Context,
	order missingArbitrageAverageOrder,
	stats *fillAverageStats,
) fillAverageMemoryResult {
	if c.streams == nil {
		return fillAverageMiss
	}
	snapshot, ok := c.streams.LookupRecentFillAverage(
		orderstream.Key{
			Account: strconv.FormatInt(order.TradingAccountID, 10),
			Venue:   order.Exchange,
			Product: order.ContractType,
		},
		order.ClientOrderID,
		order.VenueOrderID,
	)
	if !ok {
		return fillAverageMiss
	}
	if !fillAverageSnapshotMatches(order, snapshot) {
		return fillAverageMiss
	}
	average := parsePositiveDecimal(snapshot.AveragePrice)
	if !average.IsPositive() {
		return fillAverageMiss
	}
	if c.catalog == nil {
		return fillAverageMiss
	}
	instrument, err := c.catalog.Get(ctx, order.InstrumentID)
	if err != nil {
		c.logOrderError(order, "instrument", err)
		return fillAverageMiss
	}
	converted, err := exchange.FromVenueQuantity(toVenueInstrument(instrument), snapshot.CumulativeFilled)
	if err != nil {
		return fillAverageMemoryMismatch
	}
	got, err := decimal.NewFromString(converted)
	expected, expectedErr := decimal.NewFromString(strings.TrimSpace(order.FilledQuantity))
	if err != nil || expectedErr != nil || !got.Equal(expected) {
		return fillAverageMemoryMismatch
	}
	stats.memoryHits++
	c.commitAverage(ctx, order.ID, order.CombinationID, order.Exchange, average.String(), order.FilledQuantity, stats)
	return fillAverageHandled
}

func fillAverageSnapshotMatches(order missingArbitrageAverageOrder, snapshot orderstream.RecentFillAverage) bool {
	if !strings.EqualFold(snapshot.Key.Venue, strings.TrimSpace(order.Exchange)) {
		return false
	}
	if snapshot.Key.Account != strconv.FormatInt(order.TradingAccountID, 10) {
		return false
	}
	client := strings.TrimSpace(order.ClientOrderID)
	venue := strings.TrimSpace(order.VenueOrderID)
	if client != "" && snapshot.ClientOrderID != "" && client != snapshot.ClientOrderID {
		return false
	}
	if venue != "" && snapshot.VenueOrderID != "" && venue != snapshot.VenueOrderID {
		return false
	}
	if client != "" && snapshot.ClientOrderID == client {
		return true
	}
	if venue != "" && snapshot.VenueOrderID == venue {
		return true
	}
	return false
}

func (c *ArbitrageFillAverageCompensator) compensateREST(
	ctx context.Context,
	orders []missingArbitrageAverageOrder,
	stats *fillAverageStats,
) {
	groups := groupMissingAverageByAccount(orders)
	if len(groups) == 0 {
		return
	}
	sem := make(chan struct{}, fillAverageAccountConcurrency)
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
			c.compensateAccountGroup(ctx, group, stats)
		}()
	}
	wg.Wait()
}

func (c *ArbitrageFillAverageCompensator) compensateAccountGroup(
	ctx context.Context,
	group fillAverageAccountGroup,
	stats *fillAverageStats,
) {
	if ctx.Err() != nil || len(group.Orders) == 0 {
		return
	}
	if c.credentials == nil || c.catalog == nil || c.venues == nil {
		c.logger.Error("arbitrage fill average compensate is not configured")
		return
	}
	credentials, err := c.credentials.GetInternal(
		ctx, c.token, group.OwnerUsername, group.TradingAccountID,
	)
	if err != nil {
		c.logger.Error(
			"arbitrage fill average credentials failed",
			"account_id", group.TradingAccountID,
			"exchange", group.Exchange,
			"error", err,
		)
		return
	}
	adapter, ok := c.venues.Adapter(group.Exchange)
	if !ok {
		c.logger.Error(
			"arbitrage fill average adapter unavailable",
			"exchange", group.Exchange,
		)
		return
	}
	instruments := make(map[int64]Instrument, len(group.Orders))
	for _, order := range group.Orders {
		if ctx.Err() != nil {
			return
		}
		instrument, cached := instruments[order.InstrumentID]
		if !cached {
			instrument, err = c.catalog.Get(ctx, order.InstrumentID)
			if err != nil {
				c.logOrderError(order, "instrument", err)
				continue
			}
			instruments[order.InstrumentID] = instrument
		}
		stats.addRestQuery()
		queryCtx, cancel := context.WithTimeout(ctx, c.timeout)
		result, queryErr := adapter.GetOrder(
			queryCtx,
			toVenueCredentials(credentials),
			exchange.QueryRequest{
				Instrument:    toVenueInstrument(instrument),
				ClientOrderID: order.ClientOrderID,
				VenueOrderID:  order.VenueOrderID,
				CreatedAt:     order.CreatedAt,
			},
		)
		cancel()
		if queryErr != nil {
			c.logOrderError(order, "get order", queryErr)
			continue
		}
		if !restAverageCovers(order, result, instrument) {
			c.logger.Warn(
				"arbitrage fill average rest mismatch",
				"order_id", order.ID,
				"combination_id", order.CombinationID,
				"exchange", order.Exchange,
			)
			continue
		}
		if c.commitAverage(ctx, order.ID, order.CombinationID, order.Exchange, strings.TrimSpace(result.AveragePrice), order.FilledQuantity, stats) {
			stats.addRestResolved()
		}
	}
}

func restAverageCovers(
	order missingArbitrageAverageOrder,
	result exchange.Result,
	instrument Instrument,
) bool {
	average := parsePositiveDecimal(result.AveragePrice)
	if !average.IsPositive() {
		return false
	}
	converted, err := exchange.FromVenueQuantity(toVenueInstrument(instrument), result.FilledQuantity)
	if err != nil {
		return false
	}
	got, err := decimal.NewFromString(converted)
	expected, expectedErr := decimal.NewFromString(strings.TrimSpace(order.FilledQuantity))
	if err != nil || expectedErr != nil || !got.Equal(expected) {
		return false
	}
	orderVenue := strings.TrimSpace(order.VenueOrderID)
	resultVenue := strings.TrimSpace(result.VenueOrderID)
	if orderVenue != "" && resultVenue != "" && orderVenue != resultVenue {
		return false
	}
	return true
}

func (c *ArbitrageFillAverageCompensator) commitAverage(
	ctx context.Context,
	orderID, combinationID, exchangeName, average, expectedFilled string,
	stats *fillAverageStats,
) bool {
	updated, err := c.store.TrySetArbitrageOrderAverage(ctx, orderID, average, expectedFilled)
	if err != nil {
		c.logger.Error(
			"arbitrage fill average update failed",
			"order_id", orderID,
			"combination_id", combinationID,
			"exchange", exchangeName,
			"error", err,
		)
		return false
	}
	if !updated {
		stats.addNoop()
		return false
	}
	stats.addUpdated()
	return true
}

func (c *ArbitrageFillAverageCompensator) logOrderError(
	order missingArbitrageAverageOrder,
	reason string,
	err error,
) {
	c.logger.Error(
		"arbitrage fill average order failed",
		"order_id", order.ID,
		"combination_id", order.CombinationID,
		"exchange", order.Exchange,
		"reason", reason,
		"error", err,
	)
}

func groupMissingAverageByAccount(orders []missingArbitrageAverageOrder) []fillAverageAccountGroup {
	indexByKey := make(map[string]int)
	groups := make([]fillAverageAccountGroup, 0)
	for _, order := range orders {
		key := fmt.Sprintf(
			"%s\x00%s\x00%d",
			order.OwnerUsername, strings.ToLower(strings.TrimSpace(order.Exchange)), order.TradingAccountID,
		)
		if index, ok := indexByKey[key]; ok {
			groups[index].Orders = append(groups[index].Orders, order)
			continue
		}
		indexByKey[key] = len(groups)
		groups = append(groups, fillAverageAccountGroup{
			OwnerUsername:    order.OwnerUsername,
			Exchange:         strings.ToLower(strings.TrimSpace(order.Exchange)),
			TradingAccountID: order.TradingAccountID,
			Orders:           []missingArbitrageAverageOrder{order},
		})
	}
	return groups
}

func (r *Repository) ListMissingArbitrageOrderAverages(
	ctx context.Context,
	combinationIDs []string,
) ([]missingArbitrageAverageOrder, error) {
	if len(combinationIDs) == 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT o.id::text,e.combination_id::text,c.owner_username,
		       o.trading_account_id,o.exchange,o.contract_type,o.instrument_id,
		       COALESCE(o.client_order_id,''),COALESCE(o.venue_order_id,''),
		       o.filled_quantity::text,o.created_at
		FROM trader_orders o
		JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
		JOIN trader_arbitrage_combinations c ON c.id=e.combination_id
		WHERE e.combination_id=ANY($1::uuid[])
		  AND o.arbitrage_execution_id IS NOT NULL
		  AND o.status IN ('filled','canceled','rejected','expired')
		  AND o.filled_quantity>0
		  AND o.average_price<=0`, combinationIDs)
	if err != nil {
		return nil, fmt.Errorf("list missing arbitrage averages: %w", err)
	}
	defer rows.Close()
	orders := make([]missingArbitrageAverageOrder, 0)
	for rows.Next() {
		var order missingArbitrageAverageOrder
		if scanErr := rows.Scan(
			&order.ID, &order.CombinationID, &order.OwnerUsername,
			&order.TradingAccountID, &order.Exchange, &order.ContractType, &order.InstrumentID,
			&order.ClientOrderID, &order.VenueOrderID, &order.FilledQuantity, &order.CreatedAt,
		); scanErr != nil {
			return nil, fmt.Errorf("scan missing arbitrage average: %w", scanErr)
		}
		orders = append(orders, order)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate missing arbitrage averages: %w", err)
	}
	return orders, nil
}

func (r *Repository) TrySetArbitrageOrderAverage(
	ctx context.Context,
	orderID, averagePrice, expectedFilledQuantity string,
) (bool, error) {
	var id string
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_orders
		SET average_price=$2::numeric, updated_at=now()
		WHERE id=$1::uuid
		  AND average_price<=0
		  AND filled_quantity=$3::numeric
		  AND status IN ('filled','canceled','rejected','expired')
		RETURNING id::text`,
		orderID, averagePrice, expectedFilledQuantity,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("set arbitrage order average: %w", err)
	}
	return true, nil
}
