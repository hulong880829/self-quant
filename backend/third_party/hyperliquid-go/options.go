package hyperliquid

import (
	"crypto/ecdsa"
	"net/http"
	"time"

	"github.com/Simon-Busch/hyperliquid-go/info"
	"github.com/Simon-Busch/hyperliquid-go/stream"
	"github.com/Simon-Busch/hyperliquid-go/types"
)

// Option configures the top-level Client. Pass options to New.
type Option func(*clientConfig)

// WithMainnet targets the production Hyperliquid endpoint.
func WithMainnet() Option {
	return func(c *clientConfig) { c.baseURL = types.MainnetAPIURL }
}

// WithTestnet targets the Hyperliquid testnet endpoint.
func WithTestnet() Option {
	return func(c *clientConfig) { c.baseURL = types.TestnetAPIURL }
}

// WithBaseURL targets the supplied REST endpoint. Use for custom or local
// development servers.
func WithBaseURL(url string) Option {
	return func(c *clientConfig) { c.baseURL = url }
}

// WithPrivateKey provides the ECDSA key used to sign all Trader actions.
// Without it, c.Trade will be nil.
func WithPrivateKey(pk *ecdsa.PrivateKey) Option {
	return func(c *clientConfig) { c.privateKey = pk }
}

// WithAccount sets the account address Trader actions are submitted on
// behalf of (used by the agent flow when the signer differs from the owner).
func WithAccount(addr string) Option {
	return func(c *clientConfig) { c.account = addr }
}

// WithVault sets the vault address Trader actions act on.
func WithVault(addr string) Option {
	return func(c *clientConfig) { c.vault = addr }
}

// WithBuilderDex pins the client to a HIP-3 builder-deployed perp dex.
func WithBuilderDex(dex string) Option {
	return func(c *clientConfig) { c.builderDex = dex }
}

// WithHTTPClient supplies a caller-owned *http.Client for full transport
// control (custom timeouts, connection pooling, proxies, etc.).
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *clientConfig) { c.httpClient = httpClient }
}

// WithMeta supplies pre-fetched metadata (perp meta, spot meta, perp dex
// list) so New does not need to round-trip to /info during construction.
func WithMeta(meta *info.Meta, spotMeta *info.SpotMeta, perpDexs *types.MixedArray) Option {
	return func(c *clientConfig) {
		c.meta = meta
		c.spotMeta = spotMeta
		c.perpDexs = perpDexs
	}
}

// WithSkipStream disables construction of the websocket Stream. Useful for
// REST-only callers that don't want a background goroutine.
func WithSkipStream(skip bool) Option {
	return func(c *clientConfig) { c.skipStream = skip }
}

// WithLogger plugs in a Logger. The default is a no-op logger.
func WithLogger(l stream.Logger) Option {
	return func(c *clientConfig) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithMaxReconnectAttempts caps how many times the Stream will retry the
// websocket connection before giving up. A value of 0 (the default) means
// retry forever.
func WithMaxReconnectAttempts(n int) Option {
	return func(c *clientConfig) { c.maxReconnectAttempts = n }
}

// WithReconnectWait sets the initial backoff used by the Stream's
// reconnect loop. The default is 1 second; each failed attempt doubles
// the wait up to an internal one-minute ceiling.
func WithReconnectWait(d time.Duration) Option {
	return func(c *clientConfig) { c.reconnectWait = d }
}

// WithExpiresAfter pins an expiration time onto every signed action
// dispatched by the Trader. The deadline is forwarded as a Unix
// milliseconds value on the wire. Use Trader.SetExpiresAfter to mutate
// the deadline at runtime.
func WithExpiresAfter(deadline time.Time) Option {
	return func(c *clientConfig) {
		ms := deadline.UnixMilli()
		c.expiresAfter = &ms
	}
}
