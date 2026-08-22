package polymarket

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	quoteFlushInterval        = 100 * time.Millisecond
	subscriptionCheckInterval = 5 * time.Second
	maxAssetsPerWSConnection  = 16
	minReconnectBackoff       = 2 * time.Second
)

type tokenMarketLink struct {
	marketID string
	outcome  string
}

type MarketStream struct {
	url       string
	snapshots *SnapshotStore
	logger    *slog.Logger
	refreshCh chan struct{}
}

func NewMarketStream(
	url string,
	snapshots *SnapshotStore,
	logger *slog.Logger,
) *MarketStream {
	return &MarketStream{
		url: url, snapshots: snapshots, logger: logger,
		refreshCh: make(chan struct{}, 1),
	}
}

// NotifyMarketsChanged signals that the subscribed token set may have changed.
func (s *MarketStream) NotifyMarketsChanged() {
	select {
	case s.refreshCh <- struct{}{}:
	default:
	}
}

func (s *MarketStream) Run(ctx context.Context) {
	backoff := minReconnectBackoff
	for ctx.Err() == nil {
		err := s.runOnce(ctx)
		if err == nil {
			backoff = minReconnectBackoff
		}
		if err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
			s.logger.Warn("CLOB market stream disconnected", "error", err)
		}
		timer := time.NewTimer(backoff + time.Duration(rand.IntN(500))*time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (s *MarketStream) runOnce(ctx context.Context) error {
	markets := s.relevantMarkets()
	if len(markets) == 0 {
		return errors.New("no active markets to subscribe")
	}
	tokenMarkets, assets, subscriptionKey := buildTokenSubscription(markets)
	shards := shardAssets(assets, maxAssetsPerWSConnection)

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var resubscribe atomic.Bool
	var firstErr atomic.Pointer[error]

	pendingMu := sync.Mutex{}
	pending := make(map[string]quoteDelta)

	go s.flushQuotes(sessionCtx, &pendingMu, &pending, tokenMarkets)
	go func() {
		s.watchSubscription(sessionCtx, subscriptionKey, &resubscribe)
		if resubscribe.Load() {
			cancel()
		}
	}()

	var group sync.WaitGroup
	for _, shard := range shards {
		group.Add(1)
		go func(shardAssets []string) {
			defer group.Done()
			if err := s.runShard(
				sessionCtx, shardAssets, tokenMarkets, &pendingMu, &pending,
			); err != nil && ctx.Err() == nil && !resubscribe.Load() {
				firstErr.CompareAndSwap(nil, &err)
				cancel()
			}
		}(shard)
	}
	group.Wait()
	if resubscribe.Load() && firstErr.Load() == nil {
		return nil
	}
	if errPtr := firstErr.Load(); errPtr != nil {
		return *errPtr
	}
	return nil
}

func (s *MarketStream) runShard(
	ctx context.Context,
	assets []string,
	tokenMarkets map[string]tokenMarketLink,
	pendingMu *sync.Mutex,
	pending *map[string]quoteDelta,
) error {
	connection, _, err := websocket.DefaultDialer.DialContext(ctx, s.url, nil)
	if err != nil {
		return err
	}
	defer connection.Close()
	connection.SetReadLimit(8 << 20)
	if err := connection.WriteJSON(map[string]any{
		"type": "market", "assets_ids": assets,
	}); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = connection.Close()
	}()

	for {
		_, body, err := connection.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, delta := range parseCLOBQuoteMessages(body) {
			if _, ok := tokenMarkets[delta.tokenID]; !ok {
				continue
			}
			if delta.bid == "" && delta.ask == "" {
				continue
			}
			pendingMu.Lock()
			existing := (*pending)[delta.tokenID]
			if delta.bid != "" {
				existing.bid = delta.bid
			}
			if delta.ask != "" {
				existing.ask = delta.ask
			}
			existing.tokenID = delta.tokenID
			(*pending)[delta.tokenID] = existing
			pendingMu.Unlock()
		}
	}
}

func (s *MarketStream) flushQuotes(
	ctx context.Context,
	pendingMu *sync.Mutex,
	pending *map[string]quoteDelta,
	tokenMarkets map[string]tokenMarketLink,
) {
	ticker := time.NewTicker(quoteFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pendingMu.Lock()
			if len(*pending) == 0 {
				pendingMu.Unlock()
				continue
			}
			batch := *pending
			*pending = make(map[string]quoteDelta, len(batch))
			pendingMu.Unlock()
			s.applyQuoteBatch(batch, tokenMarkets)
		}
	}
}

func (s *MarketStream) applyQuoteBatch(
	batch map[string]quoteDelta,
	tokenMarkets map[string]tokenMarketLink,
) {
	type marketQuote struct {
		upBid, upAsk, downBid, downAsk string
		hasUp, hasDown                 bool
	}
	byMarket := make(map[string]marketQuote)
	for tokenID, delta := range batch {
		link, ok := tokenMarkets[tokenID]
		if !ok {
			continue
		}
		quote := byMarket[link.marketID]
		if link.outcome == "up" {
			if delta.bid != "" {
				quote.upBid = delta.bid
			}
			if delta.ask != "" {
				quote.upAsk = delta.ask
			}
			quote.hasUp = true
		} else {
			if delta.bid != "" {
				quote.downBid = delta.bid
			}
			if delta.ask != "" {
				quote.downAsk = delta.ask
			}
			quote.hasDown = true
		}
		byMarket[link.marketID] = quote
	}
	now := time.Now().UTC()
	patches := make([]QuotePatch, 0, len(byMarket))
	for marketID, quote := range byMarket {
		patches = append(patches, QuotePatch{
			MarketID: marketID,
			UpBid:    quote.upBid, UpAsk: quote.upAsk,
			DownBid: quote.downBid, DownAsk: quote.downAsk,
			HasUp: quote.hasUp, HasDown: quote.hasDown,
			SourceUpdated: now,
		})
	}
	s.snapshots.PatchQuoteBatch(patches)
}

func (s *MarketStream) watchSubscription(
	ctx context.Context,
	subscriptionKey string,
	resubscribe *atomic.Bool,
) {
	ticker := time.NewTicker(subscriptionCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.refreshCh:
			if s.subscriptionFingerprint() != subscriptionKey {
				resubscribe.Store(true)
				return
			}
		case <-ticker.C:
			if s.subscriptionFingerprint() != subscriptionKey {
				resubscribe.Store(true)
				return
			}
		}
	}
}

func (s *MarketStream) subscriptionFingerprint() string {
	_, _, key := buildTokenSubscription(s.relevantMarkets())
	return key
}

func (s *MarketStream) relevantMarkets() []Market {
	now := time.Now().UTC()
	markets, _ := s.snapshots.ListMarkets("", "", true)
	result := make([]Market, 0, len(markets))
	for _, market := range markets {
		if now.After(market.WindowEnd) {
			continue
		}
		if market.WindowStart.After(now.Add(30 * time.Minute)) {
			continue
		}
		result = append(result, market)
	}
	return result
}

func buildTokenSubscription(markets []Market) (
	map[string]tokenMarketLink,
	[]string,
	string,
) {
	tokenMarkets := make(map[string]tokenMarketLink, len(markets)*2)
	assets := make([]string, 0, len(markets)*2)
	for _, market := range markets {
		if market.UpTokenID != "" {
			tokenMarkets[market.UpTokenID] = tokenMarketLink{market.ID, "up"}
			assets = append(assets, market.UpTokenID)
		}
		if market.DownTokenID != "" {
			tokenMarkets[market.DownTokenID] = tokenMarketLink{market.ID, "down"}
			assets = append(assets, market.DownTokenID)
		}
	}
	sorted := append([]string(nil), assets...)
	sort.Strings(sorted)
	return tokenMarkets, assets, strings.Join(sorted, ",")
}

func shardAssets(assets []string, maxPerShard int) [][]string {
	if maxPerShard <= 0 || len(assets) == 0 {
		return nil
	}
	shards := make([][]string, 0, (len(assets)+maxPerShard-1)/maxPerShard)
	for start := 0; start < len(assets); start += maxPerShard {
		end := start + maxPerShard
		if end > len(assets) {
			end = len(assets)
		}
		shards = append(shards, assets[start:end])
	}
	return shards
}

func normalizeAssetSymbol(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}
