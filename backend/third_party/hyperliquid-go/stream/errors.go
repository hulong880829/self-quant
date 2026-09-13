package stream

import "errors"

var (
	// ErrNotConnected is returned when a POST is attempted before Connect
	// or after the stream has been closed.
	ErrNotConnected = errors.New("hyperliquid stream: not connected")
	// ErrConnectionLost is returned when the websocket drops while a POST
	// is in flight. The action may already have reached the venue.
	ErrConnectionLost = errors.New("hyperliquid stream: connection lost")
	// ErrRequestTimeout is returned when the caller context deadline
	// expires before a POST response arrives.
	ErrRequestTimeout = errors.New("hyperliquid stream: request timeout")
	// ErrRequestCanceled is returned when the caller context is canceled
	// before a POST response arrives.
	ErrRequestCanceled = errors.New("hyperliquid stream: request canceled")
	// ErrBadResponse is returned when a websocket POST payload cannot be
	// parsed as the expected venue envelope.
	ErrBadResponse = errors.New("hyperliquid stream: unreadable response")
)
