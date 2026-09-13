package info

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// OpenOrder is the slim open-orders row returned by /info
// {"type":"openOrders"}.
type OpenOrder struct {
	Coin      string  `json:"coin"`
	LimitPx   float64 `json:"limitPx,string"`
	Oid       int64   `json:"oid"`
	Side      string  `json:"side"`
	Size      float64 `json:"sz,string"`
	Timestamp int64   `json:"timestamp"`
}

// FrontendOpenOrder represents the detailed order information returned by frontendOpenOrders
type FrontendOpenOrder struct {
	Coin             string  `json:"coin"`
	IsPositionTpsl   bool    `json:"isPositionTpsl"`
	IsTrigger        bool    `json:"isTrigger"`
	LimitPx          float64 `json:"limitPx,string"`
	Oid              int64   `json:"oid"`
	OrderType        string  `json:"orderType"`
	OrigSz           float64 `json:"origSz,string"`
	ReduceOnly       bool    `json:"reduceOnly"`
	Side             string  `json:"side"`
	Size             float64 `json:"sz,string"`
	Timestamp        int64   `json:"timestamp"`
	TriggerCondition string  `json:"triggerCondition"`
	TriggerPx        float64 `json:"triggerPx,string"`
}

// Fill is a single trade execution row in the userFills feed.
type Fill struct {
	ClosedPnl     string `json:"closedPnl"`
	Coin          string `json:"coin"`
	Crossed       bool   `json:"crossed"`
	Dir           string `json:"dir"`
	Hash          string `json:"hash"`
	Oid           int64  `json:"oid"`
	Price         string `json:"px"`
	Side          string `json:"side"`
	StartPosition string `json:"startPosition"`
	Size          string `json:"sz"`
	Time          int64  `json:"time"`
	Fee           string `json:"fee"`
	FeeToken      string `json:"feeToken"`
}

// ReferralState is the per-user referral snapshot returned by /info
// {"type":"referral"}.
type ReferralState struct {
	ReferredBy       *ReferredBy    `json:"referredBy,omitempty"`
	CumVlm           string         `json:"cumVlm"`
	UnclaimedRewards string         `json:"unclaimedRewards"`
	ClaimedRewards   string         `json:"claimedRewards"`
	BuilderRewards   string         `json:"builderRewards"`
	ReferrerState    *ReferrerState `json:"referrerState,omitempty"`
	RewardHistory    []interface{}  `json:"rewardHistory"`
}

// ReferredBy describes the referrer of an account.
type ReferredBy struct {
	Referrer string `json:"referrer"`
	Code     string `json:"code"`
}

// ReferrerState is the per-referrer portion of ReferralState.
type ReferrerState struct {
	Stage string        `json:"stage"`
	Data  *ReferrerData `json:"data,omitempty"`
}

// ReferrerData groups the referrer code with the list of referred
// accounts.
type ReferrerData struct {
	Code           string           `json:"code"`
	ReferralStates []ReferralMember `json:"referralStates"`
}

// ReferralMember is one referred-account row inside ReferrerData.
type ReferralMember struct {
	CumVlm                       string `json:"cumVlm"`
	CumRewardedFeesSinceReferred string `json:"cumRewardedFeesSinceReferred"`
	CumFeesRewardedToReferrer    string `json:"cumFeesRewardedToReferrer"`
	TimeJoined                   int64  `json:"timeJoined"`
	User                         string `json:"user"`
}

// OrderStatusResponse represents the actual response from the orderStatus endpoint
type OrderStatusResponse struct {
	Status string `json:"status"`
	Order  struct {
		Order struct {
			Coin             string  `json:"coin"`
			Side             string  `json:"side"`
			LimitPx          string  `json:"limitPx"`
			Sz               string  `json:"sz"`
			Oid              int64   `json:"oid"`
			Timestamp        int64   `json:"timestamp"`
			TriggerCondition string  `json:"triggerCondition"`
			IsTrigger        bool    `json:"isTrigger"`
			TriggerPx        string  `json:"triggerPx"`
			Children         []any   `json:"children"`
			IsPositionTpsl   bool    `json:"isPositionTpsl"`
			ReduceOnly       bool    `json:"reduceOnly"`
			OrderType        string  `json:"orderType"`
			OrigSz           string  `json:"origSz"`
			Tif              *string `json:"tif"`
			Cloid            *string `json:"cloid"`
		} `json:"order"`
		Status          string `json:"status"`
		StatusTimestamp int64  `json:"statusTimestamp"`
	} `json:"order"`
}

// OpenOrders retrieves the user's open orders. If dex is provided and
// non-empty, the query is pinned to that HIP-3 dex. Spot open orders are
// only returned with the first perp dex.
func (c *Client) OpenOrders(address string, dex ...string) ([]OpenOrder, error) {
	payload := map[string]any{
		"type": "openOrders",
		"user": address,
	}
	if len(dex) > 0 && dex[0] != "" {
		payload["dex"] = dex[0]
	}

	resp, err := c.client.Post("/info", payload)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch open orders: %w", err)
	}

	var result []OpenOrder
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal open orders: %w", err)
	}
	return result, nil
}

// FrontendOpenOrders retrieves the user's open orders with frontend info.
// If dex is provided and non-empty, the query is pinned to that HIP-3
// dex. Spot open orders are only returned with the first perp dex.
func (c *Client) FrontendOpenOrders(address string, dex ...string) ([]FrontendOpenOrder, error) {
	payload := map[string]any{
		"type": "frontendOpenOrders",
		"user": address,
	}
	if len(dex) > 0 && dex[0] != "" {
		payload["dex"] = dex[0]
	}

	resp, err := c.client.Post("/info", payload)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch frontend open orders: %w", err)
	}

	var result []FrontendOpenOrder
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal frontend open orders: %w", err)
	}
	return result, nil
}

// Fills retrieves the trailing fill history for addr.
func (c *Client) Fills(addr string) ([]Fill, error) {
	resp, err := c.client.Post("/info", map[string]any{
		"type": "userFills",
		"user": addr,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch user fills: %w", err)
	}

	var result []Fill
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal user fills: %w", err)
	}
	return result, nil
}

// FillsBetween retrieves the fill history for addr in [start, end].
func (c *Client) FillsBetween(addr string, start int64, end *int64) ([]Fill, error) {
	resp, err := c.postTimeRangeRequest("userFillsByTime", addr, start, end, nil)
	if err != nil {
		return nil, err
	}

	var result []Fill
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal user fills by time: %w", err)
	}
	return result, nil
}

// Order returns the order status for the supplied (addr, oid) pair.
func (c *Client) Order(addr string, oid int64) (*OrderStatusResponse, error) {
	resp, err := c.client.Post("/info", map[string]any{
		"type": "orderStatus",
		"user": addr,
		"oid":  oid,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch order status: %w", err)
	}

	var result OrderStatusResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal order status: %w", err)
	}

	return &result, nil
}

// Fill finds the fill matching (addr, oid) by scanning the user's fill
// history; there is no direct endpoint for this query.
func (c *Client) Fill(addr string, oid int64) (*Fill, error) {
	fills, err := c.Fills(addr)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch user fills: %w", err)
	}

	for _, fill := range fills {
		if fill.Oid == oid {
			return &fill, nil
		}
	}

	return nil, fmt.Errorf("fill with OID %d not found for user %s", oid, addr)
}

// OrderByCloid returns the open-order row for the supplied (addr, cloid)
// pair, or nil when no live order matches the cloid. The Hyperliquid
// /info orderStatus endpoint returns a nested {status, order:{order,…}}
// envelope; we project the inner order fields into the slim OpenOrder
// shape so callers can compare oid / coin / limit price uniformly with
// Info.OpenOrders results.
func (c *Client) OrderByCloid(addr, cloid string) (*OpenOrder, error) {
	resp, err := c.client.Post("/info", map[string]any{
		"type": "orderStatus",
		"user": addr,
		"oid":  cloid,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch order status by cloid: %w", err)
	}

	var status OrderStatusResponse
	if err := json.Unmarshal(resp, &status); err != nil {
		return nil, fmt.Errorf("failed to unmarshal order status: %w", err)
	}
	// Hyperliquid replies with status="order" when the cloid resolves
	// to a live order; "unknownOid" (or absence) means no match.
	if status.Status != "order" || status.Order.Order.Oid == 0 {
		return nil, nil
	}
	o := status.Order.Order
	limitPx, _ := strconv.ParseFloat(o.LimitPx, 64)
	size, _ := strconv.ParseFloat(o.Sz, 64)
	return &OpenOrder{
		Coin:      o.Coin,
		LimitPx:   limitPx,
		Oid:       o.Oid,
		Side:      o.Side,
		Size:      size,
		Timestamp: o.Timestamp,
	}, nil
}

// Referral returns the referral state for addr.
func (c *Client) Referral(addr string) (*ReferralState, error) {
	resp, err := c.client.Post("/info", map[string]any{
		"type": "referral",
		"user": addr,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch referral state: %w", err)
	}

	var result ReferralState
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal referral state: %w", err)
	}
	return &result, nil
}

// SubAccounts returns the sub-account list for addr.
func (c *Client) SubAccounts(addr string) ([]SubAccount, error) {
	resp, err := c.client.Post("/info", map[string]any{
		"type": "subAccounts",
		"user": addr,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch sub accounts: %w", err)
	}

	var result []SubAccount
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal sub accounts: %w", err)
	}
	return result, nil
}

// MultiSigSigners returns the signer list for multiSigAddr.
func (c *Client) MultiSigSigners(multiSigAddr string) ([]MultiSigSigner, error) {
	resp, err := c.client.Post("/info", map[string]any{
		"type": "userToMultiSigSigners",
		"user": multiSigAddr,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch multi-sig signers: %w", err)
	}

	var result []MultiSigSigner
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal multi-sig signers: %w", err)
	}
	return result, nil
}
