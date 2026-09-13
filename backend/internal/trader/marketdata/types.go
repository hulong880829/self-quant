package marketdata

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	VenueBinance     = "binance"
	VenueOKX         = "okx"
	VenueBybit       = "bybit"
	VenueBitget      = "bitget"
	VenueGate        = "gate"
	VenueHyperliquid = "hyperliquid"
	VenueAster       = "aster"
	VenueLighter     = "lighter"

	ProductSpot      = "spot"
	ProductPerpetual = "perpetual"
)

var (
	ErrClosed                 = errors.New("market data subscription closed")
	ErrNoValue                = errors.New("market data value unavailable")
	ErrStale                  = errors.New("market data value stale")
	ErrUnsupportedKey         = errors.New("unsupported market data key")
	ErrSubscriptionRejected   = errors.New("market data subscription rejected")
	ErrSubscriptionAckTimeout = errors.New("market data subscription acknowledgement timeout")
	ErrSequenceGap            = errors.New("market data sequence gap")
	ErrBookUnavailable        = errors.New("market data order book unavailable")
	ErrConnectionRotation     = errors.New("market data connection rotation")
)

// Key uniquely identifies a venue instrument. Symbol is the venue-native symbol.
type Key struct {
	Venue   string
	Product string
	Symbol  string
}

func NewKey(venue, product, symbol string) (Key, error) {
	key := Key{
		Venue:   strings.ToLower(strings.TrimSpace(venue)),
		Product: strings.ToLower(strings.TrimSpace(product)),
		Symbol:  strings.TrimSpace(symbol),
	}
	if err := key.Validate(); err != nil {
		return Key{}, err
	}
	return key, nil
}

func (k Key) Validate() error {
	switch k.Venue {
	case VenueBinance, VenueOKX, VenueBybit, VenueBitget, VenueGate,
		VenueHyperliquid, VenueAster, VenueLighter:
	default:
		return fmt.Errorf("%w: venue %q", ErrUnsupportedKey, k.Venue)
	}
	switch k.Product {
	case ProductSpot, ProductPerpetual:
	default:
		return fmt.Errorf("%w: product %q", ErrUnsupportedKey, k.Product)
	}
	switch k.Venue {
	case VenueHyperliquid, VenueAster, VenueLighter:
		if k.Product != ProductPerpetual {
			return fmt.Errorf(
				"%w: venue %q only supports perpetual BBO", ErrUnsupportedKey, k.Venue,
			)
		}
	}
	if strings.TrimSpace(k.Symbol) == "" {
		return fmt.Errorf("%w: empty symbol", ErrUnsupportedKey)
	}
	return nil
}

// BBO is the latest best bid and offer from a public venue stream.
type BBO struct {
	Key              Key
	BidPrice         string
	AskPrice         string
	BidQuantity      string
	AskQuantity      string
	VenueTimestamp   time.Time
	ReceiveTimestamp time.Time
}

// Stale reports whether the value is older than maxAge according to local receipt time.
func (b BBO) Stale(now time.Time, maxAge time.Duration) bool {
	return maxAge <= 0 || b.ReceiveTimestamp.IsZero() || now.Sub(b.ReceiveTimestamp) > maxAge
}
