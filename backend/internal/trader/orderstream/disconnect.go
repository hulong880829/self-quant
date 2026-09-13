package orderstream

import (
	"errors"
	"net"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

type disconnectReason string

const (
	reasonReadTimeout            disconnectReason = "read_timeout"
	reasonRemoteClose            disconnectReason = "remote_close"
	reasonNetworkIO              disconnectReason = "network_io"
	reasonHeartbeatWriteFailed   disconnectReason = "heartbeat_write_failed"
	reasonListenKeyRefreshFailed disconnectReason = "listen_key_refresh_failed"
	reasonAuthenticationFailed   disconnectReason = "authentication_failed"
	reasonSubscriptionFailed     disconnectReason = "subscription_failed"
	reasonPlannedRotation        disconnectReason = "planned_rotation"
	reasonShutdown               disconnectReason = "shutdown"
	reasonUnknown                disconnectReason = "unknown"
)

const disconnectErrorLogLimit = 256

type disconnectError struct {
	Reason        disconnectReason
	Err           error
	HeartbeatType string
	WriteDeadline time.Time
}

func (e *disconnectError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return string(e.Reason)
	}
	if e.Reason == "" {
		return e.Err.Error()
	}
	return string(e.Reason) + ": " + e.Err.Error()
}

func (e *disconnectError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func newDisconnectError(reason disconnectReason, err error) error {
	if err == nil {
		if reason == "" {
			return nil
		}
		return &disconnectError{Reason: reason}
	}
	var existing *disconnectError
	if errors.As(err, &existing) {
		if existing.Reason != "" {
			return err
		}
		existing.Reason = reason
		return existing
	}
	return &disconnectError{Reason: reason, Err: err}
}

func classifyDisconnect(err error) disconnectReason {
	if err == nil {
		return reasonUnknown
	}
	var de *disconnectError
	if errors.As(err, &de) && de.Reason != "" {
		return de.Reason
	}
	return classifyIOError(err)
}

func classifyIOError(err error) disconnectReason {
	if err == nil {
		return reasonUnknown
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return reasonReadTimeout
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		return reasonRemoteClose
	}
	return reasonNetworkIO
}

func classifyConnectError(err error) disconnectReason {
	if err == nil {
		return reasonUnknown
	}
	if reason := classifyDisconnect(err); reason != reasonUnknown && reason != reasonNetworkIO {
		return reason
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "authenticate") || strings.Contains(message, "authentication"):
		return reasonAuthenticationFailed
	case strings.Contains(message, "subscribe") || strings.Contains(message, "subscription"):
		return reasonSubscriptionFailed
	default:
		return classifyIOError(err)
	}
}

func sanitizeDisconnectError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if !utf8.ValidString(message) {
		message = strings.ToValidUTF8(message, "")
	}
	if len(message) > disconnectErrorLogLimit {
		return message[:disconnectErrorLogLimit]
	}
	return message
}
