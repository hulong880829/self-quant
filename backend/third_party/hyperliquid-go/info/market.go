package info

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// L2Book is the L2 order book snapshot returned by /info {"type":"l2Book"}.
type L2Book struct {
	Coin   string    `json:"coin"`
	Levels [][]Level `json:"levels"`
	Time   int64     `json:"time"`
}

// Level is one price level inside an L2Book.
type Level struct {
	N  int     `json:"n"`
	Px float64 `json:"px,string"`
	Sz float64 `json:"sz,string"`
}

// Candle is a single OHLC bar.
type Candle struct {
	Timestamp int64  `json:"T"`
	Close     string `json:"c"`
	High      string `json:"h"`
	Interval  string `json:"i"`
	Low       string `json:"l"`
	Number    int    `json:"n"`
	Open      string `json:"o"`
	Symbol    string `json:"s"`
	Time      int64  `json:"t"`
	Volume    string `json:"v"`
}

// Mid returns the current mid price for coin as a float64. Coin names
// containing a "<dex>:" prefix (HIP-3 builder-deployed perps) cause the
// lookup to pin to that dex automatically, since the default-dex mid
// table does not include builder-DEX coins.
func (c *Client) Mid(coin string) (float64, error) {
	var dex []string
	if idx := strings.Index(coin, ":"); idx > 0 {
		dex = []string{coin[:idx]}
	}
	mids, err := c.AllMids(dex...)
	if err != nil {
		return 0, err
	}
	s, ok := mids[coin]
	if !ok {
		return 0, fmt.Errorf("no mid for %s", coin)
	}
	return strconv.ParseFloat(s, 64)
}

// AllMids retrieves mids for all coins. If dex is provided and non-empty,
// the snapshot is pinned to that HIP-3 dex. Spot mids are only included
// with the first perp dex.
func (c *Client) AllMids(dex ...string) (map[string]string, error) {
	payload := map[string]any{
		"type": "allMids",
	}
	if len(dex) > 0 && dex[0] != "" {
		payload["dex"] = dex[0]
	}

	resp, err := c.client.Post("/info", payload)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch all mids: %w", err)
	}

	var result map[string]string
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal all mids: %w", err)
	}
	return result, nil
}

// AllMidsOn returns the AllMids snapshot pinned to a specific HIP-3 dex.
func (c *Client) AllMidsOn(dex string) (map[string]string, error) {
	return c.AllMids(dex)
}

// Book returns the current L2 order book for coin.
func (c *Client) Book(coin string) (*L2Book, error) {
	resp, err := c.client.Post("/info", map[string]any{
		"type": "l2Book",
		"coin": c.nameToCoin[coin],
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch L2 snapshot: %w", err)
	}

	var result L2Book
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal L2 snapshot: %w", err)
	}
	return &result, nil
}

// Candles returns historical candles for coin at interval between start
// and end (Unix millis).
func (c *Client) Candles(coin, interval string, start, end int64) ([]Candle, error) {
	req := map[string]any{
		"coin":      c.nameToCoin[coin],
		"interval":  interval,
		"startTime": start,
		"endTime":   end,
	}

	resp, err := c.client.Post("/info", map[string]any{
		"type": "candleSnapshot",
		"req":  req,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch candles snapshot: %w", err)
	}

	var result []Candle
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal candles snapshot: %w", err)
	}
	return result, nil
}

// MetaAndAssetCtxs fetches perp metadata together with the per-asset
// context snapshot in a single request.
func (c *Client) MetaAndAssetCtxs() (*MetaAndAssetCtxsResponse, error) {
	resp, err := c.client.Post("/info", map[string]any{
		"type": "metaAndAssetCtxs",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch meta and asset contexts: %w", err)
	}

	// The API returns an array with two elements: [meta, assetCtxs]
	var rawResponse [2]interface{}
	if err := json.Unmarshal(resp, &rawResponse); err != nil {
		return nil, fmt.Errorf("failed to unmarshal meta and asset contexts array: %w", err)
	}

	// Parse the meta object (first element)
	metaBytes, err := json.Marshal(rawResponse[0])
	if err != nil {
		return nil, fmt.Errorf("failed to marshal meta object: %w", err)
	}

	var meta Meta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, fmt.Errorf("failed to unmarshal meta: %w", err)
	}

	// Parse the asset contexts array (second element)
	assetCtxsBytes, err := json.Marshal(rawResponse[1])
	if err != nil {
		return nil, fmt.Errorf("failed to marshal asset contexts array: %w", err)
	}

	var assetCtxs []AssetCtx
	if err := json.Unmarshal(assetCtxsBytes, &assetCtxs); err != nil {
		return nil, fmt.Errorf("failed to unmarshal asset contexts: %w", err)
	}

	return &MetaAndAssetCtxsResponse{
		Meta:      meta,
		AssetCtxs: assetCtxs,
	}, nil
}

// SpotMetaAndAssetCtxs fetches spot metadata together with the per-asset
// context snapshot.
func (c *Client) SpotMetaAndAssetCtxs() (map[string]any, error) {
	resp, err := c.client.Post("/info", map[string]any{
		"type": "spotMetaAndAssetCtxs",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch spot meta and asset contexts: %w", err)
	}

	var result map[string]any
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal spot meta and asset contexts: %w", err)
	}
	return result, nil
}
