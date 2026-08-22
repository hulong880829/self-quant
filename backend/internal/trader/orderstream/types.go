package orderstream

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	VenueBinance = "binance"
	VenueOKX     = "okx"
	VenueBybit   = "bybit"
	VenueBitget  = "bitget"
	VenueGate    = "gate"

	ProductSpot      = "spot"
	ProductPerpetual = "perpetual"
)

var (
	ErrClosed         = errors.New("order stream closed")
	ErrUnsupportedKey = errors.New("unsupported order stream key")
	ErrNoSession      = errors.New("order stream session not found")
)

// Key identifies one account-level private stream.
type Key struct {
	Account string
	Venue   string
	Product string
}

func (k Key) normalized() (Key, error) {
	k.Account = strings.TrimSpace(k.Account)
	k.Venue = strings.ToLower(strings.TrimSpace(k.Venue))
	k.Product = strings.ToLower(strings.TrimSpace(k.Product))
	if k.Account == "" {
		return Key{}, fmt.Errorf("%w: empty account", ErrUnsupportedKey)
	}
	switch k.Venue {
	case VenueBinance, VenueOKX, VenueBybit, VenueBitget, VenueGate:
	default:
		return Key{}, fmt.Errorf("%w: venue %q", ErrUnsupportedKey, k.Venue)
	}
	switch k.Product {
	case ProductSpot, ProductPerpetual:
	default:
		return Key{}, fmt.Errorf("%w: product %q", ErrUnsupportedKey, k.Product)
	}
	// Unified-account private streams are account-wide for these venues. Use a
	// canonical product key so spot and perpetual arbitrage share one session.
	switch k.Venue {
	case VenueBinance, VenueOKX, VenueBybit, VenueBitget:
		k.Product = ProductPerpetual
	}
	return k, nil
}

// Credentials is deliberately self-contained to avoid importing trader packages.
type Credentials struct {
	APIKey     string
	Secret     string
	Passphrase string
}

func (c Credentials) validate() error {
	if strings.TrimSpace(c.APIKey) == "" || strings.TrimSpace(c.Secret) == "" {
		return errors.New("order stream credentials require API key and secret")
	}
	return nil
}

type UpdateType string

const (
	UpdateOrder      UpdateType = "order"
	UpdateTrade      UpdateType = "trade"
	UpdateConnection UpdateType = "connection"
)

type Status string

const (
	StatusUnknown         Status = "unknown"
	StatusNew             Status = "new"
	StatusPartiallyFilled Status = "partially_filled"
	StatusFilled          Status = "filled"
	StatusCanceled        Status = "canceled"
	StatusRejected        Status = "rejected"
	StatusExpired         Status = "expired"
	StatusDisconnected    Status = "disconnected"
	StatusConnected       Status = "connected"
)

// Update is the normalized order, execution, or connection event.
// Numeric values remain strings to preserve exchange precision.
type Update struct {
	Type             UpdateType
	Account          string
	Venue            string
	Product          string
	ClientOrderID    string
	VenueOrderID     string
	Status           Status
	CumulativeFilled string
	AveragePrice     string
	LastFilled       string
	LastPrice        string
	TradeID          string
	EventTime        time.Time
	Sequence         int64
	Error            string
}

type VenueURLs struct {
	SpotWS        string
	PerpetualWS   string
	SpotREST      string
	PerpetualREST string
}

type URLs struct {
	Binance VenueURLs
	OKX     VenueURLs
	Bybit   VenueURLs
	Bitget  VenueURLs
	Gate    VenueURLs
}

func DefaultURLs() URLs {
	return URLs{
		Binance: VenueURLs{
			SpotWS: "wss://fstream.binance.com/pm/ws", PerpetualWS: "wss://fstream.binance.com/pm/ws",
			SpotREST: "https://papi.binance.com", PerpetualREST: "https://papi.binance.com",
		},
		OKX:    VenueURLs{SpotWS: "wss://ws.okx.com:8443/ws/v5/private", PerpetualWS: "wss://ws.okx.com:8443/ws/v5/private"},
		Bybit:  VenueURLs{SpotWS: "wss://stream.bybit.com/v5/private", PerpetualWS: "wss://stream.bybit.com/v5/private"},
		Bitget: VenueURLs{SpotWS: "wss://ws.bitget.com/v3/ws/private", PerpetualWS: "wss://ws.bitget.com/v3/ws/private"},
		Gate:   VenueURLs{SpotWS: "wss://api.gateio.ws/ws/v4/", PerpetualWS: "wss://fx-ws.gateio.ws/v4/ws/usdt"},
	}
}
