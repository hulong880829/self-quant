package stream

import (
	"context"
	"math/rand"
	"time"
)

// handleDisconnect tears down the current connection and schedules a
// reconnect. Safe to call repeatedly; no-op after Close.
func (c *Client) handleDisconnect() {
	if c.closed.Load() {
		return
	}

	c.connected.Store(false)

	c.connMu.Lock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	c.connMu.Unlock()

	c.failAllPending(ErrConnectionLost)
	c.scheduleReconnect()
}

// scheduleReconnect schedules an asynchronous reconnection attempt with
// exponential backoff and jitter. Subsequent failures double the delay
// until maxReconnectWait.
func (c *Client) scheduleReconnect() {
	if c.closed.Load() {
		return
	}

	c.reconnectMu.Lock()
	defer c.reconnectMu.Unlock()

	if c.reconnectTimer != nil {
		c.reconnectTimer.Stop()
	}

	c.reconnectAttempts++
	attempts := c.reconnectAttempts

	if c.maxReconnectAttempts > 0 && attempts > c.maxReconnectAttempts {
		c.warnf("Max reconnection attempts (%d) reached, giving up", c.maxReconnectAttempts)
		return
	}

	backoff := c.reconnectWait * time.Duration(1<<(attempts-1))
	if backoff > maxReconnectWait {
		backoff = maxReconnectWait
	}
	jitter := time.Duration(float64(backoff) * 0.2 * (2*rand.Float64() - 1))
	delay := backoff + jitter
	if delay < time.Second {
		delay = time.Second
	}

	c.warnf("Reconnection attempt %d in %v...", attempts, delay)

	c.reconnectTimer = time.AfterFunc(delay, func() {
		if c.closed.Load() || c.connected.Load() {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
		err := c.Connect(ctx)
		cancel()

		if err != nil {
			c.warnf("Reconnection attempt %d failed: %v", attempts, err)
			c.scheduleReconnect()
		} else {
			c.warnf("Reconnection successful after %d attempts", attempts)
		}
	})
}
