package aggdata

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type Server struct {
	catalog          *Catalog
	store            *Store
	history          *History
	fairHistory      FairPriceHistoryReader
	logger           *slog.Logger
	browserToken     string
	allowedOrigins   map[string]struct{}
	maxSubscriptions int
	clients          chan struct{}
	resolution       time.Duration
	streamInterval   time.Duration
	fairInterval     time.Duration
	fairHeartbeat    time.Duration
	fairSubscribers  atomic.Int64
	wsWriteFailures  atomic.Uint64
	fairPublishLagNS atomic.Int64
	upgrader         websocket.Upgrader
}

func NewServer(
	catalog *Catalog,
	store *Store,
	history *History,
	logger *slog.Logger,
	browserToken string,
	allowedOrigins []string,
	maxClients, maxSubscriptions int,
	resolution, streamInterval time.Duration,
	fairIntervals ...time.Duration,
) *Server {
	origins := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		origins[origin] = struct{}{}
	}
	fairInterval, fairHeartbeat := 5*time.Millisecond, time.Second
	if len(fairIntervals) > 0 && fairIntervals[0] > 0 {
		fairInterval = fairIntervals[0]
	}
	if len(fairIntervals) > 1 && fairIntervals[1] > 0 {
		fairHeartbeat = fairIntervals[1]
	}
	server := &Server{
		catalog: catalog, store: store, history: history, logger: logger,
		browserToken: browserToken, allowedOrigins: origins,
		maxSubscriptions: maxSubscriptions, clients: make(chan struct{}, maxClients),
		resolution: resolution, streamInterval: streamInterval,
		fairInterval: fairInterval, fairHeartbeat: fairHeartbeat,
	}
	server.upgrader = websocket.Upgrader{
		HandshakeTimeout: 5 * time.Second,
		ReadBufferSize:   4096, WriteBufferSize: 64 * 1024,
		CheckOrigin: server.checkOrigin,
	}
	return server
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.Handle("GET /v1/markets", s.authorize(http.HandlerFunc(s.markets)))
	mux.Handle("GET /v1/markets/{symbol}/snapshot", s.authorize(http.HandlerFunc(s.snapshot)))
	mux.Handle("GET /v1/markets/{symbol}/spread-history", s.authorize(http.HandlerFunc(s.spreadHistory)))
	mux.Handle("GET /v1/markets/{symbol}/fair-price-history", s.authorize(http.HandlerFunc(s.fairPriceHistory)))
	mux.Handle("GET /v1/stream", s.authorize(http.HandlerFunc(s.stream)))
	return s.cors(mux)
}

func (s *Server) SetFairPriceHistory(reader FairPriceHistoryReader) {
	s.fairHistory = reader
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		origin := request.Header.Get("Origin")
		_, allowed := s.allowedOrigins[origin]
		_, allowAll := s.allowedOrigins["*"]
		if origin != "" && !allowed && !allowAll {
			writeJSON(writer, http.StatusForbidden, map[string]string{"error": "origin is not allowed"})
			return
		}
		if origin != "" {
			writer.Header().Set("Access-Control-Allow-Origin", origin)
			writer.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			writer.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			writer.Header().Add("Vary", "Origin")
		}
		if request.Method == http.MethodOptions {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (s *Server) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ready(writer http.ResponseWriter, _ *http.Request) {
	if s.catalog.Snapshot() == nil || !s.store.Ready() {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) authorize(next http.Handler) http.Handler {
	if s.browserToken == "" {
		return next
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		token := strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
		if request.URL.Path == "/v1/stream" && token == "" {
			token = request.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.browserToken)) != 1 {
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (s *Server) markets(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"markets": s.store.Markets()})
}

func (s *Server) snapshot(writer http.ResponseWriter, request *http.Request) {
	depth := 20
	if raw := request.URL.Query().Get("depth"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > maxDepth {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "depth must be within 1..50"})
			return
		}
		depth = parsed
	}
	market, err := s.store.Lookup(request.PathValue("symbol"))
	if err != nil {
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	response := snapshotJSON(market, depth)
	if response == nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "snapshot is not ready"})
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (s *Server) spreadHistory(writer http.ResponseWriter, request *http.Request) {
	if value := request.URL.Query().Get("range"); value != "" && value != "24h" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "only range=24h is supported"})
		return
	}
	if value := request.URL.Query().Get("resolution"); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed != s.resolution {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "unsupported resolution"})
			return
		}
	}
	bpsType := request.URL.Query().Get("type")
	if bpsType == "" {
		bpsType = "gated"
	}
	market, err := s.store.Lookup(request.PathValue("symbol"))
	if err != nil {
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	catalog := s.catalog.Snapshot()
	if catalog == nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "manifest is not available"})
		return
	}
	response, err := s.history.Query(
		request.Context(), catalog, market, s.resolution, bpsType, time.Now(),
	)
	if err != nil {
		s.logger.Warn("spread history query failed", "symbol", market.catalog.Symbol, "error", err)
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "history is unavailable"})
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

type fairPriceHistoryPointJSON struct {
	ObservedAt      string        `json:"observed_at"`
	SourceWallNS    string        `json:"source_wall_ns"`
	RingEpoch       string        `json:"ring_epoch"`
	Sequence        string        `json:"seq"`
	Price           fairFixedJSON `json:"price"`
	Degraded        bool          `json:"degraded"`
	DegradedReasons []string      `json:"degraded_reasons"`
}

type fairPriceHistoryResponse struct {
	Profile      string                      `json:"profile"`
	Symbol       string                      `json:"symbol"`
	ModelID      string                      `json:"model_id"`
	Start        string                      `json:"start"`
	End          string                      `json:"end"`
	ResolutionMS int64                       `json:"resolution_ms"`
	Points       []fairPriceHistoryPointJSON `json:"points"`
}

func (s *Server) fairPriceHistory(writer http.ResponseWriter, request *http.Request) {
	if s.fairHistory == nil || !s.store.FairPriceEnabled() {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{
			"error": "fair price history is unavailable",
		})
		return
	}
	profile := strings.TrimSpace(request.URL.Query().Get("profile"))
	var market *liveMarket
	var err error
	if profile == "" {
		market, err = s.store.Lookup(request.PathValue("symbol"))
	} else {
		market, err = s.store.LookupProfile(profile, request.PathValue("symbol"))
	}
	if err != nil {
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	start, startErr := time.Parse(time.RFC3339Nano, request.URL.Query().Get("start"))
	end, endErr := time.Parse(time.RFC3339Nano, request.URL.Query().Get("end"))
	if startErr != nil || endErr != nil || !start.Before(end) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{
			"error": "start and end must be valid RFC3339 timestamps with start before end",
		})
		return
	}
	if end.Sub(start) > 24*time.Hour {
		writeJSON(writer, http.StatusBadRequest, map[string]string{
			"error": "fair price history range must not exceed 24h",
		})
		return
	}
	resolution, err := fairHistoryResolution(
		request.URL.Query().Get("resolution"), end.Sub(start),
	)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	modelID := s.store.FairPriceModelID()
	queryCtx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	points, err := s.fairHistory.QueryHistory(
		queryCtx,
		market.catalog.Profile,
		market.catalog.Symbol,
		modelID,
		start,
		end,
		resolution,
		2000,
	)
	if err != nil {
		if !errors.Is(err, ErrFairPriceHistoryUnavailable) {
			s.logger.Warn(
				"fair price history query failed",
				"profile", market.catalog.Profile,
				"symbol", market.catalog.Symbol,
				"error", err,
			)
		}
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{
			"error": "fair price history is unavailable",
		})
		return
	}
	encoded := make([]fairPriceHistoryPointJSON, 0, len(points))
	for _, point := range points {
		encoded = append(encoded, fairPriceHistoryPointJSON{
			ObservedAt:      point.ObservedAt.UTC().Format(time.RFC3339Nano),
			SourceWallNS:    strconv.FormatUint(point.SourceWallNS, 10),
			RingEpoch:       point.RingEpoch,
			Sequence:        point.RingSequence,
			Price:           fixedJSONValue(point.Price),
			Degraded:        point.Degraded,
			DegradedReasons: point.DegradedReasons,
		})
	}
	writeJSON(writer, http.StatusOK, fairPriceHistoryResponse{
		Profile:      market.catalog.Profile,
		Symbol:       market.catalog.Symbol,
		ModelID:      modelID,
		Start:        start.UTC().Format(time.RFC3339Nano),
		End:          end.UTC().Format(time.RFC3339Nano),
		ResolutionMS: resolution.Milliseconds(),
		Points:       encoded,
	})
}

func fairHistoryResolution(value string, span time.Duration) (time.Duration, error) {
	switch strings.TrimSpace(value) {
	case "", "auto":
		switch {
		case span <= 10*time.Minute:
			return time.Second, nil
		case span <= 30*time.Minute:
			return 5 * time.Second, nil
		case span <= 2*time.Hour:
			return 15 * time.Second, nil
		default:
			return time.Minute, nil
		}
	case "1s":
		return time.Second, nil
	case "5s":
		return 5 * time.Second, nil
	case "15s":
		return 15 * time.Second, nil
	case "1m":
		return time.Minute, nil
	default:
		return 0, fmt.Errorf("resolution must be auto, 1s, 5s, 15s, or 1m")
	}
}

type fixedJSON struct {
	Mantissa string `json:"mantissa"`
	Scale    uint8  `json:"scale"`
}

type contributionJSON struct {
	Venue    string    `json:"venue"`
	Quantity fixedJSON `json:"quantity"`
}

type levelJSON struct {
	Price         fixedJSON          `json:"price"`
	Quantity      fixedJSON          `json:"quantity"`
	VenueMask     uint32             `json:"venue_mask"`
	Contributions []contributionJSON `json:"contributions"`
}

func snapshotJSON(market *liveMarket, depth int) map[string]any {
	bbo, book := market.bbo.value.Load(), market.book.value.Load()
	if (bbo == nil || !bbo.Ready) && (book == nil || !book.Ready) {
		return nil
	}
	result := map[string]any{
		"symbol": market.catalog.Symbol, "profile": market.catalog.Profile,
		"ready": true,
	}
	if bbo != nil && bbo.Ready {
		value := bbo.BBO
		result["bbo"] = map[string]any{
			"sequence":    strconv.FormatUint(bbo.RingSequence, 10),
			"generation":  strconv.FormatUint(bbo.Generation, 10),
			"wall_ns":     strconv.FormatUint(bbo.WallNS, 10),
			"price_scale": value.PriceScale, "quantity_scale": value.QuantityScale,
			"gated_bid":      sideJSON(value.GatedBid, value, value.QuantityScale),
			"gated_ask":      sideJSON(value.GatedAsk, value, value.QuantityScale),
			"raw_bid":        rawSideJSON(value.RawBid, value.PriceScale, value.QuantityScale),
			"raw_ask":        rawSideJSON(value.RawAsk, value.PriceScale, value.QuantityScale),
			"raw_spread_bps": value.RawSpreadBPS, "gated_spread_bps": value.GatedSpreadBPS,
			"raw_cross_bps": value.RawCrossBPS, "gated_cross_bps": value.GatedCrossBPS,
		}
	}
	if book != nil && book.Ready {
		value := book.Book
		bids, asks := min(depth, len(value.Bids)), min(depth, len(value.Asks))
		result["orderbook"] = map[string]any{
			"sequence":    strconv.FormatUint(book.RingSequence, 10),
			"generation":  strconv.FormatUint(book.Generation, 10),
			"wall_ns":     strconv.FormatUint(book.WallNS, 10),
			"price_scale": value.PriceScale, "quantity_scale": value.QuantityScale,
			"bids": levelsJSON(value.Bids[:bids], value.VenueSlotIDs, value.PriceScale, value.QuantityScale),
			"asks": levelsJSON(value.Asks[:asks], value.VenueSlotIDs, value.PriceScale, value.QuantityScale),
		}
	}
	return result
}

func sideJSON(side Side, bbo *BBO, quantityScale uint8) levelJSON {
	return levelJSON{
		Price:         fixedJSON{Mantissa: strconv.FormatInt(side.Price, 10), Scale: bbo.PriceScale},
		Quantity:      fixedJSON{Mantissa: strconv.FormatInt(side.Quantity, 10), Scale: quantityScale},
		VenueMask:     side.VenueMask,
		Contributions: contributionsJSON(side.VenueQuantity, side.VenueMask, bbo.VenueSlotIDs, quantityScale),
	}
}

func rawSideJSON(side RawSide, priceScale, quantityScale uint8) map[string]any {
	return map[string]any{
		"price":      fixedJSON{strconv.FormatInt(side.Price, 10), priceScale},
		"quantity":   fixedJSON{strconv.FormatInt(side.Quantity, 10), quantityScale},
		"venue_mask": side.VenueMask, "best_venue": side.BestVenue,
	}
}

func levelsJSON(levels []Level, slots [8]uint8, priceScale, quantityScale uint8) []levelJSON {
	result := make([]levelJSON, 0, len(levels))
	for _, level := range levels {
		result = append(result, levelJSON{
			Price:         fixedJSON{strconv.FormatInt(level.Price, 10), priceScale},
			Quantity:      fixedJSON{strconv.FormatInt(level.Quantity, 10), quantityScale},
			VenueMask:     level.VenueMask,
			Contributions: contributionsJSON(level.VenueQuantity, level.VenueMask, slots, quantityScale),
		})
	}
	return result
}

func contributionsJSON(quantities [8]int64, mask uint32, slots [8]uint8, scale uint8) []contributionJSON {
	var result []contributionJSON
	for index, quantity := range quantities {
		if mask&(1<<index) != 0 {
			result = append(result, contributionJSON{
				Venue:    venueName(slots[index]),
				Quantity: fixedJSON{Mantissa: strconv.FormatInt(quantity, 10), Scale: scale},
			})
		}
	}
	return result
}

func (s *Server) checkOrigin(request *http.Request) bool {
	origin := request.Header.Get("Origin")
	if _, all := s.allowedOrigins["*"]; all {
		return true
	}
	_, allowed := s.allowedOrigins[origin]
	return origin != "" && allowed
}

type streamChannel uint8

const (
	streamBBO streamChannel = iota + 1
	streamOrderBook
	streamFairPrice
)

type streamSubscription struct {
	profile      string
	symbol       string
	marketKey    string
	channel      streamChannel
	depth        int
	lastEpoch    uint64
	lastSequence uint64
	sent         bool
	resetSent    bool
	acked        bool
	lastWrite    time.Time
}

type streamControlResponse struct {
	payload      []byte
	subscription *streamSubscription
}

func (s *Server) stream(writer http.ResponseWriter, request *http.Request) {
	select {
	case s.clients <- struct{}{}:
		defer func() { <-s.clients }()
	default:
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "client limit reached"})
		return
	}
	connection, err := s.upgrader.Upgrade(writer, request, nil)
	if err != nil {
		return
	}
	defer connection.Close()
	connection.SetReadLimit(4096)
	_ = connection.SetReadDeadline(time.Now().Add(45 * time.Second))
	connection.SetPongHandler(func(string) error {
		return connection.SetReadDeadline(time.Now().Add(45 * time.Second))
	})

	var mu sync.RWMutex
	subscriptions := make(map[string]*streamSubscription)
	controls := make(chan streamControlResponse, 8)
	stopWriter := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.streamWriter(connection, &mu, subscriptions, controls, stopWriter)
	}()
	defer func() {
		close(stopWriter)
		_ = connection.Close()
		<-done
	}()
	defer func() {
		mu.RLock()
		var fairCount int64
		for _, subscription := range subscriptions {
			if subscription.channel == streamFairPrice {
				fairCount++
			}
		}
		mu.RUnlock()
		s.fairSubscribers.Add(-fairCount)
	}()
	for {
		var control struct {
			Op      string `json:"op"`
			Profile string `json:"profile"`
			Symbol  string `json:"symbol"`
			Channel string `json:"channel"`
			Depth   int    `json:"depth"`
		}
		if err := connection.ReadJSON(&control); err != nil {
			return
		}
		control.Channel = strings.ToLower(strings.TrimSpace(control.Channel))
		control.Profile = strings.TrimSpace(control.Profile)
		ack := map[string]any{"ok": true, "op": control.Op}
		var market *liveMarket
		var err error
		if control.Profile == "" {
			market, err = s.store.Lookup(control.Symbol)
		} else {
			market, err = s.store.LookupProfile(control.Profile, control.Symbol)
		}
		channel := streamBBO
		switch control.Channel {
		case "bbo":
		case "orderbook":
			channel = streamOrderBook
		case "fairprice":
			channel = streamFairPrice
			if control.Depth != 0 {
				err = fmt.Errorf("depth is not supported for fairprice")
			} else if !s.store.FairPriceEnabled() {
				err = fmt.Errorf("fairprice is disabled")
			}
		default:
			err = fmt.Errorf("channel must be bbo, orderbook, or fairprice")
		}
		if channel != streamFairPrice {
			if control.Depth == 0 {
				control.Depth = 20
			}
			if control.Depth != 20 && control.Depth != 50 {
				err = fmt.Errorf("depth must be 20 or 50")
			}
		}
		key := marketIdentityIndexKey(control.Profile, control.Symbol) + ":" + control.Channel
		if market != nil {
			key = s.store.MarketKey(market) + ":" + control.Channel
		}
		var subscribed *streamSubscription
		mu.Lock()
		if err == nil {
			switch control.Op {
			case "subscribe":
				if _, exists := subscriptions[key]; !exists && len(subscriptions) >= s.maxSubscriptions {
					err = fmt.Errorf("subscription limit reached")
				} else {
					_, existed := subscriptions[key]
					subscribed = &streamSubscription{
						profile:   market.catalog.Profile,
						symbol:    market.catalog.Symbol,
						marketKey: s.store.MarketKey(market),
						channel:   channel, depth: control.Depth,
					}
					subscriptions[key] = subscribed
					if !existed && channel == streamFairPrice {
						s.fairSubscribers.Add(1)
					}
				}
			case "unsubscribe":
				if existing, exists := subscriptions[key]; exists {
					delete(subscriptions, key)
					if existing.channel == streamFairPrice {
						s.fairSubscribers.Add(-1)
					}
				}
			default:
				err = fmt.Errorf("op must be subscribe or unsubscribe")
			}
		}
		mu.Unlock()
		if err != nil {
			ack["ok"], ack["error"] = false, err.Error()
		} else if market != nil {
			ack["profile"] = market.catalog.Profile
			ack["symbol"] = market.catalog.Symbol
			ack["channel"] = control.Channel
		}
		payload, _ := json.Marshal(ack)
		select {
		case controls <- streamControlResponse{payload: payload, subscription: subscribed}:
		default:
			return
		}
	}
}

func (s *Server) streamWriter(
	connection *websocket.Conn,
	mu *sync.RWMutex,
	subscriptions map[string]*streamSubscription,
	controls <-chan streamControlResponse,
	stop <-chan struct{},
) {
	ticker := time.NewTicker(s.streamInterval)
	fairTicker := time.NewTicker(s.fairInterval)
	ping := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	defer fairTicker.Stop()
	defer ping.Stop()
	scratch := make([]*streamSubscription, 0, s.maxSubscriptions)
	for {
		select {
		case <-stop:
			return
		case control := <-controls:
			if !writeWS(connection, websocket.TextMessage, control.payload) {
				s.wsWriteFailures.Add(1)
				return
			}
			if control.subscription != nil {
				control.subscription.acked = true
			}
		case <-ping.C:
			if err := connection.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				return
			}
		case <-ticker.C:
			scratch = copySubscriptions(scratch[:0], mu, subscriptions)
			for _, subscription := range scratch {
				if !subscription.acked || subscription.channel == streamFairPrice {
					continue
				}
				market, ok := s.store.LookupKey(subscription.marketKey)
				if !ok {
					continue
				}
				kind := KindBBO
				if subscription.channel == streamOrderBook {
					kind = KindBook
				}
				snapshot := marketSnapshot(market, kind)
				if snapshot == nil {
					continue
				}
				if !snapshot.Ready {
					if subscription.sent && !subscription.resetSent {
						channel := "bbo"
						if subscription.channel == streamOrderBook {
							channel = "orderbook"
						}
						reset, _ := json.Marshal(map[string]string{
							"op": "reset", "profile": subscription.profile,
							"symbol":  subscription.symbol,
							"channel": channel,
						})
						if !writeWS(connection, websocket.TextMessage, reset) {
							s.wsWriteFailures.Add(1)
							return
						}
						subscription.sent = false
						subscription.resetSent = true
					}
					continue
				}
				if subscription.sent &&
					snapshot.RingEpoch == subscription.lastEpoch &&
					snapshot.RingSequence == subscription.lastSequence {
					continue
				}
				frame := snapshot.Browser20
				if subscription.depth == 50 {
					frame = snapshot.Browser50
				}
				if !writeWS(connection, websocket.BinaryMessage, frame) {
					s.wsWriteFailures.Add(1)
					return
				}
				subscription.lastEpoch = snapshot.RingEpoch
				subscription.lastSequence = snapshot.RingSequence
				subscription.sent = true
				subscription.resetSent = false
			}
		case now := <-fairTicker.C:
			scratch = copySubscriptions(scratch[:0], mu, subscriptions)
			for _, subscription := range scratch {
				if !subscription.acked || subscription.channel != streamFairPrice {
					continue
				}
				market, ok := s.store.LookupKey(subscription.marketKey)
				if !ok {
					continue
				}
				snapshot := market.fair.Load()
				if snapshot == nil || !snapshot.Ready {
					if !subscription.resetSent {
						reason := "waiting_for_orderbook"
						if snapshot != nil && snapshot.ResetReason != "" {
							reason = snapshot.ResetReason
						}
						reset, _ := json.Marshal(map[string]string{
							"op": "reset", "profile": subscription.profile,
							"symbol":  subscription.symbol,
							"channel": "fairprice", "reason": reason,
						})
						if !writeWS(connection, websocket.TextMessage, reset) {
							s.wsWriteFailures.Add(1)
							return
						}
						subscription.sent = false
						subscription.resetSent = true
					}
					continue
				}
				changed := !subscription.sent ||
					snapshot.RingEpoch != subscription.lastEpoch ||
					snapshot.RingSequence != subscription.lastSequence
				heartbeat := subscription.sent &&
					now.Sub(subscription.lastWrite) >= s.fairHeartbeat
				if !changed && !heartbeat {
					continue
				}
				if !writeWS(connection, websocket.TextMessage, snapshot.JSON) {
					s.wsWriteFailures.Add(1)
					return
				}
				if snapshot.WallNS <= uint64(now.UnixNano()) {
					s.fairPublishLagNS.Store(now.UnixNano() - int64(snapshot.WallNS))
				}
				subscription.lastEpoch = snapshot.RingEpoch
				subscription.lastSequence = snapshot.RingSequence
				subscription.sent = true
				subscription.resetSent = false
				subscription.lastWrite = now
			}
		}
	}
}

func (s *Server) LogFairPriceStats(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stats := s.store.FairPriceStats()
			s.logger.Info(
				"fair price realtime statistics",
				"computed", stats.Computed,
				"invalid", stats.Invalid,
				"crossed", stats.Crossed,
				"uncross_failed", stats.UncrossFailed,
				"degraded", stats.Degraded,
				"subscribers", s.fairSubscribers.Load(),
				"ws_write_failures", s.wsWriteFailures.Load(),
				"publish_lag_ns", s.fairPublishLagNS.Load(),
			)
		}
	}
}

func copySubscriptions(
	result []*streamSubscription,
	mu *sync.RWMutex,
	subscriptions map[string]*streamSubscription,
) []*streamSubscription {
	mu.RLock()
	defer mu.RUnlock()
	for _, subscription := range subscriptions {
		result = append(result, subscription)
	}
	return result
}

func writeWS(connection *websocket.Conn, messageType int, payload []byte) bool {
	if err := connection.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return false
	}
	return connection.WriteMessage(messageType, payload) == nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
