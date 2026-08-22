package polymarket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type HistoryClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewHistoryClient(baseURL, token string, timeout time.Duration) *HistoryClient {
	return &HistoryClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   strings.TrimSpace(token),
		http:    &http.Client{Timeout: timeout},
	}
}

func (c *HistoryClient) Enabled() bool {
	return c != nil && c.baseURL != ""
}

func (c *HistoryClient) PriceAt(
	ctx context.Context,
	asset string,
	at time.Time,
) (string, time.Time, error) {
	if !c.Enabled() {
		return "", time.Time{}, errors.New("chainlink history is not configured")
	}
	query := url.Values{
		"asset":     {strings.ToUpper(asset)},
		"timestamp": {at.UTC().Format(time.RFC3339Nano)},
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, c.baseURL+"?"+query.Encode(), nil,
	)
	if err != nil {
		return "", time.Time{}, err
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return "", time.Time{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", time.Time{}, fmt.Errorf("chainlink history returned %d", response.StatusCode)
	}
	var payload struct {
		Price     string `json:"price"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return "", time.Time{}, err
	}
	observedAt, err := time.Parse(time.RFC3339Nano, payload.Timestamp)
	if err != nil || payload.Price == "" {
		return "", time.Time{}, errors.New("invalid chainlink history response")
	}
	return payload.Price, observedAt, nil
}
