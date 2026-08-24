package marketdata

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// Connection is one public venue BBO stream. Close must unblock Read.
type Connection interface {
	Read() ([]byte, error)
	Close() error
}

// Connector creates an already-subscribed connection for a key.
type Connector interface {
	Connect(context.Context, Key) (Connection, error)
}

type Parser func(Key, []byte, time.Time) (BBO, bool, error)

type Options struct {
	Connector        Connector
	Parsers          map[string]Parser
	StaleAfter       time.Duration
	ReconnectInitial time.Duration
	ReconnectMax     time.Duration
	Now              func() time.Time
	Logger           *slog.Logger
}

type Manager struct {
	connector        Connector
	parsers          map[string]Parser
	staleAfter       time.Duration
	reconnectInitial time.Duration
	reconnectMax     time.Duration
	now              func() time.Time
	logger           *slog.Logger

	mu         sync.Mutex
	streams    map[Key]*stream
	closed     bool
	connects   atomic.Uint64
	reconnects atomic.Uint64
	updates    atomic.Uint64
	staleReads atomic.Uint64
}

type stream struct {
	key          Key
	ctx          context.Context
	cancel       context.CancelFunc
	refs         int
	nextID       uint64
	subs         map[uint64]chan BBO
	latest       BBO
	hasData      bool
	lastErrorLog time.Time
}

type Subscription struct {
	manager *Manager
	key     Key
	id      uint64
	updates <-chan BBO
	done    chan struct{}
	once    sync.Once
}

func New(options Options) (*Manager, error) {
	if options.Connector == nil {
		options.Connector = NewWebSocketConnector()
	}
	if options.Parsers == nil {
		options.Parsers = DefaultParsers()
	}
	if options.StaleAfter <= 0 {
		options.StaleAfter = 15 * time.Second
	}
	if options.ReconnectInitial <= 0 {
		options.ReconnectInitial = 250 * time.Millisecond
	}
	if options.ReconnectMax <= 0 {
		options.ReconnectMax = 30 * time.Second
	}
	if options.ReconnectMax < options.ReconnectInitial {
		return nil, fmt.Errorf("reconnect maximum must not be less than initial delay")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &Manager{
		connector:        options.Connector,
		parsers:          options.Parsers,
		staleAfter:       options.StaleAfter,
		reconnectInitial: options.ReconnectInitial,
		reconnectMax:     options.ReconnectMax,
		now:              options.Now,
		logger:           options.Logger,
		streams:          make(map[Key]*stream),
	}, nil
}

// Subscribe adds a reference to a shared key stream. Updates has capacity one and
// always converges to the latest value; publishers never wait for consumers.
func (m *Manager) Subscribe(ctx context.Context, key Key) (*Subscription, error) {
	normalized, err := NewKey(key.Venue, key.Product, key.Symbol)
	if err != nil {
		return nil, err
	}
	if _, ok := m.parsers[normalized.Venue]; !ok {
		return nil, fmt.Errorf("%w: no parser for venue %q", ErrUnsupportedKey, normalized.Venue)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	s := m.streams[normalized]
	if s == nil {
		streamCtx, cancel := context.WithCancel(context.Background())
		s = &stream{
			key: normalized, ctx: streamCtx, cancel: cancel,
			subs: make(map[uint64]chan BBO),
		}
		m.streams[normalized] = s
		go m.run(s)
	}
	s.nextID++
	id := s.nextID
	updates := make(chan BBO, 1)
	s.subs[id] = updates
	s.refs++
	if s.hasData {
		updates <- s.latest
	}
	m.mu.Unlock()

	subscription := &Subscription{
		manager: m, key: normalized, id: id, updates: updates, done: make(chan struct{}),
	}
	if ctx != nil && ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = subscription.Close()
			case <-subscription.done:
			case <-s.ctx.Done():
			}
		}()
	}
	return subscription, nil
}

func (s *Subscription) Updates() <-chan BBO {
	return s.updates
}

func (s *Subscription) Latest() (BBO, error) {
	if s.manager == nil {
		return BBO{}, ErrClosed
	}
	return s.manager.Latest(s.key)
}

func (s *Subscription) Close() error {
	if s.manager == nil {
		return nil
	}
	s.once.Do(func() {
		close(s.done)
		s.manager.unsubscribe(s.key, s.id)
	})
	return nil
}

func (m *Manager) unsubscribe(key Key, id uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.streams[key]
	if s == nil {
		return
	}
	updates, ok := s.subs[id]
	if !ok {
		return
	}
	delete(s.subs, id)
	s.refs--
	close(updates)
	if s.refs == 0 {
		delete(m.streams, key)
		s.cancel()
	}
}

func (m *Manager) Latest(key Key) (BBO, error) {
	normalized, err := NewKey(key.Venue, key.Product, key.Symbol)
	if err != nil {
		return BBO{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.streams[normalized]
	if s == nil || !s.hasData {
		return BBO{}, ErrNoValue
	}
	if s.latest.Stale(m.now(), m.staleAfter) {
		m.staleReads.Add(1)
		return s.latest, ErrStale
	}
	return s.latest, nil
}

func (m *Manager) IsStale(key Key) bool {
	_, err := m.Latest(key)
	return errors.Is(err, ErrNoValue) || errors.Is(err, ErrStale)
}

func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	streams := make([]*stream, 0, len(m.streams))
	for key, s := range m.streams {
		delete(m.streams, key)
		streams = append(streams, s)
		for id, updates := range s.subs {
			delete(s.subs, id)
			close(updates)
		}
	}
	m.mu.Unlock()
	for _, s := range streams {
		s.cancel()
	}
	return nil
}

func (m *Manager) run(s *stream) {
	backoff := m.reconnectInitial
	for s.ctx.Err() == nil {
		connection, err := m.connector.Connect(s.ctx, s.key)
		if err == nil {
			m.connects.Add(1)
			backoff = m.reconnectInitial
			err = m.consume(s, connection)
			m.reconnects.Add(1)
		}
		if s.ctx.Err() != nil {
			return
		}
		m.logStreamError(s, err, backoff)
		jitterMax := max(time.Millisecond, backoff/4)
		jitter := time.Duration(rand.Int64N(int64(jitterMax)))
		timer := time.NewTimer(backoff + jitter)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(m.reconnectMax, backoff*2)
	}
}

func (m *Manager) logStreamError(s *stream, err error, retryAfter time.Duration) {
	if err == nil {
		return
	}
	now := m.now()
	if !s.lastErrorLog.IsZero() && now.Sub(s.lastErrorLog) < 10*time.Second {
		return
	}
	s.lastErrorLog = now
	m.logger.Warn("public BBO stream reconnecting",
		"venue", s.key.Venue,
		"product", s.key.Product,
		"symbol", s.key.Symbol,
		"retry_after", retryAfter,
		"error", err,
	)
}

func (m *Manager) consume(s *stream, connection Connection) error {
	closed := make(chan struct{})
	go func() {
		select {
		case <-s.ctx.Done():
			_ = connection.Close()
		case <-closed:
		}
	}()
	defer close(closed)
	defer connection.Close()

	parser := m.parsers[s.key.Venue]
	for {
		payload, err := connection.Read()
		if err != nil {
			return fmt.Errorf("read %s BBO stream: %w", s.key.Venue, err)
		}
		bbo, matched, err := parser(s.key, payload, m.now())
		if err != nil {
			return fmt.Errorf("parse %s BBO: %w", s.key.Venue, err)
		}
		if !matched {
			continue
		}
		m.publish(s, bbo)
	}
}

func (m *Manager) publish(s *stream, bbo BBO) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.streams[s.key]
	if current != s {
		return
	}
	s.latest, s.hasData = bbo, true
	m.updates.Add(1)
	for _, updates := range s.subs {
		select {
		case updates <- bbo:
		default:
			select {
			case <-updates:
			default:
			}
			select {
			case updates <- bbo:
			default:
			}
		}
	}
}

type Stats struct {
	ActiveStreams uint64
	References    uint64
	Connects      uint64
	Reconnects    uint64
	Updates       uint64
	StaleReads    uint64
}

func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	var references uint64
	for _, stream := range m.streams {
		references += uint64(stream.refs)
	}
	return Stats{
		ActiveStreams: uint64(len(m.streams)), References: references,
		Connects: m.connects.Load(), Reconnects: m.reconnects.Load(),
		Updates: m.updates.Load(), StaleReads: m.staleReads.Load(),
	}
}
