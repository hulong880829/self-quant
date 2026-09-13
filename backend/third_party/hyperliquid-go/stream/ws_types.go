package stream

import (
	"encoding/json"
)

// WSMessage is a single websocket frame delivered to subscription
// callbacks. Channel is the server-side topic; Data is the raw JSON
// payload for the caller to unmarshal.
type WSMessage struct {
	Channel string          `json:"channel"`
	Data    json.RawMessage `json:"data"`
}

// SubscriptionFilter is the wire-shape that the Hyperliquid WS API
// expects in subscribe/unsubscribe commands. It is built by the
// package-level constructors (Trades, Book, etc.) and consumed by
// Client.Subscribe.
type SubscriptionFilter struct {
	Type     string `json:"type"`
	Coin     string `json:"coin,omitempty"`
	User     string `json:"user,omitempty"`
	Interval string `json:"interval,omitempty"`
	Dex      string `json:"dex,omitempty"`
}

type subKey struct {
	typ      string
	coin     string
	user     string
	interval string
	dex      string
}

func (s SubscriptionFilter) key() subKey {
	return subKey{
		typ:      s.Type,
		coin:     s.Coin,
		user:     s.User,
		interval: s.Interval,
		dex:      s.Dex,
	}
}

// WsCommand is the envelope used to send a subscribe/unsubscribe/ping
// command over the websocket.
type WsCommand struct {
	Method       string              `json:"method"`
	Subscription *SubscriptionFilter `json:"subscription,omitempty"`
}

type subscriptionCallback struct {
	id       int
	callback func(WSMessage)
}

// WebSocket POST request types

// WsPostRequest is the request structure for WebSocket POST requests
type WsPostRequest struct {
	Method  string    `json:"method"` // Always "post"
	ID      int       `json:"id"`
	Request WsRequest `json:"request"`
}

// WsRequest wraps the actual request payload
type WsRequest struct {
	Type    string `json:"type"` // "info" or "action"
	Payload any    `json:"payload"`
}

// WsPostResponseData is the data field of a POST response message
type WsPostResponseData struct {
	ID       int        `json:"id"`
	Response WsResponse `json:"response"`
}

// WsResponse contains the actual response payload
type WsResponse struct {
	Type    string          `json:"type"` // "info", "action", or "error"
	Payload json.RawMessage `json:"payload"`
}

// WsMsg represents a WebSocket message with a channel and data payload.
type WsMsg struct {
	Channel string         `json:"channel"`
	Data    map[string]any `json:"data"`
}

// Trade is a single trade record (used in the trades subscription feed).
type Trade struct {
	Coin  string   `json:"coin"`
	Side  string   `json:"side"`
	Px    string   `json:"px"`
	Sz    string   `json:"sz"`
	Time  int64    `json:"time"`
	Hash  string   `json:"hash"`
	Tid   int64    `json:"tid"`
	Users []string `json:"users"`
}
