package polymarket

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type DataClient struct {
	baseURL string
	http    *http.Client
}

func NewDataClient(baseURL string, timeout time.Duration) *DataClient {
	return &DataClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

type dataPosition struct {
	Asset       string  `json:"asset"`
	ConditionID string  `json:"conditionId"`
	Title       string  `json:"title"`
	Outcome     string  `json:"outcome"`
	Size        float64 `json:"size"`
	AvgPrice    float64 `json:"avgPrice"`
	CurPrice    float64 `json:"curPrice"`
	Initial     float64 `json:"initialValue"`
	Current     float64 `json:"currentValue"`
	CashPnL     float64 `json:"cashPnl"`
	PercentPnL  float64 `json:"percentPnl"`
	Redeemable  bool    `json:"redeemable"`
	EndDate     string  `json:"endDate"`
}

func (c *DataClient) ListPositions(ctx context.Context, wallet string) ([]Position, error) {
	query := url.Values{"user": {wallet}, "sizeThreshold": {"0.0001"}, "limit": {"500"}}
	var source []dataPosition
	if err := c.getJSON(ctx, "/positions?"+query.Encode(), &source); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	result := make([]Position, 0, len(source))
	for _, item := range source {
		endTime, _ := time.Parse(time.RFC3339, item.EndDate)
		result = append(result, Position{
			ID: item.Asset, ConditionID: item.ConditionID, TokenID: item.Asset,
			Market: item.Title, Outcome: item.Outcome,
			Size: formatDecimal(item.Size), AveragePrice: formatDecimal(item.AvgPrice),
			CurrentPrice: formatDecimal(item.CurPrice),
			InitialValue: formatDecimal(item.Initial), CurrentValue: formatDecimal(item.Current),
			CashPnL: formatDecimal(item.CashPnL), PercentPnL: formatDecimal(item.PercentPnL),
			Redeemable: item.Redeemable, EndTime: endTime, SourceUpdatedAt: now,
		})
	}
	return result, nil
}

func (c *DataClient) AccountValue(ctx context.Context, wallet string) (string, error) {
	query := url.Values{"user": {wallet}}
	var response any
	if err := c.getJSON(ctx, "/value?"+query.Encode(), &response); err != nil {
		return "", err
	}
	switch value := response.(type) {
	case float64:
		return formatDecimal(value), nil
	case map[string]any:
		for _, key := range []string{"value", "totalValue", "positionValue"} {
			if number, ok := value[key].(float64); ok {
				return formatDecimal(number), nil
			}
		}
	case []any:
		if len(value) > 0 {
			if object, ok := value[0].(map[string]any); ok {
				if number, ok := object["value"].(float64); ok {
					return formatDecimal(number), nil
				}
			}
		}
	}
	return "0", nil
}

func (c *DataClient) getJSON(ctx context.Context, path string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	response, err := doWithRetry(ctx, c.http, func() (*http.Request, error) {
		return request.Clone(ctx), nil
	}, 3)
	if err != nil {
		return fmt.Errorf("data API request: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("data API returned %d", response.StatusCode)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode data API response: %w", err)
	}
	return nil
}

func formatDecimal(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
