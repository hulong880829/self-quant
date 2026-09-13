package info

import (
	"encoding/json"
	"fmt"
)

// FundingHistory is one row in the per-coin funding rate history.
type FundingHistory struct {
	Coin        string `json:"coin"`
	FundingRate string `json:"fundingRate"`
	Premium     string `json:"premium"`
	Time        int64  `json:"time"`
}

// UserFundingHistory is one row in the per-user funding payment history.
type UserFundingHistory struct {
	User      string `json:"user"`
	Type      string `json:"type"`
	StartTime int64  `json:"startTime"`
	EndTime   int64  `json:"endTime"`
}

// Funding returns historical funding rates for coin in [start, end].
func (c *Client) Funding(coin string, start int64, end *int64) ([]FundingHistory, error) {
	cc := c.nameToCoin[coin]
	resp, err := c.postTimeRangeRequest(
		"fundingHistory",
		"",
		start,
		end,
		map[string]any{"coin": cc},
	)
	if err != nil {
		return nil, err
	}

	var result []FundingHistory
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal funding history: %w", err)
	}
	return result, nil
}

// UserFunding returns the funding history for addr in [start, end].
func (c *Client) UserFunding(addr string, start int64, end *int64) ([]UserFundingHistory, error) {
	resp, err := c.postTimeRangeRequest("userFunding", addr, start, end, nil)
	if err != nil {
		return nil, err
	}

	var result []UserFundingHistory
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal user funding history: %w", err)
	}
	return result, nil
}
