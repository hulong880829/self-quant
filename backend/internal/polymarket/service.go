package polymarket

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	ErrNotFound             = errors.New("polymarket resource not found")
	ErrInvalidArgument      = errors.New("invalid polymarket argument")
	ErrMarketClosed         = errors.New("polymarket market is closed")
	ErrInsufficientFunds    = errors.New("insufficient balance")
	ErrInsufficientPosition = errors.New("insufficient position")
	ErrCancelRejected       = errors.New("order cannot be canceled")
)

type CredentialProvider interface {
	Get(context.Context, string, int64) (Credentials, string, error)
	Refresh(context.Context, string, int64) (Credentials, string, error)
	Invalidate(context.Context, string, int64) error
	Owner(context.Context, string) (string, error)
}

type cachedSummary struct {
	value     AccountSummary
	expiresAt time.Time
}

func isCLOBUnauthorized(err error) bool {
	var clobErr *CLOBError
	return errors.As(err, &clobErr) && clobErr.StatusCode == 401
}

func (s *Service) refreshAfterUnauthorized(
	ctx context.Context,
	token string,
	accountID int64,
) (Credentials, error) {
	value, err, _ := s.group.Do(fmt.Sprintf("credentials:%d", accountID), func() (any, error) {
		refreshed, _, refreshErr := s.credentials.Refresh(ctx, token, accountID)
		return refreshed, refreshErr
	})
	if err == nil {
		return value.(Credentials), nil
	}
	code := status.Code(err)
	if !errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, context.Canceled) &&
		code != codes.DeadlineExceeded &&
		code != codes.Unavailable {
		_ = s.credentials.Invalidate(context.WithoutCancel(ctx), token, accountID)
	}
	return Credentials{}, err
}

func (s *Service) staleCacheTTL() time.Duration {
	if s.cacheTTL < 30*time.Second {
		return 30 * time.Second
	}
	return s.cacheTTL
}

func callPrivateCLOB[T any](
	ctx context.Context,
	service *Service,
	token string,
	accountID int64,
	credentials Credentials,
	call func(Credentials) (T, error),
) (T, Credentials, error) {
	result, err := call(credentials)
	if !isCLOBUnauthorized(err) {
		return result, credentials, err
	}
	refreshed, refreshErr := service.refreshAfterUnauthorized(ctx, token, accountID)
	if refreshErr != nil {
		var zero T
		return zero, credentials, refreshErr
	}
	result, err = call(refreshed)
	if isCLOBUnauthorized(err) {
		_ = service.credentials.Invalidate(context.WithoutCancel(ctx), token, accountID)
	}
	return result, refreshed, err
}

type cachedPositions struct {
	value     []Position
	expiresAt time.Time
	stale     bool
}

type cachedOpenOrders struct {
	value     []OpenOrder
	expiresAt time.Time
	stale     bool
}

type chainlinkOpenCandidate struct {
	distance   time.Duration
	price      string
	observedAt time.Time
}

type Service struct {
	repository  *Repository
	gamma       *GammaClient
	clob        *CLOBClient
	data        *DataClient
	history     *HistoryClient
	credentials CredentialProvider
	snapshots   *SnapshotStore
	logger      *slog.Logger
	cacheTTL    time.Duration
	priceSample time.Duration
	limiter     *rate.Limiter

	cacheMu    sync.RWMutex
	summaries  map[int64]cachedSummary
	positions  map[int64]cachedPositions
	openOrders map[int64]cachedOpenOrders
	group      singleflight.Group

	priceMu        sync.Mutex
	lastPersisted  map[string]time.Time
	openPersisted  map[string]bool
	openCandidates map[string]chainlinkOpenCandidate
	orderMu        sync.Mutex
	orderLocks     map[int64]*sync.Mutex
	accountStreams *UserStreamManager
}

func NewService(
	repository *Repository,
	gamma *GammaClient,
	clob *CLOBClient,
	data *DataClient,
	history *HistoryClient,
	credentials CredentialProvider,
	snapshots *SnapshotStore,
	cacheTTL time.Duration,
	priceSample time.Duration,
	logger *slog.Logger,
) *Service {
	if priceSample <= 0 {
		priceSample = 5 * time.Second
	}
	return &Service{
		repository: repository, gamma: gamma, clob: clob, data: data, history: history,
		credentials: credentials, snapshots: snapshots, cacheTTL: cacheTTL,
		priceSample: priceSample, logger: logger,
		limiter:        rate.NewLimiter(rate.Limit(12), 24),
		summaries:      make(map[int64]cachedSummary),
		positions:      make(map[int64]cachedPositions),
		openOrders:     make(map[int64]cachedOpenOrders),
		orderLocks:     make(map[int64]*sync.Mutex),
		lastPersisted:  make(map[string]time.Time),
		openPersisted:  make(map[string]bool),
		openCandidates: make(map[string]chainlinkOpenCandidate),
	}
}

func (s *Service) Hydrate(ctx context.Context) error {
	markets, err := s.repository.ListMarkets(ctx)
	if err != nil {
		return err
	}
	s.snapshots.ReplaceMarkets(markets)
	for _, market := range markets {
		points, loadErr := s.repository.LoadRecentPoints(ctx, market.ID, market.WindowStart)
		if loadErr != nil {
			return loadErr
		}
		if len(points) == 0 {
			continue
		}
		points = trimSeries(points, MaxChartPoints*4)
		last := points[len(points)-1]
		s.snapshots.Update(Snapshot{
			Market: market, OpenPrice: points[0].OpenPrice,
			ChainlinkPrice: last.ChainlinkPrice, Series: points,
			SourceUpdated: last.Timestamp,
			Stale:         time.Since(last.Timestamp) > time.Minute,
		})
	}
	return nil
}

func (s *Service) SyncMarkets(ctx context.Context) error {
	if err := s.limiter.Wait(ctx); err != nil {
		return err
	}
	markets, err := s.gamma.ListCryptoMarkets(ctx)
	if err != nil {
		s.logger.Warn("market sync failed; keeping cached markets", "error", err)
		return nil
	}
	if err := s.repository.UpsertMarkets(ctx, markets); err != nil {
		return err
	}
	s.snapshots.ReplaceMarkets(markets)
	s.applyGammaOpenPrices(ctx, markets)
	s.backfillMissingOpens(ctx, markets)
	return nil
}

func (s *Service) applyGammaOpenPrices(ctx context.Context, markets []Market) {
	for _, market := range markets {
		s.applyGammaOpenPrice(ctx, market)
	}
}

func (s *Service) applyGammaOpenPrice(ctx context.Context, market Market) {
	if market.GammaOpenPrice == "" {
		return
	}
	now := time.Now()
	if now.Before(market.WindowStart) || now.After(market.WindowEnd) {
		return
	}
	snapshot, _ := s.snapshots.Get(market.ID)
	if snapshot.OpenPrice != "" {
		return
	}
	snapshot.Market = market
	snapshot.OpenPrice = market.GammaOpenPrice
	snapshot.SourceUpdated = time.Now().UTC()
	snapshot.Stale = false
	point := PricePoint{
		Timestamp:      market.WindowStart,
		OpenPrice:      market.GammaOpenPrice,
		ChainlinkPrice: snapshot.ChainlinkPrice,
	}
	if len(snapshot.Series) == 0 {
		snapshot.Series = []PricePoint{point}
	} else if snapshot.Series[0].OpenPrice == "" {
		snapshot.Series[0].OpenPrice = market.GammaOpenPrice
	} else {
		snapshot.Series = trimSeries(
			append([]PricePoint{point}, snapshot.Series...),
			MaxChartPoints*4,
		)
	}
	s.snapshots.Update(snapshot)
	if s.repository == nil || s.isOpenPersisted(market.ID) {
		return
	}
	if err := s.repository.InsertPricePoint(ctx, market.ID, point); err != nil {
		s.logger.Warn("persist gamma Open failed", "market_id", market.ID, "error", err)
		return
	}
	s.markOpenPersisted(market.ID, market.WindowStart)
}

func (s *Service) backfillMissingOpens(ctx context.Context, markets []Market) {
	if s.history == nil || !s.history.Enabled() {
		return
	}
	now := time.Now()
	for _, market := range markets {
		if now.Before(market.WindowStart) || now.After(market.WindowEnd) {
			continue
		}
		snapshot, _ := s.snapshots.Get(market.ID)
		if snapshot.OpenPrice != "" {
			continue
		}
		price, observedAt, err := s.history.PriceAt(ctx, market.Asset, market.WindowStart)
		if err != nil {
			s.logger.Warn("chainlink Open backfill failed", "market_id", market.ID, "error", err)
			continue
		}
		delta := observedAt.Sub(market.WindowStart)
		if delta < 0 {
			delta = -delta
		}
		if delta > 5*time.Second {
			s.logger.Warn("chainlink Open backfill outside boundary", "market_id", market.ID)
			continue
		}
		snapshot.Market = market
		snapshot.OpenPrice = price
		point := PricePoint{
			Timestamp: observedAt, OpenPrice: price,
			ChainlinkPrice: snapshot.ChainlinkPrice,
		}
		snapshot.Series = trimSeries(
			append([]PricePoint{point}, snapshot.Series...),
			MaxChartPoints*4,
		)
		s.snapshots.Update(snapshot)
		if err := s.repository.InsertPricePoint(ctx, market.ID, point); err != nil {
			s.logger.Warn("persist backfilled Open failed", "market_id", market.ID, "error", err)
		}
	}
}

func (s *Service) RefreshQuotes(ctx context.Context) error {
	markets, _ := s.snapshots.ListMarkets("", "", true)
	for _, market := range markets {
		if time.Now().After(market.WindowEnd) {
			continue
		}
		if err := s.limiter.Wait(ctx); err != nil {
			return err
		}
		upBid, upAsk, err := s.clob.BestQuotes(ctx, market.UpTokenID)
		if err != nil {
			s.logger.Warn("polymarket quote refresh failed", "market_id", market.ID, "error", err)
			continue
		}
		downBid, downAsk, err := s.clob.BestQuotes(ctx, market.DownTokenID)
		if err != nil {
			s.logger.Warn("polymarket quote refresh failed", "market_id", market.ID, "error", err)
			continue
		}
		now := time.Now().UTC()
		s.snapshots.PatchQuotes(market.ID, func(snapshot *Snapshot) bool {
			if snapshot.UpBid == upBid && snapshot.UpAsk == upAsk &&
				snapshot.DownBid == downBid && snapshot.DownAsk == downAsk {
				return false
			}
			snapshot.UpBid, snapshot.UpAsk = upBid, upAsk
			snapshot.DownBid, snapshot.DownAsk = downBid, downAsk
			snapshot.SourceUpdated = now
			snapshot.Stale = false
			return true
		})
	}
	return nil
}

func (s *Service) RecordChainlinkPrice(
	ctx context.Context,
	asset, price string,
	observedAt time.Time,
) {
	markets, _ := s.snapshots.ListMarkets(strings.ToUpper(asset), "", true)
	for _, market := range markets {
		if observedAt.Before(market.WindowStart) || !observedAt.Before(market.WindowEnd) {
			continue
		}
		var shouldPersistOpen bool
		var point PricePoint
		changed, openPrice, delta := s.snapshots.PatchChainlinkPrice(
			market.ID,
			func(snapshot *Snapshot) (bool, []PricePoint) {
				snapshot.Market = market
				openBefore := snapshot.OpenPrice
				_, shouldPersistOpen = s.tryUpdateChainlinkOpen(
					market, snapshot, price, observedAt,
				)
				needSample := len(snapshot.Series) == 0 ||
					observedAt.Sub(snapshot.Series[len(snapshot.Series)-1].Timestamp) >= time.Second
				scalarChanged := snapshot.ChainlinkPrice != price ||
					snapshot.Stale ||
					!snapshot.SourceUpdated.Equal(observedAt) ||
					snapshot.OpenPrice != openBefore
				if !scalarChanged && !needSample {
					return false, nil
				}
				snapshot.ChainlinkPrice = price
				snapshot.SourceUpdated = observedAt
				snapshot.Stale = false
				point = PricePoint{
					Timestamp: observedAt, OpenPrice: snapshot.OpenPrice, ChainlinkPrice: price,
				}
				if !needSample {
					return true, nil
				}
				series := trimSeries(
					append(append([]PricePoint(nil), snapshot.Series...), point),
					MaxChartPoints*4,
				)
				snapshot.Series = series
				return true, []PricePoint{point}
			},
		)
		if !changed {
			continue
		}
		if openPrice != "" {
			point.OpenPrice = openPrice
		}
		persistOpen := shouldPersistOpen && !s.isOpenPersisted(market.ID) && openPrice != ""
		if persistOpen {
			if s.repository == nil {
				s.markOpenPersisted(market.ID, observedAt)
			} else if err := s.repository.InsertPricePoint(ctx, market.ID, point); err != nil {
				s.logger.Warn("persist chainlink open failed", "market_id", market.ID, "error", err)
			} else {
				s.markOpenPersisted(market.ID, observedAt)
			}
			continue
		}
		if len(delta) == 0 || !s.shouldPersistSample(market.ID, observedAt) {
			continue
		}
		if s.repository == nil {
			continue
		}
		if err := s.repository.InsertPricePoint(ctx, market.ID, point); err != nil {
			s.logger.Warn("persist chainlink price failed", "market_id", market.ID, "error", err)
		} else {
			s.markSamplePersisted(market.ID, observedAt)
		}
	}
}

func (s *Service) tryUpdateChainlinkOpen(
	market Market,
	snapshot *Snapshot,
	price string,
	observedAt time.Time,
) (openUpdated bool, shouldPersistOpen bool) {
	if s.isOpenPersisted(market.ID) {
		return false, false
	}
	lockEnd := market.WindowStart.Add(5 * time.Second)
	if !observedAt.Before(lockEnd) {
		return false, snapshot.OpenPrice != ""
	}
	if observedAt.Before(market.WindowStart) {
		return false, false
	}
	distance := observedAt.Sub(market.WindowStart)
	s.priceMu.Lock()
	candidate, hasCandidate := s.openCandidates[market.ID]
	if hasCandidate && snapshot.OpenPrice == "" {
		snapshot.OpenPrice = candidate.price
	}
	if hasCandidate && candidate.distance <= distance {
		s.priceMu.Unlock()
		return false, snapshot.OpenPrice != ""
	}
	s.openCandidates[market.ID] = chainlinkOpenCandidate{
		distance: distance, price: price, observedAt: observedAt,
	}
	s.priceMu.Unlock()
	snapshot.OpenPrice = price
	return true, false
}

func (s *Service) isOpenPersisted(marketID string) bool {
	s.priceMu.Lock()
	defer s.priceMu.Unlock()
	return s.openPersisted[marketID]
}

func (s *Service) markOpenPersisted(marketID string, observedAt time.Time) {
	s.priceMu.Lock()
	defer s.priceMu.Unlock()
	s.openPersisted[marketID] = true
	s.lastPersisted[marketID] = observedAt
}

func (s *Service) shouldPersistSample(marketID string, observedAt time.Time) bool {
	s.priceMu.Lock()
	defer s.priceMu.Unlock()
	last, ok := s.lastPersisted[marketID]
	if ok && observedAt.Sub(last) < s.priceSample {
		return false
	}
	return true
}

func (s *Service) markSamplePersisted(marketID string, observedAt time.Time) {
	s.priceMu.Lock()
	defer s.priceMu.Unlock()
	s.lastPersisted[marketID] = observedAt
}

func (s *Service) forgetMarketPriceState(marketIDs []string) {
	if len(marketIDs) == 0 {
		return
	}
	s.priceMu.Lock()
	defer s.priceMu.Unlock()
	for _, marketID := range marketIDs {
		delete(s.openPersisted, marketID)
		delete(s.lastPersisted, marketID)
		delete(s.openCandidates, marketID)
	}
}

func (s *Service) PruneMemory(before time.Time) int {
	removed := s.snapshots.PruneExpired(before)
	s.forgetMarketPriceState(removed)
	return len(removed)
}

func (s *Service) GetAccountSummary(
	ctx context.Context,
	token string,
	accountID int64,
) (AccountSummary, error) {
	now := time.Now()
	s.cacheMu.RLock()
	cached, ok := s.summaries[accountID]
	s.cacheMu.RUnlock()
	if ok && now.Before(cached.expiresAt) {
		return cached.value, nil
	}
	value, err, _ := s.group.Do(fmt.Sprintf("summary:%d", accountID), func() (any, error) {
		if waitErr := s.limiter.Wait(ctx); waitErr != nil {
			return AccountSummary{}, waitErr
		}
		credentials, accountName, credentialErr := s.credentials.Get(ctx, token, accountID)
		if credentialErr != nil {
			return AccountSummary{}, credentialErr
		}
		var balance, positionValue string
		var refreshedCredentials Credentials
		var balanceErr, valueErr error
		var fetches sync.WaitGroup
		fetches.Add(2)
		go func() {
			defer fetches.Done()
			balance, refreshedCredentials, balanceErr = callPrivateCLOB(
				ctx, s, token, accountID, credentials,
				func(current Credentials) (string, error) {
					return s.clob.CollateralBalance(ctx, current)
				},
			)
		}()
		go func() {
			defer fetches.Done()
			positionValue, valueErr = s.data.AccountValue(ctx, credentials.FunderAddress)
		}()
		fetches.Wait()
		if balanceErr == nil {
			credentials = refreshedCredentials
		}
		if balanceErr != nil || valueErr != nil {
			if ok {
				fallback := cached.value
				fallback.Stale = true
				s.cacheMu.Lock()
				s.summaries[accountID] = cachedSummary{
					value: fallback, expiresAt: time.Now().Add(s.staleCacheTTL()),
				}
				s.cacheMu.Unlock()
				return fallback, nil
			}
			if fallback, loadErr := s.repository.LoadAccountSummary(ctx, accountID); loadErr == nil {
				fallback.AccountName = accountName
				fallback.WalletAddress = credentials.FunderAddress
				fallback.Stale = true
				s.cacheMu.Lock()
				s.summaries[accountID] = cachedSummary{
					value: fallback, expiresAt: time.Now().Add(s.staleCacheTTL()),
				}
				s.cacheMu.Unlock()
				return fallback, nil
			}
			if balanceErr != nil {
				return AccountSummary{}, balanceErr
			}
			return AccountSummary{}, valueErr
		}
		balanceDecimal, _ := decimal.NewFromString(balance)
		positionDecimal, _ := decimal.NewFromString(positionValue)
		summary := AccountSummary{
			TradingAccountID: accountID, AccountName: accountName,
			WalletAddress:    credentials.FunderAddress,
			AvailableBalance: balanceDecimal.String(),
			PositionValue:    positionDecimal.String(),
			TotalAssets:      balanceDecimal.Add(positionDecimal).String(),
			SourceUpdatedAt:  time.Now().UTC(),
		}
		if persistErr := s.repository.UpsertAccountSummary(ctx, summary); persistErr != nil {
			s.logger.Warn("persist account summary failed", "account_id", accountID, "error", persistErr)
		}
		s.cacheMu.Lock()
		s.summaries[accountID] = cachedSummary{value: summary, expiresAt: time.Now().Add(s.cacheTTL)}
		s.cacheMu.Unlock()
		return summary, nil
	})
	if err != nil {
		return AccountSummary{}, err
	}
	return value.(AccountSummary), nil
}

func (s *Service) ListPositions(
	ctx context.Context,
	token string,
	accountID int64,
	force bool,
) ([]Position, bool, error) {
	now := time.Now()
	s.cacheMu.RLock()
	cached, ok := s.positions[accountID]
	s.cacheMu.RUnlock()
	if !force && ok && now.Before(cached.expiresAt) {
		return filterLivePositions(cached.value), cached.stale, nil
	}
	value, err, _ := s.group.Do(fmt.Sprintf("positions:%d", accountID), func() (any, error) {
		if waitErr := s.limiter.Wait(ctx); waitErr != nil {
			return cachedPositions{}, waitErr
		}
		credentials, _, credentialErr := s.credentials.Get(ctx, token, accountID)
		if credentialErr != nil {
			return cachedPositions{}, credentialErr
		}
		positions, listErr := s.data.ListPositions(ctx, credentials.FunderAddress)
		if listErr != nil {
			if ok {
				fallback := cached
				fallback.stale = true
				fallback.expiresAt = time.Now().Add(s.staleCacheTTL())
				s.cacheMu.Lock()
				s.positions[accountID] = fallback
				s.cacheMu.Unlock()
				return fallback, nil
			}
			if stored, loadErr := s.repository.LoadPositions(ctx, accountID); loadErr == nil {
				fallback := cachedPositions{
					value: stored, stale: true,
					expiresAt: time.Now().Add(s.staleCacheTTL()),
				}
				s.cacheMu.Lock()
				s.positions[accountID] = fallback
				s.cacheMu.Unlock()
				return fallback, nil
			}
			return cachedPositions{}, listErr
		}
		for index := range positions {
			positions[index].TradingAccountID = accountID
		}
		if persistErr := s.repository.UpsertPositions(ctx, accountID, positions); persistErr != nil {
			s.logger.Warn("persist positions failed", "account_id", accountID, "error", persistErr)
		}
		next := cachedPositions{
			value:     append([]Position(nil), positions...),
			expiresAt: time.Now().Add(s.cacheTTL),
		}
		s.cacheMu.Lock()
		s.positions[accountID] = next
		s.cacheMu.Unlock()
		return next, nil
	})
	if err != nil {
		return nil, false, err
	}
	result := value.(cachedPositions)
	return filterLivePositions(result.value), result.stale, nil
}

func filterLivePositions(positions []Position) []Position {
	result := make([]Position, 0, len(positions))
	for _, position := range positions {
		if position.Redeemable {
			continue
		}
		currentPrice, err := decimal.NewFromString(position.CurrentPrice)
		if err != nil || !currentPrice.GreaterThan(decimal.Zero) {
			continue
		}
		result = append(result, position)
	}
	return result
}

func (s *Service) ListOpenOrders(
	ctx context.Context,
	token string,
	accountID int64,
	force bool,
) ([]OpenOrder, bool, error) {
	now := time.Now()
	s.cacheMu.RLock()
	cached, ok := s.openOrders[accountID]
	s.cacheMu.RUnlock()
	if !force && ok && now.Before(cached.expiresAt) {
		return append([]OpenOrder(nil), cached.value...), cached.stale, nil
	}
	value, err, _ := s.group.Do(fmt.Sprintf("open-orders:%d", accountID), func() (any, error) {
		if waitErr := s.limiter.Wait(ctx); waitErr != nil {
			return cachedOpenOrders{}, waitErr
		}
		credentials, _, credentialErr := s.credentials.Get(ctx, token, accountID)
		if credentialErr != nil {
			return cachedOpenOrders{}, credentialErr
		}
		orders, _, listErr := callPrivateCLOB(
			ctx, s, token, accountID, credentials,
			func(current Credentials) ([]OpenOrder, error) {
				return s.clob.ListOpenOrders(ctx, current)
			},
		)
		if listErr != nil {
			if ok {
				fallback := cached
				fallback.stale = true
				fallback.expiresAt = time.Now().Add(s.staleCacheTTL())
				s.cacheMu.Lock()
				s.openOrders[accountID] = fallback
				s.cacheMu.Unlock()
				return fallback, nil
			}
			if stored, loadErr := s.repository.LoadOpenOrders(ctx, accountID); loadErr == nil {
				s.enrichOpenOrders(stored)
				fallback := cachedOpenOrders{
					value: stored, stale: true,
					expiresAt: time.Now().Add(s.staleCacheTTL()),
				}
				s.cacheMu.Lock()
				s.openOrders[accountID] = fallback
				s.cacheMu.Unlock()
				return fallback, nil
			}
			return cachedOpenOrders{}, listErr
		}
		s.enrichOpenOrders(orders)
		next := cachedOpenOrders{
			value:     append([]OpenOrder(nil), orders...),
			expiresAt: time.Now().Add(s.cacheTTL),
		}
		s.cacheMu.Lock()
		s.openOrders[accountID] = next
		s.cacheMu.Unlock()
		return next, nil
	})
	if err != nil {
		return nil, false, err
	}
	result := value.(cachedOpenOrders)
	return append([]OpenOrder(nil), result.value...), result.stale, nil
}

func (s *Service) enrichOpenOrders(orders []OpenOrder) {
	markets, _ := s.snapshots.ListMarkets("", "", false)
	for index := range orders {
		for _, market := range markets {
			if market.ConditionID != orders[index].ConditionID &&
				market.UpTokenID != orders[index].TokenID &&
				market.DownTokenID != orders[index].TokenID {
				continue
			}
			orders[index].MarketTitle = market.Title
			if orders[index].TokenID == market.UpTokenID {
				orders[index].Outcome = "up"
			} else if orders[index].TokenID == market.DownTokenID {
				orders[index].Outcome = "down"
			}
			break
		}
	}
}

func (s *Service) reconcileSubmissionUnknown(
	ctx context.Context,
	accountID int64,
	openOrders []OpenOrder,
	trades []Trade,
) {
	if s.repository == nil {
		return
	}
	unknown, err := s.repository.ListSubmissionUnknown(ctx, accountID)
	if err != nil {
		s.logger.Warn("load unknown polymarket submissions failed", "account_id", accountID, "error", err)
		return
	}
	for _, local := range unknown {
		type candidate struct {
			id, status, filled, price, orderType string
		}
		candidates := make(map[string]candidate)
		for _, open := range openOrders {
			if submissionCandidateMatches(local, open.TokenID, open.Side, open.CreatedAt) {
				status := "open"
				if matched, parseErr := decimal.NewFromString(open.MatchedSize); parseErr == nil &&
					matched.GreaterThan(decimal.Zero) {
					status = "partially_filled"
				}
				candidates[open.ID] = candidate{
					id: open.ID, status: status, filled: open.MatchedSize,
					price: open.Price, orderType: open.OrderType,
				}
			}
		}
		for _, trade := range trades {
			if !submissionCandidateMatches(local, trade.AssetID, trade.Side, trade.MatchedAt) {
				continue
			}
			current := candidates[trade.OrderID]
			current.id = trade.OrderID
			current.status = "filled"
			current.price = trade.Price
			filled, _ := decimal.NewFromString(current.filled)
			size, _ := decimal.NewFromString(trade.Size)
			current.filled = filled.Add(size).String()
			candidates[trade.OrderID] = current
		}
		if len(candidates) != 1 {
			continue
		}
		for _, resolved := range candidates {
			if resolved.id == "" {
				continue
			}
			if _, updateErr := s.repository.UpdateOrderResult(
				ctx, local.ID, resolved.id, resolved.status,
				zeroIfEmpty(resolved.filled), zeroIfEmpty(resolved.price),
				resolved.orderType, "", "",
			); updateErr != nil {
				s.logger.Warn(
					"resolve unknown polymarket submission failed",
					"order_id", local.ID, "account_id", accountID, "error", updateErr,
				)
			}
		}
	}
}

func submissionCandidateMatches(
	local Order,
	tokenID, side string,
	observedAt time.Time,
) bool {
	if local.TokenID != tokenID || !strings.EqualFold(local.Side, side) || observedAt.IsZero() {
		return false
	}
	return !observedAt.Before(local.CreatedAt.Add(-5*time.Second)) &&
		!observedAt.After(local.CreatedAt.Add(2*time.Minute))
}

func (s *Service) replaceCachedOpenOrders(accountID int64, orders []OpenOrder) {
	s.cacheMu.Lock()
	s.openOrders[accountID] = cachedOpenOrders{
		value: append([]OpenOrder(nil), orders...), expiresAt: time.Now().Add(s.cacheTTL),
	}
	s.cacheMu.Unlock()
}

func (s *Service) cachedOpenOrders(accountID int64) []OpenOrder {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	return append([]OpenOrder(nil), s.openOrders[accountID].value...)
}

func (s *Service) applyOpenOrderEvent(
	accountID int64,
	order OpenOrder,
	live bool,
) []OpenOrder {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	cached := s.openOrders[accountID]
	orders := make([]OpenOrder, 0, len(cached.value)+1)
	found := false
	for _, existing := range cached.value {
		if existing.ID != order.ID {
			orders = append(orders, existing)
			continue
		}
		found = true
		if live {
			orders = append(orders, order)
		}
	}
	if live && !found {
		orders = append([]OpenOrder{order}, orders...)
	}
	cached.value = orders
	cached.expiresAt = time.Now().Add(s.cacheTTL)
	cached.stale = false
	s.openOrders[accountID] = cached
	return append([]OpenOrder(nil), orders...)
}

func (s *Service) invalidatePortfolio(accountID int64) {
	s.cacheMu.Lock()
	delete(s.summaries, accountID)
	delete(s.positions, accountID)
	s.cacheMu.Unlock()
}

func (s *Service) CancelOrder(
	ctx context.Context,
	token string,
	accountID int64,
	orderID string,
) (CancelResult, error) {
	orderID = strings.TrimSpace(orderID)
	if strings.TrimSpace(token) == "" || accountID <= 0 || orderID == "" {
		return CancelResult{}, ErrInvalidArgument
	}
	lock := s.accountOrderLock(accountID)
	lock.Lock()
	defer lock.Unlock()

	credentials, _, err := s.credentials.Get(ctx, token, accountID)
	if err != nil {
		return CancelResult{}, err
	}
	if err := s.limiter.Wait(ctx); err != nil {
		return CancelResult{}, err
	}
	result, _, err := callPrivateCLOB(
		ctx, s, token, accountID, credentials,
		func(current Credentials) (CancelResult, error) {
			return s.clob.CancelOrder(ctx, current, orderID)
		},
	)
	if err != nil {
		if result.Status == "not_canceled" && result.Message != "" {
			return result, fmt.Errorf("%w: %s", ErrCancelRejected, result.Message)
		}
		return result, err
	}
	if persistErr := s.repository.MarkOrderCanceled(ctx, accountID, orderID); persistErr != nil {
		s.logger.Warn(
			"persist canceled polymarket order failed",
			"account_id", accountID, "clob_order_id", orderID, "error", persistErr,
		)
	}
	s.removeCachedOpenOrder(accountID, orderID)
	_, _, _ = s.ListOpenOrders(ctx, token, accountID, true)
	s.orderLogger().Info(
		"polymarket order canceled", "account_id", accountID, "clob_order_id", orderID,
	)
	return result, nil
}

func (s *Service) removeCachedOpenOrder(accountID int64, orderID string) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	cached, ok := s.openOrders[accountID]
	if !ok {
		return
	}
	orders := make([]OpenOrder, 0, len(cached.value))
	for _, order := range cached.value {
		if order.ID != orderID {
			orders = append(orders, order)
		}
	}
	cached.value = orders
	cached.expiresAt = time.Now().Add(s.cacheTTL)
	cached.stale = false
	s.openOrders[accountID] = cached
}

type PlaceOrderInput struct {
	Token            string
	TradingAccountID int64
	MarketID         string
	Outcome          string
	Side             string
	Amount           string
	AmountUnit       string
	IdempotencyKey   string
	ExecutionType    string
	LimitPrice       string
}

func (s *Service) PlaceOrder(ctx context.Context, input PlaceOrderInput) (Order, error) {
	input.Outcome = strings.ToLower(strings.TrimSpace(input.Outcome))
	input.Side = strings.ToLower(strings.TrimSpace(input.Side))
	input.AmountUnit = strings.ToLower(strings.TrimSpace(input.AmountUnit))
	input.ExecutionType = strings.ToLower(strings.TrimSpace(input.ExecutionType))
	input.LimitPrice = strings.TrimSpace(input.LimitPrice)
	if input.ExecutionType == "" {
		input.ExecutionType = "book"
	}
	clobOrderType := "FAK"
	if input.ExecutionType == "limit" {
		clobOrderType = "GTC"
	}
	if input.TradingAccountID <= 0 || input.MarketID == "" ||
		(input.Outcome != "up" && input.Outcome != "down") ||
		(input.Side != "buy" && input.Side != "sell") ||
		(input.AmountUnit != "usd" && input.AmountUnit != "shares") ||
		(input.ExecutionType != "book" && input.ExecutionType != "limit") ||
		(input.ExecutionType == "book" && input.LimitPrice != "") ||
		(input.ExecutionType == "limit" && (input.AmountUnit != "shares" ||
			input.LimitPrice == "")) ||
		strings.TrimSpace(input.IdempotencyKey) == "" {
		return Order{}, ErrInvalidArgument
	}
	amount, err := decimal.NewFromString(input.Amount)
	if err != nil || amount.LessThanOrEqual(decimal.Zero) {
		return Order{}, ErrInvalidArgument
	}
	limitPrice := decimal.Zero
	if input.ExecutionType == "limit" {
		limitPrice, err = decimal.NewFromString(input.LimitPrice)
		if err != nil || !limitPrice.GreaterThan(decimal.Zero) ||
			!limitPrice.LessThan(decimal.NewFromInt(1)) {
			return Order{}, ErrInvalidArgument
		}
	}
	snapshot, ok := s.snapshots.Get(input.MarketID)
	if !ok {
		s.orderLogger().Warn(
			"polymarket order rejected before intent",
			"account_id", input.TradingAccountID, "market_id", input.MarketID,
			"reason", "market_not_found",
		)
		return Order{}, ErrNotFound
	}
	now := time.Now()
	if !snapshot.Market.Active || now.Before(snapshot.Market.WindowStart) ||
		!now.Before(snapshot.Market.WindowEnd) {
		s.orderLogger().Warn(
			"polymarket order rejected before intent",
			"account_id", input.TradingAccountID, "market_id", input.MarketID,
			"reason", "market_closed",
		)
		return Order{}, ErrMarketClosed
	}
	tokenID := snapshot.Market.UpTokenID
	if input.Outcome == "down" {
		tokenID = snapshot.Market.DownTokenID
	}
	intent, created, err := s.repository.CreateOrderIntent(ctx, Order{
		IdempotencyKey: input.IdempotencyKey, TradingAccountID: input.TradingAccountID,
		MarketID: input.MarketID, TokenID: tokenID, Outcome: input.Outcome,
		Side: input.Side, RequestedAmount: amount.String(), AmountUnit: input.AmountUnit,
		ExecutionType: input.ExecutionType, LimitPrice: input.LimitPrice,
		CLOBOrderType: clobOrderType,
	})
	if err != nil || !created {
		return intent, err
	}
	lock := s.accountOrderLock(input.TradingAccountID)
	lock.Lock()
	defer lock.Unlock()
	credentials, _, err := s.credentials.Get(ctx, input.Token, input.TradingAccountID)
	if err != nil {
		return s.rejectOrder(ctx, intent.ID, "credentials_unavailable", err)
	}
	if input.Side == "buy" {
		summary, summaryErr := s.GetAccountSummary(
			ctx, input.Token, input.TradingAccountID,
		)
		if summaryErr != nil {
			return s.rejectOrder(ctx, intent.ID, "balance_unavailable", summaryErr)
		}
		available, parseErr := decimal.NewFromString(summary.AvailableBalance)
		required := amount
		if input.ExecutionType == "limit" {
			required = amount.Mul(limitPrice)
		}
		if parseErr != nil || available.LessThan(required) {
			return s.rejectOrder(
				ctx, intent.ID, "insufficient_balance", ErrInsufficientFunds,
			)
		}
	}
	if input.Side == "sell" && input.AmountUnit == "shares" {
		positions, _, positionsErr := s.ListPositions(
			ctx, input.Token, input.TradingAccountID, false,
		)
		if positionsErr != nil {
			return s.rejectOrder(ctx, intent.ID, "positions_unavailable", positionsErr)
		}
		available := decimal.Zero
		for _, position := range positions {
			if position.TokenID == tokenID {
				available, _ = decimal.NewFromString(position.Size)
				break
			}
		}
		if available.LessThan(amount) {
			return s.rejectOrder(
				ctx, intent.ID, "insufficient_position", ErrInsufficientPosition,
			)
		}
	}
	if err := s.limiter.Wait(ctx); err != nil {
		return s.rejectOrder(ctx, intent.ID, "rate_limited", err)
	}
	submission, _, err := callPrivateCLOB(
		ctx, s, input.Token, input.TradingAccountID, credentials,
		func(current Credentials) (OrderSubmission, error) {
			return s.clob.PlaceOrder(
				ctx, current, snapshot.Market, tokenID,
				input.Side, amount.String(), input.AmountUnit,
				input.ExecutionType, input.LimitPrice,
			)
		},
	)
	if err != nil {
		if isUnknownOrderSubmission(err) {
			order, updateErr := s.repository.UpdateOrderResult(
				ctx, intent.ID, "", "submission_unknown", "0", "0",
				clobOrderType, "submission_unknown",
				"request sent; awaiting private order/trade reconciliation",
			)
			if updateErr != nil {
				return Order{}, updateErr
			}
			s.orderLogger().Warn(
				"polymarket order submission outcome unknown",
				"order_id", order.ID, "account_id", order.TradingAccountID,
				"market_id", order.MarketID,
			)
			return order, nil
		}
		code, message := clobFailure(err)
		return s.rejectOrderDetailed(ctx, intent.ID, code, message, err)
	}
	order, err := s.repository.UpdateOrderResult(
		ctx, intent.ID, submission.OrderID, submission.Status,
		zeroIfEmpty(submission.FilledSize), zeroIfEmpty(submission.AveragePrice),
		submission.OrderType, submission.ErrorCode, submission.ErrorCode,
	)
	if err != nil {
		return Order{}, err
	}
	s.orderLogger().Info(
		"polymarket order submitted",
		"account_id", input.TradingAccountID, "market_id", input.MarketID,
		"side", input.Side, "outcome", input.Outcome,
		"status", order.Status, "clob_order_id", order.CLOBOrderID,
	)
	if order.Status == "filled" || order.Status == "partially_filled" {
		s.cacheMu.Lock()
		delete(s.summaries, input.TradingAccountID)
		delete(s.positions, input.TradingAccountID)
		s.cacheMu.Unlock()
		_, _, _ = s.ListPositions(ctx, input.Token, input.TradingAccountID, true)
	}
	return order, nil
}

func isUnknownOrderSubmission(err error) bool {
	var transportErr *TransportError
	return errors.As(err, &transportErr) &&
		transportErr.Method == "POST" &&
		transportErr.RequestWritten
}

func (s *Service) GetOrder(
	ctx context.Context,
	token, orderID string,
) (Order, error) {
	if token == "" || orderID == "" {
		return Order{}, ErrInvalidArgument
	}
	owner, err := s.credentials.Owner(ctx, token)
	if err != nil {
		return Order{}, err
	}
	return s.repository.GetOrderByOwner(ctx, owner, orderID)
}

func (s *Service) rejectOrder(
	ctx context.Context,
	orderID, code string,
	cause error,
) (Order, error) {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	return s.rejectOrderDetailed(ctx, orderID, code, message, cause)
}

func (s *Service) rejectOrderDetailed(
	ctx context.Context,
	orderID, code, message string,
	cause error,
) (Order, error) {
	order, updateErr := s.repository.UpdateOrderResult(
		ctx, orderID, "", "rejected", "0", "0", "", code, message,
	)
	if updateErr != nil {
		return Order{}, updateErr
	}
	s.orderLogger().Warn(
		"polymarket order rejected",
		"order_id", order.ID, "account_id", order.TradingAccountID,
		"market_id", order.MarketID, "error_code", code, "cause", cause,
	)
	return order, cause
}

func clobFailure(err error) (string, string) {
	var upstream *CLOBError
	if errors.As(err, &upstream) {
		code := strings.ToLower(strings.TrimSpace(upstream.Code))
		if code == "" {
			code = "clob_rejected"
		}
		return code, upstream.Message
	}
	return "clob_submit_failed", err.Error()
}

func (s *Service) orderLogger() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

func (s *Service) accountOrderLock(accountID int64) *sync.Mutex {
	s.orderMu.Lock()
	defer s.orderMu.Unlock()
	lock := s.orderLocks[accountID]
	if lock == nil {
		lock = &sync.Mutex{}
		s.orderLocks[accountID] = lock
	}
	return lock
}

func zeroIfEmpty(value string) string {
	if strings.TrimSpace(value) == "" {
		return "0"
	}
	return value
}
