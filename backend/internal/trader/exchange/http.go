package exchange

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type signedClient struct {
	http    *http.Client
	base    string
	limiter *limiter
}

type limiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newSignedClient(client *http.Client, base string, interval time.Duration) *signedClient {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if interval <= 0 {
		interval = 80 * time.Millisecond
	}
	return &signedClient{
		http: client, base: strings.TrimRight(base, "/"),
		limiter: &limiter{interval: interval},
	}
}

func (c *signedClient) wait(ctx context.Context) error {
	c.limiter.mu.Lock()
	delay := time.Until(c.limiter.next)
	if delay < 0 {
		delay = 0
	}
	c.limiter.next = time.Now().Add(delay + c.limiter.interval)
	c.limiter.mu.Unlock()
	if delay == 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *signedClient) postpone(delay time.Duration) {
	if delay <= 0 {
		delay = time.Second
	}
	c.limiter.mu.Lock()
	defer c.limiter.mu.Unlock()
	candidate := time.Now().Add(delay)
	if candidate.After(c.limiter.next) {
		c.limiter.next = candidate
	}
}

func (c *signedClient) do(
	ctx context.Context,
	method, path string,
	headers http.Header,
	body []byte,
	target any,
) ([]byte, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = headers.Clone()
	if len(body) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %v", ErrUncertain, err)
		}
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout {
		return raw, fmt.Errorf("%w: upstream status %d", ErrUncertain, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		delay := retryAfter(resp.Header.Get("Retry-After"))
		c.postpone(delay)
		return raw, fmt.Errorf("%w: retry after %s", ErrRateLimited, delay)
	}
	if resp.StatusCode >= 300 {
		if msg := venueErrorMessage(raw); msg != "" {
			return raw, fmt.Errorf("%w: upstream status %d: %s", ErrRejected, resp.StatusCode, msg)
		}
		return raw, fmt.Errorf("%w: upstream status %d", ErrRejected, resp.StatusCode)
	}
	if target != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, target); err != nil {
			return raw, fmt.Errorf("decode venue response: %w", err)
		}
	}
	return raw, nil
}

func retryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		if delay := time.Until(date); delay > 0 {
			return delay
		}
	}
	return time.Second
}

func hmacHex256(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func hmacBase64(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func hmacHex512(secret, payload string) string {
	mac := hmac.New(sha512.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func sha512Hex(payload []byte) string {
	sum := sha512.Sum512(payload)
	return hex.EncodeToString(sum[:])
}

func nowMillis() string {
	return strconv.FormatInt(time.Now().UnixMilli(), 10)
}

func venueErrorMessage(raw []byte) string {
	var payload struct {
		Msg, Message, RetMsg string
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return firstNonEmpty(payload.Msg, payload.Message, payload.RetMsg)
}

func compactJSON(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return raw
}

func rawMap(raw []byte) map[string]any {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return map[string]any{"body": strings.TrimSpace(string(raw))}
	}
	return payload
}
