package trader

import (
	"context"
	"time"

	"github.com/shopspring/decimal"
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
	ID                                string
	IdempotencyKey                    string
	RequestFingerprint                string
	OwnerUsername                     string
	LegA                              ArbitrageLeg
	LegB                              ArbitrageLeg
	AskThresholdBps                   string
	BidThresholdBps                   string
	TargetNotional                    string
	OrderNotional                     string
	RunMode                           string
	EntryDirection                    string
	LegALeverage                      string
	LegBLeverage                      string
	ExitPolicy                        string
	ExitAnnualizedRate                string
	ExitAfterSeconds                  int
	EarlyExitFunding8hAnnualizedFloor string
	EarlyExitLiquidationDistancePct   string
	TargetReachedAt                   time.Time
	ScheduledExitAt                   time.Time
	OneShotPhase                      string
	ExecutionMode                     string
	MakerLeg                          string
	Status                            string
	RuntimeState                      string
	PositionNotional                  string
	CumulativeTurnoverNotional        string
	GrossTurnoverNotional             string
	LegABasePosition                  string
	LegBBasePosition                  string
	CarryBaseQuantity                 string
	LegAAverageEntryPrice             string
	LegBAverageEntryPrice             string
	AverageEntrySpreadBps             string
	LegAUnrealizedPnl                 string
	LegBUnrealizedPnl                 string
	RealizedSpreadPnl                 string
	EstimatedFundingPnl               string
	EstimatedTradingFee               string
	MetricsQuality                    string
	NotionalExposureSeconds           string
	HoldingSeconds                    string
	ExposureUpdatedAt                 time.Time
	CombinedPositionAnnualized        string
	FundingHistoryComplete            bool
	LegAVenueBaselineBasePosition     string
	LegBVenueBaselineBasePosition     string
	VenueBaselineCapturedAt           time.Time
	LegAVenueBasePosition             string
	LegBVenueBasePosition             string
	LegAVenueNotional                 string
	LegBVenueNotional                 string
	LegAVenueValuationPrice           string
	LegBVenueValuationPrice           string
	LegAVenueValuationAt              time.Time
	LegBVenueValuationAt              time.Time
	LegAPositionDifference            string
	LegBPositionDifference            string
	LegAReconciliationAdjustment      string
	LegBReconciliationAdjustment      string
	LastPositionReconciledAt          time.Time
	CurrentAskSpreadBps               string
	CurrentBidSpreadBps               string
	MarketDataStale                   bool
	ErrorMessage                      string
	ConsecutiveFailures               int
	NextRetryAt                       time.Time
	PositionUncertain                 bool
	LastFailureKey                    string
	RepeatedFailureCount              int
	CircuitOpen                       bool
	Version                           int64
	SchedulerLeaseUntil               time.Time
	CreatedAt                         time.Time
	UpdatedAt                         time.Time
	ClosedAt                          time.Time
	MetricsCalculatedAt               time.Time
	FinalMetricsStatus                string
	FinalMetricsRetryCount            int
	FinalMetricsLastError             string
	FinalMetricsNextRetryAt           time.Time
	FinalMetricsRetryFinite           bool
	FinalMetricsCompletedAt           time.Time
	LegALiquidationDistancePct        string
	LegBLiquidationDistancePct        string
}

type ArbitrageValuation struct {
	LegAMid       string
	LegBMid       string
	LegAUpdatedAt time.Time
	LegBUpdatedAt time.Time
}

type arbitrageValuationProvider interface {
	ArbitrageValuation(string) (ArbitrageValuation, bool)
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
	RequestedNotional  string
	PositionEffect     string
	ReduceOnly         bool
	LastCloseClip      bool
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

type PrepareArbitrageHedgeIntentInput struct {
	Combination           ArbitrageCombination
	Execution             ArbitrageExecution
	Order                 Order
	ExpectedSequence      int64
	HedgeLeg              ArbitrageLeg
	HedgeSide             string
	TargetQuantity        string
	CarryQuantity         string
	FastPathAdmission     bool
	ConfirmedMakerFilled  string
	ConfirmedHedgeFilled  string
	AggregateCarry        bool
	ExpectedCarryQuantity string
}

type PrepareArbitrageHedgeIntentResult struct {
	Order       Order
	Created     bool
	Execution   ArbitrageExecution
	Combination ArbitrageCombination
}

type FailLastCloseClipUnbalancedResult struct {
	Combination ArbitrageCombination
	Execution   ArbitrageExecution
}

type ArbitrageEvent struct {
	ID            string
	CombinationID string
	ExecutionID   string
	Type          string
	Message       string
	Payload       map[string]any
	PayloadObject bool
	CreatedAt     time.Time
}

type ArbitrageHistoryQuery struct {
	Limit       int
	Cursor      string
	From        time.Time
	To          time.Time
	ExecutionID string
}

type PreflightDecimal struct {
	Value  string
	Status string
	Unit   string
}

type PreflightLeg struct {
	AvailableFunds      PreflightDecimal
	RequiredMargin      PreflightDecimal
	EstimatedFee        PreflightDecimal
	MinQuantity         PreflightDecimal
	MinNotional         PreflightDecimal
	QuantityStep        PreflightDecimal
	RoundedBaseQuantity PreflightDecimal
}

type PreflightWarning struct {
	Code     string
	Message  string
	Leg      string
	Exchange string
	Details  map[string]string
}

type IncreaseTargetPreflight struct {
	Executable            bool
	CurrentTargetNotional string
	NewTargetNotional     string
	RequestedTargetDelta  string
	TargetNotionalUnit    string
	LegA                  IncreaseTargetLeg
	LegB                  IncreaseTargetLeg
	Warnings              []PreflightWarning
	CheckedAt             time.Time
	ExpiresAt             time.Time
	Code                  string
	Message               string
	Leg                   string
	Exchange              string
}

type IncreaseTargetLeg struct {
	AdditionalNotional      string
	IncrementalQuantity     string
	IncrementalQuantityUnit string
	IncrementalMargin       PreflightDecimal
	EstimatedFee            PreflightDecimal
	AvailableFunds          PreflightDecimal
	MinQuantity             PreflightDecimal
	MinNotional             PreflightDecimal
	QuantityStep            PreflightDecimal
}

type ArbitragePreflight struct {
	Executable bool
	CheckedAt  time.Time
	ExpiresAt  time.Time
	LegA       PreflightLeg
	LegB       PreflightLeg
	Warnings   []PreflightWarning
	Code       string
	Message    string
	Leg        string
	Exchange   string
}

type CreateArbitrageInput struct {
	Token                             string
	LegATradingAccountID              int64
	LegAInstrumentID                  int64
	LegBTradingAccountID              int64
	LegBInstrumentID                  int64
	AskThresholdBps                   string
	BidThresholdBps                   string
	TargetNotional                    string
	ExecutionMode                     string
	MakerLeg                          string
	IdempotencyKey                    string
	RunMode                           string
	EntryDirection                    string
	LegALeverage                      string
	LegBLeverage                      string
	ExitPolicy                        string
	ExitAnnualizedRate                string
	ExitAfterSeconds                  int
	EarlyExitFunding8hAnnualizedFloor string
	EarlyExitLiquidationDistancePct   string
}

type UpdateArbitrageInput struct {
	Token                             string
	CombinationID                     string
	AskThresholdBps                   *string
	BidThresholdBps                   *string
	TargetNotional                    *string
	ExitAfterSeconds                  *int
	ExitAnnualizedRate                *string
	EarlyExitFunding8hAnnualizedFloor *string
	EarlyExitLiquidationDistancePct   *string
}

type arbitrageMetricsQuotes struct {
	MidA      string
	MidB      string
	BidA      string
	AskA      string
	BidB      string
	AskB      string
	ReceivedA time.Time
	ReceivedB time.Time
}

type exitRequest struct {
	Requested bool
	Version   int64
	Reason    string
}

type ArbitrageBBO struct {
	BidPrice    string
	AskPrice    string
	BidQuantity string
	AskQuantity string
	Timestamp   time.Time
	Stale       bool
}

type arbitrageControlSummary struct {
	Version      int64
	Status       string
	RuntimeState string
}

type arbitrageControlSummaryBatch struct {
	Found   map[string]arbitrageControlSummary
	Missing []string
}

type arbitrageMarketSnapshot struct {
	CurrentAskSpreadBps string
	CurrentBidSpreadBps string
	MarketDataStale     bool
	RuntimeState        string
	Status              string
	Version             int64
	UpdatedAt           time.Time
}

type arbitrageMarketSnapshotWrite struct {
	CombinationID     string
	AskSpread         string
	BidSpread         string
	ExpectedVersion   int64
	ExpectedUpdatedAt time.Time
	Sequence          uint64
}

type arbitrageMarketSnapshotBatchItem struct {
	CombinationID   string
	Applied         bool
	Version         int64
	Status          string
	RuntimeState    string
	MarketDataStale bool
	UpdatedAt       time.Time
	AskSpread       string
	BidSpread       string
	Sequence        uint64
}

type arbitrageControlStore interface {
	GetArbitrageControlSummary(context.Context, string) (arbitrageControlSummary, error)
}

type arbitrageControlSummaryBatchStore interface {
	ListArbitrageControlSummaries(context.Context, []string) (arbitrageControlSummaryBatch, error)
}

type arbitrageLeaseBatchStore interface {
	RenewArbitrageLeases(context.Context, []string, time.Duration) (map[string]bool, error)
}

type arbitrageSnapshotBatchStore interface {
	BatchUpdateArbitrageMarketSnapshots(
		context.Context, []arbitrageMarketSnapshotWrite,
	) ([]arbitrageMarketSnapshotBatchItem, error)
}

type arbitrageStore interface {
	CreateArbitrageCombination(context.Context, ArbitrageCombination) (ArbitrageCombination, bool, error)
	GetArbitrageCombinationByOwner(context.Context, string, string) (ArbitrageCombination, error)
	ListArbitrageCombinations(context.Context, string, string, int, string) ([]ArbitrageCombination, string, error)
	CountArbitrageCombinations(context.Context, string, string) (int64, error)
	MarkArbitrageClosing(context.Context, string, string) (ArbitrageCombination, error)
	ListArbitrageOrders(context.Context, string, string) ([]Order, error)
	ListRecentSubmittedArbitrageOrders(context.Context, string, string, int) ([]Order, error)
	ListArbitrageExecutions(context.Context, string, int) ([]ArbitrageExecution, error)
	ListArbitrageEvents(context.Context, string, int) ([]ArbitrageEvent, error)
	LeaseArbitrageCombinations(context.Context, int, time.Duration) ([]ArbitrageCombination, error)
	RenewArbitrageLease(context.Context, string, time.Duration) (bool, error)
	ClaimArbitrageExecution(context.Context, ArbitrageExecution) (ArbitrageExecution, bool, error)
	GetActiveArbitrageExecution(context.Context, string) (ArbitrageExecution, error)
	FinalizeConfirmedAbsentZeroFillExecution(context.Context, string) (confirmedAbsentFinalizeResult, error)
	PrepareArbitrageHedgeIntent(
		context.Context, PrepareArbitrageHedgeIntentInput,
	) (PrepareArbitrageHedgeIntentResult, error)
	UpdateArbitrageExecution(context.Context, ArbitrageExecution) (ArbitrageExecution, error)
	FailLastCloseClipUnbalanced(
		context.Context, ArbitrageCombination, ArbitrageExecution, string, string, string,
	) (FailLastCloseClipUnbalancedResult, error)
	UpdateArbitrageCombinationRuntime(context.Context, ArbitrageCombination) (ArbitrageCombination, error)
	UpdateArbitrageMarketSnapshot(context.Context, string, string, string, bool) (arbitrageMarketSnapshot, error)
	AddArbitragePositionDelta(context.Context, string, string) (ArbitrageCombination, error)
	UpdateArbitragePositionFromBase(context.Context, string, string, string) (ArbitrageCombination, error)
	RecomputeArbitrageBasePositions(context.Context, string) (ArbitrageCombination, error)
	RecomputeArbitrageBasePositionsForExecution(context.Context, string) (ArbitrageCombination, error)
	ApplyCircuitOpenExternalReconcile(
		context.Context, circuitOpenExternalReconcileRequest,
	) (ArbitrageCombination, bool, error)
	ClaimCircuitRecoveryExecution(
		context.Context, ClaimCircuitRecoveryExecutionInput,
	) (ArbitrageExecution, bool, error)
	CompleteCircuitOpenRecovery(
		context.Context, circuitOpenRecoveryCloseRequest,
	) (ArbitrageCombination, bool, error)
	RecordArbitrageFailure(context.Context, string, string) (ArbitrageCombination, error)
	AppendArbitrageEvent(context.Context, string, string, string, map[string]any) error
	DeleteExpiredArbitrageCombinations(context.Context, time.Time, int) (int64, error)
}

type arbitrageOneShotStore interface {
	MarkOneShotWaitingExit(context.Context, string, int64, time.Time) (ArbitrageCombination, bool, error)
	MarkOneShotExiting(context.Context, string, int64, string, map[string]any) (ArbitrageCombination, bool, error)
	MarkOneShotExited(context.Context, string, int64) (ArbitrageCombination, bool, error)
	MarkOneShotExitedWithDust(
		context.Context, string, int64, string, string, string,
	) (ArbitrageCombination, bool, error)
	ArbitrageDustOrderGate(
		context.Context, string, time.Time,
	) (liveOrder, unreconciled, liveExecution bool, err error)
	ClaimOneShotFinalFlattenExecution(
		context.Context, OneShotFinalFlattenClaim,
	) (ArbitrageExecution, bool, error)
	FinalizeOneShotFinalFlattenExecution(
		context.Context, string,
	) (oneShotFinalFlattenFinalizeResult, error)
}

type arbitrageFinalMetricsStore interface {
	PersistFinalArbitragePositionMetrics(
		context.Context, string, int64, bool, time.Time,
	) (ArbitrageCombination, error)
	MarkArbitrageClosedAndScheduleFinalMetrics(
		context.Context, string, int64,
	) (ArbitrageCombination, bool, error)
}

type arbitrageIdempotencyStore interface {
	GetArbitrageCombinationByIdempotencyKey(context.Context, string, string) (ArbitrageCombination, error)
}

type arbitrageConflictStore interface {
	FindActiveArbitrageInstrumentConflict(
		context.Context, string, string, int64, int64, int64, int64,
	) error
}

type arbitrageConfigStore interface {
	UpdateArbitrageCombinationConfig(context.Context, string, string, UpdateArbitrageInput) (ArbitrageCombination, error)
}

type arbitrageOrderStore interface {
	orderStore
	CreateArbitrageIntents(context.Context, []Order) ([]Order, []bool, error)
	ApplyStreamUpdate(context.Context, string, StreamUpdate) (Order, error)
}

type arbitrageFailureStore interface {
	ClearArbitrageFailureIfUnchanged(context.Context, string, string) (ArbitrageCombination, error)
}

type arbitrageCloseFailureStore interface {
	RecordArbitrageCloseFailure(
		context.Context, string, string, string,
	) (ArbitrageCombination, error)
}

type ClaimCircuitRecoveryExecutionInput struct {
	ExpectedVersion   int64
	FailedExecutionID string
	Execution         ArbitrageExecution
}

type circuitOpenRecoveryCloseRequest struct {
	CombinationID     string
	ExecutionID       string
	ExpectedVersion   int64
	Kind              string
	EventType         string
	VenueA            decimal.Decimal
	VenueB            decimal.Decimal
	MarkA             decimal.Decimal
	MarkB             decimal.Decimal
	InstrumentA       Instrument
	InstrumentB       Instrument
	ObservedAt        time.Time
	RecoveryStartedAt time.Time
	OrderSumA         decimal.Decimal
	OrderSumB         decimal.Decimal
	LegAFilled        string
	LegBFilled        string
	DeltaNotional     string
	TurnoverDelta     string
	Payload           map[string]any
}

type circuitOpenExternalReconcileRequest struct {
	CombinationID   string
	ExecutionID     string
	ExpectedVersion int64
	VenueA          decimal.Decimal
	VenueB          decimal.Decimal
	MarkA           decimal.Decimal
	MarkB           decimal.Decimal
	InstrumentA     Instrument
	InstrumentB     Instrument
}

type closingExternalFlatReconcileRequest struct {
	CombinationID   string
	ExpectedVersion int64
	VenueA          decimal.Decimal
	VenueB          decimal.Decimal
}
