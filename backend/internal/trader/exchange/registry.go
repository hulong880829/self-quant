package exchange

import (
	"io"
	"net/http"
	"strings"
	"time"
)

type Registry struct {
	adapters map[string]Adapter
}

func NewRegistry(client *http.Client, urls map[string]string) *Registry {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Registry{adapters: map[string]Adapter{
		"binance":     newBinance(client, urls["binance"]),
		"okx":         newOKX(client, urls["okx"]),
		"bybit":       newBybit(client, urls["bybit"]),
		"bitget":      newBitget(client, urls["bitget"]),
		"gate":        newGate(client, urls["gate"]),
		"hyperliquid": newHyperliquid(client, urls["hyperliquid"]),
		"lighter":     newLighter(client, urls["lighter"]),
		"aster":       newAster(client, urls["aster"]),
	}}
}

func (r *Registry) Adapter(exchange string) (Adapter, bool) {
	adapter, ok := r.adapters[strings.ToLower(strings.TrimSpace(exchange))]
	return adapter, ok
}

func (r *Registry) Close() error {
	if r == nil {
		return nil
	}
	for _, adapter := range r.adapters {
		if closer, ok := adapter.(io.Closer); ok {
			_ = closer.Close()
		}
	}
	return nil
}

func NewTestRegistry(adapters map[string]Adapter) *Registry {
	return &Registry{adapters: adapters}
}
