package trade

import (
	"encoding/json"
	"time"

	"github.com/Simon-Busch/hyperliquid-go/signing"
	"github.com/Simon-Busch/hyperliquid-go/types"
)

// SpotDeployResponse is returned by HIP-2 / HIP-3 spot deploy actions.
type SpotDeployResponse struct {
	Status string `json:"status"`
	TxHash string `json:"txHash,omitempty"`
	Error  string `json:"error,omitempty"`
}

// SpotDeployRegisterToken registers a new spot token.
func (c *Client) SpotDeployRegisterToken(
	tokenName string,
	szDecimals int,
	weiDecimals int,
	maxGas int,
	fullName string,
) (*SpotDeployResponse, error) {
	timestamp := time.Now().UnixMilli()

	action := map[string]any{
		"type": "spotDeploy",
		"registerToken2": map[string]any{
			"spec": map[string]any{
				"name":        tokenName,
				"szDecimals":  szDecimals,
				"weiDecimals": weiDecimals,
			},
			"maxGas":   maxGas,
			"fullName": fullName,
		},
	}

	sig, err := signing.SignL1Action(
		c.privateKey,
		action,
		"", // No vault address for spot deploy
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

// SpotDeployUserGenesis initializes user genesis for spot trading.
func (c *Client) SpotDeployUserGenesis(balances map[string]float64) (*SpotDeployResponse, error) {
	timestamp := time.Now().UnixMilli()

	action := map[string]any{
		"type":     "spotDeployUserGenesis",
		"balances": balances,
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

// SpotDeployEnableFreezePrivilege enables freeze privilege for spot deployer.
func (c *Client) SpotDeployEnableFreezePrivilege() (*SpotDeployResponse, error) {
	timestamp := time.Now().UnixMilli()

	action := map[string]any{
		"type": "spotDeployEnableFreezePrivilege",
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

// SpotDeployFreezeUser freezes a user in spot trading.
func (c *Client) SpotDeployFreezeUser(userAddress string) (*SpotDeployResponse, error) {
	timestamp := time.Now().UnixMilli()

	action := map[string]any{
		"type":        "spotDeployFreezeUser",
		"userAddress": userAddress,
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

// SpotDeployRevokeFreezePrivilege revokes freeze privilege for spot deployer.
func (c *Client) SpotDeployRevokeFreezePrivilege() (*SpotDeployResponse, error) {
	timestamp := time.Now().UnixMilli()

	action := map[string]any{
		"type": "spotDeployRevokeFreezePrivilege",
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

// SpotDeployGenesis initializes spot genesis.
func (c *Client) SpotDeployGenesis(deployer string, dexName string) (*SpotDeployResponse, error) {
	timestamp := time.Now().UnixMilli()

	action := map[string]any{
		"type":     "spotDeployGenesis",
		"deployer": deployer,
		"dexName":  dexName,
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

// SpotDeployRegisterSpot registers spot market.
func (c *Client) SpotDeployRegisterSpot(
	baseToken string,
	quoteToken string,
) (*SpotDeployResponse, error) {
	timestamp := time.Now().UnixMilli()

	action := map[string]any{
		"type":       "spotDeployRegisterSpot",
		"baseToken":  baseToken,
		"quoteToken": quoteToken,
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

// SpotDeployRegisterHyperliquidity registers hyperliquidity spot.
func (c *Client) SpotDeployRegisterHyperliquidity(
	name string,
	tokens []string,
) (*SpotDeployResponse, error) {
	timestamp := time.Now().UnixMilli()

	action := map[string]any{
		"type":   "spotDeployRegisterHyperliquidity",
		"name":   name,
		"tokens": tokens,
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

// SpotDeploySetDeployerTradingFeeShare sets deployer trading fee share.
func (c *Client) SpotDeploySetDeployerTradingFeeShare(
	feeShare float64,
) (*SpotDeployResponse, error) {
	timestamp := time.Now().UnixMilli()

	action := map[string]any{
		"type":     "spotDeploySetDeployerTradingFeeShare",
		"feeShare": feeShare,
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
