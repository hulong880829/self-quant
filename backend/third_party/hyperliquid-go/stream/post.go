package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Simon-Busch/hyperliquid-go/signing"
)

// pendingRequest tracks an in-flight WS POST awaiting its response.
type pendingRequest struct {
	responseChan chan WsPostResponseData
	err          error
	once         sync.Once
}

func (p *pendingRequest) fail(err error) {
	p.once.Do(func() {
		p.err = err
		close(p.responseChan)
	})
}

func (c *Client) failAllPending(err error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for id, pending := range c.pendingRequests {
		pending.fail(err)
		delete(c.pendingRequests, id)
	}
}

// Post sends a POST-style request over the WebSocket and waits up to
// timeout for the response. Lower-level than PostInfo / PostAction;
// prefer those.
func (c *Client) Post(
	requestType string,
	payload any,
	timeout time.Duration,
) (*WsPostResponseData, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.PostContext(ctx, requestType, payload)
}

// PostContext sends a POST-style request over the WebSocket and waits
// until ctx is done. Context cancellation removes the pending request;
// late responses are discarded. The action is never automatically resent.
func (c *Client) PostContext(
	ctx context.Context,
	requestType string,
	payload any,
) (*WsPostResponseData, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !c.connected.Load() || c.closed.Load() {
		return nil, ErrNotConnected
	}

	id := int(c.nextPostID.Add(1))
	responseChan := make(chan WsPostResponseData, 1)
	pending := &pendingRequest{responseChan: responseChan}

	c.pendingMu.Lock()
	c.pendingRequests[id] = pending
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pendingRequests, id)
		c.pendingMu.Unlock()
	}()

	request := WsPostRequest{
		Method: "post",
		ID:     id,
		Request: WsRequest{
			Type:    requestType,
			Payload: payload,
		},
	}

	if err := c.writeJSON(request); err != nil {
		pending.fail(fmt.Errorf("%w: %v", ErrConnectionLost, err))
		return nil, fmt.Errorf("%w: %v", ErrConnectionLost, err)
	}

	select {
	case response, ok := <-responseChan:
		if !ok {
			if pending.err != nil {
				return nil, pending.err
			}
			return nil, ErrRequestCanceled
		}
		return &response, nil
	case <-ctx.Done():
		pending.fail(ctx.Err())
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %v", ErrRequestTimeout, ctx.Err())
		}
		return nil, fmt.Errorf("%w: %v", ErrRequestCanceled, ctx.Err())
	}
}

// PostInfo sends an info-style request over the WebSocket. When timeout
// is zero the call waits up to 30s.
func (c *Client) PostInfo(
	payload map[string]any,
	timeout time.Duration,
) (json.RawMessage, error) {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.PostInfoContext(ctx, payload)
}

// PostInfoContext is the context-aware form of PostInfo.
func (c *Client) PostInfoContext(
	ctx context.Context,
	payload map[string]any,
) (json.RawMessage, error) {
	resp, err := c.PostContext(ctx, "info", payload)
	if err != nil {
		return nil, err
	}
	if resp.Response.Type == "error" {
		return nil, fmt.Errorf("info request error: %s", string(resp.Response.Payload))
	}
	return resp.Response.Payload, nil
}

// PostAction sends a signed action over the WebSocket. vaultAddress is
// forwarded as-is -- supply an empty string to set vaultAddress: null.
// When timeout is zero the call waits up to 30s.
func (c *Client) PostAction(
	action any,
	signature signing.SignatureResult,
	nonce int64,
	vaultAddress string,
	timeout time.Duration,
) (json.RawMessage, error) {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.PostActionContext(ctx, action, signature, nonce, vaultAddress)
}

// PostActionContext sends a signed action over the WebSocket using the
// caller's context for cancellation and timeout. It never retries.
func (c *Client) PostActionContext(
	ctx context.Context,
	action any,
	signature signing.SignatureResult,
	nonce int64,
	vaultAddress string,
) (json.RawMessage, error) {
	payload := map[string]any{
		"action":    action,
		"nonce":     nonce,
		"signature": signature,
	}
	if vaultAddress != "" {
		payload["vaultAddress"] = vaultAddress
	} else {
		payload["vaultAddress"] = nil
	}
	resp, err := c.PostContext(ctx, "action", payload)
	if err != nil {
		return nil, err
	}
	if resp.Response.Type == "error" {
		return nil, fmt.Errorf("action request error: %s", string(resp.Response.Payload))
	}
	return resp.Response.Payload, nil
}
