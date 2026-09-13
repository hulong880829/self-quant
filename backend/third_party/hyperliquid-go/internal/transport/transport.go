// Package transport holds the default HTTP transport tuned for low-latency
// Hyperliquid API calls. Keeping it internal lets us change the knobs
// without it being part of the public surface.
package transport

import (
	"net"
	"net/http"
	"time"
)

// Default returns an http.Transport tuned for low-latency Hyperliquid API
// calls: HTTP/2, generous idle pool, no gzip overhead.
func Default() *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
		DisableCompression:  true,
	}
}
