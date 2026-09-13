package polymarket

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shopspring/decimal"
)

const (
	userStreamHeartbeat = 10 * time.Second
	userStreamReadWait  = 25 * time.Second
)

var (
	ErrAccountStreamUnavailable = errors.New("polymarket account stream unavailable")
	errUserStreamUnauthorized   = errors.New("polymarket user stream credentials rejected")
)

type AccountEvent struct {
	Type             string
	Order            *OpenOrder
	OpenOrders       []OpenOrder
	PortfolioChanged bool
}

type UserStreamManager struct {
	ctx     context.Context
	url     string
	service *Service
	logger  *slog.Logger

	mu       sync.Mutex
	sessions map[int64]*userStreamSession
}

type userStreamSession struct {
	accountID   int64
	token       string
	credentials Credentials
	manager     *UserStreamManager
	ctx         context.Context
	cancel      context.CancelFunc

	mu                   sync.Mutex
	nextID               uint64
	subscribers          map[uint64]chan AccountEvent
	authRefreshAttempted bool
}

func (s *Service) EnableAccountStreams(ctx context.Context, url string) {
	url = strings.TrimSpace(url)
	if url == "" {
		return
	}
	s.accountStreams = &UserStreamManager{
		ctx: ctx, url: url, service: s, logger: s.logger,
		sessions: make(map[int64]*userStreamSession),
	}
}

func (s *Service) SubscribeAccountEvents(
	ctx context.Context,
	token string,
	accountID int64,
) (<-chan AccountEvent, func(), error) {
	if strings.TrimSpace(token) == "" || accountID <= 0 {
		return nil, nil, ErrInvalidArgument
	}
	if s.accountStreams == nil {
		return nil, nil, ErrAccountStreamUnavailable
	}
	credentials, _, err := s.credentials.Get(ctx, token, accountID)
	if err != nil {
		return nil, nil, err
	}
	return s.accountStreams.subscribe(accountID, token, credentials)
}

func (m *UserStreamManager) subscribe(
	accountID int64,
	token string,
	credentials Credentials,
) (<-chan AccountEvent, func(), error) {
	m.mu.Lock()
	session := m.sessions[accountID]
	if session == nil {
		sessionCtx, cancel := context.WithCancel(m.ctx)
		session = &userStreamSession{
			accountID: accountID, token: token, credentials: credentials, manager: m,
			ctx: sessionCtx, cancel: cancel, subscribers: make(map[uint64]chan AccountEvent),
		}
		m.sessions[accountID] = session
		go session.run()
	}
	m.mu.Unlock()

	session.mu.Lock()
	session.nextID++
	id := session.nextID
	channel := make(chan AccountEvent, 1)
	session.subscribers[id] = channel
	session.mu.Unlock()
	session.publish(AccountEvent{
		Type: "snapshot", OpenOrders: m.service.cachedOpenOrders(accountID),
	})

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			session.mu.Lock()
			delete(session.subscribers, id)
			remaining := len(session.subscribers)
			session.mu.Unlock()
			if remaining == 0 {
				m.removeSession(accountID, session)
			}
		})
	}
	return channel, unsubscribe, nil
}

func (m *UserStreamManager) removeSession(accountID int64, session *userStreamSession) {
	m.mu.Lock()
	if m.sessions[accountID] == session {
		delete(m.sessions, accountID)
		session.cancel()
	}
	m.mu.Unlock()
}

func (s *userStreamSession) run() {
	backoff := 2 * time.Second
	for s.ctx.Err() == nil {
		started := time.Now()
		err := s.runOnce()
		if s.ctx.Err() != nil {
			return
		}
		if err != nil {
			s.manager.logger.Warn(
				"CLOB user stream disconnected",
				"account_id", s.accountID, "error", err,
			)
		}
		if errors.Is(err, errUserStreamUnauthorized) {
			refreshed, _, recoverErr := s.manager.service.refreshAndVerifyCredentials(
				s.ctx, s.token, s.accountID, true,
			)
			if recoverErr == nil {
				s.setCredentials(refreshed)
				s.authRefreshAttempted = false
				continue
			}
			if isCLOBUnauthorized(recoverErr) {
				return
			}
			s.manager.logger.Warn(
				"CLOB user stream credential recovery failed",
				"account_id", s.accountID, "error", recoverErr,
			)
		}
		if time.Since(started) > time.Minute {
			backoff = 2 * time.Second
			s.authRefreshAttempted = false
		}
		timer := time.NewTimer(backoff + time.Duration(rand.IntN(500))*time.Millisecond)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func (s *userStreamSession) runOnce() error {
	connection, _, err := websocket.DefaultDialer.DialContext(s.ctx, s.manager.url, nil)
	if err != nil {
		return err
	}
	defer connection.Close()
	connection.SetReadLimit(4 << 20)
	credentials := s.currentCredentials()
	if err := connection.WriteJSON(map[string]any{
		"auth": map[string]string{
			"apiKey": credentials.APIKey, "secret": credentials.APISecret,
			"passphrase": credentials.Passphrase,
		},
		"type": "user",
	}); err != nil {
		return err
	}
	go s.reconcile()

	writeErrors := make(chan error, 1)
	go s.heartbeat(connection, writeErrors)
	go func() {
		<-s.ctx.Done()
		_ = connection.Close()
	}()

	for {
		_ = connection.SetReadDeadline(time.Now().Add(userStreamReadWait))
		_, body, err := connection.ReadMessage()
		if err != nil {
			if s.ctx.Err() != nil {
				return nil
			}
			return err
		}
		if strings.EqualFold(strings.TrimSpace(string(body)), "PONG") {
			continue
		}
		select {
		case err := <-writeErrors:
			return err
		default:
		}
		if err := s.applyMessage(body); err != nil {
			return err
		}
	}
}

func (s *userStreamSession) heartbeat(
	connection *websocket.Conn,
	writeErrors chan<- error,
) {
	ticker := time.NewTicker(userStreamHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			_ = connection.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := connection.WriteMessage(websocket.TextMessage, []byte("PING")); err != nil {
				select {
				case writeErrors <- err:
				default:
				}
				_ = connection.Close()
				return
			}
		}
	}
}

func (s *userStreamSession) reconcile() {
	ordersCtx, cancelOrders := context.WithTimeout(s.ctx, 5*time.Second)
	orders, _, err := s.manager.service.ListOpenOrders(
		ordersCtx, s.token, s.accountID, true,
	)
	cancelOrders()
	if err != nil {
		s.manager.logger.Warn(
			"CLOB user stream reconcile deferred",
			"account_id", s.accountID, "error", err,
		)
		return
	}
	s.manager.service.enrichOpenOrders(orders)
	s.manager.service.replaceCachedOpenOrders(s.accountID, orders)
	s.publish(AccountEvent{Type: "snapshot", OpenOrders: orders})

	credentialCtx, cancelCredentials := context.WithTimeout(s.ctx, 2*time.Second)
	refreshed, _, getErr := s.manager.service.credentials.Get(
		credentialCtx, s.token, s.accountID,
	)
	cancelCredentials()
	if getErr == nil {
		s.setCredentials(refreshed)
	}
	var trades []Trade
	if getErr == nil {
		tradesCtx, cancelTrades := context.WithTimeout(s.ctx, 5*time.Second)
		trades, refreshed, err = callPrivateCLOB(
			tradesCtx, s.manager.service, s.token, s.accountID, refreshed,
			func(current Credentials) ([]Trade, error) {
				return s.manager.service.clob.ListTrades(
					tradesCtx, current, time.Now().Add(-5*time.Minute), time.Now(),
				)
			},
		)
		cancelTrades()
		if err == nil {
			s.setCredentials(refreshed)
		} else {
			s.manager.logger.Warn(
				"CLOB user stream trade backfill deferred",
				"account_id", s.accountID, "error", err,
			)
		}
	}
	reconcileCtx, cancelReconcile := context.WithTimeout(s.ctx, 2*time.Second)
	s.manager.service.reconcileSubmissionUnknown(
		reconcileCtx, s.accountID, orders, trades,
	)
	cancelReconcile()
	positionsCtx, cancelPositions := context.WithTimeout(s.ctx, 5*time.Second)
	_, _, _ = s.manager.service.ListPositions(
		positionsCtx, s.token, s.accountID, true,
	)
	cancelPositions()
}

func (s *userStreamSession) currentCredentials() Credentials {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credentials
}

func (s *userStreamSession) setCredentials(credentials Credentials) {
	s.mu.Lock()
	s.credentials = credentials
	s.mu.Unlock()
}

type userStreamMessage struct {
	EventType    string `json:"event_type"`
	Type         string `json:"type"`
	ID           string `json:"id"`
	Market       string `json:"market"`
	AssetID      string `json:"asset_id"`
	Side         string `json:"side"`
	OriginalSize string `json:"original_size"`
	SizeMatched  string `json:"size_matched"`
	Price        string `json:"price"`
	Outcome      string `json:"outcome"`
	OrderType    string `json:"order_type"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
}

func (s *userStreamSession) applyMessage(body []byte) error {
	var errorMessage struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &errorMessage) == nil {
		message := strings.ToLower(strings.TrimSpace(errorMessage.Error + " " + errorMessage.Message))
		if strings.Contains(message, "invalid api key") ||
			strings.Contains(message, "unauthorized") {
			return errUserStreamUnauthorized
		}
	}
	var message userStreamMessage
	if json.Unmarshal(body, &message) != nil {
		return nil
	}
	switch strings.ToLower(message.EventType) {
	case "trade":
		s.manager.service.invalidatePortfolio(s.accountID)
		s.publish(AccountEvent{
			Type: "portfolio", OpenOrders: s.manager.service.cachedOpenOrders(s.accountID),
			PortfolioChanged: true,
		})
	case "order":
		order, live := openOrderFromUserEvent(message)
		if order.ID == "" {
			return nil
		}
		s.manager.service.enrichOpenOrders([]OpenOrder{order})
		orders := s.manager.service.applyOpenOrderEvent(s.accountID, order, live)
		if status := persistedStreamOrderStatus(message, order); status != "" {
			if err := s.manager.service.repository.UpdateOrderFromStream(
				s.ctx, s.accountID, order.ID, status, order.MatchedSize,
			); err != nil {
				s.manager.logger.Warn(
					"persist CLOB user order event failed",
					"account_id", s.accountID, "order_id", order.ID, "error", err,
				)
			}
		}
		s.publish(AccountEvent{Type: "order", Order: &order, OpenOrders: orders})
	}
	return nil
}

func persistedStreamOrderStatus(message userStreamMessage, order OpenOrder) string {
	if strings.EqualFold(message.Type, "CANCELLATION") ||
		strings.EqualFold(message.Status, "CANCELED") {
		return "canceled"
	}
	if strings.EqualFold(message.Status, "MATCHED") ||
		strings.EqualFold(message.Status, "ORDER_STATUS_MATCHED") {
		return "filled"
	}
	matched, err := decimal.NewFromString(order.MatchedSize)
	if err == nil && matched.GreaterThan(decimal.Zero) {
		return "partially_filled"
	}
	if strings.EqualFold(message.Status, "LIVE") ||
		strings.EqualFold(message.Status, "ORDER_STATUS_LIVE") {
		return "open"
	}
	return ""
}

func openOrderFromUserEvent(message userStreamMessage) (OpenOrder, bool) {
	original, originalErr := decimal.NewFromString(message.OriginalSize)
	matched, matchedErr := decimal.NewFromString(message.SizeMatched)
	remaining := decimal.Zero
	if originalErr == nil && matchedErr == nil {
		original = original.Shift(-6)
		matched = matched.Shift(-6)
		remaining = original.Sub(matched)
	}
	status := strings.ToUpper(strings.TrimSpace(message.Status))
	eventType := strings.ToUpper(strings.TrimSpace(message.Type))
	live := (status == "LIVE" || status == "ORDER_STATUS_LIVE") &&
		eventType != "CANCELLATION" && remaining.GreaterThan(decimal.Zero)
	return OpenOrder{
		ID: message.ID, ConditionID: message.Market, TokenID: message.AssetID,
		Outcome: strings.ToLower(message.Outcome), Side: strings.ToLower(message.Side),
		Price: message.Price, OriginalSize: original.String(),
		MatchedSize: matched.String(), RemainingSize: remaining.String(),
		Status: strings.ToLower(message.Status), OrderType: strings.ToUpper(message.OrderType),
		CreatedAt: parseUserEventTime(message.CreatedAt),
	}, live
}

func parseUserEventTime(value string) time.Time {
	seconds, err := decimal.NewFromString(strings.TrimSpace(value))
	if err != nil {
		return time.Now().UTC()
	}
	return time.Unix(seconds.IntPart(), 0).UTC()
}

func (s *userStreamSession) publish(event AccountEvent) {
	event.OpenOrders = append([]OpenOrder(nil), event.OpenOrders...)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, channel := range s.subscribers {
		select {
		case channel <- event:
		default:
			select {
			case <-channel:
			default:
			}
			channel <- event
		}
	}
}
