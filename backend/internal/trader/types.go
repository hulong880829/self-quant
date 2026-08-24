package trader

import (
	"errors"
	"time"
)

var (
	ErrInvalidArgument       = errors.New("invalid argument")
	ErrNotFound              = errors.New("not found")
	ErrIdempotencyConflict   = errors.New("idempotency key conflict")
	ErrActiveTwapLimit       = errors.New("active twap limit reached")
	ErrUnsupportedExchange   = errors.New("unsupported exchange")
	ErrInstrumentUnavailable = errors.New("instrument unavailable")
	ErrOrderNotCancelable    = errors.New("order is not cancelable")
	ErrTwapNotCancelable     = errors.New("twap is not cancelable")
	ErrArbitrageConflict     = errors.New("arbitrage combination conflict")
	ErrArbitrageNotClosable  = errors.New("arbitrage combination is not closable")
	ErrMarketDataStale       = errors.New("market data is stale")
	ErrRiskLimit             = errors.New("arbitrage risk limit exceeded")
	ErrPersistence           = errors.New("order persistence failed")
	ErrVenueRejected         = errors.New("venue rejected order")
	ErrVenueRateLimited      = errors.New("venue rate limited")
	ErrVenueUnavailable      = errors.New("venue unavailable")
	ErrVenueUncertain        = errors.New("venue result uncertain")
)

type Instrument struct {
	ID             int64
	Exchange       string
	ContractType   string
	ExchangeSymbol string
	BaseAsset      string
	QuoteAsset     string
	SettleAsset    string
	ContractSize   string
	PriceTick      string
	QuantityStep   string
	Metadata       map[string]any
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
}

type VenueResult struct {
	VenueOrderID   string
	Status         string
	FilledQuantity string
	AveragePrice   string
	ErrorCode      string
	ErrorMessage   string
	Raw            map[string]any
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
	Result     VenueResult
	EventAt    time.Time
	ReceivedAt time.Time
	Fills      []OrderFill
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
