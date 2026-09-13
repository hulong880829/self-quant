package exchange

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

var (
	ErrUnsupported     = errors.New("unsupported exchange")
	ErrUncertain       = errors.New("venue result uncertain")
	ErrRejected        = errors.New("venue rejected order")
	ErrAmbiguousCancel = errors.New("venue cancel result ambiguous")
	ErrOrderNotFound   = errors.New("venue order not found")
	ErrRateLimited     = errors.New("venue rate limited")
	ErrInvalidQuantity = errors.New("invalid order quantity")
)

const (
	ConstraintKnown         = "known"
	ConstraintNotApplicable = "not_applicable"
	ConstraintUnknown       = "unknown"
)

type Credentials struct {
	APIKey, APISecret, Passphrase string
	CredentialKind                string
	SigningAddress                string
	VaultAddress                  string
	AccountIndex                  *int64
	APIKeyIndex                   *int32
}

type VenueReference struct {
	ClientOrderID    string
	VenueOrderID     string
	Cloid            string
	TxHash           string
	Nonce            int64
	AccountIndex     int64
	APIKeyIndex      int32
	ClientOrderIndex int64
	EventAt          time.Time
	ReconcileStatus  string
}

type Instrument struct {
	Exchange, ContractType, ExchangeSymbol string
	BaseAsset, QuoteAsset, SettleAsset     string
	ContractSize                           string
	PriceTick                              string
	QuantityStep                           string
	MinQuantity                            string
	MinNotional                            string
	MinQuantityStatus                      string
	MinNotionalStatus                      string
	MaxQuantity                            string
	MarketQuantityStep                     string
	MarketMinQuantity                      string
	MarketMaxQuantity                      string
	MarketMinNotional                      string
	MaxQuantityStatus                      string
	MarketQuantityStepStatus               string
	MarketMinQuantityStatus                string
	MarketMaxQuantityStatus                string
	MarketMinNotionalStatus                string
	Metadata                               map[string]any
}

type OrderRequest struct {
	Instrument    Instrument
	ClientOrderID string
	Side          string
	OrderType     string
	Quantity      string
	Price         string
	TimeInForce   string
	PostOnly      bool
	ReduceOnly    bool
}

type BBO struct {
	BidPrice  string
	AskPrice  string
	Timestamp time.Time
}

type QueryRequest struct {
	Instrument    Instrument
	ClientOrderID string
	VenueOrderID  string
	CreatedAt     time.Time
}

type CancelRequest struct {
	Instrument    Instrument
	ClientOrderID string
	VenueOrderID  string
}

type Result struct {
	VenueOrderID    string
	Status          string
	FilledQuantity  string
	AveragePrice    string
	ErrorCode       string
	ErrorMessage    string
	Raw             map[string]any
	Reference       VenueReference
	LocalCommandAck bool
}

type Adapter interface {
	GetBBO(context.Context, Instrument) (BBO, error)
	PlaceOrder(context.Context, Credentials, OrderRequest) (Result, error)
	GetOrder(context.Context, Credentials, QueryRequest) (Result, error)
	// CancelOrder sends a cancel command and returns the command ACK.
	// It must not query the order.
	CancelOrder(context.Context, Credentials, CancelRequest) (Result, error)
	// CancelAndGetOrder cancels and confirms the order using the venue's
	// existing post-cancel lookup rules.
	CancelAndGetOrder(context.Context, Credentials, CancelRequest) (Result, error)
}

// OrderTransportWarmer is an optional adapter capability. Hyperliquid uses it
// to pre-establish the order-submission websocket before maker exposure.
type OrderTransportWarmer interface {
	WarmOrderTransport(context.Context, Credentials, Instrument) error
}

// OrderResolution is optional evidence used after a single-order lookup returns
// ErrOrderNotFound. ConfirmedAbsent means both history and active-order queries
// completed successfully without finding the order.
type OrderResolution struct {
	Result          Result
	Found           bool
	Active          bool
	ConfirmedAbsent bool
}

type OrderResolver interface {
	ResolveOrder(context.Context, Credentials, QueryRequest) (OrderResolution, error)
}

type PositionModeReader interface {
	GetPositionMode(context.Context, Credentials, Instrument) (string, error)
}

type LeverageApplyResult struct {
	MaxNotional   decimal.Decimal
	CapacityKnown bool
}

type LeverageSetter interface {
	SetLeverage(context.Context, Credentials, Instrument, decimal.Decimal) (LeverageApplyResult, error)
}

const EstMaxOpenUnitQuoteNotional = "quote_notional"

type LeverageSetPreview struct {
	EstMaxOpen     string
	EstMaxOpenUnit string
	RequiredMargin decimal.Decimal
	MarginChange   decimal.Decimal
}

type LeverageSetPreviewer interface {
	PreviewSetLeverage(context.Context, Credentials, Instrument, decimal.Decimal) (LeverageSetPreview, error)
}

type Capabilities struct {
	Products           []string
	QuoteAssets        []string
	TimeInForce        []string
	PostOnly           bool
	ReduceOnly         bool
	MakerTwap          bool
	PrivateOrderStream bool
	OneWayOnly         bool
	Arbitrage          bool
}

type CapabilityProvider interface {
	Capabilities(context.Context, Credentials) (Capabilities, error)
}

type ClientOrderIDEncoder interface {
	VenueClientOrderID(string) string
}

type Fill struct {
	TradeID, VenueOrderID, Quantity, Price string
	ExecutedAt                             time.Time
}

type FillReader interface {
	ListFills(context.Context, Credentials, Instrument, time.Time) ([]Fill, error)
}

type Position struct {
	Instrument string
	Quantity   string
	EntryPrice string
}

type PositionReader interface {
	ListPositions(context.Context, Credentials) ([]Position, error)
}

type Balance struct {
	Asset, Total, Available string
}

type BalanceReader interface {
	ListBalances(context.Context, Credentials) ([]Balance, error)
}

type Health struct {
	Healthy   bool
	CheckedAt time.Time
	Message   string
}

type HealthReader interface {
	Health(context.Context, Credentials) Health
}

const (
	PositionModeOneWay = "one_way"
	PositionModeHedge  = "hedge"
)

func normalizeStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "new", "live", "open", "submitted", "untriggered", "partiallyfilled", "partial-fill", "partially_filled":
		if strings.Contains(strings.ToLower(value), "partial") {
			return "partially_filled"
		}
		return "open"
	case "filled", "closed", "completely_filled":
		return "filled"
	case "canceled", "cancelled", "cancel", "deactivated":
		return "canceled"
	case "rejected", "expire", "expired":
		if strings.Contains(strings.ToLower(value), "expir") {
			return "expired"
		}
		return "rejected"
	case "pending", "created":
		return "pending"
	default:
		lower := strings.ToLower(value)
		switch {
		case strings.Contains(lower, "partial"):
			return "partially_filled"
		case strings.Contains(lower, "fill"):
			return "filled"
		case strings.Contains(lower, "cancel"):
			return "canceled"
		case strings.Contains(lower, "reject"):
			return "rejected"
		case strings.Contains(lower, "expir"):
			return "expired"
		case strings.Contains(lower, "new") || strings.Contains(lower, "live") || strings.Contains(lower, "open"):
			return "open"
		default:
			return "unknown"
		}
	}
}

func terminalOrderStatus(status string) bool {
	switch normalizeStatus(status) {
	case "filled", "canceled", "rejected", "expired":
		return true
	default:
		return false
	}
}

func metadataString(instrument Instrument, name string) string {
	for key, value := range instrument.Metadata {
		if !strings.EqualFold(key, name) {
			continue
		}
		if text, ok := value.(string); ok {
			return text
		}
		if value != nil {
			return fmt.Sprint(value)
		}
	}
	return ""
}
