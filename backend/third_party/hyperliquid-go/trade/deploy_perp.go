package trade

import (
	"encoding/json"
	"time"

	"github.com/Simon-Busch/hyperliquid-go/info"
	"github.com/Simon-Busch/hyperliquid-go/signing"
	"github.com/Simon-Busch/hyperliquid-go/types"
)

// PerpDeployResponse is returned by HIP-3 perp deploy actions; the inner
// statuses array reports per-asset outcomes.
type PerpDeployResponse struct {
	Status string `json:"status"`
	Data   struct {
		Statuses []TxStatus `json:"statuses"`
	} `json:"data"`
}

// TxStatus is one per-asset outcome inside PerpDeployResponse.
type TxStatus struct {
	Coin   string `json:"coin"`
	Status string `json:"status"`
}

// PerpDeployRegisterAsset registers a new perpetual asset.
func (c *Client) PerpDeployRegisterAsset(
	asset string,
	perpDexInput info.PerpDexSchemaInput,
) (*PerpDeployResponse, error) {
	timestamp := time.Now().UnixMilli()

	action := map[string]any{
		"type":         "perpDeployRegisterAsset",
		"asset":        asset,
		"perpDexInput": perpDexInput,
	}

	sig, err := signing.SignL1Action(
		c.privateKey,
		action,
		c.vault,
		timestamp,
		c.expiresAfter,
		c.client.BaseURL == types.MainnetAPIURL,
	)
	if err != nil {
		return nil, err
	}

	resp, err := c.postAction(action, sig, timestamp)
	if err != nil {
		return nil, err
	}

	var result PerpDeployResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// PerpDeploySetOracle sets oracle for perpetual asset.
func (c *Client) PerpDeploySetOracle(
	asset string,
	oracleAddress string,
) (*SpotDeployResponse, error) {
	timestamp := time.Now().UnixMilli()

	action := map[string]any{
		"type":          "perpDeploySetOracle",
		"asset":         asset,
		"oracleAddress": oracleAddress,
	}

	sig, err := signing.SignL1Action(
		c.privateKey,
		action,
		c.vault,
		timestamp,
		c.expiresAfter,
		c.client.BaseURL == types.MainnetAPIURL,
	)
	if err != nil {
		return nil, err
	}

	resp, err := c.postAction(action, sig, timestamp)
	if err != nil {
		return nil, err
	}

	var result SpotDeployResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
