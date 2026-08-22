package polymarket

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	chainlinkTopic         = "crypto_prices_chainlink"
	chainlinkFlushInterval = 100 * time.Millisecond
)

type chainlinkObservation struct {
	asset      string
	price      string
	observedAt time.Time
}

type ChainlinkStream struct {
	url     string
	service *Service
	logger  *slog.Logger
}

func NewChainlinkStream(url string, service *Service, logger *slog.Logger) *ChainlinkStream {
	return &ChainlinkStream{url: url, service: service, logger: logger}
}

func (s *ChainlinkStream) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		if err := s.runOnce(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warn("chainlink RTDS disconnected", "error", err)
		}
		delay := backoff + time.Duration(rand.IntN(500))*time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (s *ChainlinkStream) runOnce(ctx context.Context) error {
	connection, _, err := websocket.DefaultDialer.DialContext(ctx, s.url, nil)
	if err != nil {
		return err
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(45 * time.Second))

	if err := connection.WriteJSON(map[string]any{
		"action":        "subscribe",
		"subscriptions": chainlinkSubscriptions(),
	}); err != nil {
		return err
	}

	done := make(chan struct{})
	defer close(done)

	pendingMu := sync.Mutex{}
	pending := make(map[string]chainlinkObservation)

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				_ = connection.WriteJSON(map[string]string{"action": "ping"})
			}
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-done:
		}
	}()
	go s.flushObservations(ctx, done, &pendingMu, &pending)

	for {
		_, body, err := connection.ReadMessage()
		if err != nil {
			return err
		}
		_ = connection.SetReadDeadline(time.Now().Add(45 * time.Second))
		var message any
		if json.Unmarshal(body, &message) != nil {
			continue
		}
		asset, price, timestamp, ok := parseChainlinkMessage(message)
		if !ok || asset == "" || price == "" {
			continue
		}
		pendingMu.Lock()
		pending[asset] = chainlinkObservation{
			asset: asset, price: price, observedAt: timestamp,
		}
		pendingMu.Unlock()
	}
}

func (s *ChainlinkStream) flushObservations(
	ctx context.Context,
	done <-chan struct{},
	pendingMu *sync.Mutex,
	pending *map[string]chainlinkObservation,
) {
	ticker := time.NewTicker(chainlinkFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			pendingMu.Lock()
			if len(*pending) == 0 {
				pendingMu.Unlock()
				continue
			}
			batch := *pending
			*pending = make(map[string]chainlinkObservation, len(batch))
			pendingMu.Unlock()
			for _, observation := range batch {
				s.service.RecordChainlinkPrice(
					ctx, observation.asset, observation.price, observation.observedAt,
				)
			}
		}
	}
}

func chainlinkSubscriptions() []map[string]string {
	return []map[string]string{{
		"topic": chainlinkTopic,
		"type":  "update",
	}}
}

func parseChainlinkMessage(value any) (string, string, time.Time, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return "", "", time.Time{}, false
	}
	topic := strings.ToLower(firstString(object, "topic"))
	if topic != "" && topic != chainlinkTopic {
		return "", "", time.Time{}, false
	}
	payload := object
	if nested, ok := object["payload"].(map[string]any); ok {
		payload = nested
	}
	symbol := strings.ToLower(firstString(payload, "symbol", "asset", "pair"))
	if symbol != "" && !strings.HasSuffix(symbol, "/usd") {
		return "", "", time.Time{}, false
	}
	price := firstString(payload, "value", "price")
	if price == "" {
		for _, key := range []string{"value", "price"} {
			if number, ok := payload[key].(float64); ok {
				price = strconv.FormatFloat(number, 'f', -1, 64)
			}
		}
	}
	parts := strings.FieldsFunc(symbol, func(r rune) bool {
		return r == '/' || r == '-' || r == '_' || r == ':'
	})
	if len(parts) == 0 {
		return "", "", time.Time{}, false
	}
	asset := strings.ToUpper(parts[0])
	if !isSupportedAsset(asset) {
		return "", "", time.Time{}, false
	}
	timestamp := time.Now().UTC()
	if raw, ok := payload["timestamp"].(float64); ok {
		if raw > 1e12 {
			timestamp = time.UnixMilli(int64(raw)).UTC()
		} else {
			timestamp = time.Unix(int64(raw), 0).UTC()
		}
	} else if raw := firstString(payload, "timestamp", "observedAt"); raw != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			timestamp = parsed
		}
	}
	return asset, price, timestamp, true
}

func isSupportedAsset(asset string) bool {
	for _, candidate := range supportedAssets {
		if asset == candidate {
			return true
		}
	}
	return false
}

func firstString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := object[key].(string); ok {
			return value
		}
	}
	return ""
}
