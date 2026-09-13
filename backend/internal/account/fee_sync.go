package account

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"
	"selfquant/backend/internal/account/portfolio"
)

const (
	feeMaxAttempts     = 5
	feeHighQueueSize   = 64
	feeLowQueueSize    = 256
	feeCacheWriteTries = 3
	dailyFeeLockName   = "trading_account_fee_sync"
)

type feePersistence interface {
	GetByID(context.Context, int64) (TradingAccountRecord, error)
	GetFeeRates(context.Context, int64) (FeeRatesRecord, error)
	UpdateFeeRates(context.Context, FeeRatesRecord) error
	ListFeeCacheRows(context.Context) ([]FeeRatesRecord, error)
	ListFeeSyncAccounts(context.Context) ([]TradingAccountRecord, error)
	ListPendingFeeSyncIDs(context.Context) ([]int64, error)
	TryDailyFeeSyncLock(context.Context, string) (func(), bool, error)
}

type feeVenue interface {
	AccountFeeRates(context.Context, string, portfolio.Credentials) (portfolio.AccountFeeRates, error)
}

type feeJob struct {
	id   int64
	high bool
	done chan struct{}
}

type FeeSync struct {
	store       feePersistence
	venues      feeVenue
	decrypt     func(TradingAccountRecord) (TradingCredentials, error)
	cache       *feeCache
	pool        *pgxpool.Pool
	logger      *slog.Logger
	now         func() time.Time
	location    *time.Location
	high        chan feeJob
	low         chan feeJob
	limiters    map[string]*rate.Limiter
	accountMu   sync.Map
	dailyClaim  sync.Mutex
	lastDaily   string
	dailyUnlock func()
}

func NewFeeSync(
	store feePersistence,
	venues feeVenue,
	decrypt func(TradingAccountRecord) (TradingCredentials, error),
	pool *pgxpool.Pool,
	logger *slog.Logger,
	now func() time.Time,
) *FeeSync {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	location, err := time.LoadLocation(shanghaiLocationName)
	if err != nil {
		location = time.FixedZone("CST", 8*3600)
	}
	limiters := map[string]*rate.Limiter{
		"binance":     rate.NewLimiter(rate.Limit(2), 1),
		"okx":         rate.NewLimiter(rate.Limit(1), 1),
		"bybit":       rate.NewLimiter(rate.Limit(1), 1),
		"bitget":      rate.NewLimiter(rate.Limit(1), 1),
		"gate":        rate.NewLimiter(rate.Limit(1), 1),
		"hyperliquid": rate.NewLimiter(rate.Limit(1), 1),
		"aster":       rate.NewLimiter(rate.Limit(1), 1),
		"lighter":     rate.NewLimiter(rate.Limit(1), 1),
	}
	return &FeeSync{
		store:    store,
		venues:   venues,
		decrypt:  decrypt,
		cache:    newFeeCache(now, location),
		pool:     pool,
		logger:   logger,
		now:      now,
		location: location,
		high:     make(chan feeJob, feeHighQueueSize),
		low:      make(chan feeJob, feeLowQueueSize),
		limiters: limiters,
	}
}

func (s *FeeSync) Warmup(ctx context.Context) error {
	if s == nil || s.store == nil {
		return nil
	}
	rows, err := s.store.ListFeeCacheRows(ctx)
	if err != nil {
		return err
	}
	s.cache.loadAll(rows)
	ids, err := s.store.ListPendingFeeSyncIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		s.Enqueue(id, false)
	}
	return nil
}

func (s *FeeSync) Run(ctx context.Context) {
	if s == nil {
		return
	}
	go s.listen(ctx)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-s.high:
			s.handle(ctx, job)
		case job := <-s.low:
			s.handle(ctx, job)
		case now := <-ticker.C:
			s.maybeRunDaily(ctx, now)
		}
	}
}

func (s *FeeSync) Enqueue(id int64, high bool) <-chan struct{} {
	done := make(chan struct{})
	job := feeJob{id: id, high: high, done: done}
	queue := s.low
	if high {
		queue = s.high
	}
	select {
	case queue <- job:
	default:
		go func() { queue <- job }()
	}
	return done
}

func (s *FeeSync) Fees(id int64) (TradingAccountFees, bool) {
	if s == nil {
		return TradingAccountFees{}, false
	}
	return s.cache.get(id)
}

func (s *FeeSync) Load(ctx context.Context, id int64) (TradingAccountFees, error) {
	if fees, ok := s.cache.get(id); ok {
		return fees, nil
	}
	record, err := s.store.GetFeeRates(ctx, id)
	if err != nil {
		return TradingAccountFees{}, err
	}
	fees := feeViewFromRecord(record)
	s.putCache(id, fees)
	return fees, nil
}

func (s *FeeSync) Drop(id int64) {
	if s == nil {
		return
	}
	s.cache.delete(id)
}

func (s *FeeSync) MarkPending(ctx context.Context, record TradingAccountRecord) error {
	existing, _ := s.store.GetFeeRates(ctx, record.ID)
	existing.ID = record.ID
	existing.Exchange = record.Exchange
	existing.SyncStatus = FeeSyncPending
	existing.SyncError = ""
	if err := s.store.UpdateFeeRates(ctx, existing); err != nil {
		return err
	}
	s.putCache(record.ID, feeViewFromRecord(existing))
	return nil
}

func (s *FeeSync) MarkUnsupported(ctx context.Context, record TradingAccountRecord) error {
	now := s.now().UTC()
	stored := FeeRatesRecord{
		ID:             record.ID,
		Exchange:       record.Exchange,
		SpotStatus:     portfolio.FeeStatusUnsupported,
		ContractStatus: portfolio.FeeStatusUnsupported,
		UpdatedAt:      &now,
		SyncStatus:     FeeSyncOK,
	}
	if err := s.store.UpdateFeeRates(ctx, stored); err != nil {
		return err
	}
	s.putCache(record.ID, feeViewFromRecord(stored))
	return nil
}

func (s *FeeSync) maybeRunDaily(ctx context.Context, now time.Time) {
	local := now.In(s.location)
	if local.Hour() != 8 {
		return
	}
	day := local.Format("2006-01-02")
	s.dailyClaim.Lock()
	if s.lastDaily == day {
		s.dailyClaim.Unlock()
		return
	}
	s.dailyClaim.Unlock()
	release, locked, err := s.store.TryDailyFeeSyncLock(ctx, day)
	if err != nil {
		s.logger.Error("daily fee sync lock failed", "error", sanitizeFeeError(err))
		return
	}
	if !locked {
		s.dailyClaim.Lock()
		s.lastDaily = day
		s.dailyClaim.Unlock()
		return
	}
	s.dailyClaim.Lock()
	if s.lastDaily == day {
		s.dailyClaim.Unlock()
		if release != nil {
			release()
		}
		return
	}
	if s.dailyUnlock != nil {
		s.dailyUnlock()
	}
	s.dailyUnlock = release
	s.lastDaily = day
	s.dailyClaim.Unlock()
	started := s.now()
	accounts, err := s.store.ListFeeSyncAccounts(ctx)
	if err != nil {
		s.logger.Error("list fee sync accounts failed", "error", sanitizeFeeError(err))
		return
	}
	skipped := 0
	queued := 0
	for index, account := range accounts {
		if strings.EqualFold(account.Exchange, "polymarket") {
			skipped++
			continue
		}
		delay := time.Duration(index%8) * 250 * time.Millisecond
		id := account.ID
		time.AfterFunc(delay, func() { s.Enqueue(id, false) })
		queued++
	}
	s.logger.Info("daily fee sync queued",
		"day", day, "queued", queued, "skipped", skipped,
		"elapsed_ms", s.now().Sub(started).Milliseconds(),
	)
}

func (s *FeeSync) handle(ctx context.Context, job feeJob) {
	defer closeDone(job.done)
	if err := s.syncAccount(ctx, job.id, job.high); err != nil && !errors.Is(err, ErrTradingAccountNotFound) {
		s.logger.Warn("fee sync failed",
			"account_id", job.id, "error", sanitizeFeeError(err),
		)
	}
}

func (s *FeeSync) syncAccount(ctx context.Context, id int64, high bool) error {
	mu := s.accountLock(id)
	mu.Lock()
	defer mu.Unlock()
	record, err := s.store.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if strings.EqualFold(record.Exchange, "polymarket") {
		return s.MarkUnsupported(ctx, record)
	}
	existing, existingErr := s.store.GetFeeRates(ctx, id)
	if existingErr != nil && !errors.Is(existingErr, ErrTradingAccountNotFound) {
		return existingErr
	}
	if existing.SyncStatus == FeeSyncCredentialInvalid {
		s.putCache(id, feeViewFromRecord(existing))
		return nil
	}
	credentials, err := s.decrypt(record)
	if err != nil {
		return s.persistFailure(ctx, record, existing, FeeSyncCredentialInvalid, err)
	}
	if err := s.waitLimiter(ctx, record.Exchange); err != nil {
		return err
	}
	rates, venueErr := s.queryWithRetry(ctx, record.Exchange, toPortfolioCredentials(credentials), high)
	if venueErr != nil && rates.Spot.Status == "" && rates.Contract.Status == "" {
		status := FeeSyncFailed
		if isRetryableFeeError(venueErr) {
			status = FeeSyncRetrying
		}
		if isCredentialFeeError(venueErr) {
			status = FeeSyncCredentialInvalid
		}
		return s.persistFailure(ctx, record, existing, status, venueErr)
	}
	merged := mergeFeeRates(record, existing, rates, venueErr, s.now().UTC())
	if err := s.store.UpdateFeeRates(ctx, merged); err != nil {
		return err
	}
	s.putCache(id, feeViewFromRecord(merged))
	return venueErr
}

func (s *FeeSync) queryWithRetry(
	ctx context.Context,
	exchange string,
	credentials portfolio.Credentials,
	high bool,
) (portfolio.AccountFeeRates, error) {
	attempts := feeMaxAttempts
	if high {
		attempts = 3
	}
	var last error
	var partial portfolio.AccountFeeRates
	var retryAfter time.Duration
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if retryAfter > 0 {
				timer := time.NewTimer(retryAfter)
				select {
				case <-ctx.Done():
					timer.Stop()
					return partial, ctx.Err()
				case <-timer.C:
				}
			} else if err := sleepFeeBackoff(ctx, attempt); err != nil {
				return partial, err
			}
			if err := s.waitLimiter(ctx, exchange); err != nil {
				return partial, err
			}
		}
		rates, err := s.venues.AccountFeeRates(ctx, exchange, credentials)
		if err == nil {
			return rates, nil
		}
		partial = rates
		last = err
		retryAfter = portfolio.HTTPRetryAfter(err)
		if !isRetryableFeeError(err) || isCredentialFeeError(err) {
			return rates, err
		}
	}
	return partial, last
}

func (s *FeeSync) persistFailure(
	ctx context.Context,
	record TradingAccountRecord,
	existing FeeRatesRecord,
	status string,
	cause error,
) error {
	existing.ID = record.ID
	existing.Exchange = record.Exchange
	existing.SyncStatus = status
	existing.SyncError = sanitizeFeeError(cause)
	if err := s.store.UpdateFeeRates(ctx, existing); err != nil {
		return err
	}
	s.putCache(record.ID, feeViewFromRecord(existing))
	return cause
}

func (s *FeeSync) putCache(id int64, fees TradingAccountFees) {
	for attempt := 0; attempt < feeCacheWriteTries; attempt++ {
		s.cache.put(id, fees)
		if stored, ok := s.cache.get(id); ok && stored.SyncStatus == fees.SyncStatus {
			return
		}
	}
}

func (s *FeeSync) waitLimiter(ctx context.Context, exchange string) error {
	limiter := s.limiters[strings.ToLower(strings.TrimSpace(exchange))]
	if limiter == nil {
		return nil
	}
	return limiter.Wait(ctx)
}

func (s *FeeSync) accountLock(id int64) *sync.Mutex {
	actual, _ := s.accountMu.LoadOrStore(id, &sync.Mutex{})
	return actual.(*sync.Mutex)
}

func (s *FeeSync) listen(ctx context.Context) {
	if s.pool == nil {
		return
	}
	for ctx.Err() == nil {
		if err := s.listenOnce(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warn("fee cache listen failed", "error", sanitizeFeeError(err))
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}

func (s *FeeSync) listenOnce(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN trading_account_fees"); err != nil {
		return err
	}
	for ctx.Err() == nil {
		notification, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		id, parseErr := strconv.ParseInt(strings.TrimSpace(notification.Payload), 10, 64)
		if parseErr != nil || id <= 0 {
			continue
		}
		record, loadErr := s.store.GetFeeRates(ctx, id)
		if loadErr != nil {
			if errors.Is(loadErr, ErrTradingAccountNotFound) {
				s.cache.delete(id)
			}
			continue
		}
		s.putCache(id, feeViewFromRecord(record))
	}
	return ctx.Err()
}

func mergeFeeRates(
	account TradingAccountRecord,
	existing FeeRatesRecord,
	rates portfolio.AccountFeeRates,
	venueErr error,
	now time.Time,
) FeeRatesRecord {
	merged := existing
	merged.ID = account.ID
	merged.Exchange = account.Exchange
	merged.Source = firstNonEmpty(rates.Source, existing.Source, "venue")
	merged.UpdatedAt = &now
	spot, spotOK := applyMarketFee(existing.SpotStatus, existing.SpotMaker, existing.SpotTaker, rates.Spot)
	contract, contractOK := applyMarketFee(existing.ContractStatus, existing.ContractMaker, existing.ContractTaker, rates.Contract)
	merged.SpotStatus, merged.SpotMaker, merged.SpotTaker = spot.Status, stringPtr(spot.Maker), stringPtr(spot.Taker)
	merged.ContractStatus, merged.ContractMaker, merged.ContractTaker = contract.Status, stringPtr(contract.Maker), stringPtr(contract.Taker)
	if venueErr == nil && spotOK && contractOK {
		merged.SyncStatus = FeeSyncOK
		merged.SyncError = ""
		return merged
	}
	if isCredentialFeeError(venueErr) {
		merged.SyncStatus = FeeSyncCredentialInvalid
	} else if isRetryableFeeError(venueErr) {
		merged.SyncStatus = FeeSyncRetrying
	} else {
		merged.SyncStatus = FeeSyncFailed
	}
	merged.SyncError = sanitizeFeeError(venueErr)
	return merged
}

func applyMarketFee(prevStatus string, prevMaker, prevTaker *string, next portfolio.MarketFee) (MarketFeeView, bool) {
	switch next.Status {
	case portfolio.FeeStatusOK, portfolio.FeeStatusUnsupported:
		return MarketFeeView{Status: next.Status, Maker: next.Maker, Taker: next.Taker}, true
	default:
		return MarketFeeView{
			Status: firstNonEmpty(prevStatus, portfolio.FeeStatusUnknown),
			Maker:  derefFee(prevMaker),
			Taker:  derefFee(prevTaker),
		}, false
	}
}

func toPortfolioCredentials(item TradingCredentials) portfolio.Credentials {
	return portfolio.Credentials{
		APIKey: item.APIKey, APISecret: item.APISecret, Passphrase: item.Passphrase,
		CredentialKind: item.CredentialKind, SigningAddress: item.SigningAddress,
		AccountIndex: item.AccountIndex, APIKeyIndex: item.APIKeyIndex,
	}
}

func isRetryableFeeError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "timeout") ||
		strings.Contains(message, "status 429") ||
		strings.Contains(message, "too many requests") ||
		strings.Contains(message, "status 502") ||
		strings.Contains(message, "status 503") ||
		strings.Contains(message, "status 504") ||
		strings.Contains(message, "connection reset")
}

func isCredentialFeeError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "status 401") ||
		strings.Contains(message, "status 403") ||
		strings.Contains(message, "invalid api") ||
		(strings.Contains(message, "api-key") && strings.Contains(message, "invalid"))
}

func sanitizeFeeError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	message = strings.ReplaceAll(message, "\n", " ")
	lower := strings.ToLower(message)
	for _, secret := range []string{"apikey", "api_key", "api-secret", "apisecret", "signature", "passphrase", "private key"} {
		if strings.Contains(lower, secret) {
			return "venue rejected fee query"
		}
	}
	if len(message) > 180 {
		return message[:180]
	}
	return message
}

func sleepFeeBackoff(ctx context.Context, attempt int) error {
	delay := time.Duration(200*(1<<attempt)) * time.Millisecond
	if delay > 5*time.Second {
		delay = 5 * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func closeDone(done chan struct{}) {
	if done != nil {
		close(done)
	}
}
