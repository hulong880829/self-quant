package account

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

type EquitySnapshotRecord struct {
	TradingAccountID int64
	VenueIdentity    string
	EquityUSD        string
	Status           string
	ObservedAt       *time.Time
	LastSuccessAt    *time.Time
	ErrorCode        string
	ErrorMessage     string
}

func VenueIdentity(record TradingAccountRecord, decryptedAPIKey string) string {
	exchange := strings.ToLower(strings.TrimSpace(record.Exchange))
	switch exchange {
	case "hyperliquid", "aster":
		return exchange + ":" + strings.ToLower(firstNonEmpty(
			record.VaultAddress, walletFromDecrypted(decryptedAPIKey), record.SigningAddress,
		))
	case "lighter":
		index := "0"
		if record.AccountIndex != nil {
			index = strconv.FormatInt(*record.AccountIndex, 10)
		}
		return exchange + ":" + strings.ToLower(firstNonEmpty(
			record.VaultAddress, walletFromDecrypted(decryptedAPIKey), record.SigningAddress,
		)) + ":" + index
	default:
		return exchange + ":" + strings.ToLower(strings.TrimSpace(record.AccountName))
	}
}

func walletFromDecrypted(value string) string {
	value = strings.TrimSpace(value)
	if len(value) == 42 && strings.HasPrefix(strings.ToLower(value), "0x") {
		return value
	}
	return ""
}

type EquityRefresher struct {
	trading  *TradingRepository
	service  *Service
	interval time.Duration
	logger   *slog.Logger
}

func NewEquityRefresher(trading *TradingRepository, service *Service, interval time.Duration, logger *slog.Logger) *EquityRefresher {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &EquityRefresher{trading: trading, service: service, interval: interval, logger: logger}
}

func (r *EquityRefresher) Run(ctx context.Context) {
	r.Refresh(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Refresh(ctx)
		}
	}
}

func (r *EquityRefresher) Refresh(ctx context.Context) {
	if r.trading == nil || r.service == nil {
		return
	}
	records, err := r.trading.ListAll(ctx)
	if err != nil {
		r.logger.Error("list trading accounts for equity refresh failed", "error", err)
		return
	}
	semaphores := map[string]chan struct{}{}
	var mu sync.Mutex
	sem := func(exchange string) chan struct{} {
		mu.Lock()
		defer mu.Unlock()
		key := strings.ToLower(strings.TrimSpace(exchange))
		if slot, ok := semaphores[key]; ok {
			return slot
		}
		slot := make(chan struct{}, 2)
		semaphores[key] = slot
		return slot
	}
	var wg sync.WaitGroup
	for _, record := range records {
		record := record
		wg.Add(1)
		go func() {
			defer wg.Done()
			slot := sem(record.Exchange)
			select {
			case slot <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slot }()
			r.refreshOne(ctx, record)
		}()
	}
	wg.Wait()
}

func (r *EquityRefresher) refreshOne(ctx context.Context, record TradingAccountRecord) {
	now := time.Now().UTC()
	decrypted := ""
	if r.service.cipher != nil && len(record.APIKeyEnc) > 0 {
		if value, err := r.service.cipher.Decrypt(record.APIKeyEnc); err == nil {
			decrypted = value
		}
	}
	snapshot := EquitySnapshotRecord{
		TradingAccountID: record.ID,
		VenueIdentity:    VenueIdentity(record, decrypted),
		Status:           "failed",
		ObservedAt:       &now,
		ErrorCode:        "snapshot_unavailable",
	}
	loaded, err := r.service.snapshotForRecord(ctx, record.OwnerUsername, record)
	if err == nil && !loaded.Stale {
		snapshot.Status = "ok"
		snapshot.EquityUSD = strings.TrimSpace(loaded.AccountEquityUSD)
		snapshot.LastSuccessAt = &now
		snapshot.ErrorCode = ""
		snapshot.ErrorMessage = ""
	} else if err != nil {
		snapshot.ErrorMessage = err.Error()
	} else {
		snapshot.ErrorCode = "stale"
		snapshot.ErrorMessage = strings.TrimSpace(loaded.LastError)
		snapshot.EquityUSD = strings.TrimSpace(loaded.AccountEquityUSD)
	}
	if err := r.trading.UpsertEquitySnapshot(ctx, snapshot); err != nil {
		r.logger.Error("write equity snapshot failed", "account", record.ID, "error", err)
	}
}

type EquityAggregate struct {
	BrokerID        string
	EquityUSD       decimal.Decimal
	CoveredAccounts int64
	FailedSnapshots int64
	StaleSnapshots  int64
}

func AggregateEquitySnapshots(rows []EquitySnapshotView, staleAfter time.Duration, now time.Time) map[string]EquityAggregate {
	if staleAfter <= 0 {
		staleAfter = 15 * time.Minute
	}
	type key struct{ owner, venue, broker string }
	type pick struct {
		row   EquitySnapshotView
		fresh bool
	}
	latest := map[key]pick{}
	for _, row := range rows {
		item := key{owner: row.OwnerUsername, venue: row.VenueIdentity, broker: row.BrokerID}
		current, ok := latest[item]
		if !ok || row.ObservedAt.After(current.row.ObservedAt) {
			fresh := row.Status == "ok" && !row.ObservedAt.IsZero() && now.Sub(row.ObservedAt) <= staleAfter
			latest[item] = pick{row: row, fresh: fresh}
		}
	}
	out := map[string]EquityAggregate{}
	for _, item := range latest {
		agg := out[item.row.BrokerID]
		agg.BrokerID = item.row.BrokerID
		switch {
		case item.fresh:
			agg.CoveredAccounts++
			if value, err := decimal.NewFromString(strings.TrimSpace(item.row.EquityUSD)); err == nil {
				agg.EquityUSD = agg.EquityUSD.Add(value)
			}
		case item.row.Status == "ok":
			agg.StaleSnapshots++
		default:
			agg.FailedSnapshots++
		}
		out[item.row.BrokerID] = agg
	}
	return out
}
