package jev

import (
	"context"
	"time"

	"github.com/shopspring/decimal"
)

type Store interface {
	AcquireCycleLock(context.Context) (CycleLease, bool, error)
	EnsureActiveRun(ctx context.Context, now time.Time, initialEquity decimal.Decimal, strategyVersion string) (Run, error)
	SaveRun(context.Context, Run) error
	ListRuns(context.Context) ([]Run, error)
	ListAllPositions(ctx context.Context, runID int64) ([]Position, error)
	ListOpenPositions(ctx context.Context, runID int64) ([]Position, error)
	ListOpenPositionsExceptStrategy(ctx context.Context, strategyVersion string) ([]Position, error)
	ListPositionsPage(ctx context.Context, runID int64, limit int, cursor int64) ([]Position, int64, error)
	InsertPosition(context.Context, Position) (Position, error)
	SavePosition(context.Context, Position) error
	InsertDecision(context.Context, Decision) (Decision, error)
	SaveDecision(context.Context, Decision) error
	ListPendingDecisions(ctx context.Context, runID int64) ([]Decision, error)
	ListDecisionsPage(ctx context.Context, runID int64, limit int, cursor int64) ([]Decision, int64, error)
	InsertTrade(context.Context, Trade) (Trade, error)
	ListTradesPage(ctx context.Context, runID int64, limit int, cursor int64) ([]Trade, int64, error)
	InsertEquityPoint(context.Context, int64, EquityPoint) error
	ListEquityPoints(ctx context.Context, runID int64, from *time.Time) ([]EquityPoint, error)
	PublishSnapshot(ctx context.Context, runID int64, snapshot Snapshot) (Snapshot, error)
	LatestSnapshot(ctx context.Context, runID int64) (*Snapshot, error)
	ActiveRun(ctx context.Context, strategyVersion string) (*Run, error)
}

type MarketSource interface {
	ListRates(context.Context) ([]Rate, error)
	ListHistory(ctx context.Context, exchange, symbol string, limit int) ([]HistoryPoint, error)
}

type QuoteSource interface {
	Latest(context.Context, []MarketKey) (map[string]Quote, error)
	MidBefore(ctx context.Context, key MarketKey, settledAt time.Time, maxAge time.Duration) (decimal.Decimal, time.Time, bool, error)
	TenMinuteBBOs(ctx context.Context, keys []MarketKey, from, untilExclusive time.Time) ([]TenMinuteBBO, error)
}

type SpotCatalog interface {
	ListSpots(context.Context) (map[string]SpotInstrument, error)
}
