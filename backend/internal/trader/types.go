package trader

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"selfquant/backend/internal/trader/exchange"
)

var (
	ErrInvalidArgument                   = errors.New("invalid argument")
	ErrOrderBelowMinimum                 = errors.New("order below minimum")
	ErrNotFound                          = errors.New("not found")
	ErrIdempotencyConflict               = errors.New("idempotency key conflict")
	ErrActiveTwapLimit                   = errors.New("active twap limit reached")
	ErrUnsupportedExchange               = errors.New("unsupported exchange")
	ErrInstrumentUnavailable             = errors.New("instrument unavailable")
	ErrOrderNotCancelable                = errors.New("order is not cancelable")
	ErrTwapNotCancelable                 = errors.New("twap is not cancelable")
	ErrArbitrageConflict                 = errors.New("arbitrage combination conflict")
	ErrActiveArbitrageInstrumentConflict = errors.New(
		"active arbitrage instrument conflict",
	)
	ErrArbitrageConfigInvalid                   = errors.New("invalid arbitrage configuration")
	ErrArbitrageCreateFailed                    = errors.New("arbitrage create failed")
	ErrArbitrageNotClosable                     = errors.New("arbitrage combination is not closable")
	ErrArbitrageCloseWaitingSnapshot            = errors.New("arbitrage close waiting position snapshot")
	ErrMarketDataStale                          = errors.New("market data is stale")
	ErrRiskLimit                                = errors.New("arbitrage risk limit exceeded")
	ErrPositionMode                             = errors.New("one-way position mode required")
	ErrPersistence                              = errors.New("order persistence failed")
	ErrVenueRejected                            = errors.New("venue rejected order")
	ErrVenueRateLimited                         = errors.New("venue rate limited")
	ErrVenueUnavailable                         = errors.New("venue unavailable")
	ErrVenueUncertain                           = errors.New("venue result uncertain")
	ErrArbitrageReconciling                     = errors.New("arbitrage execution reconciling")
	ErrArbitrageExecutionTerminal               = errors.New("arbitrage execution is already terminal")
	errHyperliquidFillUnknown                   = errors.New("hyperliquid terminal snapshot missing cumulative filled")
	ErrArbitrageUnsafeHedge                     = errors.New("arbitrage hedge would reverse position")
	ErrArbitrageHedgeSequence                   = errors.New("arbitrage hedge sequence mismatch")
	ErrArbitrageHedgeAdmissionConflict          = errors.New("arbitrage hedge admission conflict")
	ErrArbitrageLastCloseClipUnbalancedConflict = errors.New(
		"last close clip unbalanced conflict",
	)
	ErrCloseOrderOpen      = errors.New("close order remains open")
	ErrCloseOrderUncertain = errors.New("close order state uncertain")
)

type ArbitrageCreateError struct {
	Code    string
	Message string
	Leg     string
	Details map[string]string
}

func (e *ArbitrageCreateError) Error() string {
	if e == nil {
		return ErrArbitrageCreateFailed.Error()
	}
	if strings.TrimSpace(e.Message) == "" {
		return e.Code
	}
	return e.Message
}

func (e *ArbitrageCreateError) Unwrap() error {
	return ErrArbitrageCreateFailed
}

func newArbitrageCreateError(code, message, leg string, details map[string]string) *ArbitrageCreateError {
	if details == nil {
		details = map[string]string{}
	}
	return &ArbitrageCreateError{Code: code, Message: message, Leg: leg, Details: details}
}

type ArbitragePositionAuditDeferredReason string

const (
	ArbitragePositionAuditDeferredVersionConflict  ArbitragePositionAuditDeferredReason = "version_conflict"
	ArbitragePositionAuditDeferredActiveExecution  ArbitragePositionAuditDeferredReason = "active_execution"
	ArbitragePositionAuditDeferredActiveOrder      ArbitragePositionAuditDeferredReason = "active_order"
	ArbitragePositionAuditDeferredReconcileFailure ArbitragePositionAuditDeferredReason = "reconcile_failure"
)

type ArbitragePositionAuditDeferredError struct {
	Reason      ArbitragePositionAuditDeferredReason
	ExecutionID string
}

func (e *ArbitragePositionAuditDeferredError) Error() string {
	if e.ExecutionID != "" {
		return fmt.Sprintf("arbitrage position audit deferred: %s (%s)", e.Reason, e.ExecutionID)
	}
	return fmt.Sprintf("arbitrage position audit deferred: %s", e.Reason)
}

type Instrument struct {
	ID                       int64
	Exchange                 string
	ContractType             string
	ExchangeSymbol           string
	BaseAsset                string
	QuoteAsset               string
	SettleAsset              string
	ContractSize             string
	PriceTick                string
	QuantityStep             string
	MinQuantity              string
	MinNotional              string
	MinQuantityStatus        string
	MinNotionalStatus        string
	MaxQuantity              string
	MarketQuantityStep       string
	MarketMinQuantity        string
	MarketMaxQuantity        string
	MarketMinNotional        string
	MaxQuantityStatus        string
	MarketQuantityStepStatus string
	MarketMinQuantityStatus  string
	MarketMaxQuantityStatus  string
	MarketMinNotionalStatus  string
	Metadata                 map[string]any
}

type VenueCapabilities struct {
	Products           []string
	QuoteAssets        []string
	TimeInForce        []string
	PostOnly           bool
	ReduceOnly         bool
	MakerTwap          bool
	PrivateOrderStream bool
	OneWayOnly         bool
}

type Order struct {
	ID                   string
	IdempotencyKey       string
	OwnerUsername        string
	TradingAccountID     int64
	ProductName          string
	Exchange             string
	InstrumentID         int64
	ContractType         string
	ExchangeSymbol       string
	BaseAsset            string
	QuoteAsset           string
	ClientOrderID        string
	VenueOrderID         string
	Side                 string
	OrderType            string
	Quantity             string
	Price                string
	FilledQuantity       string
	AveragePrice         string
	Status               string
	ErrorCode            string
	ErrorMessage         string
	RequestFingerprint   string
	CreatedAt            time.Time
	UpdatedAt            time.Time
	LastReconciledAt     time.Time
	LastStreamEventAt    time.Time
	LastVenueEventAt     time.Time
	SyncState            string
	ReconcileFailures    int
	TwapJobID            string
	TwapSliceIndex       int
	TwapAttemptIndex     int
	ArbitrageExecutionID string
	ArbitrageLeg         string
	ArbitrageRole        string
	ReduceOnly           bool
	AbsenceConfirmations int
}

type PlaceOrderInput struct {
	Token            string
	TradingAccountID int64
	InstrumentID     int64
	Side             string
	OrderType        string
	Quantity         string
	Price            string
	IdempotencyKey   string
}

type Credentials struct {
	TradingAccountID int64
	ProductName      string
	Exchange         string
	AccountName      string
	APIKey           string
	APISecret        string
	Passphrase       string
	CredentialKind   string
	SigningAddress   string
	VaultAddress     string
	AccountIndex     *int64
	APIKeyIndex      *int32
}

type AccountMeta struct {
	TradingAccountID int64
	ProductName      string
	Exchange         string
	AccountName      string
	CredentialKind   string
	AccountIndex     *int64
	APIKeyIndex      *int32
}

func derefInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func derefInt32(value *int32) int32 {
	if value == nil {
		return 0
	}
	return *value
}

type VenueResult struct {
	VenueOrderID    string
	Status          string
	FilledQuantity  string
	AveragePrice    string
	ErrorCode       string
	ErrorMessage    string
	Raw             map[string]any
	Reference       exchange.VenueReference
	LocalCommandAck bool
}

// OrderFill identifies one venue execution. TradeID is scoped by account,
// exchange, and venue order ID.
type OrderFill struct {
	TradeID    string
	Quantity   string
	Price      string
	ExecutedAt time.Time
}

// StreamUpdate is an order snapshot plus any executions carried by the same
// stream event. FilledQuantity is cumulative when present.
type StreamUpdate struct {
	Result             VenueResult
	EventAt            time.Time
	ReceivedAt         time.Time
	Fills              []OrderFill
	SkipVenueWatermark bool // Hyperliquid userFills and Bitget fill/fast-fill without cumulative qty: persist trades without changing cumulative qty, status, or last_venue_event_at
}

type TwapJob struct {
	ID                  string
	IdempotencyKey      string
	RequestFingerprint  string
	OwnerUsername       string
	TradingAccountID    int64
	ProductName         string
	Exchange            string
	InstrumentID        int64
	ContractType        string
	ExchangeSymbol      string
	BaseAsset           string
	QuoteAsset          string
	Side                string
	TotalQuantity       string
	FilledQuantity      string
	AveragePrice        string
	StartAt             time.Time
	EndAt               time.Time
	IntervalSeconds     int
	LimitPrice          string
	MaxQuantity         string
	ExecutionType       string
	OrderTimeoutSeconds int
	Status              string
	CurrentSlice        int
	CurrentAttempt      int
	NextActionAt        time.Time
	ActiveOrderID       string
	SchedulerLeaseUntil time.Time
	SchedulerFailures   int
	ErrorMessage        string
	CreatedAt           time.Time
	UpdatedAt           time.Time
	StartedAt           time.Time
	ClosedAt            time.Time
}

type CreateTwapInput struct {
	Token               string
	TradingAccountID    int64
	InstrumentID        int64
	Side                string
	TotalQuantity       string
	StartAt             time.Time
	EndAt               time.Time
	IntervalSeconds     int
	LimitPrice          string
	MaxQuantity         string
	ExecutionType       string
	OrderTimeoutSeconds int
	IdempotencyKey      string
}

func terminalTwapStatus(status string) bool {
	switch status {
	case "completed", "partially_completed", "canceled", "failed":
		return true
	default:
		return false
	}
}

func terminalStatus(status string) bool {
	switch status {
	case "filled", "canceled", "rejected", "expired":
		return true
	default:
		return false
	}
}

func cancelableStatus(status string) bool {
	switch status {
	case "open", "partially_filled", "unknown", "pending":
		return true
	default:
		return false
	}
}
