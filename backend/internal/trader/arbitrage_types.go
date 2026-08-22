package trader

import (
	"context"
	"time"
)

type ArbitrageLeg struct {
	TradingAccountID int64
	InstrumentID     int64
	ProductName      string
	AccountName      string
	Exchange         string
	ContractType     string
	ExchangeSymbol   string
	BaseAsset        string
	QuoteAsset       string
}

type ArbitrageCombination struct {
	ID                  string
	IdempotencyKey      string
	RequestFingerprint  string
	OwnerUsername       string
	LegA                ArbitrageLeg
	LegB                ArbitrageLeg
	AskThresholdBps     string
	BidThresholdBps     string
	TargetNotional      string
	OrderNotional       string
	MaxDeltaNotional    string
	ExecutionMode       string
	MakerLeg            string
	Status              string
	CompletedNotional   string
	CurrentAskSpreadBps string
	CurrentBidSpreadBps string
	MarketDataStale     bool
	ErrorMessage        string
	SchedulerLeaseUntil time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
	ClosedAt            time.Time
}

type ArbitrageExecution struct {
	ID                 string
	CombinationID      string
	Direction          string
	Sequence           int64
	Status             string
	TriggerAskSpread   string
	TriggerBidSpread   string
	TriggerLegABid     string
	TriggerLegAAsk     string
	TriggerLegBBid     string
	TriggerLegBAsk     string
	TargetBaseQuantity string
	LegAFilledQuantity string
	LegBFilledQuantity string
	DeltaNotional      string
	MakerOrderID       string
	HedgeOrderID       string
	HedgeSequence      int64
	Attempt            int
	ErrorMessage       string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	ClosedAt           time.Time
}

type ArbitrageEvent struct {
	ID            string
	CombinationID string
	ExecutionID   string
	Type          string
	Message       string
	CreatedAt     time.Time
}

type CreateArbitrageInput struct {
	Token                string
	LegATradingAccountID int64
	LegAInstrumentID     int64
	LegBTradingAccountID int64
	LegBInstrumentID     int64
	AskThresholdBps      string
	BidThresholdBps      string
	TargetNotional       string
	OrderNotional        string
	MaxDeltaNotional     string
	ExecutionMode        string
	MakerLeg             string
	IdempotencyKey       string
}

type ArbitrageBBO struct {
	BidPrice  string
	AskPrice  string
	Timestamp time.Time
	Stale     bool
}

type arbitrageStore interface {
	CreateArbitrageCombination(context.Context, ArbitrageCombination) (ArbitrageCombination, bool, error)
	GetArbitrageCombinationByOwner(context.Context, string, string) (ArbitrageCombination, error)
	ListArbitrageCombinations(context.Context, string, string, int, string) ([]ArbitrageCombination, string, error)
	CountArbitrageCombinations(context.Context, string, string) (int64, error)
	MarkArbitrageClosing(context.Context, string, string) (ArbitrageCombination, error)
	ListArbitrageOrders(context.Context, string, string) ([]Order, error)
	ListArbitrageExecutions(context.Context, string, int) ([]ArbitrageExecution, error)
	ListArbitrageEvents(context.Context, string, int) ([]ArbitrageEvent, error)
	LeaseArbitrageCombinations(context.Context, int, time.Duration) ([]ArbitrageCombination, error)
	RenewArbitrageLease(context.Context, string, time.Duration) (bool, error)
	ClaimArbitrageExecution(context.Context, ArbitrageExecution) (ArbitrageExecution, bool, error)
	GetActiveArbitrageExecution(context.Context, string, string) (ArbitrageExecution, error)
	UpdateArbitrageExecution(context.Context, ArbitrageExecution) (ArbitrageExecution, error)
	UpdateArbitrageCombinationRuntime(context.Context, ArbitrageCombination) (ArbitrageCombination, error)
	UpdateArbitrageMarketSnapshot(context.Context, string, string, string, bool) (ArbitrageCombination, error)
	AddArbitrageCompletedNotional(context.Context, string, string) (ArbitrageCombination, error)
	AppendArbitrageEvent(context.Context, string, string, string, map[string]any) error
	DeleteExpiredArbitrageCombinations(context.Context, time.Time, int) (int64, error)
}

type arbitrageOrderStore interface {
	orderStore
	CreateArbitrageIntents(context.Context, []Order) ([]Order, []bool, error)
	ApplyStreamUpdate(context.Context, string, StreamUpdate) (Order, error)
}
