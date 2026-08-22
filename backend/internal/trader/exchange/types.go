package exchange

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrUnsupported = errors.New("unsupported exchange")
	ErrUncertain   = errors.New("venue result uncertain")
	ErrRejected    = errors.New("venue rejected order")
	ErrRateLimited = errors.New("venue rate limited")
)

type Credentials struct {
	APIKey, APISecret, Passphrase string
}

type Instrument struct {
	Exchange, ContractType, ExchangeSymbol string
	BaseAsset, QuoteAsset, SettleAsset     string
	ContractSize                           string
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
}

type CancelRequest struct {
	Instrument    Instrument
	ClientOrderID string
	VenueOrderID  string
}

type Result struct {
	VenueOrderID   string
	Status         string
	FilledQuantity string
	AveragePrice   string
	ErrorCode      string
	ErrorMessage   string
	Raw            map[string]any
}

type Adapter interface {
	GetBBO(context.Context, Instrument) (BBO, error)
	PlaceOrder(context.Context, Credentials, OrderRequest) (Result, error)
	GetOrder(context.Context, Credentials, QueryRequest) (Result, error)
	CancelOrder(context.Context, Credentials, CancelRequest) (Result, error)
}

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
