// Package hyperliquid provides a Go client library for the Hyperliquid
// exchange API. It includes support for both REST API and WebSocket
// connections, allowing users to access market data, manage orders, and
// handle user account operations.
package hyperliquid

import (
	"crypto/ecdsa"
	"errors"
	"net/http"
	"time"

	"github.com/Simon-Busch/hyperliquid-go/info"
	"github.com/Simon-Busch/hyperliquid-go/internal/transport"
	"github.com/Simon-Busch/hyperliquid-go/stream"
	"github.com/Simon-Busch/hyperliquid-go/trade"
	"github.com/Simon-Busch/hyperliquid-go/types"
)

// Client is the top-level Hyperliquid client. It exposes three handles:
//
//	c.Info   — read-only queries
//	c.Trade  — signed actions (requires a private key)
//	c.Stream — websocket subscriptions and POST requests
//
// Construct it with New and a list of options.
type Client struct {
	Info   *info.Client
	Trade  *trade.Client
	Stream *stream.Client
}

// New builds a Client configured by the supplied options. At minimum either
// WithMainnet, WithTestnet, or WithBaseURL must be specified for non-default
// behaviour. Signed actions on Trade require WithPrivateKey.
func New(opts ...Option) (*Client, error) {
	cfg := defaultClientConfig()
	for _, o := range opts {
		o(cfg)
	}

	api := transport.New(cfg.baseURL, cfg.httpClient)

	infoC := info.New(cfg.baseURL, true, cfg.meta, cfg.spotMeta, cfg.perpDexs, cfg.builderDex)

	c := &Client{Info: infoC}

	if cfg.privateKey != nil {
		c.Trade = trade.New(trade.Config{
			Client:       api,
			PrivateKey:   cfg.privateKey,
			Vault:        cfg.vault,
			AccountAddr:  cfg.account,
			Dex:          cfg.builderDex,
			Info:         infoC,
			ExpiresAfter: cfg.expiresAfter,
		})
	}

	if !cfg.skipStream {
		s, err := stream.New(cfg.baseURL)
		if err != nil {
			return nil, err
		}
		s.SetLogger(cfg.logger)
		s.SetMaxReconnectAttempts(cfg.maxReconnectAttempts)
		s.SetReconnectWait(cfg.reconnectWait)
		c.Stream = s
		if c.Trade != nil {
			c.Trade.SetStream(s)
		}
	}

	return c, nil
}

// clientConfig holds the options accumulated by New.
type clientConfig struct {
	baseURL              string
	httpClient           *http.Client
	privateKey           *ecdsa.PrivateKey
	account              string
	vault                string
	builderDex           string
	meta                 *info.Meta
	spotMeta             *info.SpotMeta
	perpDexs             *types.MixedArray
	skipStream           bool
	logger               stream.Logger
	maxReconnectAttempts int
	reconnectWait        time.Duration
	expiresAfter         *int64
}

func defaultClientConfig() *clientConfig {
	return &clientConfig{
		baseURL: types.MainnetAPIURL,
		logger:  nil, // stream.Client falls back to its internal nop logger
	}
}

// ErrMissingPrivateKey is returned by Trader methods called on a Client
// constructed without WithPrivateKey.
var ErrMissingPrivateKey = errors.New("hyperliquid: trader requires WithPrivateKey")
