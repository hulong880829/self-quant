package aggdata

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type GatewayClient struct {
	url    string
	token  string
	store  *Store
	logger *slog.Logger
	dialer websocket.Dialer
}

func NewGatewayClient(url, token string, store *Store, logger *slog.Logger) *GatewayClient {
	return &GatewayClient{
		url: url, token: token, store: store, logger: logger,
		dialer: websocket.Dialer{
			HandshakeTimeout: 10 * time.Second,
			ReadBufferSize:   64 * 1024,
			WriteBufferSize:  64 * 1024,
		},
	}
}

func (c *GatewayClient) Run(ctx context.Context) {
	backoff := 250 * time.Millisecond
	for ctx.Err() == nil {
		err := c.runConnection(ctx)
		c.store.Disconnect()
		if ctx.Err() != nil {
			return
		}
		c.logger.Warn("MDS gateway disconnected", "error", err, "retry", backoff)
		jitter := time.Duration(time.Now().UnixNano() % int64(max(time.Millisecond, backoff/4)))
		timer := time.NewTimer(backoff + jitter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(30*time.Second, backoff*2)
	}
}

func (c *GatewayClient) runConnection(ctx context.Context) error {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+c.token)
	connection, response, err := c.dialer.DialContext(ctx, c.url, headers)
	if err != nil {
		if response != nil {
			return fmt.Errorf("dial MDS gateway (HTTP %d): %w", response.StatusCode, err)
		}
		return fmt.Errorf("dial MDS gateway: %w", err)
	}
	defer connection.Close()
	c.store.Disconnect()
	defer c.store.Disconnect()
	connection.SetReadLimit(128 * 1024)
	if err := connection.SetReadDeadline(time.Now().Add(45 * time.Second)); err != nil {
		return fmt.Errorf("set MDS read deadline: %w", err)
	}
	connection.SetPongHandler(func(string) error {
		return connection.SetReadDeadline(time.Now().Add(45 * time.Second))
	})

	connectionCtx, cancel := context.WithCancel(ctx)
	var writers sync.WaitGroup
	writers.Add(1)
	writerErrors := make(chan error, 1)
	go func() {
		defer writers.Done()
		writerErrors <- c.subscriptionWriter(connectionCtx, connection)
	}()
	defer func() {
		cancel()
		writers.Wait()
	}()

	for {
		messageType, payload, err := connection.ReadMessage()
		if err != nil {
			select {
			case writerErr := <-writerErrors:
				if writerErr != nil {
					return writerErr
				}
			default:
			}
			return fmt.Errorf("read MDS gateway: %w", err)
		}
		switch messageType {
		case websocket.BinaryMessage:
			frame, err := DecodeGatewayFrame(payload)
			if err != nil {
				return fmt.Errorf("decode MDS gateway frame: %w", err)
			}
			if err := c.store.Apply(frame); err != nil {
				return fmt.Errorf("apply MDS gateway frame: %w", err)
			}
		case websocket.TextMessage:
			var acknowledgement struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(payload, &acknowledgement); err != nil || !acknowledgement.OK {
				return fmt.Errorf("MDS gateway rejected control: %s", acknowledgement.Error)
			}
		default:
			return fmt.Errorf("unexpected MDS gateway message type %d", messageType)
		}
	}
}

func (c *GatewayClient) subscriptionWriter(ctx context.Context, connection *websocket.Conn) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	subscribed := make(map[string]struct{})
	for {
		desired := make(map[string]struct{})
		for _, segment := range c.store.ActiveSegments() {
			desired[segment] = struct{}{}
		}
		for segment := range subscribed {
			if _, keep := desired[segment]; !keep {
				if err := writeGatewayControl(connection, "unsubscribe", segment); err != nil {
					return err
				}
				delete(subscribed, segment)
				continue
			}
			if !c.store.SegmentMapped(segment) {
				delete(subscribed, segment)
				c.store.RecordTopicRelearn()
			}
		}
		pending := make([]string, 0, len(desired))
		for segment := range desired {
			if _, exists := subscribed[segment]; exists {
				continue
			}
			pending = append(pending, segment)
		}
		sort.Strings(pending)
		for _, segment := range pending {
			c.store.BeginLearning(segment)
			if err := writeGatewayControl(connection, "subscribe", segment); err != nil {
				return err
			}
			subscribed[segment] = struct{}{}
			deadline := time.NewTimer(5 * time.Second)
			probe := time.NewTicker(20 * time.Millisecond)
			mapped := false
			for !mapped {
				select {
				case <-ctx.Done():
					deadline.Stop()
					probe.Stop()
					return nil
				case <-deadline.C:
					probe.Stop()
					return fmt.Errorf("timed out learning MDS topic %q", segment)
				case <-probe.C:
					mapped = c.store.SegmentMapped(segment)
				}
			}
			deadline.Stop()
			probe.Stop()
		}
		if err := connection.WriteControl(
			websocket.PingMessage, nil, time.Now().Add(5*time.Second),
		); err != nil {
			return fmt.Errorf("ping MDS gateway: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func writeGatewayControl(connection *websocket.Conn, operation, segment string) error {
	if err := connection.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return fmt.Errorf("set MDS write deadline: %w", err)
	}
	if err := connection.WriteJSON(map[string]string{"op": operation, "topic": segment}); err != nil {
		return fmt.Errorf("%s MDS topic: %w", operation, err)
	}
	return nil
}
