package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/Simon-Busch/hyperliquid-go/types"
)

const (
	// httpErrorStatusCode is the minimum status code considered an error.
	httpErrorStatusCode = 400
)

// Client is the low-level HTTP wrapper used by the info and exchange
// surfaces. Construct via New.
type Client struct {
	// BaseURL is the resolved REST endpoint. Exported so callers can
	// branch on environment (mainnet vs. testnet) for signing.
	BaseURL    string
	httpClient *http.Client
}

// New builds a Client targeting baseURL with the default transport when
// httpClient is nil. Empty baseURL defaults to types.MainnetAPIURL.
func New(baseURL string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = types.MainnetAPIURL
	}
	if httpClient == nil {
		httpClient = &http.Client{Transport: Default()}
	}
	return &Client{BaseURL: baseURL, httpClient: httpClient}
}

// Post issues an HTTP POST to BaseURL+path with the supplied JSON-encoded
// payload. The HL_DEBUG_HTTP=true env var dumps the request line.
func (c *Client) Post(path string, payload any) ([]byte, error) {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal payload: %w", err)
	}

	if os.Getenv("HL_DEBUG_HTTP") == "true" {
		fmt.Fprintf(os.Stderr, ">>> POST %s%s\n%s\n", c.BaseURL, path, jsonData)
	}

	url := c.BaseURL + path
	req, err := http.NewRequestWithContext(
		context.Background(),
		"POST",
		url,
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode >= httpErrorStatusCode {
		return nil, &types.APIError{
			Code:    resp.StatusCode,
			Message: string(body),
		}
	}

	return body, nil
}

// WarmUp sends a lightweight /info request to establish the HTTP/2
// connection up front so the first real request doesn't pay the cold-start
// penalty.
func (c *Client) WarmUp() error {
	_, err := c.Post("/info", map[string]any{"type": "meta"})
	return err
}
