package account

import (
	"strings"
	"sync"
	"time"

	"selfquant/backend/internal/account/portfolio"
)

const (
	FeeSyncPending           = "pending"
	FeeSyncOK                = "ok"
	FeeSyncRetrying          = "retrying"
	FeeSyncFailed            = "failed"
	FeeSyncCredentialInvalid = "credential_invalid"
	feeStaleAfter            = 30 * time.Hour
	feeBindWaitTimeout       = 4 * time.Second
	shanghaiLocationName     = "Asia/Shanghai"
)

type FeeRatesRecord struct {
	ID             int64
	Exchange       string
	SpotMaker      *string
	SpotTaker      *string
	ContractMaker  *string
	ContractTaker  *string
	SpotStatus     string
	ContractStatus string
	Source         string
	UpdatedAt      *time.Time
	SyncStatus     string
	SyncError      string
	Markets        []byte
}

type MarketFeeView struct {
	Status string
	Maker  string
	Taker  string
}

type TradingAccountFees struct {
	Spot               MarketFeeView
	Contract           MarketFeeView
	Source             string
	UpdatedAt          time.Time
	SyncStatus         string
	SyncError          string
	Stale              bool
	UnsupportedMarkets []string
}

type feeCache struct {
	mu      sync.RWMutex
	entries map[int64]TradingAccountFees
	now     func() time.Time
	loc     *time.Location
}

func newFeeCache(now func() time.Time, loc *time.Location) *feeCache {
	if now == nil {
		now = time.Now
	}
	if loc == nil {
		loc, _ = time.LoadLocation(shanghaiLocationName)
		if loc == nil {
			loc = time.UTC
		}
	}
	return &feeCache{entries: map[int64]TradingAccountFees{}, now: now, loc: loc}
}

func (c *feeCache) get(id int64) (TradingAccountFees, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[id]
	if !ok {
		return TradingAccountFees{}, false
	}
	entry.Stale = feeIsStale(entry.UpdatedAt, c.now(), c.loc)
	return entry, true
}

func (c *feeCache) put(id int64, fees TradingAccountFees) {
	fees.Stale = feeIsStale(fees.UpdatedAt, c.now(), c.loc)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[id] = fees
}

func (c *feeCache) delete(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, id)
}

func (c *feeCache) loadAll(rows []FeeRatesRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[int64]TradingAccountFees, len(rows))
	now := c.now()
	for _, row := range rows {
		fees := feeViewFromRecord(row)
		fees.Stale = feeIsStale(fees.UpdatedAt, now, c.loc)
		c.entries[row.ID] = fees
	}
}

func feeIsStale(updatedAt, now time.Time, loc *time.Location) bool {
	if updatedAt.IsZero() {
		return false
	}
	local := now.In(loc)
	cutoff := time.Date(local.Year(), local.Month(), local.Day(), 8, 0, 0, 0, loc).Add(-feeStaleAfter)
	return updatedAt.Before(cutoff)
}

func feeViewFromRecord(record FeeRatesRecord) TradingAccountFees {
	spot := MarketFeeView{
		Status: firstNonEmpty(record.SpotStatus, portfolio.FeeStatusUnknown),
		Maker:  derefFee(record.SpotMaker),
		Taker:  derefFee(record.SpotTaker),
	}
	contract := MarketFeeView{
		Status: firstNonEmpty(record.ContractStatus, portfolio.FeeStatusUnknown),
		Maker:  derefFee(record.ContractMaker),
		Taker:  derefFee(record.ContractTaker),
	}
	unsupported := make([]string, 0, 2)
	if spot.Status == portfolio.FeeStatusUnsupported {
		unsupported = append(unsupported, portfolio.FeeMarketSpot)
	}
	if contract.Status == portfolio.FeeStatusUnsupported {
		unsupported = append(unsupported, portfolio.FeeMarketContract)
	}
	updatedAt := time.Time{}
	if record.UpdatedAt != nil {
		updatedAt = record.UpdatedAt.UTC()
	}
	return TradingAccountFees{
		Spot:               spot,
		Contract:           contract,
		Source:             record.Source,
		UpdatedAt:          updatedAt,
		SyncStatus:         record.SyncStatus,
		SyncError:          record.SyncError,
		UnsupportedMarkets: unsupported,
	}
}

func derefFee(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func stringPtr(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	copied := value
	return &copied
}
