package orderstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var ErrCredentialMismatch = errors.New("credentials differ from active order stream session")

type Options struct {
	Connector        Connector
	Parsers          map[string]Parser
	URLs             URLs
	HTTPClient       *http.Client
	Heartbeat        time.Duration
	StaleAfter       time.Duration
	ListenKeyRefresh time.Duration
	ReconnectInitial time.Duration
	ReconnectMax     time.Duration
	IdleTimeout      time.Duration
	Now              func() time.Time
	Jitter           func(time.Duration) time.Duration
	Logger           *slog.Logger
}

type Manager struct {
	connector        Connector
	parsers          map[string]Parser
	staleAfter       time.Duration
	reconnectInitial time.Duration
	reconnectMax     time.Duration
	idleTimeout      time.Duration
	now              func() time.Time
	jitter           func(time.Duration) time.Duration
	logger           *slog.Logger

	mu       sync.Mutex
	sessions map[Key]*session
	closed   bool
	wg       sync.WaitGroup

	connects     atomic.Uint64
	reconnects   atomic.Uint64
	updates      atomic.Uint64
	disconnects  atomic.Uint64
	dropped      atomic.Uint64
	authFailures atomic.Uint64

	hbMu                 sync.Mutex
	heartbeatWriteFailed map[string]uint64
}

type session struct {
	key         Key
	credentials Credentials
	ctx         context.Context
	cancel      context.CancelFunc
	refs        int
	nextID      uint64
	subs        map[uint64]chan Update
	watchers    map[string]map[uint64]chan Update
	orderState  map[string]Update
	tradeIDs    map[string]struct{}
	healthy     bool
	lastEvent   time.Time
	generation  uint64
	idleTimer   *time.Timer
}

type Subscription struct {
	manager *Manager
	session *session
	id      uint64
	updates <-chan Update
	done    chan struct{}
	once    sync.Once
}

type OrderWatch struct {
	manager       *Manager
	session       *session
	clientOrderID string
	id            uint64
	updates       <-chan Update
	done          chan struct{}
	once          sync.Once
}

func New(options Options) (*Manager, error) {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.StaleAfter <= 0 {
		options.StaleAfter = 60 * time.Second
	}
	if options.ReconnectInitial <= 0 {
		options.ReconnectInitial = 250 * time.Millisecond
	}
	if options.ReconnectMax <= 0 {
		options.ReconnectMax = 30 * time.Second
	}
	if options.ReconnectMax < options.ReconnectInitial {
		return nil, errors.New("reconnect maximum must not be less than initial delay")
	}
	if options.IdleTimeout == 0 {
		options.IdleTimeout = 5 * time.Minute
	}
	if options.Jitter == nil {
		options.Jitter = func(delay time.Duration) time.Duration {
			limit := max(time.Millisecond, delay/4)
			return time.Duration(rand.Int64N(int64(limit)))
		}
	}
	if options.Parsers == nil {
		options.Parsers = DefaultParsers()
	}
	if options.Connector == nil {
		options.Connector = NewWebSocketConnector(ConnectorOptions{
			URLs: options.URLs, HTTPClient: options.HTTPClient, Heartbeat: options.Heartbeat,
			StaleAfter: options.StaleAfter, ListenKeyRefresh: options.ListenKeyRefresh,
			Now: options.Now,
		})
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &Manager{
		connector: options.Connector, parsers: options.Parsers,
		staleAfter: options.StaleAfter, reconnectInitial: options.ReconnectInitial,
		reconnectMax: options.ReconnectMax, idleTimeout: options.IdleTimeout,
		now: options.Now, jitter: options.Jitter, logger: options.Logger,
		sessions: make(map[Key]*session),
	}, nil
}

func (m *Manager) Subscribe(
	ctx context.Context,
	key Key,
	credentials Credentials,
) (*Subscription, error) {
	normalized, err := key.normalized()
	if err != nil {
		return nil, err
	}
	if err := credentials.validate(normalized.Venue); err != nil {
		return nil, err
	}
	if (normalized.Venue == VenueOKX || normalized.Venue == VenueBitget) &&
		strings.TrimSpace(credentials.Passphrase) == "" {
		return nil, fmt.Errorf("%s order stream credentials require passphrase", normalized.Venue)
	}
	if _, ok := m.parsers[normalized.Venue]; !ok {
		return nil, fmt.Errorf("%w: no parser for venue %q", ErrUnsupportedKey, normalized.Venue)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	current := m.sessions[normalized]
	if current != nil && !credentialsEqual(current.credentials, credentials) && current.refs == 0 {
		delete(m.sessions, normalized)
		if current.idleTimer != nil {
			current.idleTimer.Stop()
		}
		current.cancel()
		m.closeWatchersLocked(current)
		current = nil
	}
	if current == nil {
		sessionCtx, cancel := context.WithCancel(context.Background())
		current = &session{
			key: normalized, credentials: credentials, ctx: sessionCtx, cancel: cancel,
			subs:       make(map[uint64]chan Update),
			watchers:   make(map[string]map[uint64]chan Update),
			orderState: make(map[string]Update),
			tradeIDs:   make(map[string]struct{}),
		}
		m.sessions[normalized] = current
		m.wg.Add(1)
		go m.run(current)
	} else if !credentialsEqual(current.credentials, credentials) {
		m.mu.Unlock()
		return nil, ErrCredentialMismatch
	}
	if current.idleTimer != nil {
		current.idleTimer.Stop()
		current.idleTimer = nil
	}
	current.nextID++
	id := current.nextID
	updates := make(chan Update, 64)
	current.subs[id] = updates
	current.refs++
	m.mu.Unlock()

	subscription := &Subscription{
		manager: m, session: current, id: id, updates: updates, done: make(chan struct{}),
	}
	if ctx != nil && ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = subscription.Close()
			case <-subscription.done:
			case <-current.ctx.Done():
			}
		}()
	}
	return subscription, nil
}

func credentialsEqual(left, right Credentials) bool {
	return left.APIKey == right.APIKey &&
		left.Secret == right.Secret &&
		left.Passphrase == right.Passphrase &&
		left.CredentialKind == right.CredentialKind &&
		left.SigningAddress == right.SigningAddress &&
		left.VaultAddress == right.VaultAddress &&
		optionalInt64Equal(left.AccountIndex, right.AccountIndex) &&
		optionalInt32Equal(left.APIKeyIndex, right.APIKeyIndex) &&
		left.AuthToken == right.AuthToken
}

func optionalInt64Equal(left, right *int64) bool {
	return (left == nil && right == nil) ||
		(left != nil && right != nil && *left == *right)
}

func optionalInt32Equal(left, right *int32) bool {
	return (left == nil && right == nil) ||
		(left != nil && right != nil && *left == *right)
}

func (s *Subscription) Updates() <-chan Update {
	return s.updates
}

func (s *Subscription) Healthy() bool {
	return s.manager != nil && s.manager.sessionHealthy(s.session)
}

func (s *Subscription) Generation() uint64 {
	if s.manager == nil {
		return 0
	}
	s.manager.mu.Lock()
	defer s.manager.mu.Unlock()
	if s.manager.sessions[s.session.key] != s.session {
		return 0
	}
	return s.session.generation
}

func (s *Subscription) WatchOrder(ctx context.Context, clientOrderID string) (*OrderWatch, error) {
	if s.manager == nil {
		return nil, ErrClosed
	}
	return s.manager.watchOrder(ctx, s.session, clientOrderID)
}

func (s *Subscription) Close() error {
	if s.manager == nil {
		return nil
	}
	s.once.Do(func() {
		close(s.done)
		s.manager.unsubscribe(s.session, s.id)
	})
	return nil
}

func (m *Manager) WatchOrder(
	ctx context.Context,
	key Key,
	clientOrderID string,
) (*OrderWatch, error) {
	normalized, err := key.normalized()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	current := m.sessions[normalized]
	m.mu.Unlock()
	if current == nil {
		return nil, ErrNoSession
	}
	return m.watchOrder(ctx, current, clientOrderID)
}

func (m *Manager) BindClientVenueIDs(key Key, clientOrderID, venueOrderID string) {
	clientOrderID = strings.TrimSpace(clientOrderID)
	venueOrderID = strings.TrimSpace(venueOrderID)
	if clientOrderID == "" || venueOrderID == "" {
		return
	}
	normalized, err := key.normalized()
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.sessions[normalized]
	if current == nil || m.closed {
		return
	}
	client := current.orderState["client:"+clientOrderID]
	if client.ClientOrderID == "" {
		client.ClientOrderID = clientOrderID
	}
	client.VenueOrderID = venueOrderID
	current.orderState["client:"+clientOrderID] = client
	venue := current.orderState["venue:"+venueOrderID]
	if venue.VenueOrderID == "" {
		venue.VenueOrderID = venueOrderID
	}
	venue.ClientOrderID = clientOrderID
	current.orderState["venue:"+venueOrderID] = venue
}

func (m *Manager) watchOrder(
	ctx context.Context,
	current *session,
	clientOrderID string,
) (*OrderWatch, error) {
	if clientOrderID == "" {
		return nil, errors.New("client order ID is required")
	}
	m.mu.Lock()
	if m.closed || m.sessions[current.key] != current {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	current.nextID++
	id := current.nextID
	channel := make(chan Update, 16)
	if current.watchers[clientOrderID] == nil {
		current.watchers[clientOrderID] = make(map[uint64]chan Update)
	}
	current.watchers[clientOrderID][id] = channel
	m.mu.Unlock()
	watch := &OrderWatch{
		manager: m, session: current, clientOrderID: clientOrderID,
		id: id, updates: channel, done: make(chan struct{}),
	}
	if ctx != nil && ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = watch.Close()
			case <-watch.done:
			case <-current.ctx.Done():
			}
		}()
	}
	return watch, nil
}

func (w *OrderWatch) Updates() <-chan Update {
	return w.updates
}

func (w *OrderWatch) Close() error {
	if w.manager == nil {
		return nil
	}
	w.once.Do(func() {
		close(w.done)
		w.manager.closeWatch(w.session, w.clientOrderID, w.id)
	})
	return nil
}

func (m *Manager) sessionHealthy(current *session) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[current.key] == current && current.healthy
}

func (m *Manager) Healthy(key Key) bool {
	normalized, err := key.normalized()
	if err != nil {
		return false
	}
	m.mu.Lock()
	current := m.sessions[normalized]
	m.mu.Unlock()
	return current != nil && m.sessionHealthy(current)
}

func (m *Manager) unsubscribe(current *session, id uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[current.key] != current {
		return
	}
	channel, ok := current.subs[id]
	if !ok {
		return
	}
	delete(current.subs, id)
	close(channel)
	current.refs--
	if current.refs == 0 {
		m.closeWatchersLocked(current)
		if m.idleTimeout <= 0 {
			delete(m.sessions, current.key)
			current.cancel()
		} else {
			current.idleTimer = time.AfterFunc(m.idleTimeout, func() {
				m.expireIdle(current)
			})
		}
	}
}

func (m *Manager) expireIdle(current *session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[current.key] != current || current.refs != 0 {
		return
	}
	delete(m.sessions, current.key)
	current.idleTimer = nil
	current.cancel()
	m.closeWatchersLocked(current)
}

func (m *Manager) closeWatch(current *session, clientOrderID string, id uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	watchers := current.watchers[clientOrderID]
	channel, ok := watchers[id]
	if !ok {
		return
	}
	delete(watchers, id)
	close(channel)
	if len(watchers) == 0 {
		delete(current.watchers, clientOrderID)
	}
}

func (m *Manager) run(current *session) {
	defer m.wg.Done()
	backoff := m.reconnectInitial
	attempted := false
	var lastDelay time.Duration
	for current.ctx.Err() == nil {
		connection, err := m.connector.Connect(current.ctx, current.key, current.credentials)
		if err != nil {
			reason := classifyConnectError(err)
			if strings.Contains(strings.ToLower(err.Error()), "auth") {
				m.authFailures.Add(1)
			}
			if current.ctx.Err() != nil {
				return
			}
			delay := backoff + m.jitter(backoff)
			m.logConnectFailed(current, reason, delay, err)
			attempted = true
			m.disconnects.Add(1)
			m.setConnectionState(current, false, err)
			if !m.waitReconnect(current, delay) {
				return
			}
			lastDelay = delay
			backoff = min(m.reconnectMax, backoff*2)
			continue
		}
		m.connects.Add(1)
		if attempted {
			m.reconnects.Add(1)
		}
		backoff = m.reconnectInitial
		m.setConnectionState(current, true, nil)
		if attempted {
			m.logReconnected(current, lastDelay)
		}
		connectedAt := m.now()
		err = m.consume(current, connection)
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "auth") {
			m.authFailures.Add(1)
		}
		if current.ctx.Err() != nil {
			return
		}
		reason := classifyDisconnect(err)
		delay := backoff + m.jitter(backoff)
		m.logDisconnected(current, reason, m.now().Sub(connectedAt), delay, err)
		attempted = true
		m.disconnects.Add(1)
		m.setConnectionState(current, false, err)
		if !m.waitReconnect(current, delay) {
			return
		}
		lastDelay = delay
		backoff = min(m.reconnectMax, backoff*2)
	}
}

func (m *Manager) waitReconnect(current *session, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	select {
	case <-current.ctx.Done():
		timer.Stop()
		return false
	case <-timer.C:
		return true
	}
}

func (m *Manager) logConnectFailed(
	current *session,
	reason disconnectReason,
	delay time.Duration,
	err error,
) {
	attrs := m.streamLogAttrs(current, reason, 0, delay, err)
	m.logger.Warn("order_stream_connect_failed", attrs...)
}

func (m *Manager) logDisconnected(
	current *session,
	reason disconnectReason,
	connected time.Duration,
	delay time.Duration,
	err error,
) {
	if reason == reasonHeartbeatWriteFailed && current != nil {
		m.addHeartbeatWriteFailed(current.key.Venue)
	}
	attrs := m.streamLogAttrs(current, reason, connected, delay, err)
	switch reason {
	case reasonShutdown:
		return
	case reasonPlannedRotation:
		m.logger.Info("order_stream_disconnected", attrs...)
	default:
		m.logger.Warn("order_stream_disconnected", attrs...)
	}
}

func (m *Manager) logReconnected(current *session, delay time.Duration) {
	attrs := m.streamLogAttrs(current, "", 0, delay, nil)
	m.logger.Info("order_stream_reconnected", attrs...)
}

func (m *Manager) streamLogAttrs(
	current *session,
	reason disconnectReason,
	connected time.Duration,
	delay time.Duration,
	err error,
) []any {
	m.mu.Lock()
	venue := current.key.Venue
	account := current.key.Account
	product := current.key.Product
	generation := current.generation
	m.mu.Unlock()
	attrs := []any{
		"venue", venue,
		"account", account,
		"product", product,
		"generation", generation,
		"connected_duration_ms", connected.Milliseconds(),
		"reconnect_delay_ms", delay.Milliseconds(),
	}
	if reason != "" {
		attrs = append(attrs, "reason", string(reason))
	}
	var de *disconnectError
	if errors.As(err, &de) {
		if de.HeartbeatType != "" {
			attrs = append(attrs, "heartbeat_type", de.HeartbeatType)
		}
		if !de.WriteDeadline.IsZero() {
			attrs = append(attrs, "write_deadline", de.WriteDeadline.UTC().Format(time.RFC3339Nano))
		}
	}
	if err != nil {
		attrs = append(attrs, "error", sanitizeDisconnectError(err))
	}
	return attrs
}

func (m *Manager) addHeartbeatWriteFailed(venue string) {
	if venue == "" {
		return
	}
	m.hbMu.Lock()
	defer m.hbMu.Unlock()
	if m.heartbeatWriteFailed == nil {
		m.heartbeatWriteFailed = map[string]uint64{}
	}
	m.heartbeatWriteFailed[venue]++
}

func (m *Manager) consume(current *session, connection Connection) error {
	closed := make(chan struct{})
	go func() {
		select {
		case <-current.ctx.Done():
			_ = connection.Close()
		case <-closed:
		}
	}()
	defer close(closed)
	defer connection.Close()

	parser := m.parsers[current.key.Venue]
	for {
		payload, err := connection.Read()
		if err != nil {
			return fmt.Errorf("read %s private stream: %w", current.key.Venue, err)
		}
		if err := privateStreamControlError(payload); err != nil {
			return fmt.Errorf("%s private stream control: %w", current.key.Venue, err)
		}
		updates, matched, err := parser(current.key, payload, m.now())
		if err != nil {
			return fmt.Errorf("parse %s private stream: %w", current.key.Venue, err)
		}
		m.touch(current)
		if !matched {
			continue
		}
		for _, update := range updates {
			if current.key.Venue == VenueHyperliquid &&
				update.Type == UpdateTrade &&
				!hyperliquidFillFieldsValid(
					update.LastPrice, update.LastFilled, update.VenueOrderID, update.TradeID,
				) {
				m.logger.Warn(
					"hyperliquid_fill_invalid_price",
					slog.String("venue_order_id", update.VenueOrderID),
					slog.String("trade_id", update.TradeID),
				)
				continue
			}
			m.publish(current, update)
		}
	}
}

func (m *Manager) touch(current *session) {
	m.mu.Lock()
	if m.sessions[current.key] == current {
		current.lastEvent = m.now()
	}
	m.mu.Unlock()
}

func (m *Manager) setConnectionState(current *session, healthy bool, cause error) {
	update := baseUpdate(current.key, m.now())
	update.Type = UpdateConnection
	update.Status = StatusConnected
	if !healthy {
		update.Status = StatusDisconnected
		if cause != nil {
			update.Error = cause.Error()
		}
	}
	m.mu.Lock()
	if m.sessions[current.key] != current {
		m.mu.Unlock()
		return
	}
	current.healthy = healthy
	if healthy {
		current.generation++
	}
	current.lastEvent = m.now()
	m.publishLocked(current, update)
	m.mu.Unlock()
}

func (m *Manager) publish(current *session, update Update) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[current.key] != current {
		return
	}
	if !reduceDEXUpdateLocked(current, &update, m.logger) {
		return
	}
	current.lastEvent = m.now()
	m.publishLocked(current, update)
}

func reduceDEXUpdateLocked(current *session, update *Update, logger *slog.Logger) bool {
	if !isDEXVenue(current.key.Venue) || update.Type == UpdateConnection {
		return true
	}
	if update.Type == UpdateTrade && update.TradeID != "" {
		tradeKey := update.VenueOrderID + "\x00" + update.TradeID
		if _, exists := current.tradeIDs[tradeKey]; exists {
			return false
		}
		current.tradeIDs[tradeKey] = struct{}{}
	}
	previous, found := dexPreviousUpdate(current, *update)
	if found && update.ClientOrderID == "" {
		update.ClientOrderID = previous.ClientOrderID
	}
	if found && update.VenueOrderID == "" {
		update.VenueOrderID = previous.VenueOrderID
	}
	if found &&
		current.key.Venue == VenueHyperliquid &&
		update.Type == UpdateTrade &&
		update.ClientOrderID != "" &&
		update.VenueOrderID != "" {
		clientPrev, hasClient := current.orderState["client:"+update.ClientOrderID]
		venuePrev, hasVenue := current.orderState["venue:"+update.VenueOrderID]
		if hasClient && hasVenue &&
			clientPrev.VenueOrderID != "" &&
			venuePrev.VenueOrderID != "" &&
			clientPrev.VenueOrderID != venuePrev.VenueOrderID {
			if logger != nil {
				logger.Warn(
					"hyperliquid_fill_venue_order_mismatch",
					slog.String("client_order_id", update.ClientOrderID),
					slog.String("fill_venue_order_id", update.VenueOrderID),
					slog.String("client_venue_order_id", clientPrev.VenueOrderID),
					slog.String("cached_venue_order_id", venuePrev.VenueOrderID),
				)
			}
			return false
		}
	}
	if current.key.Venue == VenueHyperliquid &&
		update.Type == UpdateTrade &&
		update.ClientOrderID == "" {
		if logger != nil {
			logger.Warn(
				"hyperliquid_fill_order_not_found",
				slog.String("venue_order_id", update.VenueOrderID),
				slog.String("trade_id", update.TradeID),
			)
		}
		return false
	}
	if found {
		previousFilled := parseDecimal(previous.CumulativeFilled)
		currentFilled := parseDecimal(update.CumulativeFilled)
		if update.Type == UpdateOrder && previous.CumulativeFilled != "" &&
			(update.CumulativeFilled == "" || currentFilled.LessThan(previousFilled)) {
			update.CumulativeFilled = previous.CumulativeFilled
			currentFilled = previousFilled
		}
		if isTerminalStreamStatus(previous.Status) {
			if !isTerminalStreamStatus(update.Status) ||
				terminalStreamRank(update.Status) <= terminalStreamRank(previous.Status) {
				if update.Type != UpdateTrade ||
					update.CumulativeFilled == "" ||
					currentFilled.LessThanOrEqual(previousFilled) {
					return false
				}
			}
		}
		if update.Sequence > 0 && previous.Sequence > 0 &&
			update.Sequence < previous.Sequence && !isTerminalStreamStatus(update.Status) {
			return false
		}
		if sameDEXUpdate(previous, *update) {
			return false
		}
	}
	if update.ClientOrderID != "" {
		current.orderState["client:"+update.ClientOrderID] = *update
	}
	if update.VenueOrderID != "" {
		current.orderState["venue:"+update.VenueOrderID] = *update
	}
	return true
}

func dexPreviousUpdate(current *session, update Update) (Update, bool) {
	if update.ClientOrderID != "" {
		if previous, ok := current.orderState["client:"+update.ClientOrderID]; ok {
			return previous, true
		}
	}
	if update.VenueOrderID != "" {
		previous, ok := current.orderState["venue:"+update.VenueOrderID]
		return previous, ok
	}
	return Update{}, false
}

func isDEXVenue(venue string) bool {
	return venue == VenueAster || venue == VenueHyperliquid || venue == VenueLighter
}

func isTerminalStreamStatus(status Status) bool {
	switch status {
	case StatusFilled, StatusCanceled, StatusRejected, StatusExpired:
		return true
	default:
		return false
	}
}

func terminalStreamRank(status Status) int {
	if status == StatusFilled {
		return 2
	}
	if isTerminalStreamStatus(status) {
		return 1
	}
	return 0
}

func sameDEXUpdate(left, right Update) bool {
	return left.Type == right.Type &&
		left.ClientOrderID == right.ClientOrderID &&
		left.VenueOrderID == right.VenueOrderID &&
		left.Status == right.Status &&
		left.CumulativeFilled == right.CumulativeFilled &&
		left.LastFilled == right.LastFilled &&
		left.TradeID == right.TradeID &&
		left.ErrorCode == right.ErrorCode &&
		left.ErrorMessage == right.ErrorMessage
}

func (m *Manager) publishLocked(current *session, update Update) {
	m.updates.Add(1)
	for _, channel := range current.subs {
		m.send(channel, update)
	}
	if update.ClientOrderID != "" {
		for _, channel := range current.watchers[update.ClientOrderID] {
			m.send(channel, update)
		}
	}
}

func (m *Manager) send(channel chan Update, update Update) {
	select {
	case channel <- update:
	default:
		m.dropped.Add(1)
		select {
		case <-channel:
		default:
		}
		select {
		case channel <- update:
		default:
		}
	}
}

func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	sessions := make([]*session, 0, len(m.sessions))
	for key, current := range m.sessions {
		delete(m.sessions, key)
		sessions = append(sessions, current)
		for id, channel := range current.subs {
			delete(current.subs, id)
			close(channel)
		}
		if current.idleTimer != nil {
			current.idleTimer.Stop()
			current.idleTimer = nil
		}
		m.closeWatchersLocked(current)
	}
	m.mu.Unlock()
	for _, current := range sessions {
		current.cancel()
	}
	m.wg.Wait()
	return nil
}

func (m *Manager) closeWatchersLocked(current *session) {
	for clientOrderID, watchers := range current.watchers {
		for id, channel := range watchers {
			delete(watchers, id)
			close(channel)
		}
		delete(current.watchers, clientOrderID)
	}
}

type Stats struct {
	ActiveSessions       uint64
	HealthySessions      uint64
	References           uint64
	Watchers             uint64
	Connects             uint64
	Reconnects           uint64
	Updates              uint64
	Disconnects          uint64
	Dropped              uint64
	AuthFailures         uint64
	HeartbeatWriteFailed map[string]uint64
}

func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	var references, watchers, healthy uint64
	for _, current := range m.sessions {
		references += uint64(current.refs)
		if current.healthy {
			healthy++
		}
		for _, byID := range current.watchers {
			watchers += uint64(len(byID))
		}
	}
	m.hbMu.Lock()
	heartbeatWriteFailed := copyUint64Map(m.heartbeatWriteFailed)
	m.hbMu.Unlock()
	return Stats{
		ActiveSessions: uint64(len(m.sessions)), HealthySessions: healthy,
		References: references, Watchers: watchers,
		Connects: m.connects.Load(), Reconnects: m.reconnects.Load(),
		Updates: m.updates.Load(), Disconnects: m.disconnects.Load(), Dropped: m.dropped.Load(),
		AuthFailures: m.authFailures.Load(), HeartbeatWriteFailed: heartbeatWriteFailed,
	}
}

func copyUint64Map(values map[string]uint64) map[string]uint64 {
	if len(values) == 0 {
		return nil
	}
	copied := make(map[string]uint64, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}
